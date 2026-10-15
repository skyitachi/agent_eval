package server

// The replay POST and the replay status pages.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// handleReplayPost accepts a replay form submission.
//
// The order of the checks is the order of the threat: Host (in the guard above),
// then Origin -- which is what separates a browser acting for this page from a
// page elsewhere acting through this browser -- then the CSRF token, then the
// form's own validity. Each refusal says which check failed, because a person
// who just used the form has to be able to tell a stale tab from a broken
// preset.
func (s *Server) handleReplayPost(w http.ResponseWriter, r *http.Request) {
	if !s.opts.ReplayEnabled || s.opts.Replays == nil {
		s.failure(w, http.StatusForbidden, "Replays are disabled.",
			"This server was started with replays turned off.")
		return
	}
	if err := checkSameOrigin(r); err != nil {
		s.failure(w, http.StatusForbidden, "This form answers only to this page.", err.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		s.failure(w, http.StatusBadRequest, "The form could not be read.", err.Error())
		return
	}
	if got := r.PostFormValue("csrf"); got != s.csrf {
		s.failure(w, http.StatusForbidden, "The form token did not match.",
			"The hidden csrf field does not match this server's token. Reload the page and try again -- a stale tab carries the token of the process that served it.")
		return
	}

	source := r.PathValue("source")
	id := r.PathValue("id")
	harnessName := r.PostFormValue("harness")
	skillName := orDefault(r.PostFormValue("skill"), "none")
	model := strings.TrimSpace(r.PostFormValue("model"))
	split := session.Split(orDefault(r.PostFormValue("split"), string(session.SplitSession)))
	workspaceCwd := r.PostFormValue("workspace_cwd") == "1"

	// Rejections that name the field and the reason. The form is small enough
	// that a page-per-mistake is clearer than inline field errors.
	if source != "claude" && source != "cursor" {
		s.failure(w, http.StatusNotFound, "No such session.",
			fmt.Sprintf("%q is not a session source; it is claude or cursor.", source))
		return
	}
	harness, ok := s.opts.Replays.Harness(harnessName)
		s.failure(w, http.StatusBadRequest, "Unknown harness preset.",
			fmt.Sprintf("%q is not one of the configured harness presets (%s).", harnessName, strings.Join(s.opts.Replays.HarnessNames(), ", ")))
		return
	}
	skill, ok := s.opts.Replays.Skill(skillName)
	if !ok {
		s.failure(w, http.StatusBadRequest, "Unknown skill preset.",
			fmt.Sprintf("%q is not one of the configured skill presets (%s).", skillName, strings.Join(s.opts.Replays.SkillNames(), ", ")))
		return
	}
	if !validReplayModel(model) {
		s.failure(w, http.StatusBadRequest, "The model name is not usable.",
			fmt.Sprintf("%q is empty or carries characters a model name may not have (allowed: letters, digits, . _ : / -).", model))
		return
	}
	if split != session.SplitSession && split != session.SplitTurn {
		s.failure(w, http.StatusBadRequest, "Unknown split.",
			fmt.Sprintf("The split must be %q or %q.", session.SplitSession, session.SplitTurn))
		return
	}
	if err := checkSkillUnderHarness(skill, harness); err != nil {
		s.failure(w, http.StatusBadRequest, "These presets cannot be combined.", err.Error())
		return
	}
	// The session is checked now rather than at execution time: a row about a
	// session that is already gone would be a replay that can never start.
	if _, _, err := s.loadReplaySession(store.Replay{Source: source, SessionID: id}); err != nil {
		s.failure(w, http.StatusNotFound, "No such session.",
			fmt.Sprintf("The %s session %q could not be read: %v", source, id, err))
		return
	}

	rid, err := s.submitReplay(replayRequest{
		Source:       source,
		SessionID:    id,
		Harness:      harness,
		Skill:        skill,
		Model:        model,
		Split:        split,
		WorkspaceCwd: workspaceCwd,
	})
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "The replay could not be recorded.", err.Error())
		return
	}
	// 303 rather than 302: the POST's body must not be re-sent by the
	// redirect, and 303 is the one status that says so in every browser.
	http.Redirect(w, r, "/replays/"+rid, http.StatusSeeOther)
}

// checkSameOrigin refuses a POST whose Origin (or, absent that, Referer) does
// not name this server's own host.
//
// The CSRF token is the browser-bound half of this and the Origin header is
// the network half; requiring both is belt and braces, and the check is what
// turns a cross-site form post into a 403 before the token is even read.
func checkSameOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// A Referer from a plain HTML form post in the same origin.
		if ref := r.Header.Get("Referer"); ref != "" {
			origin = ref
		}
	}
	if origin == "" {
		return nil // a non-browser client, e.g. curl; the CSRF token still guards it
	}
	u := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	u = strings.Split(u, "/")[0]
	if u == r.Host {
		return nil
	}
	return fmt.Errorf("The request says it came from %q, but it was sent to %q. Cross-site posts to the replay form are refused.", origin, r.Host)
}

// handleReplays lists the replay rows.
func (s *Server) handleReplays(w http.ResponseWriter, r *http.Request) {
	reps, err := s.st.ListReplays(r.Context(), defaultLimit)
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "The replay list could not be read.", err.Error())
		return
	}
	s.renderSessionPage(w, http.StatusOK, "replays", buildReplaysView(reps))
}

// handleReplayStatus shows one replay.
func (s *Server) handleReplayStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rep, err := s.st.GetReplay(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.failure(w, http.StatusNotFound, "No such replay.",
				fmt.Sprintf("No replay with id %q in %s.", id, s.st.Path()))
			return
		}
		s.failure(w, http.StatusInternalServerError, "The replay could not be read.", err.Error())
		return
	}
	view := buildReplayStatusView(rep)
	// An unfinished replay is followed with a meta refresh, the same way the
	// report pages are: the page reloads itself until the row says it has
	// stopped moving. 2s rather than faster: the work between refreshes is a
	// full agent run, and reloading faster only repaints the same status.
	page := sessionPage{Active: "replays"}
	if view.MetaRefresh {
		page.MetaRefresh = "2"
	}
	s.renderSessionPageExtra(w, http.StatusOK, "replay", view, page)
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

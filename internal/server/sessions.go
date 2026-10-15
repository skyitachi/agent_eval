package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/replay"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// # The session pages, and why they are guarded
//
// The report pages show what the database holds. These show what this machine
// holds -- every transcript Claude Code and Cursor have written, which carry
// source code, file paths and whatever was pasted into a prompt -- and one of
// them starts an agent with permission prompts bypassed. Two attacks follow
// from serving that on a port, and both work against a server bound to
// loopback, because the attacker's code runs in the user's own browser:
//
//   - A page on any site can POST a form to http://127.0.0.1:8080. It cannot
//     read the answer, but it does not need to: the POST starts the replay.
//     Every POST therefore carries a token only a page this server rendered
//     knows, and an Origin header, when present, must be this server.
//   - A page on a domain whose DNS answer is rebound to 127.0.0.1 is, to the
//     browser, same-origin with this server and can read every page. Its
//     requests still carry the attacker's host name, so every session route
//     answers only a Host this server is reachable by.

//go:embed templates/sessions.html
var sessionFS embed.FS

var sessionTemplates = template.Must(template.New("sessions").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"deref": func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	},
	"ms": func(ms int64) string {
		return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
	},
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
	"clip": func(s string, n int) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n]) + fmt.Sprintf("\n… (%d more characters)", len(r)-n)
		}
		return s
	},
	"join": strings.Join,
	"sourceLabel": func(s string) string {
		switch s {
		case session.CatalogClaude:
			return "Claude Code"
		case session.CatalogCursor:
			return "Cursor"
		}
		return s
	},
	"describe": describeSpec,
	"rows": func(r []store.Replay, withSession bool) map[string]any {
		return map[string]any{"Rows": r, "WithSession": withSession}
	},
}).ParseFS(sessionFS, "templates/sessions.html"))

// maxListed bounds the session list. A machine with years of transcripts has
// thousands, and a page of thousands of rows is one nobody scrolls.
const maxListed = 500

// maxTurnsShown bounds the conversation on a session page for the same reason.
const maxTurnsShown = 600

// SessionOptions enables the session pages.
type SessionOptions struct {
	Catalog *session.Catalog
	Replays *replay.Manager
}

// EnableSessions adds the session and replay routes. Without it the server is
// the read-only report server it was.
func (s *Server) EnableSessions(o SessionOptions) error {
	if o.Catalog == nil || o.Replays == nil {
		return errors.New("server: the session pages need a catalog and a replay manager")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("server: generate the form token: %w", err)
	}
	s.sessions = &o
	s.csrf = hex.EncodeToString(b[:])
	return nil
}

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	if s.sessions == nil {
		return
	}
	mux.HandleFunc("GET /sessions", s.guard(s.handleSessions))
	mux.HandleFunc("GET /sessions/{source}/{id}", s.guard(s.handleSession))
	mux.HandleFunc("POST /sessions/{source}/{id}/replay", s.guard(s.handleStartReplay))
	mux.HandleFunc("GET /replays", s.guard(s.handleReplays))
	mux.HandleFunc("GET /replays/{id}", s.guard(s.handleReplay))
	mux.HandleFunc("POST /replays/{id}/cancel", s.guard(s.handleCancelReplay))
}

// guard applies the two checks described at the top of this file.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			s.failure(w, http.StatusForbidden, "This host name is not served.",
				fmt.Sprintf("The session pages answer only on localhost or an IP address, and this request named %q. "+
					"A page that reaches this server under another name is one whose DNS was pointed here, "+
					"and it would be able to read every transcript on this machine.", r.Host))
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					s.failure(w, http.StatusForbidden, "This form was sent from another site.",
						fmt.Sprintf("The request came from %s, not from this server. Open the session here and press the button again.", origin))
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			if err := r.ParseForm(); err != nil {
				s.failure(w, http.StatusBadRequest, "The form could not be read.", err.Error())
				return
			}
			got := r.PostForm.Get("csrf")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
				s.failure(w, http.StatusForbidden, "This form is out of date.",
					"The form token does not match this server's. It changes every time the server starts: "+
						"reload the session page and press the button again.")
				return
			}
		}
		next(w, r)
	}
}

// allowedHost accepts the names this server can legitimately be reached by: an
// IP literal (whatever it was bound to) or localhost. A DNS name is refused
// because a rebound one is indistinguishable from a real one.
func allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host) != nil
}

type sessionError struct {
	Source  string
	Message string
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	entries, errs := s.sessions.Catalog.List()
	source := r.URL.Query().Get("source")
	filter := strings.TrimSpace(r.URL.Query().Get("q"))
	needle := strings.ToLower(filter)
	var shown []session.Entry
	truncated := false
	for _, e := range entries {
		if source != "" && e.Source != source {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Title+" "+e.Cwd+" "+e.ID+" "+e.Branch), needle) {
			continue
		}
		if len(shown) == maxListed {
			truncated = true
			break
		}
		shown = append(shown, e)
	}
	var problems []sessionError
	for src, err := range errs {
		problems = append(problems, sessionError{Source: src, Message: err.Error()})
	}
	s.renderSessions(w, http.StatusOK, "sessions", map[string]any{
		"Title": "Recorded sessions", "Mode": "sessions",
		"Entries": shown, "Errors": problems, "Truncated": truncated,
		"Filter": filter, "Source": source,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	s.renderSessionPage(w, r, http.StatusOK, replay.Choice{}, "")
}

func (s *Server) renderSessionPage(w http.ResponseWriter, r *http.Request, status int, pick replay.Choice, formErr string) {
	source, id := r.PathValue("source"), r.PathValue("id")
	entry, err := s.sessions.Catalog.Find(source, id)
	if err != nil {
		s.failure(w, http.StatusNotFound, "No such session.", err.Error())
		return
	}
	sess, err := s.sessions.Catalog.Load(entry)
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "This session could not be read.", err.Error())
		return
	}
	replays, err := s.st.ListReplays(r.Context(), store.ReplayFilter{Source: entry.Source, SessionID: entry.ID, Limit: 50})
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "The replays could not be read.", err.Error())
		return
	}
	presets := s.sessions.Replays.Presets()
	if pick.Harness == "" && len(presets.Harnesses) > 0 {
		pick.Harness = presets.Harnesses[0].Name
	}
	if pick.Skill == "" && len(presets.Skills) > 0 {
		pick.Skill = presets.Skills[0].Name
	}
	if pick.Model == "" {
		pick.Model = presets.DefaultModel
	}
	models := append([]string(nil), presets.Models...)
	for _, m := range sess.Models {
		if !contains(models, m) {
			models = append(models, m)
		}
	}
	turns, clipped := sess.Turns, false
	if len(turns) > maxTurnsShown {
		turns, clipped = turns[:maxTurnsShown], true
	}
	s.renderSessions(w, status, "session", map[string]any{
		"Title": entry.Title, "Mode": "session",
		"Entry": entry, "Session": sess, "Turns": turns, "Clipped": clipped,
		"Replays": replays, "Presets": presets, "Models": models, "Pick": pick,
		"FormError": formErr, "CSRF": s.csrf, "Bound": bindable(sess.Cwd),
	})
}

func (s *Server) handleStartReplay(w http.ResponseWriter, r *http.Request) {
	pick := replay.Choice{
		Harness: r.PostForm.Get("harness"),
		Skill:   r.PostForm.Get("skill"),
		Model:   r.PostForm.Get("model"),
	}
	rp, err := s.sessions.Replays.Submit(r.Context(), replay.Request{
		Source: r.PathValue("source"), SessionID: r.PathValue("id"), Choice: pick,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, replay.ErrBusy) {
			status = http.StatusServiceUnavailable
		}
		s.renderSessionPage(w, r, status, pick, err.Error())
		return
	}
	// Post/redirect/get, so a reload of the replay page does not start a
	// second replay.
	http.Redirect(w, r, "/replays/"+rp.ID, http.StatusSeeOther)
}

func (s *Server) handleReplays(w http.ResponseWriter, r *http.Request) {
	replays, err := s.st.ListReplays(r.Context(), store.ReplayFilter{Limit: 200})
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "The replays could not be read.", err.Error())
		return
	}
	s.renderSessions(w, http.StatusOK, "replays", map[string]any{
		"Title": "Replays", "Mode": "replays", "Replays": replays,
	})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	rp, err := s.st.GetReplay(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.failure(w, http.StatusNotFound, "No such replay.", err.Error())
			return
		}
		s.failure(w, http.StatusInternalServerError, "The replay could not be read.", err.Error())
		return
	}
	spec := rp.AgentSpecJSON
	var pretty map[string]any
	if json.Unmarshal([]byte(spec), &pretty) == nil {
		if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			spec = string(b)
		}
	}
	refresh := ""
	if !rp.Finished() {
		refresh = "3"
	}
	s.renderSessions(w, http.StatusOK, "replay", map[string]any{
		"Title": "Replay " + rp.ID, "Mode": "replay", "Replay": rp, "Spec": spec,
		"Refresh": refresh, "CSRF": s.csrf,
	})
}

func (s *Server) handleCancelReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sessions.Replays.Cancel(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.failure(w, http.StatusConflict, "This replay cannot be cancelled.", err.Error())
		return
	} else if err != nil {
		s.failure(w, http.StatusNotFound, "No such replay.", err.Error())
		return
	}
	http.Redirect(w, r, "/replays/"+id, http.StatusSeeOther)
}

func (s *Server) renderSessions(w http.ResponseWriter, status int, name string, data map[string]any) {
	writeHTML(w, status, func(out io.Writer) error {
		return sessionTemplates.ExecuteTemplate(out, name, data)
	})
}

// describeSpec is the one-line description of a harness with no notes.
func describeSpec(s agent.Spec) string {
	iso := s.EffectiveIsolation()
	parts := []string{orStr(s.Bin, "claude")}
	if iso.Bare {
		parts = append(parts, "--bare")
	}
	if iso.StrictMCPConfig {
		parts = append(parts, "--strict-mcp-config")
	}
	if iso.DisableSlashCommands {
		parts = append(parts, "--disable-slash-commands")
	}
	if s.AppendSystemPrompt != "" || s.SystemPrompt != "" {
		parts = append(parts, "custom system prompt")
	}
	return strings.Join(parts, " ")
}

// bindable reports whether a replay of a session in dir will start from a
// checkout of it, which is what the form tells the person before they press.
func bindable(dir string) bool {
	return session.Bindable(dir)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func orStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

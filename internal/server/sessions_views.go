package server

// View structs for the session and replay pages. They flatten their sources
// into strings here rather than in the templates, so a template field is a
// sentence and not a computation.

import (
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// sessionsView is the listing page.
type sessionsView struct {
	Total    int
	Entries  []sessionEntryView
	Page     int
	HasPrev  bool
	HasNext  bool
	PrevHref string
	NextHref string
}

type sessionEntryView struct {
	Name      string
	Muted     string
	Source    string
	ID        string
	WhenShort string
	Href      string
}

// buildSessionsView converts raw entries for the template.
func buildSessionsView(entries []sessionEntry) sessionsView {
	out := sessionsView{Total: len(entries)}
	for _, e := range entries {
		out.Entries = append(out.Entries, sessionEntryView{
			Name:      e.Name,
			Muted:     e.Muted,
			Source:    e.Source,
			ID:        shortID(e.ID),
			WhenShort: shortWhen(e.When),
			Href:      e.Href,
		})
	}
	return out
}

// promptView is one human prompt on the detail page.
type promptView struct {
	Index int
	When  string
	Text  string
}

// turnView is one turn on the detail page. Tools are pre-rendered so the
// template never formats JSON.
type turnView struct {
	Who      string
	When     string
	Meta     string
	Text     string
	Thinking string
	Tools    []toolView
}

type toolView struct {
	Name  string
	Input string
}

// sessionDetailView is the detail page.
type sessionDetailView struct {
	Title            string
	Source           string
	Cwd              string
	Git              string
	When             string
	Models           string
	Tokens           string
	Cost             string
	Busy             string
	Prompts          []promptView
	Turns            []turnView
	ReplayEnabled    bool
	CanReplay        bool
	WhyNot           string
	CSRF             string
	ReplayAction     string
	Harnesses        []string
	Skills           []string
	ModelSuggestions []string
}

// buildSessionDetailView projects a parsed session for the template.
func (s *Server) buildSessionDetailView(source, id string, sess *session.Session) sessionDetailView {
	v := sessionDetailView{
		Title:  sessionTitle(sess),
		Source: source,
		Cwd:    orDash(sess.Cwd),
		Git:    gitLine(sess),
		When:   whenRange(sess.StartedAt, sess.EndedAt),
		Models: strings.Join(sess.Models, ", "),
		Tokens: strconv.Itoa(sess.Usage.Total()),
		Cost:   fmt.Sprintf("$%.4f", sess.CostUSD),
		Busy:   shortDuration(sess.BusyMs),
	}
	if v.Models == "" && len(sess.Models) == 0 {
		v.Models = "-"
	}
	for _, p := range sess.Prompts {
		v.Prompts = append(v.Prompts, promptView{Index: p.Index, When: shortWhen(p.At), Text: p.Text})
	}
	for _, t := range sess.Turns {
		tv := turnView{
			Who:  t.Role,
			When: shortWhen(t.At),
			Meta: turnMeta(t),
		}
		if t.Role == session.RoleAssistant {
			tv.Who = "assistant"
		} else {
			tv.Who = "human"
		}
		tv.Text = t.Text
		tv.Thinking = t.Thinking
		for _, tc := range t.Tools {
			tv.Tools = append(tv.Tools, toolView{Name: tc.Name, Input: string(tc.Input)})
		}
		v.Turns = append(v.Turns, tv)
	}

	// The replay form. It needs presets and at least one prompt; a session
	// with nothing to replay says so instead of offering a form that would
	// fail at collect time.
	v.ReplayEnabled = s.opts.ReplayEnabled
	v.CanReplay = s.opts.ReplayEnabled && len(sess.Prompts) > 0 && s.opts.Replays != nil
	switch {
	case !s.opts.ReplayEnabled:
		v.WhyNot = "replays are disabled on this server"
	case len(sess.Prompts) == 0:
		v.WhyNot = "it contains no human prompt, so there is no task to replay"
	case s.opts.Replays == nil:
		v.WhyNot = "no replay presets are configured"
	}
	if v.CanReplay {
		v.CSRF = s.csrf
		v.ReplayAction = fmt.Sprintf("/sessions/%s/%s/replay", source, url.PathEscape(id))
		v.Harnesses = s.opts.Replays.HarnessNames()
		v.Skills = s.opts.Replays.SkillNames()
		v.ModelSuggestions = s.opts.Replays.ModelSuggestions
		if len(v.ModelSuggestions) == 0 {
			// A useful default set rather than an empty list: these are the
			// names the agent binary itself documents.
			v.ModelSuggestions = []string{"sonnet", "opus", "haiku"}
		}
	}
	return v
}

// replaysView is the replay listing.
type replaysView struct {
	Entries []replayEntryView
}

type replayEntryView struct {
	ID           string
	SessionShort string
	Harness      string
	Skill        string
	Model        string
	Status       string
	StatusClass  string
	Error        string
	When         string
	Links        htmlUnsafe
}

// htmlUnsafe is a pre-escaped HTML fragment (links), for the one table cell
// that builds more than one anchor.
// htmlUnsafe is HTML built by the view, for the one table cell that builds
// more than one anchor: template.HTML tells html/template it is already
// markup. Only view code that escapes its parts at construction may use it;
// nothing user-typed goes in.
type htmlUnsafe = template.HTML

// replayStatusView is the replay status page.
type replayStatusView struct {
	ID           string
	Status       string
	StatusClass  string
	Error        string
	Source       string
	SessionID    string
	Harness      string
	Skill        string
	Model        string
	Split        string
	WorkspaceCwd string
	When         string
	ReportHref   string
	RunHref      string
	MetaRefresh  bool
}

// buildReplaysView projects replay rows for the template.
func buildReplaysView(reps []store.Replay) replaysView {
	var out replaysView
	for _, r := range reps {
		e := replayEntryView{
			ID:           r.ID,
			SessionShort: r.Source + "/" + shortID(r.SessionID),
			Harness:      r.Harness,
			Skill:        r.Skill,
			Model:        r.Model,
			Status:       r.Status,
			StatusClass:  replayStatusClass(r.Status),
			When:         shortWhen(r.CreatedAt),
		}
		if r.Error != "" {
			e.Error = oneLineClip(r.Error, 120)
		}
		// The anchors are built here rather than in the template because one
		// cell holds several links. The ids are the row's stored values, not
		// anything a form can type, and they are URL-escaped all the same.
		var links []string
		if r.ReportID != "" {
			links = append(links, fmt.Sprintf(`<a href="/r/%s">report</a>`, url.PathEscape(r.ReportID)))
		}
		if len(links) == 0 {
			e.Links = ""
		} else {
			e.Links = htmlUnsafe(strings.Join(links, " "))
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

// buildReplayStatusView projects one replay row.
func buildReplayStatusView(r store.Replay) replayStatusView {
	var wsCwd = "no"
	if r.WorkspaceCwd {
		wsCwd = "yes"
	}
	v := replayStatusView{
		ID:           r.ID,
		Status:       r.Status,
		StatusClass:  replayStatusClass(r.Status),
		Error:        r.Error,
		Source:       r.Source,
		SessionID:    r.SessionID,
		Harness:      r.Harness,
		Skill:        r.Skill,
		Model:        r.Model,
		Split:        r.Split,
		WorkspaceCwd: wsCwd,
		When:         shortWhen(r.CreatedAt),
		// A finished replay is done moving; a queued or running one is
		// followed with a meta refresh, the same way a live run page is.
		MetaRefresh: r.Status == store.ReplayQueued || r.Status == store.ReplayRunning,
	}
	if r.ReportID != "" {
		v.ReportHref = "/r/" + r.ReportID
	}
	return v
}

func replayStatusClass(status string) string {
	switch status {
	case store.ReplayDone:
		return "pass"
	case store.ReplayFailed:
		return "fail"
	case store.ReplayInterrupted:
		return "warn"
	default:
		return "skip"
	}
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func shortDuration(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms/1000)%60)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms/60_000)%60)
	}
}

func whenRange(start, end time.Time) string {
	if start.IsZero() {
		return "-"
	}
	if end.IsZero() {
		return shortWhen(start)
	}
	return fmt.Sprintf("%s → %s", shortWhen(start), shortWhen(end))
}

func gitLine(sess *session.Session) string {
	parts := []string{}
	if sess.GitBranch != "" {
		parts = append(parts, sess.GitBranch)
	}
	if sess.GitStart.Known {
		if sess.GitStart.Head != "" {
			parts = append(parts, sess.GitStart.Head)
		}
		if sess.GitStart.Dirty {
			parts = append(parts, fmt.Sprintf("dirty (%d path(s))", sess.GitStart.DirtyPaths))
		} else {
			parts = append(parts, "clean")
		}
	} else if sess.GitHead != "" {
		parts = append(parts, sess.GitHead)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " @ ")
}

func turnMeta(t session.Turn) string {
	var parts []string
	if t.DurationMs > 0 {
		parts = append(parts, shortDuration(t.DurationMs))
	}
	if t.Usage != nil {
		parts = append(parts, fmt.Sprintf("%d tok", t.Usage.Total()))
	}
	if len(t.Tools) > 0 {
		parts = append(parts, fmt.Sprintf("%d tool call(s)", len(t.Tools)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "· " + strings.Join(parts, " · ")
}

func sessionTitle(sess *session.Session) string {
	if len(sess.Prompts) > 0 {
		return firstPromptPreview(sess.Prompts[0].Text)
	}
	if sess.ID != "" {
		return "session " + sess.ID
	}
	return "session"
}

func firstPromptPreview(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	runes := []rune(s)
	if len(runes) > 60 {
		return string(runes[:59]) + "…"
	}
	if s == "" {
		return "(no title)"
	}
	return s
}

func oneLineClip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return s
}

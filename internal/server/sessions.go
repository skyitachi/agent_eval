package server

// The session and replay pages.
//
// These are the one part of the site that is not a report: they browse the
// machine's own agent histories and queue replays of them, and their payloads
// are not hashable documents registered in the reports registry. So they render
// their own HTML in this package rather than going through report's payload
// pipeline -- but they reuse its stylesheet and shell conventions (see
// sessionPage), and everything user-controlled is escaped by html/template.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/session"
)

// sessionsPage caps the session listing at one page. The histories grow
// without bound, and stat-only listing is still a directory walk.
const sessionsPage = 50

// ---------------------------------------------------------------------------
// The listing
// ---------------------------------------------------------------------------

type sessionEntry struct {
	// Source is claude | cursor and names the URL prefix too.
	Source string
	ID     string
	// Name is the display name: the Cursor title or the first prompt.
	Name  string
	Muted string // secondary line: cwd, project, model...
	When  time.Time
	Href  string
}

// handleSessions lists this machine's sessions, both histories merged, newest
// first, one page at a time.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	var entries []sessionEntry

	sources, err := session.Sources(s.opts.SessionRoot)
	if err != nil {
		// A missing root is a state, not a failure: the page says so.
		if !isMissingPath(err) {
			s.failure(w, http.StatusInternalServerError, "The Claude history could not be read.", err.Error())
			return
		}
		sources = nil
	}
	for _, src := range sources {
		entries = append(entries, sessionEntry{
			Source: "claude",
			ID:     src.SessionID,
			Name:   s.sessionName(src),
			// The directory a transcript was found under is Claude Code's
			// encoding of the recording's working directory, so its last
			// segment is the closest thing to "which project" the listing can
			// show without parsing every file on the page.
			Muted: filepath.Base(src.ProjectDir),
			When:  src.ModTime,
			Href:  "/sessions/claude/" + url.PathEscape(src.SessionID),
		})
	}

	if s.opts.CursorDB != "" {
		chats, err := session.CursorChats(s.opts.CursorDB)
		if err != nil {
			if !isMissingPath(err) {
				s.failure(w, http.StatusInternalServerError, "The Cursor history could not be read.", err.Error())
				return
			}
			chats = nil
		}
		for _, ch := range chats {
			name := ch.Name
			if name == "" {
				name = "(untitled)"
			}
			entries = append(entries, sessionEntry{
				Source: "cursor",
				ID:     ch.ID,
				Name:   name,
				Muted:  ch.WorkspaceDir,
				When:   ch.UpdatedAt,
				Href:   "/sessions/cursor/" + url.PathEscape(ch.ID),
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].When.After(entries[j].When) })

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}
	start := (page - 1) * sessionsPage
	end := start + sessionsPage
	if start > len(entries) {
		start = len(entries)
	}
	if end > len(entries) {
		end = len(entries)
	}

	// The view's Total counts every entry, not the page's slice of them, so
	// the count line is honest on every page.
	view := buildSessionsView(entries[start:end])
	view.Total = len(entries)
	view.Page = page
	view.Total = len(entries)
	view.Page = page
	view.HasPrev = page > 1
	view.HasNext = end < len(entries)
	view.PrevHref = fmt.Sprintf("/sessions?page=%d", page-1)
	view.NextHref = fmt.Sprintf("/sessions?page=%d", page+1)
	s.renderSessionPage(w, http.StatusOK, "sessions", view)
}

// isMissingPath reports whether err says the data source is simply not there.
// A missing history is an empty page, not an error page.
func isMissingPath(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "not exist") ||
		strings.Contains(msg, "cannot locate")
}

// ---------------------------------------------------------------------------
// The detail page
// ---------------------------------------------------------------------------

// handleSessionDetail shows one session and the replay form.
func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	id := r.PathValue("id")

	var (
		sess   *session.Session
		exists bool
		err    error
	)
	switch source {
	case "claude":
		var src session.Source
		src, err = session.Resolve(s.opts.SessionRoot, id)
		if err == nil {
			sess, err = session.ParseFile(src.Path)
		}
		exists = err == nil
	case "cursor":
		sess, err = session.ParseCursorChat(s.opts.CursorDB, id)
		exists = err == nil
	default:
		s.failure(w, http.StatusNotFound, "No such session.",
			fmt.Sprintf("%q is not a session source; it is claude or cursor. There is no session %q under it.", source, id))
		return
	}
	if !exists {
		s.failure(w, http.StatusNotFound, "No such session.",
			fmt.Sprintf("No %s session %q. It may have been deleted, or the history it lived in is not readable.", source, id))
		return
	}

	view := s.buildSessionDetailView(source, id, sess)
	s.renderSessionPage(w, http.StatusOK, "session", view)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// sessionPage is the shared shell of the session and replay pages. It mirrors
// the report page's structure -- same stylesheet, same topbar vocabulary --
// without going through the payload pipeline, because these pages have no
// payload and no registration.
type sessionPage struct {
	Title string
	// MetaRefresh, when set, is the seconds after which the page reloads
	// itself. It is how a page follows a replay that has not finished yet;
	// the string lands in <meta http-equiv="refresh"> the same way the report
	// pages' MetaRefresh does.
	MetaRefresh string
	// Active marks which nav link the page is under.
	Active string
	Body   template.HTML
}

var sessionShell = template.Must(template.New("shell").Parse(`<!doctype html>
<html lang="en" data-theme="auto">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}} · aeval</title>
{{if .MetaRefresh}}<meta http-equiv="refresh" content="{{.MetaRefresh}}">
{{end}}<link rel="stylesheet" href="/static/app.css">
</head>
<body>
<header class="topbar">
  <div class="topbar-inner">
    <a class="brand" href="/">Reports</a>
    <nav class="nav">
      <a href="/">Reports</a>
      <a href="/sessions"{{if eq .Active "sessions"}} class="active"{{end}}>Sessions</a>
      <a href="/replays"{{if eq .Active "replays"}} class="active"{{end}}>Replays</a>
    </nav>
    <div class="topbar-actions">
      <button type="button" class="btn ghost" id="theme-toggle" title="Switch between light and dark">Theme</button>
    </div>
  </div>
</header>
<main id="main">{{.Body}}</main>
<script>
(function () {
  var btn = document.getElementById('theme-toggle');
  if (btn) btn.addEventListener('click', function () {
    var cur = document.documentElement.getAttribute('data-theme');
    var order = ['auto', 'light', 'dark'];
    var next = order[(order.indexOf(cur) + 1) % 3];
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('aeval-theme', next); } catch (e) {}
  });
  try { var saved = localStorage.getItem('aeval-theme'); if (saved) document.documentElement.setAttribute('data-theme', saved); } catch (e) {}
})();
</script>
</body>
</html>`))

// renderSessionPage renders one of the session pages in the shared shell.
// inner is the name of a body template defined in sessionTemplates.
func (s *Server) renderSessionPage(w http.ResponseWriter, status int, inner string, data any) {
	s.renderSessionPageExtra(w, status, inner, data, sessionPage{Title: sessionTitles[inner]})
}

// renderSessionPageExtra renders one of the session pages in the shared shell,
// with the page fields (meta refresh, active nav) filled in by the caller.
// inner is the name of a body template defined in sessionShellBody, meta a
// sessionPage with only its non-Title/Body fields set.
func (s *Server) renderSessionPageExtra(w http.ResponseWriter, status int, inner string, data any, meta sessionPage) {
	// Every page names the area it is under: a reader moving between a
	// session's detail page and the replay it queued stays oriented the same
	// way they do on the report pages.
	switch inner {
	case "sessions", "session":
		meta.Active = navSessions
	case "replays", "replay":
		meta.Active = navReplays
	}
	meta.Title = sessionTitles[inner]
	t, err := template.New(inner).Parse(sessionShellBody[inner])
	if err != nil {
		http.Error(w, "aeval: rendering this page failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var body bytes.Buffer
	if err := t.Execute(&body, data); err != nil {
		http.Error(w, "aeval: rendering this page failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta.Body = template.HTML(body.String())
	page := meta
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := sessionShell.Execute(w, page); err != nil {
		// The header is already sent; nothing to do but stop cleanly.
		return
	}
}

var sessionTitles = map[string]string{
	"sessions": "Sessions",
	"session":  "Session",
	"replays":  "Replays",
	"replay":   "Replay",
}

// ---------------------------------------------------------------------------
// Session names
// ---------------------------------------------------------------------------

// sessionName renders a Claude transcript's display name: the first human
// prompt, read from the file's head only, cached by mtime and size so a
// listing does not re-read every transcript on every page.
//
// The cache is a plain map guarded by a mutex -- the worst case is one extra
// file head per request for changed sessions, never per row.
const nameCacheLimit = 512

func (s *Server) sessionName(src session.Source) string {
	key := src.Path + "|" + strconv.FormatInt(src.ModTime.UnixNano(), 10) + "|" + strconv.FormatInt(src.Size, 10)
	s.namesMu.Lock()
	if name, ok := s.names[key]; ok {
		s.namesMu.Unlock()
		return name
	}
	s.namesMu.Unlock()

	name := previewFirstPrompt(src.Path)
	s.namesMu.Lock()
	if len(s.names) >= nameCacheLimit {
		s.names = map[string]string{} // drop everything: re-reads are cheap
	}
	s.names[key] = name
	s.namesMu.Unlock()
	return name
}

// previewFirstPrompt reads only the head of a transcript and returns the first
// human prompt, flattened. It does the minimal version of what the parser
// does: find the first user record with a typed prompt and take its text.
// Full parsing happens on the detail page only.
func previewFirstPrompt(path string) string {
	data, err := readFileHead(path, 128*1024)
	if err != nil {
		return "(unreadable)"
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `"type":"user"`) {
			continue
		}
		var rec struct {
			IsMeta bool `json:"isMeta"`
			Origin *struct {
				Kind string `json:"kind"`
			} `json:"origin"`
			Message *struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Message == nil || rec.IsMeta {
			continue
		}
		if rec.Origin != nil && rec.Origin.Kind != "human" {
			continue
		}
		switch content := rec.Message.Content.(type) {
		case string:
			if text := flattenPreview(content); text != "" {
				return text
			}
		case []any:
			// The SDK form: text blocks plus attachments. The first text
			// block is the typed prompt.
			for _, b := range content {
				block, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "text" {
					if text := flattenPreview(stringValue(block["text"])); text != "" {
						return text
					}
				}
			}
		}
	}
	return "(no title)"
}

// flattenPreview renders a prompt as one line, truncated for a table row.
func flattenPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > 80 {
		runes = runes[:79]
		return string(runes) + "…"
	}
	if len(runes) == 0 {
		return ""
	}
	return string(runes)
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// readFileHead reads at most limit bytes from the head of a file.
func readFileHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// shortWhen renders a timestamp the way collect's listings do.
func shortWhen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Catalog source names. They are the vocabulary of the web page and the
// replays table, and deliberately not the baseline Source names: those
// identify a capture format ("cursor-composer"), these identify where on this
// machine a session was found.
const (
	CatalogClaude = "claude"
	CatalogCursor = "cursor"
)

// Entry is one session as a listing shows it: enough to recognise it and pick
// it, without reading the whole conversation.
type Entry struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	// Title is the tool's own name for the session, falling back to the first
	// prompt. It is never empty: a row a person cannot recognise is a row they
	// cannot pick.
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Model     string    `json:"model,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Prompts is the number of human prompts, when the listing knows it
	// without parsing the conversation. -1 means it does not.
	Prompts int   `json:"prompts"`
	Bytes   int64 `json:"bytes,omitempty"`
	// Path is the transcript file for a Claude Code session and the database
	// for a Cursor one. It is what the detail page and a replay read.
	Path string `json:"-"`
}

// Catalog lists the sessions recorded on this machine, by both tools.
//
// It is what the web page reads, and it is built for being read on every page
// load: a Claude Code transcript's header is scanned only once per size and
// modification time, so a listing over a thousand transcripts costs a thousand
// stats after the first.
type Catalog struct {
	// ClaudeRoot is where Claude Code keeps transcripts. Empty disables the
	// source.
	ClaudeRoot string
	// CursorDB is Cursor's state database. Empty disables the source.
	CursorDB string

	mu    sync.Mutex
	heads map[string]cachedHead
}

type cachedHead struct {
	size  int64
	mtime time.Time
	head  transcriptHead
}

// NewCatalog builds a catalog over this machine's default locations.
func NewCatalog() *Catalog {
	return &Catalog{ClaudeRoot: DefaultRoot(), CursorDB: CursorDefaultDB()}
}

// List returns every session of every enabled source, newest first, and the
// errors of the sources that could not be read.
//
// A source that fails does not fail the listing: a machine without Cursor, or
// with a Cursor database locked by a running Cursor, still has Claude Code
// sessions to show, and the error is reported beside them rather than instead
// of them.
func (c *Catalog) List() ([]Entry, map[string]error) {
	var out []Entry
	errs := map[string]error{}
	if c.ClaudeRoot != "" {
		entries, err := c.claudeEntries()
		if err != nil {
			errs[CatalogClaude] = err
		}
		out = append(out, entries...)
	}
	if c.CursorDB != "" {
		if _, err := os.Stat(c.CursorDB); err == nil {
			chats, err := CursorChats(c.CursorDB)
			if err != nil {
				errs[CatalogCursor] = err
			}
			for _, ch := range chats {
				if ch.Prompts == 0 {
					// Nothing a human asked, so nothing to replay.
					continue
				}
				out = append(out, Entry{
					Source:    CatalogCursor,
					ID:        ch.ID,
					Title:     orDefaultString(strings.TrimSpace(ch.Name), "(untitled conversation)"),
					Cwd:       ch.WorkspaceDir,
					Model:     ch.Model,
					UpdatedAt: ch.UpdatedAt,
					Prompts:   ch.Prompts,
					Path:      c.CursorDB,
				})
			}
		}
	}
	sortEntries(out)
	return out, errs
}

// Find returns one listed session.
func (c *Catalog) Find(source, id string) (Entry, error) {
	switch source {
	case CatalogClaude, CatalogCursor:
	default:
		return Entry{}, fmt.Errorf("no session source %q (want %s or %s)", source, CatalogClaude, CatalogCursor)
	}
	entries, errs := c.List()
	for _, e := range entries {
		if e.Source == source && e.ID == id {
			return e, nil
		}
	}
	if err := errs[source]; err != nil {
		return Entry{}, fmt.Errorf("no %s session %s: the source could not be read: %w", source, id, err)
	}
	return Entry{}, fmt.Errorf("no %s session %s on this machine", source, id)
}

// Load parses a listed session in full.
func (c *Catalog) Load(e Entry) (*Session, error) {
	switch e.Source {
	case CatalogClaude:
		return ParseFile(e.Path)
	case CatalogCursor:
		return ParseCursorChat(e.Path, e.ID)
	}
	return nil, fmt.Errorf("no session source %q", e.Source)
}

func (c *Catalog) claudeEntries() ([]Entry, error) {
	srcs, err := Sources(c.ClaudeRoot)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.heads == nil {
		c.heads = map[string]cachedHead{}
	}
	seen := make(map[string]bool, len(srcs))
	out := make([]Entry, 0, len(srcs))
	for _, src := range srcs {
		seen[src.Path] = true
		h, ok := c.heads[src.Path]
		if !ok || h.size != src.Size || !h.mtime.Equal(src.ModTime) {
			head, err := scanHead(src.Path)
			if err != nil {
				continue // unreadable now; the next listing tries again
			}
			h = cachedHead{size: src.Size, mtime: src.ModTime, head: head}
			c.heads[src.Path] = h
		}
		if IsReplayWorkspace(h.head.cwd) {
			// A replay is running its own copy of a recording in a workspace
			// it owns. Its transcript is the replay's output, not another
			// recording to offer back: listing it would invite replaying a
			// replay, and its "prompt" would be the replay's input, not a
			// request anyone typed in that directory.
			continue
		}
		if h.head.firstPrompt == "" && h.head.title == "" {
			// Machine-driven: an `aeval run` attempt, a hook, an SDK call.
			// There is no human request in it to replay.
			continue
		}
		title := h.head.title
		if title == "" {
			title = oneLinePrompt(h.head.firstPrompt, 90)
		}
		out = append(out, Entry{
			Source:    CatalogClaude,
			ID:        orDefaultString(h.head.sessionID, src.SessionID),
			Title:     title,
			Cwd:       h.head.cwd,
			Branch:    h.head.branch,
			Model:     h.head.model,
			UpdatedAt: src.ModTime,
			Prompts:   -1,
			Bytes:     src.Size,
			Path:      src.Path,
		})
	}
	for p := range c.heads {
		if !seen[p] {
			delete(c.heads, p)
		}
	}
	return out, nil
}

// transcriptHead is what a listing needs from a transcript.
type transcriptHead struct {
	sessionID   string
	cwd         string
	branch      string
	model       string
	title       string
	firstPrompt string
}

// headLines bounds how far into a transcript the scan looks for the first
// prompt and the first model. The title can come later -- Claude Code writes it
// after the first exchange and rewrites it on a rename -- so it is matched on
// every line, but cheaply: only lines that mention it are decoded.
const headLines = 400

var aiTitleMarker = []byte(`"type":"ai-title"`)

// scanHead reads the provenance and the name of a transcript without building
// its conversation.
func scanHead(path string) (transcriptHead, error) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptHead{}, err
	}
	defer f.Close()

	var h transcriptHead
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for n := 0; sc.Scan(); n++ {
		line := sc.Bytes()
		if bytes.Contains(line, aiTitleMarker) {
			var rec rawRecord
			if json.Unmarshal(line, &rec) == nil && strings.TrimSpace(rec.AITitle) != "" {
				h.title = strings.TrimSpace(rec.AITitle)
			}
			continue
		}
		if n >= headLines || (h.firstPrompt != "" && h.model != "" && h.cwd != "") {
			continue
		}
		var rec rawRecord
		if json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
			continue
		}
		if h.sessionID == "" {
			h.sessionID = rec.SessionID
		}
		if h.cwd == "" {
			h.cwd = rec.Cwd
		}
		if h.branch == "" {
			h.branch = rec.GitBranch
		}
		if rec.Message == nil {
			continue
		}
		switch rec.Type {
		case "assistant":
			if h.model == "" && rec.Message.Model != "" && rec.Message.Model != "<synthetic>" {
				h.model = rec.Message.Model
			}
		case "user":
			if h.firstPrompt == "" && isHumanPrompt(rec) {
				h.firstPrompt = promptText(rec.Message.Content)
			}
		}
	}
	return h, sc.Err()
}

// promptText is the human-readable part of a user message, the same reading
// absorbUser makes.
func promptText(c content) string {
	if len(c.blocks) == 0 {
		return strings.TrimSpace(c.text)
	}
	var parts []string
	for _, b := range c.blocks {
		if b.Type == "tool_result" {
			return ""
		}
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// oneLinePrompt flattens a prompt into a listing title.
func oneLinePrompt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func sortEntries(e []Entry) {
	sort.SliceStable(e, func(i, j int) bool { return newer(e[i], e[j]) })
}

func newer(a, b Entry) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	return a.ID < b.ID
}

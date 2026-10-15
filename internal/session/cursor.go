package session

// Cursor does not write transcripts to disk the way Claude Code does. Its chat
// history lives in one SQLite database:
//
//	~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
//
// inside a generic key/value table, `cursorDiskKV`, under two key families:
//
//	composerData:<composerId>              -- one conversation: its bubble list,
//	                                          its model, its workspace
//	bubbleId:<composerId>:<bubbleId>       -- one message in that conversation
//
// `composerHeaders` is a sibling table with the same ids, but it is only a
// *partial* index -- on the machine this was written against, 195 of 330
// conversations had no header row and 70 of 148 conversations with bubbles were
// missing from it. So the reader keys off `cursorDiskKV`, and consults the
// header table only for the two flags it alone carries (isSubagent,
// isArchived). Reading it as the index would silently lose more than half of
// the history.
//
// # What a Cursor conversation cannot give back
//
// Cursor records no cost, no wall-clock duration and no split between API time
// and tool time. It does record a `createdAt` per bubble, which is enough to
// reconstruct *when* things happened but not *how long the machine worked*.
// The reader therefore estimates busy time as the sum of the gaps that end at
// an assistant bubble -- see busyFromGaps -- and labels the estimate in the
// baseline notes rather than passing it off as a measurement.
//
// # What maps to what
//
//	A bubble of type 1 -> a human turn, and therefore a Prompt
//	A bubble of type 2 -> an assistant Turn, with its thinking, text and tools
//
// One assistant *bubble* is one Turn, not one API response: Cursor writes a
// separate bubble per visible step (a text bubble, then a tool bubble, then
// another text bubble), where Claude Code multiplexes the same sequence into
// one message id. The two counters are therefore not the same unit, and a
// report that compares them side by side is comparing a finer grain against a
// coarser one.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required
)

// CursorSourceName is the producer recorded in dataset.Baseline.Source for a
// conversation collected from Cursor.
const CursorSourceName = "cursor-composer"

// CursorDBName is the file name of Cursor's state database.
const CursorDBName = "state.vscdb"

// CursorDefaultDB returns the Cursor state database for the current platform.
//
// A missing file is not an error here; the caller decides whether "this machine
// has never run Cursor" is a state to report or a failure.
func CursorDefaultDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", CursorDBName)
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", CursorDBName)
		}
		return ""
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Cursor", "User", "globalStorage", CursorDBName)
		}
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", CursorDBName)
	}
}

// CursorChat describes one stored conversation without reading its bubbles.
type CursorChat struct {
	// ID is the composer id, the conversation's identity in the database.
	ID string
	// Name is the title Cursor shows in its history list. Frequently empty.
	Name string
	// WorkspaceDir is the folder the conversation was held in, when the record
	// carries it. It is the cwd the agent saw.
	WorkspaceDir string
	// Mode is Cursor's own label: "agent", "chat", "edit", "multitask".
	Mode string
	// Model is the primary model the conversation was configured with.
	Model string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Bubbles counts messages; Prompts counts the human ones. Both come from
	// the conversation's own index, so listing does not read any bubble.
	Bubbles int
	Prompts int

	IsSubagent bool
	IsArchived bool
}

// ---------------------------------------------------------------------------
// Raw records
// ---------------------------------------------------------------------------

// composerData is the JSON under a `composerData:<id>` key. Only the fields the
// reader needs are declared: the object carries several dozen more, and
// decoding them all would make the reader break every time Cursor adds one.
type cursorComposerData struct {
	Version     int    `json:"_v"`
	ComposerID  string `json:"composerId"`
	Name        string `json:"name"`
	CreatedAt   int64  `json:"createdAt"` // epoch milliseconds
	UpdatedAt   int64  `json:"lastUpdatedAt"`
	UnifiedMode string `json:"unifiedMode"`

	Headers []cursorHeader `json:"fullConversationHeadersOnly"`

	ModelConfig *struct {
		ModelName      string `json:"modelName"`
		SelectedModels []struct {
			ModelID string `json:"modelId"`
		} `json:"selectedModels"`
	} `json:"modelConfig"`

	WorkspaceIdentifier cursorWorkspaceRef `json:"workspaceIdentifier"`
	AgentLocation       struct {
		Environment cursorWorkspaceRef `json:"environment"`
	} `json:"agentLocation"`

	SubagentComposerIDs []string `json:"subagentComposerIds"`
}

// cursorWorkspaceRef is Cursor's way of naming a folder. Both the plain and the
// URI form are read: the plain one is present in some builds and not others.
type cursorWorkspaceRef struct {
	URI *struct {
		FsPath string `json:"fsPath"`
		Path   string `json:"path"`
	} `json:"uri"`
}

func (w cursorWorkspaceRef) dir() string {
	if w.URI == nil {
		return ""
	}
	if w.URI.FsPath != "" {
		return w.URI.FsPath
	}
	return w.URI.Path
}

// cursorHeader is one entry of the conversation's own message index. It carries
// no content -- only the bubble id, the role and the time -- which is why the
// reader still has to fetch every bubble.
type cursorHeader struct {
	BubbleID  string `json:"bubbleId"`
	Type      int    `json:"type"`
	CreatedAt string `json:"createdAt"`
}

// cursorBubble is the JSON under a `bubbleId:` key.
type cursorBubble struct {
	Version   int    `json:"_v"`
	Type      int    `json:"type"`
	BubbleID  string `json:"bubbleId"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`

	Thinking *struct {
		Text string `json:"text"`
	} `json:"thinking"`
	ThinkingDurationMs int64 `json:"thinkingDurationMs"`

	ToolFormerData *cursorToolFormer `json:"toolFormerData"`

	TokenCount *struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"tokenCount"`

	ModelInfo *struct {
		ModelName string `json:"modelName"`
	} `json:"modelInfo"`

	// CodeBlocks names the files an edit touched. It is the only place an
	// edit_file_v2 call records its target: that tool's params and rawArgs are
	// both empty in current builds.
	CodeBlocks []struct {
		URI *struct {
			Path string `json:"path"`
		} `json:"uri"`
	} `json:"codeBlocks"`

	// IsNudge marks a message Cursor injected rather than one the human typed.
	IsNudge bool `json:"isNudge"`
}

// cursorToolFormer is one tool invocation as Cursor stores it. Params and
// RawArgs are JSON *strings* holding JSON, and either can be empty.
type cursorToolFormer struct {
	ToolCallID string `json:"toolCallId"`
	ToolIndex  int    `json:"toolIndex"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Params     string `json:"params"`
	RawArgs    string `json:"rawArgs"`
	Result     string `json:"result"`
}

// Bubble roles. Cursor's type field is the discriminator.
const (
	cursorBubbleUser      = 1
	cursorBubbleAssistant = 2
)

// ---------------------------------------------------------------------------
// Opening the database
// ---------------------------------------------------------------------------

// cursorDB is an opened, read-only handle to a Cursor state database.
type cursorDB struct {
	db *sql.DB
	// tmpDir is set when the database had to be copied before it could be
	// read; Close removes it.
	tmpDir string
	// Path records what was actually read, which is the copy when one was made.
	Path string
}

// openCursorDB opens the database read-only.
//
// A read-only open of a WAL database still needs to be able to create the
// shared-memory file beside it. When Cursor is not running that file may be
// gone and the open fails with "unable to open database file" -- which reads
// like a permissions problem and is not one. The fallback is to copy the
// database aside and read the copy, which costs a file copy and always works.
func openCursorDB(path string) (*cursorDB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("cursor: no database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("cursor: %s: %w", abs, err)
	}

	if db := tryOpenSQLite("file:" + abs + "?mode=ro"); db != nil {
		return &cursorDB{db: db, Path: abs}, nil
	}

	dir, err := os.MkdirTemp("", "aeval-cursor-")
	if err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyIfPresent(abs+suffix, filepath.Join(dir, CursorDBName+suffix)); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	copyPath := filepath.Join(dir, CursorDBName)
	db := tryOpenSQLite("file:" + copyPath + "?mode=ro")
	if db == nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cursor: cannot read %s (tried in place and as a copy; "+
			"is another process holding it?)", abs)
	}
	return &cursorDB{db: db, tmpDir: dir, Path: copyPath}, nil
}

func tryOpenSQLite(dsn string) *sql.DB {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil
	}
	// One connection: SQLite serves one writer, and a pool would only add
	// lock contention over a database another process is actively using.
	db.SetMaxOpenConns(1)
	var probe string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE name='cursorDiskKV'").Scan(&probe); err != nil {
		db.Close()
		return nil
	}
	return db
}

func copyIfPresent(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("cursor: copy %s: %w", filepath.Base(src), err)
	}
	return out.Close()
}

// Close releases the handle and any temporary copy.
func (c *cursorDB) Close() error {
	if c == nil {
		return nil
	}
	var err error
	if c.db != nil {
		err = c.db.Close()
	}
	if c.tmpDir != "" {
		os.RemoveAll(c.tmpDir)
	}
	return err
}

// ---------------------------------------------------------------------------
// Reading key/value pairs
// ---------------------------------------------------------------------------

// rangeKeys returns every row whose key starts with prefix.
//
// The bound is a half-open range rather than a LIKE pattern on purpose: LIKE
// treats `_` as a wildcard, keys are unbounded in length, and the range form
// uses the unique index on `key` instead of scanning the table.
func (c *cursorDB) rangeKeys(prefix string) (map[string][]byte, error) {
	upper := prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
	rows, err := c.db.Query("SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?", prefix, upper)
	if err != nil {
		return nil, fmt.Errorf("cursor: read %s*: %w", prefix, err)
	}
	defer rows.Close()

	out := map[string][]byte{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("cursor: scan %s*: %w", prefix, err)
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (c *cursorDB) get(key string) ([]byte, error) {
	var value []byte
	err := c.db.QueryRow("SELECT value FROM cursorDiskKV WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cursor: read %s: %w", key, err)
	}
	return value, nil
}

// cursorHeaderFlags is the part of `composerHeaders` the reader uses. The table
// is an incomplete index, so a missing row means "unknown", not "not a
// subagent" -- see the package comment on cursor.go.
type cursorHeaderFlags struct {
	IsSubagent bool
	IsArchived bool
}

func (c *cursorDB) headerFlags() (map[string]cursorHeaderFlags, error) {
	rows, err := c.db.Query("SELECT composerId, isSubagent, isArchived FROM composerHeaders")
	if err != nil {
		// The table is not guaranteed to exist in every build; its absence is
		// a reason to have fewer flags, not a reason to fail.
		return nil, nil
	}
	defer rows.Close()

	out := map[string]cursorHeaderFlags{}
	for rows.Next() {
		var id string
		var sub, archived sql.NullInt64
		if err := rows.Scan(&id, &sub, &archived); err != nil {
			return nil, nil
		}
		out[id] = cursorHeaderFlags{IsSubagent: sub.Int64 == 1, IsArchived: archived.Int64 == 1}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// CursorChats lists the stored conversations, newest first.
//
// Subagent conversations are excluded: their messages are a delegated
// sub-trajectory, and counting the assistant's own delegation as human work is
// the same mistake as counting a Claude Code sidechain. They are identified
// two ways because neither is complete -- the header table's isSubagent flag,
// and the parent conversation's subagentComposerIds list.
func CursorChats(dbPath string) ([]CursorChat, error) {
	c, err := openCursorDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.chats()
}

func (c *cursorDB) chats() ([]CursorChat, error) {
	raw, err := c.rangeKeys("composerData:")
	if err != nil {
		return nil, err
	}
	flags, err := c.headerFlags()
	if err != nil {
		return nil, err
	}

	subagents := map[string]bool{}
	type parsed struct {
		chat CursorChat
		data cursorComposerData
	}
	var all []parsed
	for key, value := range raw {
		var d cursorComposerData
		if err := json.Unmarshal(value, &d); err != nil {
			continue // an undecodable conversation is skipped, not fatal
		}
		id := d.ComposerID
		if id == "" {
			id = strings.TrimPrefix(key, "composerData:")
		}
		for _, sub := range d.SubagentComposerIDs {
			subagents[sub] = true
		}
		ch := CursorChat{
			ID:           id,
			Name:         d.Name,
			WorkspaceDir: d.workspaceDir(),
			Mode:         d.UnifiedMode,
			Model:        d.modelName(),
			CreatedAt:    msTime(d.CreatedAt),
			UpdatedAt:    msTime(d.UpdatedAt),
			Bubbles:      len(d.Headers),
		}
		for _, h := range d.Headers {
			if h.Type == cursorBubbleUser {
				ch.Prompts++
			}
		}
		all = append(all, parsed{chat: ch, data: d})
	}

	var out []CursorChat
	for _, p := range all {
		ch := p.chat
		if f, ok := flags[ch.ID]; ok {
			ch.IsSubagent = ch.IsSubagent || f.IsSubagent
			ch.IsArchived = f.IsArchived
		}
		ch.IsSubagent = ch.IsSubagent || subagents[ch.ID]
		if ch.IsSubagent {
			continue
		}
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// CursorChatsForCwd lists the conversations held in one working directory.
func CursorChatsForCwd(dbPath, cwd string) ([]CursorChat, error) {
	all, err := CursorChats(dbPath)
	if err != nil {
		return nil, err
	}
	want := filepath.Clean(cwd)
	var out []CursorChat
	for _, ch := range all {
		if ch.WorkspaceDir != "" && filepath.Clean(ch.WorkspaceDir) == want {
			out = append(out, ch)
		}
	}
	return out, nil
}

// LatestCursorChat returns the most recently updated conversation.
func LatestCursorChat(dbPath string) (CursorChat, error) {
	all, err := CursorChats(dbPath)
	if err != nil {
		return CursorChat{}, err
	}
	if len(all) == 0 {
		return CursorChat{}, fmt.Errorf("cursor: no conversations in %s", dbPath)
	}
	return all[0], nil
}

// ResolveCursor turns a selector into one conversation. A selector is either a
// composer id prefix or a full composer id.
func ResolveCursor(dbPath, selector string) (CursorChat, error) {
	if strings.TrimSpace(selector) == "" {
		return CursorChat{}, errors.New("cursor: no conversation given")
	}
	all, err := CursorChats(dbPath)
	if err != nil {
		return CursorChat{}, err
	}
	var matches []CursorChat
	for _, ch := range all {
		if ch.ID == selector || strings.HasPrefix(ch.ID, selector) {
			matches = append(matches, ch)
		}
	}
	switch len(matches) {
	case 0:
		return CursorChat{}, fmt.Errorf("cursor: no conversation matching %q in %s "+
			"(use --list to see the %d stored conversation(s))", selector, dbPath, len(all))
	case 1:
		return matches[0], nil
	default:
		return CursorChat{}, fmt.Errorf("cursor: %q matches %d conversations; give a longer prefix", selector, len(matches))
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// ParseCursorChat reads one conversation into the common Session shape, so that
// everything downstream -- Collect, WriteDataset, Summarize -- works unchanged.
func ParseCursorChat(dbPath, composerID string) (*Session, error) {
	c, err := openCursorDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.parseChat(composerID)
}

// ParseCursorChats reads several conversations with a single open. Reading them
// one at a time would re-open (and, when the database is locked, re-copy) the
// file once per conversation.
func ParseCursorChats(dbPath string, composerIDs []string) ([]*Session, error) {
	c, err := openCursorDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	out := make([]*Session, 0, len(composerIDs))
	for _, id := range composerIDs {
		s, err := c.parseChat(id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *cursorDB) parseChat(composerID string) (*Session, error) {
	raw, err := c.get("composerData:" + composerID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("cursor: no conversation %s in %s", composerID, c.Path)
	}
	var data cursorComposerData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("cursor: conversation %s: %w", composerID, err)
	}

	bubbles, err := c.rangeKeys("bubbleId:" + composerID + ":")
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:     composerID,
		Path:   c.Path,
		Source: CursorSourceName,
		Cwd:    data.workspaceDir(),
		Title:  strings.TrimSpace(data.Name),
		Turns:  []Turn{},
	}
	s.StartedAt = msTime(data.CreatedAt)
	s.EndedAt = msTime(data.UpdatedAt)

	modelSet := map[string]bool{}
	if m := data.modelName(); m != "" {
		modelSet[m] = true
	}
	edited := map[string]bool{}

	// The conversation's own index decides the order. Bubbles it does not list
	// are appended afterwards in timestamp order rather than dropped: an
	// incomplete index should lose the ordering, not the content.
	seen := map[string]bool{}
	var ordered []cursorBubble
	for _, h := range data.Headers {
		if h.BubbleID == "" || seen[h.BubbleID] {
			continue
		}
		seen[h.BubbleID] = true
		var b cursorBubble
		if err := json.Unmarshal(bubbles["bubbleId:"+composerID+":"+h.BubbleID], &b); err != nil || b.Type == 0 {
			// Fall back to what the index claims, so a bubble whose payload is
			// missing still marks the boundary between two turns.
			b = cursorBubble{BubbleID: h.BubbleID, Type: h.Type, CreatedAt: h.CreatedAt}
		}
		if b.CreatedAt == "" {
			b.CreatedAt = h.CreatedAt
		}
		ordered = append(ordered, b)
	}
	var orphans []cursorBubble
	for key, value := range bubbles {
		id := strings.TrimPrefix(key, "bubbleId:"+composerID+":")
		if seen[id] {
			continue
		}
		var b cursorBubble
		if err := json.Unmarshal(value, &b); err != nil {
			continue
		}
		orphans = append(orphans, b)
	}
	sort.SliceStable(orphans, func(i, j int) bool {
		ti, _ := parseTime(orphans[i].CreatedAt)
		tj, _ := parseTime(orphans[j].CreatedAt)
		return ti.Before(tj)
	})
	ordered = append(ordered, orphans...)

	var prevAt time.Time
	var assistants, untokenised int
	for _, b := range ordered {
		at, hasAt := parseTime(b.CreatedAt)
		if hasAt {
			if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
				s.StartedAt = at
			}
			if at.After(s.EndedAt) {
				s.EndedAt = at
			}
		}
		switch b.Type {
		case cursorBubbleUser:
			text := strings.TrimSpace(b.Text)
			if text == "" || b.IsNudge {
				// A user bubble with no text carries attachments, lints or
				// console logs. It is context, not something the human said.
				break
			}
			s.Prompts = append(s.Prompts, Prompt{
				Index:     len(s.Prompts),
				Text:      text,
				At:        at,
				TurnIndex: len(s.Turns),
			})
			s.Turns = append(s.Turns, Turn{Role: RoleUser, Text: text, At: at})
		case cursorBubbleAssistant:
			before := len(s.Turns)
			s.absorbCursorBubble(&b, at, modelSet, edited)
			if len(s.Turns) > before {
				assistants++
				if s.Turns[len(s.Turns)-1].Usage == nil {
					untokenised++
				}
			}
		}
		if hasAt {
			// Busy time: every gap that *ends* at an assistant bubble. The gap
			// that starts at an assistant bubble and ends at a human one is the
			// human reading and typing, and is exactly what must not be
			// counted; the gap before an assistant bubble is the model and its
			// tools working. See the package comment for why this is an
			// estimate and not a measurement.
			if b.Type == cursorBubbleAssistant && !prevAt.IsZero() && at.After(prevAt) {
				s.APIDurationMs += at.Sub(prevAt).Milliseconds()
			}
			prevAt = at
		}
	}

	s.Usage = s.summedUsage()
	s.BusyMs = s.APIDurationMs + s.ToolDurationMs
	if !s.StartedAt.IsZero() && s.EndedAt.After(s.StartedAt) {
		s.DurationMs = s.EndedAt.Sub(s.StartedAt).Milliseconds()
	}
	s.haveTotals = true // usage is already the sum over bubbles

	for m := range modelSet {
		s.Models = append(s.Models, m)
	}
	sort.Strings(s.Models)
	for p := range edited {
		s.EditedFiles = append(s.EditedFiles, p)
	}
	sort.Strings(s.EditedFiles)

	if s.Cwd == "" {
		// The workspace is stated per conversation, so an empty one is a gap in
		// the record. Say so rather than guessing from the bubble payloads.
		s.BaselineNotes = append(s.BaselineNotes,
			"cursor did not record the workspace folder for this conversation")
	}
	s.BaselineNotes = append(s.BaselineNotes,
		"collected from Cursor, which records no cost and no per-request timing: "+
			"busy time is estimated from the gaps that end at an assistant bubble")
	s.BaselineNotes = append(s.BaselineNotes,
		"assistant turns are counted per Cursor bubble, which is a finer unit than "+
			"a Claude Code assistant message")
	if untokenised > 0 {
		// A partial sum is worse than a missing one: it looks like a
		// measurement. Saying how many messages were not counted is what keeps
		// the total readable as the lower bound it is.
		s.BaselineNotes = append(s.BaselineNotes, fmt.Sprintf(
			"%d of %d assistant messages carry no token count, so the token total is a lower bound",
			untokenised, assistants))
	}
	return s, nil
}

func (s *Session) absorbCursorBubble(b *cursorBubble, at time.Time, modelSet map[string]bool, edited map[string]bool) {
	if b.ModelInfo != nil {
		if m := cursorModelName(b.ModelInfo.ModelName); m != "" {
			modelSet[m] = true
		}
	}
	t := Turn{Role: RoleAssistant, At: at, MessageID: b.BubbleID}
	t.Text = strings.TrimSpace(b.Text)
	if b.Thinking != nil {
		t.Thinking = strings.TrimSpace(b.Thinking.Text)
	}
	s.ThinkingMs += b.ThinkingDurationMs

	if b.ToolFormerData != nil && b.ToolFormerData.Name != "" {
		tf := b.ToolFormerData
		input := toolInput(tf)
		tc := ToolCall{ID: tf.ToolCallID, Name: tf.Name, Input: input}
		t.Tools = append(t.Tools, tc)
		s.ToolCalls = append(s.ToolCalls, tc)
		if strings.TrimSpace(tf.Result) != "" {
			s.ToolResults++
		}
		if cursorMutatingTools[tf.Name] {
			for _, p := range cursorEditedPaths(tf, b) {
				edited[p] = true
			}
		}
	}

	if b.TokenCount != nil && (b.TokenCount.InputTokens > 0 || b.TokenCount.OutputTokens > 0) {
		u := Usage{
			InputTokens:  b.TokenCount.InputTokens,
			OutputTokens: b.TokenCount.OutputTokens,
		}
		t.Usage = &u
	}
	// An assistant bubble that carried nothing at all is not a turn: Cursor
	// writes empty bubbles for UI grouping, and counting them would inflate
	// both the turn count and the trajectory.
	if t.Text == "" && t.Thinking == "" && len(t.Tools) == 0 {
		return
	}
	s.Turns = append(s.Turns, t)
}

// cursorMutatingTools are the tool names that change a file. Read-only tools
// are excluded: they name paths too, and a baseline that listed every file the
// agent *looked at* as "edited" would be wrong in the direction that matters.
var cursorMutatingTools = map[string]bool{
	"apply_patch":    true,
	"apply_diff":     true,
	"search_replace": true,
	"edit_file":      true,
	"edit_file_v2":   true,
	"write":          true,
	"create_file":    true,
	"delete_file":    true,
}

// toolInput renders a tool's arguments as the raw JSON the call carried.
//
// Cursor stores them as a JSON *string* inside the outer JSON, and the two
// copies (params, rawArgs) disagree about which is populated by tool: the
// edit tools fill neither and put the path in codeBlocks instead.
func toolInput(tf *cursorToolFormer) json.RawMessage {
	for _, s := range []string{tf.Params, tf.RawArgs} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		// Valid JSON strings are not guaranteed; keep the text rather than
		// dropping the only record of what the tool was called with.
		quoted, err := json.Marshal(s)
		if err != nil {
			return nil
		}
		return json.RawMessage(quoted)
	}
	return nil
}

// cursorEditedPaths names the files a mutating tool call touched.
func cursorEditedPaths(tf *cursorToolFormer, b *cursorBubble) []string {
	var out []string
	for _, s := range []string{tf.Params, tf.RawArgs} {
		var args struct {
			RelativeWorkspacePath string `json:"relativeWorkspacePath"`
			Path                  string `json:"path"`
		}
		if s == "" || json.Unmarshal([]byte(s), &args) != nil {
			continue
		}
		// The field is named "relative" but holds an absolute path in every
		// build observed; accept it either way rather than inventing a root.
		if p := strings.TrimSpace(args.RelativeWorkspacePath); p != "" {
			out = append(out, p)
		} else if p := strings.TrimSpace(args.Path); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		for _, cb := range b.CodeBlocks {
			if cb.URI != nil && cb.URI.Path != "" {
				out = append(out, cb.URI.Path)
			}
		}
	}
	return out
}

func (d cursorComposerData) workspaceDir() string {
	if p := d.WorkspaceIdentifier.dir(); p != "" {
		return p
	}
	return d.AgentLocation.Environment.dir()
}

func (d cursorComposerData) modelName() string {
	if d.ModelConfig == nil {
		return ""
	}
	if m := cursorModelName(d.ModelConfig.ModelName); m != "" {
		return m
	}
	// The model the setting resolved to is in selectedModels, when Cursor
	// bothered to pin one -- but that field carries the same placeholder when
	// it did not, so it goes through the same filter.
	if len(d.ModelConfig.SelectedModels) > 0 {
		return cursorModelName(d.ModelConfig.SelectedModels[0].ModelID)
	}
	return ""
}

// cursorModelName drops the placeholder Cursor writes when a conversation ran
// under whatever the user had selected globally. "default" names a setting, not
// a model, and emitting it into a column headed "model" reads as an answer to a
// question the record did not answer.
func cursorModelName(name string) string {
	if name == cursorDefaultModel {
		return ""
	}
	return name
}

// cursorDefaultModel is the placeholder Cursor writes when the conversation
// ran under whatever the user had selected globally.
const cursorDefaultModel = "default"

// msTime converts an epoch-millisecond stamp. Zero stays zero rather than
// becoming 1970, so "not recorded" stays distinguishable from a real time.
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

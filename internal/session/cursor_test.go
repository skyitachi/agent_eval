package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The tests build a Cursor database from scratch rather than reading the one on
// the machine. A fixture copied from a real installation would pin the reader
// to one Cursor version and one person's chat history; a synthesised one says
// exactly which shapes the reader claims to understand.

type cursorKV struct{ key, value string }

type cursorHeaderRow struct {
	id       string
	subagent bool
	archived bool
}

func writeCursorDB(t *testing.T, kvs []cursorKV, headers []cursorHeaderRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), CursorDBName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	for _, ddl := range []string{
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, workspaceId TEXT,
		 createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER,
		 recency INTEGER, checkpointAt INTEGER, value TEXT, subagentTypeName TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create fixture: %v", err)
		}
	}
	for _, kv := range kvs {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, kv.key, kv.value); err != nil {
			t.Fatalf("insert %s: %v", kv.key, err)
		}
	}
	for _, h := range headers {
		if _, err := db.Exec(`INSERT INTO composerHeaders (composerId, isSubagent, isArchived) VALUES (?, ?, ?)`,
			h.id, boolInt(h.subagent), boolInt(h.archived)); err != nil {
			t.Fatalf("insert header %s: %v", h.id, err)
		}
	}
	return path
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

// composerDataJSON builds the conversation index. The bubble ids are given as
// "type:id" pairs so a test reads as the conversation shape it is describing.
func composerDataJSON(t *testing.T, id, cwd string, model string, bubbles ...string) string {
	t.Helper()
	headers := make([]map[string]any, 0, len(bubbles))
	for _, b := range bubbles {
		typ, bid, _ := strings.Cut(b, ":")
		n := 1
		if typ == "assistant" {
			n = 2
		}
		headers = append(headers, map[string]any{"bubbleId": bid, "type": n})
	}
	doc := map[string]any{
		"_v":                          18,
		"composerId":                  id,
		"createdAt":                   int64(1767225600000), // 2026-01-01T00:00:00Z
		"lastUpdatedAt":               int64(1767225900000),
		"fullConversationHeadersOnly": headers,
	}
	if cwd != "" {
		doc["workspaceIdentifier"] = map[string]any{
			"uri": map[string]any{"$mid": 1, "fsPath": cwd, "path": cwd, "scheme": "file"},
		}
	}
	if model != "" {
		doc["modelConfig"] = map[string]any{"modelName": model}
	}
	return mustJSON(t, doc)
}

func bubbleJSON(t *testing.T, id string, assistant bool, at string, extra map[string]any) string {
	t.Helper()
	typ := 1
	if assistant {
		typ = 2
	}
	doc := map[string]any{"_v": 3, "type": typ, "bubbleId": id, "createdAt": at}
	for k, v := range extra {
		doc[k] = v
	}
	return mustJSON(t, doc)
}

func TestCursorChatReadsAPromptAndItsTools(t *testing.T) {
	const cid = "11111111-2222-3333-4444-555555555555"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/Users/x/lab/demo", "claude-opus-4-7",
			"user:u1", "assistant:a1", "assistant:a2", "user:u2", "assistant:a3")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z",
			map[string]any{"text": "add a slugify helper"})},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:10Z",
			map[string]any{"text": "I will look at the repo first."})},
		{"bubbleId:" + cid + ":a2", bubbleJSON(t, "a2", true, "2026-01-01T00:00:30Z",
			map[string]any{"toolFormerData": map[string]any{
				"toolCallId": "call-1", "name": "read_file_v2", "status": "completed",
				"params": `{"targetFile":"/Users/x/lab/demo/slugify.py"}`,
				"result": `{"contents":"..."}`,
			}})},
		{"bubbleId:" + cid + ":u2", bubbleJSON(t, "u2", false, "2026-01-01T00:05:00Z",
			map[string]any{"text": "now add a test"})},
		{"bubbleId:" + cid + ":a3", bubbleJSON(t, "a3", true, "2026-01-01T00:05:10Z",
			map[string]any{
				"text":       "Done.",
				"thinking":   map[string]any{"text": "the test file goes next to the module", "signature": "sig"},
				"tokenCount": map[string]any{"inputTokens": 1200, "outputTokens": 300},
			})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}

	if got, want := s.ID, cid; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
	if got, want := s.Source, CursorSourceName; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := s.Cwd, "/Users/x/lab/demo"; got != want {
		t.Errorf("Cwd = %q, want %q", got, want)
	}
	if got, want := strings.Join(s.Models, ","), "claude-opus-4-7"; got != want {
		t.Errorf("Models = %q, want %q", got, want)
	}
	if got, want := len(s.Prompts), 2; got != want {
		t.Fatalf("Prompts = %d, want %d", got, want)
	}
	if got, want := s.Prompts[1].Text, "now add a test"; got != want {
		t.Errorf("prompts[1] = %q, want %q", got, want)
	}
	// The second prompt's TurnIndex must point at the human turn, not at the
	// first one -- that index is what a per-turn collection slices on.
	if got, want := s.Turns[s.Prompts[1].TurnIndex].Text, "now add a test"; got != want {
		t.Errorf("Turns[TurnIndex] = %q, want the second prompt", got)
	}
	if got, want := len(s.ToolCalls), 1; got != want {
		t.Fatalf("ToolCalls = %d, want %d", got, want)
	}
	if got, want := s.ToolCalls[0].Name, "read_file_v2"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
	if !strings.Contains(string(s.ToolCalls[0].Input), "slugify.py") {
		t.Errorf("tool input lost its arguments: %s", s.ToolCalls[0].Input)
	}
	if got, want := s.ToolResults, 1; got != want {
		t.Errorf("ToolResults = %d, want %d", got, want)
	}
	// Only the last assistant bubble carried a token count; the total is
	// therefore a lower bound, and the note has to say so.
	if got, want := s.Usage.Total(), 1500; got != want {
		t.Errorf("Usage.Total() = %d, want %d", got, want)
	}
	if got := strings.Join(s.BaselineNotes, " | "); !strings.Contains(got, "lower bound") {
		t.Errorf("baseline notes do not flag the partial token total: %s", got)
	}
	if got := s.FinalOutput(); got != "Done." {
		t.Errorf("FinalOutput = %q, want %q", got, "Done.")
	}
	if s.ThinkingMs != 0 {
		t.Errorf("ThinkingMs = %d, want 0 when no bubble recorded a thinking clock", s.ThinkingMs)
	}
	// The reasoning is kept as evidence on the turn that produced it.
	if got := s.Turns[4].Thinking; got == "" {
		t.Errorf("thinking was dropped from the assistant turn")
	}
}

// Busy time is an estimate, so what it must NOT do matters more than what it
// does: the minutes a human spent reading before typing the next prompt are not
// machine time, and counting them would flatter every automated attempt.
func TestCursorBusyTimeExcludesTheHumanReadingTheAnswer(t *testing.T) {
	const cid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "",
			"user:u1", "assistant:a1", "assistant:a2", "user:u2", "assistant:a3")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z",
			map[string]any{"text": "start"})},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:10Z",
			map[string]any{"text": "step one"})},
		{"bubbleId:" + cid + ":a2", bubbleJSON(t, "a2", true, "2026-01-01T00:00:30Z",
			map[string]any{"text": "step two"})},
		// Five minutes later the human finally replies. That gap is theirs.
		{"bubbleId:" + cid + ":u2", bubbleJSON(t, "u2", false, "2026-01-01T00:05:00Z",
			map[string]any{"text": "keep going"})},
		{"bubbleId:" + cid + ":a3", bubbleJSON(t, "a3", true, "2026-01-01T00:05:10Z",
			map[string]any{"text": "step three"})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	// 10s + 20s of machine work, plus the 10s before the third answer. The
	// 270s the human took is excluded.
	if got, want := s.BusyMs, int64(40000); got != want {
		t.Errorf("BusyMs = %d, want %d (the human's 270s must not be counted)", got, want)
	}
	if got, want := s.DurationMs, int64(310000); got != want {
		t.Errorf("DurationMs = %d, want %d (wall clock keeps the human's time)", got, want)
	}
}

func TestCursorSkipsSubagentConversations(t *testing.T) {
	const parent = "parent-0001"
	const child = "child-0001"
	parentDoc := composerDataJSON(t, parent, "/x", "", "user:u1", "assistant:a1")
	var withChild map[string]any
	if err := json.Unmarshal([]byte(parentDoc), &withChild); err != nil {
		t.Fatal(err)
	}
	withChild["subagentComposerIds"] = []string{child}

	kvs := []cursorKV{
		{"composerData:" + parent, mustJSON(t, withChild)},
		{"composerData:" + child, composerDataJSON(t, child, "/x", "", "user:u1", "assistant:a1")},
		{"composerData:flagged-0002", composerDataJSON(t, "flagged-0002", "/x", "", "user:u1", "assistant:a1")},
		{"bubbleId:" + parent + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
		{"bubbleId:" + child + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "sub"})},
		{"bubbleId:flagged-0002:u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
	}
	db := writeCursorDB(t, kvs, []cursorHeaderRow{{id: "flagged-0002", subagent: true}})

	chats, err := CursorChats(db)
	if err != nil {
		t.Fatalf("CursorChats: %v", err)
	}
	// The child is named by its parent; the flagged one is named by the header
	// table. Neither is a human conversation, and both ways of saying so are
	// incomplete on their own, which is why the reader applies both.
	var ids []string
	for _, c := range chats {
		ids = append(ids, c.ID)
	}
	if len(ids) != 1 || ids[0] != parent {
		t.Errorf("chats = %v, want only %q", ids, parent)
	}
}

// composerHeaders is an incomplete index -- on the machine this was written
// against, 195 of 330 conversations had no row in it. Reading it as the index
// would silently lose more than half the history.
func TestCursorFindsConversationsMissingFromTheHeaderIndex(t *testing.T) {
	const cid = "noheader-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "", "user:u1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
	}
	db := writeCursorDB(t, kvs, nil) // no rows in composerHeaders at all

	chats, err := CursorChats(db)
	if err != nil {
		t.Fatalf("CursorChats: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != cid {
		t.Fatalf("chats = %+v, want the headerless conversation", chats)
	}
	if chats[0].Prompts != 1 {
		t.Errorf("Prompts = %d, want 1", chats[0].Prompts)
	}
}

func TestCursorListsNewestFirst(t *testing.T) {
	old := "old-0001"
	recent := "recent-0001"
	doc := func(id string, updated int64) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(composerDataJSON(t, id, "/x", "", "user:u1")), &m); err != nil {
			t.Fatal(err)
		}
		m["lastUpdatedAt"] = updated
		return mustJSON(t, m)
	}
	kvs := []cursorKV{
		{"composerData:" + old, doc(old, 1767225600000)},
		{"composerData:" + recent, doc(recent, 1767312000000)},
	}
	db := writeCursorDB(t, kvs, nil)

	chats, err := CursorChats(db)
	if err != nil {
		t.Fatalf("CursorChats: %v", err)
	}
	if len(chats) != 2 || chats[0].ID != recent {
		t.Fatalf("chats = %+v, want %q first", chats, recent)
	}
	latest, err := LatestCursorChat(db)
	if err != nil {
		t.Fatalf("LatestCursorChat: %v", err)
	}
	if latest.ID != recent {
		t.Errorf("LatestCursorChat = %q, want %q", latest.ID, recent)
	}
}

func TestCursorCwdSelectsOnlyThatWorkspace(t *testing.T) {
	a := "cwd-a-0001"
	b := "cwd-b-0001"
	kvs := []cursorKV{
		{"composerData:" + a, composerDataJSON(t, a, "/Users/x/one", "", "user:u1")},
		{"composerData:" + b, composerDataJSON(t, b, "/Users/x/two", "", "user:u1")},
	}
	db := writeCursorDB(t, kvs, nil)

	chats, err := CursorChatsForCwd(db, "/Users/x/one/")
	if err != nil {
		t.Fatalf("CursorChatsForCwd: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != a {
		t.Fatalf("chats = %+v, want only %q (a trailing separator must not matter)", chats, a)
	}
}

// A partial index should cost the ordering, not the content: a bubble the
// conversation does not list still happened, and dropping it would understate
// the trajectory.
func TestCursorKeepsBubblesTheIndexDoesNotList(t *testing.T) {
	const cid = "orphan-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "", "user:u1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
		{"bubbleId:" + cid + ":lost", bubbleJSON(t, "lost", true, "2026-01-01T00:00:05Z",
			map[string]any{"text": "an answer the index forgot"})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	if got := s.FinalOutput(); got != "an answer the index forgot" {
		t.Errorf("FinalOutput = %q, want the unlisted bubble to have been kept", got)
	}
}

// An edit_file_v2 call records neither params nor rawArgs in current builds;
// its target lives in codeBlocks. If the reader only looked at params it would
// report every edit as touching nothing.
func TestCursorReadsEditedFilesFromEditToolCalls(t *testing.T) {
	const cid = "edits-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "", "assistant:a1", "assistant:a2", "assistant:a3")},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:01Z",
			map[string]any{
				"toolFormerData": map[string]any{"name": "edit_file_v2"},
				"codeBlocks": []map[string]any{
					{"uri": map[string]any{"scheme": "file", "path": "/Users/x/lab/a.ts"}},
				},
			})},
		{"bubbleId:" + cid + ":a2", bubbleJSON(t, "a2", true, "2026-01-01T00:00:02Z",
			map[string]any{"toolFormerData": map[string]any{
				"name":   "apply_patch",
				"params": `{"relativeWorkspacePath":"/Users/x/lab/b.py"}`,
			}})},
		// A read names a path too. Listing it as an edit would be wrong in the
		// direction that matters: it would look like work that never happened.
		{"bubbleId:" + cid + ":a3", bubbleJSON(t, "a3", true, "2026-01-01T00:00:03Z",
			map[string]any{"toolFormerData": map[string]any{
				"name":   "read_file_v2",
				"params": `{"targetFile":"/Users/x/lab/c.py"}`,
			}})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	got := strings.Join(s.EditedFiles, ",")
	if want := "/Users/x/lab/a.ts,/Users/x/lab/b.py"; got != want {
		t.Errorf("EditedFiles = %q, want %q", got, want)
	}
}

// A corrupt bubble is a gap in one conversation, not a reason to lose the rest.
func TestCursorSurvivesUndecodableRecords(t *testing.T) {
	const cid = "broken-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "", "user:u1", "assistant:a1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
		{"bubbleId:" + cid + ":a1", "{not json"},
		{"composerData:junk-0001", "}{"},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	if len(s.Prompts) != 1 {
		t.Errorf("Prompts = %d, want 1: an undecodable sibling must not cost the prompt", len(s.Prompts))
	}
	chats, err := CursorChats(db)
	if err != nil {
		t.Fatalf("CursorChats: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != cid {
		t.Errorf("chats = %+v, want only the readable conversation", chats)
	}
}

func TestCursorMissingDatabaseIsReportedNotInvented(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", CursorDBName)
	if _, err := CursorChats(missing); err == nil {
		t.Fatal("CursorChats on a missing database returned no error")
	}
	if _, err := ParseCursorChat(missing, "x"); err == nil {
		t.Fatal("ParseCursorChat on a missing database returned no error")
	}
}

func TestCursorResolveReportsAnAmbiguousPrefix(t *testing.T) {
	kvs := []cursorKV{
		{"composerData:abc-1", composerDataJSON(t, "abc-1", "/x", "", "user:u1")},
		{"composerData:abc-2", composerDataJSON(t, "abc-2", "/x", "", "user:u1")},
		{"composerData:xyz-3", composerDataJSON(t, "xyz-3", "/x", "", "user:u1")},
	}
	db := writeCursorDB(t, kvs, nil)

	if _, err := ResolveCursor(db, "abc"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("ambiguous prefix: err = %v, want a complaint about 2 matches", err)
	}
	got, err := ResolveCursor(db, "xyz")
	if err != nil {
		t.Fatalf("ResolveCursor: %v", err)
	}
	if got.ID != "xyz-3" {
		t.Errorf("ResolveCursor = %q, want %q", got.ID, "xyz-3")
	}
	if _, err := ResolveCursor(db, "zzz"); err == nil || !strings.Contains(err.Error(), "--list") {
		t.Errorf("unmatched selector: err = %v, want it to point at the listing", err)
	}
}

// The model column must name a model. "default" is a setting, and printing it
// where a model name belongs reads as an answer.
func TestCursorDoesNotReportTheDefaultPlaceholderAsAModel(t *testing.T) {
	const cid = "model-0001"
	doc := composerDataJSON(t, cid, "/x", "default", "user:u1", "assistant:a1")
	// Cursor writes the placeholder into selectedModels as well as into
	// modelName, so both have to be filtered.
	var withSelected map[string]any
	if err := json.Unmarshal([]byte(doc), &withSelected); err != nil {
		t.Fatal(err)
	}
	withSelected["modelConfig"] = map[string]any{
		"modelName":      "default",
		"selectedModels": []map[string]any{{"modelId": "default"}},
	}

	kvs := []cursorKV{
		{"composerData:" + cid, mustJSON(t, withSelected)},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:01Z",
			map[string]any{
				"text":      "hi",
				"modelInfo": map[string]any{"modelName": "gpt-5.2"},
			})},
		// The placeholder turns up on bubbles too, not only on the conversation.
		{"bubbleId:" + cid + ":a2", bubbleJSON(t, "a2", true, "2026-01-01T00:00:02Z",
			map[string]any{
				"text":      "and again",
				"modelInfo": map[string]any{"modelName": "default"},
			})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	if got, want := strings.Join(s.Models, ","), "gpt-5.2"; got != want {
		t.Errorf("Models = %q, want %q (the placeholder must be dropped, the real one kept)", got, want)
	}
}

// One assistant bubble is one turn. The count is finer than a Claude Code
// message count, and the baseline has to say so -- a reader comparing the two
// columns side by side is otherwise comparing different units.
func TestCursorBaselineSaysTheTurnUnitIsNotClaudes(t *testing.T) {
	const cid = "unit-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "gpt-5.2", "user:u1", "assistant:a1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:01Z", map[string]any{"text": "hello"})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	notes := strings.Join(s.BaselineNotes, " | ")
	for _, want := range []string{"no cost", "per-request timing", "finer unit"} {
		if !strings.Contains(notes, want) {
			t.Errorf("baseline notes are missing %q: %s", want, notes)
		}
	}
	if got, want := s.sourceName(), CursorSourceName; got != want {
		t.Errorf("sourceName = %q, want %q", got, want)
	}
	if got, want := s.agentName(), "cursor"; got != want {
		t.Errorf("agentName = %q, want %q", got, want)
	}
}

// Collected conversations become tasks through the same path Claude Code
// transcripts use, which is the whole point of mapping into Session.
func TestCursorConversationCollectsLikeAnyOtherSession(t *testing.T) {
	const cid = "collect-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x/demo", "gpt-5.2", "user:u1", "assistant:a1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z",
			map[string]any{"text": "add a slugify helper"})},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:20Z",
			map[string]any{"text": "Added it."})},
	}
	db := writeCursorDB(t, kvs, nil)

	s, err := ParseCursorChat(db, cid)
	if err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if got, want := it.Task.Prompt, "add a slugify helper"; got != want {
		t.Errorf("task prompt = %q, want %q", got, want)
	}
	if got, want := it.Baseline.Source, CursorSourceName; got != want {
		t.Errorf("baseline source = %q, want %q (a reader must be able to tell the two apart)", got, want)
	}
	if got, want := it.Baseline.AgentName, "cursor"; got != want {
		t.Errorf("baseline agent = %q, want %q", got, want)
	}
	if got, want := it.Baseline.FinalOutput, "Added it."; got != want {
		t.Errorf("baseline final output = %q, want %q", got, want)
	}
	if got, want := it.Baseline.BusyMs, int64(20000); got != want {
		t.Errorf("baseline busy ms = %d, want %d", got, want)
	}
	if it.Task.Baseline == nil || it.Task.Baseline.File == "" {
		t.Fatalf("task has no baseline reference: %+v", it.Task.Baseline)
	}
	// The caveats travel into the baseline document itself, so a reader who
	// only has the dataset still sees them.
	if !strings.Contains(it.Baseline.Notes, "no cost") {
		t.Errorf("baseline notes lost the cursor caveats: %q", it.Baseline.Notes)
	}
}

func TestCursorDatabaseIsNotModified(t *testing.T) {
	const cid = "readonly-0001"
	kvs := []cursorKV{
		{"composerData:" + cid, composerDataJSON(t, cid, "/x", "gpt-5.2", "user:u1", "assistant:a1")},
		{"bubbleId:" + cid + ":u1", bubbleJSON(t, "u1", false, "2026-01-01T00:00:00Z", map[string]any{"text": "hi"})},
		{"bubbleId:" + cid + ":a1", bubbleJSON(t, "a1", true, "2026-01-01T00:00:01Z", map[string]any{"text": "yo"})},
	}
	db := writeCursorDB(t, kvs, nil)

	before := fileDigest(t, db)
	if _, err := ParseCursorChat(db, cid); err != nil {
		t.Fatalf("ParseCursorChat: %v", err)
	}
	if _, err := CursorChats(db); err != nil {
		t.Fatalf("CursorChats: %v", err)
	}
	if after := fileDigest(t, db); after != before {
		t.Errorf("the fixture database changed: %s -> %s. Collecting must not "+
			"write to the agent's own history.", before, after)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := openCursorDB(path)
	if err != nil {
		t.Fatalf("reopen fixture: %v", err)
	}
	defer f.Close()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM cursorDiskKV`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	var sig string
	if err := f.db.QueryRow(`SELECT GROUP_CONCAT(key, '|') FROM (SELECT key FROM cursorDiskKV ORDER BY key)`).Scan(&sig); err != nil {
		t.Fatalf("digest keys: %v", err)
	}
	return fmt.Sprintf("%d:%s", n, sig)
}

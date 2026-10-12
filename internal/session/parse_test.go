package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixture = "../../tests/testdata/session/basic.jsonl"

func parseFixture(t *testing.T) *Session {
	t.Helper()
	s, err := ParseFile(filepath.Join(fixture))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return s
}

func TestParseCountsUsageOncePerMessage(t *testing.T) {
	// Claude Code writes one record per content block, repeating `usage` on
	// each. A real session had 176 assistant records for 63 messages, so
	// summing per record would have inflated the token count roughly threefold.
	// The fixture's first assistant message is split into three records; its
	// usage must be counted once. 210 is 100+100 from the two messages the
	// cost-state covers plus 10 from the one after it: three messages, each
	// counted once. Counting per record would give 410.
	s := parseFixture(t)
	if got := s.Usage.InputTokens; got != 210 {
		t.Errorf("input tokens = %d, want 210 (three messages, not seven records)", got)
	}
	if got := s.Usage.OutputTokens; got != 32 {
		t.Errorf("output tokens = %d, want 32", got)
	}
	if got := s.Usage.CacheReadTokens; got != 50 {
		t.Errorf("cache read tokens = %d, want 50", got)
	}
	if got := s.Usage.Total(); got != 292 {
		t.Errorf("total = %d, want 292", got)
	}
}

func TestParseCompletesACostStateWithTheMessagesAfterIt(t *testing.T) {
	// The fixture's cost-state record lands before the last message, so its
	// 200/30 is the billed figure for a *prefix* of the session, not a total.
	// The record stays authoritative for what it covers -- it is what Claude
	// Code billed, and it stays right if a block is ever dropped from the
	// transcript -- but the message after it has to be added, or the session
	// total is whatever the checkpoint happened to reach. A real capture put
	// its last checkpoint at line 178 of 905 and under-reported its tokens
	// fourteenfold.
	s := parseFixture(t)
	if got := s.CostUSD; got != 0.25 {
		t.Errorf("cost = %v, want 0.25", got)
	}
	if got := s.Usage.InputTokens; got != 210 {
		t.Errorf("input tokens = %d, want 210: the record's 200 plus the message it does not cover", got)
	}
	if !strings.Contains(strings.Join(s.BaselineNotes, " "), "cost covers only the start") {
		t.Errorf("a partial cost must say so, got %q", s.BaselineNotes)
	}
}

func TestParseTrustsAFreshCostStateAsATotal(t *testing.T) {
	// Nothing follows this record, so it covers the session and is used as
	// written. This is what most captures look like, and the point of the
	// staleness check is that it leaves them alone.
	stream := `{"type":"user","timestamp":"2026-01-02T03:00:00.000Z","origin":{"kind":"human"},"message":{"role":"user","content":"do it"}}
{"type":"assistant","timestamp":"2026-01-02T03:00:05.000Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":999,"output_tokens":999,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
{"type":"cost-state","timestamp":"2026-01-02T03:00:06.000Z","totalCostUSD":1.5,"totalAPIDuration":4000,"totalToolDuration":2000,"totalDuration":6000,"modelUsage":{"m":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}
`
	s, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := s.Usage.InputTokens; got != 100 {
		t.Errorf("input tokens = %d, want the recorded 100, not the message's 999", got)
	}
	if got := s.BusyMs; got != 6000 {
		t.Errorf("busy = %d, want the recorded 6000", got)
	}
	if len(s.BaselineNotes) != 0 {
		t.Errorf("a fresh capture needs no caveat, got %q", s.BaselineNotes)
	}
}

func TestParseFindsOnlyHumanPrompts(t *testing.T) {
	// Three user-role records exist: the task, a tool result, and an injected
	// meta message. Only the first and the later "好的" are prompts.
	s := parseFixture(t)
	if len(s.Prompts) != 2 {
		t.Fatalf("got %d prompts, want 2: %+v", len(s.Prompts), s.Prompts)
	}
	if s.Prompts[0].Text != "修复 calc.py 里的减法错误" {
		t.Errorf("first prompt = %q", s.Prompts[0].Text)
	}
	if s.Prompts[1].Text != "好的" {
		t.Errorf("second prompt = %q", s.Prompts[1].Text)
	}
	if s.Prompts[0].Index != 0 || s.Prompts[1].Index != 1 {
		t.Errorf("prompt indices = %d,%d", s.Prompts[0].Index, s.Prompts[1].Index)
	}
}

func TestParseRecordsWhereEachPromptSits(t *testing.T) {
	// TurnIndex is what lets a per-turn collection slice the trajectory without
	// matching on prompt text, which would break the moment two prompts are
	// identical.
	s := parseFixture(t)
	if got := s.Prompts[0].TurnIndex; got != 0 {
		t.Errorf("first prompt TurnIndex = %d, want 0", got)
	}
	if got := s.Prompts[1].TurnIndex; got != 3 {
		t.Errorf("second prompt TurnIndex = %d, want 3", got)
	}
	if s.Turns[s.Prompts[1].TurnIndex].Text != "好的" {
		t.Error("TurnIndex does not point at its own prompt")
	}
}

func TestParseExcludesSubagentMessages(t *testing.T) {
	// A sidechain record belongs to a subagent. Keeping it would attribute a
	// subagent's tool call and its (very different) token spend to the task.
	s := parseFixture(t)
	for _, c := range s.ToolCalls {
		if c.Name == "Bash" {
			t.Fatal("a subagent tool call leaked into the main trajectory")
		}
	}
	if len(s.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2 (Read, Edit)", len(s.ToolCalls))
	}
	if s.Usage.InputTokens > 1000 {
		t.Errorf("subagent usage leaked into the total: %d", s.Usage.InputTokens)
	}
}

func TestParseKeepsBlocksOfOneMessageInOneTurn(t *testing.T) {
	// thinking + text + tool_use are three records but one turn.
	s := parseFixture(t)
	if len(s.Turns) != 5 {
		t.Fatalf("turns = %d, want 5", len(s.Turns))
	}
	first := s.Turns[1]
	if first.Role != RoleAssistant {
		t.Fatalf("turn 1 role = %q", first.Role)
	}
	if first.Text != "我先读一下文件。" {
		t.Errorf("turn 1 text = %q", first.Text)
	}
	if first.Thinking != "先看看文件" {
		t.Errorf("turn 1 thinking = %q", first.Thinking)
	}
	if len(first.Tools) != 1 || first.Tools[0].Name != "Read" {
		t.Errorf("turn 1 tools = %+v", first.Tools)
	}
	if s.Turns[2].Text != "已修复，减号写反了。" || len(s.Turns[2].Tools) != 1 {
		t.Errorf("turn 2 = %+v", s.Turns[2])
	}
}

func TestParseSeparatesWallClockFromBusyTime(t *testing.T) {
	// A session with a human in the loop is idle most of the time. Comparing an
	// unattended run's wall clock against the session's wall clock would flatter
	// the run; BusyMs is the comparable number. In a real session the two
	// differed by 8.3x, which is why both are recorded.
	//
	// The fixture's cost-state record is not the last thing in the session, so
	// its timers describe a prefix and are not used. The wall clock comes from
	// the session's own span (03:04:05 to 03:05:02) and the busy time from the
	// gaps that end at an assistant turn: 1s + 4s + 2s.
	s := parseFixture(t)
	if s.DurationMs != 57000 {
		t.Errorf("wall clock = %d, want 57000", s.DurationMs)
	}
	if s.BusyMs != 7000 {
		t.Errorf("busy = %d, want 7000 (the gaps ending at an assistant turn)", s.BusyMs)
	}
	// The record's own figures are kept: they are what the checkpoint said, and
	// a reader comparing them against the session total can see it was partial.
	if s.APIDurationMs != 4000 || s.ToolDurationMs != 2000 {
		t.Errorf("api/tool = %d/%d", s.APIDurationMs, s.ToolDurationMs)
	}
}

func TestParseCollectsProvenance(t *testing.T) {
	s := parseFixture(t)
	if s.ID != "aaaabbbb-1111-2222-3333-444455556666" {
		t.Errorf("session id = %q", s.ID)
	}
	if s.Cwd != "/work/proj" || s.GitBranch != "main" {
		t.Errorf("cwd/branch = %q/%q", s.Cwd, s.GitBranch)
	}
	if s.Version != "2.1.291" {
		t.Errorf("version = %q", s.Version)
	}
	if len(s.Models) != 1 || s.Models[0] != "test-model-1" {
		t.Errorf("models = %v", s.Models)
	}
	if got := s.EditedFiles; len(got) != 1 || got[0] != "/work/proj/calc.py" {
		t.Errorf("edited files = %v", got)
	}
	if s.ToolResults != 1 {
		t.Errorf("tool results = %d, want 1", s.ToolResults)
	}
	if s.ThinkingMs != 500 {
		t.Errorf("thinking = %d, want 500", s.ThinkingMs)
	}
	if got := turnDurationMs(s, 0); got != 6000 {
		t.Errorf("first turn's clock = %d, want 6000", got)
	}
	if got := turnDurationMs(s, 1); got != 0 {
		t.Errorf("second turn's clock = %d, want 0 (the build recorded none)", got)
	}
}

// turnDurationMs is the clock the session recorded for the turn the nth prompt
// started, summed over the turns that answer it.
func turnDurationMs(s *Session, promptIndex int) int64 {
	end := len(s.Turns)
	if promptIndex+1 < len(s.Prompts) {
		end = s.Prompts[promptIndex+1].TurnIndex
	}
	return sliceDurationMs(s.Turns[s.Prompts[promptIndex].TurnIndex:end])
}

func TestParseTimestampsBracketTheSession(t *testing.T) {
	s := parseFixture(t)
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !s.StartedAt.Equal(want) {
		t.Errorf("started = %v, want %v", s.StartedAt, want)
	}
	if !s.EndedAt.After(s.StartedAt) {
		t.Errorf("ended %v is not after started %v", s.EndedAt, s.StartedAt)
	}
}

func TestParseSurvivesAGarbledLine(t *testing.T) {
	// A partially corrupt capture stays usable; only I/O failures abort.
	s := parseFixture(t)
	if len(s.ParseErrors) != 1 {
		t.Fatalf("parse errors = %v, want exactly the one bad line", s.ParseErrors)
	}
	if !strings.Contains(s.ParseErrors[0], "line 14") {
		t.Errorf("parse error should name the line, got %q", s.ParseErrors[0])
	}
	if len(s.Prompts) != 2 {
		t.Error("a bad line must not stop the rest of the parse")
	}
}

func TestUnknownRecordTypesAreSkipped(t *testing.T) {
	// The fixture carries `mode` and `file-history-snapshot` records. Neither is
	// a message, and neither may abort the parse or invent a turn.
	s := parseFixture(t)
	if len(s.Turns) != 5 {
		t.Errorf("turns = %d, want 5", len(s.Turns))
	}
}

func TestFinalOutputIsTheLastAssistantProse(t *testing.T) {
	s := parseFixture(t)
	if got := s.FinalOutput(); got != "好的。" {
		t.Errorf("final output = %q, want the last non-empty assistant text", got)
	}
}

func TestParseReadsPromptsSentAsTextBlocks(t *testing.T) {
	// The IDE extensions send prompts through the SDK, which writes the content
	// as text blocks -- the typed request, then the attached file reference --
	// rather than a bare string. Reading only the bare-string form found no
	// prompt at all in such a session. A text block in a tool-result record is
	// still not a prompt.
	stream := `{"type":"user","timestamp":"2026-01-02T03:04:05.000Z","origin":{"kind":"human"},"promptSource":"sdk","message":{"role":"user","content":[{"type":"text","text":"metrics的前缀是什么"},{"type":"text","text":"[@A.java](file:///work/A.java)"}]}}
{"type":"user","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"not a prompt"}]}}
{"type":"user","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
`
	s, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Prompts) != 1 {
		t.Fatalf("got %d prompts, want 1: %+v", len(s.Prompts), s.Prompts)
	}
	want := "metrics的前缀是什么\n[@A.java](file:///work/A.java)"
	if s.Prompts[0].Text != want {
		t.Errorf("prompt = %q, want %q", s.Prompts[0].Text, want)
	}
	if s.ToolResults != 1 {
		t.Errorf("tool results = %d, want 1", s.ToolResults)
	}
}

func TestParseAttachesATurnClockToTheTurnItCloses(t *testing.T) {
	// A prompt typed while the agent is still working is written into the
	// transcript before the running turn's duration record lands. Reading the
	// clocks by their ordinal among prompts would hand every later turn the
	// clock of the one before it; the clock belongs to the assistant output it
	// followed.
	stream := `{"type":"user","timestamp":"2026-01-02T03:00:00.000Z","origin":{"kind":"human"},"message":{"role":"user","content":"first"}}
{"type":"assistant","timestamp":"2026-01-02T03:00:01.000Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"working"}]}}
{"type":"user","timestamp":"2026-01-02T03:00:02.000Z","origin":{"kind":"human"},"message":{"role":"user","content":"queued"}}
{"type":"system","subtype":"turn_duration","timestamp":"2026-01-02T03:00:06.000Z","durationMs":6000}
{"type":"assistant","timestamp":"2026-01-02T03:00:07.000Z","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"system","subtype":"turn_duration","timestamp":"2026-01-02T03:00:14.000Z","durationMs":7000}
`
	s, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(s.Turns))
	}
	if got := s.Turns[1].DurationMs; got != 6000 {
		t.Errorf("first turn's clock = %d, want 6000 on its own assistant output", got)
	}
	if got := s.Turns[2].DurationMs; got != 0 {
		t.Errorf("the queued prompt carries %d, want 0: a prompt is not a turn's clock", got)
	}
	if got := s.Turns[3].DurationMs; got != 7000 {
		t.Errorf("second turn's clock = %d, want 7000", got)
	}
}

func TestParseDropsAClockWithNoOutputToAttachTo(t *testing.T) {
	// The second record has no assistant output after the first, so there is
	// nothing it can belong to. Overwriting the first turn's clock would report
	// a duration the transcript never recorded for it.
	stream := `{"type":"user","timestamp":"2026-01-02T03:00:00.000Z","origin":{"kind":"human"},"message":{"role":"user","content":"first"}}
{"type":"assistant","timestamp":"2026-01-02T03:00:01.000Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"working"}]}}
{"type":"system","subtype":"turn_duration","timestamp":"2026-01-02T03:00:06.000Z","durationMs":6000}
{"type":"system","subtype":"turn_duration","timestamp":"2026-01-02T03:00:14.000Z","durationMs":9999}
`
	s, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := s.Turns[1].DurationMs; got != 6000 {
		t.Errorf("turn clock = %d, want the recorded 6000", got)
	}
}

func TestParseRejectsNothingForAnEmptyStream(t *testing.T) {
	s, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Prompts) != 0 || s.Usage.Total() != 0 {
		t.Errorf("empty stream produced %d prompts", len(s.Prompts))
	}
}

func TestProjectDirMatchesClaudeCodesEncoding(t *testing.T) {
	// /Users/x/lab -> -Users-x-lab. Lossy by design; used only to find files.
	got := ProjectDir("/root", "/Users/x/lab")
	if got != filepath.Join("/root", "-Users-x-lab") {
		t.Errorf("ProjectDir = %q", got)
	}
	if ProjectDir("", "/x") != "" || ProjectDir("/root", "") != "" {
		t.Error("ProjectDir with a missing half must be empty, not a path")
	}
}

func TestResolveAcceptsAPathAIdOrADirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	proj := filepath.Join(root, "-work-proj")
	if err := writeFile(filepath.Join(proj, "aaaabbbb-1111.jsonl"), "{}"); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(root, filepath.Join(proj, "aaaabbbb-1111.jsonl")); err != nil {
		t.Errorf("path selector: %v", err)
	}
	if _, err := Resolve(root, "aaaabbbb"); err != nil {
		t.Errorf("id prefix selector: %v", err)
	}
	if _, err := Resolve(root, "/work/proj"); err != nil {
		t.Errorf("directory selector: %v", err)
	}
	if _, err := Resolve(root, "nope-nope"); err == nil {
		t.Error("an unknown selector must fail")
	}
}

func TestSourcesAreOrderedNewestFirst(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "-a")
	if err := writeFile(filepath.Join(proj, "old.jsonl"), "{}"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(proj, "new.jsonl"), "{}"); err != nil {
		t.Fatal(err)
	}
	srcs, err := Sources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2", len(srcs))
	}
}

func TestSourcesOnAMissingRootIsNotAnError(t *testing.T) {
	// A machine that has never run Claude Code has no transcripts. That is a
	// state to report, not a failure to recover from.
	srcs, err := Sources(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("sources = %d, want 0", len(srcs))
	}
}

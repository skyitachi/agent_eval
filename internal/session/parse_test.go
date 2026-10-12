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
	// usage must be counted once.
	s := parseFixture(t)
	if got := s.Usage.InputTokens; got != 200 {
		t.Errorf("input tokens = %d, want 200 (100 per message, not 300 per record)", got)
	}
	if got := s.Usage.OutputTokens; got != 30 {
		t.Errorf("output tokens = %d, want 30", got)
	}
	if got := s.Usage.CacheReadTokens; got != 50 {
		t.Errorf("cache read tokens = %d, want 50", got)
	}
	if got := s.Usage.Total(); got != 280 {
		t.Errorf("total = %d, want 280", got)
	}
}

func TestParseTrustsCostStateOverTheSummedMessages(t *testing.T) {
	// The fixture's cost-state record lands before the last message, so the
	// per-message sum (210/32) deliberately disagrees with it (200/30). The
	// recorded total is authoritative: it is what Claude Code billed, and it is
	// the only figure that stays right if a block is ever dropped from the
	// transcript.
	s := parseFixture(t)
	if got := s.CostUSD; got != 0.25 {
		t.Errorf("cost = %v, want 0.25", got)
	}
	if got := s.Usage.InputTokens; got != 200 {
		t.Errorf("input tokens = %d, want the cost-state total 200, not the summed 210", got)
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
	s := parseFixture(t)
	if s.DurationMs != 60000 {
		t.Errorf("wall clock = %d, want 60000", s.DurationMs)
	}
	if s.BusyMs != 6000 {
		t.Errorf("busy = %d, want 6000 (api 4000 + tool 2000)", s.BusyMs)
	}
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
	if len(s.TurnDurations) != 1 || s.TurnDurations[0] != 6000 {
		t.Errorf("turn durations = %v", s.TurnDurations)
	}
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

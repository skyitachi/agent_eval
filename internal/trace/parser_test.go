package trace

import (
	"strings"
	"testing"
)

func TestParseRealStreamWithToolCall(t *testing.T) {
	tr, err := ParseFile("testdata/read_tool_stream.jsonl")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(tr.ParseErrors) > 0 {
		t.Fatalf("unexpected parse errors: %v", tr.ParseErrors)
	}
	if tr.Incomplete {
		t.Error("stream has a result event, Incomplete should be false")
	}

	// Environment from the init event.
	if tr.Model != "deepseek-v4-flash" {
		t.Errorf("model = %q", tr.Model)
	}
	if tr.ClaudeCodeVersion == "" {
		t.Error("claude_code_version should be captured")
	}
	if tr.PermissionMode != "acceptEdits" {
		t.Errorf("permission_mode = %q, want acceptEdits", tr.PermissionMode)
	}

	// The trajectory must contain exactly one Read tool call, joined to its result.
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (%+v)", len(tr.ToolCalls), tr.ToolCalls)
	}
	tc := tr.ToolCalls[0]
	if tc.Name != "Read" {
		t.Errorf("tool name = %q", tc.Name)
	}
	if !strings.Contains(string(tc.Input), "note.txt") {
		t.Errorf("tool input = %s", tc.Input)
	}
	if !strings.Contains(tc.Output, "platypus") {
		t.Errorf("tool result was not joined to the call: %q", tc.Output)
	}
	if tc.IsError {
		t.Error("Read should not be an error")
	}
	if tc.DurationMs < 0 {
		t.Errorf("negative duration %d", tc.DurationMs)
	}

	// Thinking blocks must be separated from answer text.
	if strings.TrimSpace(tr.ThinkingText()) == "" {
		t.Error("thinking text should be captured")
	}
	if got := strings.TrimSpace(tr.AssistantText()); got != "platypus" {
		t.Errorf("assistant text = %q, want platypus", got)
	}
	if tr.FinalText() != "platypus" {
		t.Errorf("FinalText = %q, want platypus", tr.FinalText())
	}

	// Terminal accounting.
	if tr.Result.NumTurns != 2 {
		t.Errorf("num_turns = %d, want 2", tr.Result.NumTurns)
	}
	if tr.Result.Usage.OutputTokens == 0 {
		t.Error("output tokens should be non-zero")
	}
	if tr.Result.Usage.InputTokens == 0 {
		t.Error("input tokens should be non-zero")
	}
	if tr.Result.DurationMs == 0 {
		t.Error("duration_ms should be non-zero")
	}
	if tr.Result.Subtype != "success" || tr.Result.IsError {
		t.Errorf("subtype=%q is_error=%v", tr.Result.Subtype, tr.Result.IsError)
	}

	// Cost on a third-party gateway reports costBasis "unknown", so CostReliable
	// must be false even though a number is present.
	if tr.CostReliable() {
		t.Error("deepseek gateway reports costBasis=unknown, CostReliable should be false")
	}

	if n := tr.ToolCounts()["Read"]; n != 1 {
		t.Errorf("ToolCounts[Read] = %d", n)
	}
	if tr.ToolErrorRate() != 0 {
		t.Errorf("ToolErrorRate = %v, want 0", tr.ToolErrorRate())
	}
}

func TestParseBareStreamHasNoLeakedUserConfig(t *testing.T) {
	tr, err := ParseFile("testdata/bare_stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.MCPServers) != 0 {
		t.Errorf("bare mode should expose no MCP servers, got %+v", tr.MCPServers)
	}
	if len(tr.Skills) != 0 {
		t.Errorf("bare mode should expose no skills, got %v", tr.Skills)
	}
	if len(tr.SlashCommands) != 0 {
		t.Errorf("bare mode should expose no slash commands, got %d", len(tr.SlashCommands))
	}
	if len(tr.ToolCalls) != 0 {
		t.Errorf("no tools should have been called, got %+v", tr.ToolCalls)
	}
	if tr.FinalText() != "PONG" {
		t.Errorf("FinalText = %q", tr.FinalText())
	}
}

func TestUsageTotalCountsEveryClass(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 100, CacheCreationInputTokens: 1}
	if u.Total() != 116 {
		t.Errorf("Total = %d, want 116", u.Total())
	}
}

func TestMalformedLinesAreRecordedNotFatal(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"m","session_id":"s"}`,
		`this is not json`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","result":"hi","num_turns":1}`,
	}, "\n")

	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse should tolerate bad lines, got %v", err)
	}
	if len(tr.ParseErrors) != 1 {
		t.Fatalf("parse errors = %v, want exactly 1", tr.ParseErrors)
	}
	if tr.FinalText() != "hi" {
		t.Errorf("parsing should continue after a bad line, FinalText = %q", tr.FinalText())
	}
}

func TestMissingResultEventMarksIncomplete(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working..."}]}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Incomplete {
		t.Error("a stream killed mid-flight must be flagged Incomplete")
	}
	if tr.FinalText() != "working..." {
		t.Errorf("FinalText should fall back to the last assistant text, got %q", tr.FinalText())
	}
}

func TestToolResultAcceptsBlockContent(t *testing.T) {
	// tool_result.content may be a string or a list of content blocks.
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a.txt"},{"type":"text","text":"b.txt"}]}]}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(tr.ToolCalls))
	}
	if got := tr.ToolCalls[0].Output; got != "a.txt\nb.txt" {
		t.Errorf("block tool result = %q, want %q", got, "a.txt\nb.txt")
	}
}

func TestToolResultErrorFlagPropagates(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"false"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"command failed"}]}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.ToolCalls[0].IsError {
		t.Error("is_error on the tool_result should mark the call")
	}
	if tr.ToolErrorRate() != 1 {
		t.Errorf("ToolErrorRate = %v, want 1", tr.ToolErrorRate())
	}
}

func TestMultipleToolCallsPairingIsOrdered(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{}},{"type":"tool_use","id":"b","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"second"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"first"}]}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d", len(tr.ToolCalls))
	}
	// Results arrive out of order but must bind by id, not by position.
	if tr.ToolCalls[0].Name != "Read" || tr.ToolCalls[0].Output != "first" {
		t.Errorf("call 0 = %+v", tr.ToolCalls[0])
	}
	if tr.ToolCalls[1].Name != "Bash" || tr.ToolCalls[1].Output != "second" {
		t.Errorf("call 1 = %+v", tr.ToolCalls[1])
	}
}

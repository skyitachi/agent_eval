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

// TestMultiResultStreamReportsTheSessionTotal is the regression that a real run
// found: the agent fanned out to a background search subagent, the stream
// carried two result events, and the tool stored 18,540 tokens for a session
// that had spent 164,419.
//
// The cause was reading the result event's `usage` field, which describes the
// single call that emitted that event. The second event's usage was the last
// call's (2,394 in); the session totals live in the per-model block, which both
// events reported identically.
//
// The fixture is a real stream, re-serialized from the trace's raw events, so it
// has no init line -- the parser stores that event as fields rather than as an
// event, and nothing here depends on it.
func TestMultiResultStreamReportsTheSessionTotal(t *testing.T) {
	tr, err := ParseFile("testdata/multi_result_stream.jsonl")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if tr.Incomplete {
		t.Error("Incomplete should be false: the stream ends with a result event")
	}
	nResults := 0
	for _, e := range tr.Events {
		if e.Kind == KindResult {
			nResults++
		}
	}
	if nResults != 2 {
		t.Fatalf("fixture should carry two result events, got %d", nResults)
	}

	got := tr.Usage()
	want := Usage{InputTokens: 72652, OutputTokens: 4983, CacheReadInputTokens: 86784}
	if got != want {
		t.Errorf("Usage = %+v, want %+v (total %d)", got, want, want.Total())
	}
	if got.Total() != 164419 {
		t.Errorf("total = %d, want 164419", got.Total())
	}

	// The trap has to still be present in the fixture, or this test would pass
	// against a stream that never had the problem. The single-call field is one
	// eleventh of the session, and the two must not be confused again.
	if tr.Result.Usage.Total() != 18540 {
		t.Errorf("fixture drifted: single-call usage total = %d, want 18540", tr.Result.Usage.Total())
	}
	if tr.Result.Usage.Total() == got.Total() {
		t.Fatal("fixture no longer distinguishes a call from a session; it cannot catch this bug")
	}

	// Two result events also used to fool the turn count: the last one claimed
	// num_turns 1 for a session of eleven turns.
	if tr.Result.NumTurns != 1 {
		t.Errorf("fixture drifted: result num_turns = %d, want 1", tr.Result.NumTurns)
	}
	if got, want := tr.Turns(), 11; got != want {
		t.Errorf("Turns = %d, want %d (the count derived from the stream)", got, want)
	}

	// The answer is the last event's, not the first: the first result was a note
	// that a background search was still running.
	if txt := tr.FinalText(); !strings.Contains(txt, "src/query.ts") {
		t.Errorf("FinalText should be the final answer, got %q", txt)
	}

	// Cost is cumulative like the totals, so it must survive the same way.
	if tr.CostUSD() < 0.53 {
		t.Errorf("CostUSD = %v, want the reported 0.531227", tr.CostUSD())
	}
}

// TestUsageFallsBackWhenTheEndpointOmitsPerModelTotals keeps the older shape
// working: not every gateway reports a per-model breakdown, and for those the
// result event's own usage is all there is.
func TestUsageFallsBackWhenTheEndpointOmitsPerModelTotals(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"result","subtype":"success","result":"hi","num_turns":1,` +
			`"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":5}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Usage(); got.Total() != 15 {
		t.Errorf("Usage = %+v, want the result event's own 15", got)
	}
}

// TestASmallerModelSnapshotDoesNotOverwriteALargerOne pins the merge rule. The
// two snapshots are of the same session, so a later and smaller one is a stale
// or partial view, not new work.
//
// Note the key spelling: on a real stream `usage` is snake_case but `modelUsage`
// is camelCase. Writing model_usage here parses to an empty map and the test
// silently measures nothing.
func TestASmallerModelSnapshotDoesNotOverwriteALargerOne(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"result","subtype":"success","result":"first","num_turns":2,` +
			`"total_cost_usd":0.9,"modelUsage":{"m":{"inputTokens":100,"outputTokens":10}}}`,
		`{"type":"result","subtype":"success","result":"second","num_turns":1,` +
			`"total_cost_usd":0.1,"modelUsage":{"m":{"inputTokens":40,"outputTokens":4}}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Result.ModelUsage) != 1 {
		t.Fatalf("fixture did not parse a model snapshot: %+v", tr.Result.ModelUsage)
	}
	if got := tr.Usage().Total(); got != 110 {
		t.Errorf("Usage total = %d, want the larger snapshot's 110", got)
	}
	if got := tr.CostUSD(); got != 0.9 {
		t.Errorf("CostUSD = %v, want the larger 0.9", got)
	}
	// The answer still comes from the last event.
	if got := tr.FinalText(); got != "second" {
		t.Errorf("FinalText = %q, want second", got)
	}
}

// TestModelSnapshotsFromDifferentModelsAddUp is the other half of the merge
// rule: summing across models is what makes a multi-model run's total honest.
func TestModelSnapshotsFromDifferentModelsAddUp(t *testing.T) {
	stream := `{"type":"result","subtype":"success","result":"x","num_turns":1,` +
		`"modelUsage":{"a":{"inputTokens":100},"b":{"inputTokens":23,"cacheReadInputTokens":7}}}`
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Usage().Total(); got != 130 {
		t.Errorf("Usage total = %d, want 130", got)
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

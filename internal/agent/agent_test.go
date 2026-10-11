package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/sandbox"
)

func TestBuildCommandDefaultsAreReproducible(t *testing.T) {
	cmd := BuildCommand(Spec{}, "11111111-2222-4333-8444-555555555555")

	for _, want := range []string{
		"claude",
		"-p",
		"--output-format stream-json",
		"--verbose",
		"--session-id 11111111-2222-4333-8444-555555555555",
		// Isolation must be on by default, otherwise host hooks and MCP servers
		// silently change agent behaviour between machines.
		"--bare",
		"--strict-mcp-config",
		"--disable-slash-commands",
		// Unattended runs must never wait for a permission prompt.
		"--permission-mode bypassPermissions",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--model") {
		t.Errorf("model should be omitted when unset:\n%s", cmd)
	}
}

func TestBuildCommandOmitsUnsetOptionalFlags(t *testing.T) {
	cmd := BuildCommand(Spec{Model: "sonnet"}, "s")
	for _, unwanted := range []string{
		"--fallback-model", "--effort", "--tools", "--allowedTools",
		"--disallowedTools", "--add-dir", "--mcp-config",
		"--system-prompt", "--append-system-prompt", "--settings",
		"--max-budget-usd", "--setting-sources",
	} {
		if strings.Contains(cmd, unwanted) {
			t.Errorf("unset flag %q leaked into command:\n%s", unwanted, cmd)
		}
	}
}

func TestBuildCommandQuotesUnsafeValues(t *testing.T) {
	// A model name is user input and reaches a shell; it must be quoted.
	spec := Spec{
		Model: "weird model; rm -rf /",
		Tools: []string{"Bash", "Read"},
		Env:   map[string]string{},
	}
	cmd := BuildCommand(spec, "s")
	if !strings.Contains(cmd, `--model 'weird model; rm -rf /'`) {
		t.Errorf("unsafe model value was not quoted:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--tools Bash Read") {
		t.Errorf("tool list not rendered as separate words:\n%s", cmd)
	}
}

func TestBuildCommandVariadicListsDoNotSwallowFollowingFlags(t *testing.T) {
	spec := Spec{
		Tools:        []string{"Bash", "Edit"},
		AllowedTools: []string{"Bash(git *)"},
		Model:        "sonnet",
	}
	cmd := BuildCommand(spec, "s")
	// --model must still be parsed as a flag, not consumed by --allowedTools.
	if !strings.Contains(cmd, "--model sonnet") {
		t.Fatalf("--model was consumed by a variadic flag:\n%s", cmd)
	}
	if !strings.Contains(cmd, `--allowedTools 'Bash(git *)'`) {
		t.Errorf("allowed tools not quoted:\n%s", cmd)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":       "simple",
		"with space":   "'with space'",
		"it's":         `'it'\''s'`,
		"a;b":          "'a;b'",
		"":             "''",
		"/a/b-c_d.py":  "/a/b-c_d.py",
		"model:tag=v1": "model:tag=v1",
		"$(whoami)":    "'$(whoami)'",
		"`id`":         "'`id`'",
		"line\nbreak":  "'line\nbreak'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// turn guard
// ---------------------------------------------------------------------------

func TestTurnGuardCountsDistinctAssistantMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newTurnGuard(3, cancel)

	// One assistant message split across three content blocks must count once.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"thinking"}]}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t"}]}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"

	if _, err := g.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if g.Turns() != 2 {
		t.Errorf("turns = %d, want 2", g.Turns())
	}
	if g.Exceeded() {
		t.Error("limit is 3, two turns should not exceed it")
	}
	select {
	case <-ctx.Done():
		t.Error("context should not be cancelled yet")
	default:
	}
}

func TestTurnGuardCancelsWhenExceeded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newTurnGuard(2, cancel)

	for _, id := range []string{"m1", "m2", "m3"} {
		line := `{"type":"assistant","message":{"id":"` + id + `","content":[]}}` + "\n"
		if _, err := g.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if !g.Exceeded() {
		t.Fatal("expected the budget to be exceeded")
	}
	select {
	case <-ctx.Done():
	default:
		t.Error("exceeding the budget must cancel the run")
	}
}

func TestTurnGuardHandlesSplitWrites(t *testing.T) {
	// JSON lines arrive in arbitrary chunks; a guard that only looked at
	// newline-terminated buffers would miss every event.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newTurnGuard(1, cancel)

	full := `{"type":"assistant","message":{"id":"m1","content":[]}}` + "\n" +
		`{"type":"assistant","message":{"id":"m2","content":[]}}` + "\n"
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		if _, err := g.Write([]byte(full[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	if g.Turns() != 2 {
		t.Errorf("turns = %d, want 2", g.Turns())
	}
	if !g.Exceeded() {
		t.Error("expected the budget to be exceeded")
	}
}

func TestTurnGuardDisabledWhenLimitIsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newTurnGuard(0, cancel)
	for _, id := range []string{"a", "b", "c", "d"} {
		_, _ = g.Write([]byte(`{"type":"assistant","message":{"id":"` + id + `"}}` + "\n"))
	}
	if g.Exceeded() {
		t.Error("limit 0 means unlimited")
	}
	select {
	case <-ctx.Done():
		t.Error("context should not be cancelled")
	default:
	}
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

func TestRegistryResolvesAliases(t *testing.T) {
	for _, kind := range []string{"claude_code", "claude-code", "claude", "ClaudeCode"} {
		r, err := New(kind)
		if err != nil {
			t.Errorf("New(%q): %v", kind, err)
			continue
		}
		if r.Name() != KindClaudeCode {
			t.Errorf("New(%q).Name() = %q", kind, r.Name())
		}
	}
	if _, err := New("nope"); err == nil {
		t.Error("unknown kind should error")
	}
	// An empty kind falls back to the default agent.
	r, err := New("")
	if err != nil || r.Name() != KindClaudeCode {
		t.Errorf("empty kind should default to claude_code, got %v / %v", r, err)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration should panic")
		}
	}()
	Register(KindMock, func() Runner { return &Mock{} })
}

// ---------------------------------------------------------------------------
// mock runner
// ---------------------------------------------------------------------------

func TestMockRunnerProducesGradeableTrace(t *testing.T) {
	sb, err := sandbox.NewLocal(sandbox.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(context.Background())

	spec := Spec{
		Kind: KindMock,
		// A stand-in "agent": it writes a file the way a real one would.
		Bin:   `printf 'print(1)\n' > solution.py && echo 'wrote solution.py'`,
		Model: "mock-model",
	}
	att, err := (&Mock{}).Run(context.Background(), sb, spec, "please write solution.py")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if att.Failed {
		t.Fatalf("mock run failed: %s", att.Error)
	}
	if att.Trace == nil {
		t.Fatal("trace should never be nil after a successful Run")
	}
	if got, _ := sb.ReadFile("solution.py"); string(got) != "print(1)\n" {
		t.Errorf("mock agent did not modify the workspace: %q", got)
	}
	if len(att.Trace.ToolCalls) != 1 || att.Trace.ToolCalls[0].Name != "Bash" {
		t.Errorf("synthesized trace should contain one Bash call, got %+v", att.Trace.ToolCalls)
	}
	if att.Trace.Turns() != 2 {
		t.Errorf("turns = %d, want 2", att.Trace.Turns())
	}
	if !strings.Contains(att.FinalText(), "wrote solution.py") {
		t.Errorf("final text = %q", att.FinalText())
	}
	if att.StopReason() != "completed" {
		t.Errorf("stop reason = %q", att.StopReason())
	}
}

func TestMockRunnerReportsNonZeroExitAsToolError(t *testing.T) {
	sb, err := sandbox.NewLocal(sandbox.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(context.Background())

	att, err := (&Mock{}).Run(context.Background(), sb, Spec{Kind: KindMock, Bin: "exit 4"}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if att.Trace == nil {
		t.Fatal("trace should be present so the failure stays gradeable")
	}
	if att.Trace.ToolErrorRate() != 1 {
		t.Errorf("a failing command should surface as a tool error, rate = %v", att.Trace.ToolErrorRate())
	}
	if att.StopReason() != "agent_error:error_during_execution" {
		t.Errorf("stop reason = %q, want agent_error:error_during_execution", att.StopReason())
	}
}

func TestStopReasonPrecedence(t *testing.T) {
	cases := []struct {
		name string
		att  Attempt
		want string
	}{
		{"timeout", Attempt{TimedOut: true}, "timeout"},
		{"turns", Attempt{TurnsExceeded: true}, "max_turns"},
		{"harness", Attempt{Failed: true}, "harness_error"},
		{"no trace", Attempt{}, "no_trace"},
	}
	for _, c := range cases {
		if got := c.att.StopReason(); got != c.want {
			t.Errorf("%s: StopReason() = %q, want %q", c.name, got, c.want)
		}
	}
}

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
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

// TestAttemptUsageReadsTheSessionTotalNotTheLastCall is the guard on the
// indirection: Trace.Usage has its own test, but nothing stops Attempt.Usage
// from reaching past it to Result.Usage again -- which is precisely the line
// that reported 18.5k tokens for a 164k-token run.
func TestAttemptUsageReadsTheSessionTotalNotTheLastCall(t *testing.T) {
	att := &Attempt{Trace: &trace.Trace{
		Result: trace.Result{
			Usage: trace.Usage{InputTokens: 2394, OutputTokens: 274, CacheReadInputTokens: 15872},
			ModelUsage: map[string]trace.ModelUsage{
				"m": {InputTokens: 72652, OutputTokens: 4983, CacheReadInputTokens: 86784},
			},
		},
	}}
	if got := att.Usage().Total(); got != 164419 {
		t.Errorf("Usage total = %d, want the per-model session total 164419", got)
	}
}

// A trace with nothing in it is the shape a killed run leaves behind; it must
// report nothing rather than panic.
func TestAttemptUsageOnAnEmptyTraceIsZero(t *testing.T) {
	if got := (&Attempt{}).Usage(); got.Total() != 0 {
		t.Errorf("Usage = %+v, want zero", got)
	}
	if got := (&Attempt{Trace: &trace.Trace{}}).Usage(); got.Total() != 0 {
		t.Errorf("Usage = %+v, want zero", got)
	}
}

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

// ---------------------------------------------------------------------------
// A run that never reached the model
// ---------------------------------------------------------------------------

// The prompt-side half of the diagnosis: only a bare command name counts.
//
// Rejecting paths matters more than it looks. A recorded turn that opens with
// /Users/me/x is a path, and reporting it as a command the agent failed to
// expose would manufacture a harness error out of an ordinary prompt.
func TestLeadingSlashCommandOnlyRecognisesABareCommandName(t *testing.T) {
	cases := []struct {
		prompt string
		want   string
		ok     bool
	}{
		{"/bff-oncall  测试环境 新服务", "bff-oncall", true},
		{"  /run\nnow", "run", true},
		{"/plugin:skill do it", "plugin:skill", true},
		{"/bff-oncall", "bff-oncall", true},
		{"/Users/me/lab fix the build", "", false},
		{"/README.md rewrite this", "", false},
		{"/", "", false},
		{"", "", false},
		{"just a sentence", "", false},
		{"   ", "", false},
		{"slash/in/the/middle", "", false},
	}
	for _, c := range cases {
		got, ok := LeadingSlashCommand(c.prompt)
		if ok != c.ok || got != c.want {
			t.Errorf("LeadingSlashCommand(%q) = (%q, %v), want (%q, %v)", c.prompt, got, ok, c.want, c.ok)
		}
	}
}

// A run whose prompt resolved and whose model answered must not be touched: the
// diagnosis exists for the absence of work, not for work that looks cheap.
func TestANoWorkRunIsAHarnessErrorNotAMeasurement(t *testing.T) {
	// What Claude Code 2.1.205 actually emits for an unexposed slash command:
	// a synthetic assistant message, no turns, no tokens, is_error false.
	unresolved := &trace.Trace{
		SlashCommands: nil,
		Result: trace.Result{
			Subtype:    "success",
			IsError:    false,
			NumTurns:   0,
			ResultText: "Unknown command: /bff-oncall",
		},
	}

	cases := []struct {
		name   string
		tr     *trace.Trace
		prompt string
		want   string // "" means the run is left alone
	}{
		{
			name:   "slash command the run did not expose",
			tr:     unresolved,
			prompt: "/bff-oncall  测试环境 新服务 76be4c8d",
			want:   "the agent never ran: the prompt invokes /bff-oncall, which this run did not expose (0 slash command(s) available)",
		},
		{
			name: "slash command the run did expose",
			tr: &trace.Trace{
				SlashCommands: []string{"other", "bff-oncall"},
				Result:        trace.Result{Subtype: "success", NumTurns: 0, ResultText: "Unknown command: /bff-oncall"},
			},
			prompt: "/bff-oncall now",
			// The command was on offer, so the CLI's refusal is its own; the
			// run is still a non-measurement but the prompt is not the reason.
			want: "the agent never ran: no turns, no tokens and no tool calls",
		},
		{
			name:   "an ordinary prompt that did no work",
			tr:     unresolved,
			prompt: "fix the failing test",
			want:   "the agent never ran: no turns, no tokens and no tool calls",
		},
		{
			name: "a model that answered",
			tr: &trace.Trace{
				Result: trace.Result{Subtype: "success", NumTurns: 3, ResultText: "done"},
			},
			prompt: "/bff-oncall now",
			want:   "",
		},
		{
			name: "tokens billed but no result turns",
			tr: &trace.Trace{
				Result: trace.Result{NumTurns: 0, Usage: trace.Usage{InputTokens: 12}},
			},
			prompt: "/bff-oncall now",
			want:   "",
		},
		{
			name: "a tool ran",
			tr: &trace.Trace{
				ToolCalls: []trace.ToolCall{{Name: "Read"}},
				Result:    trace.Result{NumTurns: 0},
			},
			prompt: "/bff-oncall now",
			want:   "",
		},
		{
			name: "the stream never terminated",
			tr: &trace.Trace{
				Incomplete: true,
				Result:     trace.Result{NumTurns: 0},
			},
			prompt: "/bff-oncall now",
			// Already reported as incomplete; a second reason would only
			// replace the truer one.
			want: "",
		},
		{
			name:   "no trace at all",
			tr:     nil,
			prompt: "/bff-oncall now",
			want:   "",
		},
	}
	for _, c := range cases {
		got := diagnoseNoWork(c.tr, c.prompt)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: diagnoseNoWork = %q, want the run left alone", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: diagnoseNoWork =\n  %q\nwant it to contain\n  %q", c.name, got, c.want)
		}
	}
}

// TestASkillDirectoryIsNotLoadedJustBecauseItIsDeclared is the distinction the
// run header and the comparison both rest on.
//
// The two flags remove the user's skills independently of one another, and the
// clean-room preset sets both, so the ordinary configuration is one where three
// declared plugin directories reach the agent not at all. A reader that takes
// PluginDirs for an answer reports that run as having skills loaded.
func TestASkillDirectoryIsNotLoadedJustBecauseItIsDeclared(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		active bool
		reason string
	}{
		{
			name:   "nothing declared",
			spec:   Spec{Isolation: &Isolation{}},
			active: false,
			reason: "",
		},
		{
			name:   "declared with both flags clear",
			spec:   Spec{PluginDirs: []string{"/skills/a"}, Isolation: &Isolation{}},
			active: true,
			reason: "",
		},
		{
			name:   "declared under the clean-room preset",
			spec:   Spec{PluginDirs: []string{"/skills/a"}, Isolation: &Isolation{Bare: true, DisableSlashCommands: true}},
			active: false,
			reason: "--bare and --disable-slash-commands removed them",
		},
		{
			name:   "declared with an omitted isolation means the preset",
			spec:   Spec{PluginDirs: []string{"/skills/a"}},
			active: false,
			reason: "--bare and --disable-slash-commands removed them",
		},
		{
			name:   "declared under bare alone",
			spec:   Spec{PluginDirs: []string{"/skills/a"}, Isolation: &Isolation{Bare: true}},
			active: false,
			reason: "--bare removed them",
		},
		{
			name:   "declared with slash commands alone disabled",
			spec:   Spec{PluginDirs: []string{"/skills/a"}, Isolation: &Isolation{DisableSlashCommands: true}},
			active: false,
			reason: "--disable-slash-commands removed them",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.SkillsActive(); got != c.active {
				t.Errorf("SkillsActive = %v, want %v", got, c.active)
			}
			if got := c.spec.SkillsReason(); got != c.reason {
				t.Errorf("SkillsReason = %q, want %q", got, c.reason)
			}
		})
	}
}

// TestDeclaredSkillsAreSortedForComparison: two specs that name the same
// directories in a different order configured the same agent, and a comparison
// that reports them as different is crying wolf about the one difference that
// matters.
func TestDeclaredSkillsAreSortedForComparison(t *testing.T) {
	spec := Spec{PluginDirs: []string{"/b", "/a"}}
	got := spec.SkillsDeclared()
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("SkillsDeclared = %v, want them sorted", got)
	}
	got[0] = "/mutated"
	if spec.PluginDirs[0] != "/b" {
		t.Error("SkillsDeclared must not hand back the spec's own slice")
	}
}

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
)

// ClaudeCode drives the `claude` CLI in headless mode.
//
// The prompt is written to the process's stdin rather than passed as an
// argument: prompts contain arbitrary text, and routing them through a shell
// command line would be both fragile and an injection surface.
type ClaudeCode struct{}

var _ Runner = (*ClaudeCode)(nil)

// NewClaudeCode returns the built-in Claude Code runner.
func NewClaudeCode() *ClaudeCode { return &ClaudeCode{} }

func (c *ClaudeCode) Name() string { return KindClaudeCode }

// Run implements Runner.
func (c *ClaudeCode) Run(ctx context.Context, sb sandbox.Sandbox, spec Spec, prompt string) (*Attempt, error) {
	spec = spec.withDefaults()

	sessionID := NewUUID()
	cmdline := BuildCommand(spec, sessionID)

	attempt := &Attempt{
		Agent:       c.Name(),
		Model:       spec.Model,
		Bin:         spec.Bin,
		SessionID:   sessionID,
		CommandLine: cmdline,
		StartedAt:   time.Now().UTC(),
	}

	// The turn budget is enforced here rather than by the CLI: current builds
	// expose no --max-turns flag, and doing it in the harness keeps behaviour
	// identical across sandbox backends.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	guard := newTurnGuard(spec.MaxTurns, cancel)

	res, err := sb.ExecStream(runCtx, cmdline, sandbox.ExecOptions{
		Env:     spec.resolvedEnv(),
		Timeout: spec.Timeout,
		Stdin:   prompt,
	}, sandbox.Streams{Stdout: guard})

	attempt.EndedAt = time.Now().UTC()
	if err != nil {
		attempt.Failed = true
		attempt.Error = err.Error()
		attempt.Duration = attempt.EndedAt.Sub(attempt.StartedAt)
		return attempt, nil
	}

	attempt.ExitCode = res.ExitCode
	attempt.TimedOut = res.TimedOut
	attempt.TurnsExceeded = guard.Exceeded()
	attempt.Duration = res.Duration
	attempt.Stderr = res.Stderr
	attempt.RawStream = res.Stdout

	tr, perr := trace.Parse(strings.NewReader(res.Stdout))
	if perr != nil {
		// strings.Reader cannot fail, so this indicates a programming error
		// rather than something the caller can fix.
		return attempt, fmt.Errorf("parse agent stream: %w", perr)
	}
	attempt.Trace = tr

	if reason := diagnoseEmptyRun(res); reason != "" {
		attempt.Failed = true
		attempt.Error = reason
	}
	if res.Err != nil && attempt.ExitCode < 0 && !attempt.TimedOut && !attempt.TurnsExceeded {
		attempt.Failed = true
		attempt.Error = res.Err.Error()
	}
	return attempt, nil
}

// diagnoseEmptyRun detects the common "the agent never started" cases, which
// otherwise look like a silently failing task and are hard to debug.
func diagnoseEmptyRun(res *sandbox.ExecResult) string {
	if strings.TrimSpace(res.Stdout) != "" || res.ExitCode == 0 {
		return ""
	}
	hint := ""
	low := strings.ToLower(res.Stderr)
	switch {
	case strings.Contains(low, "not found") || res.ExitCode == 127:
		hint = " (the agent binary is missing inside the sandbox; " +
			"for docker, add it to the image or mount it via sandbox.extra_mounts)"
	case strings.Contains(low, "unauthorized") || strings.Contains(low, "401") ||
		strings.Contains(low, "authentication"):
		hint = " (authentication failed; set agent.env or agent.env_from_host)"
	}
	detail := strings.TrimSpace(firstLine(res.Stderr))
	if detail == "" {
		detail = "(no stderr)"
	}
	return fmt.Sprintf("agent produced no stream output (exit %d): %s%s", res.ExitCode, detail, hint)
}

// ---------------------------------------------------------------------------
// Command construction
// ---------------------------------------------------------------------------

// flags holds the CLI flags for one invocation. Kept separate from Run so the
// exact command line can be unit tested without spawning a process.
type flags struct {
	Model                  string
	FallbackModel          string
	Effort                 string
	PermissionMode         string
	Tools                  []string
	AllowedTools           []string
	DisallowedTools        []string
	AddDirs                []string
	SystemPrompt           string
	AppendSystemPrompt     string
	AgentsJSON             string
	Settings               string
	MCPServers             []string
	SettingSources         []string
	Bare                   bool
	StrictMCPConfig        bool
	DisableSlashCommands   bool
	SessionID              string
	MaxBudgetUSD           float64
	IncludePartialMessages bool
	ExtraArgs              []string
}

// BuildCommand renders the full shell command line for one attempt.
//
// The agent is always invoked with `-p --output-format stream-json --verbose`,
// which is what makes the trajectory machine-readable.
func BuildCommand(spec Spec, sessionID string) string {
	spec = spec.withDefaults()
	iso := spec.Isolation
	f := flags{
		Model:                  spec.Model,
		FallbackModel:          spec.FallbackModel,
		Effort:                 spec.Effort,
		PermissionMode:         spec.PermissionMode,
		Tools:                  spec.Tools,
		AllowedTools:           spec.AllowedTools,
		DisallowedTools:        spec.DisallowedTools,
		AddDirs:                spec.AddDirs,
		SystemPrompt:           spec.SystemPrompt,
		AppendSystemPrompt:     spec.AppendSystemPrompt,
		AgentsJSON:             spec.AgentsJSON,
		Settings:               spec.Settings,
		MCPServers:             spec.MCPServers,
		SettingSources:         iso.SettingSources,
		Bare:                   iso.Bare,
		StrictMCPConfig:        iso.StrictMCPConfig,
		DisableSlashCommands:   iso.DisableSlashCommands,
		SessionID:              sessionID,
		MaxBudgetUSD:           spec.MaxBudgetUSD,
		IncludePartialMessages: spec.IncludePartialMessages,
		ExtraArgs:              spec.ExtraArgs,
	}
	return renderCommand(spec.Bin, f)
}

func renderCommand(bin string, f flags) string {
	parts := []string{
		shellQuote(bin),
		// -p is the headless switch; with no prompt argument the prompt is read
		// from stdin. stream-json + verbose is what yields the event stream.
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}
	if f.SessionID != "" {
		parts = append(parts, "--session-id", shellQuote(f.SessionID))
	}
	if f.Model != "" {
		parts = append(parts, "--model", shellQuote(f.Model))
	}
	if f.FallbackModel != "" {
		parts = append(parts, "--fallback-model", shellQuote(f.FallbackModel))
	}
	if f.Effort != "" {
		parts = append(parts, "--effort", shellQuote(f.Effort))
	}
	if f.PermissionMode != "" {
		parts = append(parts, "--permission-mode", shellQuote(f.PermissionMode))
	}
	// Variadic flags swallow every following non-flag token, so each list is
	// emitted immediately before the next flag.
	parts = appendList(parts, "--tools", f.Tools)
	parts = appendList(parts, "--allowedTools", f.AllowedTools)
	parts = appendList(parts, "--disallowedTools", f.DisallowedTools)
	parts = appendList(parts, "--add-dir", f.AddDirs)
	parts = appendList(parts, "--mcp-config", f.MCPServers)

	if f.SystemPrompt != "" {
		parts = append(parts, "--system-prompt", shellQuote(f.SystemPrompt))
	}
	if f.AppendSystemPrompt != "" {
		parts = append(parts, "--append-system-prompt", shellQuote(f.AppendSystemPrompt))
	}
	if f.AgentsJSON != "" {
		parts = append(parts, "--agents", shellQuote(f.AgentsJSON))
	}
	if f.Settings != "" {
		parts = append(parts, "--settings", shellQuote(f.Settings))
	}
	if len(f.SettingSources) > 0 {
		parts = append(parts, "--setting-sources", shellQuote(strings.Join(f.SettingSources, ",")))
	}
	if f.Bare {
		parts = append(parts, "--bare")
	}
	if f.StrictMCPConfig {
		parts = append(parts, "--strict-mcp-config")
	}
	if f.DisableSlashCommands {
		parts = append(parts, "--disable-slash-commands")
	}
	if f.MaxBudgetUSD > 0 {
		parts = append(parts, "--max-budget-usd", shellQuote(fmt.Sprintf("%g", f.MaxBudgetUSD)))
	}
	if f.IncludePartialMessages {
		parts = append(parts, "--include-partial-messages")
	}
	for _, a := range f.ExtraArgs {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func appendList(parts []string, flag string, values []string) []string {
	if len(values) == 0 {
		return parts
	}
	parts = append(parts, flag)
	for _, v := range values {
		parts = append(parts, shellQuote(v))
	}
	return parts
}

// ---------------------------------------------------------------------------
// Spec defaults and environment
// ---------------------------------------------------------------------------

// KindClaudeCode is the registry key for the Claude Code runner.
const KindClaudeCode = "claude_code"

// KindMock is the registry key for the deterministic mock runner.
const KindMock = "mock"

// withDefaults fills in unset fields with safe, reproducible values. It is
// idempotent, so it is safe to call from both Run and BuildCommand.
func (s Spec) withDefaults() Spec {
	if s.Kind == "" {
		s.Kind = KindClaudeCode
	}
	if s.Bin == "" {
		s.Bin = "claude"
	}
	if s.PermissionMode == "" {
		// Evaluation runs are unattended; prompting for permission would hang.
		s.PermissionMode = "bypassPermissions"
	}
	if s.Isolation == nil {
		iso := DefaultIsolation()
		s.Isolation = &iso
	}
	return s
}

// resolvedEnv composes the process environment for the agent.
func (s Spec) resolvedEnv() map[string]string {
	env := map[string]string{}
	for _, name := range s.EnvFromHost {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for k, v := range s.Env {
		env[k] = v
	}
	return env
}

// ---------------------------------------------------------------------------
// Turn budget enforcement
// ---------------------------------------------------------------------------

// turnGuard is an io.Writer that watches the stream-json output and cancels the
// run once the assistant turn budget is exhausted.
//
// A "turn" is counted per distinct assistant message id, matching the semantics
// of the CLI's own num_turns field. Counting raw events would over-count, since
// a single assistant message arrives as one event per content block.
type turnGuard struct {
	limit  int
	cancel context.CancelFunc

	mu       sync.Mutex
	partial  []byte
	seen     map[string]struct{}
	turns    int
	exceeded atomic.Bool
}

func newTurnGuard(limit int, cancel context.CancelFunc) *turnGuard {
	return &turnGuard{limit: limit, cancel: cancel, seen: map[string]struct{}{}}
}

func (g *turnGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.partial = append(g.partial, p...)
	for {
		i := bytes.IndexByte(g.partial, '\n')
		if i < 0 {
			break
		}
		line := g.partial[:i]
		// Copy before reslicing: the next append must not corrupt the line we
		// are about to read.
		g.partial = append([]byte(nil), g.partial[i+1:]...)
		g.observe(line)
	}
	return len(p), nil
}

func (g *turnGuard) observe(line []byte) {
	if g.limit <= 0 {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &ev); err != nil {
		return
	}
	if ev.Type != "assistant" || ev.Message.ID == "" {
		return
	}
	if _, dup := g.seen[ev.Message.ID]; dup {
		return
	}
	g.seen[ev.Message.ID] = struct{}{}
	g.turns++
	if g.turns > g.limit {
		g.exceeded.Store(true)
		if g.cancel != nil {
			g.cancel()
		}
	}
}

// Exceeded reports whether the turn budget was blown.
func (g *turnGuard) Exceeded() bool { return g.exceeded.Load() }

// Turns returns the number of turns observed so far.
func (g *turnGuard) Turns() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.turns
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

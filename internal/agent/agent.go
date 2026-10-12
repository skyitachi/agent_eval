// Package agent launches an agent CLI inside a sandbox and captures a
// normalized trajectory.
//
// The framework currently ships one implementation, ClaudeCode, but everything
// downstream (graders, metrics, storage, reports) depends only on the Attempt
// and trace.Trace types, so additional agents can be added without touching them.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Isolation controls how much of the host's Claude Code configuration is allowed
// to leak into an evaluation attempt.
//
// Reproducibility demands a clean room: a user's own hooks, MCP servers, slash
// commands and CLAUDE.md files all change agent behaviour, which makes scores
// incomparable between machines. `--bare` removes that surface entirely.
type Isolation struct {
	// Bare passes --bare: skips hooks (including user settings hooks), LSP,
	// plugin sync, attribution, auto-memory, keychain and CLAUDE.md
	// auto-discovery. Strongly recommended.
	Bare bool `json:"bare" yaml:"bare"`
	// StrictMCPConfig passes --strict-mcp-config so only servers named in
	// MCPServers are loaded.
	StrictMCPConfig bool `json:"strict_mcp_config" yaml:"strict_mcp_config"`
	// DisableSlashCommands removes user skills slash commands from the context.
	DisableSlashCommands bool `json:"disable_slash_commands" yaml:"disable_slash_commands"`
	// SettingSources maps to --setting-sources, e.g. ["project"] to accept only
	// repository-provided settings. Empty leaves the CLI default.
	SettingSources []string `json:"setting_sources,omitempty" yaml:"setting_sources,omitempty"`
}

// DefaultIsolation is the clean-room preset used unless a config overrides it.
func DefaultIsolation() Isolation {
	return Isolation{Bare: true, StrictMCPConfig: true, DisableSlashCommands: true}
}

// Spec is the configuration for one agent.
type Spec struct {
	// Kind selects a registered agent implementation. Default "claude_code".
	Kind string `json:"kind" yaml:"kind"`
	// Bin is the agent executable. Default "claude" (resolved through PATH
	// inside the sandbox).
	Bin string `json:"bin,omitempty" yaml:"bin,omitempty"`
	// Model is passed to --model. The literal value is forwarded untouched, so
	// both vendor aliases ("sonnet") and concrete ids ("deepseek-v4-pro") work.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// FallbackModel maps to --fallback-model.
	FallbackModel string `json:"fallback_model,omitempty" yaml:"fallback_model,omitempty"`
	// Effort maps to --effort when non-empty.
	Effort string `json:"effort,omitempty" yaml:"effort,omitempty"`

	// PermissionMode maps to --permission-mode, e.g. "bypassPermissions" for
	// fully unattended runs or "acceptEdits".
	PermissionMode string `json:"permission_mode,omitempty" yaml:"permission_mode,omitempty"`
	// Tools maps to --tools, the *available* tool set. Setting this explicitly
	// matters in bare mode, where the default set collapses to Bash/Edit/Read.
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	// AllowedTools maps to --allowedTools (auto-approve patterns).
	AllowedTools []string `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`
	// DisallowedTools maps to --disallowedTools.
	DisallowedTools []string `json:"disallowed_tools,omitempty" yaml:"disallowed_tools,omitempty"`
	// AddDirs maps to --add-dir for extra accessible directories.
	AddDirs []string `json:"add_dirs,omitempty" yaml:"add_dirs,omitempty"`

	// SystemPrompt replaces the system prompt (--system-prompt).
	SystemPrompt string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	// AppendSystemPrompt appends to the default system prompt.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty" yaml:"append_system_prompt,omitempty"`
	// AgentsJSON maps to --agents.
	AgentsJSON string `json:"agents_json,omitempty" yaml:"agents_json,omitempty"`
	// Settings maps to --settings: either a path inside the sandbox or an inline
	// JSON object.
	Settings string `json:"settings,omitempty" yaml:"settings,omitempty"`
	// MCPServers maps to --mcp-config entries (paths or JSON strings).
	MCPServers []string `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`

	// PluginDirs maps to --plugin-dir: directories of skills the agent may
	// load for this attempt. This is the hook the "skill" factor varies; it is
	// deliberately a list so that "no skills" is expressible as the empty list
	// rather than as a flag that has to be invented.
	//
	// Note that --plugin-dir coexists with --bare, but does NOT survive
	// --disable-slash-commands: that flag removes every skill from the context,
	// including these. A sweep over skills must therefore clear it.
	PluginDirs []string `json:"plugin_dirs,omitempty" yaml:"plugin_dirs,omitempty"`

	// Isolation, when nil, defaults to DefaultIsolation(). It is a pointer so that
	// an all-false configuration (deliberately accepting host configuration) is
	// distinguishable from an omitted one.
	Isolation *Isolation `json:"isolation,omitempty" yaml:"isolation,omitempty"`

	// MaxTurns bounds assistant turns. The limit is enforced by this harness
	// (the CLI has no --max-turns flag in current builds) by watching the event
	// stream and terminating the process when the budget is exhausted.
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	// MaxBudgetUSD maps to --max-budget-usd when positive.
	MaxBudgetUSD float64 `json:"max_budget_usd,omitempty" yaml:"max_budget_usd,omitempty"`
	// IncludePartialMessages maps to --include-partial-messages.
	IncludePartialMessages bool `json:"include_partial_messages,omitempty" yaml:"include_partial_messages,omitempty"`

	// ExtraArgs are appended verbatim before the prompt arguments. Use sparingly;
	// they are not validated and can break reproducibility.
	ExtraArgs []string `json:"extra_args,omitempty" yaml:"extra_args,omitempty"`

	// Env sets environment variables for the agent process.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// EnvFromHost copies named variables from the harness environment into the
	// sandbox. This is how credentials reach a Docker sandbox, whose environment
	// is otherwise isolated from the host.
	EnvFromHost []string `json:"env_from_host,omitempty" yaml:"env_from_host,omitempty"`

	// Timeout bounds the whole attempt.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Attempt is the complete record of one agent run against one task.
type Attempt struct {
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
	Bin   string `json:"bin,omitempty"`
	// SessionID is the UUID handed to the CLI, enabling a transcript to be
	// resumed or correlated after the fact.
	SessionID   string `json:"session_id,omitempty"`
	CommandLine string `json:"command_line,omitempty"`

	ExitCode      int  `json:"exit_code"`
	TimedOut      bool `json:"timed_out"`
	TurnsExceeded bool `json:"turns_exceeded"`
	// Failed reports an infrastructure failure (the agent never really ran).
	Failed bool `json:"failed"`

	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Duration  time.Duration `json:"duration"`

	// Trace is the normalized trajectory. Never nil on a successful invocation
	// of Run, even when the CLI crashed mid-stream.
	Trace *trace.Trace `json:"trace,omitempty"`

	// Stderr is the agent process's stderr, kept for diagnosing CLI failures.
	Stderr string `json:"stderr,omitempty"`
	// RawStream is the verbatim stream-json output, persisted for auditing.
	RawStream string `json:"-"`

	// Error describes an infrastructure failure when Failed is true.
	Error string `json:"error,omitempty"`
}

// StopReason summarises why the attempt ended, in a form suitable for storage
// and for conditioning grader behaviour.
func (a *Attempt) StopReason() string {
	switch {
	case a.TimedOut:
		return "timeout"
	case a.TurnsExceeded:
		return "max_turns"
	case a.Failed:
		return "harness_error"
	case a.Trace == nil:
		return "no_trace"
	case a.Trace.Incomplete:
		return "incomplete"
	case a.Trace.Result.IsError:
		if a.Trace.Result.Subtype != "" {
			return "agent_error:" + a.Trace.Result.Subtype
		}
		return "agent_error"
	case a.ExitCode != 0:
		return fmt.Sprintf("exit_%d", a.ExitCode)
	default:
		return "completed"
	}
}

// FinalText exposes the agent's answer.
func (a *Attempt) FinalText() string {
	if a.Trace == nil {
		return ""
	}
	return a.Trace.FinalText()
}

// Usage returns token accounting, zero when no trace was produced.
//
// This is the whole attempt, not one call: Trace.Usage prefers the CLI's
// cumulative per-model totals over the single-call usage on a result event.
func (a *Attempt) Usage() trace.Usage {
	if a.Trace == nil {
		return trace.Usage{}
	}
	return a.Trace.Usage()
}

// CostUSD returns the reported cost.
func (a *Attempt) CostUSD() float64 {
	if a.Trace == nil {
		return 0
	}
	return a.Trace.CostUSD()
}

// Runner executes one agent attempt inside a sandbox.
type Runner interface {
	// Name identifies the implementation, matching Spec.Kind.
	Name() string
	// Run performs one attempt and returns a populated Attempt.
	//
	// A non-nil error signals an infrastructure problem (the sandbox could not
	// start the process at all); an agent that ran but failed is reported through
	// Attempt instead, so that partial results remain gradeable.
	Run(ctx context.Context, sb sandbox.Sandbox, spec Spec, prompt string) (*Attempt, error)
}

// ResolveCommand resolves something that may be either a path or a bare command
// name.
//
// A value with no path separator is returned unchanged, because `bin: claude`
// means "find claude on PATH" and turning it into /dir/claude would break every
// invocation that does not spell out an absolute path. Anything else is joined
// with dir when relative.
//
// This rule appears in more than one place -- a command-line flag resolved
// against the caller's directory, and bin inside a matrix document resolved
// against the document's directory -- and they have to agree.
func ResolveCommand(dir, p string) string {
	if strings.TrimSpace(p) == "" || strings.TrimSpace(dir) == "" {
		return p
	}
	if !strings.ContainsRune(p, filepath.Separator) {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(dir, p))
}

// ResolveDir resolves a directory path. Unlike an executable there is no PATH
// to fall back on, so a bare value is always joined with dir rather than left
// relative to whatever directory the process happens to be in.
//
// Getting this wrong is not cosmetic. A plugin directory that silently resolves
// to nothing loads no skills at all, and a skill sweep then reports that skills
// make no difference -- the exact false conclusion the sweep's own validation
// exists to prevent.
func ResolveDir(dir, p string) string {
	if strings.TrimSpace(p) == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if strings.TrimSpace(dir) == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(dir, p))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// shellQuote renders s as a single-quoted shell word, safe to embed in the
// command string handed to `sh -c`.
//
// The agent's prompt is deliberately NOT passed this way -- it travels on stdin
// instead, because prompts routinely contain quotes, backticks, newlines and
// shell metacharacters that would otherwise be an injection surface.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// A conservative safe set; anything outside it gets quoted.
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("._-/:@=+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agent: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

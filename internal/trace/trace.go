// Package trace turns Claude Code's `--output-format stream-json` event stream
// into a normalized, queryable trajectory.
//
// The raw stream has two properties that make naive parsing wrong:
//
//  1. Assistant messages are emitted as one event per content block, all sharing
//     the same message id. Token usage repeated on each fragment must therefore
//     be deduplicated by message id; the authoritative aggregate lives on the
//     final `result` event.
//  2. Tool results arrive on `user` events, correlated by `tool_use_id`, and may
//     carry either a plain string body or a list of content blocks.
//
// Both are handled here so downstream graders and metrics can work with a flat,
// well-typed trajectory.
package trace

import (
	"encoding/json"
	"time"
)

// Kind classifies a normalized event.
type Kind string

const (
	KindInit           Kind = "init"
	KindAssistantText  Kind = "assistant_text"
	KindAssistantThink Kind = "assistant_thinking"
	KindToolUse        Kind = "tool_use"
	KindToolResult     Kind = "tool_result"
	KindResult         Kind = "result"
	KindUserMessage    Kind = "user_message"
	KindUnknown        Kind = "unknown"
)

// Event is one normalized step in the trajectory.
type Event struct {
	// Index is the position in the raw stream (0-based).
	Index int  `json:"index"`
	Kind  Kind `json:"kind"`

	// Timestamp is taken from the event when present, otherwise zero.
	Timestamp time.Time `json:"timestamp,omitempty"`

	// Text is the payload for text/thinking/user-message events.
	Text string `json:"text,omitempty"`

	// Tool fields, set for KindToolUse and KindToolResult.
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput string          `json:"tool_output,omitempty"`
	ToolError  bool            `json:"tool_error,omitempty"`

	// ThinkingDurationMs is reported by some builds on thinking blocks.
	ThinkingDurationMs int64 `json:"thinking_duration_ms,omitempty"`

	// Raw preserves the original JSON line for auditing / re-parsing.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// ToolCall is a tool invocation joined with its result.
type ToolCall struct {
	// Seq is the 1-based order of invocation within the trajectory.
	Seq     int             `json:"seq"`
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error"`
	// StartedAt/EndedAt come from event timestamps and may be zero.
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// DurationMs is wall-clock time between the tool_use and its tool_result.
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// Usage aggregates token accounting **for one API call**.
//
// This is the shape of the `usage` field on a result event, and that field
// describes the call that produced that event -- not the session. Do not treat
// it as the run's total; use Trace.Usage for that. The distinction is invisible
// in a stream that emits one result event whose usage happens to be cumulative,
// and glaring in one that emits several: a run that fanned out to a background
// search reported 18.5k here against a real total of 164k.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	ThinkingTokens           int `json:"thinking_tokens"`
}

// Total returns the sum of all billed token classes.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// Total returns the sum of all billed token classes.
func (m ModelUsage) Total() int {
	return m.InputTokens + m.OutputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens
}

// ModelUsage is per-model accounting as reported by the CLI.
type ModelUsage struct {
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	ThinkingTokens           int     `json:"thinking_tokens"`
	CostUSD                  float64 `json:"cost_usd"`
	ContextWindow            int     `json:"context_window,omitempty"`
	MaxOutputTokens          int     `json:"max_output_tokens,omitempty"`
	CanonicalModel           string  `json:"canonical_model,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
	// CostBasis records how cost was derived ("unknown" when the endpoint does
	// not report a price, which is common for third-party Anthropic-compatible
	// gateways such as DeepSeek's).
	CostBasis string `json:"cost_basis,omitempty"`
}

// MCPServerStatus is one entry from the init event.
type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
}

// Result is the terminal summary emitted by the CLI.
type Result struct {
	Subtype           string                `json:"subtype"`
	IsError           bool                  `json:"is_error"`
	ResultText        string                `json:"result"`
	SessionID         string                `json:"session_id"`
	NumTurns          int                   `json:"num_turns"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	Usage             Usage                 `json:"usage"`
	ModelUsage        map[string]ModelUsage `json:"model_usage,omitempty"`
	DurationMs        int64                 `json:"duration_ms"`
	APIDurationMs     int64                 `json:"api_duration_ms"`
	TTFTMs            int64                 `json:"ttft_ms"`
	TTFTStreamMs      int64                 `json:"ttft_stream_ms,omitempty"`
	StopReason        string                `json:"stop_reason,omitempty"`
	TerminalReason    string                `json:"terminal_reason,omitempty"`
	PermissionDenials []json.RawMessage     `json:"permission_denials,omitempty"`
	APIErrorStatus    json.RawMessage       `json:"api_error_status,omitempty"`
	Raw               json.RawMessage       `json:"-"`
}

// Trace is the normalized trajectory of one agent attempt.
type Trace struct {
	// ----- environment as reported by the init event -----
	SessionID         string            `json:"session_id,omitempty"`
	Model             string            `json:"model,omitempty"`
	CWD               string            `json:"cwd,omitempty"`
	ClaudeCodeVersion string            `json:"claude_code_version,omitempty"`
	PermissionMode    string            `json:"permission_mode,omitempty"`
	APIKeySource      string            `json:"api_key_source,omitempty"`
	Tools             []string          `json:"tools,omitempty"`
	MCPServers        []MCPServerStatus `json:"mcp_servers,omitempty"`
	Skills            []string          `json:"skills,omitempty"`
	SlashCommands     []string          `json:"slash_commands,omitempty"`

	// ----- trajectory -----
	Events    []Event    `json:"events"`
	ToolCalls []ToolCall `json:"tool_calls"`

	// ----- outcome -----
	Result Result `json:"result"`

	// TurnCount is the number of assistant turns observed in the stream, counted
	// per distinct assistant message id. It is populated even when the run was
	// killed before emitting a result event, which is exactly the case where the
	// CLI's own num_turns is unavailable.
	TurnCount int `json:"turn_count"`

	// ----- diagnostics -----
	// ParseErrors collects lines that could not be decoded. A non-empty list
	// means the trace is incomplete and the attempt should be treated carefully.
	ParseErrors []string `json:"parse_errors,omitempty"`
	// Incomplete is true when the stream ended without a terminal result event,
	// which indicates a timeout, crash, or kill.
	Incomplete bool `json:"incomplete"`

	// sawResult records whether a terminal `result` event was observed.
	sawResult bool
	// seenMsgIDs deduplicates assistant messages, which arrive as one event per
	// content block all sharing a message id.
	seenMsgIDs map[string]struct{}
}

// ---------------------------------------------------------------------------
// Derived views used by graders, metrics and reports
// ---------------------------------------------------------------------------

// FinalText returns the agent's terminal answer: the result text when present,
// otherwise the last assistant text block.
func (t *Trace) FinalText() string {
	if t.Result.ResultText != "" {
		return t.Result.ResultText
	}
	for i := len(t.Events) - 1; i >= 0; i-- {
		if t.Events[i].Kind == KindAssistantText {
			return t.Events[i].Text
		}
	}
	return ""
}

// AssistantText concatenates every assistant text block in order.
func (t *Trace) AssistantText() string {
	var b []byte
	for _, e := range t.Events {
		if e.Kind == KindAssistantText {
			b = append(b, e.Text...)
			b = append(b, '\n')
		}
	}
	return string(b)
}

// ThinkingText concatenates every thinking block (useful for reasoning metrics).
func (t *Trace) ThinkingText() string {
	var b []byte
	for _, e := range t.Events {
		if e.Kind == KindAssistantThink {
			b = append(b, e.Text...)
			b = append(b, '\n')
		}
	}
	return string(b)
}

// Usage returns the token accounting for the whole attempt.
//
// It sums the per-model totals, which the CLI reports as a running total for the
// session, and falls back to the result event's own usage only when the endpoint
// supplied no per-model breakdown. The result event's `usage` field is not a
// substitute: it describes the call that emitted that event, so reading it means
// reporting one call as if it were the run.
//
// Summing per model also makes the number comparable to a baseline recorded by
// `aeval collect`, which adds up per message. Both sides then answer the same
// question -- what did this cost in total -- and the comparison means something.
func (t *Trace) Usage() Usage {
	if len(t.Result.ModelUsage) == 0 {
		return t.Result.Usage
	}
	var u Usage
	for _, m := range t.Result.ModelUsage {
		u.InputTokens += m.InputTokens
		u.OutputTokens += m.OutputTokens
		u.CacheReadInputTokens += m.CacheReadInputTokens
		u.CacheCreationInputTokens += m.CacheCreationInputTokens
		u.ThinkingTokens += m.ThinkingTokens
	}
	return u
}

// CostUSD returns the reported cost. Zero means "not reported" when CostBasis
// is "unknown"; use CostReliable to distinguish.
func (t *Trace) CostUSD() float64 { return t.Result.TotalCostUSD }

// CostReliable reports whether the endpoint supplied a real price rather than a
// placeholder. Third-party gateways frequently return costBasis "unknown".
func (t *Trace) CostReliable() bool {
	if len(t.Result.ModelUsage) == 0 {
		return t.Result.TotalCostUSD > 0
	}
	for _, mu := range t.Result.ModelUsage {
		if mu.CostBasis == "unknown" || mu.CostBasis == "" {
			return false
		}
	}
	return true
}

// Turns returns the number of assistant turns.
//
// The count derived from the stream wins over the CLI's num_turns, because
// num_turns is not a session total. A run that emitted two result events
// reported num_turns 1 and 2 for a session that had taken eleven turns; taking
// the last event's word for it would have recorded a third of the work.
// TurnCount is defined as distinct assistant message ids, so it means the same
// thing however many result events turn up. num_turns is the fallback for a
// stream we could not count at all.
func (t *Trace) Turns() int {
	if t.TurnCount > 0 {
		return t.TurnCount
	}
	return t.Result.NumTurns
}

// ToolCounts returns per-tool invocation counts.
func (t *Trace) ToolCounts() map[string]int {
	m := map[string]int{}
	for _, c := range t.ToolCalls {
		m[c.Name]++
	}
	return m
}

// ToolErrorCount counts tool calls that returned an error.
func (t *Trace) ToolErrorCount() int {
	n := 0
	for _, c := range t.ToolCalls {
		if c.IsError {
			n++
		}
	}
	return n
}

// ToolErrorRate is the fraction of tool calls that errored (0 when no calls).
func (t *Trace) ToolErrorRate() float64 {
	if len(t.ToolCalls) == 0 {
		return 0
	}
	return float64(t.ToolErrorCount()) / float64(len(t.ToolCalls))
}

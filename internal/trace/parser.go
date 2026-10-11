package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// maxLine bounds a single stream line. Tool results can be large.
const maxLine = 64 << 20

// Parse reads a Claude Code `stream-json` stream and returns the normalized trace.
//
// It never fails on an individual malformed line: undecodable lines are recorded
// in Trace.ParseErrors and parsing continues, so a partially corrupt capture is
// still usable. A non-nil error is only returned for I/O failures.
func Parse(r io.Reader) (*Trace, error) {
	t := &Trace{
		Result:     Result{ModelUsage: map[string]ModelUsage{}},
		seenMsgIDs: map[string]struct{}{},
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	idx := 0
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var line rawLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.ParseErrors = append(t.ParseErrors, fmt.Sprintf("line %d: %v", idx+1, err))
			idx++
			continue
		}
		rawCopy := json.RawMessage(append([]byte(nil), raw...))
		ts := parseTime(line.Timestamp)

		switch line.Type {
		case "system":
			if line.Subtype == "init" {
				t.applyInit(&line)
			}
			// "thinking_tokens" is a progress ticker; not a trajectory step.

		case "assistant":
			t.applyAssistant(&line, idx, ts, rawCopy)

		case "user":
			t.applyUser(&line, idx, ts, rawCopy)

		case "result":
			t.applyResult(&line, rawCopy)

		default:
			t.Events = append(t.Events, Event{
				Index: idx, Kind: KindUnknown, Timestamp: ts, Raw: rawCopy,
			})
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return t, fmt.Errorf("read stream: %w", err)
	}

	// Any tool call that never received a result stays marked and unfinished.
	t.Incomplete = !t.sawResult
	return t, nil
}

// ParseFile parses a captured stream from disk.
func ParseFile(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// ---------------------------------------------------------------------------
// Raw wire shapes
// ---------------------------------------------------------------------------

type rawLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	CWD               string            `json:"cwd"`
	SessionID         string            `json:"session_id"`
	Tools             []string          `json:"tools"`
	MCPServers        []MCPServerStatus `json:"mcp_servers"`
	Model             string            `json:"model"`
	PermissionMode    string            `json:"permissionMode"`
	APIKeySource      string            `json:"apiKeySource"`
	Skills            []string          `json:"skills"`
	SlashCommands     []string          `json:"slash_commands"`
	ClaudeCodeVersion string            `json:"claude_code_version"`

	// assistant / user
	Message            *rawMessage     `json:"message"`
	Timestamp          string          `json:"timestamp"`
	ThinkingDurationMs int64           `json:"thinking_duration_ms"`
	ParentToolUseID    string          `json:"parent_tool_use_id"`
	ToolUseResult      json.RawMessage `json:"tool_use_result"`

	// result
	DurationAPIMs     int64                    `json:"duration_api_ms"`
	StopReason        string                   `json:"stop_reason"`
	TotalCostUSD      float64                  `json:"total_cost_usd"`
	Usage             *rawUsage                `json:"usage"`
	ModelUsage        map[string]rawModelUsage `json:"modelUsage"`
	PermissionDenials []json.RawMessage        `json:"permission_denials"`
	TerminalReason    string                   `json:"terminal_reason"`
	IsError           bool                     `json:"is_error"`
	NumTurns          int                      `json:"num_turns"`
	Result            string                   `json:"result"`
	TTFTMs            int64                    `json:"ttft_ms"`
	TTFTStreamMs      int64                    `json:"ttft_stream_ms"`
	DurationMs        int64                    `json:"duration_ms"`
	APIErrorStatus    json.RawMessage          `json:"api_error_status"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
}

type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type rawModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	ThinkingTokens           int     `json:"thinkingTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
	CanonicalModel           string  `json:"canonicalModel"`
	Provider                 string  `json:"provider"`
	CostBasis                string  `json:"costBasis"`
}

// block covers every content-block shape we have observed.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ---------------------------------------------------------------------------
// Appliers
// ---------------------------------------------------------------------------

func (t *Trace) applyInit(l *rawLine) {
	t.CWD = l.CWD
	t.SessionID = l.SessionID
	t.Tools = l.Tools
	t.MCPServers = l.MCPServers
	t.Model = l.Model
	t.PermissionMode = l.PermissionMode
	t.APIKeySource = l.APIKeySource
	t.Skills = l.Skills
	t.SlashCommands = l.SlashCommands
	t.ClaudeCodeVersion = l.ClaudeCodeVersion
}

func (t *Trace) applyAssistant(l *rawLine, idx int, ts time.Time, raw json.RawMessage) {
	if l.Message == nil {
		t.Events = append(t.Events, Event{Index: idx, Kind: KindUnknown, Timestamp: ts, Raw: raw})
		return
	}
	if t.Model == "" {
		t.Model = l.Message.Model
	}
	if id := l.Message.ID; id != "" {
		if _, dup := t.seenMsgIDs[id]; !dup {
			t.seenMsgIDs[id] = struct{}{}
			t.TurnCount++
		}
	}
	blocks := decodeBlocks(l.Message.Content)
	for i, b := range blocks {
		ev := Event{
			Index:              idx,
			Timestamp:          ts,
			ThinkingDurationMs: l.ThinkingDurationMs,
			Raw:                raw,
		}
		switch b.Type {
		case "text":
			ev.Kind = KindAssistantText
			ev.Text = b.Text
		case "thinking":
			ev.Kind = KindAssistantThink
			ev.Text = b.Thinking
		case "tool_use":
			ev.Kind = KindToolUse
			ev.ToolUseID = b.ID
			ev.ToolName = b.Name
			ev.ToolInput = b.Input
			call := &ToolCall{
				Seq:       len(t.ToolCalls) + 1,
				ID:        b.ID,
				Name:      b.Name,
				Input:     b.Input,
				StartedAt: ts,
			}
			t.ToolCalls = append(t.ToolCalls, *call)
		default:
			ev.Kind = KindUnknown
			ev.Text = b.Text
		}
		// Only the first fragment of a multi-block message keeps the line index;
		// suffixes keep ordering readable without pretending they are new lines.
		if i > 0 {
			ev.Index = idx
		}
		t.Events = append(t.Events, ev)
	}
}

func (t *Trace) applyUser(l *rawLine, idx int, ts time.Time, raw json.RawMessage) {
	if l.Message == nil {
		t.Events = append(t.Events, Event{Index: idx, Kind: KindUnknown, Timestamp: ts, Raw: raw})
		return
	}

	// A `user` event whose content is a plain string is a real user turn
	// (possible with stream-json input); otherwise it carries tool results.
	if s, ok := decodeString(l.Message.Content); ok {
		t.Events = append(t.Events, Event{
			Index: idx, Kind: KindUserMessage, Timestamp: ts, Text: s, Raw: raw,
		})
		return
	}

	for _, b := range decodeBlocks(l.Message.Content) {
		if b.Type != "tool_result" {
			continue
		}
		out := decodeToolResultContent(b.Content)
		ev := Event{
			Index:      idx,
			Kind:       KindToolResult,
			Timestamp:  ts,
			ToolUseID:  b.ToolUseID,
			ToolOutput: out,
			ToolError:  b.IsError,
			Raw:        raw,
		}
		t.Events = append(t.Events, ev)
		t.joinToolResult(b.ToolUseID, out, b.IsError, ts)
	}
}

// joinToolResult attaches a result to its originating tool call.
func (t *Trace) joinToolResult(id, output string, isErr bool, ts time.Time) {
	for i := range t.ToolCalls {
		c := &t.ToolCalls[i]
		if c.ID != id {
			continue
		}
		c.Output = output
		c.IsError = c.IsError || isErr
		c.EndedAt = ts
		if !c.StartedAt.IsZero() && !ts.IsZero() {
			c.DurationMs = ts.Sub(c.StartedAt).Milliseconds()
		}
		return
	}
}

func (t *Trace) applyResult(l *rawLine, raw json.RawMessage) {
	t.sawResult = true
	t.Result.Subtype = l.Subtype
	t.Result.IsError = l.IsError
	t.Result.ResultText = l.Result
	t.Result.NumTurns = l.NumTurns
	t.Result.TotalCostUSD = l.TotalCostUSD
	t.Result.DurationMs = l.DurationMs
	t.Result.APIDurationMs = l.DurationAPIMs
	t.Result.TTFTMs = l.TTFTMs
	t.Result.TTFTStreamMs = l.TTFTStreamMs
	t.Result.StopReason = l.StopReason
	t.Result.TerminalReason = l.TerminalReason
	t.Result.PermissionDenials = l.PermissionDenials
	t.Result.APIErrorStatus = l.APIErrorStatus
	t.Result.Raw = raw
	if l.SessionID != "" {
		t.Result.SessionID = l.SessionID
	}
	if l.Usage != nil {
		t.Result.Usage = Usage{
			InputTokens:              l.Usage.InputTokens,
			OutputTokens:             l.Usage.OutputTokens,
			CacheReadInputTokens:     l.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: l.Usage.CacheCreationInputTokens,
			ThinkingTokens:           l.Usage.OutputTokensDetails.ThinkingTokens,
		}
	}
	if len(l.ModelUsage) > 0 {
		t.Result.ModelUsage = make(map[string]ModelUsage, len(l.ModelUsage))
		for name, mu := range l.ModelUsage {
			t.Result.ModelUsage[name] = ModelUsage{
				InputTokens:              mu.InputTokens,
				OutputTokens:             mu.OutputTokens,
				CacheReadInputTokens:     mu.CacheReadInputTokens,
				CacheCreationInputTokens: mu.CacheCreationInputTokens,
				ThinkingTokens:           mu.ThinkingTokens,
				CostUSD:                  mu.CostUSD,
				ContextWindow:            mu.ContextWindow,
				MaxOutputTokens:          mu.MaxOutputTokens,
				CanonicalModel:           mu.CanonicalModel,
				Provider:                 mu.Provider,
				CostBasis:                mu.CostBasis,
			}
		}
	}
	t.Events = append(t.Events, Event{
		Index: len(t.Events), Kind: KindResult, Timestamp: time.Now().UTC(), Raw: raw,
	})
}

// ---------------------------------------------------------------------------
// Decoding helpers
// ---------------------------------------------------------------------------

func decodeBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var list []block
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var single block
	if err := json.Unmarshal(raw, &single); err == nil && single.Type != "" {
		return []block{single}
	}
	return nil
}

func decodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// decodeToolResultContent flattens a tool_result body, which may be a bare
// string or a list of content blocks.
func decodeToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := decodeString(raw); ok {
		return s
	}
	blocks := decodeBlocks(raw)
	if len(blocks) == 0 {
		return string(raw)
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.Text != "":
			parts = append(parts, b.Text)
		case b.Content != nil:
			parts = append(parts, decodeToolResultContent(b.Content))
		}
	}
	return strings.Join(parts, "\n")
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	return time.Time{}
}

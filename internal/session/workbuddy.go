package session

// WorkBuddy records its agent runs as OpenTelemetry-style traces under
//
//	~/.workbuddy/traces/<worker-pid>/trace_<id>.jsonl
//
// Each line is one event: a `trace_start`, zero or more `trace_update`, a
// `trace_end`, and a matched `span_start` / `span_end` pair per span. The
// conversation lives in the span records, not in the trace envelope:
//
//   - a `generation` span carries the LLM call: `toolInput` is the messages
//     array sent to the model (system prompt plus the turn's context) and
//     `toolOutput` is the completion (`[{choices:[{message:{content}}]}]`).
//   - a `function` span carries a tool call: `toolName`, `toolInput` (the
//     arguments) and `toolOutput` (the result).
//
// # What a WorkBuddy trace gives back, and what it does not
//
// A trace is observability telemetry, not a transcript, and it reads like one.
// The envelope (`trace_end`) is the reliable part: it names the session
// (`sessionId`), the agent (`agentName`), the opening request (`prompt`), the
// models and the token totals. The spans carry the trajectory -- every
// generation and every tool call -- so the shape of the work, the tools used
// and the assistant's prose are reconstructable.
//
// What is missing or unreliable:
//
//   - The working directory is not recorded by the trace format, so Cwd is
//     empty and a replay cannot be bound to the directory the session ran in.
//   - `toolInput` on a span may be truncated: the envelope and some spans carry
//     `tracePayloadTruncated`, and the per-span `attributes` name the field.
//   - Token counts come from the trace totals, not from a per-message sum, so
//     they are a session figure, not a split by turn.
//
// A baseline collected from a trace is therefore recognised and replayable,
// but it is a coarser record than a Claude Code transcript: the notes below say
// so, because a reader who only has the dataset must be able to tell a measured
// capture from a telemetered one.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WorkBuddySourceName is the producer recorded in dataset.Baseline.Source for a
// session collected from WorkBuddy traces.
const WorkBuddySourceName = "workbuddy-trace"

// WorkBuddyTracesDir is the subdirectory of the WorkBuddy config directory that
// holds the per-run trace logs.
const WorkBuddyTracesDir = "traces"

// WorkBuddyDefaultRoot returns the directory WorkBuddy writes its traces to, for
// the current user. A missing directory is not an error here; the caller
// decides whether "this machine has never run WorkBuddy" is a state to report
// or a failure.
func WorkBuddyDefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".workbuddy", WorkBuddyTracesDir)
}

// WorkBuddyTrace is one trace as a listing shows it: enough to recognise it and
// pick it, without reading the whole file.
type WorkBuddyTrace struct {
	// ID is the session id the trace reports, falling back to the trace id and
	// then to the file name. It is what --session selects.
	ID string
	// Title is the opening request, or the id when the trace recorded none.
	Title string
	// Model is the primary model, when the trace recorded one.
	Model string
	// UpdatedAt is when the trace ended, or the file's modification time.
	UpdatedAt time.Time
	// Status is the trace's final status ("ok", "error", "running").
	Status string
	// Path is the trace file. It is what the detail page and a parse read.
	Path string
}

// ---------------------------------------------------------------------------
// Raw records
// ---------------------------------------------------------------------------

// wbModelInfo is the model summary a trace carries on its envelope.
type wbModelInfo struct {
	Models           []string `json:"models"`
	TotalInputTokens int      `json:"totalInputTokens"`
	TotalOutputTokens int     `json:"totalOutputTokens"`
	TotalCachedTokens int      `json:"totalCachedTokens"`
	CallCount        int      `json:"callCount"`
}

// wbTrace is the envelope shared by trace_start / trace_update / trace_end.
type wbTrace struct {
	TraceID    string         `json:"traceId"`
	Name       string         `json:"name"`
	SessionID  string         `json:"sessionId"`
	AgentName  string         `json:"agentName"`
	Prompt     string         `json:"prompt"`
	StartedAt  string         `json:"startedAt"`
	EndedAt    string         `json:"endedAt"`
	DurationMs int64          `json:"duration"`
	Status     string         `json:"status"`
	SpanCount  int            `json:"spanCount"`
	TotalTokens int64         `json:"totalTokens"`
	ModelInfo  *wbModelInfo   `json:"modelInfo"`
	Metadata   map[string]any `json:"metadata"`
}

// wbSpan is one completed span. The parse reads `span_end` only: it carries the
// full span, including the `toolInput` / `toolOutput` the matching `span_start`
// left empty.
type wbSpan struct {
	TraceID   string         `json:"traceId"`
	SpanID    string         `json:"spanId"`
	ParentID  string         `json:"parentId"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	StartedAt string         `json:"startedAt"`
	EndedAt   string         `json:"endedAt"`
	DurationMs int64         `json:"duration"`
	Status    string         `json:"status"`
	Error     any            `json:"error"`
	AgentName string         `json:"agentName"`
	ToolName  string         `json:"toolName"`
	ToolInput json.RawMessage `json:"toolInput"`
	ToolOutput json.RawMessage `json:"toolOutput"`
	Attributes map[string]any `json:"attributes"`
}

// wbRecord is the envelope every line shares.
type wbRecord struct {
	Type  string   `json:"type"`
	Trace *wbTrace `json:"trace"`
	Span  *wbSpan  `json:"span"`
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// WorkBuddyTraces lists the traces under root, newest first. A missing or empty
// root yields no traces rather than an error, for the same reason the Claude
// Code and Cursor sources do: a machine that has never run the tool has nothing
// to list.
func WorkBuddyTraces(root string) ([]WorkBuddyTrace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	var matches []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is skipped, like a missing transcript.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") || !strings.HasPrefix(d.Name(), "trace_") {
			return nil
		}
		matches = append(matches, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workbuddy: list %s: %w", root, err)
	}

	var out []WorkBuddyTrace
	for _, path := range matches {
		t, err := readWorkBuddyTraceHead(path)
		if err != nil {
			// An unreadable or malformed trace is skipped, not fatal: a listing
			// should not fail on one bad file.
			continue
		}
		if t == nil {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// readWorkBuddyTraceHead reads only the trace envelope of a file -- the
// `trace_end` (or, for a still-running trace, the last `trace_update` that
// carries a session id), which is where the provenance a listing needs lives.
func readWorkBuddyTraceHead(path string) (*WorkBuddyTrace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var start, lastUpdate, lastEnd *wbTrace
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var rec wbRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		switch rec.Type {
		case "trace_start":
			if rec.Trace != nil {
				start = rec.Trace
			}
		case "trace_update":
			if rec.Trace != nil && rec.Trace.SessionID != "" {
				lastUpdate = rec.Trace
			}
		case "trace_end":
			if rec.Trace != nil {
				lastEnd = rec.Trace
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("workbuddy: read %s: %w", path, err)
	}

	// Prefer the richest record: a finished trace's envelope, then a running
	// trace's last update, then the start.
	env := lastEnd
	if env == nil {
		env = lastUpdate
	}
	if env == nil {
		env = start
	}
	if env == nil {
		return nil, nil
	}

	id := env.SessionID
	if id == "" {
		id = env.TraceID
	}
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}

	model := ""
	if env.ModelInfo != nil && len(env.ModelInfo.Models) > 0 {
		model = env.ModelInfo.Models[0]
	}

	updated, _ := parseTime(env.EndedAt)
	if updated.IsZero() {
		if fi, err := os.Stat(path); err == nil {
			updated = fi.ModTime()
		}
	}

	title := oneLinePrompt(env.Prompt, 90)
	if title == "" {
		title = id
	}

	return &WorkBuddyTrace{
		ID:        id,
		Title:     title,
		Model:     model,
		UpdatedAt: updated,
		Status:    env.Status,
		Path:      path,
	}, nil
}

// LatestWorkBuddy returns the most recently updated trace under root.
func LatestWorkBuddy(root string) (WorkBuddyTrace, error) {
	all, err := WorkBuddyTraces(root)
	if err != nil {
		return WorkBuddyTrace{}, err
	}
	if len(all) == 0 {
		return WorkBuddyTrace{}, fmt.Errorf("workbuddy: no traces under %s", root)
	}
	return all[0], nil
}

// ResolveWorkBuddy turns a selector into one trace. A selector is either a
// trace file path, a session id, or a session id prefix.
func ResolveWorkBuddy(root, selector string) (WorkBuddyTrace, error) {
	if strings.TrimSpace(selector) == "" {
		return WorkBuddyTrace{}, fmt.Errorf("workbuddy: no trace given")
	}
	if strings.HasSuffix(selector, ".jsonl") {
		t, err := readWorkBuddyTraceHead(selector)
		if err != nil {
			return WorkBuddyTrace{}, err
		}
		if t == nil {
			return WorkBuddyTrace{}, fmt.Errorf("workbuddy: %s is not a trace", selector)
		}
		return *t, nil
	}
	all, err := WorkBuddyTraces(root)
	if err != nil {
		return WorkBuddyTrace{}, err
	}
	var matches []WorkBuddyTrace
	for _, t := range all {
		if t.ID == selector || strings.HasPrefix(t.ID, selector) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return WorkBuddyTrace{}, fmt.Errorf("workbuddy: no trace matching %q under %s", selector, root)
	case 1:
		return matches[0], nil
	default:
		return WorkBuddyTrace{}, fmt.Errorf("workbuddy: %q matches %d traces; give a longer prefix or a path", selector, len(matches))
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// ParseWorkBuddy reads one trace into the common Session shape, so that
// everything downstream -- Collect, WriteDataset, Summarize -- works unchanged.
func ParseWorkBuddy(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseWorkBuddyReader(f, path)
}

func parseWorkBuddyReader(r io.Reader, path string) (*Session, error) {
	var (
		env        *wbTrace
		spans      []wbSpan
		parseErrs  []string
		truncated  bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var rec wbRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		switch rec.Type {
		case "trace_start", "trace_update", "trace_end":
			if rec.Trace != nil {
				// Keep the last envelope seen: it is the most complete.
				env = rec.Trace
			}
		case "span_end":
			if rec.Span != nil {
				spans = append(spans, *rec.Span)
				if rec.Span.Attributes != nil {
					if v, ok := rec.Span.Attributes["tracePayloadTruncated"]; ok && v == true {
						truncated = true
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("workbuddy: read %s: %w", path, err)
	}
	if env == nil {
		return nil, fmt.Errorf("workbuddy: %s has no trace envelope", path)
	}

	s := &Session{
		Path:   path,
		Source: WorkBuddySourceName,
		Turns:  []Turn{},
	}
	id := env.SessionID
	if id == "" {
		id = env.TraceID
	}
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	s.ID = id

	if at, ok := parseTime(env.StartedAt); ok {
		s.StartedAt = at
	}
	if at, ok := parseTime(env.EndedAt); ok {
		s.EndedAt = at
	}

	modelSet := map[string]bool{}
	if env.ModelInfo != nil {
		for _, m := range env.ModelInfo.Models {
			if m != "" {
				modelSet[m] = true
			}
		}
	}

	// Build the trajectory from the spans, in time order. Generations become
	// assistant turns; functions become tool calls attached to the generation
	// they ran under.
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now().UTC()
	}
	openedAt := s.StartedAt
	ordered := append([]wbSpan(nil), spans...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ai, _ := parseTime(ordered[i].StartedAt)
		aj, _ := parseTime(ordered[j].StartedAt)
		if ai.Equal(aj) {
			return ordered[i].SpanID < ordered[j].SpanID
		}
		return ai.Before(aj)
	})

	var lastAssistant *Turn
	var assistantCount int
	for i := range ordered {
		sp := ordered[i]
		at, hasAt := parseTime(sp.StartedAt)
		if hasAt {
			if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
				s.StartedAt = at
			}
			if at.After(s.EndedAt) {
				s.EndedAt = at
			}
		}
		switch sp.Type {
		case "generation":
			text := workbuddyCompletionText(sp.ToolOutput)
			t := Turn{Role: RoleAssistant, At: at, MessageID: sp.SpanID, Text: strings.TrimSpace(text)}
			s.Turns = append(s.Turns, t)
			lastAssistant = &s.Turns[len(s.Turns)-1]
			assistantCount++
			if m := workbuddyGenerationModel(sp.ToolOutput); m != "" {
				modelSet[m] = true
			}
		case "function":
			if sp.ToolName == "" {
				continue
			}
			tc := ToolCall{ID: sp.SpanID, Name: sp.ToolName, Input: sp.ToolInput}
			s.ToolCalls = append(s.ToolCalls, tc)
			if sp.ToolOutput != nil && len(strings.TrimSpace(string(sp.ToolOutput))) > 0 {
				s.ToolResults++
			}
			if lastAssistant != nil {
				lastAssistant.Tools = append(lastAssistant.Tools, tc)
			} else {
				// A tool call with no generation above it: record it on a
				// synthetic assistant turn so the trajectory stays contiguous.
				s.Turns = append(s.Turns, Turn{Role: RoleAssistant, At: at, Tools: []ToolCall{tc}})
				lastAssistant = &s.Turns[len(s.Turns)-1]
			}
		}
	}

	// The opening human request. The envelope's prompt is the best-effort
	// opening request; if the trace recorded none, take the first generation's
	// user message.
	prompt := strings.TrimSpace(env.Prompt)
	if prompt == "" {
		prompt = workbuddyOpeningPrompt(ordered)
	}
	if prompt != "" {
		s.Prompts = append(s.Prompts, Prompt{
			Index:     0,
			Text:      prompt,
			At:        openedAt,
			TurnIndex: 0,
		})
		s.Turns = append([]Turn{{Role: RoleUser, Text: prompt, At: openedAt}}, s.Turns...)
		// Re-index the prompt's turn position after the prepend.
		s.Prompts[0].TurnIndex = 0
	}

	// Provenance and totals.
	s.Models = append(s.Models, keysOf(modelSet)...)
	sort.Strings(s.Models)
	if env.ModelInfo != nil {
		s.Usage = Usage{
			InputTokens:       env.ModelInfo.TotalInputTokens,
			OutputTokens:      env.ModelInfo.TotalOutputTokens,
			CacheReadTokens:   env.ModelInfo.TotalCachedTokens,
		}
	}
	if env.DurationMs > 0 {
		s.DurationMs = env.DurationMs
	} else if !s.StartedAt.IsZero() && s.EndedAt.After(s.StartedAt) {
		s.DurationMs = s.EndedAt.Sub(s.StartedAt).Milliseconds()
	}
	// Busy time estimates from the gaps that end at an assistant turn; the
	// trace records no split between API and tool time, so it is labelled.
	s.BusyMs = busyFromGaps(s.Turns)
	s.haveTotals = true

	s.Title = oneLinePrompt(env.Prompt, 120)
	if s.Title == "" {
		s.Title = id
	}
	s.ParseErrors = parseErrs

	s.BaselineNotes = append(s.BaselineNotes,
		"collected from WorkBuddy OTel traces: telemetry, not a transcript -- the working directory "+
			"is not recorded, so a replay cannot be bound to where the session ran")
	if truncated {
		s.BaselineNotes = append(s.BaselineNotes,
			"this trace flagged tracePayloadTruncated, so some tool inputs or completions may be cut off")
	}
	s.BaselineNotes = append(s.BaselineNotes,
		"busy time is estimated from the gaps that end at an assistant turn; WorkBuddy traces record "+
			"no per-request timing split")
	if assistantCount > 0 {
		s.BaselineNotes = append(s.BaselineNotes,
			"assistant turns are counted per generation span, which is one LLM call; tool calls are "+
				"counted per function span")
	}
	return s, nil
}

// workbuddyCompletionText pulls the assistant prose out of a generation's
// toolOutput. The output is a JSON array wrapping one chat completion
// (`[{choices:[{message:{content}}]}]`); some builds wrap the object directly.
// A raw string is taken as-is, and any content that is not a string is rendered
// to JSON rather than dropped.
func workbuddyCompletionText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		if c := chatCompletionContent(arr[0]); c != "" {
			return c
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if c := chatCompletionContent(obj); c != "" {
			return c
		}
	}
	// Not a chat-completion shape: keep the text.
	return s
}

// chatCompletionContent reads choices[0].message.content from a decoded
// completion, handling both a string and a structured content.
func chatCompletionContent(o map[string]any) string {
	ch, ok := o["choices"].([]any)
	if !ok || len(ch) == 0 {
		return ""
	}
	first, ok := ch[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		if b, err := json.Marshal(c); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", c)
	}
}

// workbuddyGenerationModel reads the model name off a generation's toolOutput,
// when the completion carries one.
func workbuddyGenerationModel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		if m, ok := arr[0]["model"].(string); ok && m != "" {
			return m
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if m, ok := obj["model"].(string); ok && m != "" {
			return m
		}
	}
	return ""
}

// workbuddyOpeningPrompt finds the human request in the first generation's
// toolInput: the messages array, taking the last user-role message with text.
// It is a fallback only -- the envelope's prompt is preferred.
func workbuddyOpeningPrompt(spans []wbSpan) string {
	for _, sp := range spans {
		if sp.Type != "generation" || len(sp.ToolInput) == 0 {
			continue
		}
		if p := lastUserMessageText(sp.ToolInput); p != "" {
			return p
		}
	}
	return ""
}

// lastUserMessageText reads a messages array and returns the last user-role
// message's text. Content may be a string or a list of blocks; only the text
// blocks are joined.
func lastUserMessageText(raw json.RawMessage) string {
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		role, _ := msgs[i]["role"].(string)
		if role != "user" {
			continue
		}
		switch c := msgs[i]["content"].(type) {
		case string:
			if t := strings.TrimSpace(c); t != "" {
				return t
			}
		case []any:
			var parts []string
			for _, b := range c {
				blk, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if blk["type"] == "text" {
					if t, ok := blk["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
				return joined
			}
		}
	}
	return ""
}

// keysOf returns the sorted keys of a set, for deterministic model listing.
func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

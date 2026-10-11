package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Mock is a deterministic agent used for tests, CI and pipeline smoke runs.
//
// It is not a language model. It executes Spec.Bin as a shell command inside the
// sandbox with the task prompt on stdin, then synthesizes a well-formed
// stream-json transcript around the result.
//
// This exists so the whole pipeline -- sandbox materialization, trajectory
// parsing, grading, persistence and reporting -- can be exercised end to end
// without spending tokens or requiring a reachable model endpoint. A dataset that
// needs a "known good" and a "known bad" agent for regression testing gets both
// by pointing different runs at different scripts.
type Mock struct{}

var _ Runner = (*Mock)(nil)

func (m *Mock) Name() string { return KindMock }

func (m *Mock) Run(ctx context.Context, sb sandbox.Sandbox, spec Spec, prompt string) (*Attempt, error) {
	spec = spec.withDefaults()

	sessionID := NewUUID()
	command := spec.Bin
	if command == "" || command == "claude" {
		command = "true"
	}

	started := time.Now().UTC()
	res, err := sb.Exec(ctx, command, sandbox.ExecOptions{
		Env:     spec.resolvedEnv(),
		Timeout: spec.Timeout,
		Stdin:   prompt,
	})
	ended := time.Now().UTC()

	attempt := &Attempt{
		Agent:       KindMock,
		Model:       spec.Model,
		Bin:         command,
		SessionID:   sessionID,
		CommandLine: command,
		StartedAt:   started,
		EndedAt:     ended,
	}
	if err != nil {
		attempt.Failed = true
		attempt.Error = err.Error()
		return attempt, nil
	}

	attempt.ExitCode = res.ExitCode
	attempt.TimedOut = res.TimedOut
	attempt.Duration = res.Duration
	attempt.Stderr = res.Stderr

	stream := synthesizeStream(sessionID, spec.Model, command, res)
	tr, perr := trace.Parse(strings.NewReader(stream))
	if perr != nil {
		return nil, fmt.Errorf("parse synthesized mock stream: %w", perr)
	}
	attempt.Trace = tr
	attempt.RawStream = stream

	if res.Err != nil && res.ExitCode < 0 && !res.TimedOut {
		attempt.Failed = true
		attempt.Error = res.Err.Error()
	}
	return attempt, nil
}

// synthesizeStream builds a stream-json transcript equivalent to what the real
// CLI would emit for a one-tool-call turn, so downstream consumers cannot tell
// the difference between a mock and a real attempt by shape alone.
func synthesizeStream(sessionID, model, command string, res *sandbox.ExecResult) string {
	if model == "" {
		model = "mock"
	}
	isError := !res.Success()
	output := res.Stdout
	if isError && output == "" {
		output = res.Stderr
	}

	init := map[string]any{
		"type": "system", "subtype": "init",
		"session_id": sessionID, "model": model,
		"tools": []string{"Bash"}, "mcp_servers": []any{},
		"permissionMode": "bypassPermissions",
	}
	call := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id": "mock-turn-1", "role": "assistant", "model": model,
			"content": []any{map[string]any{
				"type": "tool_use", "id": "mock-tool-1", "name": "Bash",
				"input": map[string]any{"command": command},
			}},
		},
	}
	result := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "mock-tool-1",
				"content": output, "is_error": isError,
			}},
		},
	}
	answer := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id": "mock-turn-2", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": strings.TrimSpace(res.Stdout)}},
		},
	}
	subtype := "success"
	if isError {
		// Mirror the CLI's own vocabulary for a failed run rather than claiming
		// success with is_error set, which would make the stop reason ambiguous.
		subtype = "error_during_execution"
	}
	terminal := map[string]any{
		"type": "result", "subtype": subtype, "is_error": isError,
		"session_id": sessionID, "num_turns": 2,
		"result":      strings.TrimSpace(res.Stdout),
		"duration_ms": res.Duration.Milliseconds(),
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	var b strings.Builder
	for _, obj := range []any{init, call, result, answer, terminal} {
		line, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

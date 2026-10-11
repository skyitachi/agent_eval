package grader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/llm"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

func init() { Register(&Command{}) }

// Command runs an arbitrary shell command as a grader.
//
// This is the escape hatch that keeps the framework from being the bottleneck:
// a project with an existing checker, a bespoke linter, a benchmark harness or a
// script written in another language plugs in here without touching Go code.
//
// The command runs inside the sandbox, so it sees the agent's workspace exactly
// as the agent left it, and it can read the standard artifact bundle that the
// harness writes to .agenteval/ (see the Artifact* constants).
type Command struct{}

func (c *Command) Type() string { return "command" }

type commandConfig struct {
	// Command is the shell command. These placeholders are substituted:
	//
	//	{workspace}    absolute path of the workspace root
	//	{harness_dir}  workspace-relative path of the artifact directory
	//	{task_id}      the task id
	//	{diff}         {harness_dir}/diff.patch
	//	{answer}       {harness_dir}/answer.txt
	//	{trace}        {harness_dir}/trace.json
	Command string `json:"command"`

	// Parse selects how the outcome is read: "exit_code" (default) or "json".
	Parse string `json:"parse,omitempty"`
	// PassExitCodes lists exit codes considered passing. Default [0].
	PassExitCodes []int `json:"pass_exit_codes,omitempty"`

	// Copy lists dataset-relative files or directories copied into the sandbox
	// before the command runs.
	Copy []string `json:"copy,omitempty"`
	// CopyTarget is the destination for Copy entries. Empty means the workspace
	// root.
	CopyTarget string `json:"copy_target,omitempty"`

	// Env is passed to the command.
	Env map[string]string `json:"env,omitempty"`
	// TimeoutS bounds the command.
	TimeoutS int `json:"timeout_s,omitempty"`
	// SkipArtifacts disables writing the standard artifact bundle. Enable only
	// when the command is sensitive to extra files, such as a whole-repo linter.
	// The bundle directory is excluded from diffs either way.
	SkipArtifacts bool `json:"skip_artifacts,omitempty"`
}

// commandJSONResult is the structured output shape for Parse == "json".
//
// Every field is optional except score/passed; a script can report just
// {"passed": true} or go as far as attaching arbitrary evidence.
type commandJSONResult struct {
	Score   *float64       `json:"score,omitempty"`
	Passed  *bool          `json:"passed,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (c *Command) Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error) {
	var cfg commandConfig
	if err := DecodeConfig(spec, &cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("command grader requires config.command")
	}
	if cfg.TimeoutS <= 0 {
		cfg.TimeoutS = 300
	}
	if !cfg.SkipArtifacts {
		if _, err := WriteArtifacts(gc); err != nil {
			return nil, err
		}
	}
	for _, src := range cfg.Copy {
		if err := CopyInto(gc, src, cfg.CopyTarget); err != nil {
			return nil, err
		}
	}

	command := substitutePlaceholders(cfg.Command, gc)
	env := mergeEnv(gc.Env(), cfg.Env)

	res, err := gc.Sandbox.Exec(ctx, command, sandbox.ExecOptions{
		Env:     env,
		Timeout: time.Duration(cfg.TimeoutS) * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("run command grader: %w", err)
	}

	details := map[string]any{
		"command":     command,
		"exit_code":   res.ExitCode,
		"timed_out":   res.TimedOut,
		"duration_ms": res.Duration.Milliseconds(),
		"stdout_tail": clip(tail(res.Stdout, 60), 8000),
	}
	if res.Stderr != "" {
		details["stderr_tail"] = clip(tail(res.Stderr, 30), 4000)
	}
	if res.TimedOut {
		return &Result{
			Passed:  false,
			Score:   0,
			Reason:  fmt.Sprintf("command timed out after %ds", cfg.TimeoutS),
			Details: details,
		}, nil
	}

	switch orDefaultStr(cfg.Parse, "exit_code") {
	case "json":
		return gradeFromJSON(res.Stdout, details)
	case "exit_code":
		return gradeFromExitCode(res.ExitCode, cfg.PassExitCodes, details)
	default:
		return nil, fmt.Errorf("unknown parse mode %q (want exit_code or json)", cfg.Parse)
	}
}

func gradeFromExitCode(code int, pass []int, details map[string]any) (*Result, error) {
	if len(pass) == 0 {
		pass = []int{0}
	}
	for _, p := range pass {
		if code == p {
			return &Result{
				Passed:  true,
				Score:   1,
				Reason:  fmt.Sprintf("command exited %d", code),
				Details: details,
			}, nil
		}
	}
	details["pass_exit_codes"] = pass
	return &Result{
		Passed:  false,
		Score:   0,
		Reason:  fmt.Sprintf("command exited %d, expected one of %v", code, pass),
		Details: details,
	}, nil
}

// gradeFromJSON reads a structured verdict from the command's stdout.
func gradeFromJSON(stdout string, details map[string]any) (*Result, error) {
	raw, ok := llm.ExtractJSON(stdout)
	if !ok {
		details["stdout_tail"] = clip(tail(stdout, 60), 8000)
		return &Result{
			Passed:  false,
			Score:   0,
			Reason:  "command parse=json but no JSON object was found in stdout",
			Details: details,
		}, nil
	}
	var parsed commandJSONResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		details["parse_error"] = err.Error()
		return &Result{
			Passed:  false,
			Score:   0,
			Reason:  "command parse=json but the JSON was invalid: " + err.Error(),
			Details: details,
		}, nil
	}

	for k, v := range parsed.Details {
		details[k] = v
	}

	score := 0.0
	if parsed.Score != nil {
		score = clamp01(*parsed.Score)
	}
	passed := score >= 0.5
	if parsed.Passed != nil {
		passed = *parsed.Passed
		if parsed.Score == nil {
			if passed {
				score = 1
			} else {
				score = 0
			}
		}
	}

	reason := parsed.Reason
	if reason == "" {
		if passed {
			reason = "command reported success"
		} else {
			reason = "command reported failure"
		}
	}
	return &Result{Passed: passed, Score: score, Reason: reason, Details: details}, nil
}

// substitutePlaceholders expands the documented placeholders. An unknown
// placeholder is left intact so a typo shows up verbatim in the report instead of
// silently becoming an empty string.
func substitutePlaceholders(command string, gc *Context) string {
	harness := HarnessDir
	pairs := [][2]string{
		{"{workspace}", gc.Sandbox.Root()},
		{"{harness_dir}", harness},
		{"{task_id}", gc.Task.ID},
		{"{diff}", harness + "/" + ArtifactDiff},
		{"{answer}", harness + "/" + ArtifactAnswer},
		{"{trace}", harness + "/" + ArtifactTrace},
		{"{attempt}", harness + "/" + ArtifactAttempt},
	}
	out := command
	for _, p := range pairs {
		out = strings.ReplaceAll(out, p[0], p[1])
	}
	return out
}

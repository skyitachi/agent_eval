package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

// ---------------------------------------------------------------------------
// Oracle
// ---------------------------------------------------------------------------
//
// The anti-gaming guards live in verify.go, next to the grading sequence they
// are a step of.

// applyOracle materializes a task's reference solution inside the sandbox.
//
// Order is fixed and matches how a person would do it: patch first (it is a diff
// against the baseline, so it has to be applied to the baseline tree), then whole
// files for the cases a diff cannot express, then commands for anything that
// needs to run.
func applyOracle(ctx context.Context, sb sandbox.Sandbox, t dataset.TaskSpec) error {
	o := t.Oracle
	if o == nil {
		return fmt.Errorf("task %s declares no oracle", t.ID)
	}
	if patch := strings.TrimSpace(o.Patch); patch != "" {
		// The patch goes in on stdin. That avoids shell quoting entirely and,
		// more importantly, avoids leaving a patch file inside the workspace for
		// the graded diff to pick up.
		res, err := sb.Exec(ctx, "git apply --whitespace=nowarn -", sandbox.ExecOptions{Stdin: o.Patch + "\n"})
		if err != nil {
			return fmt.Errorf("git apply: %w", err)
		}
		if !res.Success() {
			return fmt.Errorf("git apply failed (exit %d):\n%s",
				res.ExitCode, clipText(res.CombinedText(), 2000))
		}
	}
	for rel, content := range o.Files {
		if err := sb.WriteFile(rel, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write oracle file %s: %w", rel, err)
		}
	}
	for i, c := range o.Apply {
		opts := sandbox.ExecOptions{Cwd: c.Cwd, Env: workspaceEnv(t.Workspace.Env)}
		if c.TimeoutS > 0 {
			opts.Timeout = time.Duration(c.TimeoutS) * time.Second
		}
		res, err := sb.Exec(ctx, c.Run, opts)
		if err != nil {
			return fmt.Errorf("oracle apply[%d] %q: %w", i, c.Run, err)
		}
		if !res.Success() && !c.Optional {
			return fmt.Errorf("oracle apply[%d] %q failed (exit %d):\n%s",
				i, c.Run, res.ExitCode, clipText(res.CombinedText(), 2000))
		}
	}
	return nil
}

// clipText bounds an error message. A failing setup command can emit megabytes,
// and a multi-megabyte string in an error column is worse than no detail.
func clipText(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 20 {
		s = strings.Join(lines[:20], "\n") + fmt.Sprintf("\n... (%d more lines)", len(lines)-20)
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

package engine

import (
	"context"
	"fmt"
	"os"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

// Workspace is an open, materialized task workspace: a sandbox holding the
// task's starting tree, plus the baseline commit the tree was captured at.
//
// It exists because two callers need the same thing at different times. The
// orchestrator creates one per cell and immediately runs the agent in it. The RL
// environment service creates one and hands it to a trainer's own harness, which
// may act in it for minutes before anyone asks for a reward. Both must get a
// workspace built exactly the same way -- same sandbox backend, same environment
// variables, same baseline semantics -- or a reward measured through one of them
// would not be comparable with a reward measured through the other.
type Workspace struct {
	Task     dataset.TaskSpec
	Sandbox  sandbox.Sandbox
	Baseline *sandbox.Baseline
	// Dir is the host path of the workspace root.
	Dir string
}

// OpenWorkspace creates a sandbox at dir, materializes the task's workspace into
// it and captures the baseline.
//
// datasetDir is the directory the task's relative asset paths resolve against,
// which is the dataset root, not the caller's working directory.
func (e *Engine) OpenWorkspace(ctx context.Context, t dataset.TaskSpec,
	datasetDir, dir string) (*Workspace, error) {

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace %s: %w", dir, err)
	}
	sb, err := newSandbox(ctx, e.cfg.Backend, dir, workspaceEnv(t.Workspace.Env))
	if err != nil {
		return nil, err
	}
	// From here on a failure has to release the sandbox, otherwise a broken task
	// leaks a container (or a directory) per attempt.
	if err := sandbox.Materialize(ctx, sb, t.Workspace, datasetDir); err != nil {
		_ = sb.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("materialize workspace: %w", err)
	}
	baseline, err := sandbox.CurrentBaseline(ctx, sb)
	if err != nil {
		_ = sb.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("read baseline: %w", err)
	}
	return &Workspace{Task: t, Sandbox: sb, Baseline: baseline, Dir: dir}, nil
}

// Close releases the sandbox and, when asked, the workspace directory.
//
// Both steps are best-effort and both are reported: a sandbox that will not shut
// down, or a directory that will not be removed, is leaked disk or a leaked
// container, and the caller is the only one who can act on it.
func (w *Workspace) Close(ctx context.Context, removeDir bool) error {
	if w == nil {
		return nil
	}
	var first error
	if w.Sandbox != nil {
		if err := w.Sandbox.Close(context.WithoutCancel(ctx)); err != nil {
			first = fmt.Errorf("close sandbox: %w", err)
		}
	}
	if removeDir && w.Dir != "" {
		if err := sandbox.RemoveDir(w.Dir); err != nil && first == nil {
			first = fmt.Errorf("remove workspace %s: %w", w.Dir, err)
		}
	}
	return first
}

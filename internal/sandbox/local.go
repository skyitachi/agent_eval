package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LocalConfig configures the local (host-process) backend.
type LocalConfig struct {
	// Root is the host directory backing the workspace.
	Root string
	// Shell is the interpreter used for command lines. Default "/bin/sh".
	Shell string
	// ShellArgs are passed before the command string. Default ["-c"].
	ShellArgs []string
	// Env is applied to every command.
	Env map[string]string
	// InheritEnv forwards the parent process environment. Default true; disable
	// to get a hermetic environment for reproducible evaluations.
	InheritEnv *bool
	// KeepWorkspace leaves the workspace on disk after Close, for debugging.
	KeepWorkspace bool
}

// Local runs commands directly on the host inside the workspace directory.
//
// Isolation: separate working directory plus a controlled environment. There is
// no filesystem or network boundary -- an agent running here can read anything
// the invoking user can read. Use the Docker backend when that matters.
type Local struct {
	base
	shell      string
	shellArgs  []string
	env        map[string]string
	inheritEnv bool
	keep       bool
}

var _ Sandbox = (*Local)(nil)

// NewLocal prepares a local sandbox rooted at cfg.Root.
func NewLocal(cfg LocalConfig) (*Local, error) {
	if cfg.Root == "" {
		return nil, errors.New("local sandbox: Root is required")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace %s: %w", abs, err)
	}
	shell := cfg.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	args := cfg.ShellArgs
	if len(args) == 0 {
		args = []string{"-c"}
	}
	inherit := true
	if cfg.InheritEnv != nil {
		inherit = *cfg.InheritEnv
	}
	return &Local{
		base:       base{root: abs, workdir: abs},
		shell:      shell,
		shellArgs:  args,
		env:        cfg.Env,
		inheritEnv: inherit,
		keep:       cfg.KeepWorkspace,
	}, nil
}

func (l *Local) Backend() string { return "local" }

func (l *Local) Describe() string { return "local:" + l.root }

func (l *Local) Exec(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	return l.ExecStream(ctx, command, opts, Streams{})
}

func (l *Local) ExecStream(ctx context.Context, command string, opts ExecOptions, w Streams) (*ExecResult, error) {
	start := time.Now()

	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	args := append(append([]string{}, l.shellArgs...), command)
	cmd := exec.CommandContext(runCtx, l.shell, args...)
	cmd.Dir = l.commandDir(opts.Cwd)
	cmd.Env = l.buildEnv(opts.Env)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}

	// Run each command in its own process group so that a timeout kills the
	// whole tree. Without this, a shell that spawns children (very common for
	// agent Bash calls) leaves orphans holding the output pipes open, and Wait
	// never returns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	stdout := &bufferWriter{dst: w.Stdout}
	stderr := &bufferWriter{dst: w.Stderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	res := &ExecResult{
		ExitCode: exitCodeOf(err),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	// A non-zero exit is a normal outcome, not an infrastructure error.
	if err != nil && res.ExitCode < 0 {
		res.Err = err
	}
	return res, nil
}

func (l *Local) Close(ctx context.Context) error {
	if l.keep {
		return nil
	}
	return RemoveDir(l.root)
}

// commandDir resolves a workspace-relative cwd to an absolute host path.
func (l *Local) commandDir(rel string) string {
	if strings.TrimSpace(rel) == "" {
		return l.root
	}
	return joinClean(l.root, rel)
}

// buildEnv composes the process environment.
func (l *Local) buildEnv(extra map[string]string) []string {
	env := map[string]string{}
	if l.inheritEnv {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	for k, v := range l.env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

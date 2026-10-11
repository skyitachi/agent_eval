// Package sandbox materializes an evaluation workspace and executes commands
// inside it.
//
// # Why the agent runs *inside* the sandbox
//
// The Claude Code process is launched through the same Sandbox.Exec path as any
// setup or grading command. This matters: if the agent ran on the host while only
// its graded artifacts were collected from a container, every `Bash` tool call it
// makes would execute on the host and the isolation would be cosmetic. Running the
// agent inside means a Docker sandbox constrains the agent's shell, filesystem and
// network too, not just the final diff.
//
// # Workspace file access
//
// For both backends the workspace lives at a host path which is bind-mounted into
// the container at Workdir (Docker) or used directly (Local). ReadF/WriteF
// therefore operate on the host path in both cases -- dramatically faster than
// `docker cp` and giving graders a stable view of the results.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrNotSupported is returned by optional operations a backend cannot provide.
var ErrNotSupported = errors.New("operation not supported by this sandbox backend")

// RemoveDir removes a workspace directory and verifies that it is really gone,
// retrying briefly, and reports an error only if the directory survived.
//
// A bare os.RemoveAll is not enough, for two independent reasons. On macOS it
// intermittently returns ENOTEMPTY for a tree nothing is touching. And an
// external process can recreate a path *after* RemoveAll has returned nil --
// git instrumentation attached through `trace2.eventtarget` writes into .git
// asynchronously, after the git command that triggered it has already exited.
// Checking the result rather than the return value is what makes the reported
// outcome truthful: callers use a non-nil error to warn about leaked disk, so a
// false nil would silently leak a workspace per task.
func RemoveDir(dir string) error {
	if dir == "" {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.RemoveAll(dir); err != nil {
			lastErr = err
		}
		if _, err := os.Stat(dir); err != nil {
			// Gone, whether it was removed now or was never there.
			return nil
		}
		// Still present: either the removal failed, or something recreated it.
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("directory still exists after 5 removal attempts; " +
			"something outside the harness is writing into it")
	}
	return lastErr
}

// ExecOptions configures a single command execution.
type ExecOptions struct {
	// Cwd is relative to the workspace root. Empty means the workspace root.
	Cwd string
	// Env holds extra environment variables for this command only.
	Env map[string]string
	// Timeout bounds the command. Zero means no timeout.
	Timeout time.Duration
	// Stdin, when non-empty, is fed to the command's standard input.
	Stdin string
}

// ExecResult is the outcome of a command execution.
type ExecResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"duration"`
	// TimedOut is true when the command was killed because it exceeded Timeout.
	TimedOut bool `json:"timed_out"`
	// Err is set for infrastructure failures (spawn error, backend error) as
	// opposed to a non-zero exit code, which is reported via ExitCode.
	Err error `json:"-"`
}

// Success reports a clean zero exit.
func (r *ExecResult) Success() bool { return r.Err == nil && r.ExitCode == 0 && !r.TimedOut }

// CombinedText returns stdout and stderr concatenated, for graders that do not
// care about the distinction.
func (r *ExecResult) CombinedText() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	if r.Stdout == "" {
		return r.Stderr
	}
	return r.Stdout + "\n" + r.Stderr
}

// Streams carries the writers a streaming execution should feed.
type Streams struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Sandbox is a prepared workspace plus a command execution environment.
type Sandbox interface {
	// Backend names the implementation ("local" or "docker").
	Backend() string
	// Root returns the host filesystem path of the workspace. This is the same
	// directory the container sees as Workdir.
	Root() string
	// Workdir is the path commands are executed in (the container-side path).
	Workdir() string
	// Describe renders a one-line human description for logs.
	Describe() string

	// Exec runs a command to completion, buffering output.
	Exec(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error)
	// ExecStream runs a command and streams its output to w while also
	// accumulating it in the returned ExecResult.
	ExecStream(ctx context.Context, command string, opts ExecOptions, w Streams) (*ExecResult, error)

	// ReadFile reads a workspace-relative file.
	ReadFile(rel string) ([]byte, error)
	// WriteFile writes a workspace-relative file, creating parent directories.
	WriteFile(rel string, data []byte, perm os.FileMode) error
	// Exists reports whether a workspace-relative path exists.
	Exists(rel string) bool

	// Close tears the sandbox down. For local backends this removes the
	// workspace unless it is configured to be kept.
	Close(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// Process handling shared by backends
// ---------------------------------------------------------------------------

// base provides the host-side workspace file operations used by every backend.
type base struct {
	root    string
	workdir string
}

func (b *base) Root() string    { return b.root }
func (b *base) Workdir() string { return b.workdir }

func (b *base) ReadFile(rel string) ([]byte, error) {
	return os.ReadFile(b.hostPath(rel))
}

func (b *base) WriteFile(rel string, data []byte, perm os.FileMode) error {
	p := b.hostPath(rel)
	if err := os.MkdirAll(dirOf(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, perm)
}

func (b *base) Exists(rel string) bool {
	_, err := os.Stat(b.hostPath(rel))
	return err == nil
}

func (b *base) hostPath(rel string) string {
	cleaned := strings.TrimPrefix(strings.TrimSpace(rel), "/")
	return joinClean(b.root, cleaned)
}

// bufferWriter accumulates everything written to it while forwarding to an
// optional downstream writer. Used to keep a full copy of streamed output.
type bufferWriter struct {
	buf strings.Builder
	dst io.Writer
}

func (w *bufferWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if w.dst != nil {
		return w.dst.Write(p)
	}
	return len(p), nil
}

func (w *bufferWriter) String() string { return w.buf.String() }

// exitCodeOf extracts a process exit code, distinguishing "exited with N" from
// "never ran" (which yields -1 so callers can tell them apart).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// notFoundError produces an actionable message for a missing binary.
func notFoundError(bin, hint string) error {
	if hint == "" {
		return fmt.Errorf("%s not found in PATH", bin)
	}
	return fmt.Errorf("%s not found in PATH (%s)", bin, hint)
}

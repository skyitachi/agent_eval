package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DockerConfig configures the container backend.
type DockerConfig struct {
	// Root is the host directory bind-mounted into the container.
	Root string
	// Image is the container image. It must contain the agent CLI and whatever
	// runtimes the tasks need.
	Image string
	// Workdir is the workspace path inside the container. Default "/workspace".
	Workdir string
	// ContainerName overrides the generated container name.
	ContainerName string

	// Network is passed to --network. "none" is the recommended default for
	// hermetic evaluation; use "bridge" when tasks need package downloads.
	Network string
	// Memory is a --memory value such as "4g". Empty means unlimited.
	Memory string
	// CPUs is a --cpus value such as "2". Empty means unlimited.
	CPUs string
	// PidsLimit bounds process count (fork-bomb protection). 0 disables.
	PidsLimit int
	// User is a --user value. Empty runs as the image default.
	User string
	// Env is applied to the container at run time.
	Env map[string]string
	// ExtraMounts are additional "host:container[:ro]" bind mounts.
	ExtraMounts []string
	// ExtraRunArgs are appended verbatim to `docker run`.
	ExtraRunArgs []string
	// DockerBin overrides the docker executable path.
	DockerBin string
	// Keep leaves the container running after Close, for debugging.
	Keep bool
}

// Docker runs commands inside a long-lived container with the workspace
// bind-mounted in.
type Docker struct {
	base
	cfg    DockerConfig
	bin    string
	name   string
	client *Client
	dead   bool
}

var _ Sandbox = (*Docker)(nil)

// NewDocker starts a container and returns a sandbox backed by it.
func NewDocker(ctx context.Context, cfg DockerConfig) (*Docker, error) {
	if cfg.Root == "" {
		return nil, errors.New("docker sandbox: Root is required")
	}
	if cfg.Image == "" {
		return nil, errors.New("docker sandbox: Image is required")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace %s: %w", abs, err)
	}
	workdir := cfg.Workdir
	if workdir == "" {
		workdir = "/workspace"
	}
	name := cfg.ContainerName
	if name == "" {
		name = "agenteval-" + shortID()
	}

	d := &Docker{
		base: base{root: abs, workdir: workdir},
		cfg:  cfg,
		bin:  orDefault(cfg.DockerBin, "docker"),
		name: name,
	}
	d.client = &Client{bin: d.bin, container: name}

	if err := d.checkDaemon(ctx); err != nil {
		return nil, err
	}
	if err := d.start(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Docker) Backend() string { return "docker" }

func (d *Docker) Describe() string {
	return fmt.Sprintf("docker:%s image=%s workdir=%s", d.name, d.cfg.Image, d.workdir)
}

// Name exposes the container name (used in logs).
func (d *Docker) Name() string { return d.name }

// checkDaemon verifies the Docker daemon is reachable and explains how to fix it
// when it is not. A stopped daemon is by far the most common failure here.
func (d *Docker) checkDaemon(ctx context.Context) error {
	if _, err := exec.LookPath(d.bin); err != nil {
		return fmt.Errorf("docker CLI not found (looked for %q in PATH); "+
			"install Docker Desktop, OrbStack or colima, or use sandbox.backend=local", d.bin)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, d.bin, "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if cctx.Err() != nil {
			return fmt.Errorf("docker daemon did not respond within 15s; start Docker/OrbStack and retry")
		}
		return fmt.Errorf("docker daemon unreachable: %s\n"+
			"  hint: start Docker Desktop or OrbStack (e.g. `open -a OrbStack`), "+
			"or set sandbox.backend=local", firstLine(msg))
	}
	return nil
}

func (d *Docker) start(ctx context.Context) error {
	args := []string{
		"run", "-d", "--name", d.name,
		// --init installs a tiny init as PID 1 so orphaned grandchildren are
		// reaped instead of accumulating as zombies inside a long-lived container.
		"--init",
		"-v", d.root + ":" + d.workdir,
		"-w", d.workdir,
	}
	if d.cfg.Network != "" {
		args = append(args, "--network", d.cfg.Network)
	}
	if d.cfg.Memory != "" {
		args = append(args, "--memory", d.cfg.Memory)
	}
	if d.cfg.CPUs != "" {
		args = append(args, "--cpus", d.cfg.CPUs)
	}
	if d.cfg.PidsLimit > 0 {
		args = append(args, "--pids-limit", fmt.Sprint(d.cfg.PidsLimit))
	}
	if d.cfg.User != "" {
		args = append(args, "--user", d.cfg.User)
	}
	for k, v := range d.cfg.Env {
		args = append(args, "-e", k+"="+v)
	}
	for _, m := range d.cfg.ExtraMounts {
		args = append(args, "-v", m)
	}
	args = append(args, d.cfg.ExtraRunArgs...)
	// The container is kept alive by a trivial PID that never exits; all real
	// work arrives through `docker exec`.
	args = append(args, d.cfg.Image, "sleep", "infinity")

	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := d.runCLI(cctx, args...)
	if err != nil {
		return fmt.Errorf("start container %s (image %s): %s", d.name, d.cfg.Image, firstLine(out))
	}
	return nil
}

// Exec runs a command inside the container.
func (d *Docker) Exec(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	return d.ExecStream(ctx, command, opts, Streams{})
}

// ExecStream runs a command inside the container, streaming output.
//
// # Timeout handling
//
// Killing the `docker exec` client does not kill the process it started inside
// the container, so a timed-out command would otherwise keep burning CPU. To
// bound that, the command is wrapped so its PID lands in a file inside the
// container, and on timeout we reap it with a best-effort in-container kill.
// If the container itself has become unhealthy, callers should treat the
// sandbox as dead and start a fresh one.
func (d *Docker) ExecStream(ctx context.Context, command string, opts ExecOptions, w Streams) (*ExecResult, error) {
	if d.dead {
		return nil, fmt.Errorf("docker sandbox %s is no longer usable", d.name)
	}
	start := time.Now()

	pidFile := "/tmp/.agenteval-pid-" + shortID()
	wrapped := fmt.Sprintf("echo $$ > %s; exec %s", pidFile, command)

	args := []string{"exec"}
	if opts.Stdin != "" {
		args = append(args, "-i")
	}
	args = append(args, "-w", d.execWorkdir(opts.Cwd))
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, d.name, "/bin/sh", "-c", wrapped)

	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, d.bin, args...)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
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
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		d.reap(ctx, pidFile)
	}
	if err != nil && res.ExitCode < 0 {
		res.Err = err
	}
	return res, nil
}

// reap performs a best-effort kill of a timed-out in-container process.
func (d *Docker) reap(ctx context.Context, pidFile string) {
	// Never let cleanup hang the caller; the whole point is that we already ran
	// out of patience once.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	script := fmt.Sprintf(
		"p=$(cat %s 2>/dev/null); [ -n \"$p\" ] && { kill -9 -\"$p\" 2>/dev/null || kill -9 \"$p\" 2>/dev/null; }; rm -f %s",
		pidFile, pidFile)
	_, _ = d.runCLI(rctx, "exec", d.name, "/bin/sh", "-c", script)
}

// Close stops and removes the container.
func (d *Docker) Close(ctx context.Context) error {
	if d.dead {
		return nil
	}
	if d.cfg.Keep {
		return nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	out, err := d.runCLI(cctx, "rm", "-f", "-v", d.name)
	if err != nil {
		return fmt.Errorf("remove container %s: %s", d.name, firstLine(out))
	}
	return nil
}

// execWorkdir resolves a workspace-relative cwd to a container path.
func (d *Docker) execWorkdir(rel string) string {
	if strings.TrimSpace(rel) == "" {
		return d.workdir
	}
	return d.workdir + "/" + strings.Trim(filepath.ToSlash(rel), "/")
}

// runCLI invokes the docker CLI on the host.
func (d *Docker) runCLI(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Client is a low-level docker CLI handle, useful for host-side helpers such as
// image existence checks and builds.
type Client struct {
	bin       string
	container string
}

// NewClient returns a docker CLI handle not yet bound to a container.
func NewClient(bin string) *Client {
	return &Client{bin: orDefault(bin, "docker")}
}

// ImageExists reports whether an image reference is present locally.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	cmd := exec.CommandContext(ctx, c.bin, "image", "inspect", image)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// BuildImage builds an image from a Dockerfile.
func (c *Client) BuildImage(ctx context.Context, dockerfile, tag string, buildArgs map[string]string) (string, error) {
	args := []string{"build", "-f", dockerfile, "-t", tag}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	args = append(args, filepath.Dir(dockerfile))
	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ---------------------------------------------------------------------------

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

func shortID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano() % 1e12)
	}
	return hex.EncodeToString(b[:])
}

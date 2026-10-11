package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

// BackendKind selects the isolation strategy for task workspaces.
type BackendKind string

const (
	// BackendLocal runs commands as host processes inside a temporary directory.
	// Fast and dependency-free; no filesystem or network boundary, so an agent
	// running here can read anything the invoking user can read.
	BackendLocal BackendKind = "local"
	// BackendDocker runs every command in a per-task container with the workspace
	// bind-mounted in. Slower to start, but constrains the agent's shell,
	// filesystem and network too.
	BackendDocker BackendKind = "docker"
)

// BackendConfig is the backend-independent description of a task sandbox.
//
// One struct rather than a union type: the fields that only apply to Docker are
// ignored by the local backend, which keeps the configuration file and the
// command-line flags flat and readable.
type BackendConfig struct {
	Kind BackendKind `json:"kind" yaml:"kind"`

	// Image is the container image (docker only). It must contain the agent CLI
	// and whatever runtimes the tasks need, because the agent runs inside it.
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
	// Network is passed to `docker run --network`. "none" is the recommended
	// setting for hermetic evaluation; use "bridge" when tasks must download
	// dependencies.
	Network string `json:"network,omitempty" yaml:"network,omitempty"`
	// Memory/CPUs/PidsLimit bound container resources (docker only).
	Memory    string `json:"memory,omitempty" yaml:"memory,omitempty"`
	CPUs      string `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	PidsLimit int    `json:"pids_limit,omitempty" yaml:"pids_limit,omitempty"`
	// User is a `docker run --user` value; empty uses the image default.
	User string `json:"user,omitempty" yaml:"user,omitempty"`
	// DockerBin overrides the docker executable path.
	DockerBin string `json:"docker_bin,omitempty" yaml:"docker_bin,omitempty"`
	// ExtraMounts are additional "host:container[:ro]" bind mounts.
	ExtraMounts []string `json:"extra_mounts,omitempty" yaml:"extra_mounts,omitempty"`
	// ExtraRunArgs are appended verbatim to `docker run`.
	ExtraRunArgs []string `json:"extra_run_args,omitempty" yaml:"extra_run_args,omitempty"`

	// Env is applied to every command in the sandbox, on top of the task's own
	// workspace env.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// InheritEnv forwards the harness process environment (local only). Default
	// true; set false for a hermetic environment.
	InheritEnv *bool `json:"inherit_env,omitempty" yaml:"inherit_env,omitempty"`
	// Shell/ShellArgs override the command interpreter (local only).
	Shell     string   `json:"shell,omitempty" yaml:"shell,omitempty"`
	ShellArgs []string `json:"shell_args,omitempty" yaml:"shell_args,omitempty"`

	// StartTimeout bounds how long a sandbox may take to become ready. Docker
	// pulls can be slow, so this defaults to two minutes for that backend.
	StartTimeout time.Duration `json:"start_timeout,omitempty" yaml:"start_timeout,omitempty"`
}

// Normalize fills defaults and validates the configuration.
func (c BackendConfig) Normalize() (BackendConfig, error) {
	if c.Kind == "" {
		c.Kind = BackendLocal
	}
	switch c.Kind {
	case BackendLocal:
		// Docker-only keys are harmless but silently ignoring a typo'd backend
		// is not: an explicitly set image on the local backend almost always
		// means the user forgot backend: docker.
		if strings.TrimSpace(c.Image) != "" {
			return c, fmt.Errorf(
				"backend %q ignores image %q; set backend: docker to use a container",
				c.Kind, c.Image)
		}
	case BackendDocker:
		if strings.TrimSpace(c.Image) == "" {
			return c, fmt.Errorf("backend %q requires image", c.Kind)
		}
	default:
		return c, fmt.Errorf("unknown backend %q (available: %s, %s)", c.Kind, BackendLocal, BackendDocker)
	}
	if c.StartTimeout <= 0 {
		if c.Kind == BackendDocker {
			c.StartTimeout = 2 * time.Minute
		} else {
			c.StartTimeout = 30 * time.Second
		}
	}
	if c.Network == "" {
		// A hermetic default is the whole point of the container backend.
		c.Network = "none"
	}
	return c, nil
}

// newSandbox creates a sandbox rooted at root, applying env on top of the
// task's workspace environment.
//
// Each task gets its own sandbox: sharing one across tasks would let a task
// observe the previous task's files, and for Docker it would also defeat the
// container's resource limits.
func newSandbox(ctx context.Context, cfg BackendConfig, root string, env map[string]string) (sandbox.Sandbox, error) {
	merged := mergeEnv(cfg.Env, env)

	startCtx := ctx
	if cfg.StartTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, cfg.StartTimeout)
		defer cancel()
	}

	switch cfg.Kind {
	case BackendDocker:
		sb, err := sandbox.NewDocker(startCtx, sandbox.DockerConfig{
			Root:          root,
			Image:         cfg.Image,
			Network:       cfg.Network,
			Memory:        cfg.Memory,
			CPUs:          cfg.CPUs,
			PidsLimit:     cfg.PidsLimit,
			User:          cfg.User,
			DockerBin:     cfg.DockerBin,
			Env:           merged,
			ExtraMounts:   cfg.ExtraMounts,
			ExtraRunArgs:  cfg.ExtraRunArgs,
			Keep:          false,
			ContainerName: "",
		})
		if err != nil {
			return nil, fmt.Errorf("start docker sandbox: %w", err)
		}
		return sb, nil
	case BackendLocal:
		sb, err := sandbox.NewLocal(sandbox.LocalConfig{
			Root:       root,
			Shell:      cfg.Shell,
			ShellArgs:  cfg.ShellArgs,
			Env:        merged,
			InheritEnv: cfg.InheritEnv,
			// The engine owns workspace lifetime: it keeps or removes the task
			// directory itself so that the durable artifact bundle survives even
			// when the workspace is deleted.
			KeepWorkspace: true,
		})
		if err != nil {
			return nil, fmt.Errorf("start local sandbox: %w", err)
		}
		return sb, nil
	default:
		return nil, fmt.Errorf("unknown backend %q", cfg.Kind)
	}
}

// mergeEnv layers overrides on top of base without mutating either map.
func mergeEnv(base, overrides map[string]string) map[string]string {
	if len(base) == 0 && len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// workspaceEnv converts a task's declared environment into a flat map.
func workspaceEnv(env []dataset.EnvVar) map[string]string {
	if len(env) == 0 {
		return nil
	}
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

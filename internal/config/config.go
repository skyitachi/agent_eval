// Package config loads the file that drives a complete workflow: which dataset
// to evaluate, how to run it, where results go, and how the server behaves.
//
// # Why the shape mirrors engine.Config
//
// The `run:` section decodes straight into engine.Config rather than into a
// parallel set of "config structs". A mirrored schema drifts: someone adds a
// field to the engine, forgets the copy, and the option becomes unreachable
// from the file with no compile error to say so. Decoding in place means every
// engine option is settable the moment it exists, and yaml's strict mode still
// rejects a typo'd key.
//
// The other sections (`report:`, `serve:`, `serve.rl:`) do have their own
// structs, because they are not projections of the engine: they describe what
// happens around a run — how a report is rendered, how a dataset is served —
// and have no single Go struct to decode into. They follow the same rule that
// matters, which is that decoding is strict, so a mistyped key is an error
// rather than a silently ignored line.
//
// # Durations
//
// Engine fields such as AgentTimeout are time.Duration. yaml.v3 already decodes
// the Go string form ("10m", "1h30m") into a Duration, so the file stays
// readable without any normalisation pass. A bare number is rejected on purpose:
// it is ambiguous between seconds, milliseconds and nanoseconds, and silently
// guessing one of them is how a 600-second timeout becomes 600 nanoseconds.
// durationHint attaches that explanation to the raw decode error.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/report"
)

// DefaultDatabase is where runs are recorded when nothing else is configured.
const DefaultDatabase = "agenteval.db"

// File is the on-disk configuration.
type File struct {
	// Dataset is a path to a dataset directory or a dataset.yaml file.
	Dataset string `yaml:"dataset"`
	// Database is the SQLite file that holds results.
	Database string `yaml:"database"`

	// Run configures the evaluation itself.
	Run engine.Config `yaml:"run"`

	// Report configures generated HTML reports.
	Report ReportConfig `yaml:"report"`
	// Serve configures the results server.
	Serve ServeConfig `yaml:"serve"`

	// Path records where this file was loaded from, for error messages and for
	// resolving relative paths.
	Path string `yaml:"-"`
}

// ReportConfig controls HTML report generation.
type ReportConfig struct {
	// Dir is where `aeval run` writes report.html. Empty disables it.
	Dir string `yaml:"dir"`
	// Title overrides the document title.
	Title string `yaml:"title"`
	// MaxEventsPerTask, MaxToolCallsPerTask, MaxFieldBytes and MaxInlineBytes
	// bound the payload; see report.Options.
	MaxEventsPerTask    int `yaml:"max_events_per_task"`
	MaxToolCallsPerTask int `yaml:"max_tool_calls_per_task"`
	MaxFieldBytes       int `yaml:"max_field_bytes"`
	MaxInlineBytes      int `yaml:"max_inline_bytes"`
	// IncludeRawConfig keeps the stored (redacted) run configuration in the
	// report. Default false: the report is meant to be shareable.
	IncludeRawConfig bool `yaml:"include_raw_config"`
}

// Options projects the file section onto the report package.
func (r ReportConfig) Options() report.Options {
	return report.Options{
		Title:               r.Title,
		MaxEventsPerTask:    r.MaxEventsPerTask,
		MaxToolCallsPerTask: r.MaxToolCallsPerTask,
		MaxFieldBytes:       r.MaxFieldBytes,
		MaxInlineBytes:      r.MaxInlineBytes,
		IncludeRawConfig:    r.IncludeRawConfig,
	}
}

// ServeConfig controls the results server.
type ServeConfig struct {
	Addr string `yaml:"addr"`
	// AllowDelete enables the destructive run-deletion route.
	AllowDelete bool `yaml:"allow_delete"`
	// LiveRefreshSeconds makes the index reload itself, which is how a run in
	// progress can be watched.
	LiveRefreshSeconds int `yaml:"live_refresh_seconds"`
	// RL configures the RL environment service that `serve --rl` exposes.
	RL RLEnvConfig `yaml:"rl"`
}

// RLEnvConfig controls the RL environment service.
//
// It lives under `serve` rather than under `run` because it describes how the
// dataset is *served*, not how it is evaluated: the same dataset can be
// evaluated with one concurrency and served as environments with another. The
// budgets in particular have no counterpart in an evaluation, where the number
// of cells is fixed by the dataset rather than decided by a remote caller.
//
// Every field defaults to the same value as the corresponding flag, and a flag
// that is actually typed overrides the file.
type RLEnvConfig struct {
	// Enabled serves the /api/rl routes at all. Off by default: this is the
	// only part of the tool that acts rather than renders.
	Enabled bool `yaml:"enabled"`
	// AllowExec lets /api/rl run commands and write files inside an
	// environment -- arbitrary code execution as the harness's user.
	AllowExec bool `yaml:"allow_exec"`
	// MaxEnvs bounds how many environments may be open at once.
	MaxEnvs int `yaml:"max_envs"`
	// TTL bounds how long one environment may stay open.
	TTL time.Duration `yaml:"ttl"`
	// MaxExecs bounds commands per environment.
	MaxExecs int `yaml:"max_execs"`
	// MaxRollouts bounds how many rollouts this process performs in total.
	// Zero means unlimited.
	MaxRollouts int `yaml:"max_rollouts"`
	// MaxCostUSD caps cumulative agent cost. Zero means unlimited.
	MaxCostUSD float64 `yaml:"max_cost_usd"`
}

// Config is a fully resolved configuration.
type Config struct {
	// DatasetPath is the absolute dataset path.
	DatasetPath string
	// DatabasePath is the absolute SQLite path.
	DatabasePath string

	Engine engine.Config
	Report ReportConfig
	Serve  ServeConfig

	// Source is the config file that was loaded, empty when defaults were used.
	Source string
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// candidateNames are searched, in order, when no path is given.
var candidateNames = []string{"aeval.yaml", "aeval.yml", ".aeval.yaml", "agenteval.yaml", "agenteval.yml"}

// Load reads a configuration file.
//
// An empty path searches the working directory for a conventional name and
// falls back to built-in defaults, so `aeval run --dataset X` works with no
// configuration file at all.
func Load(path string) (*File, error) {
	if path == "" {
		found, err := findDefault()
		if err != nil {
			return nil, err
		}
		if found == "" {
			return &File{}, nil
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw, abs)
}

func findDefault() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, name := range candidateNames {
		p := filepath.Join(wd, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// Parse decodes a configuration document. dir is the directory the document
// lives in, used to resolve relative paths.
//
// Decoding is strict: an unknown key is an error rather than a silently ignored
// setting. Silent misconfiguration is the expensive failure mode -- the run
// completes, and the numbers are wrong.
func Parse(raw []byte, dir string) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty document is a legal configuration: every value has a
			// default, and a file with only comments is a reasonable starting
			// point.
			return &File{Path: dir}, nil
		}
		return nil, fmt.Errorf("decode config: %w%s", err, durationHint(err))
	}
	f.Path = dir
	if err := expandEnv(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// durationHint explains the syntax when yaml rejects a duration.
//
// yaml decodes time.Duration from a string natively ("10m", "1h30m"), and
// refuses a bare number. Its own message says only which line, so the useful
// half -- that a unit is required -- is supplied here.
func durationHint(err error) string {
	msg := err.Error()
	if !strings.Contains(msg, "into time.Duration") {
		return ""
	}
	return "\n  hint: durations are written as a string with a unit, e.g. agent_timeout: 10m" +
		"\n  keys that take a duration: agent_timeout, task_timeout, retry_backoff, start_timeout, agent.timeout"
}

// expandEnv expands ${VAR} in environment values.
//
// Credentials belong in the environment, not in a file that gets committed, so
// `ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}` has to work. Only the two env maps
// are expanded: running expansion over every string would corrupt task prompts,
// which legitimately contain dollar signs and shell syntax.
//
// A referenced variable that is not set is an error. Substituting an empty
// string would produce a run that fails deep inside the agent with a confusing
// message, which is exactly the failure this expansion exists to prevent.
func expandEnv(f *File) error {
	agentEnv, err := expandMap("run.agent.env", f.Run.Agent.Env)
	if err != nil {
		return err
	}
	f.Run.Agent.Env = agentEnv

	backendEnv, err := expandMap("run.backend.env", f.Run.Backend.Env)
	if err != nil {
		return err
	}
	f.Run.Backend.Env = backendEnv
	return nil
}

func expandMap(where string, in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		var missing []string
		expanded := os.Expand(v, func(name string) string {
			if name == "$" { // "$$" is a literal dollar sign
				return "$"
			}
			val, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s.%s references unset environment variable(s): %s",
				where, k, strings.Join(missing, ", "))
		}
		out[k] = expanded
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Resolving
// ---------------------------------------------------------------------------

// Resolve turns the file into an absolute, ready-to-use configuration.
func (f *File) Resolve() (*Config, error) {
	base := "."
	if f.Path != "" {
		base = filepath.Dir(f.Path)
	}

	cfg := &Config{
		DatasetPath:  f.Dataset,
		DatabasePath: f.Database,
		Engine:       f.Run,
		Report:       f.Report,
		Serve:        f.Serve,
		Source:       f.Path,
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(base, DefaultDatabase)
	}
	cfg.DatabasePath = absFrom(base, cfg.DatabasePath)
	if cfg.DatasetPath != "" {
		cfg.DatasetPath = absFrom(base, cfg.DatasetPath)
	}
	// Every path in the file resolves against the file's own directory, not the
	// shell's working directory. Two paths out of four would be worse than none:
	// the config would then be reproducible from some directories and not
	// others, and `--report out.html` next to `report.dir: out` would silently
	// write to two different places.
	if cfg.Report.Dir != "" {
		cfg.Report.Dir = absFrom(base, cfg.Report.Dir)
	}
	if cfg.Engine.WorkRoot != "" {
		cfg.Engine.WorkRoot = absFrom(base, cfg.Engine.WorkRoot)
	}
	// agent.bin is normally a bare command name resolved on PATH inside the
	// sandbox ("claude"), which must be left alone. When it contains a
	// separator it names a location, and a location in a config file follows
	// the same rule as every other path in it.
	if looksLikePath(cfg.Engine.Agent.Bin) {
		cfg.Engine.Agent.Bin = absFrom(base, cfg.Engine.Agent.Bin)
	}
	return cfg, nil
}

// looksLikePath reports whether s names a location rather than a command to be
// looked up on PATH. "claude" is a command; "./agent.sh" and "bin/agent" are
// paths.
func looksLikePath(s string) bool {
	return strings.ContainsRune(s, '/') ||
		strings.ContainsRune(s, filepath.Separator) ||
		s == "." || s == ".."
}

// absFrom makes p absolute relative to base, leaving an already-absolute p alone.
func absFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// Validate reports configuration problems that would otherwise surface only
// after a sandbox has been created or money spent.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DatasetPath) == "" {
		return errors.New("no dataset configured: set `dataset:` in the config file or pass --dataset")
	}
	if strings.TrimSpace(c.DatabasePath) == "" {
		return errors.New("no database configured")
	}
	if c.Engine.SamplesPerTask < 0 {
		return fmt.Errorf("samples_per_task must be >= 0, got %d", c.Engine.SamplesPerTask)
	}
	if c.Engine.Concurrency < 0 {
		return fmt.Errorf("concurrency must be >= 0, got %d", c.Engine.Concurrency)
	}
	if c.Engine.GraderThreshold < 0 || c.Engine.GraderThreshold > 1 {
		return fmt.Errorf("grader_threshold must be within [0,1], got %v", c.Engine.GraderThreshold)
	}
	if c.Engine.Retries < 0 {
		return fmt.Errorf("retries must be >= 0, got %d", c.Engine.Retries)
	}
	// The RL budgets are checked only when the service is actually enabled. One
	// config file drives every subcommand, so failing `aeval run` over a
	// negative `serve.rl.max_envs` would make a section the run never reads
	// capable of breaking it.
	//
	// Zero is *not* rejected for max_envs or max_execs here even though the
	// manager substitutes a default for it: a zero means "unset", and the
	// substitution is where that is decided, in `rlenv.Options.withDefaults`.
	if rl := c.Serve.RL; rl.Enabled {
		if rl.MaxEnvs < 0 {
			return fmt.Errorf("serve.rl.max_envs must be >= 0, got %d", rl.MaxEnvs)
		}
		if rl.MaxExecs < 0 {
			return fmt.Errorf("serve.rl.max_execs must be >= 0, got %d", rl.MaxExecs)
		}
		if rl.MaxRollouts < 0 {
			return fmt.Errorf("serve.rl.max_rollouts must be >= 0, got %d", rl.MaxRollouts)
		}
		if rl.MaxCostUSD < 0 {
			return fmt.Errorf("serve.rl.max_cost_usd must be >= 0, got %v", rl.MaxCostUSD)
		}
		if rl.TTL < 0 {
			return fmt.Errorf("serve.rl.ttl must be >= 0, got %s", rl.TTL)
		}
	}
	return nil
}

// ReportOptions returns the report options implied by the configuration.
func (c *Config) ReportOptions() report.Options {
	return c.Report.Options()
}

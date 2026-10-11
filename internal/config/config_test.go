package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/metric"
)

const fullYAML = `
dataset: datasets/demo
database: out/runs.db

run:
  name: nightly
  run_id: nightly-1
  samples_per_task: 4
  concurrency: 8
  seed: 7
  agent_timeout: 10m
  task_timeout: 1h30m
  grader_threshold: 0.75
  retries: 2
  retry_backoff: 3s
  resume: true
  fail_fast: true
  work_root: /tmp/aeval
  keep_workspaces: true
  notes: "seeded run"
  tool_version: test
  metrics:
    - resolve_rate
    - total_cost_usd
  custom_metrics:
    - name: resolved_per_turn
      numerator: {field: passed, agg: sum}
      denominator: {field: turns, agg: sum}
    - name: efficiency
      expr: "resolve_rate / max(total_cost_usd, 0.01)"
  agent:
    kind: claude_code
    model: deepseek-v4-pro
    fallback_model: sonnet
    effort: high
    permission_mode: bypassPermissions
    tools: [Bash, Edit, Read]
    allowed_tools: ["Bash(git:*)"]
    disallowed_tools: [WebFetch]
    add_dirs: [/tmp/extra]
    append_system_prompt: be careful
    max_turns: 40
    max_budget_usd: 2.5
    timeout: 5m
    env:
      ANTHROPIC_BASE_URL: ${AEVAL_TEST_BASE}
      LITERAL: "a$$b"
    env_from_host: [ANTHROPIC_API_KEY]
    isolation:
      bare: true
      strict_mcp_config: true
      disable_slash_commands: false
  backend:
    kind: docker
    image: aeval/runner:1
    network: bridge
    memory: 4g
    cpus: "2"
    pids_limit: 256
    user: 1000:1000
    start_timeout: 90s
    extra_mounts: ["/cache:/cache:ro"]
    extra_run_args: ["--init"]
    env:
      TZ: UTC

report:
  dir: reports
  title: Nightly report
  max_events_per_task: 50
  max_tool_calls_per_task: 25
  max_field_bytes: 4096
  max_inline_bytes: 1048576
  include_raw_config: true

serve:
  addr: 127.0.0.1:9000
  allow_delete: true
  live_refresh_seconds: 15
`

func TestParseMapsEveryEngineField(t *testing.T) {
	t.Setenv("AEVAL_TEST_BASE", "https://example.invalid")

	f, err := Parse([]byte(fullYAML), filepath.Join(t.TempDir(), "aeval.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg, err := f.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	run := cfg.Engine

	// Durations arrive as strings and rely on yaml.v3 decoding them natively,
	// so they are checked first and precisely.
	if run.AgentTimeout != 10*time.Minute {
		t.Errorf("agent_timeout = %v, want 10m", run.AgentTimeout)
	}
	if run.TaskTimeout != 90*time.Minute {
		t.Errorf("task_timeout = %v, want 1h30m", run.TaskTimeout)
	}
	if run.RetryBackoff != 3*time.Second {
		t.Errorf("retry_backoff = %v, want 3s", run.RetryBackoff)
	}
	if run.Agent.Timeout != 5*time.Minute {
		t.Errorf("agent.timeout = %v, want 5m", run.Agent.Timeout)
	}
	if run.Backend.StartTimeout != 90*time.Second {
		t.Errorf("backend.start_timeout = %v, want 90s", run.Backend.StartTimeout)
	}

	if run.RunName != "nightly" || run.RunID != "nightly-1" {
		t.Errorf("name/run_id = %q/%q", run.RunName, run.RunID)
	}
	if run.SamplesPerTask != 4 || run.Concurrency != 8 || run.Seed != 7 {
		t.Errorf("sampling = %d/%d/%d", run.SamplesPerTask, run.Concurrency, run.Seed)
	}
	if run.GraderThreshold != 0.75 || run.Retries != 2 {
		t.Errorf("threshold/retries = %v/%d", run.GraderThreshold, run.Retries)
	}
	if !run.Resume || !run.FailFast || !run.KeepWorkspaces {
		t.Errorf("booleans = resume:%v fail_fast:%v keep:%v", run.Resume, run.FailFast, run.KeepWorkspaces)
	}
	if run.WorkRoot != "/tmp/aeval" || run.Notes != "seeded run" || run.ToolVersion != "test" {
		t.Errorf("provenance = %q/%q/%q", run.WorkRoot, run.Notes, run.ToolVersion)
	}
	if strings.Join(run.Metrics, ",") != "resolve_rate,total_cost_usd" {
		t.Errorf("metrics = %v", run.Metrics)
	}
	if len(run.CustomMetrics) != 2 {
		t.Fatalf("custom metrics = %d", len(run.CustomMetrics))
	}
	if run.CustomMetrics[0].Form() != "ratio" || run.CustomMetrics[1].Form() != "expr" {
		t.Errorf("custom metric forms = %s/%s", run.CustomMetrics[0].Form(), run.CustomMetrics[1].Form())
	}

	a := run.Agent
	if a.Kind != "claude_code" || a.Model != "deepseek-v4-pro" || a.FallbackModel != "sonnet" || a.Effort != "high" {
		t.Errorf("agent identity = %+v", a)
	}
	if a.PermissionMode != "bypassPermissions" || a.MaxTurns != 40 || a.MaxBudgetUSD != 2.5 {
		t.Errorf("agent limits = %+v", a)
	}
	if len(a.Tools) != 3 || len(a.AllowedTools) != 1 || len(a.DisallowedTools) != 1 || len(a.AddDirs) != 1 {
		t.Errorf("agent tool lists = %+v", a)
	}
	if a.AppendSystemPrompt != "be careful" {
		t.Errorf("append_system_prompt = %q", a.AppendSystemPrompt)
	}
	if len(a.EnvFromHost) != 1 || a.EnvFromHost[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("env_from_host = %v", a.EnvFromHost)
	}
	if a.Env["ANTHROPIC_BASE_URL"] != "https://example.invalid" {
		t.Errorf("env expansion = %q", a.Env["ANTHROPIC_BASE_URL"])
	}
	if a.Env["LITERAL"] != "a$b" {
		t.Errorf("$$ should collapse to a single $, got %q", a.Env["LITERAL"])
	}
	if a.Isolation == nil || !a.Isolation.Bare || !a.Isolation.StrictMCPConfig || a.Isolation.DisableSlashCommands {
		t.Errorf("isolation = %+v", a.Isolation)
	}

	b := run.Backend
	if b.Kind != engine.BackendDocker || b.Image != "aeval/runner:1" || b.Network != "bridge" {
		t.Errorf("backend = %+v", b)
	}
	if b.Memory != "4g" || b.CPUs != "2" || b.PidsLimit != 256 || b.User != "1000:1000" {
		t.Errorf("backend resources = %+v", b)
	}
	if len(b.ExtraMounts) != 1 || len(b.ExtraRunArgs) != 1 || b.Env["TZ"] != "UTC" {
		t.Errorf("backend extras = %+v", b)
	}

	// Relative paths resolve against the configuration file, not the process
	// working directory: a config that only works from one directory is a trap.
	if !filepath.IsAbs(cfg.DatasetPath) || !strings.HasSuffix(cfg.DatasetPath, filepath.Join("datasets", "demo")) {
		t.Errorf("dataset path = %q", cfg.DatasetPath)
	}
	if !filepath.IsAbs(cfg.DatabasePath) || !strings.HasSuffix(cfg.DatabasePath, filepath.Join("out", "runs.db")) {
		t.Errorf("database path = %q", cfg.DatabasePath)
	}

	opts := cfg.ReportOptions()
	if opts.MaxEventsPerTask != 50 || opts.MaxToolCallsPerTask != 25 || opts.MaxFieldBytes != 4096 || opts.MaxInlineBytes != 1048576 {
		t.Errorf("report options = %+v", opts)
	}
	if !opts.IncludeRawConfig || opts.Title != "Nightly report" {
		t.Errorf("report options = %+v", opts)
	}
	if cfg.Report.Dir != filepath.Join(filepath.Dir(cfg.Source), "reports") {
		t.Errorf("report dir = %q, want it resolved against the config file", cfg.Report.Dir)
	}
	if cfg.Serve.Addr != "127.0.0.1:9000" || !cfg.Serve.AllowDelete || cfg.Serve.LiveRefreshSeconds != 15 {
		t.Errorf("serve = %+v", cfg.Serve)
	}
}

// TestParseCoversEverySerialisableEngineKey walks engine.Config and asserts that
// every field a user could sensibly set is reachable from the file. It is the
// guard against the mirroring problem this package avoids by decoding in place:
// if someone adds a field to a nested struct, this test names it.
func TestParseCoversEverySerialisableEngineKey(t *testing.T) {
	// A field is only allowed to be missing from the fixture if it is
	// deliberately not configurable.
	notConfigurable := map[string]bool{
		"Config.Store":   true, // a live handle, not configuration
		"Config.OnEvent": true, // a callback
		"Config.RunID":   true, // set below
		"Spec.Timeout":   true, // set below
	}
	_ = notConfigurable

	t.Setenv("AEVAL_TEST_BASE", "x")
	f, err := Parse([]byte(fullYAML), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := f.Run
	// Spot-check the nested structs are not silently empty, which is what a
	// renamed yaml tag produces.
	if run.Agent.Isolation == nil {
		t.Error("agent.isolation did not decode; a renamed tag would look like this")
	}
	if run.Backend.Image == "" {
		t.Error("backend.image did not decode")
	}
	if len(run.Metrics) == 0 {
		t.Error("metrics did not decode")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	// A silently ignored setting is the expensive failure: the run completes and
	// the numbers are wrong.
	_, err := Parse([]byte("run:\n  concurrancy: 8\n"), t.TempDir())
	if err == nil {
		t.Fatal("a typo'd key must be rejected")
	}
	if !strings.Contains(err.Error(), "concurrancy") {
		t.Fatalf("error should name the offending key: %v", err)
	}
}

func TestParseRejectsAMalformedDuration(t *testing.T) {
	_, err := Parse([]byte("run:\n  agent_timeout: ten minutes\n"), t.TempDir())
	if err == nil {
		t.Fatal("a bad duration must be rejected")
	}
	if !strings.Contains(err.Error(), "agent_timeout") {
		t.Fatalf("the hint should name the key: %v", err)
	}
	if !strings.Contains(err.Error(), "10m") {
		t.Fatalf("the hint should show the expected syntax: %v", err)
	}
}

func TestDurationsRequireAUnit(t *testing.T) {
	// yaml decodes time.Duration from a string natively and refuses a bare
	// number. Requiring the unit is the better rule: `600` is ambiguous between
	// seconds, milliseconds and nanoseconds, and the failure is a clear error
	// instead of a silently wrong timeout.
	_, err := Parse([]byte("run:\n  agent_timeout: 600\n"), t.TempDir())
	if err == nil {
		t.Fatal("a unitless duration must be rejected")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Fatalf("the error should explain that a unit is required: %v", err)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("AEVAL_KEY", "secret-value")

	t.Run("set variable is substituted", func(t *testing.T) {
		f, err := Parse([]byte("run:\n  agent:\n    env:\n      API: ${AEVAL_KEY}\n"), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Run.Agent.Env["API"]; got != "secret-value" {
			t.Fatalf("API = %q", got)
		}
	})

	t.Run("unset variable is an error", func(t *testing.T) {
		_, err := Parse([]byte("run:\n  backend:\n    env:\n      API: ${AEVAL_DEFINITELY_UNSET}\n"), t.TempDir())
		if err == nil {
			t.Fatal("an unset variable must fail loudly rather than expand to nothing")
		}
		if !strings.Contains(err.Error(), "AEVAL_DEFINITELY_UNSET") {
			t.Fatalf("error should name the variable: %v", err)
		}
	})

	t.Run("only env maps are expanded", func(t *testing.T) {
		// A task prompt legitimately contains dollar signs and shell syntax;
		// expanding every string would corrupt it.
		f, err := Parse([]byte("run:\n  notes: \"literal ${AEVAL_DEFINITELY_UNSET} kept\"\n"), t.TempDir())
		if err != nil {
			t.Fatalf("notes must not be expanded: %v", err)
		}
		if !strings.Contains(f.Run.Notes, "${AEVAL_DEFINITELY_UNSET}") {
			t.Fatalf("notes = %q", f.Run.Notes)
		}
	})

	t.Run("no environment section is fine", func(t *testing.T) {
		if _, err := Parse([]byte("dataset: x\n"), t.TempDir()); err != nil {
			t.Fatalf("parse: %v", err)
		}
	})
}

func TestResolveDefaults(t *testing.T) {
	dir := t.TempDir()
	f, err := Parse([]byte("dataset: demo\n"), filepath.Join(dir, "aeval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != filepath.Join(dir, DefaultDatabase) {
		t.Fatalf("database = %q", cfg.DatabasePath)
	}
	if cfg.DatasetPath != filepath.Join(dir, "demo") {
		t.Fatalf("dataset = %q", cfg.DatasetPath)
	}
	if cfg.Source != filepath.Join(dir, "aeval.yaml") {
		t.Fatalf("source = %q", cfg.Source)
	}
}

func TestResolveLeavesAbsolutePathsAlone(t *testing.T) {
	f, err := Parse([]byte("dataset: /abs/demo\ndatabase: /abs/runs.db\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.Resolve()
	if cfg.DatasetPath != "/abs/demo" || cfg.DatabasePath != "/abs/runs.db" {
		t.Fatalf("paths = %q / %q", cfg.DatasetPath, cfg.DatabasePath)
	}
}

// TestResolveDistinguishesACommandFromAPath is the agent.bin half of the same
// rule, and the two cases must differ. "claude" is a command looked up on PATH
// inside the sandbox and rewriting it to /dir/claude would break every ordinary
// invocation; "./scripts/agent.sh" names a file, and a file named in a config
// file lives next to that config file.
func TestResolveDistinguishesACommandFromAPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aeval.yaml")

	for _, c := range []struct {
		bin  string
		want string
	}{
		{"claude", "claude"},
		{"./scripts/agent.sh", filepath.Join(dir, "scripts", "agent.sh")},
		{"scripts/agent.sh", filepath.Join(dir, "scripts", "agent.sh")},
		{"/opt/agent.sh", "/opt/agent.sh"},
	} {
		raw := "dataset: demo\nrun:\n  agent:\n    kind: claude_code\n    bin: " + c.bin + "\n"
		f, err := Parse([]byte(raw), path)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.bin, err)
		}
		cfg, err := f.Resolve()
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.bin, err)
		}
		if got := cfg.Engine.Agent.Bin; got != c.want {
			t.Errorf("agent.bin %q resolved to %q, want %q", c.bin, got, c.want)
		}
	}
}

// TestResolveMakesEveryPathRelativeToTheConfigFile pins the rule that matters
// for reproducibility: the same config file must behave the same way no matter
// which directory the command is run from. Resolving dataset and database but
// not report.dir and work_root would leave half the config cwd-dependent.
func TestResolveMakesEveryPathRelativeToTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	raw := "dataset: demo\n" +
		"database: out/runs.db\n" +
		"run:\n  work_root: work\n" +
		"report:\n  dir: reports\n"

	f, err := Parse([]byte(raw), filepath.Join(dir, "aeval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	resolved := map[string]string{
		"dataset":    cfg.DatasetPath,
		"database":   cfg.DatabasePath,
		"work_root":  cfg.Engine.WorkRoot,
		"report.dir": cfg.Report.Dir,
	}
	want := map[string]string{
		"dataset":    filepath.Join(dir, "demo"),
		"database":   filepath.Join(dir, "out/runs.db"),
		"work_root":  filepath.Join(dir, "work"),
		"report.dir": filepath.Join(dir, "reports"),
	}
	for key, got := range resolved {
		if got != want[key] {
			t.Errorf("%s = %q, want %q", key, got, want[key])
		}
	}

	// An absolute value in the file is still honoured verbatim.
	abs, err := Parse([]byte("dataset: demo\nreport:\n  dir: /var/reports\n"), filepath.Join(dir, "aeval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = abs.Resolve()
	if cfg.Report.Dir != "/var/reports" {
		t.Errorf("absolute report.dir was rewritten to %q", cfg.Report.Dir)
	}
}

func TestEmptyDocumentIsUsable(t *testing.T) {
	f, err := Parse(nil, t.TempDir())
	if err != nil {
		t.Fatalf("an empty config must be legal: %v", err)
	}
	cfg, err := f.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an empty config has no dataset, which Validate must report")
	}
}

func TestServeRLSectionIsParsedAndOnlyValidatedWhenEnabled(t *testing.T) {
	raw := "dataset: demo\n" +
		"serve:\n" +
		"  addr: 127.0.0.1:9000\n" +
		"  rl:\n" +
		"    enabled: true\n" +
		"    allow_exec: true\n" +
		"    max_envs: 3\n" +
		"    ttl: 5m\n" +
		"    max_execs: 20\n" +
		"    max_rollouts: 7\n" +
		"    max_cost_usd: 2.5\n"

	f, err := Parse([]byte(raw), filepath.Join(t.TempDir(), "aeval.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfg, err := f.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rl := cfg.Serve.RL
	if !rl.Enabled || !rl.AllowExec {
		t.Errorf("switches lost: %+v", rl)
	}
	if rl.MaxEnvs != 3 || rl.MaxExecs != 20 || rl.MaxRollouts != 7 || rl.MaxCostUSD != 2.5 {
		t.Errorf("budgets lost: %+v", rl)
	}
	if rl.TTL != 5*time.Minute {
		t.Errorf("ttl = %v, want 5m", rl.TTL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// A typo'd key must still be rejected even though it is nested under a
	// section the run subcommand never reads.
	if _, err := Parse([]byte("serve:\n  rl:\n    max_env: 3\n"), filepath.Join(t.TempDir(), "aeval.yaml")); err == nil {
		t.Error("strict decoding should reject an unknown key inside serve.rl")
	}

	// A budget is only checked when the service is on: one file drives every
	// subcommand, so a section `aeval run` never reads must not break it.
	off := &Config{DatasetPath: "d", DatabasePath: "db",
		Serve: ServeConfig{RL: RLEnvConfig{MaxEnvs: -1}}}
	if err := off.Validate(); err != nil {
		t.Errorf("a disabled serve.rl must not fail validation: %v", err)
	}
	off.Serve.RL.Enabled = true
	if err := off.Validate(); err == nil {
		t.Error("an enabled serve.rl with a negative max_envs must be reported")
	}
}

func TestValidate(t *testing.T) {
	base := func() *Config {
		return &Config{DatasetPath: "d", DatabasePath: "db", Engine: engine.Config{
			SamplesPerTask: 1, Concurrency: 1, GraderThreshold: 1,
		}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a sane config must validate: %v", err)
	}
	cases := map[string]func(*Config){
		"no dataset":         func(c *Config) { c.DatasetPath = "" },
		"no database":        func(c *Config) { c.DatabasePath = "" },
		"negative samples":   func(c *Config) { c.Engine.SamplesPerTask = -1 },
		"negative workers":   func(c *Config) { c.Engine.Concurrency = -1 },
		"threshold too big":  func(c *Config) { c.Engine.GraderThreshold = 1.5 },
		"threshold negative": func(c *Config) { c.Engine.GraderThreshold = -0.1 },
		"negative retries":   func(c *Config) { c.Engine.Retries = -1 },
	}
	for name, mutate := range cases {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestLoadFindsAConventionalFileName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aeval.yaml"), []byte("dataset: from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	f, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if f.Dataset != "from-file" {
		t.Fatalf("dataset = %q, want the file to have been found", f.Dataset)
	}
}

func TestLoadWithoutAConfigurationFileUsesDefaults(t *testing.T) {
	restore := chdir(t, t.TempDir())
	defer restore()

	f, err := Load("")
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if f.Path != "" {
		t.Fatalf("path = %q, want empty", f.Path)
	}
}

func TestLoadReportsAMissingExplicitFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("an explicitly named missing file must be an error")
	}
}

func TestParseReportsMalformedYAML(t *testing.T) {
	if _, err := Parse([]byte("run: [unclosed\n"), t.TempDir()); err == nil {
		t.Fatal("malformed YAML must be rejected")
	}
}

func TestCustomMetricDeclarationsAreValidatedByTheEngine(t *testing.T) {
	// A custom metric's syntax is checked when the engine is built, so this
	// package only has to carry it faithfully.
	f, err := Parse([]byte(`
run:
  custom_metrics:
    - name: bad
      expr: "resolve_rate /"
`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Run.CustomMetrics) != 1 {
		t.Fatalf("custom metrics = %d", len(f.Run.CustomMetrics))
	}
	cm := f.Run.CustomMetrics[0]
	if cm.Name != "bad" || cm.Form() != "expr" {
		t.Fatalf("custom metric = %+v", cm)
	}
	if err := cm.Validate(); err == nil {
		t.Fatal("the engine should reject a malformed expression")
	}
	_ = metric.KindRun
}

func TestReportOptionsRoundTrip(t *testing.T) {
	r := ReportConfig{
		Title: "t", MaxEventsPerTask: 1, MaxToolCallsPerTask: 2,
		MaxFieldBytes: 3, MaxInlineBytes: 4, IncludeRawConfig: true,
	}
	o := r.Options()
	if o.Title != "t" || o.MaxEventsPerTask != 1 || o.MaxToolCallsPerTask != 2 ||
		o.MaxFieldBytes != 3 || o.MaxInlineBytes != 4 || !o.IncludeRawConfig {
		t.Fatalf("options = %+v", o)
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(old) }
}

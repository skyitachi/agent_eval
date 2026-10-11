// Command aeval runs evaluations and inspects their results.
//
// # Exit codes
//
//	0   success
//	1   the operation failed (bad configuration, a run that aborted, ...)
//	2   the command line itself was wrong
//	130 interrupted
//
// A run where tasks failed is *not* an error: a resolve rate below 1 is the
// normal outcome of an evaluation, not a malfunction, and conflating the two
// would make the tool unusable in CI. Only a run that could not be carried out
// reports failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/audit"
	"github.com/skyitachi/agenteval/internal/config"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/metric"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/rlenv"
	"github.com/skyitachi/agenteval/internal/storage"
	"github.com/skyitachi/agenteval/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	exitCtlC  = 130
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "audit":
		return cmdAudit(rest)
	case "validate":
		return cmdValidate(rest)
	case "list", "ls":
		return cmdList(rest)
	case "report":
		return cmdReport(rest)
	case "compare":
		return cmdCompare(rest)
	case "serve":
		return cmdServe(rest)
	case "registry", "metrics":
		return cmdRegistry(rest)
	case "version", "--version", "-v":
		return cmdVersion()
	case "help", "--help", "-h":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `aeval evaluates agent runs against a dataset and reports the results.

Usage:
  aeval <command> [flags]

Commands:
  run        Evaluate a dataset and record the results
  audit      Check whether a dataset's tasks are solvable, necessary and stable
  validate   Check a dataset and a configuration file without running anything
  list       List recorded runs
  report     Write an HTML report for a recorded run
  compare    Write an HTML comparison of two recorded runs
  serve      Serve recorded runs over HTTP, with a browsable report UI
  registry   List the registered agents, graders and metrics
  version    Print build and environment information

Global flags (accepted by every command):
  --config <path>    Configuration file (default: aeval.yaml if present)
  --db <path>        SQLite database of results (default: agenteval.db)
  --dataset <path>   Dataset directory or dataset.yaml

Run "aeval <command> --help" for the flags of one command.
`)
}

// ---------------------------------------------------------------------------
// Shared flags
// ---------------------------------------------------------------------------

type commonFlags struct {
	configPath  string
	dbPath      string
	datasetPath string
	verbose     bool
}

func addCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.configPath, "config", "", "configuration file (default: aeval.yaml when present)")
	fs.StringVar(&c.dbPath, "db", "", "SQLite database of results (default: agenteval.db)")
	fs.StringVar(&c.datasetPath, "dataset", "", "dataset directory or dataset.yaml")
	fs.BoolVar(&c.verbose, "verbose", false, "log progress events during a run")
}

// resolve loads the configuration file and applies the flag overrides.
//
// Flags lose to nothing and win over the file: a value typed on the command line
// is always the most recent intent.
func resolve(c *commonFlags) (*config.Config, error) {
	file, err := config.Load(c.configPath)
	if err != nil {
		return nil, err
	}
	if c.datasetPath != "" {
		file.Dataset = c.datasetPath
	}
	if c.dbPath != "" {
		file.Database = c.dbPath
	}
	cfg, err := file.Resolve()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolveWithDataset(c *commonFlags) (*config.Config, error) {
	cfg, err := resolve(c)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.DatasetPath) == "" {
		return nil, errors.New("no dataset: pass --dataset or set `dataset:` in the configuration file")
	}
	return cfg, nil
}

func openStore(cfg *config.Config) (*storage.Store, error) {
	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", cfg.DatabasePath, err)
	}
	return store, nil
}

// signalContext cancels on SIGINT/SIGTERM.
//
// The engine persists every cell as it finishes, so an interrupted run keeps the
// work already paid for and can be continued with --resume.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "aeval: "+format+"\n", args...)
	return exitError
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		c          commonFlags
		name       string
		runID      string
		samples    int
		workers    int
		threshold  float64
		retries    int
		resume     bool
		failFast   bool
		workRoot   string
		keep       bool
		reportPath string
		backend    string
		image      string
		model      string
		agentKind  string
		agentBin   string
		concurrent int
		mode       string
	)
	addCommon(fs, &c)
	fs.StringVar(&name, "name", "", "human label for this run")
	fs.StringVar(&runID, "run-id", "", "reuse an explicit run id (implied by --resume)")
	fs.IntVar(&samples, "samples", 0, "samples per task, k for pass@k (default 1)")
	fs.IntVar(&workers, "concurrency", 0, "cells executed at once (default 1)")
	fs.Float64Var(&threshold, "threshold", 0, "score required to pass a task (default 1.0)")
	fs.IntVar(&retries, "retries", 0, "extra attempts when the agent cannot be started")
	fs.BoolVar(&resume, "resume", false, "skip cells already recorded as completed")
	fs.BoolVar(&failFast, "fail-fast", false, "stop as soon as one cell fails hard")
	fs.StringVar(&workRoot, "work-root", "", "directory for workspaces and artifacts (default: a temp dir)")
	fs.BoolVar(&keep, "keep-workspaces", false, "leave each task's workspace on disk for inspection")
	fs.StringVar(&reportPath, "report", "", "write an HTML report to this path when the run finishes")
	fs.StringVar(&backend, "backend", "", "sandbox backend: local or docker")
	fs.StringVar(&image, "image", "", "container image (docker backend)")
	fs.StringVar(&model, "model", "", "model name passed to the agent CLI")
	fs.StringVar(&agentKind, "agent", "", "agent implementation (default claude_code)")
	fs.StringVar(&agentBin, "bin", "", "agent executable (default: claude on PATH; with --agent mock, the command to run)")
	fs.IntVar(&concurrent, "jobs", 0, "alias for --concurrency")
	fs.StringVar(&mode, "mode", "", "what each cell measures: agent, oracle (apply the reference solution) or baseline (grade the untouched workspace)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: aeval run [flags]"); fs.PrintDefaults() }

	_ = fs.Parse(args)
	// Which flags were actually typed, as opposed to left at their zero value.
	// Inferring "was it given?" from the value cannot tell `--threshold 0` from
	// no --threshold at all, and it forces a sentinel default (-1 for retries)
	// that then shows up in --help as two contradictory defaults.
	given := givenFlags(fs)
	if given["jobs"] {
		workers = concurrent
	}
	// A flag that names a location resolves against *this* shell's directory.
	// The agent, by contrast, runs inside a sandbox whose working directory is
	// the task workspace, so a relative --bin would be looked for in the wrong
	// tree and reported as an opaque agent_error with no hint about the cause.
	// A bare command name is not a location and is left alone: "claude" means
	// "on PATH", while "./agent.sh" and "scripts/agent.sh" mean a file.
	if given["bin"] {
		agentBin = absFlagPath(agentBin)
	}
	if given["work-root"] {
		workRoot = absFlagPath(workRoot)
	}
	if fs.NArg() > 0 {
		// A positional argument is almost always a dataset path typed without
		// the flag; accepting it is friendlier than a usage error.
		c.datasetPath = fs.Arg(0)
	}

	cfg, err := resolveWithDataset(&c)
	if err != nil {
		return fail("%v", err)
	}

	ds, err := dataset.Load(cfg.DatasetPath)
	if err != nil {
		return fail("load dataset: %v", err)
	}

	run := cfg.Engine
	applyRunFlags(&run, runFlags{
		name: name, runID: runID, samples: samples, workers: workers,
		threshold: threshold, retries: retries, resume: resume, failFast: failFast,
		workRoot: workRoot, keep: keep,
		backend: backend, image: image, model: model, agentKind: agentKind, agentBin: agentBin,
		mode: mode,
	}, given)
	run.ToolVersion = version

	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()
	run.Store = store

	ctx, stop := signalContext()
	defer stop()

	if c.verbose {
		run.OnEvent = progressLogger()
	}

	eng, err := engine.New(run)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(os.Stderr, "%s\n", ds.Summary())
	fmt.Fprintf(os.Stderr, "run %s · agent=%s model=%s backend=%s k=%d concurrency=%d\n",
		eng.Config().RunID, eng.Config().Agent.Kind, orDefault(eng.Config().Agent.Model, "(default)"),
		eng.Config().Backend.Kind, eng.Config().SamplesPerTask, eng.Config().Concurrency)
	if m := eng.Config().Mode; m != engine.RunModeAgent {
		// Say it out loud. An oracle or baseline run stores a result with the
		// same shape as an agent run, and the moment to stop a misreading is
		// before the numbers are printed, not in the report afterwards.
		fmt.Fprintf(os.Stderr, "  mode %s: no agent will run; the numbers below are not a model's score\n", m)
	}
	started := time.Now()

	res, runErr := eng.Run(ctx, ds)
	if res != nil {
		fmt.Fprint(os.Stdout, res.Summary())
	}
	if runErr != nil && res == nil {
		return fail("run: %v", runErr)
	}

	// Write the report from the database, not from the in-memory result, so the
	// command exercises exactly the path `aeval report` uses later.
	path := reportPath
	if path == "" && cfg.Report.Dir != "" && res != nil {
		path = filepath.Join(cfg.Report.Dir, report.FileName(res.RunID))
	}
	if path != "" && res != nil {
		if err := writeRunReport(ctx, store, res.RunID, path, cfg.Report.Options()); err != nil {
			fmt.Fprintf(os.Stderr, "aeval: report: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "report written to %s\n", path)
		}
	}

	if res == nil {
		return fail("run produced no result")
	}
	fmt.Fprintf(os.Stderr, "elapsed %s\n", time.Since(started).Round(time.Millisecond))

	switch res.Status {
	case storage.StatusCompleted:
		return exitOK
	case storage.StatusCanceled:
		return exitCtlC
	default:
		return exitError
	}
}

type runFlags struct {
	name, runID            string
	samples, workers       int
	threshold              float64
	retries                int
	resume, failFast, keep bool
	workRoot               string
	backend, image, model  string
	agentKind              string
	agentBin               string
	mode                   string
}

// applyRunFlags overlays command-line values onto the configured run.
//
// A flag is applied when it was *given*, not when its value is non-zero. The
// distinction matters: a config file is expected to survive a command line that
// only mentions one option, but `--concurrency 1` on top of a config that says 8
// is a deliberate request that a "non-zero wins" rule would honour only by luck.
func applyRunFlags(run *engine.Config, f runFlags, given map[string]bool) {
	if given["name"] {
		run.RunName = f.name
	}
	if given["run-id"] {
		run.RunID = f.runID
	}
	if given["samples"] {
		run.SamplesPerTask = f.samples
	}
	if given["concurrency"] || given["jobs"] {
		run.Concurrency = f.workers
	}
	if given["threshold"] {
		run.GraderThreshold = f.threshold
	}
	if given["retries"] {
		run.Retries = f.retries
	}
	if given["work-root"] {
		run.WorkRoot = f.workRoot
	}
	if given["backend"] {
		run.Backend.Kind = engine.BackendKind(f.backend)
	}
	if given["image"] {
		run.Backend.Image = f.image
	}
	if given["model"] {
		run.Agent.Model = f.model
	}
	if given["agent"] {
		run.Agent.Kind = f.agentKind
	}
	if given["bin"] {
		run.Agent.Bin = f.agentBin
	}
	if given["mode"] {
		// The value is validated by engine.New rather than here, so the error a
		// user sees names the accepted set in the engine's own words instead of
		// a second list that can drift out of sync with it.
		run.Mode = engine.RunMode(f.mode)
	}
	if f.resume {
		run.Resume = true
	}
	if f.failFast {
		run.FailFast = true
	}
	if f.keep {
		run.KeepWorkspaces = true
	}
}

// givenFlags returns the names of the flags the user actually passed.
func givenFlags(fs *flag.FlagSet) map[string]bool {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}

// absFlagPath makes a path-typed flag absolute against the current directory.
//
// A value with no separator is a command name to be found on PATH and is
// returned untouched, because turning "claude" into "/cwd/claude" would break
// every ordinary invocation.
func absFlagPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if !strings.ContainsRune(p, '/') && !strings.ContainsRune(p, filepath.Separator) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// progressLogger prints one line per finished cell.
func progressLogger() func(engine.Event) {
	return func(ev engine.Event) {
		switch ev.Kind {
		case engine.EventRunStart:
			fmt.Fprintf(os.Stderr, "→ %d cell(s) planned\n", ev.Total)
		case engine.EventTaskRetry:
			fmt.Fprintf(os.Stderr, "  retry %s#%d: %s\n", ev.TaskID, ev.SampleIndex, ev.Message)
		case engine.EventTaskDone:
			if ev.Result == nil {
				return
			}
			mark := "FAIL"
			if ev.Result.Passed() {
				mark = "ok  "
			}
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s %s#%d %.1fs %s\n",
				ev.Completed, ev.Total, mark, ev.Result.TaskID, ev.Result.SampleIndex,
				ev.Result.Duration().Seconds(), ev.Result.StopReason)
		}
	}
}

func writeRunReport(ctx context.Context, store *storage.Store, runID, path string, opts report.Options) error {
	data, err := report.Load(ctx, store, runID, opts)
	if err != nil {
		return err
	}
	return report.RenderRunFile(path, data, report.RenderOptions{Title: data.Title})
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

func cmdAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	var (
		c        commonFlags
		group    string
		repeats  int
		only     string
		workers  int
		asJSON   bool
		strict   bool
		keep     bool
		backend  string
		image    string
		agentBin string
	)
	addCommon(fs, &c)
	fs.StringVar(&group, "group", "", "id prefix and tag for the runs this audit creates (default: audit-<uuid>)")
	fs.IntVar(&repeats, "repeats", 2, "oracle runs per task; 1 skips the stability check")
	fs.StringVar(&only, "only", "", "restrict to checks: solvability,necessity,stability")
	fs.IntVar(&workers, "concurrency", 0, "cells executed at once (default: from the config)")
	fs.BoolVar(&asJSON, "json", false, "emit the audit report as JSON")
	fs.BoolVar(&strict, "strict", false, "exit non-zero when any task is unhealthy")
	fs.BoolVar(&keep, "keep-workspaces", false, "leave each task's workspace on disk for inspection")
	fs.StringVar(&backend, "backend", "", "sandbox backend: local or docker")
	fs.StringVar(&image, "image", "", "container image (docker backend)")
	fs.StringVar(&agentBin, "bin", "", "agent executable (unused unless a task's oracle runs a command)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: aeval audit [flags] [dataset]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Runs the dataset without an agent and reports, per task:")
		fmt.Fprintln(os.Stderr, "  solvability  the declared oracle passes")
		fmt.Fprintln(os.Stderr, "  necessity    the untouched workspace fails")
		fmt.Fprintln(os.Stderr, "  stability    repeated oracle runs agree, grader by grader")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	given := givenFlags(fs)
	if fs.NArg() > 0 {
		c.datasetPath = fs.Arg(0)
	}

	onlyList, err := parseOnly(only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aeval: %v\n", err)
		return exitUsage
	}

	cfg, err := resolveWithDataset(&c)
	if err != nil {
		return fail("%v", err)
	}
	ds, err := dataset.Load(cfg.DatasetPath)
	if err != nil {
		return fail("load dataset: %v", err)
	}

	run := cfg.Engine
	// An audit never starts the agent, but the sandbox still has to be built the
	// way a real run builds it: a solvability verdict from a different
	// environment than the one being evaluated would be about a different task.
	run.ToolVersion = version
	if given["backend"] {
		run.Backend.Kind = engine.BackendKind(backend)
	}
	if given["image"] {
		run.Backend.Image = image
	}
	if given["bin"] {
		run.Agent.Bin = absFlagPath(agentBin)
	}

	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()
	run.Store = store

	ctx, stop := signalContext()
	defer stop()
	if c.verbose {
		run.OnEvent = progressLogger()
	}

	fmt.Fprintf(os.Stderr, "%s\n", ds.Summary())
	if len(onlyList) == 0 {
		fmt.Fprintf(os.Stderr, "auditing %d task(s), %d oracle run(s) each, no agent and no API spend\n",
			len(ds.Tasks), repeats)
	} else {
		fmt.Fprintf(os.Stderr, "auditing %d task(s), checks: %s\n", len(ds.Tasks), strings.Join(onlyList, ", "))
	}

	started := time.Now()
	rep, auditErr := audit.Run(ctx, run, ds, audit.Options{
		Group:          group,
		Repeats:        repeats,
		Concurrency:    workers,
		KeepWorkspaces: keep,
		Only:           onlyList,
	})
	if rep == nil {
		return fail("audit: %v", auditErr)
	}
	if asJSON {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fail("encode report: %v", err)
		}
		fmt.Println(string(raw))
	} else {
		fmt.Fprint(os.Stdout, rep.Summary())
		printUnhealthy(rep)
	}
	fmt.Fprintf(os.Stderr, "elapsed %s\n", time.Since(started).Round(time.Millisecond))

	if auditErr != nil {
		// A partial audit is still printed: the runs that did happen are real
		// evidence, and throwing them away would waste the sandbox time already
		// spent. The non-zero exit says the picture is incomplete.
		fmt.Fprintf(os.Stderr, "aeval: %v\n", auditErr)
		return exitError
	}
	if strict && rep.Healthy() < len(rep.Tasks) {
		fmt.Fprintf(os.Stderr, "aeval: %d task(s) are not healthy\n", len(rep.Tasks)-rep.Healthy())
		return exitError
	}
	return exitOK
}

// printUnhealthy lists the defective tasks, one line each.
//
// Only the unhealthy ones: a healthy task has nothing to say, and printing the
// whole dataset would bury the three lines that matter.
func printUnhealthy(rep *audit.Report) {
	bad := rep.Unhealthy()
	if len(bad) == 0 {
		fmt.Printf("\nall %d task(s) healthy\n", len(rep.Tasks))
		return
	}
	fmt.Printf("\n%d of %d task(s) unhealthy:\n", len(bad), len(rep.Tasks))
	for _, t := range bad {
		fmt.Printf("\n  %s\n", t.TaskID)
		for _, ch := range t.Checks {
			if ch.Status == audit.StatusOK {
				continue
			}
			fmt.Printf("    %-12s %-8s %s\n", ch.Name, ch.Status, ch.Detail)
		}
	}
	if len(rep.Runs) > 0 {
		fmt.Printf("\n  evidence:\n")
		for _, r := range rep.Runs {
			fmt.Printf("    %-12s %s\n", r.Role, r.RunID)
		}
	}
}

// parseOnly validates the --only list.
//
// It validates rather than silently ignoring an unknown name: a typo would
// otherwise produce an audit that ran nothing and reported every check as
// unknown, which reads like a broken dataset instead of a broken command.
func parseOnly(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !slices.Contains(audit.AllChecks, name) {
			return nil, fmt.Errorf("unknown check %q (available: %s)",
				name, strings.Join(audit.AllChecks, ", "))
		}
		out = append(out, name)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var c commonFlags
	addCommon(fs, &c)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: aeval validate [flags]"); fs.PrintDefaults() }
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		c.datasetPath = fs.Arg(0)
	}

	cfg, err := resolve(&c)
	if err != nil {
		return fail("%v", err)
	}
	problems := 0

	if cfg.Source != "" {
		fmt.Printf("config  %s\n", cfg.Source)
	} else {
		fmt.Println("config  (built-in defaults)")
	}
	fmt.Printf("db      %s\n", cfg.DatabasePath)

	if err := cfg.Validate(); err != nil {
		fmt.Printf("        ✗ %v\n", err)
		problems++
	} else {
		fmt.Println("        ✓ configuration is usable")
	}

	if cfg.DatasetPath == "" {
		fmt.Println("dataset (none configured)")
	} else {
		ds, err := dataset.Load(cfg.DatasetPath)
		if err != nil {
			fmt.Printf("dataset %s\n        ✗ %v\n", cfg.DatasetPath, err)
			problems++
		} else {
			fmt.Printf("dataset %s\n", ds.Dir)
			fmt.Printf("        ✓ %s\n", ds.Summary())
			fmt.Printf("        hash %s\n", ds.Hash)
			if err := checkGraderTypes(ds); err != nil {
				fmt.Printf("        ✗ %v\n", err)
				problems++
			}
			if err := checkMetricNames(cfg); err != nil {
				fmt.Printf("        ✗ %v\n", err)
				problems++
			}
		}
	}

	if problems > 0 {
		return exitError
	}
	fmt.Println("\nok")
	return exitOK
}

// checkGraderTypes resolves every grader a dataset names, without a sandbox.
//
// The engine does the same check at run time, but doing it here turns a typo
// into a one-second answer instead of a failure discovered after three hours of
// paid API calls.
func checkGraderTypes(ds *dataset.Dataset) error {
	seen := map[string]bool{}
	var unknown []string
	for _, t := range ds.Tasks {
		for _, g := range t.Graders {
			if seen[g.Type] {
				continue
			}
			seen[g.Type] = true
			if _, err := grader.Get(g.Type); err != nil {
				unknown = append(unknown, fmt.Sprintf("task %s: %v", t.ID, err))
			}
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown grader type(s): %s", strings.Join(unknown, "; "))
	}
	return nil
}

func checkMetricNames(cfg *config.Config) error {
	names := cfg.Engine.Metrics
	if len(names) == 0 {
		names = metric.DefaultRunMetrics()
	}
	for _, n := range names {
		if _, err := metric.Get(n); err != nil {
			return err
		}
	}
	for _, cm := range cfg.Engine.CustomMetrics {
		if err := cm.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var c commonFlags
	var limit int
	var asJSON bool
	addCommon(fs, &c)
	fs.IntVar(&limit, "limit", 50, "maximum number of runs to show")
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: aeval list [flags]"); fs.PrintDefaults() }
	_ = fs.Parse(args)

	cfg, err := resolve(&c)
	if err != nil {
		return fail("%v", err)
	}
	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()

	idx, err := report.LoadIndex(context.Background(), store, limit, cfg.Report.Options())
	if err != nil {
		return fail("%v", err)
	}
	if asJSON {
		raw, err := report.DataJSON(idx)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println(string(raw))
		return exitOK
	}
	if len(idx.Runs) == 0 {
		fmt.Printf("no runs recorded in %s\n", cfg.DatabasePath)
		return exitOK
	}

	// The id column is printed in full even though it costs horizontal space.
	// A truncated uuid cannot be pasted into `aeval report --run`, so the column
	// would look informative and be useless. `aeval list --json` is there for
	// scripts that want structure.
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tDATASET\tAGENT\tMODEL\tRESOLVED\tCOST\tDURATION\tSTARTED")
	for _, r := range idx.Runs {
		rate := "-"
		if r.TaskCount > 0 {
			rate = fmt.Sprintf("%d/%d (%.0f%%)", r.PassedCount, r.TaskCount,
				float64(r.PassedCount)/float64(r.TaskCount)*100)
		}
		cost := "-"
		if r.TotalCostUSD > 0 {
			cost = fmt.Sprintf("$%.4f", r.TotalCostUSD)
		}
		// An oracle or baseline run is not an agent run. Labelling its status
		// rather than its agent column keeps "claude_code" from being read as
		// "the model scored 100%", which is the one conclusion that must not be
		// drawn from an audit run.
		status := r.Status
		if r.Mode != "" && r.Mode != "agent" {
			status = fmt.Sprintf("%s (%s)", r.Status, r.Mode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s@%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, status, r.DatasetName, r.DatasetVersion,
			r.AgentKind, orDefault(r.AgentModel, "-"), rate, cost,
			(time.Duration(r.DurationMs) * time.Millisecond).Round(time.Millisecond),
			r.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	tw.Flush()
	fmt.Printf("\n%d run(s) in %s\n", len(idx.Runs), cfg.DatabasePath)
	return exitOK
}

// ---------------------------------------------------------------------------
// report / compare
// ---------------------------------------------------------------------------

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var c commonFlags
	var (
		runID string
		out   string
		title string
	)
	addCommon(fs, &c)
	fs.StringVar(&runID, "run", "", "run id to report on (default: the most recent run)")
	fs.StringVar(&out, "out", "", "output path (default: report-<run>.html)")
	fs.StringVar(&title, "title", "", "document title")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: aeval report [flags]"); fs.PrintDefaults() }
	_ = fs.Parse(args)
	if runID == "" && fs.NArg() > 0 {
		runID = fs.Arg(0)
	}

	cfg, err := resolve(&c)
	if err != nil {
		return fail("%v", err)
	}
	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if runID == "" {
		idx, err := report.LoadIndex(ctx, store, 1, cfg.Report.Options())
		if err != nil {
			return fail("%v", err)
		}
		if len(idx.Runs) == 0 {
			return fail("no runs recorded in %s", cfg.DatabasePath)
		}
		runID = idx.Runs[0].ID
		fmt.Fprintf(os.Stderr, "using the most recent run: %s\n", runID)
	}

	opts := cfg.Report.Options()
	if title != "" {
		opts.Title = title
	}
	data, err := report.Load(ctx, store, runID, opts)
	if err != nil {
		return fail("%v", err)
	}
	if out == "" {
		out = report.FileName(runID)
	}
	if err := report.RenderRunFile(out, data, report.RenderOptions{Title: data.Title}); err != nil {
		return fail("%v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		return fail("stat report: %v", err)
	}
	fmt.Printf("%s\n", out)
	fmt.Printf("  %d cell(s), %d resolved, %s\n", data.Counts.Samples, data.Counts.Passed, humanBytes(int(info.Size())))
	for _, w := range data.Warnings {
		fmt.Fprintf(os.Stderr, "  note: %s\n", w)
	}
	return exitOK
}

func cmdCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	var c commonFlags
	var out string
	addCommon(fs, &c)
	fs.StringVar(&out, "out", "compare.html", "output path")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: aeval compare [flags] <left-run-id> <right-run-id>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		return exitUsage
	}
	left, right := fs.Arg(0), fs.Arg(1)

	cfg, err := resolve(&c)
	if err != nil {
		return fail("%v", err)
	}
	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()

	data, err := report.LoadComparison(context.Background(), store, left, right, cfg.Report.Options())
	if err != nil {
		return fail("%v", err)
	}
	if err := report.RenderComparisonFile(out, data, report.RenderOptions{Title: data.Title}); err != nil {
		return fail("%v", err)
	}
	s := data.Compare.Summary
	fmt.Printf("%s\n", out)
	fmt.Printf("  %s → %s\n", data.Compare.LeftLabel, data.Compare.RightLabel)
	fmt.Printf("  %d common cell(s): %d fixed, %d regressed, %d still passing, %d still failing\n",
		s.Common, s.Fixed, s.Regressed, s.BothPass, s.BothFail)
	for _, w := range data.Warnings {
		fmt.Fprintf(os.Stderr, "  note: %s\n", w)
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c commonFlags
	var (
		addr     string
		allowDel bool
		refresh  int
		rl       bool
		rlExec   bool
		rlEnvs   int
		rlTTL    time.Duration
		rlRolls  int
		rlExecs  int
		rlCost   float64
	)
	addCommon(fs, &c)
	fs.StringVar(&addr, "addr", "", "listen address (default 127.0.0.1:8787)")
	fs.BoolVar(&allowDel, "allow-delete", false, "enable the run deletion route")
	fs.IntVar(&refresh, "refresh", 0, "reload the index every N seconds (0 disables)")
	fs.BoolVar(&rl, "rl", false, "also serve the dataset as RL environments under /api/rl")
	fs.BoolVar(&rlExec, "rl-allow-exec", false, "let /api/rl run commands and write files inside environments (arbitrary code execution)")
	fs.IntVar(&rlEnvs, "rl-max-envs", 8, "maximum environments open at once")
	fs.DurationVar(&rlTTL, "rl-ttl", 15*time.Minute, "how long an environment may stay open")
	fs.IntVar(&rlRolls, "rl-max-rollouts", 0, "maximum rollouts this process will perform (0 = unlimited)")
	fs.IntVar(&rlExecs, "rl-max-execs", 200, "maximum commands per environment")
	fs.Float64Var(&rlCost, "rl-max-cost-usd", 0, "cumulative agent cost cap in USD (0 = unlimited)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: aeval serve [flags]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Serves recorded runs. With --rl (or serve.rl.enabled in the config)")
		fmt.Fprintln(os.Stderr, "it also serves --dataset as a set of environments for a training")
		fmt.Fprintln(os.Stderr, "loop; see docs/rl-environment.md.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	cfg, err := resolve(&c)
	if err != nil {
		return fail("%v", err)
	}
	if addr != "" {
		cfg.Serve.Addr = addr
	}
	if allowDel {
		cfg.Serve.AllowDelete = true
	}
	if refresh > 0 {
		cfg.Serve.LiveRefreshSeconds = refresh
	}
	// The flags and the config file are merged field by field, not section by
	// section: a config file that sets only allow_exec and a command line that
	// only sets --rl-max-envs has to mean both. A flag that was actually typed
	// wins, which is why this asks which flags were given rather than comparing
	// values -- `--rl-max-envs 8` is indistinguishable from the default 8.
	given := givenFlags(fs)
	if given["rl"] {
		cfg.Serve.RL.Enabled = rl
	}
	if given["rl-allow-exec"] {
		cfg.Serve.RL.AllowExec = rlExec
	}
	if given["rl-max-envs"] {
		cfg.Serve.RL.MaxEnvs = rlEnvs
	}
	if given["rl-ttl"] {
		cfg.Serve.RL.TTL = rlTTL
	}
	if given["rl-max-rollouts"] {
		cfg.Serve.RL.MaxRollouts = rlRolls
	}
	if given["rl-max-execs"] {
		cfg.Serve.RL.MaxExecs = rlExecs
	}
	if given["rl-max-cost-usd"] {
		cfg.Serve.RL.MaxCostUSD = rlCost
	}
	if err := cfg.Validate(); err != nil {
		return fail("%v", err)
	}

	store, err := openStore(cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()

	ctx, stop := signalContext()
	defer stop()

	var mgr *rlenv.Manager
	if rlc := cfg.Serve.RL; rlc.Enabled {
		// The environment service is the one part of the tool that can act, so it
		// refuses to start on guesses: without a dataset there is nothing to
		// serve as an environment, and silently serving none looks like a
		// client-side bug.
		dsPath, err := resolveWithDataset(&c)
		if err != nil {
			return fail("--rl: %v", err)
		}
		ds, err := dataset.Load(dsPath.DatasetPath)
		if err != nil {
			return fail("--rl: load dataset: %v", err)
		}
		run := cfg.Engine
		run.Mode = engine.RunModeAgent
		run.Store = store
		run.ToolVersion = version
		mgr, err = rlenv.New(run, ds, rlenv.Options{
			MaxEnvs:     rlc.MaxEnvs,
			TTL:         rlc.TTL,
			MaxExecs:    rlc.MaxExecs,
			AllowExec:   rlc.AllowExec,
			MaxRollouts: rlc.MaxRollouts,
			MaxCostUSD:  rlc.MaxCostUSD,
		})
		if err != nil {
			return fail("--rl: %v", err)
		}
		defer mgr.CloseAll(context.WithoutCancel(ctx))
	}

	srv, err := web.New(web.Config{
		Store:              store,
		Addr:               cfg.Serve.Addr,
		Title:              cfg.Report.Title,
		Report:             cfg.Report.Options(),
		AllowDelete:        cfg.Serve.AllowDelete,
		LiveRefreshSeconds: cfg.Serve.LiveRefreshSeconds,
		RL:                 mgr,
	})
	if err != nil {
		return fail("%v", err)
	}

	fmt.Printf("serving %s on http://%s\n", cfg.DatabasePath, srv.Addr())
	if cfg.Serve.AllowDelete {
		fmt.Println("  deletion enabled: the API can remove runs")
	}
	if mgr != nil {
		st := mgr.Stats()
		fmt.Printf("  rl enabled: dataset %s, %d task(s)\n", mgr.DatasetName(), len(mgr.Tasks()))
		fmt.Printf("    open at most %d env(s) for %s, %d command(s) each\n",
			st.MaxEnvs, time.Duration(st.TTLSeconds*float64(time.Second)), st.MaxExecs)
		if st.MaxRollouts > 0 {
			fmt.Printf("    rollout cap %d\n", st.MaxRollouts)
		}
		if st.MaxCostUSD > 0 {
			fmt.Printf("    cost cap $%.4f\n", st.MaxCostUSD)
		}
		if st.ExecEnabled {
			fmt.Println("    command execution ENABLED: /api/rl can run commands as this user")
		} else {
			fmt.Println("    command execution disabled (--rl-allow-exec to enable)")
		}
	}
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fail("%v", err)
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// registry / version
// ---------------------------------------------------------------------------

func cmdRegistry(args []string) int {
	fs := flag.NewFlagSet("registry", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: aeval registry") }
	_ = fs.Parse(args)

	fmt.Println("agents")
	for _, kind := range agent.Registered() {
		fmt.Printf("  %s\n", kind)
	}

	fmt.Println("\ngraders")
	for _, t := range grader.Registered() {
		fmt.Printf("  %s\n", t)
	}

	fmt.Println("\nmetrics (run level)")
	for _, n := range metric.NamesOfKind(metric.KindRun) {
		m, err := metric.Get(n)
		if err != nil {
			continue
		}
		fmt.Printf("  %-26s %s\n", n, m.Description())
	}

	fmt.Println("\ndefault metric set")
	fmt.Printf("  %s\n", strings.Join(metric.DefaultRunMetrics(), ", "))
	return exitOK
}

func cmdVersion() int {
	fmt.Printf("aeval %s\n", version)
	fmt.Printf("  go        %s\n", runtime.Version())
	fmt.Printf("  platform  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  schema    sqlite v%d, report payload v%d\n", storage.SchemaVersion(), report.DataVersion)
	fmt.Printf("  agents    %s\n", strings.Join(agent.Registered(), ", "))
	fmt.Printf("  graders   %s\n", strings.Join(grader.Registered(), ", "))
	fmt.Printf("  metrics   %d registered\n", len(metric.Names()))
	return exitOK
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

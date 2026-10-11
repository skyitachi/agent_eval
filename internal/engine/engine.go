// Package engine orchestrates an evaluation run: it expands the dataset into a
// task x sample matrix, executes each cell in its own sandbox, grades the
// result, persists it, and finally computes the run-level metrics.
//
// # Failure isolation
//
// One broken task must not cost the whole run. Every cell runs behind a recover
// barrier and its own sandbox, a failure is recorded as a result row and the
// remaining cells continue. Only a cancelled context stops the run early, and
// even then the results collected so far are graded, persisted and reported.
//
// # Why results are persisted per task
//
// Waiting until the end to write anything means a crash or a Ctrl-C at hour
// three loses three hours of paid API calls. Each cell is written the moment it
// finishes, which also makes `resume: true` a real feature rather than a promise:
// the store's completed keys tell the second invocation exactly what is left.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/metric"
	"github.com/skyitachi/agenteval/internal/reward"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// RunMode selects what produces the grade for each cell.
//
// Only RunModeAgent spends money, because only it starts the agent. The other
// two exist so a task's own health can be established without a model in the
// loop, which is the difference between "the agent scored 40%" and "the task is
// worth 40%".
type RunMode string

const (
	// RunModeAgent runs the agent against the task. The default.
	RunModeAgent RunMode = "agent"
	// RunModeOracle applies the task's reference solution instead of running the
	// agent, then grades. A task that fails here cannot be solved at all, so its
	// resolve rate says nothing about the agent.
	RunModeOracle RunMode = "oracle"
	// RunModeBaseline grades the untouched workspace. A task that passes here is
	// already solved before anyone starts, which makes it free reward.
	RunModeBaseline RunMode = "baseline"
)

// Config describes one evaluation run.
type Config struct {
	// Mode selects what the cells measure. Empty means RunModeAgent.
	Mode RunMode `json:"mode,omitempty" yaml:"mode,omitempty"`

	// Agent is the base agent specification. Per-task Limits and SystemPrompt
	// are layered on top of it for each task.
	Agent agent.Spec `json:"agent" yaml:"agent"`

	// Backend selects and configures the sandbox.
	Backend BackendConfig `json:"backend" yaml:"backend"`

	// SamplesPerTask is k for pass@k. Values above 1 make every task run k
	// times, each in a fresh sandbox.
	SamplesPerTask int `json:"samples_per_task,omitempty" yaml:"samples_per_task,omitempty"`
	// Concurrency is the number of cells executed at once. It is the main
	// speed lever and the main source of provider rate limits.
	Concurrency int `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	// Seed identifies the sampling configuration. It is recorded on the run row
	// for provenance; the agent CLI exposes no seed parameter, so it does not
	// change the model's behaviour.
	Seed int64 `json:"seed,omitempty" yaml:"seed,omitempty"`

	// AgentTimeout is the fallback per-attempt timeout for tasks that do not set
	// limits.timeout_s.
	AgentTimeout time.Duration `json:"agent_timeout,omitempty" yaml:"agent_timeout,omitempty"`
	// TaskTimeout bounds one cell end to end (sandbox start, setup, agent,
	// grading). Zero disables the outer deadline.
	TaskTimeout time.Duration `json:"task_timeout,omitempty" yaml:"task_timeout,omitempty"`
	// GraderThreshold is the score a task must reach to count as passed.
	// Defaults to grader.Threshold (1.0), which matches the SWE-bench notion of
	// "resolved": every check must pass.
	GraderThreshold float64 `json:"grader_threshold,omitempty" yaml:"grader_threshold,omitempty"`
	// Retries is the number of extra attempts when the agent process could not
	// be started at all. It defaults to 0, so a run makes exactly the calls it
	// was asked to make and a startup problem surfaces immediately rather than
	// being papered over. Agent-level outcomes (timeout, non-zero exit, error
	// result) are never retried: they are measurements, not faults.
	Retries int `json:"retries,omitempty" yaml:"retries,omitempty"`
	// RetryBackoff is the pause between infrastructure retries.
	RetryBackoff time.Duration `json:"retry_backoff,omitempty" yaml:"retry_backoff,omitempty"`

	// Metrics are built-in run-level metric names. Empty means
	// metric.DefaultRunMetrics().
	Metrics []string `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	// CustomMetrics are evaluated after the built-ins, in order.
	CustomMetrics []metric.CustomSpec `json:"custom_metrics,omitempty" yaml:"custom_metrics,omitempty"`

	// Store persists results. Nil runs the evaluation without persistence, which
	// is how the engine is unit-tested and how `--dry-run` works.
	Store *storage.Store `json:"-" yaml:"-"`

	// RunID is the identifier to use. Empty generates one.
	RunID string `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	// RunName is a human label for listings.
	RunName string `json:"name,omitempty" yaml:"name,omitempty"`
	// Notes is free-form provenance stored on the run row.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
	// ToolVersion identifies the harness build that produced the run.
	ToolVersion string `json:"tool_version,omitempty" yaml:"tool_version,omitempty"`

	// Resume skips cells already recorded as completed in the store, so an
	// interrupted run can be continued without paying for finished work.
	Resume bool `json:"resume,omitempty" yaml:"resume,omitempty"`

	// FailFast cancels the remaining work as soon as one cell fails hard.
	FailFast bool `json:"fail_fast,omitempty" yaml:"fail_fast,omitempty"`

	// WorkRoot hosts per-task workspaces and durable artifacts. Empty uses a
	// temporary directory.
	WorkRoot string `json:"work_root,omitempty" yaml:"work_root,omitempty"`
	// KeepWorkspaces leaves each task's workspace on disk for inspection.
	// Without it only the artifact bundle survives.
	KeepWorkspaces bool `json:"keep_workspaces,omitempty" yaml:"keep_workspaces,omitempty"`

	// OnEvent, when set, receives progress notifications. It is called from
	// worker goroutines, so implementations must be safe for concurrent use.
	OnEvent func(Event) `json:"-" yaml:"-"`
}

// withDefaults returns a copy with every unspecified field resolved.
func (c Config) withDefaults() (Config, error) {
	backend, err := c.Backend.Normalize()
	if err != nil {
		return c, err
	}
	c.Backend = backend

	if c.Mode == "" {
		c.Mode = RunModeAgent
	}
	switch c.Mode {
	case RunModeAgent, RunModeOracle, RunModeBaseline:
	default:
		return c, fmt.Errorf("unknown mode %q (available: %s, %s, %s)",
			c.Mode, RunModeAgent, RunModeOracle, RunModeBaseline)
	}

	if c.Agent.Kind == "" {
		c.Agent.Kind = "claude_code"
	}
	if c.SamplesPerTask <= 0 {
		c.SamplesPerTask = 1
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.GraderThreshold <= 0 {
		c.GraderThreshold = grader.Threshold
	}
	if len(c.Metrics) == 0 {
		c.Metrics = metric.DefaultRunMetrics()
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 2 * time.Second
	}
	if c.RunID == "" {
		c.RunID = agent.NewUUID()
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// EventKind enumerates the progress notifications the engine emits.
type EventKind string

const (
	EventRunStart  EventKind = "run_start"
	EventTaskStart EventKind = "task_start"
	EventTaskRetry EventKind = "task_retry"
	EventTaskDone  EventKind = "task_done"
	EventRunDone   EventKind = "run_done"
)

// Event is a progress notification.
type Event struct {
	Kind        EventKind
	Time        time.Time
	RunID       string
	TaskID      string
	SampleIndex int
	// Total and Completed describe progress across the whole run.
	Total     int
	Completed int
	// Result is set for EventTaskDone.
	Result *TaskResult
	// Message carries a human-readable note, e.g. why a retry happened.
	Message string
}

func (e *Engine) emit(ev Event) {
	if e.cfg.OnEvent == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	ev.RunID = e.cfg.RunID
	e.cfg.OnEvent(ev)
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

// TaskResult is one graded attempt, and the unit the report layer renders.
//
// It intentionally carries the attempt and grade objects rather than only the
// derived numbers: viewing a trajectory, or re-rendering a report, needs the
// full event stream.
type TaskResult struct {
	TaskID      string   `json:"task_id"`
	Category    string   `json:"category,omitempty"`
	Difficulty  string   `json:"difficulty,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	SampleIndex int      `json:"sample_index"`

	// Status is completed | failed | skipped.
	Status string `json:"status"`
	// Mode is the run mode that produced this cell: agent, oracle or baseline.
	Mode string `json:"mode,omitempty"`
	// StopReason explains how the attempt ended; see agent.Attempt.StopReason.
	StopReason string `json:"stop_reason,omitempty"`

	Attempt *agent.Attempt `json:"-"`
	Grade   *grader.Grade  `json:"-"`

	Diff         string   `json:"-"`
	ChangedPaths []string `json:"-"`
	Answer       string   `json:"-"`

	// Reward is the scalar training signal, derived from the grade through the
	// task's declared reward contract.
	Reward float64 `json:"reward"`
	// RewardKind names the density that produced it (score|pass).
	RewardKind string `json:"reward_kind,omitempty"`
	// Violations lists anti-gaming guards that fired. A non-empty list means the
	// attempt was rejected: it is recorded as failed and its reward is zero.
	Violations []string `json:"violations,omitempty"`
	// Restored lists paths reset to their baseline state before grading.
	Restored []string `json:"restored,omitempty"`

	WorkspaceDir  string `json:"workspace_dir,omitempty"`
	ArtifactDir   string `json:"artifact_dir,omitempty"`
	RawStreamPath string `json:"raw_stream_path,omitempty"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	// Attempts counts how many times the agent process was started, including
	// infrastructure retries.
	Attempts int `json:"attempts,omitempty"`
	// Error describes a failure of this cell, whether infrastructure
	// (the agent never started) or a lost grade.
	Error string `json:"error,omitempty"`
	// err keeps the original error for in-process callers; it is not serialised.
	err error
}

// Passed reports whether the attempt satisfied every required grader.
func (r *TaskResult) Passed() bool { return r.Grade != nil && r.Grade.Passed }

// Score is the weighted grader score, zero when grading never happened.
func (r *TaskResult) Score() float64 {
	if r.Grade == nil {
		return 0
	}
	return r.Grade.Score
}

// Duration is the wall-clock time of the whole cell, including sandbox setup
// and grading.
func (r *TaskResult) Duration() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// CostUSD is the provider-reported cost, zero when no attempt was made.
func (r *TaskResult) CostUSD() float64 {
	if r.Attempt == nil {
		return 0
	}
	return r.Attempt.CostUSD()
}

// Unwrap exposes the underlying failure so callers can use errors.Is/As.
//
// It is deliberately not named Error(): TaskResult.Error is the serialised
// message, and a field and a method cannot share a name.
func (r *TaskResult) Unwrap() error { return r.err }

// Result is the outcome of a whole run.
type Result struct {
	RunID     string        `json:"run_id"`
	RunName   string        `json:"name,omitempty"`
	Mode      RunMode       `json:"mode,omitempty"`
	Status    string        `json:"status"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Duration  time.Duration `json:"duration"`

	DatasetName    string `json:"dataset_name"`
	DatasetVersion string `json:"dataset_version,omitempty"`
	DatasetHash    string `json:"dataset_hash"`
	DatasetPath    string `json:"dataset_path,omitempty"`

	AgentKind      string `json:"agent_kind"`
	AgentModel     string `json:"agent_model,omitempty"`
	SandboxBackend string `json:"sandbox_backend"`

	SamplesPerTask int   `json:"samples_per_task"`
	Concurrency    int   `json:"concurrency"`
	Seed           int64 `json:"seed,omitempty"`

	// Tasks holds one entry per executed cell, in deterministic order.
	Tasks []*TaskResult `json:"tasks"`
	// Skipped lists cells that resume mode found already completed.
	Skipped []string `json:"skipped,omitempty"`

	Metrics metric.Map `json:"metrics,omitempty"`

	// Error is set when the run aborted before finishing.
	Error string `json:"error,omitempty"`
}

// Total returns the number of cells in the plan.
func (r *Result) Total() int { return len(r.Tasks) + len(r.Skipped) }

// Passed counts passing attempts.
func (r *Result) Passed() int {
	n := 0
	for _, t := range r.Tasks {
		if t.Passed() {
			n++
		}
	}
	return n
}

// ResolveRate is the fraction of executed cells that passed.
func (r *Result) ResolveRate() float64 {
	if len(r.Tasks) == 0 {
		return 0
	}
	return float64(r.Passed()) / float64(len(r.Tasks))
}

// TotalCostUSD sums the reported cost across attempts.
func (r *Result) TotalCostUSD() float64 {
	var sum float64
	for _, t := range r.Tasks {
		sum += t.CostUSD()
	}
	return sum
}

// Failures returns the cells that did not pass, newest plan order.
func (r *Result) Failures() []*TaskResult {
	var out []*TaskResult
	for _, t := range r.Tasks {
		if !t.Passed() {
			out = append(out, t)
		}
	}
	return out
}

// Summary renders a one-paragraph digest for the console.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s [%s]\n", r.RunID, r.Status)
	fmt.Fprintf(&b, "  dataset   %s@%s (%s)\n", r.DatasetName, r.DatasetVersion, shortHash(r.DatasetHash))
	fmt.Fprintf(&b, "  agent     %s model=%s backend=%s\n", r.AgentKind, orNone(r.AgentModel), r.SandboxBackend)
	if r.Mode != "" && r.Mode != RunModeAgent {
		// Without this line an oracle run -- which passes by construction -- is
		// indistinguishable from a very good agent run in the console output.
		fmt.Fprintf(&b, "  mode      %s (no agent executed; not a model score)\n", r.Mode)
	}
	fmt.Fprintf(&b, "  cells     %d executed", len(r.Tasks))
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, ", %d skipped (resume)", len(r.Skipped))
	}
	fmt.Fprintf(&b, "  k=%d concurrency=%d\n", r.SamplesPerTask, r.Concurrency)
	fmt.Fprintf(&b, "  resolved  %d/%d (%.1f%%)\n", r.Passed(), len(r.Tasks), r.ResolveRate()*100)
	fmt.Fprintf(&b, "  cost      $%.4f\n", r.TotalCostUSD())
	fmt.Fprintf(&b, "  duration  %s\n", r.Duration.Round(time.Millisecond))
	if r.Error != "" {
		fmt.Fprintf(&b, "  error     %s\n", r.Error)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Engine executes evaluation runs. It is stateless between runs: create one per
// configuration and call Run as often as needed.
type Engine struct {
	cfg Config

	mu sync.Mutex
	// progress counters, guarded by mu.
	completed int
	total     int
}

// New validates cfg and returns an Engine.
func New(cfg Config) (*Engine, error) {
	resolved, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	// Resolve the agent runner up front so an unknown kind fails before any
	// sandbox is created or any money is spent.
	if _, err := agent.New(resolved.Agent.Kind); err != nil {
		return nil, err
	}
	if _, err := metric.Get("resolve_rate"); err != nil {
		return nil, fmt.Errorf("metrics registry unavailable: %w", err)
	}
	// Validate what can be validated before any work happens: finding a typo'd
	// metric or grader name after three hours of paid API calls is the single
	// most annoying way to lose a run.
	for _, name := range resolved.Metrics {
		if _, err := metric.Get(name); err != nil {
			return nil, err
		}
	}
	for _, cm := range resolved.CustomMetrics {
		if err := cm.Validate(); err != nil {
			return nil, err
		}
	}
	return &Engine{cfg: resolved}, nil
}

// Config returns the resolved configuration.
func (e *Engine) Config() Config { return e.cfg }

// Run evaluates every task in ds.
//
// It returns a Result even when parts of the run failed, because a partial
// result is still a result: the caller decides whether to keep it based on the
// per-cell errors and the run status.
func (e *Engine) Run(ctx context.Context, ds *dataset.Dataset) (*Result, error) {
	if ds == nil {
		return nil, fmt.Errorf("engine: dataset is nil")
	}
	if len(ds.Tasks) == 0 {
		return nil, fmt.Errorf("engine: dataset %q has no tasks", ds.Manifest.Name)
	}
	// Grader types can only be checked against a dataset, so this is the last
	// moment before any sandbox or API call is made.
	if err := e.validateTasks(ds); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	plan := e.plan(ds)
	plan, skipped, err := e.resumeFilter(runCtx, plan)
	if err != nil {
		return nil, err
	}

	res := &Result{
		RunID:          e.cfg.RunID,
		RunName:        e.cfg.RunName,
		Mode:           e.cfg.Mode,
		Status:         storage.StatusRunning,
		StartedAt:      time.Now().UTC(),
		DatasetName:    ds.Manifest.Name,
		DatasetVersion: ds.Manifest.Version,
		DatasetHash:    ds.Hash,
		DatasetPath:    ds.Dir,
		AgentKind:      e.cfg.Agent.Kind,
		AgentModel:     e.cfg.Agent.Model,
		SandboxBackend: string(e.cfg.Backend.Kind),
		SamplesPerTask: e.cfg.SamplesPerTask,
		Concurrency:    e.cfg.Concurrency,
		Seed:           e.cfg.Seed,
	}

	if err := e.createRunRow(runCtx, ds, len(plan)); err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.total = len(plan)
	e.completed = 0
	e.mu.Unlock()

	e.emit(Event{Kind: EventRunStart, Total: len(plan)})

	fatal := e.execute(runCtx, cancel, ds, plan, res)

	res.EndedAt = time.Now().UTC()
	res.Duration = res.EndedAt.Sub(res.StartedAt)
	res.Skipped = skipped

	// Compute metrics over whatever finished, even on a cancelled or failed run:
	// a run that stops early still has meaningful partial numbers, and marking
	// them "unknown" would be less honest than reporting them with a status.
	if err := e.computeMetrics(res); err != nil {
		// A metrics failure becomes the run's fatal error when nothing else
		// already failed. It is appended here only when it is *not* the fatal
		// one, because the switch below appends fatal too -- appending both
		// would print the same message twice in the run summary.
		if fatal == nil {
			fatal = err
		} else {
			res.Error = appendErr(res.Error, err)
		}
	}

	switch {
	case fatal != nil:
		res.Status = storage.StatusFailed
		res.Error = appendErr(res.Error, fatal)
	case ctx.Err() != nil:
		res.Status = storage.StatusCanceled
	default:
		res.Status = storage.StatusCompleted
	}

	if err := e.finishRun(runCtx, res); err != nil && fatal == nil {
		fatal = err
		res.Error = appendErr(res.Error, err)
		res.Status = storage.StatusFailed
	}

	e.emit(Event{Kind: EventRunDone, Total: len(plan), Completed: len(res.Tasks)})
	return res, fatal
}

// validateTasks resolves everything a cell will need, before any cell runs.
//
// A dataset that names an unregistered grader, or declares a reward density this
// build does not know, would otherwise fail once per task and the resulting
// error would sit in the report looking like an agent failure rather than a
// configuration mistake. Resolving it here also means the per-cell path can
// treat a resolution failure as unreachable.
func (e *Engine) validateTasks(ds *dataset.Dataset) error {
	seen := map[string]bool{}
	var missing []string
	for _, t := range ds.Tasks {
		if len(t.Graders) == 0 {
			return fmt.Errorf("task %s: no graders configured", t.ID)
		}
		for _, g := range t.Graders {
			if seen[g.Type] {
				continue
			}
			seen[g.Type] = true
			if _, err := grader.Get(g.Type); err != nil {
				return fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
		if _, err := reward.Resolve(t.Reward); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if e.cfg.Mode == RunModeOracle && t.Oracle == nil {
			missing = append(missing, t.ID)
		}
	}
	// An oracle run is a statement about the whole dataset, so it is refused
	// rather than quietly restricted to the tasks that can support it: a number
	// computed over a subset still reads as the dataset's score. Every offender
	// is named at once, because fixing them one run at a time is a waste of a
	// round trip.
	if len(missing) > 0 {
		return fmt.Errorf("mode %s needs an oracle on every task; %d declare none: %s "+
			"(add an oracle, narrow the dataset with a filter, or use `aeval audit`, which reports them as unknown)",
			RunModeOracle, len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

// cell is one task x sample pair.
type cell struct {
	Task        dataset.TaskSpec
	SampleIndex int
	// Key is the resume key, also used for directory names.
	Key string
}

// plan expands the dataset into cells in a deterministic order.
//
// Tasks keep their dataset order and samples come in index order, so two runs
// of the same dataset produce the same plan and therefore comparable reports.
func (e *Engine) plan(ds *dataset.Dataset) []cell {
	cells := make([]cell, 0, len(ds.Tasks)*e.cfg.SamplesPerTask)
	for _, t := range ds.Tasks {
		for s := 0; s < e.cfg.SamplesPerTask; s++ {
			cells = append(cells, cell{
				Task:        t,
				SampleIndex: s,
				Key:         storage.TaskKey(t.ID, s),
			})
		}
	}
	return cells
}

// resumeFilter drops cells already completed in the store.
//
// It returns the filtered slice rather than mutating in place: a slice header is
// passed by value, so writing through the caller's backing array would not change
// the caller's length and the supposedly skipped cells would be executed twice.
func (e *Engine) resumeFilter(ctx context.Context, cells []cell) ([]cell, []string, error) {
	if !e.cfg.Resume || e.cfg.Store == nil {
		return cells, nil, nil
	}
	done, err := e.cfg.Store.CompletedKeys(ctx, e.cfg.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("resume: %w", err)
	}
	if len(done) == 0 {
		return cells, nil, nil
	}
	var skipped []string
	kept := make([]cell, 0, len(cells))
	for _, c := range cells {
		if done[c.Key] {
			skipped = append(skipped, c.Key)
			continue
		}
		kept = append(kept, c)
	}
	sort.Strings(skipped)
	return kept, skipped, nil
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// execute runs every cell with a bounded worker pool.
//
// It returns a fatal error, which is set only when a cell failed and FailFast
// asked for the run to stop.
func (e *Engine) execute(ctx context.Context, cancel context.CancelFunc,
	ds *dataset.Dataset, cells []cell, res *Result) error {

	// Results are written into pre-sized slots, so ordering never depends on the
	// scheduling of workers.
	slots := make([]*TaskResult, len(cells))
	indexes := make(chan int)

	var (
		mu     sync.Mutex
		fatal  error
		active = e.cfg.Concurrency
	)
	if active > len(cells) {
		active = len(cells)
	}

	var wg sync.WaitGroup
	for w := 0; w < active; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexes {
				c := cells[i]
				e.emit(Event{Kind: EventTaskStart, TaskID: c.Task.ID, SampleIndex: c.SampleIndex})

				tr := e.runCell(ctx, ds, c)
				slots[i] = tr

				// Persist immediately: an interrupted run must keep the work it
				// already paid for.
				if err := e.saveCell(ctx, tr); err != nil {
					tr.Error = appendErr(tr.Error, err)
					if tr.err == nil {
						tr.err = err
					}
				}

				mu.Lock()
				e.mu.Lock()
				e.completed++
				done := e.completed
				e.mu.Unlock()
				if tr.err != nil && e.cfg.FailFast && fatal == nil {
					fatal = fmt.Errorf("task %s sample %d: %w", tr.TaskID, tr.SampleIndex, tr.err)
					// Cancelling stops the remaining cells; the ones already
					// running unwind through their own contexts.
					cancel()
				}
				mu.Unlock()

				e.emit(Event{
					Kind: EventTaskDone, TaskID: c.Task.ID, SampleIndex: c.SampleIndex,
					Total: len(cells), Completed: done, Result: tr,
				})
			}
		}()
	}

feed:
	for i := range cells {
		select {
		case indexes <- i:
		case <-ctx.Done():
			// Stop handing out work, but let the workers drain so that partial
			// results are still graded and saved.
			break feed
		}
	}
	close(indexes)
	wg.Wait()

	// A cancelled context means the remaining cells never ran. Record them as
	// skipped rather than silently dropping them from the denominator.
	for i, tr := range slots {
		if tr == nil {
			slots[i] = e.skippedCell(cells[i], ctx.Err())
		}
	}
	res.Tasks = slots
	return fatal
}

// runCell executes one task sample from sandbox creation to grading.
//
// This is the only place that touches a sandbox, and it is deliberately linear:
// workspace -> baseline -> agent (with infra retries) -> grade. Anything that
// goes wrong becomes a recorded result rather than a panic.
func (e *Engine) runCell(ctx context.Context, ds *dataset.Dataset, c cell) (out *TaskResult) {
	start := time.Now().UTC()
	tr := &TaskResult{
		TaskID:      c.Task.ID,
		Category:    c.Task.Category,
		Difficulty:  c.Task.Difficulty,
		Tags:        c.Task.Tags,
		SampleIndex: c.SampleIndex,
		Status:      storage.TaskStatusCompleted,
		StartedAt:   start,
	}
	defer func() {
		if r := recover(); r != nil {
			// A panic anywhere in the cell -- the agent implementation, a grader,
			// a filesystem call -- becomes a recorded failure rather than taking
			// the whole run down with it.
			tr.Status = storage.TaskStatusFailed
			if tr.StopReason == "" {
				tr.StopReason = "harness_error"
			}
			tr.err = fmt.Errorf("panic while evaluating %s: %v", c.Task.ID, r)
			tr.Error = appendErr(tr.Error, tr.err)
			tr.EndedAt = time.Now().UTC()
			out = tr
		}
	}()

	cellCtx := ctx
	if e.cfg.TaskTimeout > 0 {
		var cancel context.CancelFunc
		cellCtx, cancel = context.WithTimeout(ctx, e.cfg.TaskTimeout)
		defer cancel()
	}

	cellDir := filepath.Join(e.workRoot(), "tasks", safeName(c.Task.ID), fmt.Sprintf("s%d", c.SampleIndex))
	artifactDir := filepath.Join(e.workRoot(), "artifacts", safeName(c.Task.ID), fmt.Sprintf("s%d", c.SampleIndex))
	tr.ArtifactDir = artifactDir
	if e.cfg.KeepWorkspaces {
		// Only advertise the workspace path when it will still exist afterwards;
		// recording a path that the engine deletes is worse than recording none.
		tr.WorkspaceDir = cellDir
	}

	// The workspace directory is reused across retries, so start clean; a stale
	// file from a previous attempt would silently corrupt the baseline diff.
	if err := os.RemoveAll(cellDir); err != nil {
		return tr.fail(fmt.Errorf("clear workspace %s: %w", cellDir, err))
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return tr.fail(fmt.Errorf("create artifact dir %s: %w", artifactDir, err))
	}

	// The workspace itself is opened through the same entry point the RL
	// environment service uses, so a cell and a hand-driven rollout start from
	// byte-identical trees.
	ws, err := e.OpenWorkspace(cellCtx, c.Task, ds.Dir, cellDir)
	if err != nil {
		return tr.fail(err)
	}
	sb, baseline := ws.Sandbox, ws.Baseline
	defer func() {
		if cerr := ws.Close(cellCtx, !e.cfg.KeepWorkspaces); cerr != nil && tr.err == nil {
			// A failure to tear down is worth reporting but does not invalidate
			// the measurement. Artifacts live outside the workspace, so removing
			// it loses nothing the report layer needs; when the workspace is kept
			// instead, a leaked directory is the thing to report.
			tr.Error = appendErr(tr.Error, cerr)
		}
	}()

	// tr.Mode is recorded on every cell, not just on the run, because a stored
	// oracle cell that failed and a stored agent cell that failed mean opposite
	// things -- "the task cannot be solved" versus "the agent did not solve it"
	// -- and a reader who mixes them up draws the wrong conclusion about both.
	tr.Mode = string(e.cfg.Mode)

	var attempt *agent.Attempt
	if e.cfg.Mode == RunModeAgent {
		runner, err := agent.New(e.cfg.Agent.Kind)
		if err != nil {
			return tr.fail(err)
		}
		spec := e.agentSpec(c.Task)
		prompt := c.Task.ResolvedPrompt
		if prompt == "" {
			prompt = c.Task.Prompt
		}

		var runErr error
		attempt, runErr = e.runAgentWithRetries(cellCtx, runner, sb, spec, prompt, tr)
		if attempt == nil {
			// The agent never ran, so there is nothing to grade. Grading an
			// untouched workspace would produce a zero that looks like a model
			// failure; harness_error_rate is where this belongs.
			tr.Status = storage.TaskStatusFailed
			tr.StopReason = "harness_error"
			// ...unless the cell was cut off by its own budget or by the caller.
			// Those are not harness bugs, and counting them as such would make
			// harness_error_rate useless for spotting real infrastructure problems.
			switch {
			case errors.Is(cellCtx.Err(), context.DeadlineExceeded):
				tr.StopReason = "task_timeout"
			case cellCtx.Err() != nil:
				tr.StopReason = "cancelled"
			}
			tr.err = runErr
			tr.Error = appendErr(tr.Error, runErr)
			tr.EndedAt = time.Now().UTC()
			return tr
		}
		tr.Attempt = attempt
		tr.StopReason = attempt.StopReason()
		tr.Answer = attempt.FinalText()
		if attempt.RawStream != "" {
			path := filepath.Join(artifactDir, "stream.jsonl")
			if werr := os.WriteFile(path, []byte(attempt.RawStream), 0o644); werr != nil {
				tr.Error = appendErr(tr.Error, fmt.Errorf("write raw stream: %w", werr))
			} else {
				tr.RawStreamPath = path
			}
		}
	} else if e.cfg.Mode == RunModeOracle {
		// No agent at all. The whole point of this mode is to establish what the
		// task does with no model in the loop and no API spend, so the reference
		// solution is applied to the same workspace the agent would have seen.
		if err := applyOracle(cellCtx, sb, c.Task); err != nil {
			// A distinct reason, because the two failures mean opposite things:
			// "oracle_error" says the dataset's own reference solution is not
			// applicable -- a defect in the task -- while "harness_error" says the
			// environment let us down. Folding them together would make an audit
			// unable to tell a broken task from a broken machine.
			tr.StopReason = "oracle_error"
			return tr.fail(fmt.Errorf("apply oracle: %w", err))
		}
		tr.StopReason = "oracle"
	} else {
		tr.StopReason = "baseline"
	}

	// Guards only make sense for an agent attempt: the oracle is allowed to touch
	// anything, and a baseline attempt touches nothing at all.
	v := e.Verify(cellCtx, c.Task, sb, baseline, attempt, e.cfg.Mode == RunModeAgent)
	tr.Grade, tr.Diff, tr.ChangedPaths = v.Grade, v.Diff, v.ChangedPaths
	tr.Violations, tr.Restored = v.Violations, v.Restored
	tr.Reward, tr.RewardKind = v.Reward, v.RewardKind
	for _, w := range v.Warnings {
		// Evidence lost, verdict intact: worth reporting, not worth failing a
		// cell whose measurement is still perfectly good.
		tr.Error = appendErr(tr.Error, fmt.Errorf("%s", w))
	}
	if len(v.Errors) > 0 {
		// A grader that errored could not produce a verdict. Recording the cell
		// as failed keeps a broken harness from looking like a pass, and
		// grader_error_rate makes it visible in aggregate.
		tr.fail(fmt.Errorf("%s", strings.Join(v.Errors, "; ")))
	}

	if attempt != nil && attempt.Failed {
		// The agent process could not be started or died at the infrastructure
		// level, so the measurement itself is suspect.
		tr.Status = storage.TaskStatusFailed
		if tr.err == nil {
			tr.err = fmt.Errorf("agent failed: %s", attempt.Error)
			tr.Error = appendErr(tr.Error, tr.err)
		}
	}
	// Note: a non-zero exit code or a timeout is *not* a cell failure. The agent
	// ran and produced a patch, so the honest answer is a graded verdict with
	// passed=false and a stop reason of "exit_N"/"timeout". Folding those into
	// the failure status would make harness_error_rate meaningless.

	tr.EndedAt = time.Now().UTC()
	e.writeCellArtifacts(tr, c.Task, baseline)
	return tr
}

// rewardSpec resolves a task's reward contract.
//
// validateTasks has already rejected an unknown density, so the error branch is
// unreachable for a loaded dataset; returning the dense score rather than
// failing keeps a hand-built Config from turning into a panic here.
func (e *Engine) rewardSpec(t dataset.TaskSpec) reward.Spec {
	spec, err := reward.Resolve(t.Reward)
	if err != nil {
		return reward.Spec{Kind: reward.KindScore}
	}
	return spec
}

// runAgentWithRetries starts the agent, retrying only when the process could
// not be started at all.
//
// A nil attempt means "the agent never ran". An attempt that ran and failed --
// a timeout, a non-zero exit, an error result -- is returned as-is, because it
// is a measurement rather than a fault, and retrying it would quietly turn a
// timeout into a pass and make the reported timeout rate meaningless.
func (e *Engine) runAgentWithRetries(ctx context.Context, runner agent.Runner,
	sb sandbox.Sandbox, spec agent.Spec, prompt string, tr *TaskResult) (*agent.Attempt, error) {

	attempts := 1 + max(e.cfg.Retries, 0)
	var lastErr error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tr.Attempts++
		attempt, err := runner.Run(ctx, sb, spec, prompt)
		switch {
		case err != nil:
			lastErr = err
		case attempt == nil:
			lastErr = fmt.Errorf("agent returned no attempt and no error")
		default:
			return attempt, nil
		}
		if i+1 < attempts {
			e.emit(Event{Kind: EventTaskRetry, TaskID: tr.TaskID, SampleIndex: tr.SampleIndex,
				Message: fmt.Sprintf("attempt %d failed to start (%v), retrying", i+1, lastErr)})
			select {
			case <-time.After(e.cfg.RetryBackoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("agent could not be started after %d attempt(s): %w", attempts, lastErr)
}

// agentSpec layers the task's limits and system prompt onto the run's base
// agent spec.
func (e *Engine) agentSpec(t dataset.TaskSpec) agent.Spec {
	spec := e.cfg.Agent
	if l := t.Limits; l != nil {
		if l.MaxTurns > 0 {
			spec.MaxTurns = l.MaxTurns
		}
		if l.TimeoutS > 0 {
			spec.Timeout = time.Duration(l.TimeoutS) * time.Second
		}
		if l.MaxCostUSD > 0 {
			spec.MaxBudgetUSD = l.MaxCostUSD
		}
	}
	if spec.Timeout <= 0 {
		spec.Timeout = e.cfg.AgentTimeout
	}
	if t.SystemPrompt != "" {
		// The task's instruction comes last so it reads as the more specific
		// requirement, and a configured prefix is never silently dropped.
		if spec.AppendSystemPrompt != "" {
			spec.AppendSystemPrompt += "\n\n" + t.SystemPrompt
		} else {
			spec.AppendSystemPrompt = t.SystemPrompt
		}
	}
	return spec
}

// workRoot resolves the directory that hosts workspaces and artifacts.
func (e *Engine) workRoot() string {
	if e.cfg.WorkRoot != "" {
		return filepath.Join(e.cfg.WorkRoot, safeName(e.cfg.RunID))
	}
	return filepath.Join(os.TempDir(), "agenteval", safeName(e.cfg.RunID))
}

// skippedCell records a cell that never ran because the run was cancelled.
func (e *Engine) skippedCell(c cell, cause error) *TaskResult {
	now := time.Now().UTC()
	err := cause
	if err == nil {
		err = context.Canceled
	}
	tr := &TaskResult{
		TaskID:      c.Task.ID,
		Category:    c.Task.Category,
		Difficulty:  c.Task.Difficulty,
		Tags:        c.Task.Tags,
		SampleIndex: c.SampleIndex,
		Status:      storage.TaskStatusSkipped,
		StopReason:  "cancelled",
		StartedAt:   now,
		EndedAt:     now,
		err:         err,
	}
	return tr
}

// fail marks a cell as failed with err and stamps the end time.
func (r *TaskResult) fail(err error) *TaskResult {
	r.Status = storage.TaskStatusFailed
	// A failure before the agent ever ran is a harness failure; when an attempt
	// exists its own stop reason is more informative and is already set.
	if r.StopReason == "" {
		r.StopReason = "harness_error"
	}
	r.err = err
	r.Error = appendErr(r.Error, err)
	r.EndedAt = time.Now().UTC()
	return r
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func appendErr(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "-"
	}
	return h
}

func orNone(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

// safeName turns a task or run id into a single path element.
//
// Task ids come from a dataset file and routinely contain "/" and "." (upstream
// instance ids look like "django__django-11099"), which would otherwise create
// nested directories or escape the run root.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unnamed"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

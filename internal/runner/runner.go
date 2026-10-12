// Package runner executes a dataset against one agent and records the results.
//
// It replaces the prototype's orchestrator, and the difference is the point: the
// prototype carried run modes, anti-gaming guards, reward projection and an
// environment service, none of which serve the question this tool asks. What is
// left is the loop -- open a workspace, run the agent in it, grade what it left
// behind, write the row -- plus the two things that were learned the hard way:
//
//   - a timeout is a datum, not a crash. An agent that ran out of budget tells
//     you something about the agent; it is recorded as a timeout, not as an
//     error, and the two are never averaged together.
//   - the budget follows the task. A recorded baseline that spent nine minutes
//     getting to the answer must not be re-run under a thirty-second cap, or the
//     result will be a timeout that looks like incompetence.
package runner

import (
	"context"
	"encoding/json"
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
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/store"
	"github.com/skyitachi/agenteval/internal/trace"
)

// DefaultThreshold is the composite score a task must reach to pass. One means
// every weighted grader has to pass, which is the same reading as SWE-bench's
// resolved rate.
const DefaultThreshold = 1.0

// backendLocal is the only sandbox this build can select.
//
// internal/sandbox also ships a container backend, but no run path reaches it:
// there is no --backend flag, no dataset field, and no test that starts a
// container. That is worth knowing before trusting the isolation story -- a
// reference implementation nobody calls is not a feature, and a reader who sees
// NewDocker will assume containers are one flag away. Either wire it up with an
// end-to-end test or delete it; docs/real-repo-eval.md §3.1 tracks the choice.
const backendLocal = "local"

// graceAfterAgent is how much longer than the agent's own budget a cell is
// allowed to take. Workspace materialisation, diffing and grading all sit
// outside the agent attempt, and charging them to the agent's timeout would turn
// a slow filesystem into a task failure.
const graceAfterAgent = 90 * time.Second

// Config describes one run.
type Config struct {
	// Store receives the run and its cells. Required.
	Store *store.Store
	// Agent is the agent to drive. Kind defaults to claude_code.
	Agent agent.Spec

	// RunID is reused when resuming; empty generates one.
	RunID string
	// RunName and Notes are labels for the run list and the report.
	RunName string
	Notes   string

	// Samples is k in pass@k: how many times each task runs.
	Samples int
	// Concurrency is the number of cells in flight. Default 1.
	Concurrency int

	// WorkRoot is where per-cell workspaces are created. Default: a
	// "workspaces" directory beside the database.
	WorkRoot string
	// KeepWorkspaces leaves each cell's directory on disk for inspection.
	KeepWorkspaces bool

	// Timeout and MaxTurns are the fallbacks for tasks that declare none.
	Timeout  time.Duration
	MaxTurns int

	// Threshold is the composite score a task must reach. Zero means
	// DefaultThreshold.
	Threshold float64

	// FactorAxis and FactorLevel label the run as one point of a sweep, which
	// is what lets `aeval matrix` attribute a difference to a factor.
	FactorAxis  string
	FactorLevel string
	SweepID     string

	ToolVersion string

	// ConfigJSON is free-form JSON recorded on the run row, opaque to this
	// package. `aeval matrix` puts the sweep's own metadata there -- which axis
	// was swept and which level is the control -- so that attributing the sweep
	// later needs the database and not the matrix document, which may have been
	// edited in the meantime.
	ConfigJSON string

	// FailFast stops scheduling new cells after the first hard failure.
	FailFast bool
	// Resume skips cells that already have a row in the store.
	Resume bool

	// Progress, when set, is called once per finished cell.
	Progress func(Progress)
}

// Progress reports one finished cell.
type Progress struct {
	TaskID string
	Sample int
	// Done and Total count cells, not tasks.
	Done  int
	Total int
	Cell  store.Attempt
}

// Result is a finished run.
type Result struct {
	Run      store.Run
	Attempts []store.Attempt
	Summary  Summary
}

// Summary is the run's arithmetic, computed once so the CLI and the report
// cannot disagree about what happened.
type Summary struct {
	Tasks  int `json:"tasks"`
	Cells  int `json:"cells"`
	Graded int `json:"graded"`
	Passed int `json:"passed"`
	// Ungraded counts cells that were measured but had nothing to score them
	// against. They are reported rather than folded into a pass rate: a
	// collected session task is exactly this, and calling it a failure would
	// invent a verdict the data does not contain.
	Ungraded int `json:"ungraded"`
	Timeouts int `json:"timeouts"`
	// HarnessErrors counts cells the harness failed to measure at all. Separate
	// from Timeouts because "we could not measure it" and "it did not finish"
	// are different claims.
	HarnessErrors int `json:"harness_errors"`

	Tokens            int     `json:"tokens"`
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	CacheReadTokens   int     `json:"cache_read_tokens"`
	CacheCreateTokens int     `json:"cache_create_tokens"`
	CostUSD           float64 `json:"cost_usd"`
	DurationMs        int64   `json:"duration_ms"`

	// BaselineCells counts cells that had a baseline to compare against, and
	// TokenDelta sums (attempt - baseline) over them. A negative delta means
	// this run was cheaper than the recorded one.
	//
	// TokenCells and TimedCells count the subsets of those whose baseline also
	// recorded the matching measurement. They exist because a delta of zero and
	// an absent measurement are indistinguishable once summed: a recording that
	// never captured busy time leaves TimeDeltaMs at 0, and printing that as
	// "+0s" claims the attempt took exactly as long as a session nobody timed.
	// Each delta is reported only when its own count is non-zero.
	BaselineCells int   `json:"baseline_cells"`
	TokenCells    int   `json:"token_cells"`
	TimedCells    int   `json:"timed_cells"`
	TokenDelta    int   `json:"token_delta"`
	TimeDeltaMs   int64 `json:"time_delta_ms"`
}

// PassRate is the fraction of graded cells that passed. Graded only: an
// ungraded cell has no verdict, and counting it as a failure would make a
// dataset of collected sessions look like a failure rate.
func (s Summary) PassRate() (float64, bool) {
	if s.Graded == 0 {
		return 0, false
	}
	return float64(s.Passed) / float64(s.Graded), true
}

// Run executes the dataset. A non-nil error means the run could not be carried
// out at all; tasks that failed are recorded in the result, because a run in
// which some tasks failed is a measurement, not a crash.
func Run(ctx context.Context, ds *dataset.Dataset, cfg Config) (*Result, error) {
	if cfg.Store == nil {
		return nil, errors.New("runner: a store is required")
	}
	if ds == nil || len(ds.Tasks) == 0 {
		return nil, errors.New("runner: the dataset has no tasks")
	}
	cfg = withDefaults(cfg)

	drive, err := agent.New(cfg.Agent.Kind)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}

	run := store.Run{
		ID:          cfg.RunID,
		Name:        cfg.RunName,
		Status:      store.RunRunning,
		StartedAt:   time.Now().UTC(),
		DatasetPath: ds.Dir,
		DatasetName: ds.Manifest.Name,
		DatasetHash: ds.Hash,
		AgentKind:   cfg.Agent.Kind,
		AgentModel:  cfg.Agent.Model,
		AgentBin:    cfg.Agent.Bin,
		Backend:     backendLocal,
		Samples:     cfg.Samples,
		Concurrency: cfg.Concurrency,
		FactorAxis:  cfg.FactorAxis,
		FactorLevel: cfg.FactorLevel,
		SweepID:     cfg.SweepID,
		Notes:       cfg.Notes,
		ToolVersion: cfg.ToolVersion,
		ConfigJSON:  cfg.ConfigJSON,
	}
	if run.Name == "" {
		run.Name = ds.Manifest.Name
	}
	if err := cfg.Store.CreateRun(ctx, run); err != nil {
		return nil, err
	}

	cells, err := plan(ds, cfg)
	if err != nil {
		_ = cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, store.RunFailed, err.Error())
		return nil, err
	}
	if cfg.Resume {
		cells, err = dropDone(ctx, cfg.Store, run.ID, cells)
		if err != nil {
			_ = cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, store.RunFailed, err.Error())
			return nil, err
		}
	}

	// The per-cell rows are read back below rather than taken from the return
	// value, so that a resumed leg's rows are included too.
	_, err = execute(ctx, ds, cfg, drive, cells)
	status, errMsg := store.RunCompleted, ""
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		status, errMsg = store.RunInterrupted, err.Error()
	case err != nil:
		status, errMsg = store.RunFailed, err.Error()
	}
	// The finish call must survive a cancelled context: a run interrupted by
	// Ctrl-C still has to be recorded as interrupted rather than left running
	// forever in the list.
	if ferr := cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, status, errMsg); ferr != nil && err == nil {
		return nil, ferr
	}
	if err != nil {
		return nil, err
	}

	// Read back every row, including ones from an earlier resumed leg, so the
	// summary describes the run rather than this invocation of it.
	all, err := cfg.Store.ListAttempts(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	final, err := cfg.Store.GetRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	return &Result{Run: final, Attempts: all, Summary: Summarize(all, len(ds.Tasks))}, nil
}

// Summarize computes the run's arithmetic over its cells.
func Summarize(attempts []store.Attempt, tasks int) Summary {
	s := Summary{Tasks: tasks, Cells: len(attempts)}
	for _, a := range attempts {
		switch a.Status {
		case store.CellTimeout:
			s.Timeouts++
		case store.CellError:
			s.HarnessErrors++
		}
		if a.Graded {
			s.Graded++
			if a.Passed {
				s.Passed++
			}
		} else if a.Status == store.CellOK {
			s.Ungraded++
		}
		s.Tokens += a.TokenTotal()
		s.InputTokens += a.InputTokens
		s.OutputTokens += a.OutputTokens
		s.CacheReadTokens += a.CacheReadTokens
		s.CacheCreateTokens += a.CacheCreateTokens
		s.CostUSD += a.CostUSD
		s.DurationMs += a.DurationMs
		// Only cells that were actually measured contribute a comparison. See
		// store.Attempt.ComparableBaseline for why a harness error is excluded
		// and a timeout is not.
		if _, ok := a.ComparableBaseline(); ok {
			s.BaselineCells++
			if d, ok := a.TokenDelta(); ok {
				s.TokenCells++
				s.TokenDelta += d
			}
			if d, ok := a.MsDelta(); ok {
				s.TimedCells++
				s.TimeDeltaMs += d
			}
		}
	}
	return s
}

func withDefaults(cfg Config) Config {
	if cfg.Agent.Kind == "" {
		cfg.Agent.Kind = "claude_code"
	}
	// The id has to exist before the run row does: it also names the workspace
	// directory, and a run whose directory is named "" would put every cell of
	// every run in one place.
	if cfg.RunID == "" {
		cfg.RunID = agent.NewUUID()
	}
	if cfg.Samples <= 0 {
		cfg.Samples = 1
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultThreshold
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.WorkRoot == "" {
		dir := "."
		if cfg.Store != nil && cfg.Store.Path() != "" && cfg.Store.Path() != ":memory:" {
			dir = filepath.Dir(cfg.Store.Path())
		}
		cfg.WorkRoot = filepath.Join(dir, "workspaces")
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

type cell struct {
	task   dataset.TaskSpec
	sample int
	dir    string
}

func plan(ds *dataset.Dataset, cfg Config) ([]cell, error) {
	root, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		return nil, err
	}
	var cells []cell
	for _, t := range ds.Tasks {
		for s := 0; s < cfg.Samples; s++ {
			// A subdirectory per run, so two concurrent runs over one dataset
			// do not share a workspace and silently observe each other's files.
			dir := filepath.Join(root, cfg.RunID, t.ID, fmt.Sprintf("s%d", s))
			cells = append(cells, cell{task: t, sample: s, dir: dir})
		}
	}
	return cells, nil
}

func dropDone(ctx context.Context, st *store.Store, runID string, cells []cell) ([]cell, error) {
	done, err := st.ListAttempts(ctx, runID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(done))
	for _, a := range done {
		seen[fmt.Sprintf("%s#%d", a.TaskID, a.Sample)] = true
	}
	var out []cell
	for _, c := range cells {
		if seen[fmt.Sprintf("%s#%d", c.task.ID, c.sample)] {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func execute(ctx context.Context, ds *dataset.Dataset, cfg Config, drive agent.Runner, cells []cell) ([]store.Attempt, error) {
	if len(cells) == 0 {
		return nil, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		saved    []store.Attempt
		done     int
		firstErr error
	)
	queue := make(chan cell)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range queue {
				if runCtx.Err() != nil {
					return
				}
				cellCtx, cellCancel := context.WithCancel(runCtx)
				attempt := runCell(cellCtx, ds, cfg, drive, c)
				cellCancel()

				mu.Lock()
				if err := cfg.Store.SaveAttempt(runCtx, attempt); err != nil && firstErr == nil {
					firstErr = err
				}
				saved = append(saved, attempt)
				done++
				progress := Progress{TaskID: attempt.TaskID, Sample: attempt.Sample, Done: done, Total: len(cells), Cell: attempt}
				failFast := cfg.FailFast && (attempt.Status == store.CellError)
				mu.Unlock()

				if cfg.Progress != nil {
					cfg.Progress(progress)
				}
				if failFast {
					cancel()
				}
			}
		}()
	}

	var cancelled bool
	for _, c := range cells {
		select {
		case queue <- c:
		case <-runCtx.Done():
			cancelled = true
		}
		if cancelled {
			break
		}
	}
	close(queue)
	wg.Wait()

	if firstErr != nil {
		return saved, firstErr
	}
	if cancelled && ctx.Err() == nil && cfg.FailFast {
		// Fail-fast cancelled the run on purpose; that is not a run-level error.
		return saved, nil
	}
	if ctx.Err() != nil {
		return saved, ctx.Err()
	}
	return saved, nil
}

// runCell performs one attempt: build the workspace, run the agent, grade what
// is left, and compare against the recorded baseline when the task has one.
func runCell(ctx context.Context, ds *dataset.Dataset, cfg Config, drive agent.Runner, c cell) store.Attempt {
	t := c.task
	att := store.Attempt{
		RunID:    cfg.RunID,
		TaskID:   t.ID,
		Sample:   c.sample,
		Category: t.Category,
		Tags:     t.Tags,
		Status:   store.CellOK,
	}

	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fail(att, fmt.Errorf("create workspace %s: %w", c.dir, err))
	}
	sb, err := newSandbox(c.dir, t)
	if err != nil {
		return fail(att, err)
	}

	started := time.Now()
	defer func() {
		if !cfg.KeepWorkspaces {
			_ = sb.Close(context.WithoutCancel(ctx))
			_ = sandbox.RemoveDir(c.dir)
		} else {
			_ = sb.Close(context.WithoutCancel(ctx))
		}
	}()

	if err := sandbox.Materialize(ctx, sb, t.Workspace, ds.Dir); err != nil {
		return fail(att, fmt.Errorf("materialize: %w", err))
	}
	baseline, err := sandbox.CurrentBaseline(ctx, sb)
	if err != nil {
		return fail(att, fmt.Errorf("baseline: %w", err))
	}

	// The agent's own budget, with the cell given room beyond it for setup,
	// diffing and grading.
	budget := time.Duration(t.Limits.TimeoutS) * time.Second
	if budget <= 0 {
		budget = cfg.Timeout
	}
	cellCtx, cancelCell := context.WithTimeout(ctx, budget+graceAfterAgent)
	defer cancelCell()

	agentCtx, cancel := context.WithTimeout(cellCtx, budget)
	attempt, agentErr := drive.Run(agentCtx, sb, specFor(cfg, t, budget), t.ResolvedPrompt)
	cancel()

	if agentErr != nil {
		return fail(att, fmt.Errorf("agent: %w", agentErr))
	}
	if attempt == nil {
		return fail(att, errors.New("agent returned no attempt"))
	}
	att.DurationMs = time.Since(started).Milliseconds()
	att.StopReason = attempt.StopReason()
	applyUsage(&att, attempt)

	switch {
	case attempt.Failed:
		att.Status = store.CellError
		att.Error = strings.TrimSpace(attempt.Error)
		if att.Error == "" {
			att.Error = "the agent process could not be run"
		}
	case attempt.TimedOut:
		// Data about the agent, not a harness failure.
		att.Status = store.CellTimeout
	}

	// The normalized trajectory goes in the row: tool calls, the terminal
	// result and the per-model usage. The verbatim stream is not, because the
	// normalized form is what a report can render, and the raw stream's only
	// unique content is the payload size.
	if attempt.Trace != nil {
		att.Trace = traceJSON(attempt.Trace)
	}

	// Diff and grade even on a timeout: whatever the agent left behind is the
	// most informative part of a truncated attempt, and throwing it away would
	// make "ran out of time" and "did nothing" indistinguishable.
	diff, _, err := changedSet(cellCtx, baseline, t)
	if err != nil {
		att.Error = joinMsg(att.Error, "diff: "+err.Error())
		if att.Status == store.CellOK {
			att.Status = store.CellError
		}
	} else {
		att.Diff = diff
	}

	if len(t.Graders) > 0 && att.Status != store.CellError {
		grade, err := gradeTask(cellCtx, t, sb, baseline, attempt, cfg.Threshold)
		if err != nil {
			att.Error = joinMsg(att.Error, "grade: "+err.Error())
			if att.Status == store.CellOK {
				att.Status = store.CellError
			}
		} else {
			att.Graded = true
			att.Passed = grade.Passed
			att.Score = grade.Score
			att.Threshold = grade.Threshold
			att.Grades = grade.Results
		}
	}
	att.Answer = attempt.FinalText()
	att.Baseline = compareToBaseline(t, att.DurationMs)
	att.WorkspaceDir = c.dir
	return att
}

// applyUsage copies the agent's accounting into the row. The token classes are
// renamed on the way across: the trace uses the CLI's vocabulary
// (cache_read_input_tokens), the table uses the report's (cache_read_tokens).
// Thinking tokens are deliberately dropped -- they are billed as output, and
// adding them to the total would double-count.
func applyUsage(att *store.Attempt, a *agent.Attempt) {
	u := a.Usage()
	att.InputTokens = u.InputTokens
	att.OutputTokens = u.OutputTokens
	att.CacheReadTokens = u.CacheReadInputTokens
	att.CacheCreateTokens = u.CacheCreationInputTokens
	att.CostUSD = a.CostUSD()
	if a.Trace == nil {
		return
	}
	att.Turns = a.Trace.Turns()
	att.ToolCalls = len(a.Trace.ToolCalls)
	for _, tc := range a.Trace.ToolCalls {
		if tc.IsError {
			att.ToolErrors++
		}
	}
}

// traceJSON encodes the normalized trajectory. A marshalling failure is not
// worth failing a measured cell over, so it yields nil and the row keeps an
// empty trace rather than losing the tokens and the grade.
func traceJSON(t *trace.Trace) json.RawMessage {
	b, err := json.Marshal(t)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func newSandbox(root string, t dataset.TaskSpec) (sandbox.Sandbox, error) {
	env := make(map[string]string, len(t.Workspace.Env))
	for _, e := range t.Workspace.Env {
		env[e.Name] = e.Value
	}
	sb, err := sandbox.NewLocal(sandbox.LocalConfig{
		Root: root,
		Env:  env,
		// The runner owns the directory's lifetime, so the sandbox must not
		// remove it out from under the diff.
		KeepWorkspace: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start local sandbox: %w", err)
	}
	return sb, nil
}

func specFor(cfg Config, t dataset.TaskSpec, budget time.Duration) agent.Spec {
	spec := cfg.Agent
	if t.Limits.MaxTurns > 0 {
		spec.MaxTurns = t.Limits.MaxTurns
	} else if cfg.MaxTurns > 0 {
		spec.MaxTurns = cfg.MaxTurns
	}
	spec.Timeout = budget
	if t.Limits.MaxCostUSD > 0 {
		spec.MaxBudgetUSD = t.Limits.MaxCostUSD
	}
	if t.SystemPrompt != "" {
		spec.AppendSystemPrompt = joinPrompt(spec.AppendSystemPrompt, t.SystemPrompt)
	}
	return spec
}

func changedSet(ctx context.Context, b *sandbox.Baseline, t dataset.TaskSpec) (string, []string, error) {
	opts := diffOptions(t)
	diff, err := b.Diff(ctx, opts)
	if err != nil {
		return "", nil, err
	}
	changed, err := b.ChangedPaths(ctx, opts)
	if err != nil {
		return diff, nil, err
	}
	return diff, changed, nil
}

func diffOptions(t dataset.TaskSpec) sandbox.DiffOptions {
	opts := sandbox.DiffOptions{IgnorePaths: append([]string{}, grader.HarnessIgnore...)}
	if t.Output != nil {
		opts.Paths = t.Output.Paths
		opts.IgnorePaths = append(opts.IgnorePaths, t.Output.IgnorePaths...)
	}
	return opts
}

func gradeTask(ctx context.Context, t dataset.TaskSpec, sb sandbox.Sandbox, base *sandbox.Baseline,
	attempt *agent.Attempt, threshold float64) (*grader.Grade, error) {

	gc := grader.NewContext(t, sb, base, attempt)
	return grader.RunTask(ctx, gc, threshold)
}

// compareToBaseline resolves the comparison now, so the report needs the
// database and nothing else.
func compareToBaseline(t dataset.TaskSpec, durationMs int64) store.BaselineComparison {
	b := t.ResolvedBaseline
	if b == nil {
		return store.BaselineComparison{}
	}
	ref := ""
	if t.Baseline != nil {
		ref = t.Baseline.File
	}
	// BusyMs, not the wall clock: the recorded session had a human in the loop
	// and was idle for most of its wall time. See dataset.Baseline.BusyMs.
	busy := b.BusyMs
	if busy == 0 {
		busy = b.DurationMs
	}
	assistant := 0
	for _, turn := range b.Turns {
		if turn.Role == "assistant" {
			assistant++
		}
	}
	return store.BaselineComparison{
		Present: true,
		ID:      b.ID,
		Ref:     ref,
		Tokens:  b.Usage.Total(),
		Ms:      busy,
		Cost:    b.CostUSD,
		Turns:   assistant,
		Output:  b.FinalOutput,
	}
}

func fail(att store.Attempt, err error) store.Attempt {
	att.Status = store.CellError
	att.Error = err.Error()
	return att
}

func joinMsg(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func joinPrompt(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}

// SortedTaskIDs is a stable ordering for reports and diffs.
func SortedTaskIDs(attempts []store.Attempt) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range attempts {
		if !seen[a.TaskID] {
			seen[a.TaskID] = true
			out = append(out, a.TaskID)
		}
	}
	sort.Strings(out)
	return out
}

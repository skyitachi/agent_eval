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
	"github.com/skyitachi/agenteval/internal/machine"
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

	// ChainSessions gives every task collected from one recording one shared
	// workspace and runs them in the order they were asked.
	//
	// A turn-split collection makes each prompt its own task, and the later
	// prompts refer to what the earlier ones produced -- so replaying them in
	// separate, freshly materialised workspaces asks the agent to answer a
	// follow-up about files that were never there. This is the flag that puts
	// the sequence back together.
	//
	// It is off by default because it changes what the cells mean: a chained
	// cell starts from its predecessor's state, so the cells of one chain are a
	// sequence rather than independent samples, and the run row records that so
	// the report can say it.
	ChainSessions bool

	// WorkRoot is where per-cell workspaces are created. Default: a
	// "workspaces" directory beside the database. It is unused when InPlace is
	// set, because then no workspace is created at all.
	WorkRoot string
	// KeepWorkspaces leaves each cell's directory on disk for inspection.
	KeepWorkspaces bool

	// InPlace works in the directory each task records, checked out at the
	// commit the task names, instead of in a copy under WorkRoot.
	//
	// This is the one mode that touches something the harness does not own. The
	// recorded directory is the user's working tree, and the run's contract is
	// to put it back: the branch, the commit and the tracked files it found are
	// restored when the chain that was using it finishes. What cannot be put
	// back is refused before the first cell -- see Preflight -- because a
	// refusal costs nothing and a half-restored repository costs the user their
	// work.
	//
	// It is a run-time decision rather than a dataset one: the same tasks can be
	// replayed against a copy, which is what a shared or archived dataset should
	// do, and the run row records which of the two happened so that a reader of
	// the report is never left guessing.
	InPlace bool

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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Before the run row exists, so that a dataset which cannot be run this way
	// leaves no failed run behind for a mistake that was knowable up front.
	if err := CheckMachine(ds); err != nil {
		return nil, err
	}
	if err := Preflight(ctx, ds, cfg); err != nil {
		return nil, err
	}
	// One lock per repository for the whole run, taken before the first cell
	// and held until the last one has been put back. Per run rather than per
	// chain: two chains of one run can name the same repository -- two samples
	// of one task do by construction -- and a per-chain lock would make the run
	// refuse itself.
	held, err := acquireInPlace(ctx, ds, cfg)
	if err != nil {
		return nil, err
	}
	defer releaseInPlace(held)

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
		// Recorded because it changes how the cells below read: a chained cell
		// starts from what its predecessor left, so a page that shows them as
		// independent samples is describing a sequence as a set.
		ChainSessions: cfg.ChainSessions,
		// And for the same reason: a cell measured in someone's repository had
		// that repository's history in front of it, and the run put the tree
		// back afterwards. Neither is visible in any number on the row.
		InPlace:     cfg.InPlace,
		FactorAxis:  cfg.FactorAxis,
		FactorLevel: cfg.FactorLevel,
		SweepID:     cfg.SweepID,
		Notes:       cfg.Notes,
		ToolVersion: cfg.ToolVersion,
		ConfigJSON:  cfg.ConfigJSON,
	}
	// The whole configuration, not just the labels above. The skill set is
	// plugin_dirs plus the isolation flags that decide whether it survives, and
	// a comparison keyed on the labels alone cannot see it.
	//
	// The run-level spec is the right one to record even though each cell gets
	// a derived copy: specFor only fills in the per-task budget and prompt, so
	// every cell of a run drives the same agent configuration.
	if raw, err := json.Marshal(cfg.Agent); err == nil {
		run.AgentSpecJSON = string(raw)
	}
	if run.Name == "" {
		run.Name = ds.Manifest.Name
	}
	if err := cfg.Store.CreateRun(ctx, run); err != nil {
		return nil, err
	}

	planned, err := plan(ds, cfg)
	if err != nil {
		_ = cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, store.RunFailed, err.Error())
		return nil, err
	}
	cells := planned
	if cfg.Resume {
		cells, err = dropDone(ctx, cfg.Store, run.ID, cells)
		if err != nil {
			_ = cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, store.RunFailed, err.Error())
			return nil, err
		}
		if cfg.ChainSessions {
			if err := chainsIntact(planned, cells, cfg); err != nil {
				_ = cfg.Store.FinishRun(context.WithoutCancel(ctx), run.ID, store.RunFailed, err.Error())
				return nil, err
			}
		}
	}

	// The per-cell rows are read back below rather than taken from the return
	// value, so that a resumed leg's rows are included too.
	_, err = execute(ctx, ds, cfg, drive, cells, held)
	var restoreErr *restoreError
	stranded := errors.As(err, &restoreErr)
	status, errMsg := store.RunCompleted, ""
	switch {
	case stranded:
		// The run happened and its cells are measurements, so the row says
		// completed: marking it failed would hide a valid result behind a fact
		// about a directory the harness does not own. The failure still has to
		// reach the exit code, which is the one channel a person cannot miss,
		// and it does -- below, after the row is finished.
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

// Validate reports a configuration that would measure something other than what
// it says.
//
// Run calls it, and so does the command line -- not to save the call, but so
// that a flag mistake is reported as a usage error rather than as a run that
// started and failed. The check lives here because the invariant is this
// package's: chained cells share a directory, and two of them in flight at once
// would write over each other with no way for the row to say so.
func (c Config) Validate() error {
	if c.ChainSessions && c.Concurrency > 1 {
		return fmt.Errorf("chaining a session's tasks gives them one workspace and runs them in order, "+
			"so it cannot keep %d cells in flight at once", c.Concurrency)
	}
	if c.InPlace && c.Concurrency > 1 {
		// Not "the same repository": two cells can name the same directory even
		// when they are different tasks, and nothing in the scheduler keeps them
		// apart. One at a time is the only arrangement in which the state a cell
		// puts back is the state it found.
		return fmt.Errorf("a run that works in the recorded repositories themselves cannot keep "+
			"%d cells in flight at once: two cells can name the same directory, and nothing would "+
			"stop them checking out over each other and putting back a state neither of them "+
			"started from", c.Concurrency)
	}
	if c.InPlace && c.KeepWorkspaces {
		return errors.New("this run creates no workspaces to keep: it works in the recorded " +
			"repositories themselves, and each one is put back when its chain finishes. Drop the flag")
	}
	return nil
}

// InPlaceRepos are the directories an in-place run would work in, sorted and
// deduplicated. It is empty for any other run.
//
// It exists so that the caller can say what is about to happen to which
// directories before it happens, without resolving the paths a second way and
// risking a banner that names a different directory from the one the run uses.
func InPlaceRepos(ds *dataset.Dataset, cfg Config) []string {
	if !cfg.InPlace || ds == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range ds.Tasks {
		p := t.Workspace.RepoPath(ds.Dir)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Preflight reports whether a run can be carried out at all, before any of it
// is.
//
// It is separate from Validate because these questions are about the dataset
// and the machine rather than about the flags: Validate cannot see either. It
// is separate from the run itself because the answer has to arrive before the
// first cell -- a run that dies on the fourth task because the fourth task's
// repository is dirty has already worked in three others, and one that stops
// after creating the run row leaves a failed run behind for a mistake that was
// knowable up front.
//
// Everything it checks is checked again where it is used: this is the answer a
// person needs, not the one the run relies on.
func Preflight(ctx context.Context, ds *dataset.Dataset, cfg Config) error {
	if !cfg.InPlace {
		return nil
	}
	if ds == nil {
		return errors.New("no dataset to run")
	}
	for _, t := range ds.Tasks {
		ws := t.Workspace
		if ws.Type != dataset.WorkspaceGit {
			return fmt.Errorf("task %s has no repository to work in, and this run works in the "+
				"recorded directories themselves: its workspace is %s. Run it without --in-place, or "+
				"collect it bound to a repository with --workspace-cwd", t.ID, orEmptyType(ws.Type))
		}
		repo := ws.RepoPath(ds.Dir)
		if repo == "" {
			return fmt.Errorf("task %s names no repository, and this run works in the directory the "+
				"task records. Re-collect it with --workspace-cwd, or run it without --in-place", t.ID)
		}
		if strings.Contains(ws.Repo, "://") || strings.HasPrefix(ws.Repo, "git@") {
			return fmt.Errorf("task %s names %s, which is a remote rather than a directory on this "+
				"machine. This run works in a directory: clone it and point the task at the checkout, "+
				"or run without --in-place", t.ID, ws.Repo)
		}
		if !filepath.IsAbs(ws.Repo) {
			// A relative repo is resolved against the dataset directory, so a
			// dataset that has been copied elsewhere names a different
			// directory. A clone-based run would clone whatever is there and
			// the difference would show up in the diff; a run that works in the
			// directory cannot notice afterwards that it worked in the wrong
			// one.
			return fmt.Errorf("task %s names its repository as %s, which is relative to the dataset, "+
				"and this run works in a directory rather than cloning one. Point it at an absolute "+
				"path, or run without --in-place", t.ID, ws.Repo)
		}
		if ws.Subdir != "" {
			return fmt.Errorf("task %s works in a subdirectory of its repository, and this run "+
				"checks out and measures at the repository root: the agent would be pointed somewhere "+
				"the harness is not measuring. Collect it without --subdir, or run it against a copy",
				t.ID)
		}
		if len(ws.Setup) > 0 {
			return fmt.Errorf("task %s runs setup commands before the agent starts, and this run "+
				"works in the recorded directory itself: whatever they produce would be left in the "+
				"repository and measured as the agent's work. Run it against a copy, or drop the "+
				"setup commands", t.ID)
		}
		if ws.KeepGitHistory != nil && !*ws.KeepGitHistory {
			// The task says its history must not be readable. In a copy the
			// materializer honours that by dropping .git; here the history is
			// the repository, and a run that went ahead would be measuring an
			// agent that could read the answer.
			return fmt.Errorf("task %s declares that its repository's history must not be readable "+
				"by the agent, and this run works in that repository, where the history is. This mode "+
				"cannot honour that: run it against a copy", t.ID)
		}
		if _, err := os.Stat(repo); err != nil {
			return fmt.Errorf("task %s records the directory %s, which is not there: this run works "+
				"in the recorded directory rather than cloning one", t.ID, repo)
		}
	}
	for _, repo := range InPlaceRepos(ds, cfg) {
		if err := sandbox.CheckInPlace(ctx, repo); err != nil {
			return err
		}
	}
	return nil
}

// currentMachine is the identity a replay compares against. It is a variable so
// tests can stand in for a machine other than the one they run on.
var currentMachine = machine.Current

// CheckMachine refuses a dataset whose baselines were collected on a different
// machine from this one.
//
// A collected baseline is a measurement of a session on one machine: its
// working directory, the repository at that path, and the tools and caches
// around it. A replay elsewhere would compare against those numbers while
// running in something else, and nothing in the result would say so -- so the
// replay does not start. A baseline that names no machine is not checked:
// hand-written ones, and those collected before the identity was recorded,
// have always run anywhere.
//
// It is separate from Preflight because it applies to every run, not only an
// in-place one. Run calls it for the same reason it calls Preflight: a refused
// run must not leave a failed run row behind.
func CheckMachine(ds *dataset.Dataset) error {
	if ds == nil {
		return nil
	}
	var (
		here    machine.Identity
		probed  bool
		foreign []string
		other   machine.Identity
	)
	for _, t := range ds.Tasks {
		b := t.ResolvedBaseline
		if b == nil || b.Machine == nil {
			continue
		}
		if !probed {
			var err error
			if here, err = currentMachine(); err != nil {
				return fmt.Errorf("task %s was collected on %s, and this machine cannot be identified "+
					"to check that it is the same one: %v", t.ID, b.Machine, err)
			}
			probed = true
		}
		if b.Machine.Same(here) {
			continue
		}
		if len(foreign) == 0 {
			other = *b.Machine
		}
		foreign = append(foreign, t.ID)
	}
	if len(foreign) == 0 {
		return nil
	}
	names, more := foreign, ""
	if len(names) > 5 {
		names, more = names[:5], fmt.Sprintf(" and %d more", len(foreign)-5)
	}
	return fmt.Errorf("%d task(s) were collected on a different machine and cannot be replayed here: %s%s\n"+
		"  recorded on: %s\n"+
		"  running on:  %s\n"+
		"A baseline measures a session in its own environment -- its directories, its repository, "+
		"its tools -- so a replay on another machine would be compared against numbers it did not run "+
		"under. Replay on the machine that collected them, or re-collect the sessions on this one",
		len(foreign), strings.Join(names, ", "), more, other, here)
}

func orEmptyType(t dataset.WorkspaceType) string {
	if t == "" {
		return string(dataset.WorkspaceEmpty)
	}
	return string(t)
}

// acquireInPlace takes every repository an in-place run will work in.
//
// In sorted order, so that two runs over overlapping datasets cannot take the
// same two locks in opposite orders. Neither of them waits -- the lock is taken
// exclusively and refused immediately -- but a deterministic order means the
// refusal a person reads is the same one every time.
//
// A failure gives back everything already taken. A lock held by a run that
// never started is a repository that refuses every later run for a reason that
// is no longer true.
func acquireInPlace(ctx context.Context, ds *dataset.Dataset, cfg Config) (map[string]*sandbox.InPlace, error) {
	if !cfg.InPlace {
		return nil, nil
	}
	held := map[string]*sandbox.InPlace{}
	for _, repo := range InPlaceRepos(ds, cfg) {
		ip, err := sandbox.AcquireInPlace(ctx, repo)
		if err != nil {
			releaseInPlace(held)
			return nil, err
		}
		held[repo] = ip
	}
	return held, nil
}

func releaseInPlace(held map[string]*sandbox.InPlace) {
	for _, ip := range held {
		_ = ip.Release()
	}
}

// withDefaults fills in what a Config may leave out.
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
	// Resolve the isolation pointer here so that every copy of the spec -- the
	// one the cells drive and the one written to the database -- says what it
	// means. A nil Isolation already means the clean-room preset everywhere it
	// is acted on; leaving it nil in the recorded copy would move that
	// derivation into every reader, and a reader that skips it reports a
	// clean-room run as one that accepted the host's configuration.
	if cfg.Agent.Isolation == nil {
		iso := agent.DefaultIsolation()
		cfg.Agent.Isolation = &iso
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
	// chain names the sequence this cell belongs to: one session at one
	// sample, or a lone task's own name when it belongs to no session.
	chain string
	// first and last mark the ends of a chain. Only the first materialises the
	// workspace and only the last removes it, and that is the whole mechanism:
	// a chained cell starts from what its predecessor left because nobody put
	// the directory back in between.
	first bool
	last  bool
	// ip is the repository this cell works in directly, and is nil for every
	// other run. It is filled in from the run's held map when the cell is
	// dispatched, so that the cell carries the one handle whose lock the run
	// owns rather than looking one up.
	ip *sandbox.InPlace
}

// chainKey names the sequence a cell belongs to.
//
// With --chain-sessions on, everything collected from one session is one
// sequence, so its tasks share a workspace and run in the order they were
// asked. A task that names no session -- a hand-written one, or one collected
// before the field existed -- is its own sequence, which is what keeps the flag
// from changing an ordinary dataset at all.
func chainKey(t dataset.TaskSpec, chaining bool) string {
	if chaining && strings.TrimSpace(t.Session) != "" {
		return pathName(t.Session)
	}
	return pathName(t.ID)
}

// pathName makes a string safe to use as one path component.
//
// Session ids come from a recording, so they are UUIDs in practice -- but a
// task file is editable by hand and a name with a separator in it would put the
// workspace somewhere other than where the plan says, or escape the work root
// altogether.
func pathName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "task"
	}
	return out
}

func plan(ds *dataset.Dataset, cfg Config) ([]cell, error) {
	root, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		return nil, err
	}
	// Group before ordering, so that a chained session replays its prompts in
	// the order they were asked rather than in whatever order the tasks file
	// happens to list them. Without chaining every group holds one cell and this
	// reduces to the task-major, sample-minor order it has always been.
	type groupKey struct {
		chain  string
		sample int
	}
	var order []groupKey
	groups := map[groupKey][]cell{}
	for _, t := range ds.Tasks {
		key := chainKey(t, cfg.ChainSessions)
		for s := 0; s < cfg.Samples; s++ {
			gk := groupKey{chain: key, sample: s}
			if _, seen := groups[gk]; !seen {
				order = append(order, gk)
			}
			// A subdirectory per run, so two concurrent runs over one dataset
			// do not share a workspace and silently observe each other's files.
			//
			// An in-place run is the exception, and the whole point of it: the
			// directory is the one the task records, which the run has already
			// locked. Every cell that names it therefore shares it -- which is
			// what a chain wants, and why concurrency is refused for this mode.
			dir := filepath.Join(root, cfg.RunID, key, fmt.Sprintf("s%d", s))
			if cfg.InPlace {
				dir = t.Workspace.RepoPath(ds.Dir)
			}
			groups[gk] = append(groups[gk], cell{task: t, sample: s, dir: dir, chain: key})
		}
	}

	var cells []cell
	for _, gk := range order {
		g := groups[gk]
		sortChain(g)
		for i := range g {
			g[i].first = i == 0
			g[i].last = i == len(g)-1
		}
		cells = append(cells, g...)
	}
	return cells, nil
}

// sortChain puts one session's prompts in the order they were asked.
//
// PromptIndex is the recorded order and it is what the chain is replayed in.
// A group where any cell is missing it keeps the dataset's own order rather
// than guessing: sorting by a half-known key would put the unknown ones
// wherever the missing value happened to land, which is worse than leaving the
// file's order alone.
func sortChain(g []cell) {
	if len(g) < 2 {
		return
	}
	for _, c := range g {
		if c.task.PromptIndex == nil {
			return
		}
	}
	sort.SliceStable(g, func(i, j int) bool {
		return *g[i].task.PromptIndex < *g[j].task.PromptIndex
	})
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

// chainsIntact reports a chain that is only half recorded.
//
// A chained cell starts from the workspace its predecessor left, and the
// predecessor's workspace went away with its cell -- so a resume that skips the
// predecessor starts the successor from nothing, and the row it writes says
// nothing about that. Refusing is the honest answer: the numbers would be about
// the harness's bookkeeping rather than about the agent, and a report cannot
// tell the two apart after the fact.
//
// A chain that is entirely recorded is fine to skip, and one that is entirely
// missing is fine to run -- it is only the split that is unmeasurable.
func chainsIntact(planned, remaining []cell, cfg Config) error {
	present := make(map[string]bool, len(remaining))
	for _, c := range remaining {
		present[fmt.Sprintf("%s#%d", c.task.ID, c.sample)] = true
	}
	type chain struct {
		name        string
		total, left int
	}
	var order []string
	chains := map[string]*chain{}
	for _, c := range planned {
		key := fmt.Sprintf("%s#%d", c.chain, c.sample)
		ch := chains[key]
		if ch == nil {
			ch = &chain{name: key}
			chains[key] = ch
			order = append(order, key)
		}
		ch.total++
		if present[fmt.Sprintf("%s#%d", c.task.ID, c.sample)] {
			ch.left++
		}
	}
	for _, key := range order {
		ch := chains[key]
		if ch.left == 0 || ch.left == ch.total {
			continue
		}
		// Why the remaining tasks have nothing to start from depends on where
		// they would have started from. In a copy it is that the directory went
		// away with the cell; in place it is that the repository was put back
		// where it was found, which is the opposite of leaving it behind and
		// just as fatal to a half-resumed chain.
		reason := "and that workspace went away with the cell, so these would each run against nothing"
		if cfg.InPlace {
			reason = "and the repository was put back where it was found once that cell was done, " +
				"so these would each start from a state their predecessor never left"
		}
		return fmt.Errorf("chain %s is half recorded: %d of its %d task(s) still have to run and the "+
			"rest already have a row. A chained task starts from the workspace its predecessor left, "+
			"%s. Re-run the chain from its first task instead, under a new run id",
			ch.name, ch.left, ch.total, reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func execute(ctx context.Context, ds *dataset.Dataset, cfg Config, drive agent.Runner, cells []cell,
	held map[string]*sandbox.InPlace) ([]store.Attempt, error) {
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
		// broken names the chains whose workspace was never established, and
		// why. A later cell of such a chain must not be run: it would start
		// from nothing and be recorded as a result.
		broken = map[string]string{}
		// stranded names the repositories that could not be put back. They are
		// collected rather than reported per cell: the same directory is used
		// by every cell of a chain and by every sample of a task, and a reader
		// needs to know that a directory is in the wrong state, not how many
		// cells noticed.
		stranded []string
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
				mu.Lock()
				reason := broken[c.chain]
				mu.Unlock()

				var attempt store.Attempt
				var broke string
				var restoreErr error
				if reason != "" {
					// Recorded rather than skipped, so the cell shows up in the
					// report as the harness error it is. Dropping it would make
					// the chain look shorter than the dataset says, and the
					// reader would have no way to tell that from a chain the
					// dataset never had.
					attempt = fail(store.Attempt{
						RunID: cfg.RunID, TaskID: c.task.ID, Sample: c.sample,
						Category: c.task.Category, Tags: c.task.Tags,
					}, fmt.Errorf("chain %s never got a workspace: %s", c.chain, reason))
				} else {
					c.ip = held[c.dir]
					cellCtx, cellCancel := context.WithCancel(runCtx)
					attempt, broke, restoreErr = runCell(cellCtx, ds, cfg, drive, c)
					cellCancel()
				}

				mu.Lock()
				if broke != "" {
					broken[c.chain] = broke
				}
				if restoreErr != nil {
					stranded = append(stranded, restoreErr.Error())
				}
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
	if len(stranded) > 0 {
		sort.Strings(stranded)
		return saved, &restoreError{stranded: dedupeStrings(stranded)}
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

// restoreError is a run that measured what it set out to measure and could not
// put a repository back afterwards.
//
// It is not a run failure and must not be recorded as one: the cells ran, they
// are measurements, and a row that says "failed" would hide a valid result
// behind a fact about a directory the harness does not own. What it does have
// to do is reach the exit code, because a person whose repository is checked
// out at the wrong commit cannot be told by a zero exit.
type restoreError struct{ stranded []string }

func (e *restoreError) Error() string {
	return "the run finished, but these repositories were not put back:\n  " +
		strings.Join(e.stranded, "\n  ")
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// runCell performs one attempt: build the workspace, run the agent, grade what
// is left, and compare against the recorded baseline when the task has one.
//
// The second result names why the chain's workspace was never established, when
// that is what happened; it is empty for every other failure. The distinction is
// the difference between data about the agent and data about the harness: an
// agent that crashed or timed out tells you something, while a workspace that
// was never built leaves the rest of the chain with nothing to start from, and
// the cells after it would be measuring that absence rather than the agent.
func runCell(ctx context.Context, ds *dataset.Dataset, cfg Config, drive agent.Runner, c cell) (store.Attempt, string, error) {
	t := c.task
	att := store.Attempt{
		RunID:    cfg.RunID,
		TaskID:   t.ID,
		Sample:   c.sample,
		Category: t.Category,
		Tags:     t.Tags,
		Status:   store.CellOK,
	}
	var broke string
	// restored records that the repository was put back on the way out, so that
	// the deferred guard below does not do it a second time.
	var restored bool
	var restoreErr error

	// The run holds one lock per repository and hands each cell the handle it
	// owns. A cell with no handle would be a run that got past Preflight
	// without taking the lock, and working in the directory anyway would be
	// exactly the thing the lock exists to prevent -- so it is refused here
	// rather than panicking on the nil.
	if cfg.InPlace && c.ip == nil {
		msg := fmt.Sprintf("no lock was taken on %s, so this run will not work in it", c.dir)
		return fail(att, errors.New(msg)), msg, nil
	}

	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fail(att, fmt.Errorf("create workspace %s: %w", c.dir, err)), err.Error(), nil
	}
	sb, err := newSandbox(c.dir, t)
	if err != nil {
		return fail(att, err), err.Error(), nil
	}

	started := time.Now()
	defer func() {
		_ = sb.Close(context.WithoutCancel(ctx))
		if cfg.InPlace {
			// The directory is the user's, so the removal below is not reached
			// in this mode -- not because a flag says so but because this branch
			// returns first. What is left to do here is the restore for the
			// paths that skip the explicit one: a panic, or a return added
			// later above it.
			if !restored && (c.last || broke != "") {
				_, _ = c.ip.Restore(context.WithoutCancel(ctx))
			}
			return
		}
		if cfg.KeepWorkspaces {
			return
		}
		// The workspace outlives the cell inside a chain: the next task starts
		// from what this one left, which is the whole point. Only a chain's last
		// cell -- and so every cell of a chain of one -- takes it away, and so
		// does a cell that broke the chain, since nothing will come for it.
		if c.last || broke != "" {
			_ = sandbox.RemoveDir(c.dir)
		}
	}()

	// One materialisation per chain. Rebuilding it for every task would put the
	// workspace back exactly as the recording started and throw away what the
	// earlier task in the chain did -- which is the work the later prompt is
	// asking about.
	if c.first {
		if cfg.InPlace {
			if err := c.ip.Checkout(ctx, t.Workspace.Ref); err != nil {
				broke = err.Error()
				return fail(att, fmt.Errorf("check out the recorded commit: %w", err)), broke, nil
			}
		} else if err := sandbox.Materialize(ctx, sb, t.Workspace, ds.Dir); err != nil {
			broke = err.Error()
			return fail(att, fmt.Errorf("materialize: %w", err)), broke, nil
		}
	}
	baseline, err := sandbox.CurrentBaseline(ctx, sb)
	if err != nil {
		broke = err.Error()
		return fail(att, fmt.Errorf("baseline: %w", err)), broke, nil
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
		return fail(att, fmt.Errorf("agent: %w", agentErr)), broke, nil
	}
	if attempt == nil {
		return fail(att, errors.New("agent returned no attempt")), broke, nil
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

	// The repository goes back at the end of the chain that was using it, not
	// at the end of every cell: a chain's later tasks start from what the
	// earlier ones left, and putting the tree back between them would throw
	// away the very work the next prompt is about.
	if cfg.InPlace && (c.last || broke != "") {
		rep, err := c.ip.Restore(ctx)
		restored = true
		att.RestoreNotes = rep.Notes
		restoreErr = err
	}
	return att, broke, restoreErr
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
		// Who made the recording, and what it says about itself. The report
		// has no other way to learn either: a baseline is an input that lives
		// beside the dataset, and the dataset is not consulted at render time.
		AgentName:  b.AgentName,
		AgentModel: b.AgentModel,
		Source:     b.Source,
		Notes:      b.Notes,
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

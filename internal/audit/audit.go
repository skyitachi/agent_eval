// Package audit answers whether a dataset's tasks are worth measuring.
//
// # The problem it solves
//
// A resolve rate is only interpretable if the tasks behind it are sound. Three
// defects make it uninterpretable, and none of them is visible in an ordinary
// run:
//
//   - Unsolvable. The task's own reference solution does not pass, so the task
//     cannot be solved at all and its failures say nothing about the agent.
//   - Already solved. The untouched workspace already passes, so the task hands
//     out a reward for doing nothing. Under RL this is worse than useless: it is
//     free reward, and a policy finds free reward.
//   - Unstable. The graders disagree with themselves across identical runs, so
//     part of the measured score is flakiness rather than capability.
//
// Audit establishes all three without a model in the loop, by running the engine
// in its no-agent modes and comparing verdicts. It spends sandbox time but no
// API budget.
//
// # Why the evidence is ordinary runs
//
// Each check is an engine run with its own row in the database, tagged with the
// audit group and its role. Nothing here invents a parallel result format: the
// evidence for "task X is flaky" is a set of ordinary runs that `aeval report`
// can already render, and the audit report is a reading of them.
package audit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/storage"
)

// Check names. They are the vocabulary of the report and of --only.
const (
	// CheckSolvability asks whether the oracle passes.
	CheckSolvability = "solvability"
	// CheckNecessity asks whether the baseline fails.
	CheckNecessity = "necessity"
	// CheckStability asks whether repeated oracle runs agree.
	CheckStability = "stability"
)

// AllChecks lists the checks in report order.
var AllChecks = []string{CheckSolvability, CheckNecessity, CheckStability}

// Status is the outcome of one check on one task.
type Status string

const (
	// StatusOK means the task behaved the way a sound task must.
	StatusOK Status = "ok"
	// StatusFailed means the task is defective; the reason is in Detail.
	StatusFailed Status = "failed"
	// StatusUnknown means the check could not be carried out -- no oracle was
	// declared, the environment broke, or too few repeats were requested.
	//
	// Unknown is deliberately distinct from Failed. "We could not tell" and "we
	// looked, and it is broken" call for different actions, and collapsing them
	// would let a broken machine be reported as a dataset verdict.
	StatusUnknown Status = "unknown"
)

// Check is one question asked of one task, with its evidence.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Detail explains a non-ok status in one sentence.
	Detail string `json:"detail,omitempty"`
	// Passed and Score are the observed verdict, where one was observed.
	Passed bool    `json:"passed"`
	Score  float64 `json:"score"`
	// Graders names the graders that disagreed across repeats (stability only).
	Graders []string `json:"graders,omitempty"`
	// RunIDs are the runs the verdict was read from.
	RunIDs []string `json:"run_ids,omitempty"`
}

// TaskReport is the health of one task.
type TaskReport struct {
	TaskID string  `json:"task_id"`
	Checks []Check `json:"checks"`
	// Healthy is true only when every check that ran came back ok. An unknown
	// check makes a task not healthy: an unverified task is not a verified one.
	Healthy bool `json:"healthy"`
	// Reason is a one-line explanation, empty when Healthy.
	Reason string `json:"reason,omitempty"`
}

// Check returns the named check, or nil when it did not run.
func (t *TaskReport) Check(name string) *Check {
	for i := range t.Checks {
		if t.Checks[i].Name == name {
			return &t.Checks[i]
		}
	}
	return nil
}

// RunRef records a child run so a reader can go and look at it.
type RunRef struct {
	Role  string `json:"role"`
	RunID string `json:"run_id"`
}

// Options configures an audit.
type Options struct {
	// Group prefixes the child run ids and tags them, so every run produced by
	// one audit can be found again. Empty generates one.
	Group string
	// Repeats is how many oracle runs to perform. 1 establishes solvability but
	// leaves stability unchecked; 2 or more makes flakiness measurable.
	Repeats int
	// Concurrency overrides the run configuration's worker count when positive.
	Concurrency int
	// KeepWorkspaces leaves the workspaces on disk for inspection.
	KeepWorkspaces bool
	// Only narrows the checks to run. Empty means all of them.
	Only []string
}

func (o Options) withDefaults() Options {
	if strings.TrimSpace(o.Group) == "" {
		o.Group = "audit-" + agent.NewUUID()
	}
	if o.Repeats <= 0 {
		o.Repeats = 1
	}
	return o
}

// wants reports whether a check should run.
func (o Options) wants(name string) bool {
	if len(o.Only) == 0 {
		return true
	}
	for _, n := range o.Only {
		if n == name {
			return true
		}
	}
	return false
}

// Report is the outcome of an audit.
type Report struct {
	Group       string       `json:"group"`
	Dataset     string       `json:"dataset"`
	DatasetHash string       `json:"dataset_hash"`
	Repeats     int          `json:"repeats"`
	Tasks       []TaskReport `json:"tasks"`
	Runs        []RunRef     `json:"runs"`
	Warnings    []string     `json:"warnings,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	EndedAt     time.Time    `json:"ended_at"`
}

// Healthy counts the tasks that passed every check that ran.
func (r *Report) Healthy() int {
	var n int
	for i := range r.Tasks {
		if r.Tasks[i].Healthy {
			n++
		}
	}
	return n
}

// Unhealthy returns the tasks that are not healthy, in dataset order.
func (r *Report) Unhealthy() []TaskReport {
	var out []TaskReport
	for _, t := range r.Tasks {
		if !t.Healthy {
			out = append(out, t)
		}
	}
	return out
}

// Tally counts tasks per status for a named check.
func (r *Report) Tally(name string) map[Status]int {
	counts := map[Status]int{}
	for i := range r.Tasks {
		if c := r.Tasks[i].Check(name); c != nil {
			counts[c.Status]++
		}
	}
	return counts
}

// Summary renders the audit for the console.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "audit %s\n", r.Group)
	fmt.Fprintf(&b, "  dataset   %s (%s)\n", r.Dataset, shortHash(r.DatasetHash))
	fmt.Fprintf(&b, "  oracle    %d repeat(s) per task\n", r.Repeats)
	fmt.Fprintf(&b, "  healthy   %d/%d task(s)\n", r.Healthy(), len(r.Tasks))
	for _, name := range AllChecks {
		counts := r.Tally(name)
		if counts[StatusOK]+counts[StatusFailed]+counts[StatusUnknown] == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-12s  %d ok, %d failed, %d unknown\n",
			name, counts[StatusOK], counts[StatusFailed], counts[StatusUnknown])
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  note      %s\n", w)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// Run audits ds.
//
// It returns a report for any dataset that could be executed, including one
// whose tasks are all defective: a defective task is the *finding*, not a
// failure of the audit. A non-nil error means the audit itself could not be
// carried out, and the report returned alongside is then partial.
func Run(ctx context.Context, cfg engine.Config, ds *dataset.Dataset, opts Options) (*Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("audit: dataset is nil")
	}
	if len(ds.Tasks) == 0 {
		return nil, fmt.Errorf("audit: dataset %q has no tasks", ds.Manifest.Name)
	}
	o := opts.withDefaults()

	rep := &Report{
		Group:       o.Group,
		Dataset:     ds.Manifest.Name,
		DatasetHash: ds.Hash,
		Repeats:     o.Repeats,
		StartedAt:   time.Now().UTC(),
	}

	// ---- necessity: one baseline run ---------------------------------------
	var baseline *childRun
	if o.wants(CheckNecessity) {
		run, err := runChild(ctx, cfg, ds, ds, o, "baseline",
			engine.RunModeBaseline, o.Group+": necessity check, does the untouched workspace already pass?")
		if err != nil {
			// Without the baseline run there is no necessity verdict for any
			// task. That is a machine problem rather than a dataset one, so the
			// audit stops instead of reporting a page of unknowns as a finding.
			return rep, fmt.Errorf("audit: baseline run: %w", err)
		}
		rep.Runs = append(rep.Runs, RunRef{Role: "baseline", RunID: run.runID})
		baseline = run
	}

	// ---- solvability and stability: repeated oracle runs -------------------
	//
	// Only tasks that declare an oracle can be checked this way. The engine
	// refuses to run oracle mode on a task without one, which is right for
	// `aeval run` and wrong here: an audit must be able to report "this task
	// cannot be audited" rather than refuse to start.
	var oracleRuns []*childRun
	if o.wants(CheckSolvability) || o.wants(CheckStability) {
		capable := withOracle(ds)
		if len(capable.Dataset.Tasks) == 0 {
			rep.Warnings = append(rep.Warnings,
				"no task declares an oracle, so solvability and stability are unknown for every task")
		}
		for i := 0; i < o.Repeats && len(capable.Dataset.Tasks) > 0; i++ {
			role := fmt.Sprintf("oracle-%d", i)
			run, err := runChild(ctx, cfg, capable.Dataset, ds, o, role, engine.RunModeOracle,
				fmt.Sprintf("%s: reference solution, repeat %d/%d", o.Group, i+1, o.Repeats))
			if err != nil {
				// Partial repeats still answer solvability; stability degrades
				// to unknown, which is exactly what the check reports.
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("oracle run %d/%d could not be carried out: %v", i+1, o.Repeats, err))
				break
			}
			rep.Runs = append(rep.Runs, RunRef{Role: role, RunID: run.runID})
			oracleRuns = append(oracleRuns, run)
		}
	}

	// ---- assemble per-task verdicts ----------------------------------------
	for _, t := range ds.Tasks {
		tr := TaskReport{TaskID: t.ID, Healthy: true}
		if o.wants(CheckSolvability) {
			tr.Checks = append(tr.Checks, solvability(t, oracleRuns))
		}
		if o.wants(CheckNecessity) {
			tr.Checks = append(tr.Checks, necessity(t, baseline))
		}
		if o.wants(CheckStability) {
			tr.Checks = append(tr.Checks, stability(t, oracleRuns, o.Repeats))
		}
		for i := range tr.Checks {
			if tr.Checks[i].Status != StatusOK {
				tr.Healthy = false
				break
			}
		}
		tr.Reason = firstFailure(tr.Checks)
		rep.Tasks = append(rep.Tasks, tr)
	}

	rep.EndedAt = time.Now().UTC()
	return rep, nil
}

// childRun is the outcome of one engine run performed on behalf of an audit.
type childRun struct {
	runID string
	// cells maps task id to its result. The cell is nil for a task the run did
	// not reach.
	cells map[string]*engine.TaskResult
}

// cell returns the result for a task, or nil.
func (c *childRun) cell(taskID string) *engine.TaskResult {
	if c == nil {
		return nil
	}
	return c.cells[taskID]
}

// runChild performs one audit run, tags it, and projects its result by task id.
//
// The child is an ordinary engine run: same sandbox, same graders, same storage.
// The only differences are the mode, the run id, the sample count and the tags,
// so the evidence stays inspectable with the tools that already exist.
func runChild(ctx context.Context, parent engine.Config, runDS, parentDS *dataset.Dataset,
	o Options, role string, mode engine.RunMode, notes string) (*childRun, error) {

	sub := runDS
	if sub == nil {
		sub = parentDS
	}

	cfg := parent
	cfg.Mode = mode
	cfg.RunID = o.Group + "-" + role
	cfg.RunName = o.Group + " " + role
	cfg.Notes = notes
	// An audit cell is one task observed once. Repeating the oracle is expressed
	// as separate runs, because stability is a property of the task across runs
	// and folding it into samples would hide which run disagreed.
	cfg.SamplesPerTask = 1
	// Resume and FailFast both defeat the purpose: the first would answer a
	// check from a previous audit's evidence, the second would stop at the first
	// defect and leave the rest unknown.
	cfg.Resume = false
	cfg.FailFast = false
	if o.Concurrency > 0 {
		cfg.Concurrency = o.Concurrency
	}
	cfg.KeepWorkspaces = cfg.KeepWorkspaces || o.KeepWorkspaces

	eng, err := engine.New(cfg)
	if err != nil {
		return nil, err
	}
	res, err := eng.Run(ctx, sub)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("run produced no result")
	}
	if res.Status == storage.StatusFailed {
		return nil, fmt.Errorf("run %s failed: %s", res.RunID, res.Error)
	}

	if st := cfg.Store; st != nil {
		// Tags are how the runs of one audit are found again; a failure to write
		// them must not discard a measurement that has already been paid for.
		_ = st.SetRunTag(ctx, res.RunID, "audit_group", o.Group)
		_ = st.SetRunTag(ctx, res.RunID, "audit_role", role)
	}

	out := &childRun{runID: res.RunID, cells: make(map[string]*engine.TaskResult, len(res.Tasks))}
	for _, tr := range res.Tasks {
		if tr == nil {
			continue
		}
		// A task can appear twice only when samples_per_task > 1, which cannot
		// happen here; keeping the first makes the projection total either way.
		if _, seen := out.cells[tr.TaskID]; seen {
			continue
		}
		out.cells[tr.TaskID] = tr
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Per-check reasoning
// ---------------------------------------------------------------------------

// solvability reports whether the task's own reference solution passes.
func solvability(t dataset.TaskSpec, runs []*childRun) Check {
	c := Check{Name: CheckSolvability}
	if t.Oracle == nil {
		c.Status = StatusUnknown
		c.Detail = "no oracle declared, so the task cannot be shown to be solvable"
		return c
	}
	if len(runs) == 0 {
		c.Status = StatusUnknown
		c.Detail = "no oracle run was carried out"
		return c
	}
	// The first repeat is the reference reading; the rest are the stability
	// check's business.
	first := runs[0]
	c.RunIDs = []string{first.runID}
	tr := first.cell(t.ID)
	switch {
	case tr == nil:
		c.Status = StatusUnknown
		c.Detail = "the oracle run did not reach this task"
	case tr.Grade != nil:
		c.Passed, c.Score = tr.Grade.Passed, tr.Grade.Score
		if tr.Grade.Passed {
			c.Status = StatusOK
			return c
		}
		c.Status = StatusFailed
		c.Detail = fmt.Sprintf(
			"the reference solution does not pass (score %.3f, threshold %.3f); failures on this task say nothing about the agent",
			tr.Grade.Score, tr.Grade.Threshold)
	case tr.StopReason == "oracle_error":
		c.Status = StatusFailed
		c.Detail = "the declared oracle could not be applied: " + tr.Error
	default:
		c.Status = StatusUnknown
		c.Detail = "the oracle run produced no verdict (" + orNone(tr.StopReason) + ")" + colon(tr.Error)
	}
	return c
}

// necessity reports whether the untouched workspace fails.
func necessity(t dataset.TaskSpec, baseline *childRun) Check {
	c := Check{Name: CheckNecessity}
	if baseline == nil {
		c.Status = StatusUnknown
		c.Detail = "no baseline run was carried out"
		return c
	}
	c.RunIDs = []string{baseline.runID}
	tr := baseline.cell(t.ID)
	switch {
	case tr == nil:
		c.Status = StatusUnknown
		c.Detail = "the baseline run did not reach this task"
	case tr.Grade != nil:
		c.Passed, c.Score = tr.Grade.Passed, tr.Grade.Score
		if !tr.Grade.Passed {
			c.Status = StatusOK
			return c
		}
		c.Status = StatusFailed
		c.Detail = "the untouched workspace already passes (score " + trimFloat(tr.Grade.Score) +
			"); this task pays a reward for doing nothing"
	default:
		c.Status = StatusUnknown
		c.Detail = "the baseline run produced no verdict (" + orNone(tr.StopReason) + ")" + colon(tr.Error)
	}
	return c
}

// stability reports whether repeated oracle runs agree.
//
// Only the oracle is repeated. It is deterministic by construction -- a fixed
// patch against a fixed baseline -- so any disagreement is attributable to the
// graders or to the environment, which is exactly what a flakiness check is
// looking for. Repeating an agent run would fold model sampling noise into the
// measurement and flag every genuinely hard task as unstable.
func stability(t dataset.TaskSpec, runs []*childRun, repeats int) Check {
	c := Check{Name: CheckStability}
	if t.Oracle == nil {
		c.Status = StatusUnknown
		c.Detail = "no oracle declared, so run-to-run agreement cannot be measured"
		return c
	}
	if repeats < 2 {
		c.Status = StatusUnknown
		c.Detail = fmt.Sprintf("needs at least two oracle runs; %d requested (raise --repeats)", repeats)
		return c
	}
	if len(runs) < 2 {
		c.Status = StatusUnknown
		c.Detail = fmt.Sprintf("only %d of %d oracle runs completed", len(runs), repeats)
		return c
	}
	for _, r := range runs {
		if r.cell(t.ID) == nil {
			c.Status = StatusUnknown
			c.Detail = "some oracle runs did not reach this task"
			return c
		}
		c.RunIDs = append(c.RunIDs, r.runID)
	}

	first := runs[0].cell(t.ID)
	c.Passed, c.Score = first.Passed(), first.Score()

	var (
		unstable []string
		flipped  bool
	)
	for i := 1; i < len(runs); i++ {
		other := runs[i].cell(t.ID)
		if other.Passed() != first.Passed() {
			flipped = true
		}
		unstable = append(unstable, differingGraders(first, other)...)
	}
	if !flipped && len(unstable) == 0 {
		c.Status = StatusOK
		return c
	}
	c.Status = StatusFailed
	c.Graders = dedupeSorted(unstable)
	detail := "repeated oracle runs disagree"
	if flipped {
		detail += fmt.Sprintf(" on the overall verdict (%t then %t)",
			first.Passed(), runs[len(runs)-1].cell(t.ID).Passed())
	}
	if len(c.Graders) > 0 {
		detail += "; graders that moved: " + strings.Join(c.Graders, ", ")
	}
	c.Detail = detail + ". Part of this task's score is flakiness, not capability"
	return c
}

// differingGraders names the graders whose verdicts differ between two runs of
// the same task.
//
// Graders are keyed by configured name and type, falling back to position, so
// that two graders sharing an empty name do not shadow each other and get the
// disagreement attributed to an anonymous entry. A grader that appears in one
// run and not the other counts as a difference in both directions: a grader
// that vanished in a repeat is as much a reason to distrust the number as one
// that changed its answer.
func differingGraders(a, b *engine.TaskResult) []string {
	if a.Grade == nil || b.Grade == nil {
		return nil
	}
	left, right := graderVerdicts(a.Grade), graderVerdicts(b.Grade)
	seen := map[string]bool{}
	var out []string
	record := func(key, suffix string) {
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key+suffix)
	}
	for k, lp := range left {
		rp, ok := right[k]
		switch {
		case !ok:
			record(k, " (present in only one run)")
		case rp != lp:
			record(k, "")
		}
	}
	for k := range right {
		if _, ok := left[k]; !ok {
			record(k, " (present in only one run)")
		}
	}
	sort.Strings(out)
	return out
}

func graderVerdicts(g *grader.Grade) map[string]bool {
	m := make(map[string]bool, len(g.Results))
	for i, r := range g.Results {
		m[graderKey(r, i)] = r.Passed
	}
	return m
}

// graderKey identifies a grader within a task's grade.
func graderKey(r grader.Result, i int) string {
	switch {
	case r.Grader != "" && r.Type != "":
		return r.Grader + "[" + r.Type + "]"
	case r.Grader != "":
		return r.Grader
	case r.Type != "":
		return r.Type
	default:
		return fmt.Sprintf("#%d", i)
	}
}

// ---------------------------------------------------------------------------
// Dataset projection
// ---------------------------------------------------------------------------

// filtered is a dataset restricted to the tasks a check can run on.
type filtered struct {
	Dataset *dataset.Dataset
}

// withOracle returns the tasks that declare a reference solution.
//
// The manifest hash is inherited unchanged: the audit is a statement about *this*
// dataset, and a hash computed over a subset would name a dataset that does not
// exist. The engine records the run as a normal run of that dataset, and the
// audit group tags say which subset it actually covered.
func withOracle(ds *dataset.Dataset) filtered {
	var tasks []dataset.TaskSpec
	for _, t := range ds.Tasks {
		if t.Oracle == nil {
			continue
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == len(ds.Tasks) {
		// Nothing was filtered out; reuse the dataset so the child run records
		// the same hash and path an ordinary run would.
		return filtered{Dataset: ds}
	}
	sub := *ds
	sub.Tasks = tasks
	return filtered{Dataset: &sub}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func firstFailure(checks []Check) string {
	for _, c := range checks {
		if c.Status != StatusOK {
			return string(c.Status) + " " + c.Name + ": " + c.Detail
		}
	}
	return ""
}

func colon(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + s
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no stop reason"
	}
	return s
}

func trimFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", f), "0"), ".")
}

func shortHash(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Package regrade re-runs a task's graders against what a finished run recorded.
//
// # Why this exists
//
// A verdict is not always reachable at the moment the agent finishes. A
// collected session has a baseline but no grader, a judge may be missing its
// credentials, a rubric may be wrong. Re-running the task to fix any of those
// pays for the agent a second time and produces a *different* attempt, so the
// new verdict would belong to different work than the one being judged.
//
// Everything needed to score again is already in the row: the diff, the final
// answer and the normalized trajectory. This package rebuilds a grade context
// from those and runs the graders over it.
//
// # What it cannot do
//
// A grader that executes inside the workspace (pytest, command) needs a live
// sandbox that a finished run no longer has. Those are refused by name rather
// than run against nothing: a grader that errors is scored as a failure, and a
// failure produced by the harness would be read as a failure produced by the
// agent. A judge with no credentials is refused the same way and for the same
// reason -- nothing is written, because a verdict reached by a broken harness
// is worse than no verdict.
package regrade

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/llm"
	"github.com/skyitachi/agenteval/internal/store"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Options changes what a re-grade grades with.
type Options struct {
	// Dataset overrides the manifest path recorded on the run.
	//
	// The run remembers where its dataset was, but a dataset can move, and the
	// one on disk can have been edited since. Pointing at a copy is how a
	// reader scores against the original while looking at a modified tree.
	Dataset string

	// Graders replaces the task's own grader list.
	//
	// Empty means "the graders the dataset declares", which is the reproducible
	// choice: the same rubric that produced the recorded verdict, over the same
	// work. A non-empty list exists for the cases the dataset cannot cover --
	// trying a rubric, or grading a collected task that has none.
	Graders []dataset.GraderSpec

	// Fallback grades a task that declares no graders of its own, when neither
	// Graders nor the task supplies any.
	//
	// It exists for entry points that cannot ask for one. A button on a report
	// page is pressed by a reader looking at a task that says "not graded"; the
	// answer "this dataset declares no graders" is a message about a file they
	// may never have seen, and the alternative -- editing the dataset to score
	// an attempt that already happened -- is worse than naming a default.
	Fallback []dataset.GraderSpec

	// Threshold overrides the cutoff recorded on the cell. Zero keeps it.
	Threshold float64
}

// Outcome is one cell's new verdict, with the row it replaced.
type Outcome struct {
	RunID  string `json:"run_id"`
	TaskID string `json:"task_id"`
	Sample int    `json:"sample"`

	// Before is the cell as it was recorded, so a caller can say what changed
	// instead of only what the new verdict is.
	Before store.Attempt `json:"before"`

	// Grade is the verdict that was written back.
	Grade *grader.Grade `json:"grade"`

	// Dataset is the manifest the task and its graders were read from.
	Dataset string `json:"dataset"`
}

// Changed reports whether the re-grade reached a different verdict.
//
// An ungraded cell counts as changed once it has a verdict: the point of
// grading a collected task is that "measured, nothing to score" became an
// answer, and reporting that as "no change" would hide the whole result.
func (o *Outcome) Changed() bool {
	b := o.Before
	if !b.Graded {
		return true
	}
	return b.Passed != o.Grade.Passed || b.Score != o.Grade.Score
}

// One re-grades a single cell of a run.
//
// sample selects which pass of the task to grade; a negative value means "the
// first sample recorded". The verdict is written back to the row, because the
// row is what every report is rendered from -- a verdict held only in memory
// would disagree with the page the reader is looking at.
func One(ctx context.Context, st *store.Store, runID, taskID string, sample int, o Options) (*Outcome, error) {
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	row, err := cellOf(ctx, st, runID, taskID, sample)
	if err != nil {
		return nil, err
	}
	if row.Status == store.CellError {
		// Nothing ran, so there is nothing whose quality could be judged. A
		// verdict here would be a statement about the harness wearing the
		// agent's name.
		msg := strings.TrimSpace(row.Error)
		if msg == "" {
			msg = "the cell failed before the agent produced anything"
		}
		return nil, fmt.Errorf("cell %s#%d never ran (%s); there is nothing to grade", taskID, row.Sample, msg)
	}

	dsPath := o.Dataset
	if dsPath == "" {
		dsPath = run.DatasetPath
	}
	if dsPath == "" {
		return nil, fmt.Errorf("run %s does not record the dataset it used; pass --dataset", runID)
	}
	ds, err := dataset.Load(dsPath)
	if err != nil {
		return nil, fmt.Errorf("the dataset this run used cannot be read at %s: %w\n"+
			"the task and its graders live there; pass --dataset to point at a copy, "+
			"or --grader to name a grader directly", dsPath, err)
	}
	idx, ok := ds.TaskIndex()[taskID]
	if !ok {
		return nil, fmt.Errorf("%s no longer defines task %q (it defines: %s)",
			dsPath, taskID, strings.Join(taskIDs(ds), ", "))
	}
	task := ds.Tasks[idx]

	specs := o.Graders
	if len(specs) == 0 {
		specs = task.Graders
	}
	if len(specs) == 0 {
		specs = o.Fallback
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("task %q declares no graders in %s, so there is nothing to score it with; "+
			"name one (--grader on the command line, or the grader picker on a report page) -- "+
			"llm_judge is the one a web replay attaches by default", taskID, dsPath)
	}
	for _, spec := range specs {
		if err := checkUsable(spec); err != nil {
			return nil, err
		}
	}
	dataset.NameGraders(specs)
	task.Graders = specs

	threshold := o.Threshold
	if threshold <= 0 {
		threshold = row.Threshold
	}
	if threshold <= 0 {
		threshold = grader.Threshold
	}

	diff := row.Diff
	gc := &grader.Context{
		Task:    task,
		Attempt: recordedAttempt(row),
		// The diff is the one the run measured and stored. Recomputing it would
		// need the workspace, and the workspace is gone: this is the whole
		// point of grading a stored row rather than a live one.
		DiffOverride: &diff,
	}
	grade, err := grader.RunTask(ctx, gc, threshold)
	if err != nil {
		return nil, err
	}

	update := store.GradeUpdate{
		Graded:    true,
		Passed:    grade.Passed,
		Score:     grade.Score,
		Threshold: grade.Threshold,
		Grades:    grade.Results,
	}
	if len(grade.Errors) == 0 {
		if cleaned := withoutGradeNote(row.Error); cleaned != row.Error {
			update.Error = &cleaned
		}
	}
	if err := st.UpdateGrade(ctx, runID, taskID, row.Sample, update); err != nil {
		return nil, err
	}

	return &Outcome{
		RunID:   runID,
		TaskID:  taskID,
		Sample:  row.Sample,
		Before:  *row,
		Grade:   grade,
		Dataset: dsPath,
	}, nil
}

// cellOf finds one cell and, when it is missing, lists what the run does hold.
//
// "not found" on its own sends the reader to the wrong question -- a typo in a
// task id and a run that never reached that task look identical. The recorded
// ids are the answer to both, so they are part of the error.
func cellOf(ctx context.Context, st *store.Store, runID, taskID string, sample int) (*store.Attempt, error) {
	attempts, err := st.ListAttempts(ctx, runID)
	if err != nil {
		return nil, err
	}
	var row *store.Attempt
	seen := map[string]bool{}
	var tasks []string
	for i := range attempts {
		a := &attempts[i]
		if !seen[a.TaskID] {
			seen[a.TaskID] = true
			tasks = append(tasks, a.TaskID)
		}
		if a.TaskID != taskID {
			continue
		}
		if sample >= 0 && a.Sample != sample {
			continue
		}
		if row == nil || a.Sample < row.Sample {
			row = a
		}
	}
	sort.Strings(tasks)
	if row == nil {
		where := ""
		if sample >= 0 {
			where = fmt.Sprintf(" for sample %d", sample)
		}
		if len(tasks) == 0 {
			return nil, fmt.Errorf("run %s has no cells at all", runID)
		}
		return nil, fmt.Errorf("run %s has no cell for task %q%s (recorded tasks: %s)",
			runID, taskID, where, strings.Join(tasks, ", "))
	}
	return row, nil
}

// recordedAttempt rebuilds the attempt a grader sees from the stored row.
//
// The trajectory is restored because a judge may be asked for the diff, the
// answer, or both, and the answer's canonical source is the trace. When the
// trace is absent or unreadable -- it is dropped first when a payload is too
// large -- the stored answer is put where FinalText looks, so the text being
// judged is the text the run recorded, not an empty string that would read as
// an agent that said nothing.
func recordedAttempt(row *store.Attempt) *agent.Attempt {
	att := &agent.Attempt{Trace: &trace.Trace{}}
	if len(row.Trace) > 0 {
		if err := json.Unmarshal(row.Trace, att.Trace); err != nil {
			att.Trace = &trace.Trace{}
		}
	}
	if strings.TrimSpace(att.Trace.FinalText()) == "" && row.Answer != "" {
		att.Trace.Result.ResultText = row.Answer
	}
	return att
}

// checkUsable rejects a grader that cannot reach a verdict against a stored row.
//
// There are two ways that happens, and both are reasons not to write anything
// rather than failures to record in a verdict:
//
//   - the grader executes inside the workspace (pytest, command), and a
//     finished run has none left;
//   - the grader is a judge with no credentials in the environment, so every
//     call it makes fails.
//
// Left to run, both land as grader errors, and the runner records a grader
// error as a failed task -- a statement about the harness wearing the agent's
// name, written over a row that may have been passing. Refusing by name keeps
// the row untouched and the reason actionable.
func checkUsable(spec dataset.GraderSpec) error {
	if _, err := grader.Get(spec.Type); err != nil {
		return err
	}
	name := spec.Name
	if strings.TrimSpace(name) == "" {
		name = spec.Type
	}
	if grader.NeedsWorkspace(spec.Type) {
		return fmt.Errorf("grader %q (%s) runs inside the workspace, and a finished run has none left; "+
			"grade with one that reads the recorded diff and answer (%s), or re-run the task",
			name, spec.Type, strings.Join(offlineGraders(), ", "))
	}
	if spec.Type == "llm_judge" {
		c := llm.Config{}
		if v, ok := spec.Config["api_key_env"].(string); ok {
			c.APIKeyEnv = v
		}
		if v, ok := spec.Config["auth_token_env"].(string); ok {
			c.AuthTokenEnv = v
		}
		client := llm.New(c)
		if !client.Available() {
			return fmt.Errorf("grader %q calls a language model and has no credentials: %w",
				name, client.MissingCredentialsError())
		}
	}
	return nil
}

// offlineGraders lists the registered graders that can run from a stored row.
//
// Derived from the registry rather than written out, so a grader added later
// appears in the message without anyone remembering to add it -- and one that
// starts shelling out disappears from it.
func offlineGraders() []string {
	var out []string
	for _, typ := range grader.Registered() {
		if !grader.NeedsWorkspace(typ) {
			out = append(out, typ)
		}
	}
	return out
}

// taskIDs lists what a dataset defines, for the error that reports a typo.
func taskIDs(ds *dataset.Dataset) []string {
	out := make([]string, 0, len(ds.Tasks))
	for _, t := range ds.Tasks {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

// withoutGradeNote removes the note a failed grading pass left on the row.
//
// The runner appends "grade: <reason>" to the cell's error when grading itself
// breaks. A re-grade that succeeds makes that note false, and a row that says
// both "graded: yes, passed: yes" and "grade: unknown grader type" is one a
// reader cannot trust. Only segments the runner itself wrote are removed.
func withoutGradeNote(msg string) string {
	if msg == "" {
		return ""
	}
	parts := strings.Split(msg, "; ")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.HasPrefix(p, "grade: ") {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "; ")
}

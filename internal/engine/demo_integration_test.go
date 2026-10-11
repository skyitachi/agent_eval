package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// The shipped demo dataset is the only end-to-end fixture: it exercises all
// three task shapes (patch + visible pytest suite, patch + hidden pytest suite,
// stdout exact match) through the real loader, the real sandbox, the real
// baseline diff and the real graders.
//
// It is deliberately driven by the mock agent rather than a model. The mock
// writes a *wrong* patch, so the test asserts that a wrong patch is honestly
// reported as failing -- which is the property that matters, and it costs
// nothing to run.
const demoDatasetPath = "../../datasets/demo"

func loadDemo(t *testing.T) *dataset.Dataset {
	t.Helper()
	if _, err := os.Stat(demoDatasetPath); err != nil {
		t.Skipf("demo dataset not available: %v", err)
	}
	ds, err := dataset.Load(demoDatasetPath)
	if err != nil {
		t.Fatalf("load demo dataset: %v", err)
	}
	if err := ds.Validate(); err != nil {
		t.Fatalf("the shipped demo dataset does not validate: %v", err)
	}
	return ds
}

// pythonWithPytest returns an interpreter that can import pytest, or "".
//
// The demo's two pytest tasks are the only part of the pipeline that needs a
// third-party package; on a machine without one the grader reports an actionable
// error, which is correct but not what this test is about, so those assertions
// are skipped rather than weakened.
func pythonWithPytest(t *testing.T) string {
	t.Helper()
	var candidates []string
	if env := strings.TrimSpace(os.Getenv(grader.EnvPython)); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, "python3", "python")
	for _, c := range candidates {
		bin, err := exec.LookPath(c)
		if err != nil && !filepath.IsAbs(c) {
			continue
		}
		if err != nil {
			bin = c
		}
		if exec.Command(bin, "-c", "import pytest").Run() == nil {
			return bin
		}
	}
	return ""
}

func TestDemoDatasetEndToEnd(t *testing.T) {
	ds := loadDemo(t)
	if len(ds.Tasks) == 0 {
		t.Fatal("the demo dataset has no tasks")
	}

	py := pythonWithPytest(t)
	if py != "" {
		// Pin it so the pytest grader uses the interpreter we just probed rather
		// than re-probing and possibly picking a different one.
		t.Setenv(grader.EnvPython, py)
	} else {
		t.Logf("no interpreter with pytest found; the pytest-based assertions will be skipped. "+
			"Set %s to enable them.", grader.EnvPython)
	}

	inherit := false
	cfg := Config{
		Agent:        agent.Spec{Kind: agent.KindMock, Bin: clobberScript, Model: "mock"},
		Backend:      BackendConfig{Kind: BackendLocal, InheritEnv: &inherit},
		WorkRoot:     t.TempDir(),
		RunName:      "demo integration",
		RetryBackoff: time.Millisecond,
		Store:        newStore(t),
		RunID:        "demo-run",
	}
	e := mustEngine(t, cfg)

	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v\n%-40s", err, res.Summary())
	}
	// The run itself must succeed even though every graded verdict is a failure:
	// "the agent did badly" is a result, not an error.
	if res.Status != storage.StatusCompleted {
		t.Fatalf("status = %q, want completed: %s", res.Status, res.Error)
	}
	if len(res.Tasks) != len(ds.Tasks) {
		t.Fatalf("executed %d cells, want %d", len(res.Tasks), len(ds.Tasks))
	}
	for _, tr := range res.Tasks {
		if tr.Attempt == nil {
			t.Fatalf("%s never ran an agent: %s", tr.TaskID, tr.Error)
		}
		if tr.Answer == "" {
			t.Fatalf("%s produced no answer text", tr.TaskID)
		}
	}

	byID := map[string]*TaskResult{}
	for _, tr := range res.Tasks {
		byID[tr.TaskID] = tr
	}

	// The reasoning task needs no third-party package, so it must work
	// everywhere. Its prompt asks for a number and the mock prints exactly that
	// number, so it is the one cell that legitimately passes.
	var reasoning *TaskResult
	for id, tr := range byID {
		if tr.Category == "reasoning" {
			reasoning = tr
			t.Logf("reasoning task = %s", id)
		}
	}
	if reasoning == nil {
		t.Fatalf("the demo dataset no longer has a reasoning task; tasks: %v", idsOf(byID))
	}
	if !reasoning.Passed() {
		t.Fatalf("the reasoning task should pass with the matching mock answer: score=%v error=%q diff=%q",
			reasoning.Score(), reasoning.Error, reasoning.Diff)
	}

	// The patch tasks must fail, because the mock clobbers a file rather than
	// fixing anything. This is the assertion that would catch a grader which
	// always reports success.
	for _, tr := range res.Tasks {
		if tr.Category == "reasoning" {
			continue
		}
		if tr.Passed() {
			t.Fatalf("%s passed with a deliberately wrong patch; the grader is not checking anything", tr.TaskID)
		}
		if tr.Grade == nil || len(tr.Grade.Results) == 0 {
			t.Fatalf("%s produced no grader verdicts", tr.TaskID)
		}
		// A failure has to explain itself, or the report is unreadable.
		var explained bool
		for _, g := range tr.Grade.Results {
			if g.Reason != "" {
				explained = true
			}
		}
		if !explained {
			t.Fatalf("%s failed without any grader stating a reason: %+v", tr.TaskID, tr.Grade.Results)
		}
		if tr.Diff != "" && !strings.Contains(tr.Diff, "@@") {
			t.Fatalf("%s diff does not look like a unified diff:\n%s", tr.TaskID, tr.Diff)
		}

		// The task's output contract scopes what counts as the agent's work.
		// `implement-slugify` deliberately narrows the diff to slugify.py, so an
		// empty diff there is the filter doing its job, not a lost change.
		if out := taskOutput(t, ds, tr.TaskID); out != nil && len(out.Paths) > 0 {
			for _, p := range tr.ChangedPaths {
				if !matchesAny(out.Paths, p) {
					t.Fatalf("%s reported changed path %q, which is outside its output.paths %v",
						tr.TaskID, p, out.Paths)
				}
			}
		}
		if py == "" {
			continue
		}
		// With pytest available the graders must actually run. A grader that
		// could not run is reported as an errored verdict, and that distinction
		// is the whole reason grader_error_rate exists.
		if tr.Status != storage.TaskStatusCompleted {
			t.Fatalf("%s status = %q with pytest available: %s", tr.TaskID, tr.Status, tr.Error)
		}
	}

	// Metrics must agree with the cell-level truth.
	wantRate := float64(res.Passed()) / float64(len(res.Tasks))
	if got, ok := res.Metrics.Number("resolve_rate"); !ok || got != wantRate {
		t.Fatalf("resolve_rate = %v, want %v", got, wantRate)
	}
	if got, ok := res.Metrics.Number("task_count"); !ok || int(got) != len(ds.Tasks) {
		t.Fatalf("task_count = %v, want %d", got, len(ds.Tasks))
	}
	if py != "" {
		if got, _ := res.Metrics.Number("grader_error_rate"); got != 0 {
			t.Fatalf("grader_error_rate = %v, want 0 once pytest is available", got)
		}
	}

	// And the whole thing must have been persisted: a run nobody can read back
	// is not an evaluation.
	rows, err := cfg.Store.TaskRuns(context.Background(), storage.TaskRunsQuery{RunID: "demo-run"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	if len(rows) != len(ds.Tasks) {
		t.Fatalf("stored %d task runs, want %d", len(rows), len(ds.Tasks))
	}
	for _, row := range rows {
		if row.TraceJSON == "" {
			t.Fatalf("%s was stored without a trajectory", row.TaskID)
		}
		if row.RunID != "demo-run" {
			t.Fatalf("%s stored under run %q", row.TaskID, row.RunID)
		}
	}
	if got, _ := cfg.Store.Metrics(context.Background(), "demo-run"); len(got) == 0 {
		t.Fatal("no metrics were stored")
	}
}

// clobberScript is the mock agent's command: it overwrites the file the patch
// tasks are meant to fix, and prints the number the reasoning task asks for.
// One script drives both, so the dataset's own prompts decide the outcome.
const clobberScript = `echo 31; printf 'x\n' > calc.py`

func idsOf(m map[string]*TaskResult) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}

func taskOutput(t *testing.T, ds *dataset.Dataset, taskID string) *dataset.Output {
	t.Helper()
	for _, task := range ds.Tasks {
		if task.ID == taskID {
			return task.Output
		}
	}
	t.Fatalf("task %s not found in the dataset", taskID)
	return nil
}

// matchesAny reports whether path matches one of the globs, using the same
// doublestar semantics the diff filter uses.
func matchesAny(globs []string, path string) bool {
	return sandbox.MatchAny(globs, path)
}

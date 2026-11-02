package regrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// newDataset writes a manifest and the given task lines into a temp directory.
//
// The tasks are written as raw JSON on purpose: the point of these tests is
// what a *dataset on disk* asks for, so going through Go structs would let a
// field the loader cannot parse still test as working.
func newDataset(t *testing.T, tasks ...string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "dataset.yaml"),
		"name: regrade-fixture\nversion: 1.0.0\ntasks_file: tasks.jsonl\n")
	write(t, filepath.Join(dir, "tasks.jsonl"), strings.Join(tasks, "\n")+"\n")
	// A task with no grader is only legal when it carries a baseline, which is
	// exactly the shape `aeval collect` produces -- and the shape the
	// no-grader test below needs to be able to load at all.
	write(t, filepath.Join(dir, "baseline.json"), `{}`)
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// taskLine builds one task; extra is appended verbatim, which is how a test
// asks for graders or for the baseline that makes a gradeless task legal.
func taskLine(id, extra string) string {
	return `{"id":"` + id + `","prompt":"do the thing",` +
		`"workspace":{"type":"files","files":{"a.txt":"x"}},` +
		`"output":{"type":"patch"}` + extra + `}`
}

// seed creates a database holding one run over dsDir and returns the store.
func seed(t *testing.T, dsDir string, attempts ...store.Attempt) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aeval.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if err := st.CreateRun(ctx, store.Run{
		ID: "r1", Name: "fixture", Status: store.RunCompleted,
		DatasetPath: dsDir, DatasetName: "regrade-fixture",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, a := range attempts {
		a.RunID = "r1"
		if a.Status == "" {
			a.Status = store.CellOK
		}
		if err := st.SaveAttempt(ctx, a); err != nil {
			t.Fatalf("save attempt %s: %v", a.TaskID, err)
		}
	}
	return st
}

func cell(t *testing.T, st *store.Store, taskID string, sample int) store.Attempt {
	t.Helper()
	attempts, err := st.ListAttempts(context.Background(), "r1")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	for _, a := range attempts {
		if a.TaskID == taskID && a.Sample == sample {
			return a
		}
	}
	t.Fatalf("no cell %s#%d", taskID, sample)
	return store.Attempt{}
}

// ---------------------------------------------------------------------------
// Reaching a verdict from the row
// ---------------------------------------------------------------------------

func TestOneScoresTheAnswerTheRowRecorded(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"done"}}]`))
	st := seed(t, dsDir, store.Attempt{
		TaskID: "answer", Status: store.CellOK,
		Answer: "done", InputTokens: 100, OutputTokens: 20, CostUSD: 0.25, DurationMs: 1500,
	})

	out, err := One(context.Background(), st, "r1", "answer", -1, Options{})
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !out.Grade.Passed || out.Grade.Score != 1 {
		t.Errorf("verdict = %+v, want a pass", out.Grade)
	}
	if !out.Changed() {
		t.Error("an ungraded cell that gained a pass should report a change")
	}

	got := cell(t, st, "answer", 0)
	if !got.Graded || !got.Passed || got.Score != 1 || got.Threshold != grader.Threshold {
		t.Errorf("row = graded %v passed %v score %v threshold %v", got.Graded, got.Passed, got.Score, got.Threshold)
	}
	// The measurement belongs to a process that has already finished. A
	// re-grade must not touch it, or the cost beside a verdict would be fiction.
	if got.InputTokens != 100 || got.OutputTokens != 20 || got.CostUSD != 0.25 || got.DurationMs != 1500 {
		t.Errorf("re-grading rewrote the measurement: %+v", got)
	}
	if got.Answer != "done" {
		t.Errorf("answer = %q", got.Answer)
	}
}

func TestOneGradesTheStoredDiffWithoutAWorkspace(t *testing.T) {
	dsDir := newDataset(t, taskLine("patchy", `,"graders":[{"type":"patch",`+
		`"config":{"require_changed_paths":["docs/*.md"]}}]`))
	st := seed(t, dsDir, store.Attempt{
		TaskID: "patchy",
		Diff: "diff --git a/docs/replay.md b/docs/replay.md\n--- a/docs/replay.md\n" +
			"+++ b/docs/replay.md\n@@ -1 +1 @@\n-a\n+b\n",
	})

	out, err := One(context.Background(), st, "r1", "patchy", -1, Options{})
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !out.Grade.Passed {
		t.Errorf("verdict = %+v, want a pass from the recorded diff", out.Grade)
	}
	if len(out.Grade.Results) != 1 || out.Grade.Results[0].Type != "patch" {
		t.Fatalf("results = %+v", out.Grade.Results)
	}
}

// A stored trace is what a judge reads the diff and the answer from. The row's
// answer column is the fallback for the case where the trace was dropped to fit
// a payload budget -- and the fallback has to be exercised, because a judge
// handed an empty string would report "nothing to judge" and look like an agent
// that said nothing.
func TestOneFallsBackToTheStoredAnswerWhenTheTraceIsGone(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"31"}}]`))
	st := seed(t, dsDir, store.Attempt{
		TaskID: "answer", Answer: "31",
	})

	out, err := One(context.Background(), st, "r1", "answer", -1, Options{})
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !out.Grade.Passed {
		t.Errorf("verdict = %+v, want a pass from the stored answer", out.Grade)
	}
}

func TestOneGradesTheSampleItWasGiven(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"31"}}]`))
	st := seed(t, dsDir,
		store.Attempt{TaskID: "answer", Sample: 0, Answer: "31"},
		store.Attempt{TaskID: "answer", Sample: 1, Answer: "30"},
	)

	first, err := One(context.Background(), st, "r1", "answer", -1, Options{})
	if err != nil {
		t.Fatalf("One(sample=-1): %v", err)
	}
	if first.Sample != 0 || !first.Grade.Passed {
		t.Errorf("sample -1 graded %d with %+v, want sample 0 and a pass", first.Sample, first.Grade)
	}

	second, err := One(context.Background(), st, "r1", "answer", 1, Options{})
	if err != nil {
		t.Fatalf("One(sample=1): %v", err)
	}
	if second.Sample != 1 || second.Grade.Passed {
		t.Errorf("sample 1 graded %d with %+v, want a failure", second.Sample, second.Grade)
	}
	// The two samples must stay separate rows.
	if a := cell(t, st, "answer", 0); !a.Passed {
		t.Error("grading sample 1 changed sample 0")
	}
}

// ---------------------------------------------------------------------------
// What it refuses, and why the refusal has to be loud
// ---------------------------------------------------------------------------

func TestOneRefusesAGraderThatNeedsAWorkspace(t *testing.T) {
	dsDir := newDataset(t, taskLine("suite", `,"graders":[{"type":"pytest",`+
		`"config":{"paths":["test_a.py"]}}]`))
	st := seed(t, dsDir, store.Attempt{TaskID: "suite", Diff: "diff --git a/a b/a\n"})

	_, err := One(context.Background(), st, "r1", "suite", -1, Options{})
	if err == nil {
		t.Fatal("a pytest grader ran without a workspace")
	}
	for _, want := range []string{"pytest", "workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q: a verdict produced against nothing "+
				"reads as an agent failure", err, want)
		}
	}
	if got := cell(t, st, "suite", 0); got.Graded {
		t.Error("a refused re-grade wrote a verdict onto the row")
	}
}

func TestOneNamesTheTasksTheRunActuallyHolds(t *testing.T) {
	gradeless := `,"baseline":{"file":"baseline.json"}`
	dsDir := newDataset(t, taskLine("alpha", gradeless), taskLine("beta", gradeless))
	st := seed(t, dsDir, store.Attempt{TaskID: "alpha"}, store.Attempt{TaskID: "beta"})

	_, err := One(context.Background(), st, "r1", "gamma", -1, Options{})
	if err == nil {
		t.Fatal("grading an unrecorded task succeeded")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("error %q should list the recorded tasks", err)
	}
}

func TestOneRefusesACellThatNeverRan(t *testing.T) {
	dsDir := newDataset(t, taskLine("broken", `,"graders":[{"type":"patch"}]`))
	st := seed(t, dsDir, store.Attempt{
		TaskID: "broken", Status: store.CellError, Error: "agent: not found",
	})

	_, err := One(context.Background(), st, "r1", "broken", -1, Options{})
	if err == nil {
		t.Fatal("graded a cell whose agent never ran")
	}
	if !strings.Contains(err.Error(), "never ran") {
		t.Errorf("error %q should say why there is nothing to grade", err)
	}
}

func TestOneWithoutAGraderSaysHowToProvideOne(t *testing.T) {
	dsDir := newDataset(t, taskLine("bare", `,"baseline":{"file":"baseline.json"}`))
	st := seed(t, dsDir, store.Attempt{TaskID: "bare", Answer: "done"})

	_, err := One(context.Background(), st, "r1", "bare", -1, Options{})
	if err == nil {
		t.Fatal("graded a task that declares no graders")
	}
	if !strings.Contains(err.Error(), "--grader") {
		t.Errorf("error %q should name the flag that supplies one", err)
	}

	out, err := One(context.Background(), st, "r1", "bare", -1, Options{Graders: []dataset.GraderSpec{
		{Type: "exact_match", Config: map[string]any{"expected": "done"}},
	}})
	if err != nil {
		t.Fatalf("One with an explicit grader: %v", err)
	}
	if !out.Grade.Passed {
		t.Errorf("verdict = %+v", out.Grade)
	}
	// Named the way the loader would name it, so the report labels the result
	// the same on both paths.
	if got := out.Grade.Results[0].Grader; got != "exact_match" {
		t.Errorf("grader label = %q, want exact_match", got)
	}
}

func TestOnePointsAtTheDatasetItCouldNotRead(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"done"}}]`))
	st := seed(t, dsDir, store.Attempt{TaskID: "answer", Answer: "done"})
	if err := os.RemoveAll(dsDir); err != nil {
		t.Fatalf("remove dataset: %v", err)
	}

	_, err := One(context.Background(), st, "r1", "answer", -1, Options{})
	if err == nil {
		t.Fatal("re-graded a task whose dataset is gone")
	}
	if !strings.Contains(err.Error(), dsDir) {
		t.Errorf("error %q should name the path it tried", err)
	}
	if !strings.Contains(err.Error(), "--dataset") {
		t.Errorf("error %q should name the way out", err)
	}
}

func TestOneRejectsAnUnknownGraderType(t *testing.T) {
	dsDir := newDataset(t, taskLine("odd", `,"graders":[{"type":"telepathy"}]`))
	st := seed(t, dsDir, store.Attempt{TaskID: "odd", Answer: "done"})

	_, err := One(context.Background(), st, "r1", "odd", -1, Options{})
	if err == nil {
		t.Fatal("an unknown grader type was accepted")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("error %q should name the type", err)
	}
}

// ---------------------------------------------------------------------------
// Stale state
// ---------------------------------------------------------------------------

// A cell whose grading broke earlier carries "grade: <reason>" in its error
// column. A re-grade that succeeds makes that note false, and a row that reads
// "passed" and "grade: unknown grader type" at once is one nobody can act on.
func TestOneWithdrawsTheNoteAFailedGradeLeftBehind(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"done"}}]`))
	st := seed(t, dsDir, store.Attempt{
		TaskID: "answer", Answer: "done",
		Error: "diff: workspace disappeared; grade: unknown grader type \"telepathy\"",
	})

	if _, err := One(context.Background(), st, "r1", "answer", -1, Options{}); err != nil {
		t.Fatalf("One: %v", err)
	}
	got := cell(t, st, "answer", 0)
	if got.Error != "diff: workspace disappeared" {
		t.Errorf("error = %q, want the grade note gone and the rest kept", got.Error)
	}
}

func TestOneOnAMissingRunNamesTheDatabase(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"baseline":{"file":"baseline.json"}`))
	st := seed(t, dsDir)

	_, err := One(context.Background(), st, "nope", "answer", -1, Options{})
	if err == nil {
		t.Fatal("graded a run that does not exist")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("error %v should match store.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), st.Path()) {
		t.Errorf("error %q should name the database", err)
	}
}

// ---------------------------------------------------------------------------
// What a re-grade grades with
// ---------------------------------------------------------------------------

// A collected task declares nothing, and a caller that presses "grade" on a
// page means "reach a verdict". The fallback exists for exactly that caller:
// without it, the only answer would be a message about a file they may never
// have seen.
func TestOneUsesTheFallbackWhenTheTaskDeclaresNothing(t *testing.T) {
	dsDir := newDataset(t, taskLine("bare", `,"baseline":{"file":"baseline.json"}`))
	st := seed(t, dsDir, store.Attempt{TaskID: "bare", Answer: "done"})

	out, err := One(context.Background(), st, "r1", "bare", -1, Options{Fallback: []dataset.GraderSpec{
		{Type: "exact_match", Config: map[string]any{"expected": "done"}},
	}})
	if err != nil {
		t.Fatalf("One with a fallback: %v", err)
	}
	if !out.Grade.Passed {
		t.Errorf("verdict = %+v, want a pass from the fallback", out.Grade)
	}
	if got := cell(t, st, "bare", 0); !got.Graded || !got.Passed {
		t.Errorf("row = graded %v passed %v, want the verdict written", got.Graded, got.Passed)
	}
}

// The fallback is a fallback: a task that does declare graders must be graded
// by them, or the recorded verdict and the re-grade would answer different
// questions under the same name.
func TestTheDatasetsOwnGradersOutrankTheFallback(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"something else"}}]`))
	st := seed(t, dsDir, store.Attempt{TaskID: "answer", Answer: "done"})

	out, err := One(context.Background(), st, "r1", "answer", -1, Options{Fallback: []dataset.GraderSpec{
		{Type: "exact_match", Config: map[string]any{"expected": "done"}},
	}})
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if out.Before.Graded {
		t.Fatal("the fixture cell already had a verdict")
	}
	if out.Grade.Passed {
		t.Error("the fallback graded a task that declares its own grader")
	}
}

// A judge with no key fails every call, and a grader error is recorded as a
// failed task. That would write a statement about the harness over a row that
// may have been passing -- so the refusal has to come before the write.
//
// The message is part of the behaviour: a refusal that only says "no
// credentials" leaves the reader with a button that cannot work and no
// alternative, when this tool ships another grader that answers the same
// question without calling anyone. Naming patch as the way out is what makes
// the 409 actionable -- and naming llm_judge as one would not be, since that is
// the grader that just failed to authenticate.
func TestOneRefusesAJudgeWithNoCredentialsAndLeavesTheRowAlone(t *testing.T) {
	dsDir := newDataset(t, taskLine("answer", `,"graders":[{"type":"exact_match",`+
		`"config":{"expected":"done"}}]`))
	st := seed(t, dsDir, store.Attempt{TaskID: "answer", Answer: "done"})
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	_, err := One(context.Background(), st, "r1", "answer", -1, Options{Graders: []dataset.GraderSpec{
		{Type: "llm_judge"},
	}})
	if err == nil {
		t.Fatal("graded with a judge that has no credentials")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ANTHROPIC_API_KEY") {
		t.Errorf("error %q should name the variable to set", msg)
	}
	if !strings.Contains(msg, "patch") {
		t.Errorf("error %q should name a grader that reaches a verdict without a model", msg)
	}
	if offline := strings.Join(freeGraders(), ", "); strings.Contains(offline, "llm_judge") {
		t.Errorf("the alternatives offered (%s) include the judge that cannot authenticate", offline)
	}
	if got := cell(t, st, "answer", 0); got.Graded || len(got.Grades) != 0 {
		t.Errorf("row = graded %v grades %+v, want it untouched", got.Graded, got.Grades)
	}
}

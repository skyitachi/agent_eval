package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/metric"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// mockCommand is the mock agent's shell command. It runs inside the sandbox with
// the task prompt on stdin and writes both a file (for patch graders) and a
// line of stdout (for exact_match graders), so one command can drive every
// grader type under test.
const mockCommand = `printf 'done' > out.txt; echo 31`

// writeDataset materialises a dataset directory and loads it through the real
// loader, so the engine is tested against the same spec objects production uses.
func writeDataset(t *testing.T, tasks []string) *dataset.Dataset {
	t.Helper()
	dir := t.TempDir()

	manifest := `name: engine-fixture
version: "1.0.0"
tasks_file: tasks.jsonl
defaults:
  output:
    type: patch
  limits:
    max_turns: 5
    timeout_s: 30
  graders:
    - type: patch
      config:
        require_changed_paths: ["out.txt"]
  workspace:
    type: files
    files:
      README.md: "fixture workspace"
`
	if err := os.WriteFile(filepath.Join(dir, "dataset.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	body := strings.Join(tasks, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write tasks: %v", err)
	}
	ds, err := dataset.Load(dir)
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}
	if err := ds.Validate(); err != nil {
		t.Fatalf("validate dataset: %v", err)
	}
	return ds
}

// threeTasks yields one pass-by-patch task, one pass-by-answer task and one
// failure, giving a mixed outcome without needing a per-task agent.
func threeTasks(t *testing.T) *dataset.Dataset {
	t.Helper()
	return writeDataset(t, []string{
		`{"id":"alpha","category":"bugfix","difficulty":"easy","tags":["smoke"],"prompt":"alpha"}`,
		`{"id":"beta","category":"feature","difficulty":"hard","tags":["smoke","answer"],"prompt":"beta",` +
			`"output":{"type":"stdout"},` +
			`"graders":[{"type":"exact_match","config":{"expected":"31","normalize":["trim"]}}]}`,
		`{"id":"gamma","category":"feature","difficulty":"easy","prompt":"gamma",` +
			`"graders":[{"type":"patch","config":{"require_changed_paths":["never.txt"]}}]}`,
	})
}

// testConfig is a minimal working configuration: the mock agent, the local
// backend and a temporary work root.
func testConfig(t *testing.T) Config {
	t.Helper()
	inherit := false
	return Config{
		Agent: agent.Spec{Kind: agent.KindMock, Bin: mockCommand, Model: "mock-model"},
		Backend: BackendConfig{
			Kind:       BackendLocal,
			InheritEnv: &inherit,
		},
		WorkRoot:    t.TempDir(),
		RunName:     "engine test",
		ToolVersion: "test",
	}
}

func mustEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func taskByID(res *Result, taskID string, sample int) *TaskResult {
	for _, tr := range res.Tasks {
		if tr.TaskID == taskID && tr.SampleIndex == sample {
			return tr
		}
	}
	return nil
}

func taskIDsOf(res *Result) []string {
	out := make([]string, 0, len(res.Tasks))
	for _, tr := range res.Tasks {
		out = append(out, fmt.Sprintf("%s#%d", tr.TaskID, tr.SampleIndex))
	}
	return out
}

// registerOnce registers a test-only runner, tolerating repeated registration so
// that `go test -count=2` does not trip the duplicate check.
func registerOnce(t *testing.T, kind string, factory func() agent.Runner) {
	t.Helper()
	registerMu.Lock()
	defer registerMu.Unlock()
	if registeredOnce[kind] {
		return
	}
	agent.Register(kind, factory)
	registeredOnce[kind] = true
}

var (
	registerMu     sync.Mutex
	registeredOnce = map[string]bool{}
)

// registerGraderOnce registers a test-only grader, idempotently for the same
// reason.
func registerGraderOnce(t *testing.T, g grader.Grader) {
	t.Helper()
	registerMu.Lock()
	defer registerMu.Unlock()
	key := "grader:" + g.Type()
	if registeredOnce[key] {
		return
	}
	grader.Register(g)
	registeredOnce[key] = true
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestRunEvaluatesEveryCellInDeterministicOrder(t *testing.T) {
	cfg := testConfig(t)
	cfg.SamplesPerTask = 2
	e := mustEngine(t, cfg)

	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != storage.StatusCompleted {
		t.Fatalf("status = %q, want %q (error: %s)", res.Status, storage.StatusCompleted, res.Error)
	}
	if len(res.Tasks) != 6 {
		t.Fatalf("got %d cells, want 6", len(res.Tasks))
	}

	// Dataset order outer, sample index inner: two runs of one dataset must plan
	// identically or their reports are not comparable.
	want := []string{
		"alpha#0", "alpha#1", "beta#0", "beta#1", "gamma#0", "gamma#1",
	}
	got := taskIDsOf(res)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("plan order = %v, want %v", got, want)
	}

	if res.ResolveRate() != float64(4)/6 {
		t.Fatalf("resolve rate = %v, want 4/6", res.ResolveRate())
	}
	if res.Passed() != 4 {
		t.Fatalf("passed = %d, want 4", res.Passed())
	}
	if len(res.Failures()) != 2 {
		t.Fatalf("failures = %d, want 2", len(res.Failures()))
	}

	// Each cell reports the classification the report layer groups by.
	alpha := taskByID(res, "alpha", 0)
	if alpha == nil {
		t.Fatal("alpha#0 missing")
	}
	if alpha.Category != "bugfix" || alpha.Difficulty != "easy" || len(alpha.Tags) != 1 {
		t.Fatalf("classification lost: %+v", alpha)
	}
	if !alpha.Passed() || alpha.Score() != 1 {
		t.Fatalf("alpha should pass with score 1: passed=%v score=%v", alpha.Passed(), alpha.Score())
	}
	if alpha.StopReason != "completed" {
		t.Fatalf("alpha stop reason = %q, want completed", alpha.StopReason)
	}
	if alpha.Diff == "" {
		t.Fatal("alpha should have a diff")
	}
	if !strings.Contains(alpha.Diff, "out.txt") {
		t.Fatalf("alpha diff should mention out.txt:\n%s", alpha.Diff)
	}

	// beta passes through the answer path rather than the patch path.
	beta := taskByID(res, "beta", 0)
	if beta == nil || !beta.Passed() {
		t.Fatalf("beta should pass: %+v", beta)
	}
	if strings.TrimSpace(beta.Answer) != "31" {
		t.Fatalf("beta answer = %q, want 31", beta.Answer)
	}

	gamma := taskByID(res, "gamma", 0)
	if gamma == nil || gamma.Passed() {
		t.Fatalf("gamma should fail: %+v", gamma)
	}
	if gamma.Grade == nil || len(gamma.Grade.Results) != 1 {
		t.Fatalf("gamma should have one grader verdict: %+v", gamma.Grade)
	}
}

func TestRunRecordsAttemptAndTrajectory(t *testing.T) {
	e := mustEngine(t, testConfig(t))
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)

	if alpha.Attempt == nil {
		t.Fatal("attempt was not retained; the report layer needs it")
	}
	if alpha.Attempt.Trace == nil {
		t.Fatal("trace was not retained")
	}
	if alpha.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry was needed)", alpha.Attempts)
	}
	if alpha.Attempt.SessionID == "" {
		t.Fatal("session id should be recorded")
	}
	if len(alpha.Attempt.Trace.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(alpha.Attempt.Trace.ToolCalls))
	}
	if alpha.Attempt.Trace.ToolCalls[0].Name != "Bash" {
		t.Fatalf("tool = %q, want Bash", alpha.Attempt.Trace.ToolCalls[0].Name)
	}
}

func TestRunWritesDurableArtifacts(t *testing.T) {
	cfg := testConfig(t)
	// The workspace is removed by default; only the artifact bundle survives.
	e := mustEngine(t, cfg)
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)

	if alpha.ArtifactDir == "" {
		t.Fatal("artifact dir not recorded")
	}
	for _, name := range []string{artifactStream, artifactAttempt, artifactGrade, artifactDiff, artifactAnswer, artifactTask} {
		path := filepath.Join(alpha.ArtifactDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("artifact %s missing: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("artifact %s is empty", name)
		}
	}

	// The raw stream must be re-parseable: that is the whole point of keeping it.
	raw, err := os.ReadFile(alpha.RawStreamPath)
	if err != nil {
		t.Fatalf("read raw stream: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"result"`) {
		t.Fatalf("raw stream does not look like stream-json:\n%s", raw)
	}
	if alpha.RawStreamPath == "" {
		t.Fatal("raw_stream_path not recorded")
	}

	// The workspace must be gone, and WorkspaceDir must not claim otherwise.
	if alpha.WorkspaceDir != "" {
		t.Fatalf("workspace_dir = %q, but the workspace was deleted", alpha.WorkspaceDir)
	}
	cellDir := filepath.Join(cfg.WorkRoot, res.RunID, "tasks", "alpha", "s0")
	if _, err := os.Stat(cellDir); err == nil {
		// Deletion is best-effort: a just-killed git process can briefly hold the
		// directory. If it survived, the engine must say so rather than leaking
		// disk silently.
		var tree []string
		_ = filepath.WalkDir(cellDir, func(p string, d os.DirEntry, err error) error {
			if err == nil {
				tree = append(tree, strings.TrimPrefix(p, cfg.WorkRoot))
			}
			return nil
		})
		if !strings.Contains(alpha.Error, "remove workspace") {
			t.Fatalf("workspace survived removal but no warning was recorded.\n"+
				"  keep_workspaces=%v\n  cell error=%q\n  surviving tree: %v",
				e.Config().KeepWorkspaces, alpha.Error, tree)
		}
		t.Logf("workspace not removed on this run (best-effort cleanup): %s", alpha.Error)
	}
}

func TestRunKeepsWorkspacesWhenAsked(t *testing.T) {
	cfg := testConfig(t)
	cfg.KeepWorkspaces = true
	e := mustEngine(t, cfg)
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)
	if alpha.WorkspaceDir == "" {
		t.Fatal("workspace_dir should be recorded when workspaces are kept")
	}
	if _, err := os.Stat(filepath.Join(alpha.WorkspaceDir, "out.txt")); err != nil {
		t.Fatalf("agent output should still be on disk: %v", err)
	}
	// And the baseline harness directory must not be part of the diff.
	if strings.Contains(alpha.Diff, grader.HarnessDir) {
		t.Fatalf("harness directory leaked into the diff:\n%s", alpha.Diff)
	}
}

// ---------------------------------------------------------------------------
// isolation of failures
// ---------------------------------------------------------------------------

func TestRunIsolatesTaskFailures(t *testing.T) {
	// A task whose grader cannot run at all must not take the run down with it.
	registerGraderOnce(t, &boomGrader{})
	ds := writeDataset(t, []string{
		`{"id":"alpha","prompt":"alpha"}`,
		`{"id":"boom","prompt":"boom","graders":[{"type":"engine_boom"}]}`,
		`{"id":"beta","prompt":"beta"}`,
	})

	e := mustEngine(t, testConfig(t))
	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != storage.StatusCompleted {
		t.Fatalf("run status = %q; a task failure is not a run failure", res.Status)
	}
	if len(res.Tasks) != 3 {
		t.Fatalf("got %d cells, want 3", len(res.Tasks))
	}
	boom := taskByID(res, "boom", 0)
	if boom == nil {
		t.Fatal("boom cell missing")
	}
	if boom.Status != storage.TaskStatusFailed {
		t.Fatalf("boom status = %q, want failed", boom.Status)
	}
	if !strings.Contains(boom.Error, "boom") {
		t.Fatalf("boom error should explain itself: %q", boom.Error)
	}
	if boom.Unwrap() == nil {
		t.Fatal("the underlying error should be retrievable")
	}
	// The healthy tasks are unaffected and still scored.
	if !taskByID(res, "alpha", 0).Passed() || !taskByID(res, "beta", 0).Passed() {
		t.Fatal("healthy tasks should still pass")
	}
}

func TestRunContainsPanics(t *testing.T) {
	registerOnce(t, "engine_panic", func() agent.Runner { return &panicRunner{} })

	cfg := testConfig(t)
	cfg.Agent.Kind = "engine_panic"
	cfg.Agent.Bin = ""
	e := mustEngine(t, cfg)

	ds := writeDataset(t, []string{
		`{"id":"alpha","prompt":"alpha"}`,
		`{"id":"beta","prompt":"beta"}`,
	})
	res, err := e.Run(context.Background(), ds)
	// A recover barrier turns the panic into a recorded failure, not a crash.
	if err != nil {
		t.Fatalf("Run should not fail on a contained panic: %v", err)
	}
	if len(res.Tasks) != 2 {
		t.Fatalf("got %d cells, want 2", len(res.Tasks))
	}
	for _, tr := range res.Tasks {
		if tr.Status != storage.TaskStatusFailed {
			t.Fatalf("%s status = %q, want failed", tr.TaskID, tr.Status)
		}
		if !strings.Contains(tr.Error, "panic") {
			t.Fatalf("%s error = %q, want it to mention the panic", tr.TaskID, tr.Error)
		}
		// A crashed agent implementation is a harness failure, and it must be
		// visible in the aggregate rather than looking like a clean run.
		if tr.StopReason != "harness_error" {
			t.Fatalf("%s stop reason = %q, want harness_error", tr.TaskID, tr.StopReason)
		}
	}
	if got, ok := res.Metrics.Number("harness_error_rate"); !ok || got != 1 {
		t.Fatalf("harness_error_rate = %v, want 1", got)
	}
}

func TestRunRetriesOnlyInfrastructureFailures(t *testing.T) {
	// The runner reports "the process could not be started" for the first two
	// calls, then succeeds. Only wiring failures are retried.
	fr := &flakyRunner{failFirst: 2}
	registerOnce(t, "engine_flaky", func() agent.Runner { return fr })

	cfg := testConfig(t)
	cfg.Agent.Kind = "engine_flaky"
	cfg.Agent.Bin = ""
	cfg.Retries = 3
	cfg.RetryBackoff = time.Millisecond
	e := mustEngine(t, cfg)

	ds := writeDataset(t, []string{`{"id":"alpha","prompt":"alpha"}`})
	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)
	if alpha.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures then success)", alpha.Attempts)
	}
	if alpha.Status == storage.TaskStatusFailed {
		t.Fatalf("the retried cell should have succeeded: %s", alpha.Error)
	}
}

func TestRunGivesUpAfterRetries(t *testing.T) {
	registerOnce(t, "engine_flaky_forever", func() agent.Runner { return &flakyRunner{failFirst: 99} })

	cfg := testConfig(t)
	cfg.Agent.Kind = "engine_flaky_forever"
	cfg.Agent.Bin = ""
	cfg.Retries = 2
	cfg.RetryBackoff = time.Millisecond
	e := mustEngine(t, cfg)

	ds := writeDataset(t, []string{`{"id":"alpha","prompt":"alpha"}`})
	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)
	if alpha.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", alpha.Attempts)
	}
	if alpha.Status != storage.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", alpha.Status)
	}
	if alpha.StopReason != "harness_error" {
		t.Fatalf("stop reason = %q, want harness_error", alpha.StopReason)
	}
	// This is the regression test for the metric that used to be blind to it: the
	// agent never started, so there is no attempt to inspect, and an
	// attempt-derived rate reported a healthy 0.
	if got, ok := res.Metrics.Number("harness_error_rate"); !ok || got != 1 {
		t.Fatalf("harness_error_rate = %v, want 1", got)
	}
	// The agent never ran, so grading must be skipped rather than reporting a
	// zero that looks like a model failure.
	if alpha.Grade != nil {
		t.Fatal("a cell whose agent never started must not be graded")
	}
}

func TestRunDoesNotRetryAgentOutcomes(t *testing.T) {
	// A non-zero exit is a measurement, not a fault: retrying it would hide a
	// real failure and make the reported error rate meaningless.
	cfg := testConfig(t)
	cfg.Agent.Bin = "exit 3"
	cfg.Retries = 3
	cfg.RetryBackoff = time.Millisecond
	e := mustEngine(t, cfg)

	ds := writeDataset(t, []string{`{"id":"alpha","prompt":"alpha"}`})
	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	alpha := taskByID(res, "alpha", 0)
	if alpha.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (agent outcomes are never retried)", alpha.Attempts)
	}
	// The CLI's own error verdict is richer than the raw exit code, so the stop
	// reason comes from the result event rather than from `exit_3`.
	if alpha.StopReason != "agent_error:error_during_execution" {
		t.Fatalf("stop reason = %q, want agent_error:error_during_execution", alpha.StopReason)
	}
	if alpha.Attempt == nil || alpha.Attempt.ExitCode != 3 {
		t.Fatalf("the exit code should still be recorded: %+v", alpha.Attempt)
	}
	// It did run, so it must be graded: an agent that exits non-zero still
	// produced a (empty) patch, and "no patch" is a legitimate failing verdict.
	if alpha.Grade == nil {
		t.Fatal("a cell whose agent ran must still be graded")
	}
	if alpha.Passed() {
		t.Fatal("an agent that produced no patch must not pass a patch grader")
	}
	// The cell completed as an evaluation; the agent's failure shows up as
	// passed=false, not as a harness error.
	if alpha.Status != storage.TaskStatusCompleted {
		t.Fatalf("status = %q: a graded non-zero exit is an outcome, not a harness failure", alpha.Status)
	}
	if got, _ := res.Metrics.Number("harness_error_rate"); got != 0 {
		t.Fatalf("harness_error_rate = %v, want 0", got)
	}
}

func TestRunMarksHarnessFailureSeparatelyFromAgentFailure(t *testing.T) {
	// A grader that cannot run is a harness failure and must be distinguishable
	// from an agent that simply did badly.
	registerGraderOnce(t, &boomGrader{})
	ds := writeDataset(t, []string{
		`{"id":"boom","prompt":"boom","graders":[{"type":"engine_boom"}]}`,
		`{"id":"alpha","prompt":"alpha"}`,
	})
	e := mustEngine(t, testConfig(t))
	res, err := e.Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	boom := taskByID(res, "boom", 0)
	if boom.Status != storage.TaskStatusFailed {
		t.Fatalf("boom status = %q, want failed", boom.Status)
	}
	if boom.Error == "" {
		t.Fatal("the stored error message must not be empty: the report renders it")
	}
	if !strings.Contains(boom.Error, "boom") {
		t.Fatalf("boom error = %q, want it to mention the grader failure", boom.Error)
	}
	// The failing load is visible as a grader error, and the healthy task is
	// still a real measurement rather than being pulled down with it.
	if got, ok := res.Metrics.Number("grader_error_rate"); !ok || got != 0.5 {
		t.Fatalf("grader_error_rate = %v, want 0.5", got)
	}
	if got, _ := res.Metrics.Number("harness_error_rate"); got != 0 {
		t.Fatalf("harness_error_rate = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// concurrency and cancellation
// ---------------------------------------------------------------------------

func TestRunConcurrencyProducesTheSameResults(t *testing.T) {
	serial := testConfig(t)
	serial.Concurrency = 1
	parallel := testConfig(t)
	parallel.Concurrency = 8

	ds1 := threeTasks(t)
	ds2 := threeTasks(t)

	r1, err := mustEngine(t, serial).Run(context.Background(), ds1)
	if err != nil {
		t.Fatalf("serial Run: %v", err)
	}
	r2, err := mustEngine(t, parallel).Run(context.Background(), ds2)
	if err != nil {
		t.Fatalf("parallel Run: %v", err)
	}

	if len(r1.Tasks) != len(r2.Tasks) {
		t.Fatalf("cell counts differ: %d vs %d", len(r1.Tasks), len(r2.Tasks))
	}
	if r1.ResolveRate() != r2.ResolveRate() {
		t.Fatalf("resolve rates differ: %v vs %v", r1.ResolveRate(), r2.ResolveRate())
	}
	if strings.Join(taskIDsOf(r1), ",") != strings.Join(taskIDsOf(r2), ",") {
		t.Fatalf("plan order differs: %v vs %v", taskIDsOf(r1), taskIDsOf(r2))
	}
	if got, _ := r1.Metrics.Number("resolve_rate"); got != 4.0/6.0 {
		t.Fatalf("resolve_rate metric = %v, want 4/6", got)
	}
	if got, _ := r2.Metrics.Number("resolve_rate"); got != 4.0/6.0 {
		t.Fatalf("parallel resolve_rate metric = %v, want 4/6", got)
	}
}

func TestRunGivesEachCellItsOwnWorkspace(t *testing.T) {
	// Cells must not share a workspace: two tasks racing in one directory would
	// corrupt each other's baseline diff.
	cfg := testConfig(t)
	cfg.Concurrency = 4
	cfg.SamplesPerTask = 2
	cfg.KeepWorkspaces = true
	e := mustEngine(t, cfg)

	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := map[string]bool{}
	for _, tr := range res.Tasks {
		if tr.WorkspaceDir == "" {
			t.Fatalf("%s has no workspace dir", tr.TaskID)
		}
		if seen[tr.WorkspaceDir] {
			t.Fatalf("workspace %s shared by more than one cell", tr.WorkspaceDir)
		}
		seen[tr.WorkspaceDir] = true
	}
	if len(seen) != 6 {
		t.Fatalf("got %d distinct workspaces, want 6", len(seen))
	}
}

func TestRunFailFastCancelsRemainingCells(t *testing.T) {
	// A grader that always errors, on the first task, with FailFast on: the
	// later cells should never start.
	registerGraderOnce(t, &boomGrader{})
	ds := writeDataset(t, []string{
		`{"id":"boom","prompt":"boom","graders":[{"type":"engine_boom"}]}`,
		`{"id":"alpha","prompt":"alpha"}`,
		`{"id":"beta","prompt":"beta"}`,
	})

	cfg := testConfig(t)
	cfg.FailFast = true
	cfg.Concurrency = 1
	e := mustEngine(t, cfg)

	res, err := e.Run(context.Background(), ds)
	if err == nil {
		t.Fatal("FailFast should surface the fatal error")
	}
	if res.Status != storage.StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if len(res.Tasks) != 3 {
		t.Fatalf("got %d cells, want 3 (skipped cells still occupy a slot)", len(res.Tasks))
	}
	boom := taskByID(res, "boom", 0)
	if boom.Status != storage.TaskStatusFailed {
		t.Fatalf("boom status = %q, want failed", boom.Status)
	}
	// With concurrency 1 the remaining two cells were never handed out.
	var skipped int
	for _, tr := range res.Tasks {
		if tr.Status == storage.TaskStatusSkipped {
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
}

func TestRunCancellationRecordsPartialResults(t *testing.T) {
	// A runner that blocks until the context is cancelled, so the test controls
	// exactly when the run stops.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	registerOnce(t, "engine_block", func() agent.Runner { return &blockingRunner{release: block} })

	cfg := testConfig(t)
	cfg.Agent.Kind = "engine_block"
	cfg.Agent.Bin = ""
	cfg.Concurrency = 2
	cfg.SamplesPerTask = 2
	e := mustEngine(t, cfg)

	ds := writeDataset(t, []string{
		`{"id":"alpha","prompt":"alpha"}`,
		`{"id":"beta","prompt":"beta"}`,
		`{"id":"gamma","prompt":"gamma"}`,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var res *Result
	var runErr error
	go func() {
		defer close(done)
		res, runErr = e.Run(ctx, ds)
	}()

	// Let the workers pick up their first cells, then pull the plug.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if runErr != nil {
		t.Fatalf("a cancelled run should report through the result, not error: %v", runErr)
	}
	if res == nil {
		t.Fatal("Run returned no result")
	}
	if res.Status != storage.StatusCanceled {
		t.Fatalf("status = %q, want %q", res.Status, storage.StatusCanceled)
	}
	if len(res.Tasks) != 6 {
		t.Fatalf("got %d cells, want 6: cancelled cells must still be accounted for", len(res.Tasks))
	}
	// Metrics over a partial run are still computed and still meaningful.
	if res.Metrics == nil {
		t.Fatal("metrics should be computed even for a cancelled run")
	}
	if got, ok := res.Metrics.Number("sample_count"); !ok || got == 0 {
		t.Fatalf("sample_count = %v, want > 0", got)
	}
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRunPersistsEverythingToTheStore(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	cfg := testConfig(t)
	cfg.Store = store
	cfg.SamplesPerTask = 2
	cfg.RunID = "run-persist"
	e := mustEngine(t, cfg)

	res, err := e.Run(ctx, threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != storage.StatusCompleted || len(res.Tasks) != 6 {
		t.Fatalf("in-memory result disagrees with the store: %s, %d cells", res.Status, len(res.Tasks))
	}

	run, err := store.GetRun(ctx, "run-persist")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != storage.StatusCompleted {
		t.Fatalf("stored status = %q", run.Status)
	}
	if run.DatasetName != "engine-fixture" || run.DatasetHash == "" {
		t.Fatalf("dataset provenance not stored: %+v", run)
	}
	if run.AgentKind != agent.KindMock || run.SandboxBackend != string(BackendLocal) {
		t.Fatalf("agent/backend provenance not stored: %+v", run)
	}
	if run.SamplesPerTask != 2 || run.ToolVersion != "test" {
		t.Fatalf("run shape not stored: %+v", run)
	}
	if run.FinishedAt == nil || run.DurationMs < 0 {
		t.Fatalf("run was not finished: %+v", run)
	}

	rows, err := store.TaskRuns(ctx, storage.TaskRunsQuery{RunID: "run-persist"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("stored %d task runs, want 6", len(rows))
	}
	var completed int
	for _, tr := range rows {
		if tr.Status != storage.TaskStatusCompleted {
			t.Fatalf("%s stored as %q: %s", tr.TaskID, tr.Status, tr.Error)
		}
		if tr.Diff == "" {
			t.Fatalf("%s stored without a diff", tr.TaskID)
		}
		if tr.TraceJSON == "" || tr.AttemptJSON == "" || tr.GradesJSON == "" {
			t.Fatalf("%s stored without replayable blobs", tr.TaskID)
		}
		if tr.RawStreamPath == "" {
			t.Fatalf("%s stored without a raw stream path", tr.TaskID)
		}
		if tr.RunID != "run-persist" {
			t.Fatalf("%s has run_id %q", tr.TaskID, tr.RunID)
		}
		completed++
	}
	if completed != 6 {
		t.Fatalf("completed = %d, want 6", completed)
	}

	// The per-grader and per-tool-call tables are what make "which check fails
	// most" answerable without parsing any JSON.
	grades, err := store.GraderResults(ctx, "run-persist")
	if err != nil {
		t.Fatalf("GraderResults: %v", err)
	}
	if len(grades) != 6 {
		t.Fatalf("stored %d grader rows, want 6", len(grades))
	}
	calls, err := store.ToolCalls(ctx, "run-persist/"+storage.TaskKey("alpha", 0))
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("tool calls not flattened: %+v", calls)
	}

	metrics, err := store.Metrics(ctx, "run-persist")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(metrics) == 0 {
		t.Fatal("no metrics were stored")
	}
	found := map[string]float64{}
	for _, m := range metrics {
		found[m.Name] = m.Value
	}
	if found["resolve_rate"] != float64(4)/6 {
		t.Fatalf("stored resolve_rate = %v, want 4/6", found["resolve_rate"])
	}
	if _, ok := found["task_count"]; !ok {
		t.Fatal("task_count metric missing")
	}

	// The run row snapshot is what listings read.
	list, err := store.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d runs, want 1", len(list))
	}
	if list[0].TaskCount != 6 || list[0].PassedCount != 4 {
		t.Fatalf("summary counts = %d/%d, want 4/6", list[0].PassedCount, list[0].TaskCount)
	}
	if list[0].Metrics["resolve_rate"] != float64(4)/6 {
		t.Fatalf("summary metrics = %v", list[0].Metrics)
	}
}

func TestRunResumeSkipsCompletedCells(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	first := testConfig(t)
	first.Store = store
	first.RunID = "run-resume"
	first.SamplesPerTask = 2
	if _, err := mustEngine(t, first).Run(ctx, threeTasks(t)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Sabotage one finished cell so the second pass has visible work to do.
	done, err := store.CompletedKeys(ctx, "run-resume")
	if err != nil {
		t.Fatalf("CompletedKeys: %v", err)
	}
	if len(done) != 6 {
		t.Fatalf("completed keys = %d, want 6", len(done))
	}

	second := first
	second.Resume = true
	res, err := mustEngine(t, second).Run(ctx, threeTasks(t))
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if len(res.Skipped) != 6 {
		t.Fatalf("skipped = %d, want 6", len(res.Skipped))
	}
	if len(res.Tasks) != 0 {
		t.Fatalf("executed %d cells, want 0: nothing should have been re-run", len(res.Tasks))
	}

	// Now delete one attempt and confirm resume re-runs exactly that cell.
	rows, err := store.TaskRuns(ctx, storage.TaskRunsQuery{RunID: "run-resume"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	var victim storage.TaskRunRecord
	for _, r := range rows {
		if r.TaskID == "alpha" && r.SampleIndex == 0 {
			victim = r
		}
	}
	if victim.ID == "" {
		t.Fatal("could not find alpha#0 to delete")
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM task_runs WHERE id=?`, victim.ID); err != nil {
		t.Fatalf("delete attempt: %v", err)
	}

	res, err = mustEngine(t, second).Run(ctx, threeTasks(t))
	if err != nil {
		t.Fatalf("second resumed Run: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("executed %d cells, want 1", len(res.Tasks))
	}
	if got := res.Tasks[0]; got.TaskID != "alpha" || got.SampleIndex != 0 {
		t.Fatalf("re-ran the wrong cell: %s#%d", got.TaskID, got.SampleIndex)
	}
	if len(res.Skipped) != 5 {
		t.Fatalf("skipped = %d, want 5", len(res.Skipped))
	}
	if !res.Tasks[0].Passed() {
		t.Fatal("the re-run cell should pass")
	}
}

func TestRunWithoutStoreStillProducesResults(t *testing.T) {
	e := mustEngine(t, testConfig(t))
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Metrics == nil || len(res.Metrics) == 0 {
		t.Fatal("metrics must be computed even without a store")
	}
	if res.Summary() == "" {
		t.Fatal("Summary should render")
	}
}

func TestRunConfigRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	cfg := testConfig(t)
	cfg.Store = store
	cfg.RunID = "run-secrets"
	cfg.Agent.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-live-do-not-store"}
	cfg.Backend.Env = map[string]string{"SOME_TOKEN": "hunter2"}
	e := mustEngine(t, cfg)

	if _, err := e.Run(ctx, threeTasks(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := store.GetRun(ctx, "run-secrets")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if strings.Contains(run.ConfigJSON, "sk-live-do-not-store") {
		t.Fatal("an agent API key was written to the run row")
	}
	if strings.Contains(run.ConfigJSON, "hunter2") {
		t.Fatal("a backend token was written to the run row")
	}
	// The key names are still useful provenance.
	if !strings.Contains(run.ConfigJSON, "ANTHROPIC_API_KEY") {
		t.Fatal("the env key names should be kept as provenance")
	}
}

// ---------------------------------------------------------------------------
// events and metrics
// ---------------------------------------------------------------------------

func TestRunEmitsProgressEvents(t *testing.T) {
	cfg := testConfig(t)
	var (
		mu     sync.Mutex
		events []Event
	)
	cfg.OnEvent = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	e := mustEngine(t, cfg)
	if _, err := e.Run(context.Background(), threeTasks(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	runID := e.Config().RunID
	kinds := map[EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
		if ev.Kind != EventRunStart && ev.RunID != runID {
			t.Fatalf("event %v carries run id %q, want %q", ev.Kind, ev.RunID, runID)
		}
	}
	if kinds[EventRunStart] != 1 || kinds[EventRunDone] != 1 {
		t.Fatalf("run boundary events = %+v", kinds)
	}
	if kinds[EventTaskStart] != 3 || kinds[EventTaskDone] != 3 {
		t.Fatalf("task events = %+v, want 3 of each", kinds)
	}

	// Completion counters must be monotonic and end at the plan size.
	var last int
	var doneEvents int
	for _, ev := range events {
		if ev.Kind != EventTaskDone {
			continue
		}
		doneEvents++
		if ev.Completed < last {
			t.Fatalf("completion counter went backwards: %d after %d", ev.Completed, last)
		}
		last = ev.Completed
	}
	if doneEvents != 3 || last != 3 {
		t.Fatalf("completion went 0..%d over %d events, want 0..3 over 3", last, doneEvents)
	}
}

func TestRunComputesCustomMetrics(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomMetrics = []metric.CustomSpec{
		{
			Name:  "smoke_pass_rate",
			Unit:  "ratio",
			Where: "tags contains 'smoke'",
			Agg:   "mean",
			Field: "passed",
		},
		{
			Name: "pass_ratio_vs_baseline",
			Expr: "resolve_rate * 2",
		},
	}
	e := mustEngine(t, cfg)
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// alpha and beta are tagged "smoke" and both pass; gamma is not tagged.
	if got, ok := res.Metrics.Number("smoke_pass_rate"); !ok || got != 1 {
		t.Fatalf("smoke_pass_rate = %v, want 1", got)
	}
	if got, ok := res.Metrics.Number("pass_ratio_vs_baseline"); !ok || got != float64(4)/6*2 {
		t.Fatalf("pass_ratio_vs_baseline = %v, want 4/3", got)
	}
}

func TestNewRejectsInvalidCustomMetric(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomMetrics = []metric.CustomSpec{{Name: "bad", Expr: "1 +"}}
	if _, err := New(cfg); err == nil {
		t.Fatal("New should reject a syntactically broken metric declaration")
	}

	cfg.CustomMetrics = []metric.CustomSpec{{Name: "bad", Where: "field ==", Agg: "mean", Field: "score"}}
	if _, err := New(cfg); err == nil {
		t.Fatal("New should reject a syntactically broken filter")
	}

	cfg.CustomMetrics = nil
	cfg.Metrics = []string{"not_a_real_metric"}
	if _, err := New(cfg); err == nil {
		t.Fatal("New should reject an unknown built-in metric name")
	}
}

func TestRunFailsWhenCustomMetricCannotBeEvaluated(t *testing.T) {
	// The declaration parses but refers to a metric that does not exist, so the
	// failure can only surface at evaluation time. It must fail the run rather
	// than silently omitting the metric the dataset asked for.
	cfg := testConfig(t)
	cfg.CustomMetrics = []metric.CustomSpec{{Name: "derived", Expr: "no_such_metric * 2"}}
	e := mustEngine(t, cfg)

	res, err := e.Run(context.Background(), threeTasks(t))
	if err == nil {
		t.Fatal("a metric that cannot be evaluated must fail the run")
	}
	if res == nil {
		t.Fatal("Run should still return the partial result")
	}
	if res.Status != storage.StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Error, "derived") {
		t.Fatalf("error should name the metric: %q", res.Error)
	}
	// Exactly once. The metrics failure is both recorded on the result and
	// returned as the fatal error, and appending both produces a message that
	// reads like two separate faults.
	if n := strings.Count(res.Error, "unknown metric"); n != 1 {
		t.Fatalf("metrics failure reported %d times, want 1: %q", n, res.Error)
	}
	// The cells were measured before the metric failed, so they are kept.
	if len(res.Tasks) != 3 || res.Passed() != 2 {
		t.Fatalf("results should survive a metric failure: %d cells, %d passed", len(res.Tasks), res.Passed())
	}
}

func TestRunMetricsReflectFailures(t *testing.T) {
	e := mustEngine(t, testConfig(t))
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	checks := map[string]float64{
		"task_count":         3,
		"sample_count":       3,
		"resolve_rate":       float64(2) / 3,
		"timeout_rate":       0,
		"harness_error_rate": 0,
	}
	for name, want := range checks {
		got, ok := res.Metrics.Number(name)
		if !ok {
			t.Fatalf("metric %s missing", name)
		}
		if got != want {
			t.Fatalf("metric %s = %v, want %v", name, got, want)
		}
	}
	// Grouped metrics must carry their breakdown, which is what makes a report
	// explain "which kind of task regressed".
	for _, name := range []string{"resolve_rate_by_category", "stop_reason_mix"} {
		v, ok := res.Metrics[name]
		if !ok {
			t.Fatalf("metric %s missing", name)
		}
		if len(v.Extra) == 0 {
			t.Fatalf("metric %s carries no breakdown: %+v", name, v)
		}
	}
}

// ---------------------------------------------------------------------------
// unit tests
// ---------------------------------------------------------------------------

func TestAgentSpecLayering(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agent.Model = "base-model"
	cfg.Agent.MaxTurns = 10
	cfg.Agent.SystemPrompt = "base system"
	cfg.AgentTimeout = 5 * time.Minute
	e := mustEngine(t, cfg)

	// No task limits: the run defaults apply.
	plain := e.agentSpec(dataset.TaskSpec{ID: "plain"})
	if plain.Model != "base-model" || plain.MaxTurns != 10 {
		t.Fatalf("run defaults not applied: %+v", plain)
	}
	if plain.Timeout != 5*time.Minute {
		t.Fatalf("agent timeout = %v, want 5m", plain.Timeout)
	}

	// Task limits win.
	task := dataset.TaskSpec{
		ID: "limited",
		Limits: &dataset.Limits{
			MaxTurns:   3,
			TimeoutS:   45,
			MaxCostUSD: 1.25,
		},
		SystemPrompt: "task system",
	}
	spec := e.agentSpec(task)
	if spec.MaxTurns != 3 {
		t.Fatalf("MaxTurns = %d, want the task's 3", spec.MaxTurns)
	}
	if spec.Timeout != 45*time.Second {
		t.Fatalf("Timeout = %v, want 45s", spec.Timeout)
	}
	if spec.MaxBudgetUSD != 1.25 {
		t.Fatalf("MaxBudgetUSD = %v, want 1.25", spec.MaxBudgetUSD)
	}
	if spec.SystemPrompt != "base system" {
		t.Fatalf("the run's system prompt was overwritten: %q", spec.SystemPrompt)
	}
	// Both prompts must survive, task last.
	if !strings.Contains(spec.AppendSystemPrompt, "task system") {
		t.Fatalf("task system prompt lost: %q", spec.AppendSystemPrompt)
	}
}

func TestSafeNameBlocksPathTraversal(t *testing.T) {
	tests := map[string]string{
		"django__django-11099": "django__django-11099",
		"a/b/c":                "a-b-c",
		"../../etc/passwd":     "etc-passwd",
		"":                     "unnamed",
		"...":                  "unnamed",
		"has space&sym!":       "has-space-sym",
	}
	for in, want := range tests {
		if got := safeName(in); got != want {
			t.Fatalf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	// The result must never contain a separator, whatever the input.
	for _, in := range []string{"a/b", `a\b`, "../x", "//x//"} {
		if strings.ContainsAny(safeName(in), `/\`) {
			t.Fatalf("safeName(%q) = %q still contains a separator", in, safeName(in))
		}
	}
}

func TestBackendNormalizeRejectsBadConfigurations(t *testing.T) {
	tests := []struct {
		name string
		cfg  BackendConfig
		want string
	}{
		{"unknown backend", BackendConfig{Kind: "kubernetes"}, "unknown backend"},
		{"docker without image", BackendConfig{Kind: BackendDocker}, "requires image"},
		{
			// An image on the local backend is nearly always a forgotten
			// `kind: docker`, and silently ignoring it would run the agent on
			// the host while the user believes it is containerised.
			"image on local backend", BackendConfig{Kind: BackendLocal, Image: "python:3.12"},
			"set backend: docker",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Normalize()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// Defaults.
	local, err := BackendConfig{}.Normalize()
	if err != nil {
		t.Fatalf("empty config should be valid: %v", err)
	}
	if local.Kind != BackendLocal || local.StartTimeout <= 0 {
		t.Fatalf("local defaults = %+v", local)
	}
	docker, err := BackendConfig{Kind: BackendDocker, Image: "img"}.Normalize()
	if err != nil {
		t.Fatalf("docker config should be valid: %v", err)
	}
	if docker.Network != "none" {
		t.Fatalf("docker network = %q, want the hermetic default none", docker.Network)
	}
	if docker.StartTimeout != 2*time.Minute {
		t.Fatalf("docker start timeout = %v, want 2m", docker.StartTimeout)
	}
}

func TestNewRejectsUnknownAgentKind(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agent.Kind = "no-such-agent"
	if _, err := New(cfg); err == nil {
		t.Fatal("New should reject an unregistered agent kind before spending money")
	}
}

func TestRunRejectsEmptyDataset(t *testing.T) {
	e := mustEngine(t, testConfig(t))
	if _, err := e.Run(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil dataset")
	}
	empty := &dataset.Dataset{Manifest: dataset.Manifest{Name: "empty"}}
	if _, err := e.Run(context.Background(), empty); err == nil {
		t.Fatal("expected an error for a dataset with no tasks")
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	resolved, err := cfg.withDefaults()
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if resolved.Agent.Kind != "claude_code" {
		t.Fatalf("agent kind = %q", resolved.Agent.Kind)
	}
	if resolved.SamplesPerTask != 1 || resolved.Concurrency != 1 {
		t.Fatalf("run shape = k=%d c=%d, want 1 and 1", resolved.SamplesPerTask, resolved.Concurrency)
	}
	if resolved.GraderThreshold != grader.Threshold {
		t.Fatalf("threshold = %v, want %v", resolved.GraderThreshold, grader.Threshold)
	}
	if resolved.RunID == "" {
		t.Fatal("a run id should be generated")
	}
	if len(resolved.Metrics) == 0 {
		t.Fatal("default metrics should be populated")
	}
}

func TestCellRecordRoundTripsThroughStorage(t *testing.T) {
	cfg := testConfig(t)
	cfg.RunID = "run-record"
	e := mustEngine(t, cfg)

	now := time.Now().UTC()
	tr := &TaskResult{
		TaskID:      "task-x",
		Category:    "bugfix",
		Difficulty:  "hard",
		Tags:        []string{"a", "b"},
		SampleIndex: 1,
		Status:      storage.TaskStatusCompleted,
		StopReason:  "completed",
		StartedAt:   now.Add(-time.Second),
		EndedAt:     now,
		Diff:        "diff",
		Answer:      "answer",
		Error:       "warning",
		Grade: &grader.Grade{
			TaskID: "task-x", Score: 0.5, Passed: false, Threshold: 0.75,
			Results: []grader.Result{
				{Grader: "patch", Type: "patch", Passed: false, Score: 0.5, Weight: 1, Reason: "why"},
			},
		},
	}
	rec := e.cellRecord(tr)

	if rec.ID != "run-record/task-x#1" {
		t.Fatalf("row id = %q; it must be unique per cell and stable across saves", rec.ID)
	}
	if rec.RunID != "run-record" || rec.TaskID != "task-x" || rec.SampleIndex != 1 {
		t.Fatalf("identity wrong: %+v", rec)
	}
	if rec.Score != 0.5 || rec.Passed {
		t.Fatalf("verdict wrong: %+v", rec)
	}
	// The grade's own threshold must win over the run default, or a re-graded
	// run would report a verdict that never applied.
	if rec.Threshold != 0.75 {
		t.Fatalf("threshold = %v, want the grade's 0.75", rec.Threshold)
	}
	if rec.GradesJSON == "" {
		t.Fatal("grader results were not serialised")
	}
	if !strings.Contains(rec.GradesJSON, `"grader":"patch"`) {
		t.Fatalf("grader blob = %q", rec.GradesJSON)
	}
	if rec.DurationMs != 1000 {
		t.Fatalf("duration_ms = %d, want 1000", rec.DurationMs)
	}
	if len(rec.Tags) != 2 {
		t.Fatalf("tags lost: %v", rec.Tags)
	}
	if rec.Error != "warning" {
		t.Fatalf("error lost: %q", rec.Error)
	}

	// A cell that never ran must produce a row with no attempt/grades blobs
	// rather than JSON "null".
	bare := e.cellRecord(&TaskResult{TaskID: "t", Status: storage.TaskStatusSkipped})
	if bare.AttemptJSON != "" || bare.TraceJSON != "" || bare.GradesJSON != "" {
		t.Fatalf("a bare cell should have empty blobs: %+v", bare)
	}
}

func TestRunCellRecordsMaterializationFailure(t *testing.T) {
	// A cell whose workspace cannot be built must still produce a row, so the
	// failure is visible in the report instead of silently missing from it.
	e := mustEngine(t, testConfig(t))

	tr := e.runCell(context.Background(), &dataset.Dataset{Dir: t.TempDir()}, cell{
		Task: dataset.TaskSpec{
			ID:     "broken",
			Prompt: "x",
			// Files workspaces copy Assets relative to the dataset dir; this one
			// does not exist, so materialization must fail deterministically.
			Workspace: dataset.Workspace{Type: dataset.WorkspaceFiles, Assets: "no-such-assets-dir"},
			Graders:   []dataset.GraderSpec{{Type: "patch"}},
		},
		Key: "broken#0",
	})
	if tr.Status != storage.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tr.Status)
	}
	if !strings.Contains(tr.Error, "materialize") {
		t.Fatalf("error = %q, want it to mention materialization", tr.Error)
	}
	if tr.Grade != nil {
		t.Fatal("a cell that never ran must not be graded")
	}
	if tr.Attempt != nil {
		t.Fatal("no attempt should have been made")
	}
}

// ---------------------------------------------------------------------------
// configuration failures happen before any work
// ---------------------------------------------------------------------------

func TestRunRejectsUnknownGraderTypeBeforeStarting(t *testing.T) {
	ds := writeDataset(t, []string{
		`{"id":"alpha","prompt":"alpha"}`,
		`{"id":"beta","prompt":"beta","graders":[{"type":"no-such-grader"}]}`,
	})
	e := mustEngine(t, testConfig(t))

	_, err := e.Run(context.Background(), ds)
	if err == nil {
		t.Fatal("Run should refuse a dataset naming an unregistered grader")
	}
	if !strings.Contains(err.Error(), "no-such-grader") {
		t.Fatalf("error should name the bad grader: %v", err)
	}
	// The message must point at the task, or the user has to search the dataset.
	if !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error should name the offending task: %v", err)
	}
}

func TestRunRejectsTaskWithoutGraders(t *testing.T) {
	// The loader refuses such a dataset, but Run accepts any *dataset.Dataset, so
	// a programmatically built one must be refused here too. A task with no way
	// to be scored would otherwise fill a report with zeroes that look like model
	// failures.
	ds := &dataset.Dataset{
		Manifest: dataset.Manifest{Name: "hand-built"},
		Dir:      t.TempDir(),
		Tasks: []dataset.TaskSpec{
			{ID: "alpha", Prompt: "alpha", Workspace: dataset.Workspace{Type: dataset.WorkspaceEmpty}},
		},
	}
	_, err := mustEngine(t, testConfig(t)).Run(context.Background(), ds)
	if err == nil {
		t.Fatal("Run should refuse a task with no graders")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("error should name the offending task: %v", err)
	}
}

func TestTaskTimeoutIsReportedAsSuch(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	registerOnce(t, "engine_block_timeout", func() agent.Runner { return &blockingRunner{release: block} })

	cfg := testConfig(t)
	cfg.Agent.Kind = "engine_block_timeout"
	cfg.Agent.Bin = ""
	cfg.TaskTimeout = 250 * time.Millisecond
	e := mustEngine(t, cfg)

	start := time.Now()
	res, err := e.Run(context.Background(), writeDataset(t, []string{`{"id":"alpha","prompt":"alpha"}`}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the task timeout did not bound the cell: took %v", elapsed)
	}

	alpha := taskByID(res, "alpha", 0)
	if alpha.Status != storage.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", alpha.Status)
	}
	// A harness-imposed budget is not a harness bug: counting it as one would
	// make harness_error_rate useless for spotting real infrastructure failures.
	if alpha.StopReason != "task_timeout" {
		t.Fatalf("stop reason = %q, want task_timeout", alpha.StopReason)
	}
	if got, _ := res.Metrics.Number("harness_error_rate"); got != 0 {
		t.Fatalf("harness_error_rate = %v, want 0", got)
	}
	if got, _ := res.Metrics.Number("timeout_rate"); got != 1 {
		t.Fatalf("timeout_rate = %v, want 1", got)
	}
}

func TestDockerBackendFailureIsActionable(t *testing.T) {
	// The Docker backend must fail with a message that says what to do, even when
	// the daemon is unreachable -- and must never silently fall back to running
	// the agent on the host.
	e := mustEngine(t, testConfig(t))
	cfg := e.cfg
	cfg.Backend = BackendConfig{
		Kind:         BackendDocker,
		Image:        "python:3.12-slim",
		DockerBin:    "/nonexistent/docker",
		StartTimeout: time.Second,
		Network:      "none",
	}
	// Build the engine again so the docker backend is the resolved configuration
	// the cell actually uses.
	e = mustEngine(t, cfg)

	tr := e.runCell(context.Background(), &dataset.Dataset{Dir: t.TempDir()}, cell{
		Task: dataset.TaskSpec{
			ID:        "alpha",
			Prompt:    "x",
			Workspace: dataset.Workspace{Type: dataset.WorkspaceFiles},
			Graders:   []dataset.GraderSpec{{Type: "patch"}},
		},
		Key: "alpha#0",
	})
	if tr.Status != storage.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tr.Status)
	}
	if !strings.Contains(tr.Error, "docker") {
		t.Fatalf("error should mention docker: %q", tr.Error)
	}
	if tr.Attempt != nil {
		t.Fatal("the agent must not have run on the host as a fallback")
	}
}

func TestRunResultAccessors(t *testing.T) {
	cfg := testConfig(t)
	cfg.SamplesPerTask = 2
	e := mustEngine(t, cfg)
	res, err := e.Run(context.Background(), threeTasks(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total() != 6 {
		t.Fatalf("Total() = %d, want 6", res.Total())
	}
	if res.ResolveRate() != float64(4)/6 {
		t.Fatalf("ResolveRate() = %v, want 4/6", res.ResolveRate())
	}
	if len(res.Failures()) != 2 {
		t.Fatalf("Failures() = %d, want 2", len(res.Failures()))
	}
	if res.TotalCostUSD() != 0 {
		t.Fatalf("TotalCostUSD() = %v, want 0 for the mock", res.TotalCostUSD())
	}
	summary := res.Summary()
	for _, want := range []string{res.RunID, "engine-fixture", "resolved", "cost"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// boomGrader always fails to run, which is what "grader infrastructure error"
// looks like from the engine's point of view.
type boomGrader struct{}

func (g *boomGrader) Type() string { return "engine_boom" }

func (g *boomGrader) Grade(ctx context.Context, gc *grader.Context, spec dataset.GraderSpec) (*grader.Result, error) {
	return nil, errors.New("boom: grader infrastructure failure")
}

// panicRunner simulates an agent implementation that panics, to prove the
// engine's recover barrier holds the run together.
type panicRunner struct{}

func (r *panicRunner) Name() string { return "engine_panic" }

func (r *panicRunner) Run(ctx context.Context, sb sandbox.Sandbox, spec agent.Spec, prompt string) (*agent.Attempt, error) {
	panic("panicRunner: simulated crash")
}

// flakyRunner returns a spawn error for its first failFirst calls, then defers
// to the mock so the retried cell can actually succeed.
type flakyRunner struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	inner     agent.Runner
}

func (r *flakyRunner) Name() string { return "engine_flaky" }

func (r *flakyRunner) Run(ctx context.Context, sb sandbox.Sandbox, spec agent.Spec, prompt string) (*agent.Attempt, error) {
	r.mu.Lock()
	r.calls++
	fail := r.calls <= r.failFirst
	r.mu.Unlock()
	if fail {
		return nil, errors.New("simulated spawn failure: agent binary not found")
	}
	if r.inner == nil {
		r.inner = &agent.Mock{}
	}
	spec.Bin = mockCommand
	return r.inner.Run(ctx, sb, spec, prompt)
}

// blockingRunner blocks until released, so the test can cancel deterministically.
type blockingRunner struct {
	release <-chan struct{}
}

func (r *blockingRunner) Name() string { return "engine_block" }

func (r *blockingRunner) Run(ctx context.Context, sb sandbox.Sandbox, spec agent.Spec, prompt string) (*agent.Attempt, error) {
	select {
	case <-r.release:
		return nil, errors.New("released")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

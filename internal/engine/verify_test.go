package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// These tests reach for the real loader, like writeDataset does, because the
// reward contract, the oracle and the guards are all *dataset* fields: building
// a TaskSpec by hand would test the engine against a spec no dataset can
// produce.
// ---------------------------------------------------------------------------

func writeDatasetWith(t *testing.T, manifest string, tasks []string) *dataset.Dataset {
	t.Helper()
	dir := t.TempDir()
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

// guardManifest has one agent command that tampers with a file the task cares
// about and writes the output file its grader requires.
const guardManifest = `name: guard-fixture
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
      name: default-work
      config:
        require_changed_paths: ["out.txt"]
  workspace:
    type: files
    files:
      README.md: "fixture workspace"
`

// tamperingAgent writes to guardme.txt as well as to out.txt, so a single
// command exercises both the guard and the grader.
const tamperingAgent = `printf 'tampered' > guardme.txt; printf 'done' > out.txt; echo 31`

func TestRestorePathsResetTheFileBeforeGrading(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"guarded","prompt":"fix it",` +
			`"workspace":{"type":"files","files":{"guardme.txt":"original","README.md":"w"}},` +
			`"restore_paths":["guardme.txt"],` +
			`"graders":[` +
			`{"type":"command","name":"untouched","config":{"command":"grep -q original guardme.txt"}},` +
			`{"type":"patch","name":"did-the-work","config":{"require_changed_paths":["out.txt"]}}` +
			`]}`,
	})

	cfg := testConfig(t)
	cfg.Agent.Bin = tamperingAgent
	res, err := mustEngine(t, cfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := taskByID(res, "guarded", 0)
	if tr == nil {
		t.Fatal("cell missing")
	}

	// The reset is repair, not punishment: the attempt still passes, because
	// rewriting a file the task told it not to touch does not stop it from
	// having done the work.
	if !tr.Passed() {
		t.Errorf("attempt should pass after the guard repaired the file; errors=%s violations=%v", tr.Error, tr.Violations)
	}
	if len(tr.Violations) != 0 {
		t.Errorf("RestorePaths must not fail the attempt, got violations %v", tr.Violations)
	}
	if len(tr.Restored) == 0 || !containsStr(tr.Restored, "guardme.txt") {
		t.Errorf("Restored = %v, want guardme.txt", tr.Restored)
	}
	// A reset file cannot have contributed anything, so it must not appear in
	// the graded diff either.
	if strings.Contains(tr.Diff, "guardme.txt") {
		t.Errorf("restored path leaked into the graded diff:\n%s", tr.Diff)
	}
	if tr.Reward != 1 {
		t.Errorf("reward = %v, want 1 (dense score of a passing attempt)", tr.Reward)
	}
}

func TestForbidChangedPathsRejectsWithoutRepairing(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"forbidden","prompt":"fix it",` +
			`"workspace":{"type":"files","files":{"guardme.txt":"original","README.md":"w"}},` +
			`"forbid_changed_paths":["guardme.txt"],` +
			`"graders":[{"type":"patch","name":"did-the-work","config":{"require_changed_paths":["out.txt"]}}]}`,
	})

	cfg := testConfig(t)
	cfg.Agent.Bin = tamperingAgent
	res, err := mustEngine(t, cfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := taskByID(res, "forbidden", 0)
	if tr == nil {
		t.Fatal("cell missing")
	}

	if len(tr.Violations) == 0 {
		t.Fatal("expected a forbidden_path_changed violation")
	}
	if !strings.Contains(tr.Violations[0], "guardme.txt") {
		t.Errorf("violation = %q, want the offending path", tr.Violations[0])
	}
	// Report, not repair: the graders still passed, and the verdict is failed
	// because of the violation alone.
	if tr.Grade == nil || tr.Grade.Score == 0 {
		t.Errorf("graders should still have run and scored, got %+v", tr.Grade)
	}
	if tr.Passed() {
		t.Error("a rejected attempt must not be reported as passed")
	}
	if tr.Reward != 0 {
		t.Errorf("reward = %v, want 0: a rejected attempt must never be paid", tr.Reward)
	}
	// The change is left in place so the report can show what was done.
	if !strings.Contains(tr.Diff, "guardme.txt") {
		t.Errorf("forbidden change should remain visible in the diff:\n%s", tr.Diff)
	}
}

func TestSparseRewardContract(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		// The composite scores 0.5 but does not pass, so a sparse contract must
		// pay 0 where a dense one pays 0.5.
		`{"id":"sparse","prompt":"fix it",` +
			`"reward":{"type":"pass"},` +
			`"graders":[` +
			`{"type":"patch","name":"did-the-work","weight":1,"config":{"require_changed_paths":["out.txt"]}},` +
			`{"type":"patch","name":"also-this","weight":1,"config":{"require_changed_paths":["never.txt"]}}` +
			`]}`,
		// Same shape without a contract: the default is dense.
		`{"id":"dense","prompt":"fix it",` +
			`"graders":[` +
			`{"type":"patch","name":"did-the-work","weight":1,"config":{"require_changed_paths":["out.txt"]}},` +
			`{"type":"patch","name":"also-this","weight":1,"config":{"require_changed_paths":["never.txt"]}}` +
			`]}`,
	})

	res, err := mustEngine(t, testConfig(t)).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sparse, dense := taskByID(res, "sparse", 0), taskByID(res, "dense", 0)
	if sparse == nil || dense == nil {
		t.Fatal("cells missing")
	}
	if sparse.Score() != 0.5 || dense.Score() != 0.5 {
		t.Fatalf("scores = %v / %v, want 0.5 each", sparse.Score(), dense.Score())
	}
	if sparse.Reward != 0 || sparse.RewardKind != "pass" {
		t.Errorf("sparse reward = %v (%s), want 0 (pass)", sparse.Reward, sparse.RewardKind)
	}
	if dense.Reward != 0.5 || dense.RewardKind != "score" {
		t.Errorf("dense reward = %v (%s), want 0.5 (score)", dense.Reward, dense.RewardKind)
	}
}

func TestOracleAndBaselineModesNeedNoAgent(t *testing.T) {
	// The manifest's default workspace has no solution.txt. A task that supplies
	// it through the oracle is solvable; one that supplies something else is not.
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"solvable","prompt":"add solution.txt",` +
			`"oracle":{"files":{"solution.txt":"present"}},` +
			`"graders":[{"type":"command","name":"has-solution","config":{"command":"grep -q present solution.txt"}}]}`,
		`{"id":"unsolvable","prompt":"add solution.txt",` +
			`"oracle":{"files":{"elsewhere.txt":"present"}},` +
			`"graders":[{"type":"command","name":"has-solution","config":{"command":"grep -q present solution.txt"}}]}`,
		`{"id":"already-solved","prompt":"add solution.txt",` +
			`"workspace":{"type":"files","files":{"solution.txt":"present","README.md":"w"}},` +
			`"oracle":{"files":{"solution.txt":"present"}},` +
			`"graders":[{"type":"command","name":"has-solution","config":{"command":"grep -q present solution.txt"}}]}`,
	})

	// Oracle mode: no agent at all, so the mock agent's bin is irrelevant and the
	// run must not touch it.
	oracleCfg := testConfig(t)
	oracleCfg.Mode = RunModeOracle
	oracleCfg.Agent.Bin = "false" // would fail every cell if it were ever started
	oracleRes, err := mustEngine(t, oracleCfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("oracle Run: %v", err)
	}
	if got := taskByID(oracleRes, "solvable", 0); got == nil || !got.Passed() {
		t.Errorf("solvable task should pass under its own oracle, got %+v", got)
	}
	unsolvable := taskByID(oracleRes, "unsolvable", 0)
	if unsolvable == nil {
		t.Fatal("unsolvable cell missing")
	}
	if unsolvable.Passed() {
		t.Error("a task whose oracle does not pass must be reported as failed")
	}
	if unsolvable.StopReason != "oracle" {
		t.Errorf("stop reason = %q, want oracle", unsolvable.StopReason)
	}
	if oracleRes.Mode != RunModeOracle {
		t.Errorf("result mode = %q, want oracle", oracleRes.Mode)
	}

	// Baseline mode: the untouched workspace is graded as it stands.
	baseCfg := testConfig(t)
	baseCfg.Mode = RunModeBaseline
	baseCfg.Agent.Bin = "false"
	baseRes, err := mustEngine(t, baseCfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("baseline Run: %v", err)
	}
	if got := taskByID(baseRes, "already-solved", 0); got == nil || !got.Passed() {
		t.Errorf("already-solved task should pass untouched, got %+v", got)
	}
	if got := taskByID(baseRes, "solvable", 0); got == nil || got.Passed() {
		t.Errorf("solvable task must not pass untouched, got %+v", got)
	}
	for _, tr := range baseRes.Tasks {
		if tr.StopReason != "baseline" {
			t.Errorf("%s stop reason = %q, want baseline", tr.TaskID, tr.StopReason)
		}
	}
}

func TestOracleFailureIsNotAHarnessFailure(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		// A patch that cannot apply: the dataset's own reference solution is
		// broken, which is a fact about the task and not about the machine.
		`{"id":"broken-oracle","prompt":"x",` +
			`"oracle":{"patch":"--- a/nope.txt\n+++ b/nope.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n"},` +
			`"graders":[{"type":"patch","config":{"require_changed_paths":["out.txt"]}}]}`,
	})
	cfg := testConfig(t)
	cfg.Mode = RunModeOracle
	res, err := mustEngine(t, cfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := taskByID(res, "broken-oracle", 0)
	if tr == nil {
		t.Fatal("cell missing")
	}
	if tr.StopReason != "oracle_error" {
		t.Errorf("stop reason = %q, want oracle_error", tr.StopReason)
	}
	if tr.Status != storage.TaskStatusFailed {
		t.Errorf("status = %q, want failed: the oracle never produced a verdict", tr.Status)
	}
}

func TestOracleModeRequiresAnOracle(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"no-oracle","prompt":"x"}`,
		`{"id":"also-missing","prompt":"x"}`,
	})
	cfg := testConfig(t)
	cfg.Mode = RunModeOracle
	_, err := mustEngine(t, cfg).Run(context.Background(), ds)
	if err == nil {
		t.Fatal("oracle mode should refuse a dataset that declares no oracle")
	}
	// Refusing beats quietly grading the subset that can be graded: the number
	// would still read as the dataset's score. And the message has to name
	// every offender, or the user pays one run per missing oracle to find them.
	for _, id := range []string{"no-oracle", "also-missing"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error should name every task without an oracle, missing %q: %v", id, err)
		}
	}
}

func TestConfigRejectsUnknownMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = "sideways"
	if _, err := New(cfg); err == nil {
		t.Fatal("an unknown mode must be rejected at construction")
	}
}

func TestRunRecordsTheModeOnEveryCell(t *testing.T) {
	ds := threeTasks(t)
	res, err := mustEngine(t, testConfig(t)).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Mode != RunModeAgent {
		t.Errorf("run mode = %q, want agent", res.Mode)
	}
	for _, tr := range res.Tasks {
		if tr.Mode != string(RunModeAgent) {
			t.Errorf("%s mode = %q, want agent", tr.TaskID, tr.Mode)
		}
	}
}

func TestRunStoresModeRewardAndGuards(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"guarded","prompt":"fix it",` +
			`"reward":{"type":"pass"},` +
			`"workspace":{"type":"files","files":{"guardme.txt":"original","README.md":"w"}},` +
			`"forbid_changed_paths":["guardme.txt"],` +
			`"graders":[{"type":"patch","name":"did-the-work","config":{"require_changed_paths":["out.txt"]}}]}`,
	})
	store := newStore(t)
	cfg := testConfig(t)
	cfg.Agent.Bin = tamperingAgent
	cfg.Store = store
	cfg.RunID = "run-guards"

	res, err := mustEngine(t, cfg).Run(context.Background(), ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != storage.StatusCompleted {
		t.Fatalf("status = %s: %s", res.Status, res.Error)
	}

	rec, err := store.GetRun(context.Background(), "run-guards")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if rec.Mode != string(RunModeAgent) {
		t.Errorf("stored mode = %q, want agent", rec.Mode)
	}

	runs, err := store.TaskRuns(context.Background(), storage.TaskRunsQuery{RunID: "run-guards"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d task runs, want 1", len(runs))
	}
	got := runs[0]
	if got.Reward != 0 {
		t.Errorf("stored reward = %v, want 0 (rejected attempt)", got.Reward)
	}
	if len(got.Violations) == 0 {
		t.Errorf("violations did not survive the round trip: %+v", got)
	}
}

func TestModeSurvivesInTheRedactedConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = RunModeBaseline
	e := mustEngine(t, cfg)
	rc := e.redactedConfig()
	if rc["mode"] != string(RunModeBaseline) {
		t.Errorf("redacted config mode = %v, want baseline", rc["mode"])
	}
}

func TestOpenWorkspaceMatchesACellWorkspace(t *testing.T) {
	// The RL environment service builds workspaces through OpenWorkspace while
	// ordinary cells go through runCell. If the two ever diverged, a reward
	// measured through one would not be comparable with the other, so the
	// baseline semantics are pinned here.
	ds := writeDatasetWith(t, guardManifest, []string{`{"id":"alpha","prompt":"alpha"}`})
	e := mustEngine(t, testConfig(t))
	dir := filepath.Join(t.TempDir(), "ws")

	ws, err := e.OpenWorkspace(context.Background(), ds.Tasks[0], ds.Dir, dir)
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	defer ws.Close(context.Background(), true)

	if ws.Baseline == nil || ws.Baseline.SHA == "" {
		t.Fatal("workspace has no baseline commit")
	}
	data, err := ws.Sandbox.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if !strings.Contains(string(data), "fixture workspace") {
		t.Errorf("materialized content = %q", data)
	}
	// A fresh workspace has no changes; that is what makes it a baseline.
	changed, err := ws.Baseline.ChangedPaths(context.Background(), sandbox.DiffOptions{})
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("fresh workspace reports changes: %v", changed)
	}
}

func TestOpenWorkspaceReportsAMissingAsset(t *testing.T) {
	ds := writeDatasetWith(t, guardManifest, []string{
		`{"id":"alpha","prompt":"alpha","workspace":{"type":"files","assets":"does-not-exist"}}`,
	})
	e := mustEngine(t, testConfig(t))
	if _, err := e.OpenWorkspace(context.Background(), ds.Tasks[0], ds.Dir, filepath.Join(t.TempDir(), "ws")); err == nil {
		t.Fatal("a missing asset directory must fail the workspace, not produce an empty one")
	}
}

func TestAgentKindIsResolvable(t *testing.T) {
	// Guards the fixture assumption above: the mock agent accepts a shell line.
	if _, err := agent.New(agent.KindMock); err != nil {
		t.Fatal(err)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

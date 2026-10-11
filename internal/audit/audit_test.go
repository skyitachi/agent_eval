package audit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/storage"
)

const fixtureDir = "../../tests/testdata/audit"

func fixture(t *testing.T) *dataset.Dataset {
	t.Helper()
	ds, err := dataset.Load(fixtureDir)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(ds.Tasks) != 3 {
		t.Fatalf("fixture has %d tasks, want 3", len(ds.Tasks))
	}
	return ds
}

func testConfig(t *testing.T) engine.Config {
	t.Helper()
	inherit := false
	return engine.Config{
		// A binary that does not exist. Nothing in an audit may start the agent,
		// so if any cell tries, it fails loudly instead of quietly costing money.
		Agent: agent.Spec{Kind: agent.KindClaudeCode, Bin: filepath.Join(t.TempDir(), "no-such-agent")},
		Backend: engine.BackendConfig{
			Kind:       engine.BackendLocal,
			InheritEnv: &inherit,
		},
		WorkRoot:    t.TempDir(),
		ToolVersion: "test",
	}
}

func runAudit(t *testing.T, cfg engine.Config, ds *dataset.Dataset, opts Options) *Report {
	t.Helper()
	rep, err := Run(context.Background(), cfg, ds, opts)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	if rep == nil {
		t.Fatal("audit.Run returned no report")
	}
	return rep
}

func taskReport(t *testing.T, rep *Report, id string) TaskReport {
	t.Helper()
	for _, tr := range rep.Tasks {
		if tr.TaskID == id {
			return tr
		}
	}
	t.Fatalf("task %s missing from the report", id)
	return TaskReport{}
}

func TestAuditFindsTheTaskThatCannotBeSolved(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{Repeats: 1, Only: []string{CheckSolvability}})
	tr := taskReport(t, rep, "unsolvable")

	c := tr.Check(CheckSolvability)
	if c == nil {
		t.Fatal("solvability check missing")
	}
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed; detail: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "does not pass") {
		t.Errorf("detail should explain the failure, got %q", c.Detail)
	}
	if tr.Healthy {
		t.Error("a task whose oracle fails is not healthy")
	}
	// The sound task in the same fixture must come out clean, or the check is
	// just reporting that auditing happened.
	sound := taskReport(t, rep, "sound")
	if got := sound.Check(CheckSolvability); got == nil || got.Status != StatusOK {
		t.Errorf("sound task solvability = %+v, want ok", got)
	}
}

func TestAuditFindsTheTaskThatIsAlreadySolved(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{Repeats: 1, Only: []string{CheckNecessity}})
	tr := taskReport(t, rep, "already-solved")

	c := tr.Check(CheckNecessity)
	if c == nil {
		t.Fatal("necessity check missing")
	}
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed; detail: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "reward for doing nothing") {
		t.Errorf("detail should say why free reward is a defect, got %q", c.Detail)
	}
	sound := taskReport(t, rep, "sound")
	if got := sound.Check(CheckNecessity); got == nil || got.Status != StatusOK {
		t.Errorf("sound task necessity = %+v, want ok", got)
	}
}

func TestAuditIsCleanOnASoundDataset(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{Repeats: 2, Only: []string{CheckSolvability, CheckNecessity}})
	// Every task has an oracle in the fixture, so solvability and necessity must
	// both be decided for all three -- and exactly one is defective in each.
	solv := rep.Tally(CheckSolvability)
	if solv[StatusOK] != 2 || solv[StatusFailed] != 1 || solv[StatusUnknown] != 0 {
		t.Errorf("solvability tally = %v", solv)
	}
	necc := rep.Tally(CheckNecessity)
	if necc[StatusOK] != 2 || necc[StatusFailed] != 1 || necc[StatusUnknown] != 0 {
		t.Errorf("necessity tally = %v", necc)
	}
	if rep.Healthy() != 1 {
		t.Errorf("healthy = %d, want 1 (only the sound task)", rep.Healthy())
	}
	if len(rep.Unhealthy()) != 2 {
		t.Errorf("unhealthy = %d, want 2", len(rep.Unhealthy()))
	}
}

func TestAuditLeavesStabilityUnknownWithoutRepeats(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{Repeats: 1, Only: []string{CheckStability}})
	for _, tr := range rep.Tasks {
		c := tr.Check(CheckStability)
		if c == nil {
			t.Fatalf("%s: stability check missing", tr.TaskID)
		}
		if c.Status != StatusUnknown {
			t.Errorf("%s: status = %q, want unknown with one repeat", tr.TaskID, c.Status)
		}
		if !strings.Contains(c.Detail, "--repeats") {
			t.Errorf("%s: detail should say how to fix it, got %q", tr.TaskID, c.Detail)
		}
		if tr.Healthy {
			// Unknown is deliberately not ok: an unverified task is not a
			// verified one, and reporting it healthy would let a check that
			// never ran read as a check that passed.
			t.Errorf("%s: an unchecked task must not be reported healthy", tr.TaskID)
		}
	}
}

func TestAuditStabilityPassesOnDeterministicTasks(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{
		Repeats: 3, Only: []string{CheckStability},
	})
	for _, tr := range rep.Tasks {
		if c := tr.Check(CheckStability); c == nil || c.Status != StatusOK {
			t.Errorf("%s: stability = %+v, want ok", tr.TaskID, c)
		}
	}
}

func TestAuditOnlyNarrowsTheChecks(t *testing.T) {
	rep := runAudit(t, testConfig(t), fixture(t), Options{Repeats: 1, Only: []string{CheckNecessity}})
	for _, tr := range rep.Tasks {
		if tr.Check(CheckSolvability) != nil {
			t.Errorf("%s: solvability ran although it was excluded", tr.TaskID)
		}
		if tr.Check(CheckStability) != nil {
			t.Errorf("%s: stability ran although it was excluded", tr.TaskID)
		}
		if tr.Check(CheckNecessity) == nil {
			t.Errorf("%s: necessity did not run", tr.TaskID)
		}
	}
	// One run only: the baseline. Narrowing must actually skip the work.
	if len(rep.Runs) != 1 || rep.Runs[0].Role != "baseline" {
		t.Errorf("runs = %+v, want just the baseline", rep.Runs)
	}
}

func TestAuditRecordsItsRunsAndTags(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := testConfig(t)
	cfg.Store = store
	rep := runAudit(t, cfg, fixture(t), Options{Group: "grp", Repeats: 2})

	wantRoles := map[string]bool{"baseline": false, "oracle-0": false, "oracle-1": false}
	for _, ref := range rep.Runs {
		if _, ok := wantRoles[ref.Role]; !ok {
			t.Errorf("unexpected run role %q", ref.Role)
			continue
		}
		wantRoles[ref.Role] = true

		rec, err := store.GetRun(context.Background(), ref.RunID)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", ref.RunID, err)
		}
		// The evidence has to be readable months later, which means the mode has
		// to be stored: an oracle run's 100% is not a model score.
		if rec.Mode != "oracle" && rec.Mode != "baseline" {
			t.Errorf("%s: stored mode = %q", ref.RunID, rec.Mode)
		}
		tags, err := store.RunTags(context.Background(), ref.RunID)
		if err != nil {
			t.Fatalf("RunTags: %v", err)
		}
		if tags["audit_group"] != "grp" {
			t.Errorf("%s: audit_group tag = %q", ref.RunID, tags["audit_group"])
		}
		if tags["audit_role"] != ref.Role {
			t.Errorf("%s: audit_role tag = %q, want %q", ref.RunID, tags["audit_role"], ref.Role)
		}
	}
	for role, seen := range wantRoles {
		if !seen {
			t.Errorf("run %q was reported but never executed", role)
		}
	}

	ids, err := store.RunIDsWithTag(context.Background(), "audit_group", "grp", 10)
	if err != nil {
		t.Fatalf("RunIDsWithTag: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("RunIDsWithTag returned %d ids, want 3: %v", len(ids), ids)
	}
}

func TestAuditDoesNotRunTheAgent(t *testing.T) {
	// testConfig points the agent at a path that does not exist. An audit that
	// reached for the agent would fail every cell, so a clean report is the
	// proof that no API budget was touched.
	inherit := false
	cfg := engine.Config{
		Agent: agent.Spec{Kind: agent.KindClaudeCode, Bin: "/definitely/not/here"},
		Backend: engine.BackendConfig{
			Kind:       engine.BackendLocal,
			InheritEnv: &inherit,
		},
		WorkRoot: t.TempDir(),
	}
	rep := runAudit(t, cfg, fixture(t), Options{Repeats: 2})
	for _, tr := range rep.Tasks {
		for _, c := range tr.Checks {
			if c.Status == StatusUnknown && strings.Contains(c.Detail, "agent") {
				t.Errorf("%s/%s: %s", tr.TaskID, c.Name, c.Detail)
			}
		}
	}
	// sound must be fully healthy: if anything had tried to start the agent, its
	// cells would have been harness failures rather than clean verdicts.
	if got := taskReport(t, rep, "sound"); !got.Healthy {
		t.Errorf("sound task is not healthy: %+v", got)
	}
}

func TestAuditRefusesAnEmptyDataset(t *testing.T) {
	ds := &dataset.Dataset{}
	if _, err := Run(context.Background(), testConfig(t), ds, Options{}); err == nil {
		t.Fatal("an empty dataset must be refused")
	}
	if _, err := Run(context.Background(), testConfig(t), nil, Options{}); err == nil {
		t.Fatal("a nil dataset must be refused")
	}
}

func TestAuditReportsATaskWithoutAnOracleAsUnknown(t *testing.T) {
	ds := fixture(t)
	// Strip the oracle from one task: the audit must still be able to run and
	// report, rather than refusing to start the way `aeval run --mode oracle`
	// correctly does.
	for i := range ds.Tasks {
		if ds.Tasks[i].ID == "sound" {
			ds.Tasks[i].Oracle = nil
		}
	}
	rep := runAudit(t, testConfig(t), ds, Options{Repeats: 2, Only: []string{CheckSolvability, CheckNecessity}})

	tr := taskReport(t, rep, "sound")
	c := tr.Check(CheckSolvability)
	if c == nil || c.Status != StatusUnknown {
		t.Fatalf("solvability = %+v, want unknown for a task with no oracle", c)
	}
	if !strings.Contains(c.Detail, "no oracle") {
		t.Errorf("detail = %q", c.Detail)
	}
	// Necessity is unaffected: it never needed an oracle.
	if got := tr.Check(CheckNecessity); got == nil || got.Status != StatusOK {
		t.Errorf("necessity = %+v, want ok", got)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("a partial dataset is not a warning, got %v", rep.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Stability reasoning, exercised directly
//
// The end-to-end tests can only prove that a deterministic task is reported as
// stable. Disagreement needs a grader that misbehaves, and a flaky grader in a
// fixture would make the fixture's own verdicts unreliable -- which is exactly
// the property it is meant to be testing. So the comparison is driven with
// synthetic verdicts instead.
// ---------------------------------------------------------------------------

func verdict(passed bool, graders map[string]bool) *engine.TaskResult {
	g := &grader.Grade{Passed: passed}
	for name, ok := range graders {
		g.Results = append(g.Results, grader.Result{
			Grader: name, Type: "pytest", Passed: ok,
		})
	}
	// Map iteration order is random; sort so the synthetic runs are comparable.
	sortResults(g)
	return &engine.TaskResult{TaskID: "t", Grade: g}
}

func sortResults(g *grader.Grade) {
	for i := 1; i < len(g.Results); i++ {
		for j := i; j > 0 && g.Results[j].Grader < g.Results[j-1].Grader; j-- {
			g.Results[j], g.Results[j-1] = g.Results[j-1], g.Results[j]
		}
	}
}

func runsOf(cells ...*engine.TaskResult) []*childRun {
	out := make([]*childRun, 0, len(cells))
	for i, c := range cells {
		out = append(out, &childRun{
			runID: "run-" + string(rune('a'+i)),
			cells: map[string]*engine.TaskResult{"t": c},
		})
	}
	return out
}

func TestStabilityDetectsAGraderThatFlips(t *testing.T) {
	task := dataset.TaskSpec{ID: "t", Oracle: &dataset.Oracle{Patch: "x"}}
	runs := runsOf(
		verdict(true, map[string]bool{"hidden": true, "lint": true}),
		verdict(true, map[string]bool{"hidden": true, "lint": false}),
	)
	c := stability(task, runs, 2)
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed: %s", c.Status, c.Detail)
	}
	if len(c.Graders) != 1 || c.Graders[0] != "lint[pytest]" {
		t.Errorf("Graders = %v, want the one that moved", c.Graders)
	}
	if !strings.Contains(c.Detail, "flakiness") {
		t.Errorf("detail should name what the number now contains, got %q", c.Detail)
	}
}

func TestStabilityDetectsAFlipInTheOverallVerdict(t *testing.T) {
	task := dataset.TaskSpec{ID: "t", Oracle: &dataset.Oracle{Patch: "x"}}
	runs := runsOf(
		verdict(true, map[string]bool{"hidden": true}),
		verdict(false, map[string]bool{"hidden": false}),
	)
	c := stability(task, runs, 2)
	if c.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "overall verdict") {
		t.Errorf("detail = %q", c.Detail)
	}
}

func TestStabilityIsQuietWhenRepeatsAgree(t *testing.T) {
	task := dataset.TaskSpec{ID: "t", Oracle: &dataset.Oracle{Patch: "x"}}
	runs := runsOf(
		verdict(false, map[string]bool{"hidden": false}),
		verdict(false, map[string]bool{"hidden": false}),
		verdict(false, map[string]bool{"hidden": false}),
	)
	if c := stability(task, runs, 3); c.Status != StatusOK {
		t.Fatalf("status = %q, want ok: %s", c.Status, c.Detail)
	}
}

func TestStabilityCopesWithFewerRunsThanRequested(t *testing.T) {
	task := dataset.TaskSpec{ID: "t", Oracle: &dataset.Oracle{Patch: "x"}}
	runs := runsOf(verdict(true, map[string]bool{"hidden": true}))
	c := stability(task, runs, 3)
	if c.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "only 1 of 3") {
		t.Errorf("detail = %q, want the shortfall spelled out", c.Detail)
	}
}

func TestDifferingGradersNamesTheMissingOne(t *testing.T) {
	// A grader that is gone in the repeat is a difference, not a silent pass.
	forward := differingGraders(
		verdict(true, map[string]bool{"hidden": true}),
		verdict(true, map[string]bool{}),
	)
	if len(forward) != 1 || !strings.Contains(forward[0], "only one run") {
		t.Errorf("differingGraders = %v, want the vanished grader named", forward)
	}
	// ...and so is one that only shows up in the repeat.
	backward := differingGraders(
		verdict(true, map[string]bool{}),
		verdict(true, map[string]bool{"hidden": true}),
	)
	if len(backward) != 1 || !strings.Contains(backward[0], "only one run") {
		t.Errorf("differingGraders = %v, want the new grader named too", backward)
	}
}

func TestDifferingGradersKeepsAnonymousGradersApart(t *testing.T) {
	// Two graders with no configured name must not collapse into one key, or a
	// disagreement between them would be attributed to an anonymous entry.
	g := &grader.Grade{Results: []grader.Result{
		{Type: "pytest", Passed: true},
		{Type: "exact_match", Passed: false},
	}}
	keys := graderVerdicts(g)
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want two distinct entries", keys)
	}
}

func TestFirstFailureExplainsTheVerdict(t *testing.T) {
	checks := []Check{
		{Name: CheckSolvability, Status: StatusOK},
		{Name: CheckNecessity, Status: StatusFailed, Detail: "passes untouched"},
	}
	got := firstFailure(checks)
	if !strings.Contains(got, "necessity") || !strings.Contains(got, "passes untouched") {
		t.Errorf("firstFailure = %q", got)
	}
	if firstFailure([]Check{{Status: StatusOK}}) != "" {
		t.Error("a healthy task needs no reason")
	}
}

func TestWithOracleKeepsTheDatasetHash(t *testing.T) {
	ds := fixture(t)
	ds.Tasks[0].Oracle = nil
	sub := withOracle(ds)
	if len(sub.Dataset.Tasks) != len(ds.Tasks)-1 {
		t.Fatalf("filtered to %d tasks", len(sub.Dataset.Tasks))
	}
	if sub.Dataset.Hash != ds.Hash {
		t.Error("the hash must identify the dataset that was audited, not the subset that was run")
	}
	if hasTask(sub.Dataset, ds.Tasks[0].ID) {
		t.Error("a task without an oracle must not be reported as capable")
	}
	if !hasTask(sub.Dataset, "unsolvable") {
		t.Error("a task with an oracle must be reported as capable")
	}
}

func hasTask(ds *dataset.Dataset, id string) bool {
	for _, t := range ds.Tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

package rlenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const fixtureDir = "../../tests/testdata/rlenv"

// fixedThing is the corrected workspace file: a `subtract` answer.
const fixedThing = `"""Two-argument arithmetic."""


def calc(a, b):
    """Return the difference of a and b."""
    return a - b
`

// scaledThing additionally satisfies the second grader of the `sparse` task.
const scaledThing = `"""Two-argument arithmetic."""


def calc(a, b):
    """Return the difference of a and b."""
    return a - b


def scale(a, k):
    """Return a scaled by k."""
    return a * k
`

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

// baseConfig points the agent at a binary that must never be started. Every
// test that does not call Rollout therefore proves, by not failing, that the
// hand-driven routes do not reach for the agent.
func baseConfig(t *testing.T) engine.Config {
	t.Helper()
	inherit := false
	return engine.Config{
		Agent: agent.Spec{Kind: agent.KindClaudeCode, Bin: filepath.Join(t.TempDir(), "no-such-agent")},
		Backend: engine.BackendConfig{
			Kind:       engine.BackendLocal,
			InheritEnv: &inherit,
		},
		WorkRoot:    t.TempDir(),
		ToolVersion: "test",
	}
}

func newManager(t *testing.T, cfg engine.Config, ds *dataset.Dataset, opts Options) *Manager {
	t.Helper()
	m, err := New(cfg, ds, opts)
	if err != nil {
		t.Fatalf("rlenv.New: %v", err)
	}
	t.Cleanup(func() { m.CloseAll(context.Background()) })
	return m
}

// execManager is a manager with the command route switched on.
func execManager(t *testing.T, ds *dataset.Dataset, opts Options) *Manager {
	t.Helper()
	opts.AllowExec = true
	return newManager(t, baseConfig(t), ds, opts)
}

func mustOpen(t *testing.T, m *Manager, taskID string) *Env {
	t.Helper()
	env, err := m.Open(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Open(%s): %v", taskID, err)
	}
	return env
}

func mustWrite(t *testing.T, m *Manager, id, rel, body string) {
	t.Helper()
	if err := m.WriteFile(context.Background(), id, rel, []byte(body)); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

func mustVerify(t *testing.T, m *Manager, id string) *Result {
	t.Helper()
	res, err := m.Verify(context.Background(), id)
	if err != nil {
		t.Fatalf("Verify(%s): %v", id, err)
	}
	return res
}

// ---------------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------------

func TestNewRejectsAnUnusableDataset(t *testing.T) {
	if _, err := New(baseConfig(t), nil, Options{}); err == nil {
		t.Error("a nil dataset must be refused")
	}
	empty := &dataset.Dataset{}
	if _, err := New(baseConfig(t), empty, Options{}); err == nil {
		t.Error("a dataset with no tasks must be refused")
	}
}

func TestNewFailsUpFrontOnABadConfiguration(t *testing.T) {
	// An unknown grader or metric is a typo a trainer would otherwise discover
	// an hour into a job, one rollout at a time. New runs the engine's own
	// validation so the same mistake surfaces before the first rollout.
	cfg := baseConfig(t)
	cfg.Metrics = []string{"no_such_metric"}
	if _, err := New(cfg, fixture(t), Options{}); err == nil {
		t.Error("New accepted a configuration with an unknown metric")
	}
}

func TestOptionsDefaultsAreBoundNotUnlimited(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})
	got := m.Options()
	if got.MaxEnvs != 8 || got.TTL != 15*time.Minute || got.MaxExecs != 200 {
		t.Errorf("defaults = %+v, want bounded budgets", got)
	}
	if got.AllowExec {
		t.Error("the command route must be off unless it was asked for")
	}
}

// ---------------------------------------------------------------------------
// task projection
// ---------------------------------------------------------------------------

func TestTaskInfoDescribesTheTaskWithoutGivingAwayTheAnswer(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})

	info, ok := m.Task("subtract")
	if !ok {
		t.Fatal("subtract is missing")
	}
	if info.Prompt == "" || info.PromptChars != len(info.Prompt) {
		t.Errorf("prompt = %q (chars %d)", info.Prompt, info.PromptChars)
	}
	if info.RewardKind != "score" {
		t.Errorf("reward kind = %q, want the dense default", info.RewardKind)
	}
	if info.WorkspaceKind != "files" {
		t.Errorf("workspace kind = %q", info.WorkspaceKind)
	}
	if len(info.Graders) != 1 || info.Graders[0] != "subtracts" {
		t.Errorf("graders = %v", info.Graders)
	}
	if info.Guarded {
		t.Error("subtract declares no guards")
	}
	if !info.HasOracle {
		t.Error("subtract declares an oracle, and HasOracle is the signal a trainer audits with")
	}
}

func TestTaskInfoReportsOracleAvailabilityRatherThanTheOracle(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})

	// Only `subtract` declares a reference solution in the fixture, so a
	// service that reported "true" unconditionally would be lying about the
	// other two, and one that shipped the oracle would train a policy to ask
	// for answers.
	withOracle, _ := m.Task("subtract")
	if !withOracle.HasOracle {
		t.Error("subtract has an oracle")
	}
	without, _ := m.Task("sparse")
	if without.HasOracle {
		t.Error("sparse has no oracle, so HasOracle must be false")
	}
	// Nothing in the projection may carry the patch itself.
	raw := without.Prompt + without.RewardKind
	if strings.Contains(raw, "a - b") {
		t.Errorf("the task projection leaked the answer: %q", raw)
	}
}

func TestTaskInfoCarriesTheDensityAndTheGuardFlag(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})

	sparse, _ := m.Task("sparse")
	if sparse.RewardKind != "pass" {
		t.Errorf("sparse reward kind = %q, want pass", sparse.RewardKind)
	}
	tamper, _ := m.Task("tamper")
	if !tamper.Guarded {
		t.Error("a task that forbids paths must report itself as guarded, or its reward reads as approval")
	}
	if _, ok := m.Task("no-such-task"); ok {
		t.Error("an unknown task id must not resolve")
	}
}

func TestTasksKeepsTheDatasetOrder(t *testing.T) {
	ds := fixture(t)
	m := newManager(t, baseConfig(t), ds, Options{})
	got := m.Tasks()
	if len(got) != len(ds.Tasks) {
		t.Fatalf("Tasks returned %d entries, want %d", len(got), len(ds.Tasks))
	}
	for i := range got {
		if got[i].ID != ds.Tasks[i].ID {
			t.Fatalf("position %d = %q, want %q; a trainer iterating twice must see one curriculum", i, got[i].ID, ds.Tasks[i].ID)
		}
	}
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

func TestOpenCapsConcurrentEnvironments(t *testing.T) {
	m := execManager(t, fixture(t), Options{MaxEnvs: 1})

	first := mustOpen(t, m, "subtract")
	_, err := m.Open(context.Background(), "subtract")
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("second Open = %v, want ErrLimit", err)
	}

	// Verifying frees the slot: a verified environment accepts nothing further,
	// so refusing to open a new one would deadlock a sequential trainer.
	mustWrite(t, m, first.ID(), "thing.py", fixedThing)
	mustVerify(t, m, first.ID())

	if _, err := m.Open(context.Background(), "subtract"); err != nil {
		t.Fatalf("Open after verifying = %v, want success", err)
	}
}

func TestOpenRejectsAnUnknownTask(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})
	if _, err := m.Open(context.Background(), "no-such-task"); !errors.Is(err, ErrNoSuchEnv) {
		t.Fatalf("Open = %v, want ErrNoSuchEnv", err)
	}
}

func TestEnvironmentsAreListedOldestFirst(t *testing.T) {
	m := execManager(t, fixture(t), Options{})

	clock := time.Now().UTC()
	m.now = func() time.Time { return clock }
	a := mustOpen(t, m, "subtract")
	clock = clock.Add(time.Second)
	b := mustOpen(t, m, "tamper")

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d environments, want 2", len(list))
	}
	if list[0].ID != a.ID() || list[1].ID != b.ID() {
		t.Errorf("List order = %s, %s", list[0].ID, list[1].ID)
	}
	if list[0].State != EnvOpen {
		t.Errorf("state = %q, want open", list[0].State)
	}
	if list[0].MaxExecs != m.Options().MaxExecs {
		t.Errorf("MaxExecs = %d, want the configured budget", list[0].MaxExecs)
	}
}

func TestWorkspacesLiveUnderTheWorkRoot(t *testing.T) {
	cfg := baseConfig(t)
	m := newManager(t, cfg, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")

	dir := m.Info(env).WorkspaceDir
	if !strings.HasPrefix(dir, cfg.WorkRoot) {
		t.Errorf("workspace dir %q is outside the work root %q", dir, cfg.WorkRoot)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the workspace must exist before it is verified: %v", err)
	}
}

func TestCloseDiscardsAnEnvironmentWithoutGradingIt(t *testing.T) {
	st := tempStore(t)
	cfg := baseConfig(t)
	cfg.Store = st
	m := newManager(t, cfg, fixture(t), Options{AllowExec: true})

	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	if err := m.Close(context.Background(), env.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Get(env.ID()); !errors.Is(err, ErrNoSuchEnv) {
		t.Errorf("Get after Close = %v, want ErrNoSuchEnv", err)
	}
	// Cleaning up is not verifying: a discarded environment must not pay.
	runs, err := st.ListRuns(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("Close wrote %d run(s); discarding is not a verdict", len(runs))
	}
}

func TestTTLReapsEnvironmentsThatOutlivedTheirBudget(t *testing.T) {
	m := execManager(t, fixture(t), Options{TTL: time.Hour})

	base := time.Now().UTC()
	m.now = func() time.Time { return base }
	env := mustOpen(t, m, "subtract")
	dir := m.Info(env).WorkspaceDir

	// Not yet: an environment inside its TTL survives a reap pass.
	m.reapOnce(context.Background())
	if _, err := m.Get(env.ID()); err != nil {
		t.Fatalf("a fresh environment was reaped: %v", err)
	}

	m.now = func() time.Time { return base.Add(2 * time.Hour) }
	m.reapOnce(context.Background())

	if _, err := m.Get(env.ID()); !errors.Is(err, ErrNoSuchEnv) {
		t.Errorf("Get after the TTL = %v, want ErrNoSuchEnv", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("reaping must destroy the workspace, not just forget the id")
	}
	if got := m.Stats().OpenEnvs; got != 0 {
		t.Errorf("open environments = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// the command route
// ---------------------------------------------------------------------------

func TestCommandRouteIsOffUnlessItWasAskedFor(t *testing.T) {
	m := newManager(t, baseConfig(t), fixture(t), Options{})
	env := mustOpen(t, m, "subtract")

	if _, err := m.Exec(context.Background(), env.ID(), "true", 0); !errors.Is(err, ErrExecDisabled) {
		t.Errorf("Exec = %v, want ErrExecDisabled", err)
	}
	if err := m.WriteFile(context.Background(), env.ID(), "thing.py", []byte(fixedThing)); !errors.Is(err, ErrExecDisabled) {
		t.Errorf("WriteFile = %v, want ErrExecDisabled", err)
	}
}

func TestCommandsAreRunAndRecorded(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")

	ok, err := m.Exec(context.Background(), env.ID(), "echo hello", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if ok.Seq != 1 || ok.ExitCode != 0 || !strings.Contains(ok.Output, "hello") {
		t.Errorf("exec log = %+v", ok)
	}

	bad, err := m.Exec(context.Background(), env.ID(), "exit 3", 0)
	if err != nil {
		t.Fatalf("a non-zero exit is a command result, not an error: %v", err)
	}
	if bad.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", bad.ExitCode)
	}

	arch, err := m.Archive(context.Background(), env.ID())
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if len(arch.Execs) != 2 {
		t.Fatalf("archived %d command(s), want 2", len(arch.Execs))
	}
	if arch.Execs[0].Command != "echo hello" || arch.Execs[1].Seq != 2 {
		t.Errorf("commands were not kept in order: %+v", arch.Execs)
	}
}

func TestCommandsAreCapped(t *testing.T) {
	m := execManager(t, fixture(t), Options{MaxExecs: 2})
	env := mustOpen(t, m, "subtract")

	for i := 0; i < 2; i++ {
		if _, err := m.Exec(context.Background(), env.ID(), "true", 0); err != nil {
			t.Fatalf("exec %d: %v", i+1, err)
		}
	}
	_, err := m.Exec(context.Background(), env.ID(), "true", 0)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("exec past the cap = %v, want ErrLimit", err)
	}
	// A capped command is an operational event, so it must not be recorded as
	// something the policy did.
	arch, _ := m.Archive(context.Background(), env.ID())
	if len(arch.Execs) != 2 {
		t.Errorf("recorded %d command(s), want the 2 that ran", len(arch.Execs))
	}
}

func TestCommandOutputIsClipped(t *testing.T) {
	m := execManager(t, fixture(t), Options{MaxOutputBytes: 64})
	env := mustOpen(t, m, "subtract")

	log, err := m.Exec(context.Background(), env.ID(),
		"for i in $(seq 1 40); do echo aaaaaaaaaa; done", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !log.Truncated {
		t.Fatal("output over the cap must be reported as truncated")
	}
	if len(log.Output) > 200 {
		t.Errorf("clipped output is %d bytes, want it near the 64-byte cap", len(log.Output))
	}
	if !strings.Contains(log.Output, "more bytes") {
		t.Errorf("a clipped capture must say how much was dropped, got %q", log.Output)
	}
}

func TestEmptyCommandsAndPathsAreRefused(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")

	if _, err := m.Exec(context.Background(), env.ID(), "   ", 0); err == nil {
		t.Error("an empty command must be refused")
	}
	if err := m.WriteFile(context.Background(), env.ID(), "  ", []byte("x")); err == nil {
		t.Error("an empty path must be refused")
	}
}

// ---------------------------------------------------------------------------
// the reward contract
// ---------------------------------------------------------------------------

func TestHandDrivenEnvironmentEarnsTheDenseReward(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)

	res := mustVerify(t, m, env.ID())
	if res.Reward != 1 || !res.Passed || res.Score != 1 {
		t.Errorf("reward = %v, passed = %v, score = %v; want 1/true/1", res.Reward, res.Passed, res.Score)
	}
	if res.RewardKind != "score" {
		t.Errorf("reward kind = %q, want score", res.RewardKind)
	}
	if res.Rejected || len(res.Violations) != 0 {
		t.Errorf("nothing was tampered with: %+v", res.Violations)
	}
	if res.StopReason != "verified" {
		t.Errorf("stop reason = %q; no agent ran, so it must not borrow the agent vocabulary", res.StopReason)
	}
	if len(res.Graders) != 1 || !res.Graders[0].Passed {
		t.Errorf("graders = %+v", res.Graders)
	}
	if res.EnvID != env.ID() || res.TaskID != "subtract" {
		t.Errorf("result is not attributed: %+v", res)
	}
	if res.Diff == "" {
		t.Error("the diff is the evidence behind the reward; it must be recorded")
	}
}

func TestAnUnsolvedEnvironmentPaysNothing(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	// No write at all: the policy did nothing.
	res := mustVerify(t, m, env.ID())
	if res.Reward != 0 || res.Passed || res.Score != 0 {
		t.Errorf("reward = %v, passed = %v, score = %v; want zero", res.Reward, res.Passed, res.Score)
	}
}

func TestTamperingIsRejectedAndPaysNothing(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "tamper")

	// The fix is applied *and* the verifier is rewritten. The score is perfect;
	// the reward must not be, because the number a trainer learns from has to
	// price tampering out of the environment.
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	mustWrite(t, m, env.ID(), "verifier.py", "def solves(source):\n    return True\n")

	res := mustVerify(t, m, env.ID())
	if !res.Rejected {
		t.Fatal("a forbidden path changed, so the attempt must be marked rejected")
	}
	if res.Reward != 0 {
		t.Errorf("reward = %v, want 0; a rejected attempt must not be paid", res.Reward)
	}
	if !containsSubstring(res.Violations, "verifier.py") {
		t.Errorf("violations = %v, want the path named", res.Violations)
	}
	if res.Passed {
		t.Error("a rejected attempt must not count as passed, however good the diff was")
	}
}

func TestSparseTasksDoNotPayForPartialCredit(t *testing.T) {
	m := execManager(t, fixture(t), Options{})

	// Half of the work: the first grader passes, the second does not.
	half := mustOpen(t, m, "sparse")
	mustWrite(t, m, half.ID(), "thing.py", fixedThing)
	partial := mustVerify(t, m, half.ID())

	if partial.Score <= 0 || partial.Score >= 1 {
		t.Fatalf("score = %v, want a partial score", partial.Score)
	}
	if partial.Passed {
		t.Fatal("the threshold is 1.0, so a partial fix is not a pass")
	}
	if partial.Reward != 0 {
		t.Errorf("reward = %v, want 0: under a sparse contract half a fix pays nothing", partial.Reward)
	}
	if partial.RewardKind != "pass" {
		t.Errorf("reward kind = %q, want pass", partial.RewardKind)
	}

	// The whole job, for contrast, is paid in full.
	full := mustOpen(t, m, "sparse")
	mustWrite(t, m, full.ID(), "thing.py", scaledThing)
	done := mustVerify(t, m, full.ID())
	if done.Reward != 1 || !done.Passed {
		t.Errorf("reward = %v, passed = %v; want 1/true", done.Reward, done.Passed)
	}
}

func TestADenseTaskStillPaysForPartialCredit(t *testing.T) {
	// The same half-fix, read through a dense contract, is worth exactly its
	// score. The two densities are a choice, not a difference in measurement.
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "sparse")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	res := mustVerify(t, m, env.ID())

	if res.RewardKind != "pass" {
		t.Skipf("fixture changed: sparse is %q", res.RewardKind)
	}
	// Compare against the same task declared dense.
	ds := fixture(t)
	for i := range ds.Tasks {
		if ds.Tasks[i].ID == "sparse" {
			ds.Tasks[i].Reward = nil
		}
	}
	dense := execManager(t, ds, Options{})
	d := mustOpen(t, dense, "sparse")
	mustWrite(t, dense, d.ID(), "thing.py", fixedThing)
	dres := mustVerify(t, dense, d.ID())

	if dres.RewardKind != "score" {
		t.Errorf("reward kind = %q, want the dense default", dres.RewardKind)
	}
	if dres.Reward <= 0 || dres.Reward >= 1 {
		t.Errorf("dense reward = %v, want the partial score paid out", dres.Reward)
	}
	if dres.Reward == res.Reward {
		t.Errorf("both contracts paid %v; the density made no difference", dres.Reward)
	}
}

func TestVerificationIsTerminal(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	first := mustVerify(t, m, env.ID())

	// Re-verifying must not re-grade: letting a policy act after seeing the
	// reward is exactly how a verifier gets gamed. The write is expected to
	// fail, so its error is asserted rather than fatal-ed on.
	if err := m.WriteFile(context.Background(), env.ID(), "thing.py",
		[]byte("def calc(a, b):\n    return 0\n")); !errors.Is(err, ErrEnvVerified) {
		t.Fatalf("WriteFile after verify = %v, want ErrEnvVerified", err)
	}
	second, err := m.Verify(context.Background(), env.ID())
	if !errors.Is(err, ErrEnvVerified) {
		t.Fatalf("second Verify = %v, want ErrEnvVerified", err)
	}
	if second == nil || second.Reward != first.Reward {
		t.Errorf("the original verdict must be handed back, got %+v", second)
	}

	if _, err := m.Exec(context.Background(), env.ID(), "true", 0); !errors.Is(err, ErrEnvVerified) {
		t.Errorf("Exec after verify = %v, want ErrEnvVerified", err)
	}
	if err := m.WriteFile(context.Background(), env.ID(), "thing.py", nil); !errors.Is(err, ErrEnvVerified) {
		t.Errorf("WriteFile after verify = %v, want ErrEnvVerified", err)
	}
	if got := m.Info(env).State; got != EnvVerified {
		t.Errorf("state = %q, want verified", got)
	}
	if got := m.Info(env).Result; got == nil || got.Reward != first.Reward {
		t.Errorf("the result must be readable from the environment, got %+v", got)
	}
}

func TestVerificationDestroysTheWorkspace(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	dir := m.Info(env).WorkspaceDir
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	mustVerify(t, m, env.ID())

	if _, err := os.Stat(dir); err == nil {
		t.Error("a verified environment is finished; its workspace must not survive")
	}
}

func TestVerifyingAnUnknownEnvironmentIsRefused(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	if _, err := m.Verify(context.Background(), "env-nope"); !errors.Is(err, ErrNoSuchEnv) {
		t.Fatalf("Verify = %v, want ErrNoSuchEnv", err)
	}
}

func TestClosedEnvironmentsAcceptNothing(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	if err := m.Close(context.Background(), env.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Verify(context.Background(), env.ID()); !errors.Is(err, ErrNoSuchEnv) {
		t.Errorf("Verify after Close = %v, want ErrNoSuchEnv", err)
	}
	if _, err := m.Exec(context.Background(), env.ID(), "true", 0); !errors.Is(err, ErrNoSuchEnv) {
		t.Errorf("Exec after Close = %v, want ErrNoSuchEnv", err)
	}
	if err := m.Close(context.Background(), env.ID()); !errors.Is(err, ErrNoSuchEnv) {
		t.Errorf("double Close = %v, want ErrNoSuchEnv", err)
	}
}

func TestArchiveShowsWhatChangedBeforeTheFilters(t *testing.T) {
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	mustWrite(t, m, env.ID(), "scratch.txt", "not part of the answer")

	arch, err := m.Archive(context.Background(), env.ID())
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !containsSubstring(arch.Changed, "thing.py") || !containsSubstring(arch.Changed, "scratch.txt") {
		t.Errorf("changed = %v; the archive is pre-filter, so a stray write must show up", arch.Changed)
	}
	if arch.Env.TaskID != "subtract" {
		t.Errorf("archived environment = %+v", arch.Env)
	}
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

func TestAVerifiedEnvironmentIsStoredAsAnRLRun(t *testing.T) {
	st := tempStore(t)
	cfg := baseConfig(t)
	cfg.Store = st
	m := newManager(t, cfg, fixture(t), Options{AllowExec: true})

	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	res := mustVerify(t, m, env.ID())

	if res.RunID == "" || res.TaskRunID == "" {
		t.Fatalf("result does not point at its evidence: %+v", res)
	}
	run, err := st.GetRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	// Mode is the difference between "a model did this" and "a harness did
	// this"; losing it makes an external rollout read as a benchmark score.
	if run.Mode != string(RunModeRL) {
		t.Errorf("mode = %q, want %q", run.Mode, RunModeRL)
	}
	if run.DatasetName != "rlenv-fixture" {
		t.Errorf("dataset = %q", run.DatasetName)
	}
	ids, err := st.RunIDsWithTag(context.Background(), "rl_env", env.ID(), 10)
	if err != nil {
		t.Fatalf("RunIDsWithTag: %v", err)
	}
	if len(ids) != 1 || ids[0] != res.RunID {
		t.Errorf("rl_env tag = %v, want the run to be findable by environment", ids)
	}

	trRec, err := st.TaskRun(context.Background(), res.RunID, "subtract", 0)
	if err != nil {
		t.Fatalf("TaskRun: %v", err)
	}
	if trRec.Reward != res.Reward || trRec.TaskID != "subtract" {
		t.Errorf("stored task run = %+v, want the reward %v", trRec, res.Reward)
	}
	// The density has to survive the write as well. A stored reward with no
	// kind is the exact ambiguity the column exists to remove: a later reader
	// cannot tell a sparse contract paying nothing from a wrong answer.
	if trRec.RewardKind != res.RewardKind || trRec.RewardKind == "" {
		t.Errorf("stored reward kind = %q, want %q", trRec.RewardKind, res.RewardKind)
	}
}

func TestPersistenceIsOptionalAndItsFailureIsNotFatal(t *testing.T) {
	// No store at all: the reward is still a reward.
	m := execManager(t, fixture(t), Options{})
	env := mustOpen(t, m, "subtract")
	mustWrite(t, m, env.ID(), "thing.py", fixedThing)
	res := mustVerify(t, m, env.ID())
	if res.Reward != 1 {
		t.Errorf("reward = %v, want 1", res.Reward)
	}
	if res.RunID != "" {
		t.Errorf("RunID = %q, but nothing was persisted", res.RunID)
	}
}

// ---------------------------------------------------------------------------
// rollouts
// ---------------------------------------------------------------------------

// solvingMock is a mock agent that writes the answer into the workspace, which
// is what makes the batch path look like a real policy for the purpose of this
// test. It also echoes something so the stdout path is exercised.
const solvingMock = `printf '"""Two-argument arithmetic."""\n\n\ndef calc(a, b):\n    """Return the difference."""\n    return a - b\n' > thing.py; echo done`

func rolloutConfig(t *testing.T) engine.Config {
	t.Helper()
	cfg := baseConfig(t)
	cfg.Agent = agent.Spec{Kind: agent.KindMock, Bin: solvingMock, Model: "mock"}
	return cfg
}

func TestRolloutRunsTheAgentAndPricesTheResult(t *testing.T) {
	st := tempStore(t)
	cfg := rolloutConfig(t)
	cfg.Store = st
	m := newManager(t, cfg, fixture(t), Options{})

	res, err := m.Rollout(context.Background(), "subtract")
	if err != nil {
		t.Fatalf("Rollout: %v", err)
	}
	if res.Reward != 1 || !res.Passed {
		t.Errorf("reward = %v, passed = %v; want the batch path's verdict", res.Reward, res.Passed)
	}
	if res.StopReason == "" || res.StopReason == "verified" {
		t.Errorf("stop reason = %q; a rollout went through the agent, so it must not claim verification", res.StopReason)
	}
	if res.RunID == "" || res.TaskRunID == "" {
		t.Errorf("a rollout must land in the database: %+v", res)
	}
	run, err := st.GetRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Mode != string(engine.RunModeAgent) {
		t.Errorf("mode = %q, want agent: a rollout is an ordinary run", run.Mode)
	}
	if got := m.Stats().Rollouts; got != 1 {
		t.Errorf("rollouts = %d, want 1", got)
	}
}

func TestRolloutRejectsAnUnknownTask(t *testing.T) {
	m := newManager(t, rolloutConfig(t), fixture(t), Options{})
	if _, err := m.Rollout(context.Background(), "no-such-task"); !errors.Is(err, ErrNoSuchEnv) {
		t.Fatalf("Rollout = %v, want ErrNoSuchEnv", err)
	}
	if got := m.Stats().Rollouts; got != 0 {
		t.Errorf("rollouts = %d; a refused rollout must not be charged", got)
	}
}

func TestRolloutsAreCapped(t *testing.T) {
	m := newManager(t, rolloutConfig(t), fixture(t), Options{MaxRollouts: 1})
	if _, err := m.Rollout(context.Background(), "subtract"); err != nil {
		t.Fatalf("first Rollout: %v", err)
	}
	if _, err := m.Rollout(context.Background(), "subtract"); !errors.Is(err, ErrLimit) {
		t.Fatalf("second Rollout = %v, want ErrLimit", err)
	}
	// The cap is an operational stop, not a task outcome.
	if got := m.Stats(); got.Rollouts != 1 || got.MaxRollouts != 1 {
		t.Errorf("stats = %+v", got)
	}
}

func TestCostCapRefusesTheNextRollout(t *testing.T) {
	m := newManager(t, rolloutConfig(t), fixture(t), Options{MaxCostUSD: 0.5})
	m.mu.Lock()
	m.costUSD = 0.5
	m.mu.Unlock()

	_, err := m.Rollout(context.Background(), "subtract")
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("Rollout = %v, want ErrLimit once the budget is spent", err)
	}
	if !strings.Contains(err.Error(), "0.5000") {
		t.Errorf("err = %q, want the spent and capped amounts reported", err)
	}
}

func TestRolloutBudgetIsReadableForMonitoring(t *testing.T) {
	m := newManager(t, rolloutConfig(t), fixture(t), Options{MaxRollouts: 3, MaxCostUSD: 10})
	stats := m.Stats()
	if stats.MaxRollouts != 3 || stats.MaxCostUSD != 10 {
		t.Errorf("stats = %+v, want the configured caps", stats)
	}
	if stats.ExecEnabled {
		t.Error("exec is off by default")
	}
	if stats.TTLSeconds <= 0 {
		t.Errorf("TTLSeconds = %v", stats.TTLSeconds)
	}
}

// ---------------------------------------------------------------------------
// helpers exercised directly
// ---------------------------------------------------------------------------

func TestClipReportsWhatItDropped(t *testing.T) {
	if got, cut := clip("short", 100); got != "short" || cut {
		t.Errorf("clip = %q, %v", got, cut)
	}
	got, cut := clip(strings.Repeat("x", 100), 10)
	if !cut {
		t.Error("clip must report that it clipped")
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.Contains(got, "90 more bytes") {
		t.Errorf("clip = %q", got)
	}
	if got, cut := clip("anything", 0); got != "anything" || cut {
		t.Errorf("a non-positive bound means unbounded, got %q, %v", got, cut)
	}
}

func TestJoinMsgDoesNotLeaveStraySeparators(t *testing.T) {
	if got := joinMsg("", ""); got != "" {
		t.Errorf("joinMsg = %q, want empty", got)
	}
	if got := joinMsg("", "b"); got != "b" {
		t.Errorf("joinMsg = %q, want b", got)
	}
	if got := joinMsg("a", ""); got != "a" {
		t.Errorf("joinMsg = %q, want a", got)
	}
	if got := joinMsg("a", "b"); got != "a; b" {
		t.Errorf("joinMsg = %q, want a; b", got)
	}
}

func TestTaskPromptPrefersTheResolvedText(t *testing.T) {
	if got := taskPrompt(dataset.TaskSpec{Prompt: "raw"}); got != "raw" {
		t.Errorf("taskPrompt = %q", got)
	}
	if got := taskPrompt(dataset.TaskSpec{Prompt: "raw", ResolvedPrompt: "file-backed"}); got != "file-backed" {
		t.Errorf("taskPrompt = %q, want the resolved prompt", got)
	}
}

func TestRunModeRLIsItsOwnMode(t *testing.T) {
	// A fourth mode alongside agent/oracle/baseline: reading an externally
	// driven rollout as a benchmark score would be a category error, and the
	// stored mode is what stops that reading. If rl ever collapsed into one of
	// the other three, the distinction would be gone and nothing would say so.
	modes := map[engine.RunMode]string{
		engine.RunModeAgent:    "agent",
		engine.RunModeOracle:   "oracle",
		engine.RunModeBaseline: "baseline",
		RunModeRL:              "rl",
	}
	if len(modes) != 4 {
		t.Fatalf("the four run modes collapsed into %d distinct value(s)", len(modes))
	}
	if string(RunModeRL) != "rl" {
		t.Errorf("RunModeRL = %q, want rl", RunModeRL)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func tempStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "rlenv.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func containsSubstring(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

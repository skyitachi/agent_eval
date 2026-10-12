package attrib

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

func TestSignTail(t *testing.T) {
	cases := []struct {
		n, k int
		want float64
	}{
		// P(X <= 0) for n fair coins is 2^-n.
		{8, 0, 1.0 / 256},
		{6, 0, 1.0 / 64},
		{4, 0, 1.0 / 16},
		{10, 0, 1.0 / 1024},
		// P(X <= 1) = 6/32 for n = 5.
		{5, 1, 6.0 / 32},
		// P(X <= 2) = 16/32, the exact middle of the distribution.
		{5, 2, 0.5},
		{3, 0, 1.0 / 8},
		{3, 3, 1},
	}
	for _, tc := range cases {
		if got := signTail(tc.n, tc.k); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("signTail(%d, %d) = %v, want %v", tc.n, tc.k, got, tc.want)
		}
	}
	// A split of six against nothing out of six is significant; five against
	// one is not, and that boundary is what a reader will lean on.
	if p := 2 * signTail(6, 0); p >= Alpha {
		t.Errorf("6-0 in 6 pairs gives p=%v, expected below alpha", p)
	}
	if p := 2 * signTail(6, 1); p < Alpha {
		t.Errorf("5-1 in 6 pairs gives p=%v, expected above alpha", p)
	}
	// The log-space sum has to stay sane at sample counts where 0.5^n underflows
	// and the binomial coefficient overflows. 900 of 2000 is 4.5 standard
	// deviations below the mean, whose one-sided tail is a few parts per million.
	if got := signTail(2000, 900); got <= 0 || got > 1e-5 {
		t.Errorf("signTail(2000, 900) = %v, want a small positive tail", got)
	}
	if got := signTail(2000, 2000); got != 1 {
		t.Errorf("signTail(2000, 2000) = %v, want 1", got)
	}
}

func TestSignTailRejectsImpossibleInput(t *testing.T) {
	if got := signTail(4, -1); got != 0 {
		t.Errorf("signTail(4, -1) = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Attribution
// ---------------------------------------------------------------------------

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

const testHash = "hash-of-the-dataset"

func addRun(t *testing.T, st *store.Store, id, sweep, level string, meta Meta, hash string) {
	t.Helper()
	blob, err := meta.Encode()
	if err != nil {
		t.Fatalf("encode meta: %v", err)
	}
	ctx := context.Background()
	err = st.CreateRun(ctx, store.Run{
		ID: id, Name: meta.Name, Status: store.RunRunning,
		StartedAt: time.Now().UTC(), DatasetName: "demo", DatasetHash: hash,
		Samples: 1, FactorAxis: string(meta.Axis), FactorLevel: level, SweepID: sweep,
		ConfigJSON: blob,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if err := st.FinishRun(ctx, id, store.RunCompleted, ""); err != nil {
		t.Fatalf("finish run %s: %v", id, err)
	}
}

// cell describes one attempt in the fixture.
type cell struct {
	task    string
	graded  bool
	passed  bool
	tokens  int
	ms      int64
	status  string
	costUSD float64
}

func addCells(t *testing.T, st *store.Store, runID string, cells []cell) {
	t.Helper()
	ctx := context.Background()
	for i, c := range cells {
		status := c.status
		if status == "" {
			status = store.CellOK
		}
		a := store.Attempt{
			RunID: runID, TaskID: c.task, Sample: 0,
			Status: status, Graded: c.graded, Passed: c.passed,
			Score: 1, DurationMs: c.ms, CostUSD: c.costUSD,
			OutputTokens: c.tokens,
		}
		if !c.graded {
			a.Score = 0
		}
		if err := st.SaveAttempt(ctx, a); err != nil {
			t.Fatalf("save attempt %s#%d: %v", c.task, i, err)
		}
	}
}

func tasks(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("t%02d", i+1)
	}
	return out
}

func TestAttributePairedDeltas(t *testing.T) {
	st := newStore(t)
	meta := Meta{Name: "model-sweep", Axis: AxisModel, Control: "sonnet"}
	addRun(t, st, "run-sonnet", "sw1", "sonnet", meta, testHash)
	addRun(t, st, "run-haiku", "sw1", "haiku", meta, testHash)

	ids := tasks(10)
	var control, level []cell
	for i, id := range ids {
		// Control gets four of ten right; the level gets all ten. Six pairs move,
		// which is the smallest split the exact test can call significant.
		control = append(control, cell{task: id, graded: true, passed: i < 4, tokens: 1000, ms: 5000})
		level = append(level, cell{task: id, graded: true, passed: true, tokens: 700, ms: 6000})
	}
	addCells(t, st, "run-sonnet", control)
	addCells(t, st, "run-haiku", level)

	att, err := Attribute(context.Background(), st, "sw1")
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if att.Axis != AxisModel || att.Control != "sonnet" || att.Name != "model-sweep" {
		t.Errorf("attribution identity = %+v", att)
	}
	if att.DatasetDrift {
		t.Error("the two levels ran the same dataset but drift was reported")
	}
	if len(att.Comparisons) != 1 {
		t.Fatalf("got %d comparisons, want 1", len(att.Comparisons))
	}
	cmp := att.Comparisons[0]
	if cmp.Level != "haiku" || cmp.Common != 10 {
		t.Fatalf("comparison = %+v", cmp)
	}
	if len(cmp.Fixed) != 6 {
		t.Errorf("fixed = %d tasks, want 6", len(cmp.Fixed))
	}
	if len(cmp.Broke) != 0 {
		t.Errorf("broke = %v, want none", cmp.Broke)
	}

	stat := func(key string) PairedStat {
		t.Helper()
		for _, s := range cmp.Stats {
			if s.Metric == key {
				return s
			}
		}
		t.Fatalf("no stat for %s", key)
		return PairedStat{}
	}

	pass := stat(MetricPassRate)
	if pass.N != 10 || pass.Wins != 6 || pass.Losses != 0 || pass.Ties != 4 {
		t.Errorf("pass rate split = %+v", pass)
	}
	if pass.MeanDelta != 60 {
		t.Errorf("pass rate mean delta = %v, want 60 percentage points", pass.MeanDelta)
	}
	if !pass.Testable || math.Abs(pass.P-0.03125) > 1e-9 {
		t.Errorf("pass rate p = %v (testable=%v), want 0.03125", pass.P, pass.Testable)
	}
	if pass.ControlMean != 40 || pass.LevelMean != 100 {
		t.Errorf("pass rate means = %v vs %v, want 40 vs 100", pass.ControlMean, pass.LevelMean)
	}

	tok := stat(MetricTokens)
	if tok.Wins != 10 || tok.Losses != 0 {
		t.Errorf("token split = %+v", tok)
	}
	if tok.MeanDelta != -300 {
		t.Errorf("token mean delta = %v, want -300", tok.MeanDelta)
	}
	if tok.MedianDelta != -300 {
		t.Errorf("token median delta = %v", tok.MedianDelta)
	}
	if !tok.LowerIsBetter {
		t.Error("tokens are marked as better when higher")
	}
	if tok.P > 0.002 {
		t.Errorf("10-0 in 10 pairs gives p=%v", tok.P)
	}

	// Time went the other way: the level is slower, so it must lose on a metric
	// where lower is better even though its delta is positive.
	dur := stat(MetricDuration)
	if dur.Wins != 0 || dur.Losses != 10 {
		t.Errorf("duration split = %+v", dur)
	}

	if !pass.Primary {
		t.Error("the correctness metric is not marked primary")
	}
	if len(att.Findings) == 0 {
		t.Fatal("no findings")
	}
	// Correctness is ranked first even though tokens produced a smaller p, so a
	// reader cannot see a saving without seeing what it cost.
	if att.Findings[0].Metric != MetricPassRate {
		t.Errorf("the first finding is about %s, want the primary metric", att.Findings[0].Metric)
	}
	if att.Findings[0].Direction != "better" {
		t.Errorf("pass rate direction = %q", att.Findings[0].Direction)
	}
	var tokenFinding, timeFinding *Finding
	for i := range att.Findings {
		switch att.Findings[i].Metric {
		case MetricTokens:
			tokenFinding = &att.Findings[i]
		case MetricDuration:
			timeFinding = &att.Findings[i]
		}
	}
	if tokenFinding == nil || tokenFinding.Direction != "better" {
		t.Errorf("token finding = %+v, want better", tokenFinding)
	}
	// The verb has to match the direction; "improved" for a slowdown is the bug
	// this guards.
	if timeFinding == nil || timeFinding.Direction != "worse" || !strings.Contains(timeFinding.Text, "slower") {
		t.Errorf("time finding = %+v, want worse and phrased as slower", timeFinding)
	}
	if !strings.Contains(tokenFinding.Text, "fewer tokens") {
		t.Errorf("token finding does not read as a saving: %q", tokenFinding.Text)
	}

	if len(att.Caveats) == 0 {
		t.Fatal("no caveats were attached")
	}
	joined := strings.Join(att.Caveats, " | ")
	for _, want := range []string{"one axis at a time", "sign test", "once per level"} {
		if !strings.Contains(joined, want) {
			t.Errorf("caveats do not mention %q: %v", want, att.Caveats)
		}
	}
}

func TestAttributeExcludesUngradedTasksFromCorrectness(t *testing.T) {
	st := newStore(t)
	meta := Meta{Axis: AxisModel, Control: "a", Name: "s"}
	addRun(t, st, "r-a", "sw", "a", meta, testHash)
	addRun(t, st, "r-b", "sw", "b", meta, testHash)

	// t01 is collected rather than graded: it has tokens and a duration, so it is
	// measurable, but there is nothing to score it against.
	addCells(t, st, "r-a", []cell{
		{task: "t01", graded: false, tokens: 100, ms: 10},
		{task: "t02", graded: true, passed: true, tokens: 100, ms: 10},
	})
	addCells(t, st, "r-b", []cell{
		{task: "t01", graded: false, tokens: 50, ms: 5},
		{task: "t02", graded: true, passed: true, tokens: 50, ms: 5},
	})

	att, err := Attribute(context.Background(), st, "sw")
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	cmp := att.Comparisons[0]
	for _, s := range cmp.Stats {
		if s.Metric == MetricPassRate && s.N != 1 {
			t.Errorf("pass rate paired %d tasks, want only the graded one", s.N)
		}
		if s.Metric == MetricTokens && s.N != 2 {
			t.Errorf("tokens paired %d tasks, want both", s.N)
		}
	}
	for _, p := range cmp.Pairs {
		if p.TaskID == "t01" && p.Verdict != "not_graded" {
			t.Errorf("the collected task was given the verdict %q", p.Verdict)
		}
	}
	// A level with an ungraded task is still measured; it just has no pass rate
	// on that one, which is not the same as failing it.
	for _, l := range att.Levels {
		if l.Summary.Ungraded != 1 {
			t.Errorf("level %s reported %d ungraded cells, want 1", l.Name, l.Summary.Ungraded)
		}
	}
}

func TestAttributeSaturatedDatasetIsNotTestable(t *testing.T) {
	st := newStore(t)
	meta := Meta{Axis: AxisHarness, Control: "plain", Name: "s"}
	addRun(t, st, "r-1", "sw", "plain", meta, testHash)
	addRun(t, st, "r-2", "sw", "fancy", meta, testHash)
	var cells []cell
	for _, id := range tasks(5) {
		cells = append(cells, cell{task: id, graded: true, passed: true, tokens: 10, ms: 1})
	}
	addCells(t, st, "r-1", cells)
	addCells(t, st, "r-2", cells)

	att, err := Attribute(context.Background(), st, "sw")
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	for _, s := range att.Comparisons[0].Stats {
		if s.Metric != MetricPassRate {
			continue
		}
		if s.Testable {
			t.Error("a saturated metric was reported as testable")
		}
		if s.P != 1 {
			t.Errorf("untestable metric has p=%v, want 1", s.P)
		}
		if s.Ties != 5 || s.Wins != 0 || s.Losses != 0 {
			t.Errorf("split = %+v", s)
		}
	}
	var found bool
	for _, f := range att.Findings {
		if f.Metric == MetricPassRate {
			found = true
			if f.Direction != "no_clear_difference" {
				t.Errorf("direction = %q", f.Direction)
			}
			if !strings.Contains(f.Text, "cannot separate") {
				t.Errorf("text does not say the dataset cannot separate them: %q", f.Text)
			}
		}
	}
	if !found {
		t.Error("no finding was produced for the saturated metric")
	}
}

func TestAttributeRefusesWhatItCannotCompare(t *testing.T) {
	t.Run("no metadata", func(t *testing.T) {
		st := newStore(t)
		ctx := context.Background()
		// A plain run has no sweep metadata; asking to attribute it must fail
		// rather than pick a control at random.
		if err := st.CreateRun(ctx, store.Run{
			ID: "r", Status: store.RunRunning, StartedAt: time.Now().UTC(),
			FactorAxis: string(AxisModel), FactorLevel: "a", SweepID: "sw",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := Attribute(ctx, st, "sw"); err == nil || !strings.Contains(err.Error(), "control") {
			t.Fatalf("err = %v, want a complaint about the missing control", err)
		}
	})

	t.Run("unknown sweep", func(t *testing.T) {
		st := newStore(t)
		if _, err := Attribute(context.Background(), st, "nope"); err == nil {
			t.Fatal("attributing a sweep with no runs succeeded")
		}
	})

	t.Run("duplicate level", func(t *testing.T) {
		st := newStore(t)
		meta := Meta{Axis: AxisModel, Control: "a", Name: "s"}
		addRun(t, st, "r1", "sw", "a", meta, testHash)
		addRun(t, st, "r2", "sw", "b", meta, testHash)
		addRun(t, st, "r3", "sw", "b", meta, testHash)
		_, err := Attribute(context.Background(), st, "sw")
		if err == nil || !strings.Contains(err.Error(), "two runs for level") {
			t.Fatalf("err = %v, want a complaint about the repeated level", err)
		}
	})

	t.Run("missing control level", func(t *testing.T) {
		st := newStore(t)
		meta := Meta{Axis: AxisModel, Control: "a", Name: "s"}
		addRun(t, st, "r1", "sw", "b", meta, testHash)
		_, err := Attribute(context.Background(), st, "sw")
		if err == nil || !strings.Contains(err.Error(), "no run for its control") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("level with no name", func(t *testing.T) {
		st := newStore(t)
		meta := Meta{Axis: AxisModel, Control: "a", Name: "s"}
		addRun(t, st, "r1", "sw", "a", meta, testHash)
		addRun(t, st, "r2", "sw", "", meta, testHash)
		_, err := Attribute(context.Background(), st, "sw")
		if err == nil || !strings.Contains(err.Error(), "names no factor level") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAttributeDetectsDatasetDrift(t *testing.T) {
	st := newStore(t)
	meta := Meta{Axis: AxisModel, Control: "a", Name: "s"}
	addRun(t, st, "r1", "sw", "a", meta, testHash)
	addRun(t, st, "r2", "sw", "b", meta, "a-different-hash")
	addCells(t, st, "r1", []cell{{task: "t01", graded: true, passed: true}})
	addCells(t, st, "r2", []cell{{task: "t01", graded: true, passed: false}})

	att, err := Attribute(context.Background(), st, "sw")
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if !att.DatasetDrift {
		t.Fatal("two levels ran different datasets and drift was not reported")
	}
	if !strings.Contains(strings.Join(att.Caveats, " "), "DID NOT RUN THE SAME DATASET") {
		t.Errorf("the caveat does not shout: %v", att.Caveats)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	m := Meta{
		Name:    "sweep",
		Axis:    AxisSkill,
		Control: "none",
		Notes:   "why",
		Changed: []string{"plugin_dirs"},
	}
	blob, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := DecodeMeta(blob)
	if !ok {
		t.Fatal("metadata did not decode")
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("round trip = %+v, want %+v", got, m)
	}
	if len(got.Changed) != 1 || got.Changed[0] != "plugin_dirs" {
		t.Errorf("changed did not survive the round trip: %v", got.Changed)
	}
	for _, junk := range []string{"", "{}", "null", "not json", `{"axis":"skill"}`} {
		if _, ok := DecodeMeta(junk); ok {
			t.Errorf("DecodeMeta(%q) claimed to find sweep metadata", junk)
		}
	}
}

// TestAttributeCarriesEachLevelsChangedKeys guards the evidence for the
// orthogonality claim.
//
// A sweep's whole value is that it moved one thing, and the reader can only
// check that if each level says what it varied. The list travels in the run's
// own metadata rather than the sweep's, because levels do not vary the same
// keys -- so reading it once for the whole sweep would give every level the
// control's empty answer.
func TestAttributeCarriesEachLevelsChangedKeys(t *testing.T) {
	st := newStore(t)
	base := Meta{Name: "skill-sweep", Axis: AxisSkill, Control: "no-skills"}
	addRun(t, st, "run-none", "sw2", "no-skills", base, testHash)
	withSkills := base
	withSkills.Changed = []string{"plugin_dirs"}
	addRun(t, st, "run-skills", "sw2", "with-skills", withSkills, testHash)

	for _, id := range tasks(3) {
		addCells(t, st, "run-none", []cell{{task: id, graded: true, passed: false}})
		addCells(t, st, "run-skills", []cell{{task: id, graded: true, passed: true}})
	}

	att, err := Attribute(context.Background(), st, "sw2")
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	byName := map[string]LevelResult{}
	for _, l := range att.Levels {
		byName[l.Name] = l
	}
	if len(byName) != 2 {
		t.Fatalf("levels = %v, want two", byName)
	}
	if got := byName["with-skills"].Changed; len(got) != 1 || got[0] != "plugin_dirs" {
		t.Errorf("with-skills changed = %v, want [plugin_dirs]", got)
	}
	if got := byName["no-skills"].Changed; len(got) != 0 {
		t.Errorf("the control varies nothing, but reports %v", got)
	}
}

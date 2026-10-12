package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/attrib"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/store"
	"github.com/skyitachi/agenteval/internal/trace"
)

// ---------------------------------------------------------------------------
// Fixtures
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

type runOpts struct {
	id, name, dataset string
	status            string
	model             string
	samples           int
	hash              string
	factor            [2]string // axis, level
	sweep             string
	config            string
	notes             string
}

func addRun(t *testing.T, st *store.Store, o runOpts) {
	t.Helper()
	if o.status == "" {
		o.status = store.RunCompleted
	}
	if o.hash == "" {
		o.hash = "hash-1"
	}
	if o.samples == 0 {
		o.samples = 1
	}
	ctx := context.Background()
	started := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	err := st.CreateRun(ctx, store.Run{
		ID: o.id, Name: o.name, Status: store.RunRunning, StartedAt: started,
		DatasetName: o.dataset, DatasetHash: o.hash,
		AgentKind: "claude_code", AgentModel: o.model, Backend: "local",
		Samples: o.samples, Concurrency: 1,
		FactorAxis: o.factor[0], FactorLevel: o.factor[1], SweepID: o.sweep,
		Notes: o.notes, ConfigJSON: o.config,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", o.id, err)
	}
	if err := st.FinishRun(ctx, o.id, o.status, ""); err != nil {
		t.Fatalf("finish run %s: %v", o.id, err)
	}
}

type cellOpts struct {
	task     string
	sample   int
	status   string
	category string
	graded   bool
	passed   bool
	score    float64
	tokens   int
	ms       int64
	cost     float64
	turns    int
	tools    int
	stop     string
	answer   string
	diff     string
	grades   []grader.Result
	events   []trace.Event
	calls    []trace.ToolCall
	base     *store.BaselineComparison
}

func addCell(t *testing.T, st *store.Store, runID string, o cellOpts) {
	t.Helper()
	status := o.status
	if status == "" {
		status = store.CellOK
	}
	a := store.Attempt{
		RunID: runID, TaskID: o.task, Sample: o.sample, Category: o.category,
		Status: status, StopReason: o.stop,
		Graded: o.graded, Passed: o.passed, Score: o.score, Threshold: 1,
		DurationMs: o.ms, CostUSD: o.cost, OutputTokens: o.tokens,
		Turns: o.turns, ToolCalls: o.tools,
		Answer: o.answer, Diff: o.diff, Grades: o.grades,
	}
	if o.base != nil {
		a.Baseline = *o.base
	}
	if len(o.events) > 0 || len(o.calls) > 0 {
		raw, err := json.Marshal(trace.Trace{Events: o.events, ToolCalls: o.calls})
		if err != nil {
			t.Fatal(err)
		}
		a.Trace = raw
	}
	if err := st.SaveAttempt(context.Background(), a); err != nil {
		t.Fatalf("save cell %s#%d: %v", o.task, o.sample, err)
	}
}

func mustLoad(t *testing.T, st *store.Store, runID string, opts Options) *Data {
	t.Helper()
	d, err := Load(context.Background(), st, runID, opts)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return d
}

func metricByKey(t *testing.T, d *Data, key string) Metric {
	t.Helper()
	for _, m := range d.Metrics {
		if m.Key == key {
			return m
		}
	}
	t.Fatalf("no metric %q in %v", key, d.Metrics)
	return Metric{}
}

func taskByID(t *testing.T, d *Data, id string) Task {
	t.Helper()
	for _, x := range d.Tasks {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("no task %q in the payload", id)
	return Task{}
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

func TestLoadProjectsTheRunFaithfully(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", name: "demo", dataset: "fixtures", model: "sonnet", notes: "why"})
	addCell(t, st, "r1", cellOpts{
		task: "a", category: "bugfix", graded: true, passed: true, score: 1,
		tokens: 100, ms: 1000, cost: 0.25, turns: 3, tools: 2, stop: "completed",
		answer: "done",
		grades: []grader.Result{{Grader: "patch", Type: "patch", Passed: true, Score: 1, Weight: 1}},
	})
	addCell(t, st, "r1", cellOpts{
		task: "b", category: "feature", graded: true, passed: false, score: 0,
		tokens: 300, ms: 2000, cost: 0.75, turns: 5, tools: 4, stop: "completed",
	})
	addCell(t, st, "r1", cellOpts{task: "c", category: "feature", graded: false, tokens: 10, ms: 100})

	d := mustLoad(t, st, "r1", Options{})

	if d.Version != DataVersion {
		t.Errorf("version = %d, want %d", d.Version, DataVersion)
	}
	if d.Run.ID != "r1" || d.Run.Name != "demo" || d.Run.Notes != "why" {
		t.Errorf("run = %+v", d.Run)
	}
	if d.Run.StartedAt != "2026-03-01T10:00:00Z" {
		t.Errorf("started_at = %q", d.Run.StartedAt)
	}
	if d.Counts.Cells != 3 || d.Counts.Tasks != 3 {
		t.Errorf("counts = %+v", d.Counts)
	}
	if d.Counts.Graded != 2 || d.Counts.Passed != 1 || d.Counts.Failed != 1 {
		t.Errorf("graded tally = %+v", d.Counts)
	}
	// The third cell was measured and had nothing to score it against. Reporting
	// it as a failure would invent a verdict.
	if d.Counts.Ungraded != 1 {
		t.Errorf("ungraded = %d, want 1", d.Counts.Ungraded)
	}

	pr := metricByKey(t, d, "pass_rate")
	if pr.Display != "50.0%" {
		t.Errorf("pass rate = %q, want 50.0%%", pr.Display)
	}
	if pr.Direction != "up" {
		t.Errorf("pass rate direction = %q", pr.Direction)
	}
	tok := metricByKey(t, d, "tokens")
	if tok.Direction != "down" {
		t.Errorf("token direction = %q, want down: fewer tokens is better", tok.Direction)
	}
	if d.Run.Summary.Tokens != 410 {
		t.Errorf("summary tokens = %d, want 410", d.Run.Summary.Tokens)
	}

	// Failures sort first: the reader's first question is what went wrong.
	if d.Tasks[0].ID != "b" {
		t.Errorf("first task = %q, want the failing one", d.Tasks[0].ID)
	}
	last := d.Tasks[len(d.Tasks)-1]
	if last.ID != "a" {
		t.Errorf("last task = %q, want the passing one", last.ID)
	}
	b := taskByID(t, d, "b")
	if b.Status != store.CellOK || !b.Graded || b.Passed {
		t.Errorf("task b = %+v", b)
	}
	if b.Detail != "" {
		t.Errorf("task b lost detail: %q", b.Detail)
	}
	a := taskByID(t, d, "a")
	if a.Answer != "done" || len(a.Graders) != 1 || a.Graders[0].Name != "patch" {
		t.Errorf("task a = %+v", a)
	}
}

func TestLoadRendersARunWithNoGradedCells(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "collected"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: false, tokens: 5})

	d := mustLoad(t, st, "r1", Options{})
	pr := metricByKey(t, d, "pass_rate")
	if pr.Display != "—" {
		t.Errorf("pass rate = %q, want a dash rather than 0%%", pr.Display)
	}
	if pr.Note == "" {
		t.Error("the absent pass rate carries no explanation")
	}
	if d.Counts.Failed != 0 {
		t.Errorf("failed = %d, want 0: nothing was graded, so nothing failed", d.Counts.Failed)
	}
	if d.Counts.Ungraded != 1 {
		t.Errorf("ungraded = %d", d.Counts.Ungraded)
	}
}

func TestLoadToleratesMissingAndUnreadableTraces(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "no-trace", graded: true, passed: true})
	addCell(t, st, "r1", cellOpts{task: "bad-trace", graded: true, passed: true})

	// A trace the decoder cannot make sense of must not take the whole report
	// down: the numbers are still the numbers.
	if err := st.SaveAttempt(context.Background(), store.Attempt{
		RunID: "r1", TaskID: "bad-trace", Sample: 0, Status: store.CellOK,
		Graded: true, Passed: true, Trace: json.RawMessage(`"not a trace at all"`),
	}); err != nil {
		t.Fatal(err)
	}

	d := mustLoad(t, st, "r1", Options{})
	if len(d.Tasks) != 2 {
		t.Fatalf("got %d tasks", len(d.Tasks))
	}
	for _, x := range d.Tasks {
		if len(x.Events) != 0 || len(x.Tools) != 0 {
			t.Errorf("task %s produced detail from a trace it does not have", x.ID)
		}
	}
}

func TestLoadRejectsAnUnknownRun(t *testing.T) {
	st := newStore(t)
	if _, err := Load(context.Background(), st, "nope", Options{}); err == nil {
		t.Fatal("loading a run that does not exist succeeded")
	}
}

// ---------------------------------------------------------------------------
// Trajectories and the size budget
// ---------------------------------------------------------------------------

func TestTrajectoryKeepsHeadAndTail(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	var evs []trace.Event
	for i := 0; i < 100; i++ {
		evs = append(evs, trace.Event{Index: i, Kind: trace.KindAssistantText, Text: fmt.Sprintf("event %d", i)})
	}
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true, events: evs})

	d := mustLoad(t, st, "r1", Options{MaxEventsPerTask: 10})
	x := taskByID(t, d, "a")
	if len(x.Events) != 11 {
		t.Fatalf("kept %d events, want 10 plus the gap marker", len(x.Events))
	}
	if x.Events[0].Text != "event 0" {
		t.Errorf("the head was dropped: %q", x.Events[0].Text)
	}
	gap := x.Events[5]
	if gap.Kind != "truncated" || !strings.Contains(gap.Text, "90 events omitted") {
		t.Errorf("gap marker = %+v", gap)
	}
	if last := x.Events[10]; last.Text != "event 99" {
		t.Errorf("the tail was dropped: %q", last.Text)
	}
}

func TestBudgetDropsPassingDetailBeforeFailingDetail(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	big := strings.Repeat("x", 60000)
	addCell(t, st, "r1", cellOpts{task: "passes", graded: true, passed: true, diff: big})
	addCell(t, st, "r1", cellOpts{task: "fails", graded: true, passed: false, diff: big})

	// The field budget is wide enough not to clip either diff, so the only thing
	// that can shrink the payload is shedding whole cells' detail.
	d := mustLoad(t, st, "r1", Options{MaxFieldBytes: 65000, MaxInlineBytes: 100000})

	if got := taskByID(t, d, "fails").Diff; got != big {
		t.Errorf("the failing cell lost its evidence (len %d)", len(got))
	}
	if got := taskByID(t, d, "passes").Diff; got != "" {
		t.Errorf("the passing cell kept its evidence (len %d) while the budget was full", len(got))
	}
	if len(d.Warnings) == 0 {
		t.Fatal("detail was dropped without saying so")
	}
	if !strings.Contains(strings.Join(d.Warnings, " "), "budget") {
		t.Errorf("warnings = %v", d.Warnings)
	}
	// The numbers must survive: a dropped diff is not a dropped cell.
	if d.Counts.Cells != 2 || d.Counts.Passed != 1 {
		t.Errorf("counts changed when detail was shed: %+v", d.Counts)
	}
	if d.Run.Summary.Tokens != 0 {
		t.Errorf("summary changed: %+v", d.Run.Summary)
	}
}

func TestNoTaskDetailProducesASummaryOnly(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true, diff: "x", answer: "y"})

	d := mustLoad(t, st, "r1", Options{NoTaskDetail: true})
	if len(d.Tasks) != 0 {
		t.Errorf("got %d tasks, want none", len(d.Tasks))
	}
	if d.Counts.Cells != 1 {
		t.Errorf("counts = %+v: the numbers must survive the omission", d.Counts)
	}
	if !strings.Contains(strings.Join(d.Warnings, " "), "omitted on request") {
		t.Errorf("warnings = %v", d.Warnings)
	}
}

func TestClipKeepsBothEnds(t *testing.T) {
	s := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	got := clip(s, 20)
	if !strings.HasPrefix(got, "aaaaaaaaaa") {
		t.Errorf("head lost: %q", got)
	}
	if !strings.HasSuffix(got, "bbbbbbbbbb") {
		t.Errorf("tail lost: %q", got)
	}
	if !strings.Contains(got, "omitted") {
		t.Errorf("no gap marker: %q", got)
	}
	if clip("short", 20) != "short" {
		t.Error("short text was modified")
	}
	if clip("anything", 0) != "anything" {
		t.Error("a zero budget must mean no clipping")
	}
}

// ---------------------------------------------------------------------------
// The recorded baseline
// ---------------------------------------------------------------------------

// TestTheRecordedAnswerReachesThePayload is the projection half of the baseline
// answer: the store keeps it and the page has to receive it, or the whole
// column is dead weight.
func TestTheRecordedAnswerReachesThePayload(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{
		task: "a", answer: "this attempt's answer",
		base: &store.BaselineComparison{
			Present: true, ID: "sess-1", Tokens: 1000, Ms: 5000, Turns: 4,
			Output: "the recorded answer",
		},
	})
	addCell(t, st, "r1", cellOpts{
		task: "b",
		base: &store.BaselineComparison{Present: true, ID: "sess-2", Tokens: 1000, Ms: 5000},
	})

	d := mustLoad(t, st, "r1", Options{})

	a := taskByID(t, d, "a")
	if a.Baseline == nil {
		t.Fatal("task a lost its baseline")
	}
	if a.Baseline.Output != "the recorded answer" {
		t.Errorf("baseline output = %q, want the recording's answer in the payload", a.Baseline.Output)
	}
	if a.Detail != "" {
		t.Errorf("an answer inside the field budget was reported as clipped: %q", a.Detail)
	}
	// A recording with no final answer has to read as "none was kept". The
	// client renders that as a sentence; an empty string reaching it as an
	// empty box would look like a rendering bug instead.
	b := taskByID(t, d, "b")
	if b.Baseline == nil {
		t.Fatal("task b lost its baseline")
	}
	if b.Baseline.Output != "" {
		t.Errorf("baseline output = %q, want empty when the recording kept none", b.Baseline.Output)
	}
}

// TestTheRecordedAnswerCountsAgainstTheFieldBudget: the recorded answer is agent
// text like any other, so it is clipped like any other -- and says so.
func TestTheRecordedAnswerCountsAgainstTheFieldBudget(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	long := strings.Repeat("x", 400)
	addCell(t, st, "r1", cellOpts{
		task: "a",
		base: &store.BaselineComparison{
			Present: true, ID: "sess-1", Tokens: 1000, Ms: 5000, Output: long,
		},
	})

	d := mustLoad(t, st, "r1", Options{MaxFieldBytes: 80})

	a := taskByID(t, d, "a")
	if a.Baseline == nil {
		t.Fatal("task a lost its baseline")
	}
	if len(a.Baseline.Output) >= len(long) || !strings.Contains(a.Baseline.Output, "omitted") {
		t.Errorf("the recorded answer was not clipped to the field budget: %q", a.Baseline.Output)
	}
	if !strings.Contains(a.Detail, "clipped") {
		t.Errorf("clipping the recorded answer said nothing about it: %q", a.Detail)
	}
}

// ---------------------------------------------------------------------------
// Breakdowns
// ---------------------------------------------------------------------------

func TestBreakdownsComeFromCells(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "a", category: "bugfix", graded: true, passed: true, stop: "completed"})
	addCell(t, st, "r1", cellOpts{task: "b", category: "bugfix", graded: true, passed: false, stop: "completed"})
	addCell(t, st, "r1", cellOpts{task: "c", category: "feature", graded: true, passed: true, stop: "max_turns"})
	addCell(t, st, "r1", cellOpts{task: "d", category: "feature", status: store.CellTimeout, stop: "timeout"})

	d := mustLoad(t, st, "r1", Options{})

	var outcomes *Breakdown
	for i := range d.Breakdowns {
		if strings.Contains(d.Breakdowns[i].Title, "outcome") {
			outcomes = &d.Breakdowns[i]
		}
	}
	if outcomes == nil {
		t.Fatalf("no outcome breakdown in %v", d.Breakdowns)
	}
	// The timeout has to appear as its own row rather than being folded into
	// "failed": they are different claims.
	var sawTimeout bool
	for _, r := range outcomes.Rows {
		if strings.Contains(r.Label, "timed out") {
			sawTimeout = true
			if r.Hits != 1 {
				t.Errorf("timeout row = %+v", r)
			}
		}
	}
	if !sawTimeout {
		t.Errorf("the timeout is missing from %+v", outcomes.Rows)
	}

	var cats *Breakdown
	for i := range d.Breakdowns {
		if strings.Contains(d.Breakdowns[i].Title, "category") {
			cats = &d.Breakdowns[i]
		}
	}
	if cats == nil {
		t.Fatal("no category breakdown")
	}
	// bugfix is one of two graded cells; feature is one of one, because the
	// timed-out cell in it was never graded. An ungraded cell must not drag a
	// category's rate down.
	want := map[string]float64{"bugfix": 50, "feature": 100}
	for _, row := range cats.Rows {
		if row.Value != want[row.Label] {
			t.Errorf("category %s = %v%%, want %v%%", row.Label, row.Value, want[row.Label])
		}
	}
	if len(cats.Rows) != 2 {
		t.Errorf("got %d category rows: %+v", len(cats.Rows), cats.Rows)
	}
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

func TestCompareClassifiesEveryTransition(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a", name: "before"})
	addRun(t, st, runOpts{id: "b", name: "after"})
	addCell(t, st, "a", cellOpts{task: "fixed", graded: true, passed: false, ms: 10})
	addCell(t, st, "b", cellOpts{task: "fixed", graded: true, passed: true, ms: 5})
	addCell(t, st, "a", cellOpts{task: "regressed", graded: true, passed: true, ms: 5})
	addCell(t, st, "b", cellOpts{task: "regressed", graded: true, passed: false, ms: 50})
	addCell(t, st, "a", cellOpts{task: "same", graded: true, passed: true})
	addCell(t, st, "b", cellOpts{task: "same", graded: true, passed: true})
	addCell(t, st, "a", cellOpts{task: "removed", graded: true, passed: true})
	addCell(t, st, "b", cellOpts{task: "added", graded: true, passed: true})
	addCell(t, st, "a", cellOpts{task: "ungraded", graded: false})
	addCell(t, st, "b", cellOpts{task: "ungraded", graded: false})

	d, err := LoadComparison(context.Background(), st, "a", "b", Options{})
	if err != nil {
		t.Fatalf("LoadComparison: %v", err)
	}
	c := d.Compare
	if c == nil {
		t.Fatal("no comparison in the payload")
	}
	if c.LeftLabel != "before" || c.RightLabel != "after" {
		t.Errorf("labels = %q → %q", c.LeftLabel, c.RightLabel)
	}
	verdicts := map[string]string{}
	for _, td := range c.Tasks {
		verdicts[td.ID] = td.Verdict
	}
	want := map[string]string{
		"fixed": "fixed", "regressed": "regressed", "same": "unchanged",
		"removed": "removed", "added": "added", "ungraded": "ungraded",
	}
	for id, w := range want {
		if verdicts[id] != w {
			t.Errorf("%s: verdict %q, want %q", id, verdicts[id], w)
		}
	}
	if c.Summary.Fixed != 1 || c.Summary.Regressed != 1 {
		t.Errorf("summary = %+v", c.Summary)
	}
	if c.Summary.OnlyLeft != 1 || c.Summary.OnlyRight != 1 {
		t.Errorf("summary = %+v", c.Summary)
	}
	if c.Summary.Ungraded != 1 {
		t.Errorf("summary = %+v: an ungraded pair is neither a win nor a loss", c.Summary)
	}
	// Regressions come first: they are the reason someone opens a comparison.
	if c.Tasks[0].Verdict != "regressed" {
		t.Errorf("first row = %q, want the regression", c.Tasks[0].Verdict)
	}
}

func TestCompareVerdictsRespectDirection(t *testing.T) {
	left := withCells(10, 5, 1000, 0.10, 1000)
	right := withCells(10, 6, 800, 0.08, 900)

	deltas := map[string]MetricDelta{}
	for _, m := range metricDeltas(left, right) {
		deltas[m.Key] = m
	}
	// More passes is better even though the number went up...
	if got := deltas["pass_rate"].Verdict; got != "better" {
		t.Errorf("pass rate verdict = %q, want better", got)
	}
	// ...and more tokens is worse even though the number also went up.
	if got := deltas["tokens"].Verdict; got != "better" {
		t.Errorf("token verdict = %q, want better: the run used fewer", got)
	}
	if deltas["tokens"].Direction != "down" {
		t.Errorf("token direction = %q", deltas["tokens"].Direction)
	}
	if deltas["cost_usd"].Verdict != "better" {
		t.Errorf("cost verdict = %q", deltas["cost_usd"].Verdict)
	}

	// A metric that is not better in either direction must not be given a
	// verdict, or two runs over different amounts of work would read as a win.
	if deltas["cells"].Verdict != "unknown" {
		t.Errorf("cell count verdict = %q, want unknown", deltas["cells"].Verdict)
	}
	if deltas["cells"].Direction != "" {
		t.Errorf("cell count direction = %q, want none", deltas["cells"].Direction)
	}
}

func TestCompareReportsUnchangedCellsHonestly(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a"})
	addRun(t, st, runOpts{id: "b"})
	addCell(t, st, "a", cellOpts{task: "x", graded: true, passed: true})
	addCell(t, st, "b", cellOpts{task: "x", graded: true, passed: true})
	addCell(t, st, "a", cellOpts{task: "y", graded: true, passed: false})
	addCell(t, st, "b", cellOpts{task: "y", graded: true, passed: false})

	d, err := LoadComparison(context.Background(), st, "a", "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Compare.Summary.BothPass != 1 || d.Compare.Summary.BothFail != 1 {
		t.Errorf("summary = %+v", d.Compare.Summary)
	}
}

// ---------------------------------------------------------------------------
// Sweeps
// ---------------------------------------------------------------------------

func TestLoadSweepAttachesTheAttribution(t *testing.T) {
	st := newStore(t)
	meta := attrib.Meta{Name: "model-sweep", Axis: attrib.AxisModel, Control: "sonnet"}
	blob, err := meta.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, lvl := range []string{"sonnet", "haiku"} {
		addRun(t, st, runOpts{
			id: "run-" + lvl, name: "model-sweep", dataset: "demo",
			factor: [2]string{"model", lvl}, sweep: "sw1", config: blob,
		})
	}
	for i := 0; i < 8; i++ {
		task := fmt.Sprintf("t%02d", i)
		addCell(t, st, "run-sonnet", cellOpts{task: task, graded: true, passed: i < 2, tokens: 1000, ms: 5000})
		addCell(t, st, "run-haiku", cellOpts{task: task, graded: true, passed: i < 8, tokens: 700, ms: 4000})
	}

	d, err := LoadSweep(context.Background(), st, "sw1", Options{})
	if err != nil {
		t.Fatalf("LoadSweep: %v", err)
	}
	if d.Sweep == nil {
		t.Fatal("no attribution in the payload")
	}
	if d.Sweep.Axis != attrib.AxisModel || d.Sweep.Control != "sonnet" {
		t.Errorf("attribution = %+v", d.Sweep)
	}
	if len(d.Sweep.Comparisons) != 1 {
		t.Fatalf("got %d comparisons", len(d.Sweep.Comparisons))
	}
	if !strings.Contains(d.Title, "2 levels of model") {
		t.Errorf("title = %q", d.Title)
	}
	// A sweep report is about the comparison; carrying the control's per-task
	// evidence would triple its size for nothing.
	if len(d.Tasks) != 0 {
		t.Errorf("the sweep report carries %d task rows", len(d.Tasks))
	}
	if len(d.Sweep.Caveats) == 0 {
		t.Error("the attribution carries no caveats")
	}
}

func TestLoadSweepRefusesARunThatIsNotASweep(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", factor: [2]string{"model", "sonnet"}, sweep: "sw1"})
	if _, err := LoadSweep(context.Background(), st, "sw1", Options{}); err == nil {
		t.Fatal("attributing a run with no recorded control succeeded")
	}
}

// ---------------------------------------------------------------------------
// Index
// ---------------------------------------------------------------------------

func TestLoadIndexListsRunsNewestFirst(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i, id := range []string{"older", "newer"} {
		err := st.CreateRun(ctx, store.Run{
			ID: id, Name: id, Status: store.RunRunning,
			StartedAt:   time.Date(2026, 3, 1+i, 9, 0, 0, 0, time.UTC),
			DatasetName: "demo", AgentKind: "claude_code", Samples: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRun(ctx, id, store.RunCompleted, ""); err != nil {
			t.Fatal(err)
		}
	}
	addCell(t, st, "newer", cellOpts{task: "a", graded: true, passed: true, tokens: 42})

	idx, err := LoadIndex(ctx, st, 0, store.RunFilter{}, Options{})
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	if len(idx.Runs) != 2 || idx.Runs[0].ID != "newer" {
		t.Fatalf("runs = %+v", idx.Runs)
	}
	if idx.Runs[0].Tokens != 42 {
		t.Errorf("tokens = %d", idx.Runs[0].Tokens)
	}
	if idx.Runs[0].PassRate == nil || *idx.Runs[0].PassRate != 1 {
		t.Errorf("pass rate = %v", idx.Runs[0].PassRate)
	}
	// A run with nothing graded has no pass rate, which is not a pass rate of
	// zero.
	if idx.Runs[1].PassRate != nil {
		t.Errorf("the ungraded run reports a pass rate of %v", *idx.Runs[1].PassRate)
	}
	if idx.Runs[1].Error != "" {
		t.Errorf("error = %q", idx.Runs[1].Error)
	}
}

func TestLoadIndexCanFilterBySweep(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "s1", factor: [2]string{"model", "a"}, sweep: "sw1"})
	addRun(t, st, runOpts{id: "other"})

	idx, err := LoadIndex(context.Background(), st, 0, store.RunFilter{SweepID: "sw1"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Runs) != 1 || idx.Runs[0].ID != "s1" {
		t.Fatalf("runs = %+v", idx.Runs)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestStandaloneReportIsOneFile(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "demo"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true})

	var buf bytes.Buffer
	if err := RenderRun(&buf, mustLoad(t, st, "r1", Options{}), RenderOptions{Standalone: true}); err != nil {
		t.Fatalf("RenderRun: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "app.css") || strings.Contains(out, `src="/static/app.js"`) {
		t.Error("the standalone report links to assets instead of inlining them")
	}
	// The stylesheet has to arrive as CSS, not as escaped text: injecting it as
	// template.HTML would have html/template re-escape it and silently break
	// every rule.
	if !strings.Contains(out, "--accent:") {
		t.Error("the stylesheet was mangled on the way in")
	}
	if !strings.Contains(out, "renderSweep") {
		t.Error("the client script was mangled on the way in")
	}
	if !strings.Contains(out, `data-mode="run"`) {
		t.Error("the page does not declare its mode")
	}
}

func TestServedReportReferencesAssetsExternally(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true})

	var buf bytes.Buffer
	if err := RenderRun(&buf, mustLoad(t, st, "r1", Options{}), RenderOptions{AssetBase: "/static/"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/static/app.css"`) || !strings.Contains(out, `src="/static/app.js"`) {
		t.Error("the served report does not reference its assets")
	}
	if strings.Contains(out, "--accent:") {
		t.Error("the served report inlined the stylesheet anyway")
	}
}

// A `hidden` attribute is a promise the stylesheet can quietly break. The rule
// that implements it lives in the UA stylesheet and is therefore weaker than
// any author rule that sets `display` on the same element -- and the drawer
// does set `display: flex`. While the drawer is empty it is an opaque 1000px
// panel, so the symptom was not "a stray panel appears" but "the report is cut
// off two-thirds of the way across", which reads like a layout bug in the
// report itself. Rather than pin the one selector that got bitten, this checks
// the invariant for every hidden element the shell ships.
func TestHiddenElementsCanActuallyBeHidden(t *testing.T) {
	shell, err := templateFS.ReadFile("templates/page.html")
	if err != nil {
		t.Fatalf("read shell: %v", err)
	}
	css := CSS()

	// Every selector the stylesheet gives a `display` to, exactly as written.
	setsDisplay := map[string]bool{}
	for _, r := range cssRules(css) {
		if strings.Contains(r.body, "display:") {
			setsDisplay[r.sel] = true
		}
	}

	classes := hiddenClasses(string(shell))
	if len(classes) == 0 {
		t.Skip("the shell ships no hidden elements, so there is no promise to keep")
	}
	for _, class := range classes {
		plain, override := "."+class, "."+class+"[hidden]"
		if !setsDisplay[plain] {
			continue // nothing the stylesheet does can defeat [hidden]
		}
		if !setsDisplay[override] {
			t.Errorf("the shell ships a hidden .%s and the stylesheet sets its display, "+
				"but there is no %q rule -- the element stays visible until something "+
				"opens it, and while it is empty it covers the page", class, override)
		}
	}
}

// hiddenClasses returns the class tokens of every element in the shell that
// carries a bare `hidden` attribute.
func hiddenClasses(shell string) []string {
	var out []string
	tags := regexp.MustCompile(`(?s)<[a-zA-Z][^>]*>`).FindAllString(shell, -1)
	for _, tag := range tags {
		var hidden bool
		for _, attr := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(tag, "<"), ">")) {
			if attr == "hidden" {
				hidden = true
				break
			}
		}
		if !hidden {
			continue
		}
		m := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil {
			continue // no class, so no author rule can reach it
		}
		out = append(out, strings.Fields(m[1])...)
	}
	return out
}

type cssRule struct{ sel, body string }

// cssRules is a deliberately shallow parse: it pairs each `{...}` body with the
// selector text in front of it, which is enough to answer "does this selector
// set display". Nested at-rules come out as their inner rules, which is what we
// want -- `@media` does not change what a selector says. Comments are stripped
// first, or the prose in front of a rule would be read as part of its selector.
func cssRules(css string) []cssRule {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var out []cssRule
	for _, m := range regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		out = append(out, cssRule{
			sel:  strings.Join(strings.Fields(m[1]), " "),
			body: m[2],
		})
	}
	return out
}

func TestEachModeRendersInItsOwnShell(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a"})
	addRun(t, st, runOpts{id: "b"})
	addCell(t, st, "a", cellOpts{task: "x", graded: true, passed: true})
	addCell(t, st, "b", cellOpts{task: "x", graded: true, passed: true})

	cmp, err := LoadComparison(context.Background(), st, "a", "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := LoadIndex(context.Background(), st, 0, store.RunFilter{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mode   string
		render func(*bytes.Buffer) error
	}{
		{"run", "run", func(b *bytes.Buffer) error {
			return RenderRun(b, mustLoad(t, st, "a", Options{}), RenderOptions{})
		}},
		{"compare", "compare", func(b *bytes.Buffer) error { return RenderComparison(b, cmp, RenderOptions{}) }},
		{"index", "index", func(b *bytes.Buffer) error { return RenderIndex(b, idx, RenderOptions{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.render(&buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(buf.String(), `data-mode="`+tc.mode+`"`) {
				t.Errorf("the page does not declare mode %q", tc.mode)
			}
		})
	}
}

func TestRenderRejectsMissingInput(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderRun(&buf, nil, RenderOptions{}); err == nil {
		t.Error("rendering a nil payload succeeded")
	}
	if err := RenderComparison(&buf, &Data{}, RenderOptions{}); err == nil {
		t.Error("rendering a comparison with no comparison payload succeeded")
	}
	if err := RenderSweep(&buf, &Data{}, RenderOptions{}); err == nil {
		t.Error("rendering a sweep with no attribution succeeded")
	}
	if err := RenderIndex(&buf, nil, RenderOptions{}); err == nil {
		t.Error("rendering a nil index succeeded")
	}
}

func TestRenderRunFileCreatesDirectories(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true})

	path := filepath.Join(t.TempDir(), "nested", "deeper", "report.html")
	if err := RenderRunFile(path, mustLoad(t, st, "r1", Options{}), RenderOptions{}); err != nil {
		t.Fatalf("RenderRunFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("report not written: %v", err)
	}
}

func TestFileNameIsSafeForAnUntrustedRunID(t *testing.T) {
	cases := map[string]string{
		"run-1":                  "report-run-1.html",
		"../../etc/passwd":       "report-etc-passwd.html",
		"":                       "report-run.html",
		"a b/c":                  "report-a-b-c.html",
		strings.Repeat("z", 200): "report-" + strings.Repeat("z", 80) + ".html",
	}
	for in, want := range cases {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.ContainsAny(FileName("../../etc/passwd"), `/\`) {
		t.Error("a path separator survived sanitisation")
	}
}

// ---------------------------------------------------------------------------
// XSS and payload identity
// ---------------------------------------------------------------------------

func TestPayloadCannotBreakOutOfItsScriptElement(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", notes: "</script><script>alert('run')</script>"})
	addCell(t, st, "r1", cellOpts{
		task: "a", graded: true, passed: false, answer: "</script><img src=x onerror=alert(1)>",
		diff:   "--- a\n+++ b\n<!-- comment -->\n",
		events: []trace.Event{{Index: 0, Kind: trace.KindAssistantText, Text: "<b>raw</b>"}},
	})

	var buf bytes.Buffer
	if err := RenderRun(&buf, mustLoad(t, st, "r1", Options{}), RenderOptions{Standalone: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The embedded payload is the risk: the page shell legitimately contains
	// "<script>" for its own tags and, in standalone mode, the whole client
	// script after it -- so the check has to be on the payload literal itself.
	payload := extractPayload(t, out)
	if strings.Contains(payload, "<") || strings.Contains(payload, ">") {
		t.Errorf("the payload literal contains a raw angle bracket:\n%s", payload[:min(len(payload), 400)])
	}
	if !strings.Contains(payload, `\u003c`) {
		t.Error("the offending text was not escaped")
	}
}

func TestNeutralizeEscapesExactlyTheSignificantBytes(t *testing.T) {
	in := []byte(`{"a":"<b>&c>","b":1}`)
	got := neutralize(in)
	want := `{"a":"\u003cb\u003e\u0026c\u003e","b":1}`
	if got != want {
		t.Errorf("neutralize = %s, want %s", got, want)
	}
	// A document with none of the three must come back byte for byte, or every
	// report would be reformatted for no reason.
	plain := []byte(`{"a":"plain text"}`)
	if got := neutralize(plain); got != string(plain) {
		t.Errorf("neutralize rewrote a clean document: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Error("neutralize produced invalid JSON")
	}
}

func TestDataJSONIsTheSameDocumentAsTheEmbeddedOne(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true, answer: "</script>"})

	d := mustLoad(t, st, "r1", Options{})
	var buf bytes.Buffer
	if err := RenderRun(&buf, d, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	embedded := extractPayload(t, buf.String())

	api, err := DataJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != string(api) {
		t.Fatal("the file report and the API disagree about the same run")
	}
}

func extractPayload(t *testing.T, page string) string {
	t.Helper()
	const marker = "window.__AEVAL_PAYLOAD__ = "
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatal("no payload literal")
	}
	rest := page[i+len(marker):]
	// The template writes "{{.Payload}};</script>", so the first closing tag
	// after the marker terminates the literal. Searching from the end would run
	// past it into the inlined client script.
	end := strings.Index(rest, ";</script>")
	if end < 0 {
		end = strings.Index(rest, "</script>")
	}
	if end < 0 {
		t.Fatal("the payload literal is not terminated")
	}
	return strings.TrimSpace(rest[:end])
}

func TestDefaultsBoundEveryUnsetOption(t *testing.T) {
	o := Options{}.withDefaults()
	if o.MaxInlineBytes <= 0 || o.MaxToolCallsPerTask <= 0 || o.MaxFieldBytes <= 0 || o.MaxEventsPerTask <= 0 {
		t.Errorf("defaults leave a budget unbounded: %+v", o)
	}
	if o.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is unset")
	}
	given := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := (Options{GeneratedAt: given}).withDefaults().GeneratedAt; !got.Equal(given) {
		t.Errorf("an explicit GeneratedAt was overwritten: %v", got)
	}
}

// withCells builds the summary shape the direction tests need.
func withCells(cells, passed, tokens int, cost float64, ms int64) runner.Summary {
	return runner.Summary{
		Cells: cells, Graded: cells, Passed: passed,
		Tokens: tokens, CostUSD: cost, DurationMs: ms,
	}
}

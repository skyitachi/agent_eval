package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
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
	// agent, bin and backend default to the ordinary local Claude Code cell.
	// They exist so a comparison test can make two runs differ in exactly one
	// of the things a run row records.
	agent   string
	bin     string
	backend string
	// spec, when set, is the agent configuration recorded on the run. Nil
	// leaves it unrecorded, which is how every row written before schema v4
	// reads -- and reading that as "configured nothing" is the mistake the
	// report has to avoid.
	spec *agent.Spec
	// chained is whether the run gave each session's tasks one workspace and
	// ran them in order. False is also how every row written before schema v5
	// reads, and there that is the answer rather than a gap.
	chained bool
	// inPlace is whether the run worked in the directories the tasks record.
	// False is likewise how every row written before schema v7 reads.
	inPlace bool
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
	if o.agent == "" {
		o.agent = "claude_code"
	}
	if o.backend == "" {
		o.backend = "local"
	}
	var specJSON string
	if o.spec != nil {
		raw, err := json.Marshal(*o.spec)
		if err != nil {
			t.Fatalf("marshal the fixture's agent spec: %v", err)
		}
		specJSON = string(raw)
	}
	ctx := context.Background()
	started := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	err := st.CreateRun(ctx, store.Run{
		ID: o.id, Name: o.name, Status: store.RunRunning, StartedAt: started,
		DatasetName: o.dataset, DatasetHash: o.hash,
		AgentKind: o.agent, AgentModel: o.model, AgentBin: o.bin, Backend: o.backend,
		Samples: o.samples, Concurrency: 1,
		FactorAxis: o.factor[0], FactorLevel: o.factor[1], SweepID: o.sweep,
		Notes: o.notes, ConfigJSON: o.config, AgentSpecJSON: specJSON,
		ChainSessions: o.chained,
		InPlace:       o.inPlace,
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
	// observed is the model this cell's stream says served it, recorded where a
	// real stream records it: as the per-model accounting on the result event.
	// Empty means the stream named no model, which is what a fixture that emits
	// no stream at all looks like.
	observed string
	// announced is the model the stream named at init instead. It is set only
	// by the tests that are about the two sources disagreeing.
	announced string
	// workspaceDir and restoreNotes are what an in-place cell records about the
	// directory it worked in.
	workspaceDir string
	restoreNotes string
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
		WorkspaceDir: o.workspaceDir, RestoreNotes: o.restoreNotes,
	}
	if o.base != nil {
		a.Baseline = *o.base
	}
	if len(o.events) > 0 || len(o.calls) > 0 || o.observed != "" || o.announced != "" {
		tr := trace.Trace{Events: o.events, ToolCalls: o.calls, Model: o.announced}
		if o.observed != "" {
			tr.Result.ModelUsage = map[string]trace.ModelUsage{o.observed: {InputTokens: 10}}
		}
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		a.Trace = raw
	}
	if err := st.SaveAttempt(context.Background(), a); err != nil {
		t.Fatalf("save cell %s#%d: %v", o.task, o.sample, err)
	}
}

// renderPage writes the run's report the way a caller does and returns the
// document, because the chip and the notices are in the rendered page and not
// in the payload.
func renderPage(t *testing.T, d *Data) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.html")
	if err := RenderRunFile(path, d, RenderOptions{}); err != nil {
		t.Fatalf("RenderRunFile: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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

// TestCompareSaysWhatItsDeltasBundle covers the one thing a comparison can say
// about attribution: what the two runs differ in besides the numbers.
//
// The command cannot refuse a comparison -- "what changed between these two?"
// is a fair question about any pair -- so silence would read as "these two
// numbers measured the same thing", which is the one reading that cannot hold
// when the two runs differ in two recorded things at once.
func TestCompareSaysWhatItsDeltasBundle(t *testing.T) {
	cases := []struct {
		name  string
		left  runOpts
		right runOpts
		want  []string // substrings that must appear; empty means no warnings
	}{
		{
			name:  "two runs that differ in nothing recorded are not annotated",
			left:  runOpts{id: "a", model: "m"},
			right: runOpts{id: "b", model: "m"},
		},
		{
			name:  "one recorded difference is named as the single cause",
			left:  runOpts{id: "a", model: "m1"},
			right: runOpts{id: "b", model: "m2"},
			want:  []string{"differ in the model (m1 vs m2)", "that one difference and nothing else"},
		},
		{
			// The case the whole thing exists for: an agent swap and a model
			// swap at once. Every delta below is a bundle.
			name:  "two recorded differences refuse the attribution",
			left:  runOpts{id: "a", model: "m1", agent: "claude_code"},
			right: runOpts{id: "b", model: "m2", agent: "mock"},
			want: []string{
				"differ in more than one thing",
				"the agent (claude_code vs mock)",
				"the model (m1 vs m2)",
				"so no delta below can be attributed to any single one",
			},
		},
		{
			name:  "the agent binary is one of the recorded things",
			left:  runOpts{id: "a", model: "m", bin: "/bin/good.sh"},
			right: runOpts{id: "b", model: "m", bin: "/bin/bad.sh"},
			want:  []string{"differ in the agent binary (/bin/good.sh vs /bin/bad.sh)"},
		},
		{
			name:  "the sandbox backend is one of them too",
			left:  runOpts{id: "a", model: "m", backend: "local"},
			right: runOpts{id: "b", model: "m", backend: "docker"},
			want:  []string{"differ in the sandbox backend (local vs docker)"},
		},
		{
			// A different dataset is not a difference of degree: the cells
			// paired below are different questions.
			name:  "a different dataset is its own warning",
			left:  runOpts{id: "a", model: "m", hash: "hash-1"},
			right: runOpts{id: "b", model: "m", hash: "hash-2"},
			want:  []string{"did not run the same dataset content"},
		},
		{
			// The gap this exists to close: the skill set is not one of the
			// label columns, so this pair agreed on everything the row used to
			// record and the page had nothing to say about it.
			name: "a skill difference is a recorded difference",
			left: runOpts{id: "a", model: "m", spec: &agent.Spec{
				Model: "m", PluginDirs: []string{"/skills/a"},
				Isolation: &agent.Isolation{}}},
			right: runOpts{id: "b", model: "m", spec: &agent.Spec{
				Model: "m", Isolation: &agent.Isolation{}}},
			want: []string{
				"differ in the skills loaded (/skills/a vs none)",
				"that one difference and nothing else",
			},
		},
		{
			// Declaring a directory is not loading it. The clean-room flags
			// remove the agent's skills, so this pair drove the same
			// configuration and reporting a difference would be wrong.
			name: "a directory the flags removed is not a skill the run had",
			left: runOpts{id: "a", model: "m", spec: &agent.Spec{
				Model: "m", PluginDirs: []string{"/skills/a"},
				Isolation: &agent.Isolation{Bare: true}}},
			right: runOpts{id: "b", model: "m", spec: &agent.Spec{
				Model: "m", Isolation: &agent.Isolation{}}},
		},
		{
			// Two runs that declared the same directories but only one of
			// which let them through are a skill difference, whatever the
			// declarations look like.
			name: "the same declaration with different flags is a difference",
			left: runOpts{id: "a", model: "m", spec: &agent.Spec{
				Model: "m", PluginDirs: []string{"/skills/a"},
				Isolation: &agent.Isolation{}}},
			right: runOpts{id: "b", model: "m", spec: &agent.Spec{
				Model: "m", PluginDirs: []string{"/skills/a"},
				Isolation: &agent.Isolation{Bare: true}}},
			want: []string{"differ in the skills loaded (/skills/a vs none)"},
		},
		{
			// Only one side can answer the question, so the comparison cannot
			// answer it either -- and it has to name which side, because the
			// guess a reader makes about a silent run is "it loaded nothing".
			name: "a run with no recorded configuration is named as unknown",
			left: runOpts{id: "a", name: "old", model: "m"},
			right: runOpts{id: "b", model: "m", spec: &agent.Spec{
				Model: "m", Isolation: &agent.Isolation{}}},
			want: []string{"run old recorded no agent configuration",
				"whether the two runs loaded the same skills is unknown"},
		},
		{
			// The two runs agree on every label and still measured different
			// questions: one replayed each session's tasks in a fresh
			// workspace, the other carried the state forward. Nothing else on
			// the page says so, and a reader who takes the paired deltas for a
			// like-for-like comparison has been misled by silence.
			name:  "a chained replay and a plain one are not the same measurement",
			left:  runOpts{id: "a", model: "m"},
			right: runOpts{id: "b", model: "m", chained: true},
			want: []string{
				"whether a session's tasks were chained into one workspace (not chained vs chained)",
				"that one difference and nothing else",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			addRun(t, st, tc.left)
			addRun(t, st, tc.right)
			d, err := LoadComparison(context.Background(), st, tc.left.id, tc.right.id, Options{})
			if err != nil {
				t.Fatalf("LoadComparison: %v", err)
			}
			if len(tc.want) == 0 {
				if len(d.Warnings) != 0 {
					t.Fatalf("warnings = %v, want none", d.Warnings)
				}
				return
			}
			got := strings.Join(d.Warnings, "\n")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("warnings should mention %q, got:\n%s", w, got)
				}
			}
		})
	}
}

// TestTheRunHeaderCanProveWhichSkillsWereLoaded covers the claim the page has to
// be able to make or withhold.
//
// Four states, and the point is that they are four: a run that loaded the
// user's skills, a run that declared them and had them removed by the
// clean-room flags, a run that declared none, and a run that predates the
// harness recording any of it. Collapsing any two of them -- and in particular
// reading the last as the third -- is a page asserting a configuration it was
// never told.
func TestTheRunHeaderCanProveWhichSkillsWereLoaded(t *testing.T) {
	cases := []struct {
		name string
		spec *agent.Spec
		want *Skills
	}{
		{
			name: "skills loaded",
			spec: &agent.Spec{PluginDirs: []string{"/b", "/a"}, Isolation: &agent.Isolation{}},
			want: &Skills{Dirs: []string{"/a", "/b"}, Active: true, Text: "2 plugin dir(s)"},
		},
		{
			name: "declared and removed by the clean-room flags",
			spec: &agent.Spec{PluginDirs: []string{"/a"}, Isolation: &agent.Isolation{Bare: true}},
			want: &Skills{Dirs: []string{"/a"}, RemovedBy: "--bare removed them",
				Text: "1 plugin dir(s), not loaded (--bare removed them)"},
		},
		{
			name: "none declared",
			spec: &agent.Spec{Isolation: &agent.Isolation{}},
			want: &Skills{Dirs: []string{}, Text: "none declared"},
		},
		{
			name: "no configuration recorded at all",
			spec: nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			addRun(t, st, runOpts{id: "r1", dataset: "d", spec: tc.spec})
			d, err := Load(context.Background(), st, "r1", Options{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := d.Run.Skills
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("skills = %+v, want nothing: an unrecorded configuration must not be "+
					"reported as one that declared no skills", got)
			case tc.want != nil && got == nil:
				t.Fatalf("skills = nil, want %+v", tc.want)
			}
			if tc.want == nil {
				return
			}
			if got.Text != tc.want.Text || got.Active != tc.want.Active ||
				got.RemovedBy != tc.want.RemovedBy ||
				strings.Join(got.Dirs, ",") != strings.Join(tc.want.Dirs, ",") {
				t.Errorf("skills = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTheHeaderSaysWhichModelRan covers the four states the record can be in.
//
// The label column holds what the command line asked for, and it is empty on
// every run that did not pass --model. Rendering nothing in that case is what
// this replaced: "no model was requested", "the CLI's default was used" and
// "nobody recorded it" are three different facts that all looked like an empty
// space on the page.
func TestTheHeaderSaysWhichModelRan(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		observed string
		want     string
		subst    bool
	}{
		{
			name: "a model was asked for and answered",
			// The stream names the resolved model, not the alias, and that must
			// not read as a substitution.
			declared: "sonnet", observed: "claude-sonnet-4-5-20250929",
			want: "sonnet (claude-sonnet-4-5-20250929)",
		},
		{
			name:     "nothing was asked for, so the CLI chose",
			declared: "", observed: "claude-opus-4-7",
			want: "default (the CLI chose it): claude-opus-4-7",
		},
		{
			name:     "asked for one model and served another",
			declared: "claude-opus-4-7", observed: "claude-haiku-4",
			want:  "claude-opus-4-7, but the stream billed claude-haiku-4",
			subst: true,
		},
		{
			name:     "asked for a model and the stream named none",
			declared: "sonnet", observed: "",
			want: "sonnet (the stream named no model)",
		},
		{
			name:     "nothing is known at all",
			declared: "", observed: "",
			want: "not recorded",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(t)
			addRun(t, st, runOpts{id: "r1", dataset: "d", model: c.declared})
			addCell(t, st, "r1", cellOpts{task: "t", graded: true, passed: true, observed: c.observed})

			d := mustLoad(t, st, "r1", Options{})
			m := d.Run.Model
			if m == nil {
				t.Fatal("the run has no model view, so the page cannot state one")
			}
			if m.Text != c.want {
				t.Errorf("text = %q, want %q", m.Text, c.want)
			}
			if m.Substitute != c.subst {
				t.Errorf("substitute = %v, want %v", m.Substitute, c.subst)
			}
			if m.Declared != c.declared {
				t.Errorf("declared = %q, want %q", m.Declared, c.declared)
			}
			// The declared label is still on the payload: the comparison reads
			// it, and it is the one field a consumer already knows.
			if d.Run.AgentModel != c.declared {
				t.Errorf("agent_model = %q, want the declared value kept", d.Run.AgentModel)
			}
		})
	}
}

// TestEveryCellSaysWhichModelServedIt: a run is not necessarily one model. A
// fallback mid-run is served by one cell's stream, and the run header can only
// say that it happened somewhere -- so the cell has to carry its own answer, and
// carry it in the same words whether or not it has one.
func TestEveryCellSaysWhichModelServedIt(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "d", model: "sonnet"})
	addCell(t, st, "r1", cellOpts{task: "matched", graded: true, passed: true,
		observed: "claude-sonnet-4-5-20250929"})
	addCell(t, st, "r1", cellOpts{task: "fell-back", graded: true, passed: true,
		observed: "claude-haiku-4"})
	addCell(t, st, "r1", cellOpts{task: "silent", graded: true, passed: true})

	d := mustLoad(t, st, "r1", Options{})
	if got := taskByID(t, d, "matched").Model; got != "claude-sonnet-4-5-20250929" {
		t.Errorf("the alias resolved to %q, want the resolved name alone: asking for sonnet is not a difference", got)
	}
	if got := taskByID(t, d, "fell-back").Model; got != "claude-haiku-4 (asked for sonnet)" {
		t.Errorf("model = %q, want the cell to name what it was served and what was asked for", got)
	}
	if got := taskByID(t, d, "silent").Model; got != "not named by the stream" {
		t.Errorf("model = %q, want the absence stated rather than left blank", got)
	}

	// The header sees both names, because it is the run-level claim, and a
	// reader who only read the header would otherwise take "sonnet" for the
	// whole run.
	m := d.Run.Model
	if len(m.Observed) != 2 || m.Observed[0] != "claude-haiku-4" || m.Observed[1] != "claude-sonnet-4-5-20250929" {
		t.Errorf("observed = %v, want both models the streams named, sorted", m.Observed)
	}
	if !m.Substitute {
		t.Error("one cell was served a model other than the one asked for, which the header has to say")
	}
}

// TestTheHeaderReadsTheModelFromEveryCellNotTheFirstOne: a run whose model
// changed partway is the case this exists for, and taking the first cell's word
// for it would hide exactly that.
func TestTheHeaderReadsTheModelFromEveryCellNotTheFirstOne(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "d"})
	addCell(t, st, "r1", cellOpts{task: "a", graded: true, passed: true, observed: "claude-opus-4-7"})
	addCell(t, st, "r1", cellOpts{task: "b", graded: true, passed: true, observed: "claude-haiku-4"})

	d := mustLoad(t, st, "r1", Options{})
	if got := d.Run.Model.Observed; len(got) != 2 {
		t.Errorf("observed = %v, want both, because the run is what changed", got)
	}
	if !strings.Contains(d.Run.Model.Text, "claude-opus-4-7") ||
		!strings.Contains(d.Run.Model.Text, "claude-haiku-4") {
		t.Errorf("text = %q, want both models named", d.Run.Model.Text)
	}
}

// TestTwoRunsServedDifferentModelsIsNotSilence covers the arm the columns
// cannot reach, and the one the label column was quietly standing in for.
//
// runs.agent_model holds what the command line asked for, so it is empty on
// every run that did not pass --model. Two such runs whose streams named
// different models agreed on every column the comparison looked at, and the
// page presented the pair as like-for-like with nothing saying otherwise.
func TestTwoRunsServedDifferentModelsIsNotSilence(t *testing.T) {
	compare := func(t *testing.T, left, right cellOpts) []string {
		t.Helper()
		st := newStore(t)
		addRun(t, st, runOpts{id: "a", dataset: "d"})
		addRun(t, st, runOpts{id: "b", dataset: "d"})
		left.task, right.task = "t", "t"
		addCell(t, st, "a", left)
		addCell(t, st, "b", right)
		d, err := LoadComparison(context.Background(), st, "a", "b", Options{})
		if err != nil {
			t.Fatalf("LoadComparison: %v", err)
		}
		return d.Warnings
	}

	// Neither asked for a model, and the streams name two different ones. This
	// is a real difference in what the numbers below measure.
	got := strings.Join(compare(t,
		cellOpts{observed: "claude-opus-4-7"},
		cellOpts{observed: "claude-haiku-4"},
	), "\n")
	for _, want := range []string{
		"which model served the requests (claude-opus-4-7 vs claude-haiku-4)",
		"that one difference and nothing else",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the comparison should say %q, got:\n%s", want, got)
		}
	}

	// The same model on both sides is not a difference, and an empty list here
	// is the claim that the pair really did measure the same thing.
	if w := compare(t,
		cellOpts{observed: "claude-sonnet-4-5"},
		cellOpts{observed: "claude-sonnet-4-5"},
	); len(w) != 0 {
		t.Errorf("warnings = %v, want none: the two runs were served the same model", w)
	}

	// An alias on one side and the resolved name on the other is the same
	// model, and calling it a difference would put a false warning on an
	// ordinary pair -- which is how a warning stops being read.
	if w := compare(t,
		cellOpts{observed: "sonnet"},
		cellOpts{observed: "claude-sonnet-4-5"},
	); len(w) != 0 {
		t.Errorf("warnings = %v, want none: an alias is not a different model", w)
	}

	// One side's stream named nothing, with neither run having asked for a
	// model. The pair is not known to be comparable, and the reader's guess
	// about the silent side is the one that has to be corrected by name.
	got = strings.Join(compare(t,
		cellOpts{observed: "claude-opus-4-7"},
		cellOpts{},
	), "\n")
	if !strings.Contains(got, "named no model in its stream") {
		t.Errorf("a pair whose models cannot both be read should say so, got:\n%s", got)
	}
	if !strings.Contains(got, "run b") {
		t.Errorf("the silent side must be named, not left as \"one of these runs\", got:\n%s", got)
	}
}

// TestASubstitutedModelIsNotAlsoCalledARecordedDifference: when the two runs
// declared different models the sentence above already covers the model, and a
// second one about the same column is how both stop being read.
func TestASubstitutedModelIsNotAlsoCalledARecordedDifference(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a", dataset: "d", model: "m1"})
	addRun(t, st, runOpts{id: "b", dataset: "d", model: "m2"})
	addCell(t, st, "a", cellOpts{task: "t", graded: true, passed: true, observed: "m1"})
	addCell(t, st, "b", cellOpts{task: "t", graded: true, passed: true, observed: "m2"})

	d, err := LoadComparison(context.Background(), st, "a", "b", Options{})
	if err != nil {
		t.Fatalf("LoadComparison: %v", err)
	}
	got := strings.Join(d.Warnings, "\n")
	if !strings.Contains(got, "differ in the model (m1 vs m2)") {
		t.Errorf("the declared difference must still be named, got:\n%s", got)
	}
	if strings.Contains(got, "which model served the requests") {
		t.Errorf("the model should be named once, not twice, got:\n%s", got)
	}
}

// TestTheRunHeaderSaysWhetherTheCellsWereChained covers the fact that changes
// what the cell list means.
//
// A chained run reports the same k and the same rows as an unchained one; what
// differs is that each cell started from the state its predecessor left. A page
// that renders the two identically invites the reader to average a sequence as
// though it were a set of repeats, which is the one reading that is certainly
// wrong.
func TestTheRunHeaderSaysWhetherTheCellsWereChained(t *testing.T) {
	for _, chained := range []bool{true, false} {
		st := newStore(t)
		addRun(t, st, runOpts{id: "r1", dataset: "d", chained: chained})
		d, err := Load(context.Background(), st, "r1", Options{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if d.Run.ChainSessions != chained {
			t.Errorf("chain_sessions = %v, want %v", d.Run.ChainSessions, chained)
		}
	}
}

// TestTheRecordingSaysWhoMadeItAndWhetherThatIsThisAgent covers the baseline
// card's provenance. A collected Cursor session replayed by Claude Code is a
// real comparison -- it is what replaying a recording means -- but its delta is
// the difference between two agents rather than the effect of one, and the card
// is the only place that can be said.
func TestTheRecordingSaysWhoMadeItAndWhetherThatIsThisAgent(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a", name: "replay", model: "sonnet"})
	addCell(t, st, "a", cellOpts{task: "t", graded: true, passed: true, base: &store.BaselineComparison{
		Present: true, ID: "sess-1", Tokens: 3000, Ms: 60000, Turns: 4,
		AgentName: "cursor", AgentModel: "grok-4.5",
		Source: "cursor-composer", Notes: "records no cost",
	}})

	b := taskByID(t, mustLoad(t, st, "a", Options{}), "t").Baseline
	if b == nil {
		t.Fatal("the cell lost its baseline")
	}
	if b.AgentName != "cursor" || b.AgentModel != "grok-4.5" || b.Source != "cursor-composer" {
		t.Errorf("the card does not name the recording: %+v", b)
	}
	// The caveats travel with the numbers they qualify, which is the whole
	// reason they are stored rather than left in the dataset.
	if b.Notes != "records no cost" {
		t.Errorf("notes = %q, want the recording's own caveat", b.Notes)
	}
	if !b.DifferentAgent {
		t.Error("a Cursor recording replayed by a claude_code run is a cross-agent comparison")
	}
}

// TestABaselineFromTheSameAgentIsNotCalledCrossAgent pins the folding, which is
// what keeps the cross-agent notice from firing on the ordinary case.
//
// The two names come from different layers and are spelled differently: a
// capture writes the producer's name ("claude-code"), while a run registers the
// implementation it drives ("claude_code"). A warning that fires on every
// collected-and-replayed Claude Code baseline is how the one real warning stops
// being read.
func TestABaselineFromTheSameAgentIsNotCalledCrossAgent(t *testing.T) {
	for _, tc := range []struct {
		recorded string
		differ   bool
	}{
		{"claude-code", false},
		{"claude_code", false},
		{"Claude Code", false},
		{"cursor", true},
		{"mock", true},
		// No record is not evidence of a different agent: the report says the
		// absence separately, and a notice here would be inventing one.
		{"", false},
	} {
		if got := !sameAgent(tc.recorded, "claude_code"); got != tc.differ {
			t.Errorf("a recording from %q compared against claude_code: cross-agent = %v, want %v",
				tc.recorded, got, tc.differ)
		}
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
	listing, err := LoadReports(context.Background(), st, Options{}, 0, "")
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
		{"reports", "reports", func(b *bytes.Buffer) error { return RenderReports(b, listing, RenderOptions{}) }},
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
	if err := RenderReports(&buf, nil, RenderOptions{}); err == nil {
		t.Error("rendering a nil report listing succeeded")
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

// ---------------------------------------------------------------------------
// The registry
// ---------------------------------------------------------------------------

// sampleRunCell is the smallest run a report can be made about.
func sampleRunCell(t *testing.T, st *store.Store) {
	t.Helper()
	addRun(t, st, runOpts{id: "r1", name: "nightly"})
	addCell(t, st, "r1", cellOpts{task: "t1", graded: true, passed: true})
}

// TestTheHashIsOverTheBytesTheEndpointServes is the invariant the whole
// registry rests on.
//
// The stored hash is only worth anything if a reader can check it against
// something they can fetch. That something is DataJSON, so hashing anything
// else -- the struct before encoding, the HTML, a pretty-printed form -- would
// make the hash a private number that happens to be reproducible inside this
// program and nowhere else.
func TestTheHashIsOverTheBytesTheEndpointServes(t *testing.T) {
	st := newStore(t)
	sampleRunCell(t, st)
	data := mustLoad(t, st, "r1", Options{})

	hash, size, err := Fingerprint(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := DataJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Errorf("fingerprint = %s, want sha256 of the served bytes %s", hash, want)
	}
	if size != len(raw) {
		t.Errorf("size = %d, want %d", size, len(raw))
	}
}

// TestARegistrationRebuildsToItsOwnHash is the claim a recorded report makes,
// exercised end to end: register a view, rebuild it from the registration
// alone, and get the same bytes.
func TestARegistrationRebuildsToItsOwnHash(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	sampleRunCell(t, st)

	opts := Options{Title: "nightly"}.Resolve()
	data := mustLoad(t, st, "r1", opts)
	reg, err := Save(ctx, st, Request{
		Kind: store.ReportKindRun, Title: data.Title, Subject: data.Title,
		Params: map[string]string{"run": "r1"}, Options: opts,
	}, data)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if reg.PayloadHash == "" || reg.PayloadBytes == 0 {
		t.Fatalf("the registration recorded no payload (%+v)", reg)
	}

	// Rebuild from nothing but the row, exactly as the server does.
	stored, err := st.GetReport(ctx, reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayOpts, err := ParseOptions(stored)
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	payload, mode, err := Derive(ctx, st, stored, replayOpts)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if mode != ModeRun {
		t.Errorf("mode = %q, want %q", mode, ModeRun)
	}
	again, _, err := Fingerprint(payload)
	if err != nil {
		t.Fatal(err)
	}
	if again != reg.PayloadHash {
		t.Errorf("rebuilding the report hashed to %s, but it was registered as %s\n"+
			"the registration does not describe the document it was made from", again, reg.PayloadHash)
	}
}

// TestARebuiltReportCarriesTheTimeItWasRecorded is why the options are stored
// rather than re-derived.
//
// The generation stamp is part of the document. Rebuilt with today's clock, the
// payload differs on every read and the stored hash would disagree always --
// making the drift check useless in exactly the case the feature exists for.
func TestARebuiltReportCarriesTheTimeItWasRecorded(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	sampleRunCell(t, st)

	recordedAt := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	opts := Options{GeneratedAt: recordedAt}.Resolve()
	data := mustLoad(t, st, "r1", opts)
	reg, err := Save(ctx, st, Request{
		Kind: store.ReportKindRun, Params: map[string]string{"run": "r1"}, Options: opts,
	}, data)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := st.GetReport(ctx, reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayOpts, err := ParseOptions(stored)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := Derive(ctx, st, stored, replayOpts)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := payload.(*Data)
	if !ok {
		t.Fatalf("payload is %T", payload)
	}
	if rebuilt.Generated != "2026-05-06T07:08:09Z" {
		t.Errorf("the rebuilt document says it was generated %q, want the time it was recorded",
			rebuilt.Generated)
	}
	if !reg.CreatedAt.Equal(recordedAt) {
		t.Errorf("created at = %s, want %s", reg.CreatedAt, recordedAt)
	}
}

// TestParseOptionsRefusesRatherThanGuessing covers the two ways a stored
// request can be unusable.
//
// Both must refuse. Falling back to the defaults would rebuild a different
// document and then present the difference as the data having moved -- a wrong
// diagnosis the reader has no way to check, and worse than an error page.
func TestParseOptionsRefusesRatherThanGuessing(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"not json", `{`},
		{"no generation time", `{"max_inline_bytes":1024}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := store.Report{ID: "deadbeefdeadbeef", OptionsJSON: tc.json}
			opts, err := ParseOptions(reg)
			if err == nil {
				t.Fatalf("accepted %q and produced %+v", tc.json, opts)
			}
			if !strings.Contains(err.Error(), reg.ID) {
				t.Errorf("err = %q, want it to name the registration", err)
			}
		})
	}
}

// TestDeriveRefusesARequestItCannotHonour covers the parameters.
//
// An empty projection and a projection of nothing look the same on the page, so
// a registration that does not say which run it was about has to be an error
// rather than a run report with no run in it.
func TestDeriveRefusesARequestItCannotHonour(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	sampleRunCell(t, st)
	opts := Options{}.Resolve()

	cases := []struct {
		name string
		reg  store.Report
		want string
	}{
		{"no parameter", store.Report{ID: "a", Kind: store.ReportKindRun, ParamsJSON: `{}`}, "run"},
		{"blank parameter", store.Report{ID: "b", Kind: store.ReportKindRun, ParamsJSON: `{"run":"  "}`}, "run"},
		{"half a comparison", store.Report{ID: "c", Kind: store.ReportKindCompare, ParamsJSON: `{"left":"r1"}`}, "right"},
		{"unknown kind", store.Report{ID: "d", Kind: "reward", ParamsJSON: `{}`}, "reward"},
		{"unreadable params", store.Report{ID: "e", Kind: store.ReportKindRun, ParamsJSON: `[1,2]`}, "e"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Derive(ctx, st, tc.reg, opts); err == nil {
				t.Fatal("Derive accepted a registration it cannot rebuild")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestAListingShowsStoredFactsAndFlagsOlderFormats covers the registry listing.
//
// It deliberately does not rebuild the payloads it lists: that would cost a
// full projection per row. So the flag for a payload written in another format
// has to come off the row, and it has to be there -- otherwise such a report
// reads as one whose data changed, which is a different and much louder claim.
func TestAListingShowsStoredFactsAndFlagsOlderFormats(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	sampleRunCell(t, st)
	opts := Options{}.Resolve()
	data := mustLoad(t, st, "r1", opts)
	if _, err := Save(ctx, st, Request{
		Kind: store.ReportKindRun, Title: "nightly", Subject: "nightly",
		Params: map[string]string{"run": "r1"}, Options: opts,
	}, data); err != nil {
		t.Fatal(err)
	}
	// A registration from a build with a different payload format.
	older := saveAs(t, st, store.Report{
		ID: "oldoldoldoldold1", Kind: store.ReportKindRun, Title: "older",
		Subject: "r0 / model=opus", ParamsJSON: `{"run":"r0"}`,
		OptionsJSON: `{}`, CreatedAt: time.Now().Add(-time.Hour),
		PayloadHash: "beefbeefbeefbeef", PayloadBytes: 4096,
		DataVersion: DataVersion - 1,
	})

	listing, err := LoadReports(ctx, st, opts, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("listing holds %d entries, want 2", len(listing.Entries))
	}
	byID := map[string]ReportEntry{}
	for _, e := range listing.Entries {
		byID[e.ID] = e
	}
	if got := byID[older.ID]; !got.OlderFormat {
		t.Errorf("entry %+v is not flagged as written in another format", got)
	}
	fresh := listing.Entries[0]
	if fresh.OlderFormat {
		t.Errorf("this build's own registration is flagged as an older format: %+v", fresh)
	}
	if fresh.Href != "/r/"+fresh.ID {
		t.Errorf("href = %q, want the id appended to /r/", fresh.Href)
	}
	if len(fresh.Hash) != 12 {
		t.Errorf("hash = %q, want it clipped to 12 characters for the listing", fresh.Hash)
	}
	if fresh.Bytes == 0 {
		t.Error("the listing does not say how big the recorded document was")
	}
	// The subject survives on its own: this entry names a run that does not
	// exist, which is exactly when the stored label is the only thing left.
	if byID[older.ID].Subject != "r0 / model=opus" {
		t.Errorf("subject = %q, want the label it was registered with", byID[older.ID].Subject)
	}
}

// TestAListingCarriesTheLinkPrefixItWasGiven is what lets a listing inlined
// into a file point somewhere other than at one server's route.
func TestAListingCarriesTheLinkPrefixItWasGiven(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	saveAs(t, st, store.Report{
		ID: "aaaaaaaaaaaaaaaa", Kind: store.ReportKindRun, ParamsJSON: `{}`,
		OptionsJSON: `{}`, CreatedAt: time.Now(), DataVersion: DataVersion,
	})
	listing, err := LoadReports(ctx, st, Options{}, 0, "/report/")
	if err != nil {
		t.Fatal(err)
	}
	if got := listing.Entries[0].Href; got != "/report/aaaaaaaaaaaaaaaa" {
		t.Errorf("href = %q", got)
	}
}

// saveAs stores a row verbatim, so a test can build a shape this build would
// not write -- an older payload format, an unreadable request.
func saveAs(t *testing.T, st *store.Store, r store.Report) store.Report {
	t.Helper()
	if err := st.SaveReport(context.Background(), r); err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	return r
}

// TestTheRecordBannerIsOutsideThePayload is what keeps the hash a claim about
// the document.
//
// The banner describes the page, so it cannot be inside the thing the page is a
// rendering of: with it in the payload, the document a reader opens would never
// be the document that was hashed, and the JSON endpoint would have to strip a
// field to be the same document twice.
func TestTheRecordBannerIsOutsideThePayload(t *testing.T) {
	st := newStore(t)
	sampleRunCell(t, st)
	data := mustLoad(t, st, "r1", Options{})
	before, err := DataJSON(data)
	if err != nil {
		t.Fatal(err)
	}

	rec := &Record{ID: "abc", Hash: "aaaa", CurrentHash: "bbbb",
		Severity: "warn", Head: "Moved.", Note: "it moved"}
	var buf bytes.Buffer
	if err := RenderRun(&buf, data, RenderOptions{Record: rec}); err != nil {
		t.Fatal(err)
	}
	doc := buf.String()
	if !strings.Contains(doc, "it moved") {
		t.Error("the banner did not reach the page")
	}
	if !strings.Contains(doc, `class="recordbar"`) {
		t.Error("the banner is not in the shell's own container")
	}
	// The payload embedded in the page is unchanged by the banner existing.
	after := extractPayload(t, doc)
	if after != string(before) {
		t.Errorf("the banner changed the embedded payload:\n before: %s\n  after: %s", before, after)
	}
	if !strings.Contains(doc, `data-mode="run"`) {
		t.Error("the banner replaced the mode instead of sitting beside it")
	}
}

// TestTheFailurePageCarriesNoClientScript covers the error shell.
//
// app.js renders "nothing has been recorded yet" when it finds no listing, so
// shipping it on a failure page would paint that over the reason the page
// exists.
func TestTheFailurePageCarriesNoClientScript(t *testing.T) {
	var buf bytes.Buffer
	err := RenderError(&buf, ErrorOptions{
		Title:    "No such report.",
		Message:  `No report with id "nope" in aeval.db.`,
		Home:     "/",
		HomeText: "All recorded reports",
	}, RenderOptions{})
	if err != nil {
		t.Fatalf("RenderError: %v", err)
	}
	doc := buf.String()
	for _, want := range []string{"No such report.", "aeval.db", `href="/"`, "app.css"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the failure page does not mention %q:\n%s", want, doc)
		}
	}
	for _, unwanted := range []string{"app.js", "__AEVAL_PAYLOAD__", `data-mode="reports"`} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("the failure page carries %q", unwanted)
		}
	}
}

// TestTheFailurePageEscapesWhatItIsGiven covers the one place a failure page
// renders something a user chose: a run name, which comes from a dataset, ends
// up inside a message about that run being gone.
func TestTheFailurePageEscapesWhatItIsGiven(t *testing.T) {
	var buf bytes.Buffer
	err := RenderError(&buf, ErrorOptions{
		Title:   "Gone.",
		Message: `<script>alert(1)</script> & "quoted"`,
		Home:    "/",
	}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := buf.String()
	if strings.Contains(doc, "<script>alert(1)</script>") {
		t.Error("the message reached the page as markup")
	}
	if !strings.Contains(doc, "&lt;script&gt;") {
		t.Errorf("the message was not escaped:\n%s", doc)
	}
}

// TestThePageSaysARunWorkedInYourRepositories covers the largest of the
// facts that change what a cell means.
//
// A run that worked in place put an agent inside a working repository: the
// history was readable, the rest of the tree was around it, and the run put the
// tree back afterwards. A page that shows it beside a copy-based run without
// saying so invites the reader to take the pair for the same experiment.
func TestThePageSaysARunWorkedInYourRepositories(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "d", inPlace: true})
	addCell(t, st, "r1", cellOpts{
		task: "t1", graded: true, passed: true,
		workspaceDir: "/work/proj",
		restoreNotes: "put back at 2d347614 in /work/proj; nothing was left behind",
	})
	d := mustLoad(t, st, "r1", Options{})
	if !d.Run.InPlace {
		t.Error("the payload must say the run worked in place")
	}
	task := taskByID(t, d, "t1")
	if task.WorkspaceDir != "/work/proj" {
		t.Errorf("workspace_dir = %q, want the directory the agent worked in", task.WorkspaceDir)
	}
	if !strings.Contains(task.RestoreNotes, "nothing was left behind") {
		t.Errorf("restore_notes = %q", task.RestoreNotes)
	}
	// The page renders the chip, which is what a reader actually sees.
	page := renderPage(t, d)
	if !strings.Contains(page, "in your repositories") {
		t.Error("the header must say the agent worked in the reader's repositories")
	}
}

// TestARestoreThatFailedIsVisibleOnThePage: the one thing here that is about
// the reader's own machine rather than about the measurement.
func TestARestoreThatFailedIsVisibleOnThePage(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "r1", dataset: "d", inPlace: true})
	addCell(t, st, "r1", cellOpts{
		task: "t1", graded: true, passed: true,
		workspaceDir: "/work/proj",
		restoreNotes: "the repository at /work/proj was NOT fully put back: check the worktree out: " +
			"error: Your local changes would be overwritten",
	})
	page := renderPage(t, mustLoad(t, st, "r1", Options{}))
	if !strings.Contains(page, "Not put back") {
		t.Error("a restore that failed must be rendered as a failure, not as a note")
	}
}

// TestAnInPlaceRunAndACopyAreNotTheSameMeasurement: the comparison has to name
// the difference, or the deltas beside it read as one agent beating another.
func TestAnInPlaceRunAndACopyAreNotTheSameMeasurement(t *testing.T) {
	st := newStore(t)
	addRun(t, st, runOpts{id: "a", dataset: "d", inPlace: false})
	addRun(t, st, runOpts{id: "b", dataset: "d", inPlace: true})
	addCell(t, st, "a", cellOpts{task: "t1", graded: true, passed: true})
	addCell(t, st, "b", cellOpts{task: "t1", graded: true, passed: false})
	d, err := LoadComparison(context.Background(), st, "a", "b", Options{})
	if err != nil {
		t.Fatalf("LoadComparison: %v", err)
	}
	joined := strings.Join(d.Warnings, "\n")
	if !strings.Contains(joined, "in the recorded directories themselves") {
		t.Errorf("the comparison must name the difference, got: %v", d.Warnings)
	}
	if !strings.Contains(joined, "in a copy vs in place") {
		t.Errorf("the comparison must say which side was which, got: %v", d.Warnings)
	}
}

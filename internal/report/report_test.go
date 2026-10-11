package report

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/storage"
	"github.com/skyitachi/agenteval/internal/trace"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type fixture struct {
	store *storage.Store
	runID string
}

// seedStore builds a store holding one finished run with n cells.
//
// The rows are written through the real storage layer rather than by hand, so a
// schema or column change breaks these tests too -- which is the point of
// projecting from the database instead of from memory.
func seedStore(t *testing.T, n int) *fixture {
	t.Helper()
	ctx := context.Background()

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	const runID = "run-fixture"
	run := &storage.RunRecord{
		ID: runID, Name: "fixture run", Status: storage.StatusRunning,
		DatasetName: "demo", DatasetVersion: "1.0.0", DatasetHash: "sha256:" + strings.Repeat("a", 32),
		DatasetPath: "/tmp/demo", AgentKind: "mock", AgentModel: "test-model",
		SandboxBackend: "local", SamplesPerTask: 1, Concurrency: 4,
		ConfigJSON: `{"agent":{"kind":"mock"}}`, ConfigHash: "deadbeef",
		ToolVersion: "test", HostArch: "darwin/arm64",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	for i := 0; i < n; i++ {
		passed := i%2 == 0
		taskID := "task-" + string(rune('a'+i))
		rec := buildTaskRun(t, runID, taskID, 0, passed, i)
		if err := store.SaveTaskRun(ctx, rec); err != nil {
			t.Fatalf("save task run %s: %v", taskID, err)
		}
	}
	if err := store.FinishRun(ctx, runID, storage.StatusCompleted, "", 90*time.Second); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	metrics := []storage.MetricRecord{
		{RunID: runID, Name: "resolve_rate", Value: 0.5, Unit: "ratio", Kind: "run", ExtraJSON: `{"passed":1,"total":2}`},
		{RunID: runID, Name: "total_cost_usd", Value: 1.25, Unit: "usd", Kind: "run"},
		{RunID: runID, Name: "cost_per_resolved_usd", Value: 0.4, Unit: "usd", Kind: "run"},
		{RunID: runID, Name: "harness_error_rate", Value: 0, Unit: "ratio", Kind: "run"},
		{RunID: runID, Name: "custom_ratio", Value: 0.75, Unit: "ratio", Kind: "run"},
	}
	if err := store.SaveMetrics(ctx, runID, metrics); err != nil {
		t.Fatalf("save metrics: %v", err)
	}
	return &fixture{store: store, runID: runID}
}

func buildTaskRun(t *testing.T, runID, taskID string, sample int, passed bool, i int) *storage.TaskRunRecord {
	t.Helper()
	started := time.Now().UTC().Add(-time.Duration(i+1) * time.Minute)
	ended := started.Add(time.Duration(i+1) * 30 * time.Second)

	tr := trace.Trace{
		SessionID: "sess-" + taskID,
		Model:     "test-model",
		TurnCount: 3,
		Events: []trace.Event{
			{Index: 0, Kind: trace.KindAssistantText, Text: "reading the repository"},
			{Index: 1, Kind: trace.KindToolUse, ToolUseID: "t1", ToolName: "Read", ToolInput: json.RawMessage(`{"path":"calc.py"}`)},
			{Index: 2, Kind: trace.KindToolResult, ToolUseID: "t1", ToolName: "Read", ToolOutput: "def add(a,b): ..."},
			{Index: 3, Kind: trace.KindAssistantThink, Text: "the bug is an off-by-one"},
			{Index: 4, Kind: trace.KindAssistantText, Text: "fixed"},
		},
		ToolCalls: []trace.ToolCall{
			{Seq: 1, ID: "t1", Name: "Read", Input: json.RawMessage(`{"path":"calc.py"}`), Output: "def add(a,b): ...", DurationMs: 12},
		},
		Result: trace.Result{
			Subtype:      "success",
			ResultText:   "done",
			SessionID:    "sess-" + taskID,
			NumTurns:     3,
			TotalCostUSD: 0.05,
			Usage:        trace.Usage{InputTokens: 1000, OutputTokens: 200},
			DurationMs:   30000,
		},
	}
	att := agent.Attempt{
		Agent: "mock", Model: "test-model", SessionID: "sess-" + taskID,
		CommandLine: "claude -p ...", ExitCode: 0,
		StartedAt: started, EndedAt: ended, Duration: ended.Sub(started),
		Trace: &tr,
	}
	grades := []grader.Result{
		{
			Grader: "hidden-suite", Type: "pytest", Passed: passed, Score: boolScore(passed),
			Weight: 0.8, Required: true, Reason: reason(passed),
			Details: map[string]any{"passed": 6, "failed": 0},
		},
		{
			Grader: "patch-applied", Type: "patch", Passed: passed, Score: boolScore(passed),
			Weight: 0.2, Reason: reason(passed), Duration: 3 * time.Millisecond,
		},
	}

	return &storage.TaskRunRecord{
		ID: runID + "/" + storage.TaskKey(taskID, sample), RunID: runID, TaskID: taskID,
		SampleIndex: sample, Category: category(i), Difficulty: "easy", Tags: []string{"python"},
		Status: storage.TaskStatusCompleted, StopReason: "completed", Passed: passed,
		Score: boolScore(passed), Threshold: 1.0,
		SessionID: "sess-" + taskID, Model: "test-model",
		StartedAt: &started, EndedAt: &ended, DurationMs: ended.Sub(started).Milliseconds(),
		CostUSD: 0.05, CostReliable: true, InputTokens: 1000, OutputTokens: 200,
		Turns: 3, ToolCalls: 1, ToolErrors: 0, TTFTMs: 120, APIDurationMs: 20000,
		Diff:        "diff --git a/calc.py b/calc.py\n--- a/calc.py\n+++ b/calc.py\n@@ -1 +1 @@\n-a\n+b\n",
		Answer:      "done",
		AttemptJSON: mustJSON(t, att),
		TraceJSON:   mustJSON(t, tr),
		GradesJSON:  mustJSON(t, grades),
	}
}

func boolScore(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func reason(passed bool) string {
	if passed {
		return "all checks passed"
	}
	return "2 assertion(s) failed"
}

func category(i int) string {
	if i%2 == 0 {
		return "bugfix"
	}
	return "feature"
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

func TestLoadProjectsTheRunFaithfully(t *testing.T) {
	fx := seedStore(t, 4)
	data, err := Load(context.Background(), fx.store, fx.runID, Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if data.Run.ID != fx.runID || data.Run.Status != storage.StatusCompleted {
		t.Fatalf("run identity lost: %+v", data.Run)
	}
	if data.Run.AgentModel != "test-model" || data.Run.SandboxBackend != "local" {
		t.Fatalf("run provenance lost: %+v", data.Run)
	}
	if data.Counts.Samples != 4 || data.Counts.Tasks != 4 {
		t.Fatalf("counts = %+v", data.Counts)
	}
	if data.Counts.Passed != 2 || data.Counts.TasksPassed != 2 {
		t.Fatalf("expected 2 of 4 passing, got %+v", data.Counts)
	}
	if data.Title == "" {
		t.Fatal("title should default rather than be empty")
	}
	if data.Version != DataVersion {
		t.Fatalf("version = %d", data.Version)
	}
	if len(data.Tasks) != 4 {
		t.Fatalf("tasks = %d", len(data.Tasks))
	}

	// Task order must follow the store's deterministic ordering so two reports
	// of the same run line up row for row.
	for i, task := range data.Tasks {
		want := "task-" + string(rune('a'+i))
		if task.ID != want {
			t.Fatalf("task %d = %s, want %s", i, task.ID, want)
		}
		if task.TotalTokens != 1200 {
			t.Fatalf("token total = %d, want 1200", task.TotalTokens)
		}
		if task.ExitCode == nil || *task.ExitCode != 0 {
			t.Fatalf("exit code lost for %s", task.ID)
		}
	}

	// Grader order comes from the seq column, not from insertion luck.
	first := data.Tasks[0]
	if len(first.Graders) != 2 {
		t.Fatalf("graders = %d", len(first.Graders))
	}
	if first.Graders[0].Type != "pytest" || first.Graders[1].Type != "patch" {
		t.Fatalf("grader order not preserved: %s then %s", first.Graders[0].Type, first.Graders[1].Type)
	}
	if first.Graders[0].Details["passed"] != float64(6) {
		t.Fatalf("grader details lost: %+v", first.Graders[0].Details)
	}
}

// The report is the only place a reader can check what a training loop would
// have been paid, and the mode is the only thing that stops an oracle run from
// being read as a model score. Both live on the stored row, so this asserts
// they survive the projection into the view model rather than being dropped
// silently along the way.
func TestLoadProjectsTheRewardContractAndTheMode(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	const runID = "run-oracle"
	run := &storage.RunRecord{
		ID: runID, Name: "oracle probe", Status: storage.StatusRunning, Mode: "oracle",
		DatasetName: "demo", DatasetHash: "sha256:" + strings.Repeat("b", 32),
		AgentKind: "mock", AgentModel: "test-model", SandboxBackend: "local",
		SamplesPerTask: 1, Concurrency: 1, ToolVersion: "test",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	rec := buildTaskRun(t, runID, "task-a", 0, false, 0)
	rec.Score = 0.4
	rec.Reward, rec.RewardKind = 0, "pass"
	rec.Violations = []string{"forbidden_path: tests/helper.py"}
	rec.Restored = []string{"tests/helper.py"}
	if err := store.SaveTaskRun(ctx, rec); err != nil {
		t.Fatalf("save task run: %v", err)
	}
	if err := store.FinishRun(ctx, runID, storage.StatusCompleted, "", time.Second); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	data, err := Load(ctx, store, runID, Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if data.Run.Mode != "oracle" {
		t.Errorf("run mode = %q, want oracle; without it the report cannot say who acted", data.Run.Mode)
	}

	got := data.Tasks[0]
	if got.Score != 0.4 || got.Reward != 0 || got.RewardKind != "pass" {
		t.Errorf("score/reward = %v / %v / %q, want 0.4 / 0 / pass", got.Score, got.Reward, got.RewardKind)
	}
	if !got.Rejected {
		t.Error("an attempt with violations must be flagged as rejected; a zero reward alone does not say why")
	}
	if len(got.Violations) != 1 || len(got.Restored) != 1 {
		t.Errorf("guards lost in projection: violations=%v restored=%v", got.Violations, got.Restored)
	}
}

func TestLoadSurvivesMissingAttemptAndTrace(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	run := &storage.RunRecord{ID: "r", Status: storage.StatusRunning, DatasetName: "d"}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// A cell the harness failed to measure: no attempt, no trace, no graders.
	rec := &storage.TaskRunRecord{
		ID: "r/t#0", RunID: "r", TaskID: "t", SampleIndex: 0,
		Status: storage.TaskStatusFailed, StopReason: "harness_error",
		Error: "agent could not be started",
	}
	if err := store.SaveTaskRun(ctx, rec); err != nil {
		t.Fatal(err)
	}

	data, err := Load(ctx, store, "r", Options{})
	if err != nil {
		t.Fatalf("a cell with no attempt must not fail the report: %v", err)
	}
	if len(data.Tasks) != 1 || len(data.Warnings) != 0 {
		t.Fatalf("tasks=%d warnings=%v", len(data.Tasks), data.Warnings)
	}
	task := data.Tasks[0]
	if task.ExitCode != nil || len(task.Events) != 0 || len(task.Graders) != 0 {
		t.Fatalf("unexpected content: %+v", task)
	}
	if data.Counts.Failed != 1 || data.Counts.Errored != 1 {
		t.Fatalf("counts = %+v", data.Counts)
	}
}

func TestLoadReportsUnreadableBlobsInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	_ = store.CreateRun(ctx, &storage.RunRecord{ID: "r", Status: storage.StatusRunning, DatasetName: "d"})

	rec := &storage.TaskRunRecord{
		ID: "r/t#0", RunID: "r", TaskID: "t", Status: storage.TaskStatusCompleted,
		AttemptJSON: "{not json", TraceJSON: "also not json",
	}
	if err := store.SaveTaskRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	data, err := Load(ctx, store, "r", Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(data.Warnings) != 2 {
		t.Fatalf("expected one warning per unreadable blob, got %v", data.Warnings)
	}
	for _, w := range data.Warnings {
		if !strings.Contains(w, "t#0") {
			t.Fatalf("warning should name the cell: %q", w)
		}
	}
}

func TestLoadRejectsNilStore(t *testing.T) {
	if _, err := Load(context.Background(), nil, "x", Options{}); err == nil {
		t.Fatal("expected an error for a nil store")
	}
	if _, err := LoadIndex(context.Background(), nil, 10, Options{}); err == nil {
		t.Fatal("expected an error for a nil store")
	}
}

// ---------------------------------------------------------------------------
// Truncation and budget
// ---------------------------------------------------------------------------

func TestTrajectoryKeepsHeadAndTail(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	_ = store.CreateRun(ctx, &storage.RunRecord{ID: "r", Status: storage.StatusRunning, DatasetName: "d"})

	var events []trace.Event
	for i := 0; i < 100; i++ {
		events = append(events, trace.Event{Index: i, Kind: trace.KindAssistantText, Text: "step " + string(rune('0'+i%10))})
	}
	tr := trace.Trace{Events: events, Result: trace.Result{ResultText: "final"}}
	rec := &storage.TaskRunRecord{
		ID: "r/t#0", RunID: "r", TaskID: "t", Status: storage.TaskStatusCompleted,
		TraceJSON: mustJSON(t, tr),
	}
	if err := store.SaveTaskRun(ctx, rec); err != nil {
		t.Fatal(err)
	}

	data, err := Load(ctx, store, "r", Options{MaxEventsPerTask: 10})
	if err != nil {
		t.Fatal(err)
	}
	task := data.Tasks[0]
	if len(task.Events) != 11 { // 10 kept + the gap marker
		t.Fatalf("events = %d, want 11", len(task.Events))
	}
	if task.Events[0].Index != 0 {
		t.Fatalf("the head was dropped: first index %d", task.Events[0].Index)
	}
	if last := task.Events[len(task.Events)-1]; last.Index != 99 {
		t.Fatalf("the tail was dropped: last index %d", last.Index)
	}
	gap := task.Events[5]
	if gap.Kind != "truncated" || !strings.Contains(gap.Text, "omitted") {
		t.Fatalf("expected an explicit gap marker, got %+v", gap)
	}
	if !strings.Contains(task.Detail, "truncated") {
		t.Fatalf("truncation must be recorded on the task, got %q", task.Detail)
	}
}

func TestBudgetDropsPassingDetailBeforeFailingDetail(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	_ = store.CreateRun(ctx, &storage.RunRecord{ID: "r", Status: storage.StatusRunning, DatasetName: "d"})

	big := strings.Repeat("x", 4000)
	for i := 0; i < 6; i++ {
		passed := i != 2 // exactly one failure
		id := "t" + string(rune('0'+i))
		rec := &storage.TaskRunRecord{
			ID: "r/" + id + "#0", RunID: "r", TaskID: id, Status: storage.TaskStatusCompleted,
			Passed: passed, Diff: big, Answer: big,
			GradesJSON: mustJSON(t, []grader.Result{{Grader: "g", Type: "patch", Passed: passed}}),
		}
		if err := store.SaveTaskRun(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	// Force a budget that holds only a fraction of the detail.
	data, err := Load(ctx, store, "r", Options{MaxInlineBytes: 12000})
	if err != nil {
		t.Fatal(err)
	}
	var failing, passingWithDetail, passingWithout int
	for i := range data.Tasks {
		task := &data.Tasks[i]
		hasDetail := task.Diff != "" || task.Answer != ""
		if !task.Passed {
			if !hasDetail {
				t.Fatalf("failing cell %s lost its detail", task.ID)
			}
			failing++
			continue
		}
		if hasDetail {
			passingWithDetail++
		} else {
			passingWithout++
			if !strings.Contains(task.Detail, "budget") {
				t.Fatalf("dropped detail must say why: %q", task.Detail)
			}
		}
	}
	if failing != 1 {
		t.Fatalf("expected 1 failing cell, found %d", failing)
	}
	if passingWithout == 0 {
		t.Fatalf("expected the budget to force some cells to lose detail (with=%d)", passingWithDetail)
	}
	if len(data.Warnings) == 0 {
		t.Fatal("a lossy budget must produce a warning")
	}
	if !strings.Contains(strings.Join(data.Warnings, " "), "budget") {
		t.Fatalf("warning should explain the budget: %v", data.Warnings)
	}
}

func TestBudgetTooSmallForSummariesIsReported(t *testing.T) {
	fx := seedStore(t, 4)
	data, err := Load(context.Background(), fx.store, fx.runID, Options{MaxInlineBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Warnings) == 0 {
		t.Fatal("an impossible budget must be reported, not silently applied")
	}
	// The rows survive regardless: a report that cannot show detail is still
	// useful, a report that cannot show which cells failed is not.
	if len(data.Tasks) != 4 {
		t.Fatalf("rows must survive: %d", len(data.Tasks))
	}
}

func TestClipIsRuneSafe(t *testing.T) {
	var task Task
	// "中" is three bytes; a limit of 4 would otherwise split it.
	out := clip("中中中", 4, &task)
	if !utf8.ValidString(out) {
		t.Fatalf("clipped text is not valid UTF-8: %q", out)
	}
	if !strings.HasPrefix(out, "中") {
		t.Fatalf("clipped text lost the first rune: %q", out)
	}
	if !strings.Contains(task.Detail, "clipped") {
		t.Fatalf("clipping must be recorded, got %q", task.Detail)
	}
}

func TestClipLeavesShortTextAlone(t *testing.T) {
	var task Task
	if got := clip("short", 100, &task); got != "short" {
		t.Fatalf("got %q", got)
	}
	if task.Detail != "" {
		t.Fatalf("no truncation should be recorded, got %q", task.Detail)
	}
}

// ---------------------------------------------------------------------------
// Metrics, breakdowns, counts
// ---------------------------------------------------------------------------

// TestDeclaredMetricHintsBeatTheNameHeuristic covers the only place a user can
// correct a wrong colour: a custom metric's name carries no information about
// which direction is an improvement, and guessing "up" for "slow_cells" would
// render a regression as green.
func TestDeclaredMetricHintsBeatTheNameHeuristic(t *testing.T) {
	rows := []storage.MetricRecord{
		{Name: "slow_cells", Value: 3, Unit: "count", Kind: "run",
			ExtraJSON: `{"samples":4,"agg":"count","direction":"down","group":"latency"}`},
		{Name: "cost_saved_usd", Value: 1.5, Unit: "usd", Kind: "run",
			ExtraJSON: `{"direction":"up","group":"cost"}`},
		// No hints declared: the heuristic still applies, and the computed
		// extras must survive untouched.
		{Name: "hard_resolve_rate", Value: 0.5, Unit: "ratio", Kind: "run",
			ExtraJSON: `{"samples":4,"field":"passed","agg":"mean"}`},
		{Name: "unparseable", Value: 1, Unit: "count", Kind: "run",
			ExtraJSON: `{not json`},
	}
	got := map[string]Metric{}
	for _, m := range metricsFrom(rows) {
		got[m.Name] = m
	}

	if m := got["slow_cells"]; m.Direction != "down" || m.Group != "latency" {
		t.Errorf("slow_cells = %s/%s, want down/latency", m.Direction, m.Group)
	}
	if m := got["slow_cells"]; m.Extra["agg"] != "count" {
		t.Errorf("declared hints displaced the computed extras: %v", m.Extra)
	}
	// "cost_saved_usd" contains "cost", which the heuristic would read as
	// lower-is-better. The declaration wins.
	if m := got["cost_saved_usd"]; m.Direction != "up" {
		t.Errorf("cost_saved_usd direction = %q, want up (declared)", m.Direction)
	}
	if m := got["hard_resolve_rate"]; m.Direction != "up" || m.Group != "other" {
		t.Errorf("undeclared metric = %s/%s, want the heuristic's up/other", m.Direction, m.Group)
	}
	// A metric whose extras cannot be decoded must still render.
	if m := got["unparseable"]; m.Display == "" || m.Direction == "" {
		t.Errorf("undecodable extras broke rendering: %+v", m)
	}

	// The declared group also drives ordering, so a "latency" custom metric
	// sits with the latency section rather than at the end.
	if groupRank(got["slow_cells"].Group) != groupRank("latency") {
		t.Errorf("declared group did not affect ordering: %q", got["slow_cells"].Group)
	}
}

func TestMetricGroupingAndDirection(t *testing.T) {
	cases := map[string]struct{ group, direction string }{
		"resolve_rate":          {"outcome", "up"},
		"total_cost_usd":        {"cost", "down"},
		"cost_per_resolved_usd": {"cost", "down"},
		"p95_latency_ms":        {"latency", "down"},
		"timeout_rate":          {"health", "down"},
		"harness_error_rate":    {"health", "down"},
		"mean_total_tokens":     {"token", "down"},
		"task_count":            {"outcome", "neutral"},
		"some_custom_metric":    {"other", "up"},
	}
	for name, want := range cases {
		if got := metricGroup(name); got != want.group {
			t.Errorf("%s group = %q, want %q", name, got, want.group)
		}
		if got := metricDirection(name); got != want.direction {
			t.Errorf("%s direction = %q, want %q", name, got, want.direction)
		}
	}
}

func TestMetricsCarryRegistryDescriptionsAndSortByGroup(t *testing.T) {
	fx := seedStore(t, 4)
	data, err := Load(context.Background(), fx.store, fx.runID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var sawKnown, sawCustom bool
	rank := map[string]int{}
	for _, m := range data.Metrics {
		rank[m.Name] = groupRank(m.Group)
		if m.Name == "resolve_rate" {
			if m.Description == "" {
				t.Error("a registered metric should carry its description")
			}
			sawKnown = true
		}
		if m.Name == "custom_ratio" {
			// A custom metric is not in the registry, so it has no description
			// and lands in the catch-all group.
			if m.Group != "other" {
				t.Errorf("custom metric group = %q", m.Group)
			}
			sawCustom = true
		}
		if m.Display == "" {
			t.Errorf("%s has no display form", m.Name)
		}
	}
	if !sawKnown || !sawCustom {
		t.Fatalf("missing metrics: known=%v custom=%v", sawKnown, sawCustom)
	}
	for i := 1; i < len(data.Metrics); i++ {
		if rank[data.Metrics[i-1].Name] > rank[data.Metrics[i].Name] {
			t.Fatalf("metrics are not ordered by group: %v", data.Metrics)
		}
	}
}

func TestBreakdownsAreBuiltFromCellsNotMetrics(t *testing.T) {
	// The dataset may configure no breakdown metric at all; the report still
	// needs the split, so it derives one from the cells.
	fx := seedStore(t, 4)
	data, err := Load(context.Background(), fx.store, fx.runID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]Breakdown{}
	for _, b := range data.Breakdowns {
		byTitle[b.Title] = b
	}
	for _, want := range []string{"Resolve rate by category", "Resolve rate by difficulty", "Stop reason mix", "Tool usage", "Score distribution"} {
		if _, ok := byTitle[want]; !ok {
			t.Errorf("missing breakdown %q", want)
		}
	}
	cat := byTitle["Resolve rate by category"]
	if len(cat.Rows) != 2 {
		t.Fatalf("category rows = %d, want 2", len(cat.Rows))
	}
	// The fixture makes every bugfix cell pass and every feature cell fail, so
	// the split must be total, not average: deriving it from the cells rather
	// than from a configured metric is the whole point.
	got := map[string]float64{}
	tallies := map[string][2]int{}
	for _, row := range cat.Rows {
		got[row.Label] = row.Value
		tallies[row.Label] = [2]int{row.Hits, row.Total}
	}
	if got["bugfix"] != 1 || got["feature"] != 0 {
		t.Fatalf("category rates = %v, want bugfix 1.0 / feature 0.0", got)
	}
	if tallies["bugfix"] != [2]int{2, 2} || tallies["feature"] != [2]int{0, 2} {
		t.Fatalf("category tallies = %v", tallies)
	}
}

func TestCountsSeparateSamplesFromTasks(t *testing.T) {
	tasks := []Task{
		{ID: "a", Passed: true},
		{ID: "a", Passed: false},
		{ID: "b", Passed: true},
		{ID: "c", Status: storage.TaskStatusSkipped},
		{ID: "d", Status: storage.TaskStatusFailed, Error: "boom"},
	}
	c := counts(tasks, 2)
	if c.Samples != 5 || c.Tasks != 4 {
		t.Fatalf("samples/tasks = %d/%d", c.Samples, c.Tasks)
	}
	if c.Passed != 2 || c.Failed != 2 || c.Skipped != 1 || c.Errored != 1 {
		t.Fatalf("tally = %+v", c)
	}
	// A task counts as resolved when any of its samples passed, which is the
	// pass@k reading; the alternative would understate capability.
	if c.TasksPassed != 2 {
		t.Fatalf("tasks passed = %d, want 2 (a and b)", c.TasksPassed)
	}
}

// ---------------------------------------------------------------------------
// Artifact bundle
// ---------------------------------------------------------------------------

func TestDefinitionIsRecoveredFromTheArtifactBundle(t *testing.T) {
	dir := t.TempDir()
	spec := struct {
		dataset.TaskSpec
		Baseline string `json:"_baseline"`
	}{}
	spec.ID = "t"
	spec.Prompt = "fix the bug"
	spec.Workspace = dataset.Workspace{Type: dataset.WorkspaceFiles, Assets: "assets/x"}
	spec.Output = &dataset.Output{Type: dataset.OutputPatch, Paths: []string{"calc.py"}}
	spec.Limits = &dataset.Limits{MaxTurns: 20, TimeoutS: 420}
	spec.Graders = []dataset.GraderSpec{{Type: "pytest", Name: "visible", Required: true}}
	spec.Metadata = map[string]any{"demo": true}
	spec.Baseline = "abc123"

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	def := definitionOf(dir)
	if def == nil {
		t.Fatal("definition not recovered")
	}
	if def.Prompt != "fix the bug" {
		t.Fatalf("prompt = %q", def.Prompt)
	}
	if !strings.Contains(def.Workspace, "files") || !strings.Contains(def.Workspace, "assets/x") {
		t.Fatalf("workspace = %q", def.Workspace)
	}
	if !strings.Contains(def.Output, "patch") || !strings.Contains(def.Output, "calc.py") {
		t.Fatalf("output = %q", def.Output)
	}
	if !strings.Contains(def.Limits, "max_turns=20") || !strings.Contains(def.Limits, "420s") {
		t.Fatalf("limits = %q", def.Limits)
	}
	if len(def.Graders) != 1 || !strings.Contains(def.Graders[0], "required") {
		t.Fatalf("graders = %v", def.Graders)
	}
	if def.Baseline != "abc123" {
		t.Fatalf("baseline = %q", def.Baseline)
	}
}

func TestDefinitionIsAbsentWithoutABundle(t *testing.T) {
	if got := definitionOf(""); got != nil {
		t.Fatalf("expected nil for an empty directory, got %+v", got)
	}
	if got := definitionOf(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("expected nil for a missing bundle, got %+v", got)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := definitionOf(dir); got != nil {
		t.Fatalf("expected nil for an unparsable bundle, got %+v", got)
	}
}

func TestArtifactDirPrefersTheExplicitMapping(t *testing.T) {
	row := storage.TaskRunRecord{TaskID: "t", SampleIndex: 0, RawStreamPath: "/derived/stream.jsonl"}
	if got := artifactDir(row, Options{}); got != "/derived" {
		t.Fatalf("derived = %q", got)
	}
	opts := Options{Artifacts: map[string]string{"t#0": "/explicit"}}
	if got := artifactDir(row, opts); got != "/explicit" {
		t.Fatalf("explicit = %q", got)
	}
	// A cell with no stream still resolves through the mapping.
	bare := storage.TaskRunRecord{TaskID: "t", SampleIndex: 0}
	if got := artifactDir(bare, opts); got != "/explicit" {
		t.Fatalf("mapped-without-stream = %q", got)
	}
	if got := artifactDir(bare, Options{}); got != "" {
		t.Fatalf("unmapped = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

func TestCompareClassifiesEveryTransition(t *testing.T) {
	left := &Data{
		Run: RunInfo{ID: "L", Name: "before"},
		Tasks: []Task{
			{Key: "t1#0", ID: "t1", Passed: false, Status: storage.TaskStatusCompleted},
			{Key: "t2#0", ID: "t2", Passed: true, Status: storage.TaskStatusCompleted},
			{Key: "t3#0", ID: "t3", Passed: true, Status: storage.TaskStatusCompleted},
			{Key: "t4#0", ID: "t4", Passed: false, Status: storage.TaskStatusCompleted},
		},
	}
	right := &Data{
		Run: RunInfo{ID: "R", Name: "after"},
		Tasks: []Task{
			{Key: "t1#0", ID: "t1", Passed: true, Status: storage.TaskStatusCompleted},
			{Key: "t2#0", ID: "t2", Passed: false, Status: storage.TaskStatusCompleted},
			{Key: "t3#0", ID: "t3", Passed: true, Status: storage.TaskStatusCompleted},
			{Key: "t4#0", ID: "t4", Passed: false, Status: storage.TaskStatusCompleted},
			{Key: "t5#0", ID: "t5", Passed: true, Status: storage.TaskStatusCompleted},
		},
	}
	cmp := compare(left, right)

	want := map[string]string{
		"t1#0": "fixed", "t2#0": "regressed", "t3#0": "both_pass",
		"t4#0": "both_fail", "t5#0": "added",
	}
	if len(cmp.Tasks) != 5 {
		t.Fatalf("tasks = %d, want the union", len(cmp.Tasks))
	}
	for _, d := range cmp.Tasks {
		if want[d.Key] != d.Verdict {
			t.Errorf("%s verdict = %s, want %s", d.Key, d.Verdict, want[d.Key])
		}
	}
	if cmp.Summary.Fixed != 1 || cmp.Summary.Regressed != 1 {
		t.Fatalf("summary = %+v", cmp.Summary)
	}
	if cmp.Summary.Common != 4 || cmp.Summary.OnlyRight != 1 {
		t.Fatalf("summary = %+v", cmp.Summary)
	}
	if cmp.LeftLabel != "before" || cmp.RightLabel != "after" {
		t.Fatalf("labels = %q / %q", cmp.LeftLabel, cmp.RightLabel)
	}
}

func TestComparisonWarnsAcrossDatasetRevisions(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()

	for _, id := range []string{"a", "b"} {
		hash := "sha256:" + strings.Repeat("a", 32)
		if id == "b" {
			hash = "sha256:" + strings.Repeat("b", 32)
		}
		_ = store.CreateRun(ctx, &storage.RunRecord{ID: id, Status: storage.StatusCompleted, DatasetName: "d", DatasetHash: hash})
	}

	data, err := LoadComparison(ctx, store, "a", "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Warnings) == 0 {
		t.Fatal("comparing two dataset revisions must be flagged")
	}
	if !strings.Contains(strings.Join(data.Warnings, " "), "different dataset revisions") {
		t.Fatalf("warning = %v", data.Warnings)
	}

	same, err := LoadComparison(ctx, store, "a", "a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Warnings) != 0 {
		t.Fatalf("identical revisions should not warn: %v", same.Warnings)
	}
}

func TestCompareMetricVerdictsRespectDirection(t *testing.T) {
	left := &Data{Metrics: []Metric{
		{Name: "resolve_rate", Value: 0.5, Unit: "ratio", Display: "0.5000", Direction: "up"},
		{Name: "total_cost_usd", Value: 10, Unit: "usd", Display: "$10.0000", Direction: "down"},
		{Name: "task_count", Value: 4, Unit: "count", Display: "4", Direction: "neutral"},
		{Name: "only_here", Value: 1, Unit: "ratio", Display: "1.0000", Direction: "up"},
	}}
	right := &Data{Metrics: []Metric{
		{Name: "resolve_rate", Value: 0.75, Unit: "ratio", Display: "0.7500", Direction: "up"},
		{Name: "total_cost_usd", Value: 8, Unit: "usd", Display: "$8.0000", Direction: "down"},
		{Name: "task_count", Value: 6, Unit: "count", Display: "6", Direction: "neutral"},
		{Name: "brand_new", Value: 2, Unit: "ratio", Display: "2.0000", Direction: "up"},
	}}

	got := map[string]string{}
	for _, d := range compareMetrics(left, right) {
		got[d.Name] = d.Verdict
	}
	want := map[string]string{
		"resolve_rate":   "better",  // rose, higher is better
		"total_cost_usd": "better",  // fell, lower is better
		"task_count":     "same",    // neutral: more tasks is not an improvement
		"only_here":      "unknown", // present on one side only
		"brand_new":      "unknown",
	}
	for name, verdict := range want {
		if got[name] != verdict {
			t.Errorf("%s verdict = %q, want %q", name, got[name], verdict)
		}
	}
}

func TestVerdictIgnoresFloatNoise(t *testing.T) {
	// A 1e-15 difference is not an improvement, and reporting it as one would
	// make every comparison page look noisy.
	if v := verdict(1e-15, "up"); v != "same" {
		t.Fatalf("verdict = %q", v)
	}
	if v := verdict(math.SmallestNonzeroFloat64, "down"); v != "same" {
		t.Fatalf("verdict = %q", v)
	}
}

// ---------------------------------------------------------------------------
// Index
// ---------------------------------------------------------------------------

func TestLoadIndexListsRunsNewestFirst(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	for _, id := range []string{"old", "new"} {
		rec := &storage.RunRecord{ID: id, Status: storage.StatusCompleted, DatasetName: "d"}
		if id == "old" {
			rec.CreatedAt = time.Now().Add(-time.Hour)
		}
		if err := store.CreateRun(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := LoadIndex(ctx, store, 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Runs) != 2 || idx.Runs[0].ID != "new" {
		t.Fatalf("runs = %+v", idx.Runs)
	}
	if idx.Stats == nil || idx.Stats["runs"] != 2 {
		t.Fatalf("stats = %v", idx.Stats)
	}
	if idx.Title == "" || idx.Generated == "" {
		t.Fatalf("index metadata missing: %+v", idx)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func renderToString(t *testing.T, fn func(io.Writer) error) string {
	t.Helper()
	var b strings.Builder
	if err := fn(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestStandaloneReportIsOneFile(t *testing.T) {
	fx := seedStore(t, 3)
	data, err := Load(context.Background(), fx.store, fx.runID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := renderToString(t, func(w io.Writer) error {
		return RenderRun(w, data, RenderOptions{Standalone: true})
	})

	if strings.Contains(html, "<script src=") {
		t.Error("a standalone report must not reference an external script")
	}
	if strings.Contains(html, `rel="stylesheet"`) {
		t.Error("a standalone report must not reference an external stylesheet")
	}
	if !strings.Contains(html, "window.__AEVAL_PAYLOAD__ =") {
		t.Error("payload assignment missing")
	}
	if !strings.Contains(html, "--accent") {
		t.Error("stylesheet was not inlined")
	}
	if !strings.Contains(html, "aeval:raw") {
		t.Error("client script was not inlined")
	}
	if !strings.Contains(html, `data-mode="run"`) {
		t.Error("mode attribute missing")
	}
	if !strings.Contains(html, fx.runID) {
		t.Error("run id missing from the payload")
	}
}

func TestServedReportReferencesAssetsExternally(t *testing.T) {
	fx := seedStore(t, 2)
	data, _ := Load(context.Background(), fx.store, fx.runID, Options{})
	html := renderToString(t, func(w io.Writer) error {
		return RenderRun(w, data, RenderOptions{
			Standalone: false,
			Nav:        []NavLink{{Label: "Runs", Href: "/"}, {Label: "This run", Href: "/runs/x", Active: true}},
			Home:       "/",
		})
	})
	if !strings.Contains(html, `href="/static/app.css"`) {
		t.Error("stylesheet link missing")
	}
	if !strings.Contains(html, `src="/static/app.js"`) {
		t.Error("script link missing")
	}
	if strings.Contains(html, "--accent") {
		t.Error("the stylesheet should not be inlined when served")
	}
	if !strings.Contains(html, "Runs") || !strings.Contains(html, "This run") {
		t.Error("navigation links missing")
	}
	if !strings.Contains(html, `class="active"`) {
		t.Error("active navigation state missing")
	}
}

// TestPayloadCannotBreakOutOfItsScriptElement is the report's XSS regression
// test. Every value in a payload is agent- or model-generated, and the report is
// opened in a browser, so a payload that can terminate its own script element
// would be a stored-XSS primitive.
func TestPayloadCannotBreakOutOfItsScriptElement(t *testing.T) {
	hostile := "</script><script>alert(1)</script><!--<img src=x onerror=alert(2)>"
	long := strings.Repeat("&amp;&lt;", 500)

	data := &Data{
		Title: hostile,
		Run:   RunInfo{ID: "r", Name: hostile, Notes: long, Error: hostile},
		Tasks: []Task{{
			Key: "t#0", ID: hostile, Category: hostile, StopReason: hostile,
			Error:      hostile,
			Diff:       hostile + "\n" + long,
			Answer:     hostile,
			Events:     []Event{{Index: 0, Kind: "assistant_text", Text: hostile}, {Index: 1, Kind: "tool_use", ToolName: hostile, ToolInput: hostile, ToolOut: long}},
			Graders:    []Grader{{Grader: hostile, Type: hostile, Reason: hostile, Details: map[string]any{"x": hostile}}},
			Tools:      []ToolCall{{Seq: 1, Name: hostile, Input: hostile, Output: hostile}},
			Definition: &Definition{Prompt: hostile, Raw: hostile},
		}},
		Warnings: []string{hostile},
	}
	html := renderToString(t, func(w io.Writer) error {
		return RenderRun(w, data, RenderOptions{Standalone: true})
	})

	segment, err := payloadSegment(html)
	if err != nil {
		t.Fatalf("payload segment: %v", err)
	}
	if strings.Contains(segment, "<") {
		t.Fatalf("payload still contains '<': %q", firstN(segment, 200))
	}
	// With no "<" available, no tag can be opened and nothing in the payload can
	// escape the script element or start an HTML comment. Checking for the two
	// sequences that would end it is the direct assertion of that property;
	// strings like "onerror=" are inert once they cannot be part of a tag.
	for _, token := range []string{"</script", "<!--", "-->"} {
		if strings.Contains(segment, token) {
			t.Fatalf("payload contains %q", token)
		}
	}
	// Exactly two script elements, both opened and closed by the template.
	if n := strings.Count(html, "<script"); n != 2 {
		t.Fatalf("script elements = %d, want 2", n)
	}
	if n := strings.Count(html, "</script>"); n != 2 {
		t.Fatalf("script closings = %d, want 2", n)
	}
	// The payload must still be a faithful round-trip of what went in.
	decoded, err := decodePayload(segment)
	if err != nil {
		t.Fatalf("payload is not valid JSON after escaping: %v", err)
	}
	if decoded.Run.Name != hostile {
		t.Fatalf("payload was corrupted: %q", decoded.Run.Name)
	}
	if decoded.Tasks[0].Answer != hostile {
		t.Fatalf("payload was corrupted: %q", decoded.Tasks[0].Answer)
	}
	if decoded.Run.Notes != long {
		t.Fatalf("long payload was corrupted (len %d, want %d)", len(decoded.Run.Notes), len(long))
	}
}

func TestNeutralizeEscapesExactlyTheSignificantBytes(t *testing.T) {
	got := neutralize([]byte(`{"a":"<&>"}`))
	want := `{"a":"\u003c\u0026\u003e"}`
	if got != want {
		t.Fatalf("neutralize = %q, want %q", got, want)
	}
	// Fast path: nothing to do.
	if got := neutralize([]byte(`{"a":"plain"}`)); got != `{"a":"plain"}` {
		t.Fatalf("neutralize changed an already-safe payload: %q", got)
	}
}

func TestDataJSONIsTheSameDocumentAsTheEmbeddedOne(t *testing.T) {
	fx := seedStore(t, 2)
	data, _ := Load(context.Background(), fx.store, fx.runID, Options{})

	api, err := DataJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	html := renderToString(t, func(w io.Writer) error {
		return RenderRun(w, data, RenderOptions{Standalone: true})
	})
	segment, err := payloadSegment(html)
	if err != nil {
		t.Fatal(err)
	}
	// The web API and the file report must agree byte for byte, otherwise the
	// two front ends can disagree about the same run.
	if segment != string(api) {
		t.Fatalf("embedded payload differs from the API payload:\n embedded: %s\n api:      %s",
			firstN(segment, 120), firstN(string(api), 120))
	}
}

func TestRenderRejectsMissingInput(t *testing.T) {
	var b strings.Builder
	if err := RenderRun(&b, nil, RenderOptions{}); err == nil {
		t.Error("expected an error for nil data")
	}
	if err := RenderIndex(&b, nil, RenderOptions{}); err == nil {
		t.Error("expected an error for a nil index")
	}
	if err := RenderComparison(&b, &Data{}, RenderOptions{}); err == nil {
		t.Error("expected an error when the comparison payload is missing")
	}
}

func TestRenderRunFileCreatesDirectories(t *testing.T) {
	fx := seedStore(t, 2)
	data, _ := Load(context.Background(), fx.store, fx.runID, Options{})
	path := filepath.Join(t.TempDir(), "nested", "deep", "report.html")
	if err := RenderRunFile(path, data, RenderOptions{}); err != nil {
		t.Fatalf("render to file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	if info.Size() < 4096 {
		t.Fatalf("report is suspiciously small: %d bytes", info.Size())
	}
}

func TestIndexAndComparisonRenderInTheirOwnModes(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	for _, id := range []string{"a", "b"} {
		_ = store.CreateRun(ctx, &storage.RunRecord{ID: id, Status: storage.StatusCompleted, DatasetName: "d", AgentKind: "mock"})
	}

	idx, err := LoadIndex(ctx, store, 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	html := renderToString(t, func(w io.Writer) error { return RenderIndex(w, idx, RenderOptions{Standalone: true}) })
	if !strings.Contains(html, `data-mode="index"`) {
		t.Fatal("index mode missing")
	}

	data, err := LoadComparison(ctx, store, "a", "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	html = renderToString(t, func(w io.Writer) error { return RenderComparison(w, data, RenderOptions{Standalone: true}) })
	if !strings.Contains(html, `data-mode="compare"`) {
		t.Fatal("compare mode missing")
	}
	if !strings.Contains(html, `"compare"`) {
		t.Fatal("comparison payload missing")
	}
}

func TestDefaultsBoundEveryUnsetOption(t *testing.T) {
	o := Options{}.withDefaults()
	if o.MaxEventsPerTask <= 0 || o.MaxToolCallsPerTask <= 0 || o.MaxFieldBytes <= 0 || o.MaxInlineBytes <= 0 {
		t.Fatalf("unbounded defaults: %+v", o)
	}
	if o.GeneratedAt.IsZero() {
		t.Fatal("GeneratedAt should default to now")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// payloadSegment extracts the JSON assigned to window.__AEVAL_PAYLOAD__.
func payloadSegment(html string) (string, error) {
	const marker = "window.__AEVAL_PAYLOAD__ = "
	i := strings.Index(html, marker)
	if i < 0 {
		return "", errNoMarker
	}
	rest := html[i+len(marker):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		return "", errNoMarker
	}
	seg := strings.TrimRight(rest[:end], " \t\r\n")
	seg = strings.TrimSuffix(seg, ";")
	return seg, nil
}

var errNoMarker = errors.New("payload marker not found")

func decodePayload(segment string) (*Data, error) {
	// The escapes are JSON escapes, so a plain decoder recovers the original.
	var d Data
	if err := json.Unmarshal([]byte(segment), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

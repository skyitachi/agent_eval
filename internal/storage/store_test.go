package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testRun builds a fully populated run row so that round-trip assertions can
// compare every field rather than a convenient subset.
func testRun(id string) *RunRecord {
	created := time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC)
	return &RunRecord{
		ID:             id,
		Name:           "swe-lite baseline",
		Status:         StatusRunning,
		CreatedAt:      created,
		DatasetName:    "demo",
		DatasetVersion: "1.0.0",
		DatasetHash:    "abc123def456",
		DatasetPath:    "/datasets/demo",
		AgentKind:      "claude_code",
		AgentModel:     "deepseek-v4-pro",
		SandboxBackend: "local",
		SandboxImage:   "",
		SamplesPerTask: 3,
		Concurrency:    4,
		Seed:           42,
		ConfigJSON:     `{"max_turns":30}`,
		ConfigHash:     "cfg0001",
		Notes:          "first run",
		ToolVersion:    "0.1.0-dev",
		HostArch:       "darwin/arm64",
	}
}

// testGrades mirrors what grader.RunTask marshals: a duration in nanoseconds
// and an arbitrary details object.
const testGrades = `[
  {"grader":"patch","type":"patch","passed":true,"score":1,"weight":1,
   "reason":"changed calc.py","duration":2500000,
   "details":{"changed_files":["calc.py"]}},
  {"grader":"hidden-tests","type":"pytest","passed":false,"score":0,"weight":2,
   "required":true,"reason":"1 failed, 2 passed","error":"assert 6 == 7",
   "duration":90000000,"details":{"failed":1,"passed":2}}
]`

func testTrace(toolCalls int) string {
	var b strings.Builder
	b.WriteString(`{"session_id":"sess-01","model":"deepseek-v4-pro","turn_count":`)
	fmt.Fprintf(&b, "%d", toolCalls)
	b.WriteString(`,"tool_calls":[`)
	for i := 0; i < toolCalls; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"seq":%d,"id":"tu_%d","name":"Bash","input":{"command":"ls -la"},`+
			`"output":"ok","is_error":%t,"duration_ms":%d}`,
			i, i, i == 1, 10*(i+1))
	}
	b.WriteString(`]}`)
	return b.String()
}

func testTaskRun(id, runID, taskID string) *TaskRunRecord {
	started := time.Date(2026, 3, 14, 9, 31, 0, 0, time.UTC)
	ended := started.Add(12 * time.Second)
	return &TaskRunRecord{
		ID:          id,
		RunID:       runID,
		TaskID:      taskID,
		SampleIndex: 0,
		Category:    "bugfix",
		Difficulty:  "easy",
		Tags:        []string{"python", "off-by-one"},
		Status:      TaskStatusCompleted,
		StopReason:  "success",
		Passed:      true,
		Score:       0.6,
		Threshold:   1,
		SessionID:   "sess-01",
		Model:       "deepseek-v4-pro",
		StartedAt:   &started,
		EndedAt:     &ended,
		DurationMs:  12000,

		CostUSD:           0.0421,
		CostReliable:      true,
		InputTokens:       1500,
		OutputTokens:      320,
		CacheReadTokens:   900,
		CacheCreateTokens: 100,
		Turns:             4,
		ToolCalls:         3,
		ToolErrors:        1,
		TTFTMs:            820,
		APIDurationMs:     6100,

		Diff:          "--- a/calc.py\n+++ b/calc.py\n",
		Answer:        "Fixed the off-by-one in the mean helper.",
		AttemptJSON:   `{"index":0,"ok":true}`,
		TraceJSON:     testTrace(3),
		GradesJSON:    testGrades,
		WorkspaceDir:  "/tmp/agenteval/run-1/fix-average",
		RawStreamPath: "/tmp/agenteval/run-1/fix-average/stream.jsonl",
	}
}

func mustCreateRun(t *testing.T, s *Store, r *RunRecord) {
	t.Helper()
	if err := s.CreateRun(context.Background(), r); err != nil {
		t.Fatalf("CreateRun(%s): %v", r.ID, err)
	}
}

func mustSaveTaskRun(t *testing.T, s *Store, tr *TaskRunRecord) {
	t.Helper()
	if err := s.SaveTaskRun(context.Background(), tr); err != nil {
		t.Fatalf("SaveTaskRun(%s): %v", tr.ID, err)
	}
}

func countRows(t *testing.T, s *Store, table, where string, args ...any) int {
	t.Helper()
	q := "SELECT COUNT(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := s.DB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

func TestMigrateIsIdempotentAndSetsVersion(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var v int
	if err := s.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	// Re-applying must be a no-op, not an error: every statement uses
	// IF NOT EXISTS precisely so that Open() can always migrate.
	for i := 0; i < 3; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate #%d: %v", i+2, err)
		}
	}

	// Every table in the schema must exist after migration.
	for _, table := range []string{"runs", "task_runs", "grader_results", "tool_calls", "run_metrics", "run_tags"} {
		if n := countRows(t, s, table, ""); n != 0 {
			t.Fatalf("table %s should start empty, got %d rows", table, n)
		}
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eval.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.DB().ExecContext(context.Background(), "PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A database written by a newer build must be refused with an actionable
	// message rather than failing later on a missing column.
	_, err = Open(path)
	if err == nil {
		t.Fatal("expected Open to reject schema version 99")
	}
	if !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("error should mention a newer version, got: %v", err)
	}
}

// A database written before the reward columns existed must be upgraded in
// place, and the upgrade must not invent values it never recorded.
//
// CREATE TABLE IF NOT EXISTS does not add columns to a table that already
// exists, so the ALTER TABLE list is the only thing standing between an
// existing user and a "no such column: reward" failure on their next report.
// The older shape is produced by dropping the columns from a current database
// rather than by hand-writing DDL, so the test cannot drift out of sync with
// the schema it is meant to reproduce.
func TestMigrateAddsTheRewardColumnsToAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreateRun(t, s, testRun("run-old"))
	tr := testTaskRun("tr-old", "run-old", "fix-average")
	tr.Reward, tr.RewardKind = 0.75, "pass"
	tr.Violations, tr.Restored = []string{"forbidden_path: tests/helper.py"}, []string{"tests/helper.py"}
	mustSaveTaskRun(t, s, tr)

	for _, stmt := range []string{
		"ALTER TABLE task_runs DROP COLUMN reward_kind",
		"ALTER TABLE task_runs DROP COLUMN reward",
		"ALTER TABLE task_runs DROP COLUMN guard_json",
		"ALTER TABLE runs DROP COLUMN mode",
		"PRAGMA user_version = 1",
	} {
		if _, err := s.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open an older database: %v", err)
	}
	defer upgraded.Close()

	run, err := upgraded.GetRun(ctx, "run-old")
	if err != nil {
		t.Fatalf("GetRun after the upgrade: %v", err)
	}
	if run.Name != "swe-lite baseline" {
		t.Errorf("the upgrade lost run data: %+v", run)
	}
	if run.Mode != "agent" {
		t.Errorf("mode = %q, want the backfilled default", run.Mode)
	}

	got, err := upgraded.TaskRun(ctx, "run-old", "fix-average", 0)
	if err != nil {
		t.Fatalf("TaskRun after the upgrade: %v", err)
	}
	if got.Score != 0.6 || !got.Passed {
		t.Errorf("the upgrade lost the score: %+v", got)
	}
	// The whole point of reward_kind: a row that predates the column must read
	// as "not recorded", not as a dense reward of zero.
	if got.Reward != 0 || got.RewardKind != "" {
		t.Errorf("an upgraded row must not claim a reward it never recorded: reward=%v kind=%q", got.Reward, got.RewardKind)
	}
	if len(got.Violations) != 0 || len(got.Restored) != 0 {
		t.Errorf("an upgraded row must not claim guards it never recorded: %+v", got)
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "eval.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with missing parent dir: %v", err)
	}
	defer s.Close()
	if s.Path() != path {
		t.Fatalf("Path() = %q, want %q", s.Path(), path)
	}
}

func TestInMemoryStoresAreIsolated(t *testing.T) {
	// Two ":memory:" stores must not share rows; the DSN used to name them all
	// identically, which made fixtures leak into each other.
	a := newStore(t)
	b := newStore(t)
	mustCreateRun(t, a, testRun("run-a"))

	if n := countRows(t, b, "runs", ""); n != 0 {
		t.Fatalf("second in-memory store sees %d runs from the first", n)
	}
}

// ---------------------------------------------------------------------------
// run lifecycle
// ---------------------------------------------------------------------------

func TestRunLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := testRun("run-001")
	mustCreateRun(t, s, want)

	got, err := s.GetRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.FinishedAt != nil {
		t.Fatalf("finished_at should be nil while running, got %v", got.FinishedAt)
	}
	// Compare on a copy with the unset fields blanked so the diff is readable.
	if got.ID != want.ID || got.Name != want.Name || got.Status != StatusRunning {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.DatasetName != "demo" || got.DatasetVersion != "1.0.0" || got.DatasetHash != "abc123def456" {
		t.Fatalf("dataset fields not round-tripped: %+v", got)
	}
	if got.AgentKind != "claude_code" || got.AgentModel != "deepseek-v4-pro" || got.SandboxBackend != "local" {
		t.Fatalf("agent fields not round-tripped: %+v", got)
	}
	if got.SamplesPerTask != 3 || got.Concurrency != 4 || got.Seed != 42 {
		t.Fatalf("run shape not round-tripped: %+v", got)
	}
	if got.ConfigHash != "cfg0001" || got.ToolVersion != "0.1.0-dev" || got.HostArch != "darwin/arm64" {
		t.Fatalf("provenance not round-tripped: %+v", got)
	}

	if err := s.FinishRun(ctx, "run-001", StatusCompleted, "", 95*time.Second); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, err = s.GetRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("GetRun after finish: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", got.Status, StatusCompleted)
	}
	if got.DurationMs != 95000 {
		t.Fatalf("duration_ms = %d, want 95000", got.DurationMs)
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at should be set after FinishRun")
	}
	if !got.CreatedAt.Before(*got.FinishedAt) && !got.CreatedAt.Equal(*got.FinishedAt) {
		t.Fatalf("finished_at %v precedes created_at %v", got.FinishedAt, got.CreatedAt)
	}
}

func TestFinishRunRecordsError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-err"))

	if err := s.FinishRun(ctx, "run-err", StatusFailed, "sandbox backend unavailable", time.Second); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, err := s.GetRun(ctx, "run-err")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != StatusFailed || got.Error != "sandbox backend unavailable" {
		t.Fatalf("error not recorded: %+v", got)
	}
}

func TestCreateRunDefaultsAndValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.CreateRun(ctx, &RunRecord{Name: "no id"}); err == nil {
		t.Fatal("expected an error when the run id is missing")
	}
	if err := s.CreateRun(ctx, &RunRecord{ID: "   "}); err == nil {
		t.Fatal("expected an error for a whitespace-only run id")
	}

	// Status and CreatedAt are filled in so callers cannot persist a row that
	// lists as "unknown status / zero time".
	if err := s.CreateRun(ctx, &RunRecord{ID: "auto"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := s.GetRun(ctx, "auto")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want %q", got.Status, StatusRunning)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at should default to now")
	}

	if err := s.CreateRun(ctx, testRun("auto")); err == nil {
		t.Fatal("expected a primary-key conflict on duplicate run id")
	}
}

func TestGetRunNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetRun(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFinishRunNotFound(t *testing.T) {
	s := newStore(t)
	err := s.FinishRun(context.Background(), "missing", StatusCompleted, "", time.Second)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// task runs
// ---------------------------------------------------------------------------

func TestSaveTaskRunRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))

	got, err := s.TaskRun(ctx, "run-1", "fix-average", 0)
	if err != nil {
		t.Fatalf("TaskRun: %v", err)
	}
	if got.ID != "tr-1" || got.RunID != "run-1" || got.SampleIndex != 0 {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.Category != "bugfix" || got.Difficulty != "easy" {
		t.Fatalf("classification lost: %+v", got)
	}
	if !got.Passed || got.Score != 0.6 || got.Threshold != 1 {
		t.Fatalf("verdict lost: %+v", got)
	}
	if got.Status != TaskStatusCompleted || got.StopReason != "success" {
		t.Fatalf("status lost: %+v", got)
	}
	// The blob columns are what the report layer replays, so they must come back
	// byte-identical, not re-encoded.
	if got.AttemptJSON != `{"index":0,"ok":true}` {
		t.Fatalf("attempt_json = %q", got.AttemptJSON)
	}
	if got.GradesJSON != testGrades {
		t.Fatalf("grades_json changed:\n got %q\nwant %q", got.GradesJSON, testGrades)
	}
	if got.TraceJSON != testTrace(3) {
		t.Fatal("trace_json changed")
	}
	if !got.CostReliable || got.CostUSD != 0.0421 {
		t.Fatalf("cost lost: %+v", got)
	}
	if got.Turns != 4 || got.ToolCalls != 3 || got.ToolErrors != 1 {
		t.Fatalf("turn accounting lost: %+v", got)
	}
	if got.TTFTMs != 820 || got.APIDurationMs != 6100 {
		t.Fatalf("latency lost: %+v", got)
	}
	if got.StartedAt == nil || got.EndedAt == nil || got.EndedAt.Sub(*got.StartedAt) != 12*time.Second {
		t.Fatalf("timestamps lost: start=%v end=%v", got.StartedAt, got.EndedAt)
	}
	if got.RawStreamPath == "" || got.WorkspaceDir == "" {
		t.Fatalf("artifact pointers lost: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "python" || got.Tags[1] != "off-by-one" {
		t.Fatalf("tags lost: %v", got.Tags)
	}
}

// The reward columns are the only part of a task run that a training loop
// reads, so they get their own round trip: a reward that survives the write but
// loses its density would let a sparse contract be read as a dense one.
func TestSaveTaskRunKeepsTheRewardAndItsDensity(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	tr := testTaskRun("tr-1", "run-1", "implement-slugify")
	tr.Passed = false
	tr.Score = 0.4
	tr.Reward = 0
	tr.RewardKind = "pass"
	tr.Violations = []string{"forbidden_path: tests/test_slugify.py"}
	tr.Restored = []string{"tests/test_slugify.py"}
	mustSaveTaskRun(t, s, tr)

	got, err := s.TaskRun(ctx, "run-1", "implement-slugify", 0)
	if err != nil {
		t.Fatalf("TaskRun: %v", err)
	}
	if got.Reward != 0 || got.RewardKind != "pass" {
		t.Fatalf("reward = %v / %q, want 0 / pass; a partial fix under a sparse contract must not read as a dense 0.4", got.Reward, got.RewardKind)
	}
	if len(got.Violations) != 1 || got.Violations[0] != "forbidden_path: tests/test_slugify.py" {
		t.Fatalf("violations lost: %v", got.Violations)
	}
	if len(got.Restored) != 1 || got.Restored[0] != "tests/test_slugify.py" {
		t.Fatalf("restored paths lost: %v", got.Restored)
	}

	// An upsert must overwrite both halves together. Updating the value while
	// leaving the old density behind is the same misreading from the other
	// direction.
	tr.Reward, tr.RewardKind = 1, "score"
	mustSaveTaskRun(t, s, tr)
	got, err = s.TaskRun(ctx, "run-1", "implement-slugify", 0)
	if err != nil {
		t.Fatalf("TaskRun after upsert: %v", err)
	}
	if got.Reward != 1 || got.RewardKind != "score" {
		t.Fatalf("after upsert reward = %v / %q, want 1 / score", got.Reward, got.RewardKind)
	}
}

func TestSaveTaskRunFlattensGraders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))

	grades, err := s.GraderResults(ctx, "run-1")
	if err != nil {
		t.Fatalf("GraderResults: %v", err)
	}
	if len(grades) != 2 {
		t.Fatalf("got %d grader rows, want 2", len(grades))
	}
	first, second := grades[0], grades[1]

	// seq records the order the graders ran in, so reports list them the same
	// way the attempt did.
	if first.Seq != 0 || second.Seq != 1 {
		t.Fatalf("grader seq = %d,%d, want 0,1", first.Seq, second.Seq)
	}

	if first.Grader != "patch" || first.Type != "patch" || !first.Passed || first.Score != 1 {
		t.Fatalf("first grader wrong: %+v", first)
	}
	if first.Weight != 1 || first.Required {
		t.Fatalf("first grader flags wrong: %+v", first)
	}
	// 2_500_000 ns must be stored as 2 ms, not 2500000.
	if first.DurationMs != 2 {
		t.Fatalf("patch duration_ms = %d, want 2", first.DurationMs)
	}
	var details struct {
		ChangedFiles []string `json:"changed_files"`
	}
	if err := json.Unmarshal([]byte(first.DetailsJSON), &details); err != nil {
		t.Fatalf("details_json is not valid JSON (%q): %v", first.DetailsJSON, err)
	}
	if len(details.ChangedFiles) != 1 || details.ChangedFiles[0] != "calc.py" {
		t.Fatalf("details lost: %+v", details)
	}

	if second.Grader != "hidden-tests" || second.Type != "pytest" || second.Passed {
		t.Fatalf("second grader wrong: %+v", second)
	}
	if !second.Required || second.Weight != 2 {
		t.Fatalf("second grader flags wrong: %+v", second)
	}
	if second.DurationMs != 90 {
		t.Fatalf("pytest duration_ms = %d, want 90", second.DurationMs)
	}
	if second.Error != "assert 6 == 7" || second.Reason != "1 failed, 2 passed" {
		t.Fatalf("grader diagnostics lost: %+v", second)
	}
	if second.TaskRunID != "tr-1" || second.TaskID != "fix-average" || second.RunID != "run-1" {
		t.Fatalf("grader row not linked back to the attempt: %+v", second)
	}
}

func TestSaveTaskRunFlattensToolCalls(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))

	calls, err := s.ToolCalls(ctx, "tr-1")
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("got %d tool calls, want 3", len(calls))
	}
	for i, c := range calls {
		if c.Seq != i {
			t.Fatalf("tool call %d has seq %d; ordering is by seq", i, c.Seq)
		}
		if c.Name != "Bash" || c.ToolUseID != fmt.Sprintf("tu_%d", i) {
			t.Fatalf("tool call %d wrong: %+v", i, c)
		}
		if c.RunID != "run-1" || c.TaskID != "fix-average" || c.TaskRunID != "tr-1" {
			t.Fatalf("tool call %d not linked: %+v", i, c)
		}
	}
	if calls[1].IsError != true || calls[0].IsError != false {
		t.Fatalf("is_error not round-tripped: %+v", calls)
	}
	if calls[2].DurationMs != 30 {
		t.Fatalf("duration_ms = %d, want 30", calls[2].DurationMs)
	}
	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(calls[0].InputJSON), &input); err != nil {
		t.Fatalf("input_json is not valid JSON (%q): %v", calls[0].InputJSON, err)
	}
	if input.Command != "ls -la" {
		t.Fatalf("tool input lost: %+v", input)
	}
}

func TestSaveTaskRunTruncatesHugeToolOutput(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	// A `cat` of a large file must not bloat the database row.
	huge := strings.Repeat("x", 25000)
	tr := testTaskRun("tr-1", "run-1", "fix-average")
	tr.TraceJSON = fmt.Sprintf(`{"tool_calls":[{"seq":0,"id":"tu_0","name":"Read","output":%q}]}`, huge)
	mustSaveTaskRun(t, s, tr)

	calls, err := s.ToolCalls(ctx, "tr-1")
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if len(calls[0].Output) != 20000+len("...(truncated)") {
		t.Fatalf("output length = %d, want 20014", len(calls[0].Output))
	}
	if !strings.HasSuffix(calls[0].Output, "...(truncated)") {
		t.Fatalf("truncated output should be marked: %q", calls[0].Output[len(calls[0].Output)-20:])
	}
}

func TestSaveTaskRunReplacesStaleChildRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))

	if n := countRows(t, s, "grader_results", "task_run_id = ?", "tr-1"); n != 2 {
		t.Fatalf("setup: %d grader rows, want 2", n)
	}
	if n := countRows(t, s, "tool_calls", "task_run_id = ?", "tr-1"); n != 3 {
		t.Fatalf("setup: %d tool call rows, want 3", n)
	}

	// Re-grade with a different, smaller grader set and a shorter trace. The
	// old rows must disappear: a re-run that leaves stale verdicts behind makes
	// "which grader fails most often" silently wrong.
	tr := testTaskRun("tr-1", "run-1", "fix-average")
	tr.Passed = false
	tr.Score = 0
	tr.GradesJSON = `[{"grader":"hidden-tests","type":"pytest","passed":false,"score":0,"weight":1,"required":true,"reason":"still failing"}]`
	tr.TraceJSON = testTrace(1)
	mustSaveTaskRun(t, s, tr)

	if n := countRows(t, s, "task_runs", "id = ?", "tr-1"); n != 1 {
		t.Fatalf("upsert created a duplicate row: %d", n)
	}
	grades, err := s.GraderResults(ctx, "run-1")
	if err != nil {
		t.Fatalf("GraderResults: %v", err)
	}
	if len(grades) != 1 || grades[0].Grader != "hidden-tests" {
		t.Fatalf("stale grader rows survived: %+v", grades)
	}
	calls, err := s.ToolCalls(ctx, "tr-1")
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("stale tool calls survived: %d rows", len(calls))
	}

	got, err := s.TaskRun(ctx, "run-1", "fix-average", 0)
	if err != nil {
		t.Fatalf("TaskRun: %v", err)
	}
	if got.Passed || got.Score != 0 {
		t.Fatalf("verdict not updated by the second save: %+v", got)
	}
}

func TestSaveTaskRunKeepsSamplesDistinct(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	// pass@k needs several attempts of the same task to coexist.
	for i := 0; i < 3; i++ {
		tr := testTaskRun(fmt.Sprintf("tr-%d", i), "run-1", "fix-average")
		tr.SampleIndex = i
		tr.Score = float64(i) / 2
		tr.Passed = i == 2
		mustSaveTaskRun(t, s, tr)
	}

	got, err := s.TaskRuns(ctx, TaskRunsQuery{RunID: "run-1"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d attempts, want 3", len(got))
	}
	for i, tr := range got {
		if tr.SampleIndex != i {
			t.Fatalf("attempt %d has sample_index %d; default order is by sample", i, tr.SampleIndex)
		}
	}
	if n := countRows(t, s, "grader_results", "run_id = ?", "run-1"); n != 6 {
		t.Fatalf("grader rows = %d, want 6 (2 per sample)", n)
	}
}

func TestSaveTaskRunValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	if err := s.SaveTaskRun(ctx, &TaskRunRecord{RunID: "run-1", TaskID: "t"}); err == nil {
		t.Fatal("expected an error when the task run id is missing")
	}

	// A foreign key violation must surface rather than creating an orphan row.
	orphan := testTaskRun("tr-x", "no-such-run", "t")
	if err := s.SaveTaskRun(ctx, orphan); err == nil {
		t.Fatal("expected a foreign key error for an unknown run")
	}
	if n := countRows(t, s, "task_runs", "id = ?", "tr-x"); n != 0 {
		t.Fatal("the failed save should have rolled back entirely")
	}
}

func TestSaveTaskRunDefaults(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	// No status, no tags, no blobs: still a valid row.
	bare := &TaskRunRecord{ID: "tr-bare", RunID: "run-1", TaskID: "t"}
	mustSaveTaskRun(t, s, bare)

	got, err := s.TaskRun(ctx, "run-1", "t", 0)
	if err != nil {
		t.Fatalf("TaskRun: %v", err)
	}
	if got.Status != TaskStatusCompleted {
		t.Fatalf("status = %q, want %q", got.Status, TaskStatusCompleted)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("tags = %v, want empty", got.Tags)
	}
	if n := countRows(t, s, "grader_results", "task_run_id = ?", "tr-bare"); n != 0 {
		t.Fatalf("empty grades must produce no grader rows, got %d", n)
	}
	if n := countRows(t, s, "tool_calls", "task_run_id = ?", "tr-bare"); n != 0 {
		t.Fatalf("empty trace must produce no tool call rows, got %d", n)
	}
}

func TestSaveTaskRunToleratesMalformedTrace(t *testing.T) {
	s := newStore(t)
	mustCreateRun(t, s, testRun("run-1"))

	// A truncated capture must not lose the verdict; the task row and its
	// graders are the important part.
	tr := testTaskRun("tr-1", "run-1", "fix-average")
	tr.TraceJSON = `{"tool_calls":[{"seq":0,`
	mustSaveTaskRun(t, s, tr)

	if n := countRows(t, s, "tool_calls", "task_run_id = ?", "tr-1"); n != 0 {
		t.Fatalf("malformed trace produced %d tool call rows, want 0", n)
	}
	if n := countRows(t, s, "grader_results", "task_run_id = ?", "tr-1"); n != 2 {
		t.Fatal("graders should still be saved when the trace is malformed")
	}
}

func TestSaveTaskRunRejectsMalformedGrades(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	tr := testTaskRun("tr-1", "run-1", "fix-average")
	tr.GradesJSON = `[{"grader":`
	if err := s.SaveTaskRun(ctx, tr); err == nil {
		t.Fatal("expected an error for undecodable grades JSON")
	}
	// Grader rows are the grading record; silently dropping them would make the
	// run look like it had no graders at all.
	if n := countRows(t, s, "task_runs", "id = ?", "tr-1"); n != 0 {
		t.Fatal("the whole save should have rolled back")
	}
}

// ---------------------------------------------------------------------------
// task run queries
// ---------------------------------------------------------------------------

func seedQueryRun(t *testing.T, s *Store) {
	t.Helper()
	mustCreateRun(t, s, testRun("run-q"))
	specs := []struct {
		id       string
		task     string
		category string
		passed   bool
		score    float64
		duration int64
		cost     float64
		stop     string
	}{
		{"tr-a", "task-a", "bugfix", true, 1.0, 1000, 0.01, "success"},
		{"tr-b", "task-b", "bugfix", false, 0.25, 5000, 0.05, "success"},
		{"tr-c", "task-c", "feature", false, 0.5, 3000, 0.03, "max_turns"},
		{"tr-d", "task-d", "feature", true, 0.9, 2000, 0.02, "success"},
	}
	for _, sp := range specs {
		tr := testTaskRun(sp.id, "run-q", sp.task)
		tr.Category = sp.category
		tr.Passed = sp.passed
		tr.Score = sp.score
		tr.DurationMs = sp.duration
		tr.CostUSD = sp.cost
		tr.StopReason = sp.stop
		mustSaveTaskRun(t, s, tr)
	}
}

func taskIDs(trs []TaskRunRecord) []string {
	out := make([]string, len(trs))
	for i, tr := range trs {
		out[i] = tr.TaskID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTaskRunsQueryFilters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedQueryRun(t, s)

	tests := []struct {
		name string
		q    TaskRunsQuery
		want []string
	}{
		{"all", TaskRunsQuery{RunID: "run-q"}, []string{"task-a", "task-b", "task-c", "task-d"}},
		{"by task", TaskRunsQuery{RunID: "run-q", TaskID: "task-c"}, []string{"task-c"}},
		{"by category", TaskRunsQuery{RunID: "run-q", Category: "feature"}, []string{"task-c", "task-d"}},
		{"by stop reason", TaskRunsQuery{RunID: "run-q", StopReason: "max_turns"}, []string{"task-c"}},
		{"failures only", TaskRunsQuery{RunID: "run-q", OnlyFailed: true}, []string{"task-b", "task-c"}},
		{"worst score first", TaskRunsQuery{RunID: "run-q", OrderBy: "score"}, []string{"task-b", "task-c", "task-d", "task-a"}},
		{"slowest first", TaskRunsQuery{RunID: "run-q", OrderBy: "duration_ms"}, []string{"task-b", "task-c", "task-d", "task-a"}},
		{"costliest first", TaskRunsQuery{RunID: "run-q", OrderBy: "cost_usd"}, []string{"task-b", "task-c", "task-d", "task-a"}},
		{"limited", TaskRunsQuery{RunID: "run-q", Limit: 2}, []string{"task-a", "task-b"}},
		{"combined", TaskRunsQuery{RunID: "run-q", Category: "feature", OnlyFailed: true}, []string{"task-c"}},
		{"no match", TaskRunsQuery{RunID: "run-q", TaskID: "nope"}, nil},
		{"other run", TaskRunsQuery{RunID: "other"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.TaskRuns(ctx, tc.q)
			if err != nil {
				t.Fatalf("TaskRuns: %v", err)
			}
			if !equalStrings(taskIDs(got), tc.want) {
				t.Fatalf("task ids = %v, want %v", taskIDs(got), tc.want)
			}
		})
	}
}

func TestTaskRunsQueryDoesNotLeakAcrossRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedQueryRun(t, s)

	other := testTaskRun("tr-other", "run-other", "task-a")
	mustCreateRun(t, s, testRun("run-other"))
	mustSaveTaskRun(t, s, other)

	got, err := s.TaskRuns(ctx, TaskRunsQuery{RunID: "run-q"})
	if err != nil {
		t.Fatalf("TaskRuns: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d attempts, want 4", len(got))
	}
	for _, tr := range got {
		if tr.RunID != "run-q" {
			t.Fatalf("row from another run leaked in: %+v", tr)
		}
	}
}

func TestTaskRunNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.TaskRun(context.Background(), "run-q", "task-a", 7)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// metrics
// ---------------------------------------------------------------------------

func TestSaveMetricsReplacesAndSnapshots(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	first := []MetricRecord{
		{Name: "resolve_rate", Value: 0.5, Unit: "ratio", Kind: "pass_rate"},
		{Name: "total_cost_usd", Value: 0.1, Unit: "usd", Kind: "cost", Label: "Total cost"},
	}
	if err := s.SaveMetrics(ctx, "run-1", first); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}
	got, err := s.Metrics(ctx, "run-1")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d metrics, want 2", len(got))
	}
	// Results come back name-sorted so reports are reproducible.
	if got[0].Name != "resolve_rate" || got[1].Name != "total_cost_usd" {
		t.Fatalf("metrics not name-ordered: %v", got)
	}
	if got[1].Label != "Total cost" || got[1].Unit != "usd" {
		t.Fatalf("metric metadata lost: %+v", got[1])
	}

	// A provider fallback that changes the numbers must overwrite, not append.
	second := []MetricRecord{
		{Name: "resolve_rate", Value: 0.75, Unit: "ratio", Kind: "pass_rate"},
	}
	if err := s.SaveMetrics(ctx, "run-1", second); err != nil {
		t.Fatalf("SaveMetrics #2: %v", err)
	}
	got, err = s.Metrics(ctx, "run-1")
	if err != nil {
		t.Fatalf("Metrics #2: %v", err)
	}
	if len(got) != 1 || got[0].Value != 0.75 {
		t.Fatalf("metrics were appended instead of replaced: %+v", got)
	}

	// The headline numbers are denormalised onto the run row so listings need
	// no join; verify the snapshot is refreshed on every save.
	var raw string
	if err := s.DB().QueryRowContext(ctx, `SELECT metrics_json FROM runs WHERE id=?`, "run-1").Scan(&raw); err != nil {
		t.Fatalf("read metrics_json: %v", err)
	}
	var snap map[string]float64
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("metrics_json is not valid JSON (%q): %v", raw, err)
	}
	if len(snap) != 1 || snap["resolve_rate"] != 0.75 {
		t.Fatalf("snapshot = %v, want only resolve_rate=0.75", snap)
	}

	// Clearing all metrics must also clear the snapshot.
	if err := s.SaveMetrics(ctx, "run-1", nil); err != nil {
		t.Fatalf("SaveMetrics #3: %v", err)
	}
	if got, _ := s.Metrics(ctx, "run-1"); len(got) != 0 {
		t.Fatalf("metrics not cleared: %+v", got)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT metrics_json FROM runs WHERE id=?`, "run-1").Scan(&raw); err != nil {
		t.Fatalf("read metrics_json: %v", err)
	}
	if raw != "{}" {
		t.Fatalf("snapshot after clearing = %q, want {}", raw)
	}
}

// ---------------------------------------------------------------------------
// listings and tags
// ---------------------------------------------------------------------------

func TestListRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for i, status := range []string{StatusCompleted, StatusFailed, StatusCompleted} {
		r := testRun(fmt.Sprintf("run-%d", i))
		r.CreatedAt = time.Date(2026, 3, 14, 10+i, 0, 0, 0, time.UTC)
		r.Status = status
		mustCreateRun(t, s, r)
	}

	// run-0 is the oldest and has two attempts, one passing.
	mustSaveTaskRun(t, s, func() *TaskRunRecord {
		tr := testTaskRun("tr-0a", "run-0", "task-a")
		tr.Passed = true
		return tr
	}())
	mustSaveTaskRun(t, s, func() *TaskRunRecord {
		tr := testTaskRun("tr-0b", "run-0", "task-b")
		tr.Passed = false
		return tr
	}())

	if err := s.SaveMetrics(ctx, "run-0", []MetricRecord{
		{Name: "total_cost_usd", Value: 0.42},
		{Name: "mean_latency_ms", Value: 1200},
		{Name: "resolve_rate", Value: 0.5},
	}); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	runs, err := s.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	// Newest first.
	if runs[0].ID != "run-2" || runs[1].ID != "run-1" || runs[2].ID != "run-0" {
		t.Fatalf("wrong order: %s %s %s", runs[0].ID, runs[1].ID, runs[2].ID)
	}

	oldest := runs[2]
	if oldest.TaskCount != 2 || oldest.PassedCount != 1 {
		t.Fatalf("counts = %d/%d, want 1/2 passing", oldest.PassedCount, oldest.TaskCount)
	}
	if oldest.ResolveRate != 0.5 {
		t.Fatalf("resolve_rate = %v, want 0.5", oldest.ResolveRate)
	}
	if oldest.TotalCostUSD != 0.42 || oldest.MeanLatencyMs != 1200 {
		t.Fatalf("snapshot not surfaced in the summary: %+v", oldest)
	}
	if oldest.Metrics["resolve_rate"] != 0.5 {
		t.Fatalf("full metric map not surfaced: %v", oldest.Metrics)
	}

	// A run with no attempts must not divide by zero.
	empty := runs[0]
	if empty.TaskCount != 0 || empty.ResolveRate != 0 {
		t.Fatalf("empty run summary = %+v", empty)
	}
	if empty.Metrics != nil {
		t.Fatalf("empty run should have no metric map, got %v", empty.Metrics)
	}

	if limited, err := s.ListRuns(ctx, 1); err != nil {
		t.Fatalf("ListRuns(1): %v", err)
	} else if len(limited) != 1 || limited[0].ID != "run-2" {
		t.Fatalf("limit ignored: %+v", limited)
	}
}

func TestSetRunTag(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	if err := s.SetRunTag(ctx, "run-1", "harness_rev", "abc123"); err != nil {
		t.Fatalf("SetRunTag: %v", err)
	}
	// Upsert, not insert: the same key must be overwritten.
	if err := s.SetRunTag(ctx, "run-1", "harness_rev", "def456"); err != nil {
		t.Fatalf("SetRunTag #2: %v", err)
	}
	if err := s.SetRunTag(ctx, "run-1", "gpu", "none"); err != nil {
		t.Fatalf("SetRunTag #3: %v", err)
	}

	if n := countRows(t, s, "run_tags", "run_id = ?", "run-1"); n != 2 {
		t.Fatalf("tag rows = %d, want 2", n)
	}
	var value string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT value FROM run_tags WHERE run_id=? AND key=?`, "run-1", "harness_rev").Scan(&value); err != nil {
		t.Fatalf("read tag: %v", err)
	}
	if value != "def456" {
		t.Fatalf("tag value = %q, want def456", value)
	}
}

// ---------------------------------------------------------------------------
// resume support
// ---------------------------------------------------------------------------

func TestCompletedKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	complete := testTaskRun("tr-1", "run-1", "task-done")
	mustSaveTaskRun(t, s, complete)

	failed := testTaskRun("tr-2", "run-1", "task-failed")
	failed.Status = TaskStatusFailed
	mustSaveTaskRun(t, s, failed)

	skipped := testTaskRun("tr-3", "run-1", "task-skipped")
	skipped.Status = TaskStatusSkipped
	mustSaveTaskRun(t, s, skipped)

	// A distinct sample of an already completed task is still outstanding.
	second := testTaskRun("tr-4", "run-1", "task-done")
	second.SampleIndex = 1
	mustSaveTaskRun(t, s, second)

	keys, err := s.CompletedKeys(ctx, "run-1")
	if err != nil {
		t.Fatalf("CompletedKeys: %v", err)
	}
	// Only fully graded attempts count; failures must be retried on resume.
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2 entries", keys)
	}
	if !keys[TaskKey("task-done", 0)] || !keys[TaskKey("task-done", 1)] {
		t.Fatalf("completed samples missing: %v", keys)
	}
	if keys[TaskKey("task-failed", 0)] || keys[TaskKey("task-skipped", 0)] {
		t.Fatalf("failed/skipped attempts must not count as completed: %v", keys)
	}

	if k := TaskKey("task-done", 0); k != "task-done#0" {
		t.Fatalf("TaskKey = %q, want task-done#0", k)
	}
}

// ---------------------------------------------------------------------------
// deletions
// ---------------------------------------------------------------------------

func TestDeleteRunCascades(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))
	if err := s.SaveMetrics(ctx, "run-1", []MetricRecord{{Name: "resolve_rate", Value: 1}}); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}
	if err := s.SetRunTag(ctx, "run-1", "k", "v"); err != nil {
		t.Fatalf("SetRunTag: %v", err)
	}

	if err := s.DeleteRun(ctx, "run-1"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	for _, table := range []string{"runs", "task_runs", "grader_results", "tool_calls", "run_metrics", "run_tags"} {
		if n := countRows(t, s, table, ""); n != 0 {
			t.Fatalf("%s still has %d rows after DeleteRun", table, n)
		}
	}

	if err := s.DeleteRun(ctx, "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteRun err = %v, want ErrNotFound", err)
	}
}

func TestStats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))
	if err := s.SaveMetrics(ctx, "run-1", []MetricRecord{{Name: "resolve_rate", Value: 1}}); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := map[string]int{
		"runs": 1, "task_runs": 1, "grader_results": 2, "tool_calls": 3, "run_metrics": 1,
	}
	for k, v := range want {
		if stats[k] != v {
			t.Fatalf("stats[%s] = %d, want %d (full: %v)", k, stats[k], v, stats)
		}
	}
}

// ---------------------------------------------------------------------------
// durability and concurrency
// ---------------------------------------------------------------------------

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eval.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-1", "run-1", "fix-average"))
	if err := s.SaveMetrics(ctx, "run-1", []MetricRecord{{Name: "resolve_rate", Value: 1}}); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}
	if err := s.FinishRun(ctx, "run-1", StatusCompleted, "", time.Minute); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	got, err := again.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if got.Status != StatusCompleted || got.FinishedAt == nil {
		t.Fatalf("run state lost across reopen: %+v", got)
	}
	tr, err := again.TaskRun(ctx, "run-1", "fix-average", 0)
	if err != nil {
		t.Fatalf("TaskRun after reopen: %v", err)
	}
	if !tr.Passed || tr.GradesJSON != testGrades {
		t.Fatalf("attempt lost across reopen: %+v", tr)
	}
	if calls, err := again.ToolCalls(ctx, "tr-1"); err != nil || len(calls) != 3 {
		t.Fatalf("tool calls lost across reopen: %d rows, err = %v", len(calls), err)
	}
	metrics, err := again.Metrics(ctx, "run-1")
	if err != nil || len(metrics) != 1 {
		t.Fatalf("metrics lost across reopen: %+v, err = %v", metrics, err)
	}
}

func TestConcurrentSaves(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	// The engine grades tasks concurrently and saves from many goroutines; the
	// store serialises writers rather than letting SQLITE_BUSY escape.
	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr := testTaskRun(fmt.Sprintf("tr-%02d", i), "run-1", fmt.Sprintf("task-%02d", i))
			tr.SampleIndex = i % 2
			errs[i] = s.SaveTaskRun(ctx, tr)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := countRows(t, s, "task_runs", "run_id = ?", "run-1"); got != n {
		t.Fatalf("task_runs = %d, want %d", got, n)
	}
	if got := countRows(t, s, "grader_results", "run_id = ?", "run-1"); got != 2*n {
		t.Fatalf("grader_results = %d, want %d", got, 2*n)
	}
	if got := countRows(t, s, "tool_calls", "run_id = ?", "run-1"); got != 3*n {
		t.Fatalf("tool_calls = %d, want %d", got, 3*n)
	}
}

func TestConcurrentMetricsAndTaskSaves(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))

	// Metrics land while attempts are still being saved; the snapshot rewrite
	// and the attempt upsert must not deadlock or corrupt each other.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr := testTaskRun(fmt.Sprintf("tr-%d", i), "run-1", fmt.Sprintf("task-%d", i))
			if err := s.SaveTaskRun(ctx, tr); err != nil {
				t.Errorf("SaveTaskRun %d: %v", i, err)
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := []MetricRecord{{Name: "resolve_rate", Value: float64(i)}}
			if err := s.SaveMetrics(ctx, "run-1", m); err != nil {
				t.Errorf("SaveMetrics %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := countRows(t, s, "task_runs", "run_id = ?", "run-1"); got != 8 {
		t.Fatalf("task_runs = %d, want 8", got)
	}
	if got := countRows(t, s, "run_metrics", "run_id = ?", "run-1"); got != 1 {
		t.Fatalf("run_metrics = %d, want 1 (each save replaces)", got)
	}
	var raw string
	if err := s.DB().QueryRowContext(ctx, `SELECT metrics_json FROM runs WHERE id=?`, "run-1").Scan(&raw); err != nil {
		t.Fatalf("read metrics_json: %v", err)
	}
	var snap map[string]float64
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("metrics_json is not valid JSON after concurrent writes (%q): %v", raw, err)
	}
}

func TestConcurrentReadsDuringWrites(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, testRun("run-1"))
	mustSaveTaskRun(t, s, testTaskRun("tr-seed", "run-1", "task-seed"))

	done := make(chan struct{})
	var readerErr error
	var once sync.Once
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if _, err := s.TaskRuns(ctx, TaskRunsQuery{RunID: "run-1"}); err != nil {
				once.Do(func() { readerErr = err })
				return
			}
			if _, err := s.GetRun(ctx, "run-1"); err != nil {
				once.Do(func() { readerErr = err })
				return
			}
		}
	}()

	for i := 0; i < 10; i++ {
		tr := testTaskRun(fmt.Sprintf("tr-w%d", i), "run-1", fmt.Sprintf("task-w%d", i))
		mustSaveTaskRun(t, s, tr)
	}
	<-done
	if readerErr != nil {
		t.Fatalf("concurrent read failed: %v", readerErr)
	}
}

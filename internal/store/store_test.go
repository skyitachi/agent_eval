package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/skyitachi/agenteval/internal/grader"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleRun(id string) Run {
	return Run{
		ID: id, Name: "sweep-model-opus", Status: RunRunning,
		StartedAt:   time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		DatasetPath: "/ds/demo", DatasetName: "demo", DatasetHash: "sha256:abc",
		AgentKind: "claude_code", AgentModel: "opus", AgentBin: "claude",
		Backend: "local", Samples: 3, Concurrency: 4,
		FactorAxis: "model", FactorLevel: "opus", SweepID: "sw1",
		Notes: "n", ToolVersion: "dev", ConfigJSON: `{"a":1}`,
		AgentSpecJSON: `{"kind":"claude_code","model":"opus","plugin_dirs":["/skills/a"],` +
			`"isolation":{"bare":false,"strict_mcp_config":true,"disable_slash_commands":false}}`,
		// True here so the round trip covers it: a chained run has to stay
		// distinguishable after the fact, because its cells are a sequence and
		// not independent samples and nothing on an attempt row says so.
		ChainSessions: true,
	}
}

func sampleAttempt(runID, taskID string) Attempt {
	return Attempt{
		RunID: runID, TaskID: taskID, Sample: 0,
		Category: "bugfix", Tags: []string{"go", "easy"},
		Status: CellOK, StopReason: "completed",
		Graded: true, Passed: true, Score: 0.75, Threshold: 1,
		DurationMs: 1234, CostUSD: 0.5,
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheCreateTokens: 1,
		Turns: 7, ToolCalls: 9, ToolErrors: 1,
		Diff: "--- a\n+++ b\n", Answer: "done",
		Grades: []grader.Result{{Grader: "tests", Type: "pytest", Passed: true, Score: 1, Weight: 1}},
		Trace:  json.RawMessage(`{"turns":[{"role":"assistant"}]}`),
		Baseline: BaselineComparison{
			Present: true, ID: "sess-1", Ref: "baselines/x.json",
			Tokens: 9000, Ms: 60000, Cost: 0.25, Turns: 12,
			Output: "the recorded answer",
			// The recording's provenance, which is what lets the report say
			// what is being compared with what.
			AgentName: "cursor", AgentModel: "grok-4.5",
			Source: "cursor-composer", Notes: "records no cost",
		},
		Error: "warn", WorkspaceDir: "/tmp/ws",
	}
}

func TestSchemaVersionIsWrittenOnFirstOpen(t *testing.T) {
	s := openTemp(t)
	v, err := s.existingVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("version = %d, want %d", v, schemaVersion)
	}
}

func TestOpenRefusesASchemaFromAnotherBuild(t *testing.T) {
	// Silently reusing a database whose columns mean something else is how a
	// reporting bug becomes a week of confusion.
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT);` +
		`INSERT INTO schema_meta VALUES('version','99')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("opening a v99 database must fail")
	}
	if !strings.Contains(err.Error(), "v99") || !strings.Contains(err.Error(), "fresh file") {
		t.Errorf("the error should name the version and the way out, got %v", err)
	}
}

func TestOpenRefusesAPrototypeDatabase(t *testing.T) {
	// The earlier prototype had a task_runs table and RL reward columns. Its
	// results answer different questions, so it is refused rather than guessed
	// at.
	path := filepath.Join(t.TempDir(), "prototype.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE task_runs(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("opening a prototype database must fail")
	}
}

// TestOpenUpgradesAV1Database covers the other half of the version rule: an
// older database is not one to throw away. The alternative is asking someone to
// discard runs they already paid for because a text column was added.
//
// The v1 shape is built from this build's own schema minus the column v2 adds,
// rather than from a copied-out DDL string, so the fixture cannot quietly drift
// away from what v1 actually was.
func TestOpenUpgradesAV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The database is built by taking today's schema and deleting what v2, v3,
	// v4, v5, v6 and v7 added, rather than by pasting in an old DDL file: a copy
	// of the old schema would drift from the migrations and stop testing them.
	for _, stmt := range []string{
		string(body),
		`ALTER TABLE attempts DROP COLUMN baseline_output`,
		`ALTER TABLE attempts DROP COLUMN baseline_agent_name`,
		`ALTER TABLE attempts DROP COLUMN baseline_agent_model`,
		`ALTER TABLE attempts DROP COLUMN baseline_source`,
		`ALTER TABLE attempts DROP COLUMN baseline_notes`,
		`ALTER TABLE attempts DROP COLUMN restore_notes`,
		`ALTER TABLE runs DROP COLUMN agent_spec_json`,
		`ALTER TABLE runs DROP COLUMN chain_sessions`,
		`ALTER TABLE runs DROP COLUMN in_place`,
		`INSERT INTO schema_meta(key, value) VALUES('version','1')`,
		`INSERT INTO runs(id, status, started_at) VALUES('r1','completed','2026-01-01T00:00:00Z')`,
		`INSERT INTO attempts(run_id, task_id, status, has_baseline, baseline_id, baseline_tokens)
		 VALUES('r1','t1','ok',1,'sess-1',9000)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v1 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v1 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	got, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want the row that was already in the database", len(got))
	}
	// Empty is the honest reading of a v1 row, not a defect: the column did not
	// exist when it was written, so no recording's answer was ever kept for it.
	if got[0].Baseline.Output != "" {
		t.Errorf("baseline output = %q, want empty for a row written before v2", got[0].Baseline.Output)
	}
	if !got[0].Baseline.Present || got[0].Baseline.Tokens != 9000 || got[0].Baseline.ID != "sess-1" {
		t.Errorf("the upgrade lost numbers that were already in the row: %+v", got[0].Baseline)
	}
	// Read at all is half the assertion: scanAttempt names the four provenance
	// columns, so a migration that failed to add them fails here rather than
	// returning zeros.
	if got[0].Baseline.AgentName != "" || got[0].Baseline.AgentModel != "" ||
		got[0].Baseline.Source != "" || got[0].Baseline.Notes != "" {
		t.Errorf("provenance = %+v, want empty for a row written before v3", got[0].Baseline)
	}
	// The point of upgrading rather than refusing is being able to write to the
	// old database at the new revision.
	if err := s.SaveAttempt(ctx, Attempt{
		RunID: "r1", TaskID: "t2", Status: CellOK,
		Baseline: BaselineComparison{Present: true, Output: "an answer kept after the upgrade"},
	}); err != nil {
		t.Fatalf("save into an upgraded database: %v", err)
	}
	again, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range again {
		if a.TaskID == "t2" && a.Baseline.Output != "an answer kept after the upgrade" {
			t.Errorf("baseline output = %q after the upgrade", a.Baseline.Output)
		}
	}
}

// TestOpenUpgradesAV2Database is the v2-to-v3 half of the same story: a
// database written when a baseline carried its numbers but not its provenance.
// It upgrades, and the row reads back with the provenance empty -- which is
// exactly true of it, because those runs were told who made the recording by
// the dataset and the store had nowhere to keep it.
func TestOpenUpgradesAV2Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`ALTER TABLE attempts DROP COLUMN baseline_agent_name`,
		`ALTER TABLE attempts DROP COLUMN baseline_agent_model`,
		`ALTER TABLE attempts DROP COLUMN baseline_source`,
		`ALTER TABLE attempts DROP COLUMN baseline_notes`,
		`ALTER TABLE runs DROP COLUMN agent_spec_json`,
		`ALTER TABLE runs DROP COLUMN chain_sessions`,
		`INSERT INTO schema_meta(key, value) VALUES('version','2')`,
		`INSERT INTO runs(id, status, started_at) VALUES('r1','completed','2026-01-01T00:00:00Z')`,
		`INSERT INTO attempts(run_id, task_id, status, has_baseline, baseline_id,
		 baseline_tokens, baseline_output)
		 VALUES('r1','t1','ok',1,'sess-1',9000,'the recorded answer')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v2 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v2 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	got, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want the row that was already in the database", len(got))
	}
	// v2's own column is untouched: the upgrade adds, it does not rewrite.
	if got[0].Baseline.Output != "the recorded answer" {
		t.Errorf("baseline output = %q, want v2's column left alone", got[0].Baseline.Output)
	}
	// Empty provenance is the absence of a record, not a recording that had no
	// agent. The report has to tell those apart, and cannot if the upgrade
	// invents a name for a row that never had one.
	if got[0].Baseline.AgentName != "" || got[0].Baseline.AgentModel != "" ||
		got[0].Baseline.Source != "" || got[0].Baseline.Notes != "" {
		t.Errorf("provenance = %+v, want empty for a row written before v3", got[0].Baseline)
	}
	// Being writable at the new revision is the point of upgrading rather than
	// refusing a database that already holds a paid-for run.
	if err := s.SaveAttempt(ctx, Attempt{
		RunID: "r1", TaskID: "t2", Status: CellOK,
		Baseline: BaselineComparison{
			Present: true, AgentName: "cursor", Source: "cursor-composer",
			Notes: "records no cost",
		},
	}); err != nil {
		t.Fatalf("save provenance into an upgraded database: %v", err)
	}
	again, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range again {
		if a.TaskID != "t2" {
			continue
		}
		if a.Baseline.AgentName != "cursor" || a.Baseline.Source != "cursor-composer" ||
			a.Baseline.Notes != "records no cost" {
			t.Errorf("provenance = %+v after the upgrade", a.Baseline)
		}
	}
}

// TestOpenUpgradesAV3Database is the v3-to-v4 half: a database written when a
// run recorded only the four labels it is compared by. It upgrades, and the run
// reads back with no configuration -- which is what it is, not a run that
// configured nothing.
//
// The distinction is the whole point of the column: the report has to be able
// to say "this run loaded the user's skills" or "nothing was recorded about
// skills", and an upgrade that filled in a plausible default would make the
// second unreadable.
func TestOpenUpgradesAV3Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`ALTER TABLE runs DROP COLUMN agent_spec_json`,
		`ALTER TABLE runs DROP COLUMN chain_sessions`,
		`INSERT INTO schema_meta(key, value) VALUES('version','3')`,
		`INSERT INTO runs(id, name, status, started_at, agent_kind, agent_model)
		 VALUES('r1','nightly','completed','2026-01-01T00:00:00Z','claude_code','sonnet')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v3 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v3 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	run, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("get the run that was already in the database: %v", err)
	}
	// Reading the row at all is half the assertion: runColumns names the new
	// column, so a migration that failed to add it fails here.
	if _, ok := run.AgentSpec(); ok {
		t.Error("a row written before v4 must not claim to have recorded a configuration")
	}
	if run.AgentKind != "claude_code" || run.AgentModel != "sonnet" {
		t.Errorf("the labels the row did carry were disturbed: %+v", run)
	}
	// Being writable at the new revision is the point of upgrading rather than
	// refusing a database that already holds a paid-for run.
	run.AgentSpecJSON = `{"kind":"claude_code","plugin_dirs":["/skills"]}`
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("write a configuration into an upgraded database: %v", err)
	}
	again, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := again.AgentSpec()
	if !ok || len(spec.PluginDirs) != 1 || spec.PluginDirs[0] != "/skills" {
		t.Errorf("spec = %+v, ok=%v after the upgrade", spec, ok)
	}
}

// TestOpenUpgradesAV4Database covers the v4 -> v5 step, which is the one where
// the new column is not a gap in the record.
//
// Every earlier column carried an "empty means nobody recorded this" reading,
// and reading one of those wrong makes the tool report a fact as unknown. This
// one is different: no build that wrote a v4 row could chain a session, so 0 is
// not silence, it is the answer.
func TestOpenUpgradesAV4Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`ALTER TABLE runs DROP COLUMN chain_sessions`,
		`INSERT INTO schema_meta(key, value) VALUES('version','4')`,
		`INSERT INTO runs(id, name, status, started_at, agent_kind, agent_model, agent_spec_json)
		 VALUES('r1','nightly','completed','2026-01-01T00:00:00Z','claude_code','sonnet',
		        '{"kind":"claude_code"}')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v4 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v4 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	run, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("get the run that was already in the database: %v", err)
	}
	if run.ChainSessions {
		t.Error("a row written before v5 cannot have chained a session")
	}
	if _, ok := run.AgentSpec(); !ok {
		t.Error("the upgrade disturbed what v4 had already recorded")
	}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("write into an upgraded database: %v", err)
	}
	again, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ChainSessions {
		t.Error("an unchained run came back chained")
	}
}

// TestRunRecordsWhetherItChainedASession is the round trip the report depends
// on: a chained run has to be distinguishable from an unchained one after the
// fact, because the cells of a chain are a sequence and not independent
// samples, and nothing on an attempt row says so.
func TestRunRecordsWhetherItChainedASession(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	r := sampleRun("chained")
	r.ChainSessions = true
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "chained")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ChainSessions {
		t.Error("a chained run came back unchained, which is the state the report cannot recover from a cell")
	}

	// And the flag is not a one-way latch: a later write of the same run says
	// what the current configuration is.
	got.ChainSessions = false
	if err := s.CreateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, err := s.GetRun(ctx, "chained")
	if err != nil {
		t.Fatal(err)
	}
	if back.ChainSessions {
		t.Error("rewriting a run did not update whether it chained")
	}
}

// TestAnInterruptedUpgradeCanBeReRun covers the window between a migration
// step's DDL and the version bump: the column already exists and the version
// still says v1. ADD COLUMN is not idempotent, so without the guard the next
// open dies on "duplicate column name" -- a confusing way to say "nothing is
// wrong".
func TestAnInterruptedUpgradeCanBeReRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "midway.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_meta(key, value) VALUES('version','1')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("re-running an interrupted migration must be a no-op, got: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d", v, schemaVersion)
	}
}

func TestRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	want := sampleRun("r1")
	if err := s.CreateRun(ctx, want); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Name != want.Name || got.AgentModel != "opus" || got.Backend != "local" {
		t.Errorf("run = %+v", got)
	}
	if got.Samples != 3 || got.Concurrency != 4 {
		t.Errorf("samples/concurrency = %d/%d", got.Samples, got.Concurrency)
	}
	if got.FactorAxis != "model" || got.FactorLevel != "opus" || got.SweepID != "sw1" {
		t.Errorf("factor labels lost: %+v", got)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("started = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if got.Status != RunRunning {
		t.Errorf("status = %q", got.Status)
	}
	// The configuration is read back whole, and the fields a comparison needs
	// are among them: the plugin directories are what the skill factor varies,
	// and the isolation flags decide whether they survive.
	spec, ok := got.AgentSpec()
	if !ok {
		t.Fatalf("the run's configuration did not survive the round trip: %q", got.AgentSpecJSON)
	}
	if len(spec.PluginDirs) != 1 || spec.PluginDirs[0] != "/skills/a" || !spec.SkillsActive() {
		t.Errorf("spec = %+v, want the declared skill directory and it active", spec)
	}
	// Whether the run chained a session's cells reads back too. The report
	// cannot recover it from the cells -- two rows sharing a workspace look the
	// same as two that happened to be given the same path -- so a lost flag is
	// a page that presents one continuation as k independent samples.
	if !got.ChainSessions {
		t.Error("the run came back unchained, which no cell row can contradict")
	}
}

func TestGetRunReportsMissing(t *testing.T) {
	_, err := openTemp(t).GetRun(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestMissingRunNamesTheDatabase pins the one piece of context that turns a
// confusing failure into an obvious one. "run X not found" reads like a broken
// tool; "not found in aeval.db" reads like the wrong --db, which is what it
// almost always is. The sentinel still has to match, because callers branch on
// it rather than on the wording.
func TestMissingRunNamesTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	_, err := s.GetRun(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing run must still match ErrNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), s.path) {
		t.Errorf("the error does not say which database was searched: %v", err)
	}

	// FinishRun and DeleteRun are the other two lookups a user can miss with,
	// and they have to say the same thing.
	if err := s.FinishRun(ctx, "nope", RunCompleted, ""); !strings.Contains(err.Error(), s.path) {
		t.Errorf("FinishRun does not name the database: %v", err)
	}
	if err := s.DeleteRun(ctx, "nope"); !strings.Contains(err.Error(), s.path) {
		t.Errorf("DeleteRun does not name the database: %v", err)
	}
}

func TestFinishRunStampsTheOutcome(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "r1", RunCompleted, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, _ := s.GetRun(ctx, "r1")
	if got.Status != RunCompleted {
		t.Errorf("status = %q", got.Status)
	}
	if got.EndedAt == nil {
		t.Error("ended_at was not set")
	}
	if err := s.FinishRun(ctx, "missing", RunCompleted, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("finishing a missing run: %v", err)
	}
}

func TestCreateRunUpserts(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	r := sampleRun("r1")
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Notes = "updated"
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("second CreateRun: %v", err)
	}
	runs, err := s.ListRuns(ctx, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Notes != "updated" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestListRunsOrdersAndFilters(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	base := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

	a := sampleRun("a")
	a.StartedAt = base
	b := sampleRun("b")
	b.StartedAt = base.Add(time.Hour)
	b.SweepID, b.FactorLevel = "sw2", "sonnet"
	c := sampleRun("c")
	c.StartedAt = base.Add(2 * time.Hour)
	c.SweepID, c.FactorAxis, c.FactorLevel = "sw2", "skill", "none"
	for _, r := range []Run{a, b, c} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	all, _ := s.ListRuns(ctx, RunFilter{})
	if len(all) != 3 {
		t.Fatalf("runs = %d, want 3", len(all))
	}
	if all[0].ID != "c" || all[2].ID != "a" {
		t.Errorf("order = %s,%s,%s; want newest first", all[0].ID, all[1].ID, all[2].ID)
	}

	bySweep, _ := s.ListRuns(ctx, RunFilter{SweepID: "sw2"})
	if len(bySweep) != 2 {
		t.Errorf("sweep filter returned %d, want 2", len(bySweep))
	}

	byAxis, _ := s.ListRuns(ctx, RunFilter{FactorAxis: "skill"})
	if len(byAxis) != 1 || byAxis[0].ID != "c" {
		t.Errorf("axis filter returned %+v", byAxis)
	}

	limited, _ := s.ListRuns(ctx, RunFilter{Limit: 2})
	if len(limited) != 2 {
		t.Errorf("limit returned %d, want 2", len(limited))
	}
}

func TestDeleteRunTakesItsAttemptsWithIt(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAttempt(ctx, sampleAttempt("r1", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "r1"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := s.GetRun(ctx, "r1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("run survived deletion: %v", err)
	}
	attempts, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Errorf("attempts survived their run: %d", len(attempts))
	}
}

func TestAttemptRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	want := sampleAttempt("r1", "t1")
	if err := s.SaveAttempt(ctx, want); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	got, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("attempts = %d", len(got))
	}
	g := got[0]
	switch {
	case g.TaskID != "t1" || g.Category != "bugfix":
		t.Errorf("identity = %+v", g)
	case !g.Graded || !g.Passed || g.Score != 0.75 || g.Threshold != 1:
		t.Errorf("verdict = %+v", g)
	case g.DurationMs != 1234 || g.CostUSD != 0.5:
		t.Errorf("cost = %+v", g)
	case g.InputTokens != 100 || g.OutputTokens != 20 || g.CacheReadTokens != 5 || g.CacheCreateTokens != 1:
		t.Errorf("tokens = %+v", g)
	case g.Turns != 7 || g.ToolCalls != 9 || g.ToolErrors != 1:
		t.Errorf("counts = %+v", g)
	case g.StopReason != "completed" || g.Error != "warn":
		t.Errorf("outcome = %+v", g)
	case g.WorkspaceDir != "/tmp/ws":
		t.Errorf("workspace = %q", g.WorkspaceDir)
	}
	if len(g.Tags) != 2 || g.Tags[0] != "go" {
		t.Errorf("tags = %v", g.Tags)
	}
	if len(g.Grades) != 1 || g.Grades[0].Grader != "tests" || !g.Grades[0].Passed {
		t.Errorf("grades = %+v", g.Grades)
	}
	if len(g.Trace) == 0 {
		t.Error("trace did not survive")
	}
	if !g.Baseline.Present || g.Baseline.ID != "sess-1" || g.Baseline.Tokens != 9000 ||
		g.Baseline.Ms != 60000 || g.Baseline.Cost != 0.25 || g.Baseline.Turns != 12 {
		t.Errorf("baseline = %+v", g.Baseline)
	}
	// The recorded answer is why the column exists: without it the report can
	// compare what two attempts cost but never whether they agreed.
	if g.Baseline.Output != "the recorded answer" {
		t.Errorf("baseline output = %q, want the recorded answer to survive storage", g.Baseline.Output)
	}
	// The provenance is stored rather than re-read for the same reason the
	// numbers are: the report is a function of the database, and a baseline
	// document living beside a dataset is not consulted at render time. Without
	// these four, a replay against a recording made by another agent renders as
	// a like-for-like comparison.
	if g.Baseline.AgentName != "cursor" || g.Baseline.AgentModel != "grok-4.5" ||
		g.Baseline.Source != "cursor-composer" || g.Baseline.Notes != "records no cost" {
		t.Errorf("baseline provenance = %q/%q/%q/%q, want the recording's own",
			g.Baseline.AgentName, g.Baseline.AgentModel, g.Baseline.Source, g.Baseline.Notes)
	}
}

func TestAttemptUpsertsOnTheSameCell(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	a := sampleAttempt("r1", "t1")
	if err := s.SaveAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Passed = false
	a.Score = 0
	if err := s.SaveAttempt(ctx, a); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ := s.ListAttempts(ctx, "r1")
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want 1 (the cell is the key)", len(got))
	}
	if got[0].Passed {
		t.Error("the second save did not replace the first")
	}
}

func TestSamplesAreDistinctCells(t *testing.T) {
	// pass@k runs a task k times; the samples must not collapse onto one row.
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a := sampleAttempt("r1", "t1")
		a.Sample = i
		if err := s.SaveAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.ListAttempts(ctx, "r1")
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(got))
	}
	for i, a := range got {
		if a.Sample != i {
			t.Errorf("attempt %d has sample %d; ListAttempts must order by sample", i, a.Sample)
		}
	}
}

func TestSaveAttemptRejectsAMalformedTrace(t *testing.T) {
	// Storing invalid JSON would push the failure into the reporting layer,
	// where it would look like a rendering bug.
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	a := sampleAttempt("r1", "t1")
	a.Trace = json.RawMessage(`{"oops"`)
	if err := s.SaveAttempt(ctx, a); err == nil {
		t.Fatal("a malformed trace must be refused")
	}
}

func TestSaveAttemptRequiresIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.SaveAttempt(ctx, Attempt{TaskID: "t1"}); err == nil {
		t.Error("an attempt with no run id must be refused")
	}
	if err := s.SaveAttempt(ctx, Attempt{RunID: "r1"}); err == nil {
		t.Error("an attempt with no task id must be refused")
	}
}

func TestColumnListsMatchTheirValues(t *testing.T) {
	// The insert builds its values as a slice and checks the count against the
	// column list. This asserts the mechanism itself works, so a future column
	// addition fails loudly here rather than in a saved run.
	if err := checkArity("attempts", attemptColumns, make([]any, countColumns(attemptColumns))); err != nil {
		t.Errorf("arity check rejected a matching slice: %v", err)
	}
	err := checkArity("attempts", attemptColumns, make([]any, countColumns(attemptColumns)-1))
	if err == nil {
		t.Fatal("arity check accepted a short slice")
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("the error should name the table, got %v", err)
	}
}

func countColumns(list string) int {
	n := 0
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}

func TestOpenCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "runs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if s.Path() != path {
		t.Errorf("path = %q", s.Path())
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("an empty path must be refused")
	}
}

func TestTagsAndGradesNeverStoreAsNull(t *testing.T) {
	// NULL and [] decode differently; a report that has to special-case nil
	// slices is a report that will forget to.
	ctx := context.Background()
	s := openTemp(t)
	if err := s.CreateRun(ctx, sampleRun("r1")); err != nil {
		t.Fatal(err)
	}
	a := sampleAttempt("r1", "t1")
	a.Tags, a.Grades = nil, nil
	if err := s.SaveAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAttempts(ctx, "r1")
	if got[0].Tags == nil || got[0].Grades == nil {
		t.Errorf("tags/grades came back nil: %+v", got[0])
	}
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

// sampleReport is a registration with every column set, so a round trip that
// drops one of them shows up rather than reading as a zero that was never
// written.
func sampleReport(id string) Report {
	return Report{
		ID:           id,
		Kind:         ReportKindRun,
		Title:        "nightly / model=opus",
		Subject:      "nightly / model=opus",
		ParamsJSON:   `{"run":"r1"}`,
		OptionsJSON:  `{"max_inline_bytes":8388608}`,
		CreatedAt:    time.Date(2026, 4, 5, 6, 7, 8, 900, time.UTC),
		PayloadHash:  "9f2c4e6a8b0d1f3a5c7e9b1d3f5a7c9e",
		PayloadBytes: 123456,
		DataVersion:  2,
	}
}

func TestReportRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	want := sampleReport("a1b2c3d4e5f60718")
	if err := s.SaveReport(ctx, want); err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	got, err := s.GetReport(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if got.Kind != want.Kind || got.Title != want.Title || got.Subject != want.Subject {
		t.Errorf("labels came back as %+v, want %+v", got, want)
	}
	if got.ParamsJSON != want.ParamsJSON || got.OptionsJSON != want.OptionsJSON {
		t.Errorf("the request came back as %q / %q", got.ParamsJSON, got.OptionsJSON)
	}
	if got.PayloadHash != want.PayloadHash || got.PayloadBytes != want.PayloadBytes {
		t.Errorf("the claim came back as %s / %d", got.PayloadHash, got.PayloadBytes)
	}
	if got.DataVersion != want.DataVersion {
		t.Errorf("data version = %d, want %d", got.DataVersion, want.DataVersion)
	}
	// Nanoseconds survive: the generation stamp in the payload is formatted
	// from this instant, so truncating it here would change the document.
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created at = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
}

// TestRegistrationsAreALogNotAState pins the append-only choice.
//
// Collapsing two registrations of one view into a row would keep the latest
// hash and throw away the earlier one, and the pair of hashes is the only
// evidence that the document changed between the two. A store that deduplicated
// would make that impossible to see afterwards.
func TestRegistrationsAreALogNotAState(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	first := sampleReport("1111111111111111")
	second := sampleReport("2222222222222222")
	second.PayloadHash = "00000000000000000000000000000000"
	second.CreatedAt = first.CreatedAt.Add(time.Hour)
	for _, r := range []Report{first, second} {
		if err := s.SaveReport(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the registry holds %d rows, want both registrations", len(got))
	}
	// Newest first, so the list reads as a log with the latest at the top.
	if got[0].ID != second.ID {
		t.Errorf("the listing starts with %s, want the newest registration", got[0].ID)
	}
}

func TestListReportsHonoursTheLimit(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	base := sampleReport("0000000000000000")
	for i := 0; i < 5; i++ {
		r := base
		r.ID = string(rune('a'+i)) + "000000000000000"
		r.CreatedAt = base.CreatedAt.Add(time.Duration(i) * time.Minute)
		if err := s.SaveReport(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListReports(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d rows", len(got))
	}
}

func TestGetReportReportsMissing(t *testing.T) {
	s := openTemp(t)
	_, err := s.GetReport(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want it to match ErrNotFound", err)
	}
	// And it names the file, for the same reason the run lookup does: "not
	// found" on its own reads like a bug, "not found in <file>" reads like the
	// wrong --db, which is what it almost always is.
	if !strings.Contains(err.Error(), s.Path()) {
		t.Errorf("err = %q, want it to name %s", err, s.Path())
	}
	if !strings.Contains(err.Error(), "report") {
		t.Errorf("err = %q, want it to say what kind of record was missing", err)
	}
}

func TestSaveReportRequiresIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	noID := sampleReport("")
	if err := s.SaveReport(ctx, noID); err == nil {
		t.Error("a report with no id was stored, so nothing can ever open it")
	}
	noKind := sampleReport("aaaaaaaaaaaaaaaa")
	noKind.Kind = ""
	if err := s.SaveReport(ctx, noKind); err == nil {
		t.Error("a report with no kind was stored, so nothing can say which view it is")
	}
}

// TestOpenUpgradesAV5Database is the other half of the version rule: a database
// written before reports existed has to keep working, and the table it never
// had has to appear.
//
// The fixture is a database this build wrote with the v6 addition deleted --
// the table -- rather than a copy of v5's DDL. A copy would keep passing after
// the real definition changed, which is the one thing a migration test must not
// be able to do. Deleting the table is also exactly what a genuine v5 database
// looks like: v6 added a table and touched nothing else.
func TestOpenUpgradesAV5Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`DROP TABLE reports`,
		`INSERT INTO schema_meta(key, value) VALUES('version','5')`,
		`INSERT INTO runs(id, name, status, started_at) VALUES('r1','nightly','completed','2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v5 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v5 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	if _, err := s.GetRun(ctx, "r1"); err != nil {
		t.Fatalf("the upgrade lost what was already in the database: %v", err)
	}
	// The new table has to be usable, not merely present: a create that ran
	// without the columns would still let the version bump.
	if err := s.SaveReport(ctx, sampleReport("abcdefabcdefabcd")); err != nil {
		t.Fatalf("write into an upgraded database: %v", err)
	}
	reports, err := s.ListReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Errorf("the upgraded database lists %d reports, want the one just written", len(reports))
	}
}

// TestAV5DatabaseKeepsWorkingAfterTheUpgradeIsRecorded covers the second open.
//
// The first open adds the table and bumps the version; the second has to be a
// no-op rather than replaying anything. A migration whose step assumed it was
// running on the old shape would fail here and nowhere else.
func TestAV5DatabaseKeepsWorkingAfterTheUpgradeIsRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{string(body), `DROP TABLE reports`,
		`INSERT INTO schema_meta(key, value) VALUES('version','5')`} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveReport(context.Background(), sampleReport("0011223344556677")); err != nil {
		t.Fatal(err)
	}
	d.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an upgraded database: %v", err)
	}
	defer again.Close()
	reports, err := again.ListReports(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].ID != "0011223344556677" {
		t.Errorf("the registry came back as %+v", reports)
	}
}

// TestOpenUpgradesAV6Database covers the columns that say a run worked in the
// directories its tasks record, and what happened to each one afterwards.
//
// The two readings that have to stay apart are "this row predates the column"
// and "this run did not work in place". They agree here -- no build before v7
// could work in place -- which is what makes 0 the right default rather than a
// third state to keep.
func TestOpenUpgradesAV6Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`ALTER TABLE attempts DROP COLUMN restore_notes`,
		`ALTER TABLE runs DROP COLUMN in_place`,
		`INSERT INTO schema_meta(key, value) VALUES('version','6')`,
		`INSERT INTO runs(id, status, started_at) VALUES('r1','completed','2026-01-01T00:00:00Z')`,
		`INSERT INTO attempts(run_id, task_id, status) VALUES('r1','t1','ok')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v6 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v6 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	ctx := context.Background()
	runs, err := s.ListRuns(ctx, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the row that was already there", len(runs))
	}
	if runs[0].InPlace {
		t.Error("a row written before v7 must read as not in place")
	}
	attempts, err := s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Read at all is half the assertion: scanAttempt names the column, so a
	// migration that failed to add it fails here rather than returning zeros.
	if len(attempts) != 1 || attempts[0].RestoreNotes != "" {
		t.Errorf("attempts = %+v, want one row with no restore notes", attempts)
	}

	// And the upgraded database takes the new values.
	if err := s.SaveAttempt(ctx, Attempt{
		RunID: "r1", TaskID: "t2", Status: CellOK,
		RestoreNotes: "put back at 2d347614 in /work/proj; nothing was left behind",
	}); err != nil {
		t.Fatalf("save into an upgraded database: %v", err)
	}
	attempts, err = s.ListAttempts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		if a.TaskID == "t2" && !strings.Contains(a.RestoreNotes, "nothing was left behind") {
			t.Errorf("restore notes = %q after the upgrade", a.RestoreNotes)
		}
	}
}

// TestOpenUpgradesAV7Database covers the replays table.
//
// The upgrade is empty by design -- v8 only added a table no old database has
// rows in -- so what the test pins is the pair of half-truths an empty
// migration could still tell: that the version was bumped (it is what stops
// the next start from trying again) and that the table exists with the
// columns the replay pages read and write (a missing column would fail the
// writes below, not the upgrade).
func TestOpenUpgradesAV7Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		string(body),
		`DROP TABLE replays`,
		`INSERT INTO schema_meta(key, value) VALUES('version','7')`,
		`INSERT INTO runs(id, status, started_at) VALUES('r1','completed','2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("build a v7 database (%s): %v", stmt, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a v7 database must upgrade rather than be refused: %v", err)
	}
	defer s.Close()
	if v, _ := s.existingVersion(); v != schemaVersion {
		t.Errorf("version = %d, want %d once the upgrade has run", v, schemaVersion)
	}

	// The upgraded database takes the new rows.
	ctx := context.Background()
	if err := s.CreateReplay(ctx, Replay{
		ID: "rep1", Source: "claude", SessionID: "aaaabbbb",
		Harness: "clean-room", Skill: "none", Model: "sonnet", Split: "session",
	}); err != nil {
		t.Fatalf("create a replay in an upgraded database: %v", err)
	}
	got, err := s.GetReplay(ctx, "rep1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ReplayQueued {
		t.Errorf("status = %q, want the queued default", got.Status)
	}
}

// TestRebootReplaysInterruptsUnfinishedWork pins what a start owes the rows a
// previous process left behind: a replay is a background task of one server
// process and nothing resumes it, so queued and running are marked
// interrupted rather than left promising work that is never coming. Terminal
// rows are not touched -- rewriting history is not cleanup.
//
// A missing check would leave an unfinished row waiting forever, which is the
// same lie as a queue that never moves.
func TestRebootReplaysInterruptsUnfinishedWork(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	makeReplay := func(id, status string) {
		t.Helper()
		if err := s.CreateReplay(ctx, Replay{ID: id, Source: "claude", SessionID: "x", Status: status}); err != nil {
			t.Fatalf("create %s as %s: %v", id, status, err)
		}
	}
	// create-as-status covers queued and done; running and failed go through
	// FinishReplay because that is the only way they exist in production.
	makeReplay("q1", ReplayQueued)
	makeReplay("d1", ReplayDone)
	makeReplay("r1", ReplayQueued)
	if err := s.FinishReplay(ctx, "r1", ReplayRunning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	makeReplay("f1", ReplayQueued)
	if err := s.FinishReplay(ctx, "f1", ReplayFailed, "it failed before the stop", "", ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.RebootReplays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("RebootReplays interrupted %d, want the queued and the running row", n)
	}

	for _, tc := range []struct{ id, want, why string }{
		{"q1", ReplayInterrupted, "a queued row"},
		{"r1", ReplayInterrupted, "a running row"},
		{"d1", ReplayDone, "a finished row has nothing waiting"},
		{"f1", ReplayFailed, "a failed row already ended"},
	} {
		got, err := s.GetReplay(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.want {
			t.Errorf("%s after the reboot = %q, want %q", tc.why, got.Status, tc.want)
		}
	}

	// A second reboot is a no-op: the interrupted rows are not unfinished
	// work any more, and re-marking them would reset their finished_at.
	if n, _ := s.RebootReplays(ctx); n != 0 {
		t.Errorf("a second reboot interrupted %d, want 0", n)
	}
}

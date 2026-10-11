// Package storage persists evaluation runs to SQLite.
//
// # Why SQLite
//
// A single file that can be queried with SQL, copied, committed as a fixture or
// opened read-only by the reporting layer. No server, no migration tooling, no
// external dependency -- which matters because an evaluation harness is most
// useful when it can run anywhere, including a container with no network.
//
// # Concurrency
//
// SQLite permits one writer at a time. The engine grades tasks concurrently, so
// Store serialises writes behind a mutex and relies on WAL mode plus a busy
// timeout for any other process touching the same file. Reads are unrestricted.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required
)

//go:embed schema.sql
var schemaFS embed.FS

// schemaVersion is bumped whenever schema.sql changes. Older databases are
// rejected with a clear message rather than failing later on a missing column.
//
// v2 added task_runs.reward and task_runs.guard_json (the RL-facing columns).
// v3 added runs.mode (agent / oracle / baseline / rl).
// v4 added task_runs.reward_kind, without which a stored 0 cannot be read.
const schemaVersion = 4

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("record not found")

// SchemaVersion is the schema revision this build writes. It is exported so
// `aeval version` can report the real number instead of a hard-coded one that
// drifts the moment a migration is added.
func SchemaVersion() int { return schemaVersion }

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// RunRecord is one evaluation run.
type RunRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs int64      `json:"duration_ms"`

	DatasetName    string `json:"dataset_name"`
	DatasetVersion string `json:"dataset_version"`
	DatasetHash    string `json:"dataset_hash"`
	DatasetPath    string `json:"dataset_path"`

	AgentKind      string `json:"agent_kind"`
	AgentModel     string `json:"agent_model"`
	SandboxBackend string `json:"sandbox_backend"`
	SandboxImage   string `json:"sandbox_image"`
	// Mode is the run's question: agent, oracle or baseline.
	Mode string `json:"mode"`

	SamplesPerTask int   `json:"samples_per_task"`
	Concurrency    int   `json:"concurrency"`
	Seed           int64 `json:"seed"`

	ConfigJSON  string `json:"config_json,omitempty"`
	ConfigHash  string `json:"config_hash"`
	Notes       string `json:"notes,omitempty"`
	ToolVersion string `json:"tool_version"`
	HostArch    string `json:"host_arch"`
	Error       string `json:"error,omitempty"`
}

// Status values for a run.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "cancelled"
)

// TaskRunRecord is one graded attempt.
type TaskRunRecord struct {
	ID          string   `json:"id"`
	RunID       string   `json:"run_id"`
	TaskID      string   `json:"task_id"`
	SampleIndex int      `json:"sample_index"`
	Category    string   `json:"category,omitempty"`
	Difficulty  string   `json:"difficulty,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	Status     string  `json:"status"`
	StopReason string  `json:"stop_reason"`
	Passed     bool    `json:"passed"`
	Score      float64 `json:"score"`
	Threshold  float64 `json:"threshold"`
	// Reward is the scalar training signal for this attempt, produced by the
	// task's declared reward contract. It is not a function of Score alone: an
	// attempt rejected by an anti-gaming guard scores 0 whatever its Score.
	Reward float64 `json:"reward"`
	// RewardKind is the density that produced it: "score" (dense) or "pass".
	RewardKind string `json:"reward_kind,omitempty"`

	SessionID  string     `json:"session_id,omitempty"`
	Model      string     `json:"model,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs int64      `json:"duration_ms"`

	CostUSD           float64 `json:"cost_usd"`
	CostReliable      bool    `json:"cost_reliable"`
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	CacheReadTokens   int     `json:"cache_read_tokens"`
	CacheCreateTokens int     `json:"cache_create_tokens"`
	Turns             int     `json:"turns"`
	ToolCalls         int     `json:"tool_calls"`
	ToolErrors        int     `json:"tool_errors"`
	TTFTMs            int64   `json:"ttft_ms"`
	APIDurationMs     int64   `json:"api_duration_ms"`

	// Diff is the agent's patch, Answer its final text.
	Diff   string `json:"diff,omitempty"`
	Answer string `json:"answer,omitempty"`

	// Blobs as raw JSON, kept verbatim so the report layer can reconstruct the
	// full attempt without a schema change per field.
	AttemptJSON string `json:"-"`
	TraceJSON   string `json:"-"`
	GradesJSON  string `json:"-"`
	GuardJSON   string `json:"-"`

	// Violations lists the anti-gaming guards this attempt tripped, and Restored
	// the paths reset to baseline before grading. Both are decoded from
	// GuardJSON for convenience; the JSON is the stored form.
	Violations []string `json:"violations,omitempty"`
	Restored   []string `json:"restored,omitempty"`

	Error         string `json:"error,omitempty"`
	WorkspaceDir  string `json:"workspace_dir,omitempty"`
	RawStreamPath string `json:"raw_stream_path,omitempty"`
}

// GuardRecord is the JSON document stored in task_runs.guard_json.
type GuardRecord struct {
	Violations []string `json:"violations,omitempty"`
	Restored   []string `json:"restored,omitempty"`
}

// Task run status values.
const (
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
	TaskStatusSkipped   = "skipped"
)

// GraderResultRecord is a flattened grader verdict.
type GraderResultRecord struct {
	RunID       string  `json:"run_id"`
	TaskRunID   string  `json:"task_run_id"`
	TaskID      string  `json:"task_id"`
	SampleIndex int     `json:"sample_index"`
	Seq         int     `json:"seq"`
	Grader      string  `json:"grader"`
	Type        string  `json:"type"`
	Passed      bool    `json:"passed"`
	Score       float64 `json:"score"`
	Weight      float64 `json:"weight"`
	Required    bool    `json:"required"`
	Reason      string  `json:"reason,omitempty"`
	Error       string  `json:"error,omitempty"`
	DurationMs  int64   `json:"duration_ms"`
	DetailsJSON string  `json:"-"`
}

// ToolCallRecord is a flattened tool invocation.
type ToolCallRecord struct {
	RunID       string `json:"run_id"`
	TaskRunID   string `json:"task_run_id"`
	TaskID      string `json:"task_id"`
	SampleIndex int    `json:"sample_index"`
	Seq         int    `json:"seq"`
	ToolUseID   string `json:"tool_use_id"`
	Name        string `json:"name"`
	IsError     bool   `json:"is_error"`
	DurationMs  int64  `json:"duration_ms"`
	InputJSON   string `json:"input_json,omitempty"`
	Output      string `json:"output,omitempty"`
}

// MetricRecord is a computed metric value.
type MetricRecord struct {
	RunID     string  `json:"run_id"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Kind      string  `json:"kind"`
	Label     string  `json:"label,omitempty"`
	ExtraJSON string  `json:"-"`
}

// RunSummary is the compact view used in listings and comparisons.
type RunSummary struct {
	RunRecord
	TaskCount     int                `json:"task_count"`
	PassedCount   int                `json:"passed_count"`
	ResolveRate   float64            `json:"resolve_rate"`
	TotalCostUSD  float64            `json:"total_cost_usd"`
	MeanLatencyMs float64            `json:"mean_latency_ms"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store is a handle to an evaluation database.
type Store struct {
	db   *sql.DB
	path string
	// writeMu serialises writers; SQLite allows only one at a time and the
	// engine saves results from many goroutines.
	writeMu sync.Mutex
}

// Open opens (creating if needed) the database at path.
//
// Use ":memory:" for an ephemeral store in tests; each call then gets its own
// private database.
func Open(path string) (*Store, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// A single connection avoids SQLITE_BUSY entirely for in-process writes;
	// WAL keeps reads concurrent for other processes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path}
	if err := s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// memorySeq makes every in-memory database unique.
//
// "file::memory:?cache=shared" would be a single database shared by every
// connection in the process, so two Open(":memory:") calls would silently see
// each other's rows. Tests rely on isolation, so each call gets its own name.
var memorySeq atomic.Int64

func buildDSN(path string) (string, error) {
	// Pragmas are set per connection through the DSN. WAL survives crashes and
	// allows readers; foreign_keys must be enabled explicitly in SQLite;
	// busy_timeout makes a concurrent process wait rather than fail.
	pragmas := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		name := fmt.Sprintf("agenteval_mem_%d", memorySeq.Add(1))
		// journal_mode=WAL is meaningless for an in-memory database and the
		// driver rejects it, so use the memory-safe pragma set instead.
		memPragmas := "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
		return "file:" + name + "?mode=memory&cache=shared&" + memPragmas, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("create database directory: %w", err)
	}
	return "file:" + abs + "?" + pragmas, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close releases the database handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying handle for ad-hoc queries by the reporting layer.
func (s *Store) DB() *sql.DB { return s.db }

// Migrate applies the schema and verifies the version.
func (s *Store) Migrate(ctx context.Context) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read embedded schema: %w", err)
	}
	// The schema is written with IF NOT EXISTS throughout, so applying it is
	// idempotent and needs no per-statement bookkeeping.
	if _, err := s.db.ExecContext(ctx, string(raw)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS does not add columns to a table that already
	// exists, so a database written by an older build needs the additive
	// migrations run explicitly. Without them an upgraded binary would fail at
	// the first INSERT with "no such column" -- on a database that looks fine.
	if err := s.migrateColumns(ctx); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf(
			"database %s has schema version %d but this build understands %d; "+
				"it was written by a newer version of the tool", s.path, version, schemaVersion)
	}
	if version < schemaVersion {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

// columnMigration is one additive column introduced after the first release.
type columnMigration struct {
	table  string
	column string
	ddl    string
}

// columnMigrations are applied in order and are safe to re-run.
var columnMigrations = []columnMigration{
	{
		table: "task_runs", column: "reward",
		ddl: "ALTER TABLE task_runs ADD COLUMN reward REAL NOT NULL DEFAULT 0",
	},
	{
		table: "task_runs", column: "guard_json",
		ddl: "ALTER TABLE task_runs ADD COLUMN guard_json TEXT NOT NULL DEFAULT ''",
	},
	{
		table: "runs", column: "mode",
		ddl: "ALTER TABLE runs ADD COLUMN mode TEXT NOT NULL DEFAULT 'agent'",
	},
	{
		table: "task_runs", column: "reward_kind",
		ddl: "ALTER TABLE task_runs ADD COLUMN reward_kind TEXT NOT NULL DEFAULT ''",
	},
}

func (s *Store) migrateColumns(ctx context.Context) error {
	for _, m := range columnMigrations {
		has, err := s.hasColumn(ctx, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

// hasColumn consults the table-valued pragma so the table name can be a
// parameter; PRAGMA table_info(<name>) cannot be.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// CreateRun inserts a run row.
func (s *Store) CreateRun(ctx context.Context, r *RunRecord) error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("run id is required")
	}
	if r.Status == "" {
		r.Status = StatusRunning
	}
	// An empty mode means "the caller did not care", which historically meant an
	// ordinary agent run. Defaulting here keeps old callers correct and stops an
	// empty string from becoming a fourth, meaningless mode in the database.
	if r.Mode == "" {
		r.Mode = "agent"
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, name, status, created_at, finished_at, duration_ms,
			dataset_name, dataset_version, dataset_hash, dataset_path,
			agent_kind, agent_model, sandbox_backend, sandbox_image, mode,
			samples_per_task, concurrency, seed,
			config_json, config_hash, notes, tool_version, host_arch
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Status, r.CreatedAt.UTC().Format(time.RFC3339Nano), nullTime(r.FinishedAt), r.DurationMs,
		r.DatasetName, r.DatasetVersion, r.DatasetHash, r.DatasetPath,
		r.AgentKind, r.AgentModel, r.SandboxBackend, r.SandboxImage, r.Mode,
		r.SamplesPerTask, r.Concurrency, r.Seed,
		r.ConfigJSON, r.ConfigHash, r.Notes, r.ToolVersion, r.HostArch,
	)
	if err != nil {
		return fmt.Errorf("insert run %s: %w", r.ID, err)
	}
	return nil
}

// FinishRun marks a run terminal and stores its computed metrics.
func (s *Store) FinishRun(ctx context.Context, runID, status, errMsg string, duration time.Duration) error {
	now := time.Now().UTC()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status=?, finished_at=?, duration_ms=?, error=?
		WHERE id=?`,
		status, now.Format(time.RFC3339Nano), duration.Milliseconds(), errMsg, runID)
	if err != nil {
		return fmt.Errorf("finish run %s: %w", runID, err)
	}
	return requireAffected(res, "run", runID)
}

// ReopenRun puts a finished run back into the running state.
//
// Used when a run is resumed: the row must not be re-inserted (its id already
// exists and the original provenance is worth keeping), but leaving it marked
// "completed" while work is in flight would make listings lie.
func (s *Store) ReopenRun(ctx context.Context, runID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, finished_at=NULL, error='' WHERE id=?`,
		StatusRunning, runID)
	if err != nil {
		return fmt.Errorf("reopen run %s: %w", runID, err)
	}
	return requireAffected(res, "run", runID)
}

// SaveMetrics replaces the metric rows for a run.
func (s *Store) SaveMetrics(ctx context.Context, runID string, metrics []MetricRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM run_metrics WHERE run_id=?`, runID); err != nil {
		return fmt.Errorf("clear metrics: %w", err)
	}
	for _, m := range metrics {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_metrics (run_id, name, value, unit, kind, label, extra_json)
			VALUES (?,?,?,?,?,?,?)`,
			runID, m.Name, m.Value, m.Unit, m.Kind, m.Label, m.ExtraJSON); err != nil {
			return fmt.Errorf("insert metric %s: %w", m.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Snapshot the headline numbers on the run row so listings need no join.
	return s.snapshotMetrics(ctx, runID)
}

func (s *Store) snapshotMetrics(ctx context.Context, runID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name, value FROM run_metrics WHERE run_id=?`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	snapshot := map[string]float64{}
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			return err
		}
		// JSON cannot represent Infinity or NaN. The metric layer guarantees
		// finite values, but a row written by an older build or by hand must not
		// be able to break the snapshot for every other metric.
		if math.IsInf(value, 0) || math.IsNaN(value) {
			continue
		}
		snapshot[name] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET metrics_json=? WHERE id=?`, string(raw), runID)
	return err
}

// SaveTaskRun upserts a graded attempt along with its grader and tool-call rows.
//
// The whole write is one transaction so a partially recorded attempt can never be
// read back by the report layer.
func (s *Store) SaveTaskRun(ctx context.Context, t *TaskRunRecord) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("task run id is required")
	}
	if t.Status == "" {
		t.Status = TaskStatusCompleted
	}
	tags, err := json.Marshal(t.Tags)
	if err != nil {
		tags = []byte("[]")
	}
	// Accept either the decoded lists or the raw blob, so a caller that built a
	// record by hand does not have to know about the storage encoding.
	guard := t.GuardJSON
	if guard == "" && (len(t.Violations) > 0 || len(t.Restored) > 0) {
		if raw, err := json.Marshal(GuardRecord{Violations: t.Violations, Restored: t.Restored}); err == nil {
			guard = string(raw)
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_runs (
			id, run_id, task_id, sample_index, category, difficulty, tags_json,
			status, stop_reason, passed, score, threshold, reward, reward_kind,
			session_id, model, started_at, ended_at, duration_ms,
			cost_usd, cost_reliable, input_tokens, output_tokens,
			cache_read_tokens, cache_create_tokens,
			turns, tool_calls, tool_errors, ttft_ms, api_duration_ms,
			diff, answer, attempt_json, trace_json, grades_json,
			guard_json, error, workspace_dir, raw_stream_path
		) VALUES (
			?,?,?,?,?,?,?,
			?,?,?,?,?,?,?,
			?,?,?,?,?,
			?,?,?,?,
			?,?,
			?,?,?,?,?,
			?,?,?,?,?,
			?,?,?,?
		)
		ON CONFLICT(run_id, task_id, sample_index) DO UPDATE SET
			status=excluded.status, stop_reason=excluded.stop_reason,
			passed=excluded.passed, score=excluded.score, threshold=excluded.threshold,
			reward=excluded.reward, reward_kind=excluded.reward_kind,
			session_id=excluded.session_id, model=excluded.model,
			started_at=excluded.started_at, ended_at=excluded.ended_at, duration_ms=excluded.duration_ms,
			cost_usd=excluded.cost_usd, cost_reliable=excluded.cost_reliable,
			input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_read_tokens=excluded.cache_read_tokens, cache_create_tokens=excluded.cache_create_tokens,
			turns=excluded.turns, tool_calls=excluded.tool_calls, tool_errors=excluded.tool_errors,
			ttft_ms=excluded.ttft_ms, api_duration_ms=excluded.api_duration_ms,
			diff=excluded.diff, answer=excluded.answer,
			attempt_json=excluded.attempt_json, trace_json=excluded.trace_json,
			grades_json=excluded.grades_json, guard_json=excluded.guard_json,
			error=excluded.error,
			workspace_dir=excluded.workspace_dir, raw_stream_path=excluded.raw_stream_path`,
		t.ID, t.RunID, t.TaskID, t.SampleIndex, t.Category, t.Difficulty, string(tags),
		t.Status, t.StopReason, boolInt(t.Passed), t.Score, t.Threshold, t.Reward, t.RewardKind,
		t.SessionID, t.Model, nullTime(t.StartedAt), nullTime(t.EndedAt), t.DurationMs,
		t.CostUSD, boolInt(t.CostReliable), t.InputTokens, t.OutputTokens,
		t.CacheReadTokens, t.CacheCreateTokens,
		t.Turns, t.ToolCalls, t.ToolErrors, t.TTFTMs, t.APIDurationMs,
		t.Diff, t.Answer, t.AttemptJSON, t.TraceJSON, t.GradesJSON,
		guard, t.Error, t.WorkspaceDir, t.RawStreamPath,
	); err != nil {
		return fmt.Errorf("upsert task run %s: %w", t.ID, err)
	}

	if err := replaceGraders(ctx, tx, t); err != nil {
		return err
	}
	if err := replaceToolCalls(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceGraders(ctx context.Context, tx *sql.Tx, t *TaskRunRecord) error {
	// Grader verdicts are re-derived on every save, so clear first; a re-run of a
	// task must not leave stale verdicts behind.
	if _, err := tx.ExecContext(ctx, `DELETE FROM grader_results WHERE task_run_id=?`, t.ID); err != nil {
		return err
	}
	if t.GradesJSON == "" {
		return nil
	}
	var results []struct {
		Grader     string  `json:"grader"`
		Type       string  `json:"type"`
		Passed     bool    `json:"passed"`
		Score      float64 `json:"score"`
		Weight     float64 `json:"weight"`
		Required   bool    `json:"required"`
		Reason     string  `json:"reason"`
		Error      string  `json:"error"`
		Duration   int64   `json:"duration"` // nanoseconds in Go's encoding
		DetailsRaw any     `json:"details"`
	}
	if err := json.Unmarshal([]byte(t.GradesJSON), &results); err != nil {
		return fmt.Errorf("decode grades for %s: %w", t.ID, err)
	}
	for i, r := range results {
		details := ""
		if r.DetailsRaw != nil {
			if raw, err := json.Marshal(r.DetailsRaw); err == nil {
				details = string(raw)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO grader_results (
				run_id, task_run_id, task_id, sample_index, seq, grader, type,
				passed, score, weight, required, reason, error, duration_ms, details_json
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.RunID, t.ID, t.TaskID, t.SampleIndex, i, r.Grader, r.Type,
			boolInt(r.Passed), r.Score, r.Weight, boolInt(r.Required),
			r.Reason, r.Error, r.Duration/1e6, details,
		); err != nil {
			return fmt.Errorf("insert grader result %s: %w", r.Grader, err)
		}
	}
	return nil
}

func replaceToolCalls(ctx context.Context, tx *sql.Tx, t *TaskRunRecord) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM tool_calls WHERE task_run_id=?`, t.ID); err != nil {
		return err
	}
	if t.TraceJSON == "" {
		return nil
	}
	var tr struct {
		ToolCalls []struct {
			Seq        int             `json:"seq"`
			ID         string          `json:"id"`
			Name       string          `json:"name"`
			Input      json.RawMessage `json:"input"`
			Output     string          `json:"output"`
			IsError    bool            `json:"is_error"`
			DurationMs int64           `json:"duration_ms"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(t.TraceJSON), &tr); err != nil {
		// A malformed trace is not worth failing the whole save; the task row and
		// graders are the important part.
		return nil
	}
	for _, c := range tr.ToolCalls {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_calls (
				run_id, task_run_id, task_id, sample_index, seq,
				tool_use_id, name, is_error, duration_ms, input_json, output
			) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			t.RunID, t.ID, t.TaskID, t.SampleIndex, c.Seq,
			c.ID, c.Name, boolInt(c.IsError), c.DurationMs, string(c.Input), clip(c.Output, 20000),
		); err != nil {
			return fmt.Errorf("insert tool call %d: %w", c.Seq, err)
		}
	}
	return nil
}

// SetRunTag stores an annotation.
func (s *Store) SetRunTag(ctx context.Context, runID, key, value string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_tags (run_id, key, value) VALUES (?,?,?)
		ON CONFLICT(run_id, key) DO UPDATE SET value=excluded.value`, runID, key, value)
	return err
}

// RunTags returns every annotation on a run.
func (s *Store) RunTags(ctx context.Context, runID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM run_tags WHERE run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// RunIDsWithTag returns the ids of runs annotated key=value, newest first.
//
// Only ids are returned, so a caller that needs full rows goes through GetRun
// and one projection of a run is defined rather than two that can drift.
func (s *Store) RunIDsWithTag(ctx context.Context, key, value string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id FROM runs r
		JOIN run_tags t ON t.run_id = r.id
		WHERE t.key = ? AND t.value = ?
		ORDER BY r.created_at DESC
		LIMIT ?`, key, value, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteRun removes a run and everything cascading from it.
func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, runID)
	if err != nil {
		return err
	}
	return requireAffected(res, "run", runID)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetRun loads one run.
func (s *Store) GetRun(ctx context.Context, runID string) (*RunRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, status, created_at, finished_at, duration_ms,
		       dataset_name, dataset_version, dataset_hash, dataset_path,
		       agent_kind, agent_model, sandbox_backend, sandbox_image, mode,
		       samples_per_task, concurrency, seed,
		       config_json, config_hash, notes, tool_version, host_arch, error
		FROM runs WHERE id=?`, runID)

	var r RunRecord
	var createdAt string
	var finishedAt sql.NullString
	err := row.Scan(
		&r.ID, &r.Name, &r.Status, &createdAt, &finishedAt, &r.DurationMs,
		&r.DatasetName, &r.DatasetVersion, &r.DatasetHash, &r.DatasetPath,
		&r.AgentKind, &r.AgentModel, &r.SandboxBackend, &r.SandboxImage, &r.Mode,
		&r.SamplesPerTask, &r.Concurrency, &r.Seed,
		&r.ConfigJSON, &r.ConfigHash, &r.Notes, &r.ToolVersion, &r.HostArch, &r.Error,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	r.CreatedAt = parseTime(createdAt)
	r.FinishedAt = parseNullTime(finishedAt)
	return &r, nil
}

// ListRuns returns recent runs, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, r.status, r.created_at, r.finished_at, r.duration_ms,
		       r.dataset_name, r.dataset_version, r.dataset_hash, r.dataset_path,
		       r.agent_kind, r.agent_model, r.sandbox_backend, r.sandbox_image, r.mode,
		       r.samples_per_task, r.concurrency, r.seed,
		       r.config_hash, r.notes, r.tool_version, r.host_arch, r.error,
		       r.metrics_json,
		       (SELECT COUNT(*) FROM task_runs t WHERE t.run_id = r.id),
		       (SELECT COUNT(*) FROM task_runs t WHERE t.run_id = r.id AND t.passed = 1)
		FROM runs r
		ORDER BY r.created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		var s RunSummary
		var createdAt string
		var finishedAt sql.NullString
		var metricsJSON string
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Status, &createdAt, &finishedAt, &s.DurationMs,
			&s.DatasetName, &s.DatasetVersion, &s.DatasetHash, &s.DatasetPath,
			&s.AgentKind, &s.AgentModel, &s.SandboxBackend, &s.SandboxImage, &s.Mode,
			&s.SamplesPerTask, &s.Concurrency, &s.Seed,
			&s.ConfigHash, &s.Notes, &s.ToolVersion, &s.HostArch, &s.Error,
			&metricsJSON, &s.TaskCount, &s.PassedCount,
		); err != nil {
			return nil, err
		}
		s.CreatedAt = parseTime(createdAt)
		s.FinishedAt = parseNullTime(finishedAt)
		if s.TaskCount > 0 {
			s.ResolveRate = float64(s.PassedCount) / float64(s.TaskCount)
		}
		if metricsJSON != "" {
			snapshot := map[string]float64{}
			if err := json.Unmarshal([]byte(metricsJSON), &snapshot); err == nil {
				s.Metrics = snapshot
				s.TotalCostUSD = snapshot["total_cost_usd"]
				s.MeanLatencyMs = snapshot["mean_latency_ms"]
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TaskRunsQuery filters the attempts returned for a run.
type TaskRunsQuery struct {
	RunID string
	// TaskID narrows to one task.
	TaskID string
	// Category and StopReason narrow by those fields.
	Category   string
	StopReason string
	// OnlyFailed returns just the failures.
	OnlyFailed bool
	// OrderBy is "task_id" (default) or "score" / "duration_ms" / "cost_usd".
	OrderBy string
	// Limit caps the result; 0 means no cap.
	Limit int
}

// TaskRuns lists attempts matching a query.
func (s *Store) TaskRuns(ctx context.Context, q TaskRunsQuery) ([]TaskRunRecord, error) {
	var where []string
	var args []any
	where = append(where, "run_id = ?")
	args = append(args, q.RunID)
	if q.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, q.TaskID)
	}
	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if q.StopReason != "" {
		where = append(where, "stop_reason = ?")
		args = append(args, q.StopReason)
	}
	if q.OnlyFailed {
		where = append(where, "passed = 0")
	}

	order := "task_id ASC, sample_index ASC"
	switch q.OrderBy {
	case "score":
		order = "score ASC, task_id ASC"
	case "duration_ms":
		order = "duration_ms DESC, task_id ASC"
	case "cost_usd":
		order = "cost_usd DESC, task_id ASC"
	}

	query := `SELECT ` + taskRunColumns + ` FROM task_runs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + order
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRuns(rows)
}

const taskRunColumns = `
	id, run_id, task_id, sample_index, category, difficulty, tags_json,
	status, stop_reason, passed, score, threshold, reward, reward_kind,
	session_id, model, started_at, ended_at, duration_ms,
	cost_usd, cost_reliable, input_tokens, output_tokens,
	cache_read_tokens, cache_create_tokens,
	turns, tool_calls, tool_errors, ttft_ms, api_duration_ms,
	diff, answer, attempt_json, trace_json, grades_json,
	guard_json, error, workspace_dir, raw_stream_path`

func scanTaskRuns(rows *sql.Rows) ([]TaskRunRecord, error) {
	var out []TaskRunRecord
	for rows.Next() {
		var t TaskRunRecord
		var tagsJSON, startedAt, endedAt sql.NullString
		var passed, costReliable int
		if err := rows.Scan(
			&t.ID, &t.RunID, &t.TaskID, &t.SampleIndex, &t.Category, &t.Difficulty, &tagsJSON,
			&t.Status, &t.StopReason, &passed, &t.Score, &t.Threshold, &t.Reward, &t.RewardKind,
			&t.SessionID, &t.Model, &startedAt, &endedAt, &t.DurationMs,
			&t.CostUSD, &costReliable, &t.InputTokens, &t.OutputTokens,
			&t.CacheReadTokens, &t.CacheCreateTokens,
			&t.Turns, &t.ToolCalls, &t.ToolErrors, &t.TTFTMs, &t.APIDurationMs,
			&t.Diff, &t.Answer, &t.AttemptJSON, &t.TraceJSON, &t.GradesJSON,
			&t.GuardJSON, &t.Error, &t.WorkspaceDir, &t.RawStreamPath,
		); err != nil {
			return nil, err
		}
		t.Passed = passed == 1
		t.CostReliable = costReliable == 1
		t.StartedAt = parseNullTime(startedAt)
		t.EndedAt = parseNullTime(endedAt)
		if tagsJSON.Valid && tagsJSON.String != "" {
			_ = json.Unmarshal([]byte(tagsJSON.String), &t.Tags)
		}
		if t.GuardJSON != "" {
			var g GuardRecord
			if err := json.Unmarshal([]byte(t.GuardJSON), &g); err == nil {
				t.Violations, t.Restored = g.Violations, g.Restored
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskRun loads one attempt by run, task and sample.
func (s *Store) TaskRun(ctx context.Context, runID, taskID string, sampleIndex int) (*TaskRunRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskRunColumns+` FROM task_runs WHERE run_id=? AND task_id=? AND sample_index=?`,
		runID, taskID, sampleIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got, err := scanTaskRuns(rows)
	if err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("%w: task %s sample %d in run %s", ErrNotFound, taskID, sampleIndex, runID)
	}
	return &got[0], nil
}

// GraderResults returns the flattened grader verdicts for a run.
func (s *Store) GraderResults(ctx context.Context, runID string) ([]GraderResultRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, task_run_id, task_id, sample_index, seq, grader, type,
		       passed, score, weight, required, reason, error, duration_ms, details_json
		FROM grader_results WHERE run_id=? ORDER BY task_id, sample_index, seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GraderResultRecord
	for rows.Next() {
		var g GraderResultRecord
		var passed, required int
		if err := rows.Scan(&g.RunID, &g.TaskRunID, &g.TaskID, &g.SampleIndex, &g.Seq, &g.Grader, &g.Type,
			&passed, &g.Score, &g.Weight, &required, &g.Reason, &g.Error, &g.DurationMs, &g.DetailsJSON); err != nil {
			return nil, err
		}
		g.Passed = passed == 1
		g.Required = required == 1
		out = append(out, g)
	}
	return out, rows.Err()
}

// ToolCalls returns the flattened tool invocations for one attempt.
func (s *Store) ToolCalls(ctx context.Context, taskRunID string) ([]ToolCallRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, task_run_id, task_id, sample_index, seq, tool_use_id,
		       name, is_error, duration_ms, input_json, output
		FROM tool_calls WHERE task_run_id=? ORDER BY seq`, taskRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolCallRecord
	for rows.Next() {
		var c ToolCallRecord
		var isErr int
		if err := rows.Scan(&c.RunID, &c.TaskRunID, &c.TaskID, &c.SampleIndex, &c.Seq,
			&c.ToolUseID, &c.Name, &isErr, &c.DurationMs, &c.InputJSON, &c.Output); err != nil {
			return nil, err
		}
		c.IsError = isErr == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// Metrics returns a run's metric rows.
func (s *Store) Metrics(ctx context.Context, runID string) ([]MetricRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, name, value, unit, kind, label, extra_json
		FROM run_metrics WHERE run_id=? ORDER BY name`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MetricRecord
	for rows.Next() {
		var m MetricRecord
		if err := rows.Scan(&m.RunID, &m.Name, &m.Value, &m.Unit, &m.Kind, &m.Label, &m.ExtraJSON); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CompletedKeys returns "taskID#sampleIndex" keys already recorded as completed,
// which is what makes a re-run resume instead of repeating paid work.
//
// Only fully graded attempts count. A failed or skipped row is intentionally left
// out so that re-running retries it.
func (s *Store) CompletedKeys(ctx context.Context, runID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, sample_index FROM task_runs WHERE run_id=? AND status=?`,
		runID, TaskStatusCompleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var taskID string
		var idx int
		if err := rows.Scan(&taskID, &idx); err != nil {
			return nil, err
		}
		out[TaskKey(taskID, idx)] = true
	}
	return out, rows.Err()
}

// TaskKey builds the resume key for a task sample.
func TaskKey(taskID string, sampleIndex int) string {
	return fmt.Sprintf("%s#%d", taskID, sampleIndex)
}

// Stats reports row counts, useful for a quick health check.
func (s *Store) Stats(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, table := range []string{"runs", "task_runs", "grader_results", "tool_calls", "run_metrics"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

func requireAffected(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

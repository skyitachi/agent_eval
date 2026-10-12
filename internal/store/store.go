// Package store persists evaluation runs to SQLite.
//
// # Why SQLite
//
// One file that can be queried, copied and opened read-only by the reporting
// layer. No server, no migration tooling, nothing to install -- which matters
// because a harness is most useful when it runs anywhere, including a container
// with no network.
//
// # The invariant this package exists to protect
//
// `aeval report` must be a function of this file. Two weeks from now, on another
// machine, rendering a copied `.db` must produce the document the run produced.
// That is why the baseline comparison is stored as resolved numbers rather than
// as a path to re-read, and why nothing the report needs is left in the
// workspace. Inputs live in the dataset; results live here.
//
// # Concurrency
//
// SQLite allows one writer at a time. The runner grades cells concurrently, so
// writes are serialised behind a mutex and the connection runs in WAL mode with
// a busy timeout for any other process touching the same file. Reads are not
// restricted.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required

	"github.com/skyitachi/agenteval/internal/grader"
)

//go:embed schema.sql
var schemaFS embed.FS

// schemaVersion is bumped whenever schema.sql changes.
//
// A database written by a *newer* build is refused with a clear message rather
// than read and failing later on an unexpected column; a database written by an
// *older* build is upgraded in place, because the alternative is discarding
// runs someone already paid for. `migrate` is the one place that decides which
// of the two is happening.
//
// v1 is the minimal rewrite: runs + attempts, with the factor labels a sweep is
// attributed by. It is deliberately not compatible with the earlier prototype
// schema (which carried RL reward columns and a third table); those databases
// are detected and refused rather than migrated, because the two schemas answer
// different questions.
//
// v2 adds attempts.baseline_output: the recorded session's final answer. A v1
// database upgrades with a single ALTER TABLE and reads back with that column
// empty, which is exactly true of it -- the column did not exist when those
// rows were written, so the recording's answer was never kept.
const schemaVersion = 2

// ErrNotFound is matched by every missing-record error. It is a sentinel, not a
// message: the errors that match it carry their own text, which names the record
// and the file it was looked for in. That file is the single piece of context a
// reader needs -- "not found" on its own reads like a bug in the tool, while
// "not found in aeval.db" reads like a wrong --db, which is what it almost
// always is.
var ErrNotFound = errors.New("store: record not found")

// NotFoundError reports a missing record, naming where it was looked for.
//
// It satisfies `errors.Is(err, ErrNotFound)` so callers keep a single sentinel
// to test against, and it can point at the database: a run looked up in the
// wrong file is a different problem from a run that was never recorded.
type NotFoundError struct {
	Kind string // "run"
	ID   string
	Path string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("store: %s %s not found in %s", e.Kind, e.ID, e.Path)
}

// Is makes errors.Is(err, ErrNotFound) true for every NotFoundError.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// SchemaVersion is the revision this build writes. Exported so `aeval version`
// reports the real number instead of a constant that drifts.
func SchemaVersion() int { return schemaVersion }

// Run statuses.
const (
	// RunRunning is a run that has been created and not yet finished.
	RunRunning = "running"
	// RunCompleted is a run that finished, whether or not tasks passed.
	RunCompleted = "completed"
	// RunFailed is a run that could not be carried out.
	RunFailed = "failed"
	// RunInterrupted is a run stopped by a signal.
	RunInterrupted = "interrupted"
)

// Attempt statuses.
//
// The distinction is the one the whole harness turns on: a timeout is a datum
// about the agent (it did not finish inside the budget), whereas an error is the
// harness failing to measure at all. Averaging them together is what makes a
// broken harness look like a bad agent.
const (
	// CellOK means a verdict was reached.
	CellOK = "ok"
	// CellTimeout means the attempt was cut off by its own budget.
	CellTimeout = "timeout"
	// CellError means the attempt could not be measured.
	CellError = "error"
)

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// Run is one invocation of `aeval run`.
type Run struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs int64      `json:"duration_ms"`

	DatasetPath string `json:"dataset_path"`
	DatasetName string `json:"dataset_name"`
	DatasetHash string `json:"dataset_hash"`

	AgentKind  string `json:"agent_kind"`
	AgentModel string `json:"agent_model"`
	AgentBin   string `json:"agent_bin"`
	Backend    string `json:"backend"`

	Samples     int `json:"samples"`
	Concurrency int `json:"concurrency"`

	// FactorAxis and FactorLevel are the sweep coordinates. Empty on a plain
	// run, which is a different thing from a run whose axis happens to be the
	// empty string: only a run with a non-empty axis can be attributed.
	FactorAxis  string `json:"factor_axis,omitempty"`
	FactorLevel string `json:"factor_level,omitempty"`
	SweepID     string `json:"sweep_id,omitempty"`

	Notes       string `json:"notes,omitempty"`
	ToolVersion string `json:"tool_version,omitempty"`
	ConfigJSON  string `json:"config_json,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Attempt is one (task, sample) cell.
type Attempt struct {
	// RowID is the database row; 0 before it is saved.
	RowID int64 `json:"row_id"`

	RunID    string   `json:"run_id"`
	TaskID   string   `json:"task_id"`
	Sample   int      `json:"sample_index"`
	Category string   `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`

	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	// Graded is false when the task had no grader: the attempt was measured but
	// there was nothing to score it against.
	Graded    bool    `json:"graded"`
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`

	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`

	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheReadTokens   int `json:"cache_read_tokens"`
	CacheCreateTokens int `json:"cache_create_tokens"`

	Turns      int `json:"turns"`
	ToolCalls  int `json:"tool_calls"`
	ToolErrors int `json:"tool_errors"`

	Diff   string          `json:"diff,omitempty"`
	Answer string          `json:"answer,omitempty"`
	Grades []grader.Result `json:"grades,omitempty"`
	Trace  json.RawMessage `json:"trace,omitempty"`

	// Baseline is the comparison resolved at run time.
	Baseline BaselineComparison `json:"baseline"`

	Error        string `json:"error,omitempty"`
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

// BaselineComparison is what the attempt was measured against.
type BaselineComparison struct {
	Present bool `json:"present"`
	// ID and Ref identify the recorded attempt (session id, file path).
	ID  string `json:"id,omitempty"`
	Ref string `json:"ref,omitempty"`
	// Tokens and Ms are the baseline's busy time and total tokens.
	Tokens int     `json:"tokens"`
	Ms     int64   `json:"ms"`
	Cost   float64 `json:"cost_usd"`
	Turns  int     `json:"turns"`
	// Output is the recorded session's final answer, kept so the report can set
	// the two conclusions side by side. Empty is ordinary rather than broken:
	// a recording may have no final assistant text, and every row written
	// before schema v2 has none by construction.
	Output string `json:"output,omitempty"`
}

// TokenTotal is the attempt's total token spend, billed and cached alike.
func (a Attempt) TokenTotal() int {
	return a.InputTokens + a.OutputTokens + a.CacheReadTokens + a.CacheCreateTokens
}

// ComparableBaseline returns the baseline this cell can honestly be compared
// against, and false when there is none to speak of.
//
// A cell the harness failed to measure is excluded on purpose: it spent nothing
// because it never ran, and folding that into "cheaper than the recording"
// would let a broken harness look efficient -- the exact confusion the
// CellError/CellTimeout split exists to prevent. A timeout does count, because
// the agent really did spend that budget and still did not finish, which is
// what a reader wants to see beside the recorded attempt.
//
// The rule lives here rather than at each call site because two aggregates --
// the run summary and the per-task report row -- both need it, and a rule
// duplicated in two places is a rule that eventually disagrees with itself.
func (a Attempt) ComparableBaseline() (BaselineComparison, bool) {
	if !a.Baseline.Present || a.Status == CellError {
		return BaselineComparison{}, false
	}
	return a.Baseline, true
}

// TokenDelta is the token difference against a comparable baseline. ok is false
// when there is nothing honest to subtract.
//
// A baseline that records no token total reports zero, and zero here is the
// absence of a measurement rather than a measurement of zero: subtracting it
// would credit this attempt with saving every token the recording ever spent.
// A recording keeps what it knows and omits the rest, so the reader has to be
// told which of the two it is holding.
func (a Attempt) TokenDelta() (int, bool) {
	b, ok := a.ComparableBaseline()
	if !ok || b.Tokens <= 0 {
		return 0, false
	}
	return a.TokenTotal() - b.Tokens, true
}

// MsDelta is the busy-time difference against a comparable baseline. ok is
// false when there is nothing honest to subtract, for the same reason as
// TokenDelta, and it bites harder here.
//
// A recording whose busy time was never captured reports Ms == 0, and an
// unattended attempt always spends some wall clock, so subtracting that zero
// presents the whole of this attempt's duration as a regression against a
// baseline that never claimed to be instantaneous. That is exactly the shape
// of the first dataset collected before cost-state records were read: it
// printed "+23.4s" against a baseline that had simply not been measured.
func (a Attempt) MsDelta() (int64, bool) {
	b, ok := a.ComparableBaseline()
	if !ok || b.Ms <= 0 {
		return 0, false
	}
	return a.DurationMs - b.Ms, true
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store is an open database handle.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex // serialises writes; SQLite permits one writer at a time
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One writer at a time is SQLite's rule, not ours; a single connection
	// avoids the lock contention that would otherwise show up as flaky
	// "database is locked" errors under concurrency.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path is the file backing the store.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	found, err := s.existingVersion()
	if err != nil {
		return err
	}
	// A database from a later build is refused rather than read: its columns
	// mean things this build does not know about, and guessing at them is how a
	// reporting bug becomes an afternoon of confusion.
	//
	// Anything at or below this build's revision is brought forward, on every
	// open -- including `aeval report`. That is deliberate: a database that
	// needs a separate migration command before it can be looked at is a
	// database that stops working the moment someone looks at it, and the
	// upgrade is what keeps "report is a function of the file" true across
	// revisions rather than only within one.
	if found > schemaVersion {
		return fmt.Errorf(
			"store: %s has schema v%d but this build writes v%d; "+
				"point --db at a fresh file (the two schemas answer different questions, "+
				"so this is a refusal, not a migration)", s.path, found, schemaVersion)
	}
	if found == 0 {
		if legacy, err := s.hasForeignTables(); err != nil {
			return err
		} else if legacy {
			return fmt.Errorf(
				"store: %s holds tables from an earlier prototype but no schema version; "+
					"point --db at a fresh file", s.path)
		}
	}

	body, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("store: embedded schema: %w", err)
	}
	// Apply before upgrading: on a fresh database this creates the tables and
	// there is no history behind them, while on an older one CREATE TABLE IF
	// NOT EXISTS is a no-op and the steps below bring the existing tables
	// forward. Doing it the other way round would have a step run against a
	// table that does not exist yet.
	if _, err := s.db.Exec(string(body)); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	if err := s.upgrade(found); err != nil {
		return err
	}
	// Overwrite rather than insert-if-absent: an upgraded database must end up
	// recording the revision it now has, or the next open would replay the
	// migration.
	_, err = s.db.Exec(
		`INSERT INTO schema_meta(key, value) VALUES('version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(schemaVersion))
	if err != nil {
		return fmt.Errorf("store: record schema version: %w", err)
	}
	return nil
}

// migrations turns a database forward one schema revision at a time.
//
// Index i is the step from v(i+1) to v(i+2), so the slice is the schema's
// history in order and a new revision appends to it rather than editing it.
// Each step has to be safe to run twice: a process killed between a step's DDL
// and the version bump leaves a database that will be handed the same step on
// the next open.
var migrations = []func(*Store) error{
	upgradeV1ToV2,
}

// upgrade walks from a database's revision to this build's.
func (s *Store) upgrade(from int) error {
	if from == 0 {
		// A fresh database was just created at the current revision; there is
		// no history behind it to replay.
		return nil
	}
	for v := from; v < schemaVersion; v++ {
		if v < 1 || v > len(migrations) {
			return fmt.Errorf("store: %s has schema v%d and this build has no path from it", s.path, v)
		}
		if err := migrations[v-1](s); err != nil {
			return fmt.Errorf("store: upgrade %s from v%d to v%d: %w", s.path, v, v+1, err)
		}
	}
	return nil
}

// upgradeV1ToV2 adds the recorded answer to a database that predates it.
func upgradeV1ToV2(s *Store) error {
	return s.addColumnIfMissing("attempts", "baseline_output", "TEXT NOT NULL DEFAULT ''")
}

// addColumnIfMissing is why the steps above can be re-run: ADD COLUMN is not
// idempotent, and a step interrupted before the version bump would otherwise
// fail the next open with "duplicate column name" -- a message that names
// neither the migration nor the way out.
func (s *Store) addColumnIfMissing(table, column, decl string) error {
	// The table name is a compile-time constant from this package, not user
	// input, so interpolating it is safe; binding it is not possible inside a
	// PRAGMA. The column name is checked against the result rather than
	// interpolated at all.
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("store: inspect %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: inspect %s: %w", table, err)
	}
	if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl); err != nil {
		return fmt.Errorf("store: add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) existingVersion() (int, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM schema_meta WHERE key='version'`).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		// The table itself may not exist yet, which is the fresh-database case.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return 0, fmt.Errorf("store: unreadable schema version %q", raw)
	}
	return v, nil
}

func (s *Store) hasForeignTables() (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type='table' AND name NOT IN ('schema_meta','runs','attempts','sqlite_sequence')`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: inspect tables: %w", err)
	}
	return n > 0, nil
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

const runColumns = `id, name, status, started_at, ended_at, duration_ms,
	dataset_path, dataset_name, dataset_hash,
	agent_kind, agent_model, agent_bin, backend,
	samples, concurrency, factor_axis, factor_level, sweep_id,
	notes, tool_version, config_json, error`

// CreateRun inserts a run. The caller sets StartedAt; a run with no status is
// recorded as running.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("store: run id is required")
	}
	if r.Status == "" {
		r.Status = RunRunning
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	args := []any{
		r.ID, r.Name, r.Status, fmtTime(r.StartedAt), nullTime(r.EndedAt), r.DurationMs,
		r.DatasetPath, r.DatasetName, r.DatasetHash,
		r.AgentKind, r.AgentModel, r.AgentBin, r.Backend,
		r.Samples, r.Concurrency, r.FactorAxis, r.FactorLevel, r.SweepID,
		r.Notes, r.ToolVersion, r.ConfigJSON, r.Error,
	}
	if err := checkArity("runs", runColumns, args); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (`+runColumns+`) VALUES (`+placeholders(len(args))+`)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, status=excluded.status, started_at=excluded.started_at,
			ended_at=excluded.ended_at, duration_ms=excluded.duration_ms,
			dataset_path=excluded.dataset_path, dataset_name=excluded.dataset_name,
			dataset_hash=excluded.dataset_hash,
			agent_kind=excluded.agent_kind, agent_model=excluded.agent_model,
			agent_bin=excluded.agent_bin, backend=excluded.backend,
			samples=excluded.samples, concurrency=excluded.concurrency,
			factor_axis=excluded.factor_axis, factor_level=excluded.factor_level,
			sweep_id=excluded.sweep_id,
			notes=excluded.notes, tool_version=excluded.tool_version,
			config_json=excluded.config_json, error=excluded.error`,
		args...)
	if err != nil {
		return fmt.Errorf("store: create run: %w", err)
	}
	return nil
}

// FinishRun records the outcome of a run.
func (s *Store) FinishRun(ctx context.Context, id, status string, errMsg string) error {
	if status == "" {
		status = RunCompleted
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs
		   SET status=?, ended_at=?, error=?,
		       duration_ms = CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
		 WHERE id=?`, status, fmtTime(now), errMsg, fmtTime(now), id)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Kind: "run", ID: id, Path: s.path}
	}
	return nil
}

// GetRun loads one run.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &NotFoundError{Kind: "run", ID: id, Path: s.path}
	}
	return r, err
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	// SweepID selects the runs of one sweep.
	SweepID string
	// FactorAxis selects runs that varied a particular axis.
	FactorAxis string
	// Limit caps the result (0 = no cap).
	Limit int
}

// ListRuns returns runs, newest first.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	q := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	var args []any
	if f.SweepID != "" {
		q += ` AND sweep_id=?`
		args = append(args, f.SweepID)
	}
	if f.FactorAxis != "" {
		q += ` AND factor_axis=?`
		args = append(args, f.FactorAxis)
	}
	q += ` ORDER BY started_at DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRun removes a run and its attempts.
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Kind: "run", ID: id, Path: s.path}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Attempts
// ---------------------------------------------------------------------------

const attemptColumns = `id, run_id, task_id, sample_index, category, tags_json,
	status, stop_reason, graded, passed, score, threshold,
	duration_ms, cost_usd,
	input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
	turns, tool_calls, tool_errors,
	diff, answer, grades_json, trace_json,
	has_baseline, baseline_id, baseline_ref, baseline_tokens, baseline_ms,
	baseline_cost, baseline_turns, baseline_output,
	error, workspace_dir`

// SaveAttempt inserts or replaces one cell.
func (s *Store) SaveAttempt(ctx context.Context, a Attempt) error {
	if a.RunID == "" || a.TaskID == "" {
		return fmt.Errorf("store: attempt needs a run id and a task id")
	}
	tags, err := json.Marshal(nonNil(a.Tags))
	if err != nil {
		return fmt.Errorf("store: encode tags: %w", err)
	}
	grades, err := json.Marshal(nonNilResults(a.Grades))
	if err != nil {
		return fmt.Errorf("store: encode grades: %w", err)
	}
	trace := "null"
	if len(a.Trace) > 0 {
		if !json.Valid(a.Trace) {
			return fmt.Errorf("store: attempt %s: trace is not valid JSON", a.TaskID)
		}
		trace = string(a.Trace)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	args := []any{
		nil, a.RunID, a.TaskID, a.Sample, a.Category, string(tags),
		a.Status, a.StopReason, boolInt(a.Graded), boolInt(a.Passed), a.Score, a.Threshold,
		a.DurationMs, a.CostUSD,
		a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheCreateTokens,
		a.Turns, a.ToolCalls, a.ToolErrors,
		a.Diff, a.Answer, string(grades), trace,
		boolInt(a.Baseline.Present), a.Baseline.ID, a.Baseline.Ref,
		a.Baseline.Tokens, a.Baseline.Ms, a.Baseline.Cost, a.Baseline.Turns,
		a.Baseline.Output,
		a.Error, a.WorkspaceDir,
	}
	if err := checkArity("attempts", attemptColumns, args); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO attempts (`+attemptColumns+`) VALUES (`+placeholders(len(args))+`)
		ON CONFLICT(run_id, task_id, sample_index) DO UPDATE SET
			category=excluded.category, tags_json=excluded.tags_json,
			status=excluded.status, stop_reason=excluded.stop_reason,
			graded=excluded.graded, passed=excluded.passed,
			score=excluded.score, threshold=excluded.threshold,
			duration_ms=excluded.duration_ms, cost_usd=excluded.cost_usd,
			input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_read_tokens=excluded.cache_read_tokens,
			cache_create_tokens=excluded.cache_create_tokens,
			turns=excluded.turns, tool_calls=excluded.tool_calls,
			tool_errors=excluded.tool_errors,
			diff=excluded.diff, answer=excluded.answer,
			grades_json=excluded.grades_json, trace_json=excluded.trace_json,
			has_baseline=excluded.has_baseline, baseline_id=excluded.baseline_id,
			baseline_ref=excluded.baseline_ref, baseline_tokens=excluded.baseline_tokens,
			baseline_ms=excluded.baseline_ms, baseline_cost=excluded.baseline_cost,
			baseline_turns=excluded.baseline_turns,
			baseline_output=excluded.baseline_output,
			error=excluded.error, workspace_dir=excluded.workspace_dir`,
		args...)
	if err != nil {
		return fmt.Errorf("store: save attempt %s#%d: %w", a.TaskID, a.Sample, err)
	}
	return nil
}

// checkArity turns an off-by-one between a column list and its values into a
// named error instead of a SQL message that says only "N values for M columns"
// and leaves the reader to count by hand. It has happened here before.
func checkArity(table, columns string, args []any) error {
	n := len(strings.Split(columns, ","))
	if n != len(args) {
		return fmt.Errorf("store: internal error: %s insert has %d values for %d columns", table, len(args), n)
	}
	return nil
}

// ListAttempts returns the cells of a run, ordered by task then sample.
func (s *Store) ListAttempts(ctx context.Context, runID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE run_id=? ORDER BY task_id, sample_index`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var started string
	var ended sql.NullString
	err := sc.Scan(&r.ID, &r.Name, &r.Status, &started, &ended, &r.DurationMs,
		&r.DatasetPath, &r.DatasetName, &r.DatasetHash,
		&r.AgentKind, &r.AgentModel, &r.AgentBin, &r.Backend,
		&r.Samples, &r.Concurrency, &r.FactorAxis, &r.FactorLevel, &r.SweepID,
		&r.Notes, &r.ToolVersion, &r.ConfigJSON, &r.Error)
	if err != nil {
		return Run{}, err
	}
	r.StartedAt = parseTime(started)
	if ended.Valid {
		t := parseTime(ended.String)
		r.EndedAt = &t
	}
	return r, nil
}

func scanAttempt(sc scanner) (Attempt, error) {
	var a Attempt
	var tagsJSON, gradesJSON, traceJSON string
	var graded, passed, hasBaseline int
	err := sc.Scan(&a.RowID, &a.RunID, &a.TaskID, &a.Sample, &a.Category, &tagsJSON,
		&a.Status, &a.StopReason, &graded, &passed, &a.Score, &a.Threshold,
		&a.DurationMs, &a.CostUSD,
		&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheCreateTokens,
		&a.Turns, &a.ToolCalls, &a.ToolErrors,
		&a.Diff, &a.Answer, &gradesJSON, &traceJSON,
		&hasBaseline, &a.Baseline.ID, &a.Baseline.Ref,
		&a.Baseline.Tokens, &a.Baseline.Ms, &a.Baseline.Cost, &a.Baseline.Turns,
		&a.Baseline.Output,
		&a.Error, &a.WorkspaceDir)
	if err != nil {
		return Attempt{}, err
	}
	a.Graded, a.Passed = graded != 0, passed != 0
	a.Baseline.Present = hasBaseline != 0
	// An empty column decodes to nothing rather than to an error: the schema's
	// own defaults are the empty string, so a row that came from anywhere but
	// SaveAttempt -- a hand-edited database, a migration -- is readable instead
	// of taking the whole listing down with a JSON complaint.
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &a.Tags); err != nil {
			return Attempt{}, fmt.Errorf("store: decode tags: %w", err)
		}
	}
	if gradesJSON != "" {
		if err := json.Unmarshal([]byte(gradesJSON), &a.Grades); err != nil {
			return Attempt{}, fmt.Errorf("store: decode grades: %w", err)
		}
	}
	if traceJSON != "" && traceJSON != "null" {
		a.Trace = json.RawMessage(traceJSON)
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilResults(r []grader.Result) []grader.Result {
	if r == nil {
		return []grader.Result{}
	}
	return r
}

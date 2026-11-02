package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// The life of one grade request: queued until a worker picks it up, running
// while the grader runs, then done or failed. Nothing goes back to queued --
// a request that did not finish is a failure to report, not a retry to make
// silently, because the grader may already have spent a paid call.
const (
	GradeQueued  = "queued"
	GradeRunning = "running"
	GradeDone    = "done"
	GradeFailed  = "failed"
)

// GradeJob is one re-grade that was asked for, with what it cost.
//
// The verdict it reached lives on the attempt; this row says what the *request*
// cost and whether it is still going. A cell can be graded many times, so the
// attempt cannot hold the history and is left alone.
type GradeJob struct {
	ID       string
	ReportID string
	RunID    string
	TaskID   string
	Sample   int
	// Grader is the value the picker sent: auto | llm_judge | patch. What was
	// asked for, which is not necessarily what ran -- auto resolves to the
	// dataset's own graders.
	Grader    string
	Status    string
	CreatedAt time.Time
	StartedAt *time.Time
	EndedAt   *time.Time

	// What reaching the verdict cost. Zero when no model was called.
	Model        string
	InputTokens  int
	OutputTokens int
	LatencyMs    int64

	// The verdict this job produced, repeated so the row says what it did even
	// after the cell has been graded again.
	Score   float64
	Passed  bool
	Details string

	Error string
}

// Active reports whether the job is still going. A page shows a running state
// for these and a finished state for the others, and the difference is the
// whole reason the status exists.
func (j GradeJob) Active() bool {
	return j.Status == GradeQueued || j.Status == GradeRunning
}

// GradeRequest is what a caller knows before the work starts.
type GradeRequest struct {
	ReportID string
	RunID    string
	TaskID   string
	Sample   int
	Grader   string
}

// GradeOutcome is what a finished job cost and reached.
type GradeOutcome struct {
	Score        float64
	Passed       bool
	Model        string
	InputTokens  int
	OutputTokens int
	LatencyMs    int64
	Details      string
}

// CreateGradeJob records a request as queued and returns the row, so the caller
// can hand its id to the page before any work starts.
func (s *Store) CreateGradeJob(ctx context.Context, req GradeRequest) (*GradeJob, error) {
	if req.RunID == "" || req.TaskID == "" {
		return nil, fmt.Errorf("store: a grade request needs a run id and a task id")
	}
	id, err := newGradeJobID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	j := &GradeJob{
		ID:        id,
		ReportID:  req.ReportID,
		RunID:     req.RunID,
		TaskID:    req.TaskID,
		Sample:    req.Sample,
		Grader:    req.Grader,
		Status:    GradeQueued,
		CreatedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO grade_jobs (id, report_id, run_id, task_id, sample_index, grader, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.ReportID, j.RunID, j.TaskID, j.Sample, j.Grader, j.Status, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("store: record grade request: %w", err)
	}
	return j, nil
}

// StartGradeJob marks a queued job as running.
//
// The WHERE clause keeps a job that was abandoned while it sat in the queue --
// by a shutdown, or by another process -- from being resurrected into "running"
// by a worker that got to it late.
func (s *Store) StartGradeJob(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE grade_jobs SET status=?, started_at=? WHERE id=? AND status=?`,
		GradeRunning, fmtTime(time.Now()), id, GradeQueued)
	if err != nil {
		return fmt.Errorf("store: start grade job %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: start grade job %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: grade job %s is no longer queued (it was abandoned or already started)", id)
	}
	return nil
}

// FinishGradeJob records what a finished job reached and cost.
func (s *Store) FinishGradeJob(ctx context.Context, id string, out GradeOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE grade_jobs SET status=?, ended_at=?, score=?, passed=?, model=?,
			input_tokens=?, output_tokens=?, latency_ms=?, details_json=?
		WHERE id=?`,
		GradeDone, fmtTime(time.Now()), out.Score, boolInt(out.Passed), out.Model,
		out.InputTokens, out.OutputTokens, out.LatencyMs, out.Details, id)
	if err != nil {
		return fmt.Errorf("store: finish grade job %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Kind: "grade job", ID: id, Path: s.path}
	}
	return nil
}

// FailGradeJob records why a job reached no verdict.
//
// The attempt is untouched by definition: everything that can fail -- a grader
// that needs a workspace, a judge with no credentials, a model that answered
// nothing -- fails before the verdict is written, so the cell keeps the one it
// had.
//
// Only a live job is failed: a shutdown racing a job that just finished must
// not overwrite a done row with "the server stopped". Zero rows touched is
// therefore not an error -- it means somebody else got there first, and the row
// already says something true.
func (s *Store) FailGradeJob(ctx context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE grade_jobs SET status=?, ended_at=?, error=? WHERE id=? AND status IN (?, ?)`,
		GradeFailed, fmtTime(time.Now()), reason, id, GradeQueued, GradeRunning)
	if err != nil {
		return fmt.Errorf("store: fail grade job %s: %w", id, err)
	}
	return nil
}

// GetGradeJob reads one job by id.
func (s *Store) GetGradeJob(ctx context.Context, id string) (*GradeJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+gradeJobColumns+` FROM grade_jobs WHERE id=?`, id)
	j, err := scanGradeJob(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, &NotFoundError{Kind: "grade job", ID: id, Path: s.path}
		}
		return nil, fmt.Errorf("store: read grade job %s: %w", id, err)
	}
	return &j, nil
}

// LatestGradeJob returns the most recent job for a cell, or nil when it was
// never graded from a page or a shell.
//
// "Latest" rather than "last finished": a running job is the answer to "what is
// happening to this cell", and it is the newest row by construction.
func (s *Store) LatestGradeJob(ctx context.Context, runID, taskID string, sample int) (*GradeJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+gradeJobColumns+` FROM grade_jobs
		 WHERE run_id=? AND task_id=? AND sample_index=?
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`, runID, taskID, sample)
	j, err := scanGradeJob(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read latest grade job for %s#%d: %w", taskID, sample, err)
	}
	return &j, nil
}

// ActiveGradeJob returns the job still running for a cell, or nil.
//
// It is what stops a second press from paying for the same judgement twice: the
// answer is not "refused", it is "the one you asked for is still going".
func (s *Store) ActiveGradeJob(ctx context.Context, runID, taskID string, sample int) (*GradeJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+gradeJobColumns+` FROM grade_jobs
		 WHERE run_id=? AND task_id=? AND sample_index=? AND status IN (?, ?)
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		runID, taskID, sample, GradeQueued, GradeRunning)
	j, err := scanGradeJob(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read active grade job for %s#%d: %w", taskID, sample, err)
	}
	return &j, nil
}

// ListGradeJobs returns a run's grade history, newest first.
func (s *Store) ListGradeJobs(ctx context.Context, runID string, limit int) ([]GradeJob, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+gradeJobColumns+` FROM grade_jobs WHERE run_id=?
		 ORDER BY created_at DESC, rowid DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list grade jobs: %w", err)
	}
	defer rows.Close()
	var out []GradeJob
	for rows.Next() {
		j, err := scanGradeJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AbandonGradeJobs closes every job still queued or running, and returns how
// many it closed.
//
// It is for a process that is starting up. The work is a goroutine of the
// process that accepted the request, so a row in either state when a new
// process opens this database belonged to one that is gone: the verdict was
// never written and never will be. Left alone it would show "grading" forever,
// which is the one reading that is certainly false.
func (s *Store) AbandonGradeJobs(ctx context.Context, reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE grade_jobs SET status=?, ended_at=?, error=? WHERE status IN (?, ?)`,
		GradeFailed, fmtTime(time.Now()), reason, GradeQueued, GradeRunning)
	if err != nil {
		return 0, fmt.Errorf("store: abandon grade jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

const gradeJobColumns = `id, report_id, run_id, task_id, sample_index, grader, status,
	created_at, started_at, ended_at, model, input_tokens, output_tokens, latency_ms,
	score, passed, details_json, error`

func scanGradeJob(sc scanner) (GradeJob, error) {
	var (
		j              GradeJob
		created        string
		started, ended sql.NullString
		passed         int
	)
	err := sc.Scan(&j.ID, &j.ReportID, &j.RunID, &j.TaskID, &j.Sample, &j.Grader, &j.Status,
		&created, &started, &ended, &j.Model, &j.InputTokens, &j.OutputTokens, &j.LatencyMs,
		&j.Score, &passed, &j.Details, &j.Error)
	if err != nil {
		return GradeJob{}, err
	}
	j.CreatedAt = parseTime(created)
	if started.Valid {
		t := parseTime(started.String)
		j.StartedAt = &t
	}
	if ended.Valid {
		t := parseTime(ended.String)
		j.EndedAt = &t
	}
	j.Passed = passed != 0
	return j, nil
}

// newGradeJobID mirrors the report registry's ids: eight random bytes, so two
// presses in the same nanosecond cannot collide and an id carries no meaning
// that a reader might take for ordering.
func newGradeJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: grade job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

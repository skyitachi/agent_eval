package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Replay statuses. A replay moves forward through them and never back.
const (
	ReplayQueued    = "queued"
	ReplayRunning   = "running"
	ReplayCompleted = "completed"
	ReplayFailed    = "failed"
)

// Replay is one replay started from the web page: a recorded session, the
// choices it was replayed under, and what it produced.
type Replay struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	SessionID    string `json:"session_id"`
	SessionTitle string `json:"session_title"`

	// Harness, Skill and Model are what the person picked, as they read it.
	// AgentSpecJSON is what those resolved to, which is what actually ran.
	Harness       string `json:"harness"`
	Skill         string `json:"skill"`
	Model         string `json:"model"`
	AgentSpecJSON string `json:"agent_spec_json"`

	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	DatasetDir string `json:"dataset_dir"`
	RunID      string `json:"run_id"`
	ReportID   string `json:"report_id"`
	Error      string `json:"error"`
	Log        string `json:"log"`
}

// Finished reports whether the replay has stopped, one way or the other.
func (r Replay) Finished() bool {
	return r.Status == ReplayCompleted || r.Status == ReplayFailed
}

const replayColumns = `id, source, session_id, session_title, harness, skill, model,
	agent_spec_json, status, created_at, started_at, ended_at,
	dataset_dir, run_id, report_id, error, log`

// CreateReplay records a replay that has been asked for.
func (s *Store) CreateReplay(ctx context.Context, r Replay) error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("store: replay has no id")
	}
	if r.Status == "" {
		r.Status = ReplayQueued
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO replays(`+replayColumns+`) VALUES(`+placeholders(17)+`)`,
		r.ID, r.Source, r.SessionID, r.SessionTitle, r.Harness, r.Skill, r.Model,
		r.AgentSpecJSON, r.Status, fmtTime(r.CreatedAt), nullTime(r.StartedAt), nullTime(r.EndedAt),
		r.DatasetDir, r.RunID, r.ReportID, r.Error, r.Log)
	if err != nil {
		return fmt.Errorf("store: create replay: %w", err)
	}
	return nil
}

// UpdateReplay writes every mutable field of a replay back.
//
// One write of the whole row rather than a setter per field: the job that owns
// a replay is the only writer, and it always knows the whole state it is in.
func (s *Store) UpdateReplay(ctx context.Context, r Replay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE replays SET status=?, started_at=?, ended_at=?, dataset_dir=?, run_id=?,
		 report_id=?, error=?, log=?, agent_spec_json=? WHERE id=?`,
		r.Status, nullTime(r.StartedAt), nullTime(r.EndedAt), r.DatasetDir, r.RunID,
		r.ReportID, r.Error, r.Log, r.AgentSpecJSON, r.ID)
	if err != nil {
		return fmt.Errorf("store: update replay %s: %w", r.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Kind: "replay", ID: r.ID, Path: s.path}
	}
	return nil
}

// GetReplay returns one replay.
func (s *Store) GetReplay(ctx context.Context, id string) (Replay, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+replayColumns+` FROM replays WHERE id=?`, id)
	r, err := scanReplay(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Replay{}, &NotFoundError{Kind: "replay", ID: id, Path: s.path}
	}
	return r, err
}

// ReplayFilter narrows ListReplays.
type ReplayFilter struct {
	// Source and SessionID select the replays of one recording.
	Source    string
	SessionID string
	// Limit caps the result (0 = no cap).
	Limit int
}

// ListReplays returns replays, newest first.
func (s *Store) ListReplays(ctx context.Context, f ReplayFilter) ([]Replay, error) {
	q := `SELECT ` + replayColumns + ` FROM replays WHERE 1=1`
	var args []any
	if f.Source != "" {
		q += ` AND source=?`
		args = append(args, f.Source)
	}
	if f.SessionID != "" {
		q += ` AND session_id=?`
		args = append(args, f.SessionID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list replays: %w", err)
	}
	defer rows.Close()
	var out []Replay
	for rows.Next() {
		r, err := scanReplay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AbandonReplays closes every replay still queued or running, and returns how
// many it closed.
//
// It is for a process that is starting up. The jobs are goroutines of the
// process that created them, so a row in either state when a new process starts
// belonged to one that is gone, and nothing will ever move it on. Left alone it
// would show a spinner forever.
func (s *Store) AbandonReplays(ctx context.Context, reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE replays SET status=?, ended_at=?, error=? WHERE status IN (?, ?)`,
		ReplayFailed, fmtTime(time.Now()), reason, ReplayQueued, ReplayRunning)
	if err != nil {
		return 0, fmt.Errorf("store: abandon replays: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanReplay(sc scanner) (Replay, error) {
	var (
		r              Replay
		created        string
		started, ended sql.NullString
	)
	err := sc.Scan(&r.ID, &r.Source, &r.SessionID, &r.SessionTitle, &r.Harness, &r.Skill, &r.Model,
		&r.AgentSpecJSON, &r.Status, &created, &started, &ended,
		&r.DatasetDir, &r.RunID, &r.ReportID, &r.Error, &r.Log)
	if err != nil {
		return Replay{}, err
	}
	r.CreatedAt = parseTime(created)
	if started.Valid {
		t := parseTime(started.String)
		r.StartedAt = &t
	}
	if ended.Valid {
		t := parseTime(ended.String)
		r.EndedAt = &t
	}
	return r, nil
}

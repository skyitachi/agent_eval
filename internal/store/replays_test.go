package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReplaysRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"r1", "r2", "r3"} {
		sid := "s1"
		if id == "r3" {
			sid = "s2"
		}
		if err := st.CreateReplay(ctx, Replay{
			ID: id, Source: "claude", SessionID: sid, SessionTitle: "fix it",
			Harness: "clean-room", Skill: "none", Model: "sonnet",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetReplay(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ReplayQueued || got.StartedAt != nil || got.Finished() {
		t.Errorf("a new replay = %+v, want queued and not started", got)
	}

	now := base.Add(time.Hour)
	got.Status, got.StartedAt, got.EndedAt = ReplayCompleted, &now, &now
	got.RunID, got.ReportID, got.Log = "run-1", "rep-1", "line\n"
	if err := st.UpdateReplay(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetReplay(ctx, "r1")
	if again.RunID != "run-1" || again.ReportID != "rep-1" || !again.Finished() ||
		again.EndedAt == nil || !again.EndedAt.Equal(now) || again.Log != "line\n" {
		t.Errorf("after update = %+v", again)
	}

	list, err := st.ListReplays(ctx, ReplayFilter{Source: "claude", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "r2" || list[1].ID != "r1" {
		t.Errorf("one session's replays = %+v, want r2 then r1", list)
	}

	if _, err := st.GetReplay(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing replay = %v, want ErrNotFound", err)
	}
	if err := st.UpdateReplay(ctx, Replay{ID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating a missing replay = %v, want ErrNotFound", err)
	}
}

func TestAbandonReplaysClosesOnlyWhatIsStillGoing(t *testing.T) {
	// A replay is a goroutine of the server that started it. One found queued
	// or running by a new process belonged to a process that is gone.
	st, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for id, status := range map[string]string{
		"q": ReplayQueued, "r": ReplayRunning, "c": ReplayCompleted, "f": ReplayFailed,
	} {
		if err := st.CreateReplay(ctx, Replay{ID: id, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.AbandonReplays(ctx, "the server stopped")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("abandoned %d, want the queued and the running one", n)
	}
	for id, want := range map[string]string{
		"q": ReplayFailed, "r": ReplayFailed, "c": ReplayCompleted, "f": ReplayFailed,
	} {
		r, _ := st.GetReplay(ctx, id)
		if r.Status != want {
			t.Errorf("%s: status = %s, want %s", id, r.Status, want)
		}
		if (id == "q" || id == "r") && (r.Error != "the server stopped" || r.EndedAt == nil) {
			t.Errorf("%s: an abandoned replay must say why and when: %+v", id, r)
		}
	}
}

// TestOpenUpgradesAV7Database covers the replays table, which v8 adds.
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
		t.Errorf("version = %d, want %d", v, schemaVersion)
	}
	ctx := context.Background()
	if runs, _ := s.ListRuns(ctx, RunFilter{}); len(runs) != 1 {
		t.Errorf("runs = %d, want the row that was already there", len(runs))
	}
	if err := s.CreateReplay(ctx, Replay{ID: "x"}); err != nil {
		t.Errorf("the upgraded database cannot record a replay: %v", err)
	}
}

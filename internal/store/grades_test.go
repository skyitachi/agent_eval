package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newGradeStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "grades.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The row exists to answer three questions a verdict cannot: what was asked
// for, is it still going, and what did it cost.
func TestAGradeJobRecordsWhatItCost(t *testing.T) {
	st := newGradeStore(t)
	ctx := context.Background()

	job, err := st.CreateGradeJob(ctx, GradeRequest{
		ReportID: "rep1", RunID: "r1", TaskID: "t1", Sample: 0, Grader: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != GradeQueued || !job.Active() || job.CreatedAt.IsZero() {
		t.Fatalf("a new request = %+v, want queued and stamped", job)
	}
	if len(job.ID) != 16 {
		t.Errorf("id %q is %d chars; ids are eight bytes of randomness in hex", job.ID, len(job.ID))
	}

	if err := st.StartGradeJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	running, err := st.GetGradeJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != GradeRunning || running.StartedAt == nil || !running.Active() {
		t.Errorf("after start = %+v, want running with a start time", running)
	}

	if err := st.FinishGradeJob(ctx, job.ID, GradeOutcome{
		Score: 0.75, Passed: true, Model: "deepseek-flash",
		InputTokens: 1018, OutputTokens: 245, LatencyMs: 23800,
		Details: `[{"grader":"judge"}]`,
	}); err != nil {
		t.Fatal(err)
	}
	done, err := st.GetGradeJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != GradeDone || done.Active() {
		t.Errorf("a finished job = %q, want done and not active", done.Status)
	}
	if done.Model != "deepseek-flash" || done.InputTokens != 1018 || done.OutputTokens != 245 {
		t.Errorf("spend = %s %d/%d, want the judge's numbers", done.Model, done.InputTokens, done.OutputTokens)
	}
	if !done.Passed || done.Score != 0.75 || done.LatencyMs != 23800 || done.EndedAt == nil {
		t.Errorf("verdict = %+v, want the one that was written", done)
	}

	// The page asks by cell, not by id: the id is on the URL, the cell is what
	// the table knows how to find.
	latest, err := st.LatestGradeJob(ctx, "r1", "t1", 0)
	if err != nil || latest == nil || latest.ID != job.ID {
		t.Fatalf("latest for the cell = %+v, %v", latest, err)
	}
	if active, err := st.ActiveGradeJob(ctx, "r1", "t1", 0); err != nil || active != nil {
		t.Errorf("a finished job is still reported active: %+v %v", active, err)
	}
}

// Two presses on the same cell must not become two paid judgements. The check a
// handler does first is a race; this is the constraint behind it.
func TestOneLiveGradePerCellIsEnforcedByTheDatabase(t *testing.T) {
	st := newGradeStore(t)
	ctx := context.Background()
	req := GradeRequest{ReportID: "rep1", RunID: "r1", TaskID: "t1", Sample: 0, Grader: "auto"}

	first, err := st.CreateGradeJob(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateGradeJob(ctx, req); err == nil {
		t.Fatal("a second live request for the same cell was accepted")
	} else if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("the refusal is %q, want the constraint's own words so the caller can tell a race from a bug", err)
	}

	// A different sample is a different cell, and a finished request frees the
	// cell for another attempt.
	other := req
	other.Sample = 1
	if _, err := st.CreateGradeJob(ctx, other); err != nil {
		t.Errorf("sample 1 was refused because sample 0 is running: %v", err)
	}
	if err := st.FailGradeJob(ctx, first.ID, "no credentials"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateGradeJob(ctx, req); err != nil {
		t.Errorf("the cell was not freed after its request failed: %v", err)
	}
}

// A row left queued or running belongs to a process that is gone. Closing it is
// the only reading that is not a lie, and it must not touch a finished job.
func TestAbandonClosesOnlyTheUnfinishedRequests(t *testing.T) {
	st := newGradeStore(t)
	ctx := context.Background()

	live, err := st.CreateGradeJob(ctx, GradeRequest{RunID: "r1", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := st.CreateGradeJob(ctx, GradeRequest{RunID: "r1", TaskID: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartGradeJob(ctx, finished.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishGradeJob(ctx, finished.ID, GradeOutcome{Score: 1, Passed: true}); err != nil {
		t.Fatal(err)
	}

	n, err := st.AbandonGradeJobs(ctx, "the server stopped")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("closed %d requests, want the one that was still queued", n)
	}
	closed, _ := st.GetGradeJob(ctx, live.ID)
	if closed.Status != GradeFailed || !strings.Contains(closed.Error, "server stopped") || closed.EndedAt == nil {
		t.Errorf("abandoned row = %+v, want failed with the reason", closed)
	}
	kept, _ := st.GetGradeJob(ctx, finished.ID)
	if kept.Status != GradeDone || kept.Error != "" {
		t.Errorf("a finished job was rewritten by the sweep: %+v", kept)
	}
}

// A shutdown racing a job that just finished must not turn a verdict into a
// failure. Zero rows touched is the correct outcome there, not an error.
func TestFailingAFinishedJobLeavesTheVerdictAlone(t *testing.T) {
	st := newGradeStore(t)
	ctx := context.Background()

	job, err := st.CreateGradeJob(ctx, GradeRequest{RunID: "r1", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartGradeJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishGradeJob(ctx, job.ID, GradeOutcome{Score: 1, Passed: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailGradeJob(ctx, job.ID, "the server stopped before this grade started"); err != nil {
		t.Fatalf("failing a finished job reported an error: %v", err)
	}
	after, _ := st.GetGradeJob(ctx, job.ID)
	if after.Status != GradeDone || after.Error != "" {
		t.Errorf("after a late failure = %+v, want the verdict it already had", after)
	}
}

// Start is guarded by status on purpose: a request abandoned while it sat in
// the queue must not be resurrected by a worker that got to it late.
func TestAWorkerCannotStartARequestThatWasAbandoned(t *testing.T) {
	st := newGradeStore(t)
	ctx := context.Background()

	job, err := st.CreateGradeJob(ctx, GradeRequest{RunID: "r1", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AbandonGradeJobs(ctx, "the server stopped"); err != nil {
		t.Fatal(err)
	}
	if err := st.StartGradeJob(ctx, job.ID); err == nil {
		t.Fatal("an abandoned request was started anyway; its verdict would be written on behalf of a failed row")
	}
}

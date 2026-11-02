package server

import (
	"context"
	"sync"
	"time"

	"github.com/skyitachi/agenteval/internal/regrade"
	"github.com/skyitachi/agenteval/internal/store"
)

// # Grading happens off the request
//
// A judge call took 24 seconds when this was measured, and a browser cannot
// tell "working" from "broken" for that long: the button appears to do nothing,
// the tab can be closed by a reader who concludes it failed, and the page it
// eventually lands on was built from a verdict that arrived after the reader
// stopped waiting.
//
// So a press records a request and returns. The work runs in this process, one
// slot per worker, and the row it wrote is what the page reads back: "queued",
// then "running", then done or failed, with the tokens the judgement cost. The
// page follows it by refreshing, which is how the session pages already follow a
// replay -- one mechanism for "this is still happening", not two.
//
// # Why a bound
//
// Grading can cost money. Two presses on ten cells must not become twenty
// simultaneous model calls, so work queues behind a small number of slots. The
// limit is per process and deliberately small: a reader grading a report is not
// a load test, and the queue is visible on the page while it waits.

// gradeWorkers is how many cells one server grades at a time. Two is enough to
// make a handful of presses feel immediate without turning a page with twenty
// ungraded rows into twenty simultaneous paid calls.
const gradeWorkers = 2

type gradeRunner struct {
	st    *store.Store
	slots chan struct{}
	// base is the parent of every job's context, so a shutdown cancels work
	// that is in flight instead of waiting for a twenty-second call to finish.
	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newGradeRunner(st *store.Store) *gradeRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &gradeRunner{
		st:     st,
		slots:  make(chan struct{}, gradeWorkers),
		base:   ctx,
		cancel: cancel,
	}
}

// submit accepts a recorded request. It returns immediately; nothing about the
// caller's request is waiting on a grader.
func (r *gradeRunner) submit(job *store.GradeJob, opts regrade.Options) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		case <-r.base.Done():
			// Accepted but never started: the process is going down. Say so on
			// the row, because a page left open would otherwise wait for a
			// verdict that nothing is producing.
			_ = r.abandon(job.ID, "the server stopped before this grade started; the cell keeps the verdict it had")
			return
		}
		// regrade.Run records its own failure: a request that was accepted has
		// to end somewhere a reader can see it, so the error is not returned to
		// a caller that has already been answered.
		_, _, _ = regrade.Run(r.base, r.st, job, opts)
	}()
}

// abandon writes a failure onto a job with a context that survives shutdown.
func (r *gradeRunner) abandon(id, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.base), 5*time.Second)
	defer cancel()
	return r.st.FailGradeJob(ctx, id, reason)
}

// wait blocks until every accepted job has finished, or until ctx is done.
//
// It exists for two callers with the same need from opposite directions: a
// shutdown that wants to leave no row saying "running", and a test that wants to
// assert on a verdict without polling for it.
func (r *gradeRunner) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close stops accepting work and waits for what was accepted.
//
// Cancelling first is what makes this quick: an in-flight judge call is
// abandoned and recorded as "the server stopped", rather than holding the
// process open for as long as the model takes.
func (r *gradeRunner) close(ctx context.Context) error {
	r.cancel()
	return r.wait(ctx)
}

// WaitGrades blocks until the grades this server accepted have finished.
//
// Exported for tests and for shutdown code that wants to know the work is done;
// not part of the page's contract, which only ever reads rows.
func (s *Server) WaitGrades(ctx context.Context) error {
	return s.grades.wait(ctx)
}

// Close stops the server's background grading.
//
// A server without a Close leaves the last press half-done: the row would say
// "running" until the next process swept it up.
func (s *Server) Close(ctx context.Context) error {
	if s.grades == nil {
		return nil
	}
	return s.grades.close(ctx)
}

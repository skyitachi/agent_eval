package regrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/store"
)

// # Requests, as distinct from verdicts
//
// A verdict belongs to a cell: it is the cell's current answer, it is what the
// report renders, and there is exactly one of it. Reaching that verdict is an
// *event* -- it can take half a minute, it can cost a model call, and it can
// fail without writing anything. Those two things have different lifetimes, so
// they are stored separately: the verdict on the attempt, the request in
// grade_jobs.
//
// That split is what lets a page answer a press immediately. It records the
// request, hands back an id, and goes to work in the background; the reader
// sees "grading" instead of a browser that appears to have hung for twenty-four
// seconds, and the tokens the judgement actually cost are somewhere a reader can
// find them afterwards instead of being folded into the agent's spend.

// Start records a request as queued and returns it.
//
// Nothing has run yet: this is the row a page points at while it waits, and the
// id the report uses to say which press it is talking about.
func Start(ctx context.Context, st *store.Store, req store.GradeRequest) (*store.GradeJob, error) {
	return st.CreateGradeJob(ctx, req)
}

// Run executes one recorded request to completion and records what it cost.
//
// Every failure lands on the job row rather than escaping as an error a caller
// must handle: a request that was accepted has to end somewhere a reader can
// see, and the alternative -- an accepted request that silently vanishes -- is
// the one outcome a page cannot explain. The attempt is untouched on failure,
// so the cell keeps the verdict it had.
func Run(ctx context.Context, st *store.Store, job *store.GradeJob, o Options) (*Outcome, *store.GradeJob, error) {
	if err := st.StartGradeJob(ctx, job.ID); err != nil {
		// The row left "queued" underneath us: it was abandoned by a startup
		// sweep or another process. Running on would write a verdict on behalf
		// of a request that is on record as failed.
		return nil, nil, err
	}
	started := time.Now()

	outcome, err := One(ctx, st, job.RunID, job.TaskID, job.Sample, o)
	if err != nil {
		reason := describeFailure(ctx, err)
		if ferr := fail(context.WithoutCancel(ctx), st, job.ID, reason); ferr != nil {
			return nil, nil, fmt.Errorf("%w (and the job could not be marked failed: %v)", err, ferr)
		}
		return nil, nil, err
	}

	model, in, out := judgeSpend(outcome.Grade.Results)
	details := ""
	if raw, err := json.Marshal(outcome.Grade.Results); err == nil {
		details = string(raw)
	}
	finished := store.GradeOutcome{
		Score:        outcome.Grade.Score,
		Passed:       outcome.Grade.Passed,
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		LatencyMs:    time.Since(started).Milliseconds(),
		Details:      details,
	}
	if err := st.FinishGradeJob(ctx, job.ID, finished); err != nil {
		// The verdict is written; only the receipt failed. Reported rather than
		// swallowed, because the page reads the receipt to say what it cost.
		return outcome, nil, err
	}

	done := *job
	done.Status = store.GradeDone
	done.Model, done.InputTokens, done.OutputTokens = finished.Model, finished.InputTokens, finished.OutputTokens
	done.LatencyMs, done.Score, done.Passed, done.Details = finished.LatencyMs, finished.Score, finished.Passed, finished.Details
	return outcome, &done, nil
}

// fail writes the failure onto the job row.
//
// It is given a context that survives the caller's cancellation on purpose: a
// shutdown is exactly when a row would otherwise be left saying "running" with
// nothing running it.
func fail(ctx context.Context, st *store.Store, id, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return st.FailGradeJob(ctx, id, reason)
}

// describeFailure turns what a grader returned into what the page will say.
//
// The distinction that matters is between "the grading was wrong" and "the
// grading was interrupted": a cancelled context is this process's doing -- a
// shutdown, a timeout it set -- and saying so keeps it from reading as a verdict
// about the work.
func describeFailure(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "the server stopped before the verdict was reached; the cell keeps the verdict it had"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "the verdict took longer than the server allows and was abandoned; the cell keeps the verdict it had"
	default:
		return strings.TrimSpace(err.Error())
	}
}

// judgeSpend sums what the graders spent on a model.
//
// Only graders that call one report a cost, and the numbers come from their own
// details rather than from a table here: the judge is the only thing that knows
// what a call used, and a grader added later that calls a model would otherwise
// have its spend silently dropped.
func judgeSpend(results []grader.Result) (model string, in, out int) {
	for _, r := range results {
		if r.Details == nil {
			continue
		}
		if m, ok := r.Details["judge_model"].(string); ok && m != "" && model == "" {
			model = m
		}
		usage, ok := r.Details["judge_usage"].(map[string]any)
		if !ok {
			continue
		}
		in += asInt(usage["input_tokens"])
		out += asInt(usage["output_tokens"])
	}
	return model, in, out
}

// asInt reads a token count that may have been through JSON and back.
//
// The judge puts Go ints in its details in-process, and the same details are
// float64 once they have been round-tripped through the stored grades JSON. A
// single type assertion here would count one of the two as zero.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

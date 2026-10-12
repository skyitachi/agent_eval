package report

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/skyitachi/agenteval/internal/attrib"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/store"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Load projects one run onto a payload.
func Load(ctx context.Context, st *store.Store, runID string, opts Options) (*Data, error) {
	opts = opts.withDefaults()
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	attempts, err := st.ListAttempts(ctx, runID)
	if err != nil {
		return nil, err
	}
	data := build(run, attempts, opts)
	return data, nil
}

// LoadSweep projects a sweep onto a payload, with the attribution attached.
//
// The attribution is computed rather than stored, but only from rows the sweep
// wrote about itself: which axis, which levels, which control. The matrix
// document that produced it is not consulted, so the page stays a function of
// the database.
func LoadSweep(ctx context.Context, st *store.Store, sweepID string, opts Options) (*Data, error) {
	opts = opts.withDefaults()
	att, err := attrib.Attribute(ctx, st, sweepID)
	if err != nil {
		return nil, err
	}
	runs, err := st.ListRuns(ctx, store.RunFilter{SweepID: sweepID})
	if err != nil {
		return nil, err
	}

	// The page is about the sweep, so its run section describes the sweep
	// rather than any one level. The control is the reference every number in
	// the attribution is measured against, so it is the natural stand-in.
	var control store.Run
	for _, r := range runs {
		if r.FactorLevel == att.Control {
			control = r
			break
		}
	}
	var attempts []store.Attempt
	if control.ID != "" {
		attempts, err = st.ListAttempts(ctx, control.ID)
		if err != nil {
			return nil, err
		}
	}

	data := build(control, attempts, opts)
	data.Sweep = att
	data.Title = sweepTitle(att)
	// A sweep report is about the comparison, not about the control level's
	// per-task evidence, and carrying that evidence would triple its size for
	// nothing.
	data.Tasks = nil
	if att.DatasetDrift {
		data.Warnings = append(data.Warnings,
			"the levels of this sweep did not run the same dataset; the comparisons below are not readable")
	}
	return data, nil
}

// LoadComparison projects two runs onto one payload.
func LoadComparison(ctx context.Context, st *store.Store, leftID, rightID string, opts Options) (*Data, error) {
	opts = opts.withDefaults()
	leftRun, err := st.GetRun(ctx, leftID)
	if err != nil {
		return nil, err
	}
	rightRun, err := st.GetRun(ctx, rightID)
	if err != nil {
		return nil, err
	}
	left, err := st.ListAttempts(ctx, leftID)
	if err != nil {
		return nil, err
	}
	right, err := st.ListAttempts(ctx, rightID)
	if err != nil {
		return nil, err
	}

	data := build(rightRun, right, opts)
	data.Title = fmt.Sprintf("%s vs %s", label(leftRun), label(rightRun))
	data.Compare = compare(leftRun, left, rightRun, right, opts)
	return data, nil
}

// LoadIndex projects the run listing.
func LoadIndex(ctx context.Context, st *store.Store, limit int, filter store.RunFilter, opts Options) (*Index, error) {
	opts = opts.withDefaults()
	filter.Limit = limit
	runs, err := st.ListRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		Version:   DataVersion,
		Generated: opts.GeneratedAt.UTC().Format(time.RFC3339),
		Title:     "Evaluation runs",
	}
	for _, r := range runs {
		attempts, err := st.ListAttempts(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		s := runner.Summarize(attempts, distinctTasks(attempts))
		entry := IndexEntry{
			ID:          r.ID,
			Name:        r.Name,
			Status:      r.Status,
			StartedAt:   r.StartedAt.UTC().Format(time.RFC3339),
			DurationMs:  r.DurationMs,
			DatasetName: r.DatasetName,
			AgentKind:   r.AgentKind,
			AgentModel:  r.AgentModel,
			FactorAxis:  r.FactorAxis,
			FactorLevel: r.FactorLevel,
			SweepID:     r.SweepID,
			Counts:      count(s),
			Tokens:      s.Tokens,
			CostUSD:     s.CostUSD,
			Error:       r.Error,
		}
		if pr, ok := s.PassRate(); ok {
			entry.PassRate = &pr
		}
		if r.Status == store.RunRunning || r.Status == store.RunInterrupted {
			// An unfinished run's numbers move; the page says so rather than
			// presenting a partial total as final.
			idx.Live = r.Status == store.RunRunning
		}
		idx.Runs = append(idx.Runs, entry)
	}
	if opts.Title != "" {
		idx.Title = opts.Title
	}
	return idx, nil
}

// distinctTasks counts the task ids in a run, which is not the cell count
// whenever a task was sampled more than once.
func distinctTasks(attempts []store.Attempt) int {
	seen := map[string]bool{}
	for _, a := range attempts {
		seen[a.TaskID] = true
	}
	return len(seen)
}

// TitleOf is the document title for a payload.
func TitleOf(d *Data) string {
	if d == nil {
		return "Evaluation report"
	}
	if d.Title != "" {
		return d.Title
	}
	return "Evaluation report"
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

func build(run store.Run, attempts []store.Attempt, opts Options) *Data {
	summary := runner.Summarize(attempts, distinctTasks(attempts))
	data := &Data{
		Version:   DataVersion,
		Generated: opts.GeneratedAt.UTC().Format(time.RFC3339),
		Title:     runTitle(run),
		Run:       runInfo(run, summary),
		Counts:    count(summary),
	}
	data.Metrics = metrics(summary)
	data.Breakdowns = breakdowns(attempts, summary)
	if !opts.NoTaskDetail {
		data.Tasks = tasks(attempts, opts)
		data.Warnings = append(data.Warnings, fit(data, opts)...)
	} else {
		data.Warnings = append(data.Warnings, "per-task detail was omitted on request")
	}
	return data
}

func runTitle(run store.Run) string {
	switch {
	case run.Name != "" && run.FactorLevel != "":
		return fmt.Sprintf("%s / %s=%s", run.Name, run.FactorAxis, run.FactorLevel)
	case run.Name != "":
		return run.Name
	case run.DatasetName != "":
		return run.DatasetName
	default:
		return "Evaluation report"
	}
}

func sweepTitle(att *attrib.Attribution) string {
	name := att.Name
	if name == "" {
		name = att.SweepID
	}
	return fmt.Sprintf("%s: %d levels of %s", name, len(att.Levels), att.Axis)
}

func runInfo(run store.Run, s runner.Summary) RunInfo {
	info := RunInfo{
		ID:          run.ID,
		Name:        run.Name,
		Status:      run.Status,
		Error:       run.Error,
		StartedAt:   run.StartedAt.UTC().Format(time.RFC3339),
		DurationMs:  run.DurationMs,
		DatasetName: run.DatasetName,
		DatasetHash: run.DatasetHash,
		DatasetPath: run.DatasetPath,
		AgentKind:   run.AgentKind,
		AgentModel:  run.AgentModel,
		AgentBin:    run.AgentBin,
		Backend:     run.Backend,
		Samples:     run.Samples,
		Concurrency: run.Concurrency,
		FactorAxis:  run.FactorAxis,
		FactorLevel: run.FactorLevel,
		SweepID:     run.SweepID,
		Notes:       run.Notes,
		ToolVersion: run.ToolVersion,
		Summary:     s,
	}
	if run.EndedAt != nil {
		info.EndedAt = run.EndedAt.UTC().Format(time.RFC3339)
	}
	return info
}

func count(s runner.Summary) Counts {
	return Counts{
		Cells:          s.Cells,
		Tasks:          s.Tasks,
		Graded:         s.Graded,
		Passed:         s.Passed,
		Failed:         s.Graded - s.Passed,
		Ungraded:       s.Ungraded,
		Timeouts:       s.Timeouts,
		HarnessErrors:  s.HarnessErrors,
		SamplesPerTask: 1,
	}
}

// metrics turns the summary into headline numbers.
//
// Direction is which way is *good*, not which way the number went, because that
// is what the page needs to colour a value. A metric whose goodness depends on
// the reader's intent would be better left neutral than painted.
func metrics(s runner.Summary) []Metric {
	out := []Metric{
		{Key: "cells", Label: "cells", Display: fmt.Sprintf("%d", s.Cells), Direction: "neutral"},
	}
	if pr, ok := s.PassRate(); ok {
		out = append(out, Metric{
			Key: "pass_rate", Label: "pass rate",
			Display:   fmt.Sprintf("%.1f%%", pr*100),
			Unit:      fmt.Sprintf("%d of %d graded cells", s.Passed, s.Graded),
			Direction: "up",
		})
	} else {
		out = append(out, Metric{
			Key: "pass_rate", Label: "pass rate", Display: "—", Direction: "neutral",
			Note: "nothing in this run was graded, so there is no pass rate to report",
		})
	}
	out = append(out,
		Metric{Key: "tokens", Label: "tokens", Display: humanCount(s.Tokens), Direction: "down",
			Unit: fmt.Sprintf("%d in / %d out / %d cached", s.InputTokens, s.OutputTokens, s.CacheReadTokens)},
		Metric{Key: "cost_usd", Label: "cost", Display: fmt.Sprintf("$%.4f", s.CostUSD), Direction: "down"},
		Metric{Key: "duration_ms", Label: "wall clock", Display: humanDuration(s.DurationMs), Direction: "down"},
	)
	if s.Cells > 0 {
		out = append(out, Metric{
			Key: "tokens_per_cell", Label: "tokens / cell",
			Display: fmt.Sprintf("%d", s.Tokens/s.Cells), Direction: "down",
		})
	}
	// Timeouts and harness errors are always shown, even at zero: a header that
	// only lists the problems it has makes an absent one invisible.
	out = append(out,
		Metric{Key: "timeouts", Label: "timeouts", Display: fmt.Sprintf("%d", s.Timeouts), Direction: "down",
			Note: "cells that ran out of budget; a datum about the agent, not a harness failure"},
		Metric{Key: "harness_errors", Label: "harness errors", Display: fmt.Sprintf("%d", s.HarnessErrors),
			Direction: "down", Note: "cells the harness could not measure at all"},
	)
	if s.Ungraded > 0 {
		out = append(out, Metric{Key: "ungraded", Label: "ungraded", Display: fmt.Sprintf("%d", s.Ungraded),
			Direction: "neutral", Note: "measured but with nothing to score against"})
	}
	// Each delta is shown only when some cell actually carried the matching
	// measurement. A recording that never captured busy time leaves the sum at
	// zero, and a zero rendered as "+0s" reads as "no change" rather than as
	// "never measured".
	if s.TokenCells > 0 {
		out = append(out, Metric{
			Key: "baseline_tokens", Label: "tokens vs baseline",
			Display: signedCount(s.TokenDelta), Direction: "down",
			Unit: fmt.Sprintf("across %d cells with a recorded baseline", s.TokenCells),
		})
	}
	if s.TimedCells > 0 {
		out = append(out, Metric{
			Key: "baseline_time", Label: "time vs baseline",
			Display: signedDuration(s.TimeDeltaMs), Direction: "down",
			Unit: "measured against the recorded session's busy time, not its wall clock",
		})
	}
	return out
}

func breakdowns(attempts []store.Attempt, s runner.Summary) []Breakdown {
	out := []Breakdown{}

	// Outcome mix first: it is the one breakdown that can explain a bad pass
	// rate without any other number being wrong.
	outcome := Breakdown{Title: "Cell outcomes"}
	outcome.Rows = append(outcome.Rows, BreakdownRow{
		Label: "graded and passed", Value: float64(s.Passed), Hits: s.Passed, Total: s.Cells,
		Text: fmt.Sprintf("%d", s.Passed),
	})
	failed := s.Graded - s.Passed
	outcome.Rows = append(outcome.Rows, BreakdownRow{
		Label: "graded and failed", Value: float64(failed), Hits: failed, Total: s.Cells,
		Text: fmt.Sprintf("%d", failed),
	})
	if s.Ungraded > 0 {
		outcome.Rows = append(outcome.Rows, BreakdownRow{
			Label: "measured, not graded", Value: float64(s.Ungraded), Hits: s.Ungraded, Total: s.Cells,
			Text: fmt.Sprintf("%d", s.Ungraded),
		})
	}
	if s.Timeouts > 0 {
		outcome.Rows = append(outcome.Rows, BreakdownRow{
			Label: "timed out", Value: float64(s.Timeouts), Hits: s.Timeouts, Total: s.Cells,
			Text: fmt.Sprintf("%d", s.Timeouts),
		})
	}
	if s.HarnessErrors > 0 {
		outcome.Rows = append(outcome.Rows, BreakdownRow{
			Label: "harness error", Value: float64(s.HarnessErrors), Hits: s.HarnessErrors, Total: s.Cells,
			Text: fmt.Sprintf("%d", s.HarnessErrors),
		})
	}
	out = append(out, outcome)

	if b := byCategory(attempts); len(b.Rows) > 0 {
		out = append(out, b)
	}
	if b := stopReasons(attempts); len(b.Rows) > 0 {
		out = append(out, b)
	}
	return out
}

func byCategory(attempts []store.Attempt) Breakdown {
	type acc struct{ passed, graded, total int }
	per := map[string]*acc{}
	var order []string
	for _, a := range attempts {
		cat := a.Category
		if cat == "" {
			cat = "(uncategorised)"
		}
		if per[cat] == nil {
			per[cat] = &acc{}
			order = append(order, cat)
		}
		e := per[cat]
		e.total++
		if a.Graded {
			e.graded++
			if a.Passed {
				e.passed++
			}
		}
	}
	b := Breakdown{Title: "Pass rate by category", Note: "ungraded cells are excluded from the rate and shown in the count"}
	sort.Strings(order)
	for _, cat := range order {
		e := per[cat]
		row := BreakdownRow{Label: cat, Total: e.total, Hits: e.passed}
		if e.graded > 0 {
			row.Value = 100 * float64(e.passed) / float64(e.graded)
			row.Text = fmt.Sprintf("%.0f%% (%d/%d)", row.Value, e.passed, e.graded)
		} else {
			row.Text = fmt.Sprintf("ungraded (%d)", e.total)
		}
		b.Rows = append(b.Rows, row)
	}
	// A category with no graded cells has no rate to sort on, so it sorts to the
	// end rather than being dropped.
	sort.SliceStable(b.Rows, func(i, j int) bool { return b.Rows[i].Value > b.Rows[j].Value })
	return b
}

func stopReasons(attempts []store.Attempt) Breakdown {
	per := map[string]int{}
	for _, a := range attempts {
		reason := a.StopReason
		if reason == "" {
			reason = "(unrecorded)"
		}
		per[reason]++
	}
	if len(per) == 0 {
		return Breakdown{}
	}
	b := Breakdown{Title: "Stop reasons"}
	for reason, n := range per {
		b.Rows = append(b.Rows, BreakdownRow{
			Label: reason, Value: float64(n), Hits: n, Total: len(attempts),
			Text: fmt.Sprintf("%d", n),
		})
	}
	sort.Slice(b.Rows, func(i, j int) bool {
		if b.Rows[i].Value != b.Rows[j].Value {
			return b.Rows[i].Value > b.Rows[j].Value
		}
		return b.Rows[i].Label < b.Rows[j].Label
	})
	return b
}

func tasks(attempts []store.Attempt, opts Options) []Task {
	out := make([]Task, 0, len(attempts))
	for _, a := range attempts {
		t := Task{
			Key:          cellKey(a.TaskID, a.Sample),
			ID:           a.TaskID,
			SampleIndex:  a.Sample,
			Category:     a.Category,
			Tags:         a.Tags,
			Status:       a.Status,
			StopReason:   a.StopReason,
			Graded:       a.Graded,
			Passed:       a.Passed,
			Score:        a.Score,
			Threshold:    a.Threshold,
			DurationMs:   a.DurationMs,
			CostUSD:      a.CostUSD,
			InputTokens:  a.InputTokens,
			OutputTokens: a.OutputTokens,
			CacheRead:    a.CacheReadTokens,
			CacheCreate:  a.CacheCreateTokens,
			TotalTokens:  a.TokenTotal(),
			Turns:        a.Turns,
			ToolCalls:    a.ToolCalls,
			ToolErrors:   a.ToolErrors,
			Error:        a.Error,
		}
		diff, answer := clip(a.Diff, opts.MaxFieldBytes), clip(a.Answer, opts.MaxFieldBytes)
		t.Baseline = baselineView(a, opts.MaxFieldBytes)
		// The recorded answer is a text field like any other, so it counts
		// against the same budget and says so the same way when it is cut.
		if diff != a.Diff || answer != a.Answer ||
			(t.Baseline != nil && t.Baseline.Output != a.Baseline.Output) {
			t.Detail = "text fields were clipped to the report's field budget"
		}
		t.Diff, t.Answer = diff, answer
		for _, g := range a.Grades {
			t.Graders = append(t.Graders, Grader{
				Name: g.Grader, Type: g.Type, Passed: g.Passed, Score: g.Score,
				Weight: g.Weight, Required: g.Required, Reason: g.Reason,
				Error: g.Error, Details: g.Details,
			})
		}
		if len(a.Trace) > 0 {
			var tr trace.Trace
			if err := json.Unmarshal(a.Trace, &tr); err == nil {
				t.Tools = toolCalls(tr.ToolCalls, opts.MaxToolCallsPerTask, opts.MaxFieldBytes)
				t.Events = events(tr.Events, opts.MaxEventsPerTask, opts.MaxFieldBytes)
			}
		}
		out = append(out, t)
	}
	// Failures first, then by id: the reader's first question is always what
	// went wrong.
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].priority(), out[j].priority()
		if pi != pj {
			return pi > pj
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].SampleIndex < out[j].SampleIndex
	})
	return out
}

// priority orders how much a cell's evidence deserves to survive the size
// budget: a failure explains a bad number, a pass explains nothing.
func (t Task) priority() int {
	switch {
	case t.Status == store.CellError:
		return 4
	case t.Status == store.CellTimeout:
		return 3
	case t.Graded && !t.Passed:
		return 2
	case !t.Graded:
		return 1
	default:
		return 0
	}
}

func baselineView(a store.Attempt, maxField int) *BaselineView {
	b, ok := a.ComparableBaseline()
	if !ok {
		// No comparison is shown for a cell that was never measured, so the row
		// reads as "nothing to compare" rather than as an exact match.
		return nil
	}
	tokenDelta, hasTokens := a.TokenDelta()
	msDelta, hasTime := a.MsDelta()
	return &BaselineView{
		ID:         b.ID,
		Ref:        b.Ref,
		Tokens:     b.Tokens,
		Ms:         b.Ms,
		Cost:       b.Cost,
		Turns:      b.Turns,
		TokenDelta: tokenDelta,
		MsDelta:    msDelta,
		HasTokens:  hasTokens,
		HasTime:    hasTime,
		Output:     clip(b.Output, maxField),
	}
}

// events projects the trajectory, keeping both ends when it has to be cut.
//
// A viewer that shows only the first N events shows the task framing and never
// the answer, which is the half a reader is looking for; a viewer that shows
// only the last N shows the answer with no idea what it is answering. So a cut
// trajectory keeps a head, a tail, and a gap marker that states in the document
// how much is missing.
func events(evs []trace.Event, max int, maxField int) []Event {
	if max <= 0 || len(evs) <= max {
		return project(evs, maxField)
	}
	head := max / 2
	tail := max - head
	out := project(evs[:head], maxField)
	out = append(out, Event{
		Index: -1,
		Kind:  "truncated",
		Text: fmt.Sprintf("%d events omitted from the middle of the trajectory to keep the report under its size budget",
			len(evs)-max),
	})
	out = append(out, project(evs[len(evs)-tail:], maxField)...)
	return out
}

func project(evs []trace.Event, maxField int) []Event {
	out := make([]Event, 0, len(evs))
	for _, e := range evs {
		p := Event{
			Index: e.Index, Kind: string(e.Kind),
			Text:      clip(e.Text, maxField),
			ToolUseID: e.ToolUseID,
			ToolName:  e.ToolName,
			ToolInput: clip(string(e.ToolInput), maxField),
			ToolOut:   clip(e.ToolOutput, maxField),
			ToolError: e.ToolError,
		}
		if !e.Timestamp.IsZero() {
			p.Timestamp = e.Timestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, p)
	}
	return out
}

func toolCalls(calls []trace.ToolCall, max int, maxField int) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for i, c := range calls {
		if max > 0 && len(out) >= max {
			tc := ToolCall{Seq: 0, Name: fmt.Sprintf("... %d more tool calls omitted", len(calls)-i)}
			out = append(out, tc)
			break
		}
		out = append(out, ToolCall{
			Seq: c.Seq, ID: c.ID, Name: c.Name, IsError: c.IsError, DurationMs: c.DurationMs,
			Input:  clip(string(c.Input), maxField),
			Output: clip(c.Output, maxField),
		})
	}
	return out
}

func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// Keep both ends: the head of a diff or a trace carries the framing and the
	// tail carries the answer, and dropping either makes the remainder useless.
	half := max / 2
	omitted := len(s) - 2*half
	return s[:half] + fmt.Sprintf("\n\n... [%d bytes omitted] ...\n\n", omitted) + s[len(s)-half:]
}

// ---------------------------------------------------------------------------
// Size budget
// ---------------------------------------------------------------------------

// fit sheds per-task detail until the payload fits the budget, and reports what
// it dropped.
//
// The order is not arbitrary: a cell's detail is dropped in reverse order of how
// much it explains, so the failures keep their evidence longest. The accounting
// is incremental -- each task's detail is measured once and subtracted as it is
// shed -- because re-marshalling the whole document after every drop would cost
// more than the report does.
func fit(data *Data, opts Options) []string {
	total, err := serializedSize(data)
	if err != nil || total <= opts.MaxInlineBytes {
		return nil
	}

	// Measure each task's detail and record an order to shed in.
	type victim struct {
		index int
		bytes int
	}
	order := make([]victim, 0, len(data.Tasks))
	for i := range data.Tasks {
		t := data.Tasks[i]
		n, err := serializedSize(t.detail())
		if err != nil {
			continue
		}
		order = append(order, victim{index: i, bytes: n})
	}
	// Least explicative first: passes, then ungraded, then the rest.
	sort.SliceStable(order, func(a, b int) bool {
		ta, tb := data.Tasks[order[a].index], data.Tasks[order[b].index]
		if ta.priority() != tb.priority() {
			return ta.priority() < tb.priority()
		}
		return order[a].bytes > order[b].bytes
	})

	shed := 0
	for _, v := range order {
		if total <= opts.MaxInlineBytes {
			break
		}
		data.Tasks[v.index].strip()
		total -= v.bytes
		shed++
	}
	if shed == 0 {
		return nil
	}
	warnings := []string{fmt.Sprintf(
		"the payload exceeded the %s budget, so per-task diff, trajectory and grader detail was dropped from "+
			"%d of %d cells; cells that failed or timed out kept theirs longest",
		humanBytes(opts.MaxInlineBytes), shed, len(data.Tasks))}
	if total > opts.MaxInlineBytes {
		// Every cell's detail is gone and it still does not fit, so the run
		// itself is bigger than the budget. Saying so is the only honest
		// option: silently emitting a 40 MiB page would look like it worked.
		warnings = append(warnings, fmt.Sprintf(
			"even with no per-task detail the payload is about %s, above the %s budget; "+
				"the report is still complete, it is just larger than requested",
			humanBytes(total), humanBytes(opts.MaxInlineBytes)))
	}
	return warnings
}

// taskDetail is the part of a task that shedding removes. It is a named type
// rather than the whole task because only these fields are weighed: every number
// that feeds a total stays, so the headline figures do not move when detail is
// dropped.
type taskDetail struct {
	Diff     string        `json:"diff,omitempty"`
	Answer   string        `json:"answer,omitempty"`
	Graders  []Grader      `json:"graders,omitempty"`
	Tools    []ToolCall    `json:"tools,omitempty"`
	Baseline *BaselineView `json:"baseline,omitempty"`
}

func (t Task) detail() taskDetail {
	return taskDetail{
		Diff: t.Diff, Answer: t.Answer,
		Graders: t.Graders, Tools: t.Tools, Baseline: t.Baseline,
	}
}

// strip removes a task's bulky evidence while keeping every number.
func (t *Task) strip() {
	t.Diff, t.Answer = "", ""
	t.Graders, t.Tools = nil, nil
	t.Detail = "per-task detail was dropped to keep the report under its size budget"
}

func serializedSize(v any) (int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

func compare(leftRun store.Run, left []store.Attempt,
	rightRun store.Run, right []store.Attempt, opts Options) *Compare {

	cmp := &Compare{
		LeftRunID:  leftRun.ID,
		RightRunID: rightRun.ID,
		LeftLabel:  label(leftRun),
		RightLabel: label(rightRun),
	}

	ls := runner.Summarize(left, distinctTasks(left))
	rs := runner.Summarize(right, distinctTasks(right))
	cmp.Metrics = metricDeltas(ls, rs)

	lIdx := indexAttempts(left)
	rIdx := indexAttempts(right)
	keys := unionKeys(lIdx, rIdx)
	sort.Strings(keys)
	for _, k := range keys {
		l, okL := lIdx[k]
		r, okR := rIdx[k]
		d := TaskDelta{
			Key: k, ID: l.TaskID, Category: firstNonEmpty(l.Category, r.Category),
			Left:  taskRef(l, okL),
			Right: taskRef(r, okR),
		}
		if d.ID == "" {
			d.ID = r.TaskID
		}
		d.Verdict = compareVerdict(l, okL, r, okR)
		switch d.Verdict {
		case "fixed":
			cmp.Summary.Fixed++
		case "regressed":
			cmp.Summary.Regressed++
		case "ungraded":
			// A pair that has nothing to score it against is neither a win nor
			// a loss, and counting it as either would turn a dataset of
			// collected sessions into a mass regression.
			cmp.Summary.Ungraded++
		case "unchanged":
			if l.Passed {
				cmp.Summary.BothPass++
			} else {
				cmp.Summary.BothFail++
			}
		}
		switch {
		case !okL:
			cmp.Summary.OnlyRight++
		case !okR:
			cmp.Summary.OnlyLeft++
		default:
			cmp.Summary.Common++
		}
		cmp.Tasks = append(cmp.Tasks, d)
	}
	sort.SliceStable(cmp.Tasks, func(i, j int) bool {
		return rank(cmp.Tasks[i].Verdict) > rank(cmp.Tasks[j].Verdict)
	})
	return cmp
}

func taskRef(a store.Attempt, present bool) TaskRef {
	if !present {
		return TaskRef{}
	}
	return TaskRef{
		Present: present, Graded: a.Graded, Passed: a.Passed, Status: a.Status,
		StopReason: a.StopReason, Score: a.Score, DurationMs: a.DurationMs,
		CostUSD: a.CostUSD, Tokens: a.TokenTotal(), Turns: a.Turns, Error: a.Error,
	}
}

// compareVerdict talks about correctness only. A task that got cheaper and wrong
// did not improve, and a comparison that averages the two would say it did.
func compareVerdict(l store.Attempt, okL bool, r store.Attempt, okR bool) string {
	switch {
	case !okL:
		return "added"
	case !okR:
		return "removed"
	case !l.Graded || !r.Graded:
		return "ungraded"
	case !l.Passed && r.Passed:
		return "fixed"
	case l.Passed && !r.Passed:
		return "regressed"
	default:
		return "unchanged"
	}
}

func rank(verdict string) int {
	switch verdict {
	case "regressed":
		return 4
	case "fixed":
		return 3
	case "added", "removed":
		return 2
	case "ungraded":
		return 1
	default:
		return 0
	}
}

func metricDeltas(l, r runner.Summary) []MetricDelta {
	type def struct {
		key, label, unit string
		// neutral marks a metric that is not better in either direction. Cell
		// count is the case: when two runs cover different numbers of cells they
		// are not comparable, and calling one of them better would hide that.
		neutral       bool
		lowerIsBetter bool
		left, right   float64
		format        func(float64) string
		deltaFormat   func(float64) string
	}
	defs := []def{
		{key: "pass_rate", label: "pass rate", left: rate(l), right: rate(r),
			format: percent, deltaFormat: signedPercent},
		{key: "cells", label: "cells", neutral: true, left: float64(l.Cells), right: float64(r.Cells),
			format: whole, deltaFormat: signedWhole},
		{key: "tokens", label: "tokens", lowerIsBetter: true, left: float64(l.Tokens), right: float64(r.Tokens),
			format: humanCountF, deltaFormat: signedCountF},
		{key: "cost_usd", label: "cost", unit: "USD", lowerIsBetter: true, left: l.CostUSD, right: r.CostUSD,
			format:      func(v float64) string { return fmt.Sprintf("$%.4f", v) },
			deltaFormat: func(v float64) string { return fmt.Sprintf("%+.4f", v) }},
		{key: "duration_ms", label: "wall clock", unit: "ms", lowerIsBetter: true,
			left: float64(l.DurationMs), right: float64(r.DurationMs),
			format: humanDurationF, deltaFormat: signedDurationF},
		{key: "timeouts", label: "timeouts", lowerIsBetter: true, left: float64(l.Timeouts), right: float64(r.Timeouts),
			format: whole, deltaFormat: signedWhole},
		{key: "harness_errors", label: "harness errors", lowerIsBetter: true,
			left: float64(l.HarnessErrors), right: float64(r.HarnessErrors),
			format: whole, deltaFormat: signedWhole},
	}

	var out []MetricDelta
	for _, d := range defs {
		if l.Cells == 0 && r.Cells == 0 {
			continue
		}
		m := MetricDelta{
			Key: d.key, Label: d.label, Unit: d.unit,
			Left: d.left, Right: d.right,
			LeftText: d.format(d.left), RightText: d.format(d.right),
			Delta: d.right - d.left, DeltaText: d.deltaFormat(d.right - d.left),
		}
		switch {
		case d.neutral:
			m.Verdict = "unknown"
		case d.lowerIsBetter:
			m.Direction = "down"
		default:
			m.Direction = "up"
		}
		if !d.neutral {
			switch {
			case d.right == d.left:
				m.Verdict = "same"
			case (d.right < d.left) == d.lowerIsBetter:
				m.Verdict = "better"
			default:
				m.Verdict = "worse"
			}
		}
		out = append(out, m)
	}
	return out
}

func rate(s runner.Summary) float64 {
	if pr, ok := s.PassRate(); ok {
		return pr * 100
	}
	return 0
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func cellKey(taskID string, sample int) string {
	return fmt.Sprintf("%s#%d", taskID, sample)
}

func indexAttempts(attempts []store.Attempt) map[string]store.Attempt {
	out := make(map[string]store.Attempt, len(attempts))
	for _, a := range attempts {
		out[cellKey(a.TaskID, a.Sample)] = a
	}
	return out
}

func unionKeys(a, b map[string]store.Attempt) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		seen[k] = true
		out = append(out, k)
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func label(r store.Run) string {
	switch {
	case r.Name != "" && r.FactorLevel != "":
		return r.Name + "/" + r.FactorLevel
	case r.Name != "":
		return r.Name
	default:
		return r.ID
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func humanCountF(v float64) string { return humanCount(int(v)) }

func signedCount(n int) string {
	if n == 0 {
		return "0"
	}
	return fmt.Sprintf("%+s", humanCount(n))
}

func signedCountF(v float64) string { return signedCount(int(v)) }

func humanDuration(ms int64) string { return humanDurationF(float64(ms)) }

func humanDurationF(ms float64) string {
	switch {
	case ms <= 0:
		return "0s"
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", ms/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", int(ms/60_000), int(ms/1000)%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(ms/3_600_000), int(ms/60_000)%60)
	}
}

func signedDuration(ms int64) string { return signedDurationF(float64(ms)) }

func signedDurationF(ms float64) string {
	if ms == 0 {
		return "0s"
	}
	sign := "+"
	if ms < 0 {
		sign = "-"
		ms = -ms
	}
	return sign + humanDurationF(ms)
}

func percent(v float64) string { return fmt.Sprintf("%.1f%%", v) }

func signedPercent(v float64) string { return fmt.Sprintf("%+.1f pp", v) }

func whole(v float64) string { return fmt.Sprintf("%.0f", v) }

func signedWhole(v float64) string { return fmt.Sprintf("%+.0f", v) }

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

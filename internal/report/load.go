package report

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
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
	data.Warnings = append(data.Warnings,
		comparisonWarnings(leftRun, rightRun, observedModels(left), observedModels(right))...)
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
		Run:       runInfo(run, attempts, summary),
		Counts:    count(summary),
	}
	data.Metrics = metrics(summary)
	data.Breakdowns = breakdowns(attempts, summary)
	if !opts.NoTaskDetail {
		data.Tasks = tasks(attempts, run.AgentKind, run.AgentModel, opts)
		data.Warnings = append(data.Warnings, fit(data, opts)...)
	} else {
		data.Warnings = append(data.Warnings, "per-task detail was omitted on request")
	}
	return data
}

// modelView states which model ran, in the four states the record can be in.
//
// The hard case is the third: no --model was passed, so the CLI used its own
// default, and the stream is the only place that says which one that was. The
// fourth state is the one a reader has to act on -- a model was asked for and a
// different one did the work -- and it is decided by trace.SameModel rather
// than by string equality, so that the alias the CLI resolves (`sonnet` to
// `claude-sonnet-4-5-20250929`) does not read as a substitution.
func modelView(declared string, observed []string) *ModelInfo {
	m := &ModelInfo{Declared: strings.TrimSpace(declared), Observed: observed}
	switch {
	case len(observed) == 0 && m.Declared == "":
		m.Text = "not recorded"
	case len(observed) == 0:
		// The command line is a claim about intent. Saying only "sonnet" here
		// would present it as a fact about what ran.
		m.Text = m.Declared + " (the stream named no model)"
	case m.Declared == "":
		// The ordinary case for a real run: no --model was passed, so the CLI
		// used its default, and the stream is the only place that says which.
		m.Text = "default (the CLI chose it): " + strings.Join(observed, ", ")
	default:
		for _, name := range observed {
			if !trace.SameModel(m.Declared, name) {
				m.Substitute = true
				break
			}
		}
		if m.Substitute {
			m.Text = fmt.Sprintf("%s, but the stream billed %s", m.Declared, strings.Join(observed, ", "))
		} else {
			m.Text = m.Declared
			// An alias resolves to a longer name, and naming both is what lets
			// a reader see that the run got what it asked for.
			if len(observed) == 1 && !strings.EqualFold(observed[0], m.Declared) {
				m.Text = m.Declared + " (" + observed[0] + ")"
			}
		}
	}
	return m
}

// cellModel states which model ran for one cell.
//
// The names come first and the caveat second, because a run is not necessarily
// one model: a fallback mid-run is served by one cell's stream, and the run
// header can only say that it happened somewhere. Naming the cell is how a
// reader finds which one.
func cellModel(declared string, observed []string) string {
	if len(observed) == 0 {
		// Set on a cell whose trace names none, which is the ordinary case for
		// a fixture that emits no stream. The row says so instead of vanishing,
		// because an absent row reads as "there was nothing to say".
		return "not named by the stream"
	}
	line := strings.Join(observed, ", ")
	if strings.TrimSpace(declared) == "" {
		return line
	}
	for _, name := range observed {
		if !trace.SameModel(declared, name) {
			return fmt.Sprintf("%s (asked for %s)", line, strings.TrimSpace(declared))
		}
	}
	return line
}

// observedModels collects what the streams named across a run's attempts.
//
// Every cell is consulted, not the first one that answers: a run whose model
// changed partway -- a fallback, a gateway substituting -- is exactly the case
// this exists to make visible, and taking one cell's word for it would hide it.
func observedModels(attempts []store.Attempt) []string {
	seen := map[string]bool{}
	for _, a := range attempts {
		if len(a.Trace) == 0 {
			continue
		}
		var tr trace.Trace
		if err := json.Unmarshal(a.Trace, &tr); err != nil {
			// A trace that will not decode is a fact about the record, not a
			// reason to take the whole report down; the other cells still get
			// to answer.
			continue
		}
		for _, name := range tr.Models() {
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

func runInfo(run store.Run, attempts []store.Attempt, s runner.Summary) RunInfo {
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
		Skills:      skillView(run),
		Summary:     s,
		// Two sources, and the page needs both: the label says what was asked
		// for, the streams say what answered. The view states which of the two
		// it had, so a run with no --model is not rendered as a run whose model
		// nobody wrote down.
		Model: modelView(run.AgentModel, observedModels(attempts)),
		// Taken straight off the row. Nothing on an attempt says which cells
		// shared a workspace, so a run whose flag was lost would be rendered as
		// a set of independent samples -- the one reading that is certainly
		// wrong about a chain.
		ChainSessions: run.ChainSessions,
		// Also taken straight off the row, and for the same reason: an attempt
		// carries no marker saying its workspace was the user's own directory.
		// Its diff reads exactly like one taken in a fresh checkout, so the
		// only place this fact can be stated is here.
		InplaceTasks: run.InplaceTasks,
	}
	if run.EndedAt != nil {
		info.EndedAt = run.EndedAt.UTC().Format(time.RFC3339)
	}
	return info
}

// skillView renders what a run's recorded configuration says about skills, or
// nil when it recorded none.
//
// Nil is not "none": the harness only started storing the configuration in
// schema v4, so every earlier row answers this question with silence, and a page
// that turns silence into "this run loaded nothing" is asserting something it
// was never told.
func skillView(run store.Run) *Skills {
	spec, ok := run.AgentSpec()
	if !ok {
		return nil
	}
	sv := &Skills{Dirs: spec.SkillsDeclared(), Active: spec.SkillsActive()}
	switch {
	case len(sv.Dirs) == 0:
		sv.Text = "none declared"
	case sv.Active:
		sv.Text = fmt.Sprintf("%d plugin dir(s)", len(sv.Dirs))
	default:
		sv.RemovedBy = spec.SkillsReason()
		sv.Text = fmt.Sprintf("%d plugin dir(s), not loaded (%s)", len(sv.Dirs), sv.RemovedBy)
	}
	return sv
}

// loadedSkills is the set that actually reached the agent.
//
// A declared directory that the isolation flags removed is not a skill the run
// had, so two runs whose declarations differ only in inert entries are running
// the same agent configuration -- and reporting that as a difference would make
// the one real skill difference harder to see.
func loadedSkills(spec agent.Spec) []string {
	if !spec.SkillsActive() {
		return nil
	}
	return spec.SkillsDeclared()
}

// skillLabel names a set of plugin directories for a sentence, capped so that
// two of them fit on a line.
func skillLabel(dirs []string) string {
	switch len(dirs) {
	case 0:
		return "none"
	case 1, 2:
		return strings.Join(dirs, ", ")
	default:
		return fmt.Sprintf("%s and %d more", strings.Join(dirs[:2], ", "), len(dirs)-2)
	}
}

// chainedLabel names the two states of the chaining flag for a sentence.
func chainedLabel(chained bool) string {
	if chained {
		return "chained"
	}
	return "not chained"
}

// inplaceLabel names how many of a run's tasks were replayed in the
// recording's own directory.
//
// The unit is spelled out because the comparison sentence reads "(1 task vs 0
// tasks)": a bare pair of numbers would leave the reader guessing whether they
// are being told about tasks or cells, and those differ as soon as a run has
// more than one sample.
func inplaceLabel(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}

func sameSkills(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// tasks projects the cells. runAgent is the agent the run itself drove, which
// the baseline card needs in order to say whether the recording it shows was
// made by the same one.
func tasks(attempts []store.Attempt, runAgent, declaredModel string, opts Options) []Task {
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
		t.Baseline = baselineView(a, runAgent, opts.MaxFieldBytes)
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
		var named []string
		if len(a.Trace) > 0 {
			var tr trace.Trace
			if err := json.Unmarshal(a.Trace, &tr); err == nil {
				t.Tools = toolCalls(tr.ToolCalls, opts.MaxToolCallsPerTask, opts.MaxFieldBytes)
				t.Events = events(tr.Events, opts.MaxEventsPerTask, opts.MaxFieldBytes)
				named = tr.Models()
			}
		}
		t.Model = cellModel(declaredModel, named)
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

func baselineView(a store.Attempt, runAgent string, maxField int) *BaselineView {
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
		// The provenance is not clipped: it is a name, not a transcript. The
		// notes are, because a capture is free to write an essay there.
		AgentName:      b.AgentName,
		AgentModel:     b.AgentModel,
		Source:         b.Source,
		DifferentAgent: !sameAgent(b.AgentName, runAgent),
		Notes:          clip(b.Notes, maxField),
	}
}

// sameAgent reports whether a recording's producer and a run's implementation
// are the same agent.
//
// The two names come from different layers and are spelled differently: a
// recording names its producer the way the capture wrote it ("claude-code",
// "cursor"), while a run names the implementation it registered ("claude_code").
// Comparing them literally would flag every collected-and-replayed Claude Code
// baseline as cross-agent, and a warning that fires on the ordinary case is how
// the one real warning gets ignored.
//
// An unknown name on either side counts as "not different": an empty field is
// the absence of a record, and the report already says that separately. It is
// not evidence of a different agent.
func sameAgent(recorded, ran string) bool {
	if recorded == "" || ran == "" {
		return true
	}
	fold := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "-", "")
		s = strings.ReplaceAll(s, "_", "")
		return strings.ReplaceAll(s, " ", "")
	}
	return fold(recorded) == fold(ran)
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

// comparisonWarnings names what differed between the two runs beyond the thing
// the reader is looking at.
//
// `report --compare a b` prints deltas between two runs whatever they are, and
// those deltas are attributable only when the two differ in one thing. The
// command cannot refuse the comparison -- "what changed between these two?" is
// a fair question about any pair -- so it does the next best thing and says
// what the delta is really a bundle of. Silence here reads as "these two
// numbers measured the same thing", which is the one reading that is certainly
// false.
//
// The skill set is checked here for the same reason. It is not one of the label
// columns: it lives in plugin_dirs plus the isolation flags that decide whether
// those directories survive, so a pair of runs that differ only in their skills
// used to compare as identical and the page had no way to say otherwise.
//
// seenLeft and seenRight are what the two runs' streams named, which is not the
// same claim as the model columns: those hold what each run asked for.
func comparisonWarnings(left, right store.Run, seenLeft, seenRight []string) []string {
	var differing []string
	if !sameAgent(left.AgentKind, right.AgentKind) {
		differing = append(differing, fmt.Sprintf(
			"the agent (%s vs %s)", orUnset(left.AgentKind), orUnset(right.AgentKind)))
	}
	if left.AgentModel != right.AgentModel {
		differing = append(differing, fmt.Sprintf(
			"the model (%s vs %s)", orUnset(left.AgentModel), orUnset(right.AgentModel)))
	} else if len(seenLeft) > 0 && len(seenRight) > 0 && !sameModels(seenLeft, seenRight) {
		// The declared models agree and the runs were still served differently,
		// which the columns above cannot show -- including when both declared
		// nothing, which is the ordinary case for a run that lets the CLI pick.
		//
		// Checked only when the declarations agree: when they do not, the
		// sentence above is already about the model, and two sentences about
		// one column is how both stop being read. A side whose stream named
		// nothing is silent rather than different, because an unnamed model is
		// a gap in the record and not a second model.
		differing = append(differing, fmt.Sprintf(
			"which model served the requests (%s vs %s)",
			strings.Join(seenLeft, ", "), strings.Join(seenRight, ", ")))
	}
	if left.AgentBin != right.AgentBin {
		differing = append(differing, fmt.Sprintf(
			"the agent binary (%s vs %s)", orUnset(left.AgentBin), orUnset(right.AgentBin)))
	}
	if left.Backend != right.Backend {
		differing = append(differing, fmt.Sprintf(
			"the sandbox backend (%s vs %s)", orUnset(left.Backend), orUnset(right.Backend)))
	}
	lSpec, lKnown := left.AgentSpec()
	rSpec, rKnown := right.AgentSpec()
	if lKnown && rKnown {
		lSkills, rSkills := loadedSkills(lSpec), loadedSkills(rSpec)
		if !sameSkills(lSkills, rSkills) {
			differing = append(differing, fmt.Sprintf(
				"the skills loaded (%s vs %s)", skillLabel(lSkills), skillLabel(rSkills)))
		}
	}
	// Chaining is checked for the same reason and is not the same kind of fact:
	// it does not change which agent ran, it changes what the cells are. One
	// run's cells each start from a fresh workspace and the other's start from
	// their predecessor's, so the same task is a different question in each and
	// the paired deltas below cannot be read as one agent beating another.
	// Older rows read false, which is the correct answer for them rather than a
	// gap, so this needs no "unknown" arm.
	if left.ChainSessions != right.ChainSessions {
		differing = append(differing, fmt.Sprintf(
			"whether a session's tasks were chained into one workspace (%s vs %s)",
			chainedLabel(left.ChainSessions), chainedLabel(right.ChainSessions)))
	}
	// Replaying in place is the other half of that fact, and a sharper one.
	// Both sides may have run the same agent on the same task, but one of them
	// handed the agent the recording's own directory -- with whatever the user
	// had left in it -- and the other built a workspace from the dataset. The
	// paired cells are then not the same question, whatever the labels say.
	// Older rows read 0, which is correct for them, so there is no "unknown"
	// arm here either.
	if left.InplaceTasks != right.InplaceTasks {
		differing = append(differing, fmt.Sprintf(
			"whether the cells ran in the recording's own directory (%s vs %s)",
			inplaceLabel(left.InplaceTasks), inplaceLabel(right.InplaceTasks)))
	}

	var out []string
	switch len(differing) {
	case 0:
		// Nothing the run row records changed. That is not the same as nothing
		// changed -- it is a statement about the columns that exist.
	case 1:
		out = append(out, "the two runs differ in "+differing[0]+
			", so the deltas below are that one difference and nothing else")
	default:
		out = append(out, "the two runs differ in more than one thing ("+
			strings.Join(differing, "; ")+
			"), so no delta below can be attributed to any single one of them")
	}
	if left.DatasetHash != right.DatasetHash {
		out = append(out, "the two runs did not run the same dataset content, "+
			"so the cells paired below are different questions and their verdicts do not compare")
	}
	// One side's stream named a model and the other's named none, with neither
	// run having asked for one. The label column is empty on both -- which is
	// the ordinary case, because a run only fills it by passing --model -- so
	// the page has nothing to say about either run's model and a reader fills
	// the gap with "they were the same". Naming the silent side is what the
	// skills arm below does, for the same reason: the wrong guess a reader
	// makes about silence is that the silent run had nothing to declare.
	//
	// Restricted to the pair that asked for nothing. When a model was declared
	// on both sides, that declaration already answers the question and this
	// would be a second sentence about the same column.
	if left.AgentModel == "" && right.AgentModel == "" && (len(seenLeft) == 0) != (len(seenRight) == 0) {
		which := label(right)
		if len(seenRight) != 0 {
			which = label(left)
		}
		out = append(out, fmt.Sprintf(
			"run %s named no model in its stream, so whether the two runs were served "+
				"by the same model is unknown", which))
	}
	if lKnown != rKnown {
		// Only one side can answer the skills question, so the comparison cannot
		// answer it either. Which side is named: "one of these runs" would leave
		// the reader to guess which, and the guess that matters -- that the
		// silent one loaded nothing -- is the wrong one.
		which := label(right)
		if !lKnown {
			which = label(left)
		}
		out = append(out, fmt.Sprintf(
			"run %s recorded no agent configuration (it predates the harness storing one), "+
				"so whether the two runs loaded the same skills is unknown", which))
	}
	return out
}

// sameModels reports whether two runs were served the same model.
//
// trace.SameModel rather than string equality: the two sides are read from two
// separate streams, and nothing makes those spell a model the same way. An
// alias on one side and its resolved name on the other is the same model, and
// calling it a difference would put a false warning on an ordinary pair.
func sameModels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !trace.SameModel(a[i], b[i]) {
			return false
		}
	}
	return true
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
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

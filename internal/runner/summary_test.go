package runner

import (
	"testing"

	"github.com/skyitachi/agenteval/internal/store"
)

// TestSummarizeSeparatesVerdictsFromMeasurements pins the four-way split the
// whole harness turns on: what was scored, what was merely measured, what ran
// out of budget, and what could not be measured at all. Folding any two of
// these together is how a broken harness comes to look like a bad agent.
func TestSummarizeSeparatesVerdictsFromMeasurements(t *testing.T) {
	attempts := []store.Attempt{
		// Graded and passed.
		{TaskID: "a", Status: store.CellOK, Graded: true, Passed: true,
			InputTokens: 10, OutputTokens: 5, CostUSD: 0.01, DurationMs: 100},
		// Measured, with nothing to score it against: a collected session.
		{TaskID: "b", Status: store.CellOK, InputTokens: 20, DurationMs: 200},
		// Ran out of budget. Data about the agent, not a harness failure.
		{TaskID: "c", Status: store.CellTimeout, InputTokens: 30, DurationMs: 300},
		// The harness could not measure it at all.
		{TaskID: "d", Status: store.CellError, Error: "no such binary", DurationMs: 400},
	}

	s := Summarize(attempts, 4)

	if s.Tasks != 4 || s.Cells != 4 {
		t.Errorf("tasks/cells = %d/%d, want 4/4", s.Tasks, s.Cells)
	}
	if s.Graded != 1 || s.Passed != 1 {
		t.Errorf("graded/passed = %d/%d, want 1/1", s.Graded, s.Passed)
	}
	if s.Ungraded != 1 {
		t.Errorf("ungraded = %d, want 1: a timeout and an error are not 'measured and unscored'", s.Ungraded)
	}
	if s.Timeouts != 1 {
		t.Errorf("timeouts = %d, want 1", s.Timeouts)
	}
	if s.HarnessErrors != 1 {
		t.Errorf("harness errors = %d, want 1", s.HarnessErrors)
	}
	if rate, ok := s.PassRate(); !ok || rate != 1 {
		t.Errorf("pass rate = %v (ok=%v), want 1 over the one graded cell", rate, ok)
	}
	// Spend is a fact about every cell that ran, whichever way it ended.
	if s.Tokens != 65 {
		t.Errorf("tokens = %d, want 65", s.Tokens)
	}
	if s.CostUSD != 0.01 {
		t.Errorf("cost = %v, want 0.01", s.CostUSD)
	}
}

func TestPassRateIsUndefinedWithoutAGrader(t *testing.T) {
	s := Summarize([]store.Attempt{{TaskID: "a", Status: store.CellOK}}, 1)
	if rate, ok := s.PassRate(); ok {
		t.Errorf("pass rate = %v on a run with nothing graded; an ungraded cell has no verdict to count", rate)
	}
}

// TestSummarizeOnlyComparesCellsItMeasured pins the rule that a cell the
// harness failed to measure contributes no baseline comparison -- otherwise a
// run that never started can report itself as cheaper than the recording.
func TestSummarizeOnlyComparesCellsItMeasured(t *testing.T) {
	withBaseline := func(a store.Attempt) store.Attempt {
		a.Baseline = store.BaselineComparison{Present: true, ID: "sess", Tokens: 900, Ms: 5000}
		return a
	}
	attempts := []store.Attempt{
		withBaseline(store.Attempt{TaskID: "a", Status: store.CellOK, InputTokens: 100, DurationMs: 1000}),
		withBaseline(store.Attempt{TaskID: "b", Status: store.CellTimeout, InputTokens: 50, DurationMs: 2000}),
		withBaseline(store.Attempt{TaskID: "c", Status: store.CellError, DurationMs: 30}),
	}

	s := Summarize(attempts, 3)

	if s.BaselineCells != 2 {
		t.Errorf("baseline cells = %d, want 2: a harness error has nothing to compare", s.BaselineCells)
	}
	if s.TokenCells != 2 || s.TimedCells != 2 {
		t.Errorf("token/time cells = %d/%d, want 2/2: this baseline recorded both",
			s.TokenCells, s.TimedCells)
	}
	if want := (100 - 900) + (50 - 900); s.TokenDelta != want {
		t.Errorf("token delta = %d, want %d", s.TokenDelta, want)
	}
	if want := int64((1000 - 5000) + (2000 - 5000)); s.TimeDeltaMs != want {
		t.Errorf("time delta = %d, want %d", s.TimeDeltaMs, want)
	}
}

// TestSummarizeWithholdsADeltaTheBaselineNeverRecorded pins the other half of
// the rule. A recording that captured tokens but no busy time has a baseline
// and nothing to subtract from its time column: an unattended attempt always
// spends wall clock, so folding in an unmeasured zero would report the whole
// of it as a regression and print something like "+23.4s" beside a session
// nobody ever timed.
func TestSummarizeWithholdsADeltaTheBaselineNeverRecorded(t *testing.T) {
	attempts := []store.Attempt{{
		TaskID: "a", Status: store.CellOK, InputTokens: 100, DurationMs: 23400,
		Baseline: store.BaselineComparison{Present: true, ID: "sess", Tokens: 900},
	}}

	s := Summarize(attempts, 1)

	if s.BaselineCells != 1 {
		t.Fatalf("baseline cells = %d, want 1: there is a baseline, it is just partial", s.BaselineCells)
	}
	if s.TokenCells != 1 || s.TokenDelta != 100-900 {
		t.Errorf("token comparison = (%d cells, %d), want (1, %d): the recording did carry tokens",
			s.TokenCells, s.TokenDelta, 100-900)
	}
	if s.TimedCells != 0 || s.TimeDeltaMs != 0 {
		t.Errorf("time comparison = (%d cells, %d ms), want (0, 0): the recording never timed itself",
			s.TimedCells, s.TimeDeltaMs)
	}
}

func TestSummarizeOfNothingIsZeroAndNotACrash(t *testing.T) {
	s := Summarize(nil, 0)
	if s.Cells != 0 || s.Tokens != 0 || s.BaselineCells != 0 {
		t.Errorf("summary of an empty run = %+v, want zeroes", s)
	}
}

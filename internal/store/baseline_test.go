package store

import "testing"

// The baseline comparison is the one number in a collected-session run that a
// reader is tempted to take at face value, so which cells are allowed to
// produce it is worth pinning down.
func TestComparableBaselineExcludesUnmeasuredCells(t *testing.T) {
	base := sampleAttempt("r1", "t1")

	cases := []struct {
		name    string
		mut     func(*Attempt)
		ok      bool // the cell has a baseline it may be compared against
		tokenOK bool // and that baseline carried a token total
		timeOK  bool // and that baseline carried a busy time
		delta   int
		ms      int64
	}{
		{
			name: "no baseline declared",
			mut:  func(a *Attempt) { a.Baseline = BaselineComparison{} },
			ok:   false,
		},
		{
			name: "measured",
			// 126 tokens against a 9000-token recording.
			ok: true, tokenOK: true, timeOK: true,
			delta: 126 - 9000, ms: 1234 - 60000,
		},
		{
			name: "ran out of budget",
			// A timeout is data about the agent: it spent this budget and still
			// did not finish, which is exactly what belongs beside the recording.
			mut: func(a *Attempt) { a.Status = CellTimeout },
			ok:  true, tokenOK: true, timeOK: true,
			delta: 126 - 9000, ms: 1234 - 60000,
		},
		{
			name: "the harness could not measure it",
			// Nothing ran, so "280 tokens cheaper" would be arithmetic without a
			// subject: a broken harness must not be able to look efficient.
			mut: func(a *Attempt) { a.Status = CellError },
			ok:  false,
		},
		{
			name: "the recording captured nothing",
			// A session recorded before the cost-state records were read leaves
			// both totals at zero. Zero is the absence of a measurement, so
			// there is a baseline but nothing to subtract from it.
			mut: func(a *Attempt) { a.Baseline.Tokens, a.Baseline.Ms = 0, 0 },
			ok:  true,
		},
		{
			name: "the recording captured tokens but never timed itself",
			// This is the shape that printed "+23.4s": an attempt always spends
			// wall clock, so subtracting an unmeasured zero presents the whole
			// of it as a regression.
			mut: func(a *Attempt) { a.Baseline.Ms = 0 },
			ok:  true, tokenOK: true,
			delta: 126 - 9000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			if tc.mut != nil {
				tc.mut(&a)
			}
			got, ok := a.ComparableBaseline()
			if ok != tc.ok {
				t.Fatalf("ComparableBaseline ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.ID != "sess-1" {
				t.Errorf("baseline id = %q, want sess-1", got.ID)
			}
			if d, has := a.TokenDelta(); d != tc.delta || has != tc.tokenOK {
				t.Errorf("TokenDelta = (%d, %v), want (%d, %v)", d, has, tc.delta, tc.tokenOK)
			}
			if d, has := a.MsDelta(); d != tc.ms || has != tc.timeOK {
				t.Errorf("MsDelta = (%d, %v), want (%d, %v)", d, has, tc.ms, tc.timeOK)
			}
		})
	}
}

func TestTokenTotalAddsEveryClass(t *testing.T) {
	a := Attempt{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreateTokens: 4}
	if got := a.TokenTotal(); got != 10 {
		t.Errorf("TokenTotal = %d, want 10", got)
	}
}

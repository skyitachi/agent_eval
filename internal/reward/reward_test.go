package reward

import (
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
)

func grade(passed bool, score float64) *grader.Grade {
	return &grader.Grade{Passed: passed, Score: score}
}

func TestResolveDefaultsToDense(t *testing.T) {
	// A dataset written before the reward contract existed has no block at all,
	// and must keep behaving exactly as it did.
	for _, spec := range []*dataset.RewardSpec{nil, {Type: ""}} {
		got, err := Resolve(spec)
		if err != nil {
			t.Fatalf("Resolve(%+v): %v", spec, err)
		}
		if got.Kind != KindScore {
			t.Errorf("Resolve(%+v).Kind = %q, want %q", spec, got.Kind, KindScore)
		}
	}
}

func TestResolveReadsTheDeclaredDensity(t *testing.T) {
	got, err := Resolve(&dataset.RewardSpec{Type: dataset.RewardPass})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != KindPass {
		t.Errorf("Kind = %q, want %q", got.Kind, KindPass)
	}
}

func TestResolveRejectsAnUnknownDensity(t *testing.T) {
	// Guessing here would silently train against the wrong signal, so an
	// unrecognised value has to be an error rather than a fallback.
	if _, err := Resolve(&dataset.RewardSpec{Type: "dense-ish"}); err == nil {
		t.Fatal("expected an error for an unknown reward type")
	}
}

func TestEvaluateDense(t *testing.T) {
	spec := Spec{Kind: KindScore}
	cases := []struct {
		name     string
		grade    *grader.Grade
		rejected bool
		want     float64
	}{
		{"passing attempt", grade(true, 1), false, 1},
		{"partial credit", grade(false, 0.4), false, 0.4},
		{"failing attempt", grade(false, 0), false, 0},
		{"no grade at all", nil, false, 0},
		// Guards decide the reward, not the graders: a rejected attempt must
		// never be paid, or tampering with the verifier becomes profitable.
		{"rejected despite full marks", grade(true, 1), true, 0},
		{"rejected with partial credit", grade(false, 0.7), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := spec.Evaluate(tc.grade, tc.rejected, "reason")
			if v.Value != tc.want {
				t.Errorf("Value = %v, want %v", v.Value, tc.want)
			}
			if v.Rejected != tc.rejected {
				t.Errorf("Rejected = %v, want %v", v.Rejected, tc.rejected)
			}
			if v.Kind != KindScore {
				t.Errorf("Kind = %q", v.Kind)
			}
		})
	}
}

func TestEvaluateSparse(t *testing.T) {
	spec := Spec{Kind: KindPass}
	cases := []struct {
		name     string
		grade    *grader.Grade
		rejected bool
		want     float64
	}{
		{"passing attempt", grade(true, 1), false, 1},
		// The whole difference between the densities: partial credit is not paid.
		{"partial credit", grade(false, 0.9), false, 0},
		{"failing attempt", grade(false, 0), false, 0},
		{"no grade at all", nil, false, 0},
		{"rejected", grade(true, 1), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spec.Evaluate(tc.grade, tc.rejected, "").Value; got != tc.want {
				t.Errorf("Value = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluateClampsOutOfRangeScores(t *testing.T) {
	// A custom grader can return anything. A reward outside [0,1] would let one
	// task dominate a batch, so it is clamped rather than trusted.
	spec := Spec{Kind: KindScore}
	for _, tc := range []struct{ score, want float64 }{
		{-1, 0}, {1.5, 1}, {0.5, 0.5},
	} {
		if got := spec.Evaluate(grade(false, tc.score), false, "").Value; got != tc.want {
			t.Errorf("score %v -> reward %v, want %v", tc.score, got, tc.want)
		}
	}
}

func TestVerdictCarriesTheReason(t *testing.T) {
	v := Spec{Kind: KindScore}.Evaluate(grade(true, 1), true, "forbidden_path_changed: tests/x.py")
	if v.Reason == "" {
		t.Error("a rejected verdict must carry the reason forward: it is what makes the zero explainable")
	}
}

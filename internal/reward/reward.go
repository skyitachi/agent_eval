// Package reward projects a graded attempt onto the scalar signal that
// reinforcement learning optimises.
//
// # Why this is separate from the grade
//
// A grade answers "did this attempt satisfy the task?" and is deliberately rich:
// per-grader verdicts, partial credit, weights, thresholds. A reward answers a
// narrower question -- "what number does the trainer get?" -- and it has to be
// narrow, because everything downstream will optimise exactly that number and
// ignore everything else.
//
// Keeping the two apart is what makes the failure mode visible. A reward that is
// only implied by grader_threshold cannot be audited: a reader of a trajectory
// has no way to tell whether 0.7 meant "most of the task done" or "the task
// failed and 0.7 is the partial credit the graders happen to report". Spelling
// out the density makes that question answerable from the dataset alone.
//
// # The two densities
//
// KindScore rewards partial progress. It gives a gradient on nearly every
// attempt, which is what makes long-horizon tasks learnable at all, and it pays
// for that with a reward whose shape the task author did not choose -- the
// grader weights become the reward function whether or not anyone meant them to
// be. KindPass is the opposite trade: a single bit, slow to learn from, but
// almost impossible to farm.
package reward

import (
	"fmt"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
)

// Kind is the resolved density of the reward signal.
type Kind string

const (
	// KindScore is the dense weighted score in [0,1].
	KindScore Kind = Kind(dataset.RewardScore)
	// KindPass is the sparse 0/1 success indicator.
	KindPass Kind = Kind(dataset.RewardPass)
)

// Spec is a resolved reward contract.
type Spec struct {
	Kind Kind `json:"kind"`
}

// Resolve validates a declared reward contract.
func Resolve(r *dataset.RewardSpec) (Spec, error) {
	switch typ := r.EffectiveType(); typ {
	case dataset.RewardScore:
		return Spec{Kind: KindScore}, nil
	case dataset.RewardPass:
		return Spec{Kind: KindPass}, nil
	default:
		return Spec{}, fmt.Errorf("unknown reward type %q (want %s|%s)",
			typ, dataset.RewardScore, dataset.RewardPass)
	}
}

// Verdict is the reward for one attempt, with enough provenance to explain a
// zero that did not come from the score.
type Verdict struct {
	// Value is the scalar handed to the trainer, always in [0,1].
	Value float64 `json:"value"`
	// Kind is the density that produced it.
	Kind Kind `json:"kind"`
	// Rejected is set when an anti-gaming guard, not the grader, decided the
	// outcome. It is reported separately because "the model failed the task" and
	// "the model tampered with the verifier" must not look the same in a
	// training log: one is a capability signal, the other is a safety signal.
	Rejected bool `json:"rejected,omitempty"`
	// Reason names the guard that fired.
	Reason string `json:"reason,omitempty"`
}

// Evaluate computes the reward for one graded attempt.
//
// rejected must be set when a guard invalidated the attempt. A rejected attempt
// pays zero under every density, including the dense one, and that is the whole
// point: if tampering with the verifier still produced a partial reward, the
// policy would be optimising towards tampering, since it is the cheapest way to
// move the number. Callers also mark the attempt as failed; zeroing here keeps
// the reward view and the verdict view consistent instead of leaving the reward
// as the one place where a rejected attempt still looks successful.
func (s Spec) Evaluate(g *grader.Grade, rejected bool, reason string) Verdict {
	v := Verdict{Kind: s.kindOrDefault()}
	if rejected {
		v.Rejected = true
		v.Reason = reason
		return v
	}
	if g == nil {
		// No grade means the attempt never produced a verdict -- a harness
		// failure. Zero is the honest value, and the reason is carried by the
		// cell's error rather than invented here.
		return v
	}
	if v.Kind == KindPass {
		if g.Passed {
			v.Value = 1
		}
		return v
	}
	v.Value = clamp01(g.Score)
	return v
}

// kindOrDefault makes the zero Spec behave as the dense score, so a caller that
// failed to resolve a contract still produces a sensible number rather than a
// silent all-zero reward.
func (s Spec) kindOrDefault() Kind {
	if s.Kind == "" {
		return KindScore
	}
	return s.Kind
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

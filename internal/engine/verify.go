package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

// Verdict is the outcome of grading one workspace.
//
// It is deliberately a plain value rather than a *TaskResult: a TaskResult also
// carries identity, timing and status, which belong to whoever decided to run
// the cell. Two very different callers produce a workspace and need it graded --
// the orchestrator, for every cell of a run, and the RL environment service, for
// every rollout a trainer asks for -- and if they each did their own grading the
// reward contract would exist in two places and drift.
type Verdict struct {
	Grade *grader.Grade

	// Diff and ChangedPaths describe what the workspace's occupant left behind,
	// after the guards and the task's own output filters have been applied.
	Diff         string
	ChangedPaths []string

	// Violations lists the anti-gaming guards that fired; non-empty means the
	// attempt is rejected, and Reward is then zero whatever the grade says.
	Violations []string
	// Restored lists paths reset to the baseline before grading.
	Restored []string

	// Reward is the scalar training signal, projected through the task's reward
	// contract once the verdict is final.
	Reward     float64
	RewardKind string

	// Errors lists what went wrong on the harness side badly enough that no
	// verdict was produced. A non-empty list means the attempt is a harness
	// failure, which is a different statement from "the attempt failed".
	Errors []string
	// Warnings lists problems that cost evidence but not the verdict: a diff
	// that could not be computed, a guard list that could not be read. They are
	// reported, and they never turn a graded attempt into a failed one.
	Warnings []string
}

// Passed reports whether the attempt satisfied every required grader and no
// guard rejected it.
func (v Verdict) Passed() bool { return v.Grade != nil && v.Grade.Passed }

// Score is the weighted grader score, zero when grading never happened.
func (v Verdict) Score() float64 {
	if v.Grade == nil {
		return 0
	}
	return v.Grade.Score
}

// Verify grades a workspace and projects the reward.
//
// The sequence is fixed and must stay that way:
//
//  1. list the raw changed set and run the guards, because a guard can reset
//     files and therefore changes what there is to grade;
//  2. take the diff through the grader context, which applies the task's output
//     filters on top of the paths the guards already neutralised;
//  3. grade;
//  4. fold guard violations into Passed, so every downstream metric inherits the
//     rejection without having to know about guards;
//  5. project the reward from the now-final verdict.
//
// Steps 4 and 5 are ordered for a reason. Computing the reward before the
// violation is folded in would pay a rejected attempt whatever its score, and a
// policy that can be paid for tampering with the verifier has an incentive to
// tamper with the verifier.
func (e *Engine) Verify(ctx context.Context, t dataset.TaskSpec, sb sandbox.Sandbox,
	baseline *sandbox.Baseline, attempt *agent.Attempt, enforceGuards bool) Verdict {

	var v Verdict
	if enforceGuards {
		e.applyGuards(ctx, baseline, t, &v)
	}

	gc := grader.NewContext(t, sb, baseline, attempt)
	if diff, err := gc.Diff(); err != nil {
		v.Warnings = append(v.Warnings, fmt.Sprintf("compute diff: %v", err))
	} else {
		v.Diff = diff
	}
	if changed, err := gc.ChangedPaths(); err == nil {
		v.ChangedPaths = changed
	}

	grade, err := grader.RunTask(ctx, gc, e.cfg.GraderThreshold)
	switch {
	case err != nil:
		v.Errors = append(v.Errors, fmt.Sprintf("grade: %v", err))
	default:
		if len(v.Violations) > 0 {
			// A guard that fired decides the verdict whatever the graders said.
			// Folding it into Passed rather than inventing a separate status is
			// what keeps resolve_rate, pass^k and every cost-per-resolved figure
			// honest without teaching each of them about guards.
			grade.Passed = false
		}
		v.Grade = grade
		if len(grade.Errors) > 0 {
			// A grader that errored could not produce a verdict. Leaving the
			// grade in place keeps the evidence while Errors flags it as
			// unreliable, and grader_error_rate makes it visible in aggregate.
			v.Errors = append(v.Errors, "graders errored: "+strings.Join(grade.Errors, "; "))
		}
	}

	// The reward is projected once the verdict is final, so that a rejected
	// attempt cannot pay out a partial reward.
	eval := e.rewardSpec(t).Evaluate(v.Grade, len(v.Violations) > 0, strings.Join(v.Violations, "; "))
	v.Reward, v.RewardKind = eval.Value, string(eval.Kind)
	return v
}

// applyGuards enforces a task's anti-gaming path lists.
//
// It writes into a *Verdict rather than a *TaskResult so that the orchestrator
// and the RL environment service share one implementation. The guards are the
// part of the reward contract that decides whether a measurement is admissible
// at all, and two copies of that rule would eventually disagree.
func (e *Engine) applyGuards(ctx context.Context, baseline *sandbox.Baseline,
	t dataset.TaskSpec, v *Verdict) {
	if len(t.ForbidChangedPaths) == 0 && len(t.RestorePaths) == 0 {
		return
	}
	// The raw changed set, deliberately without the task's output filters: a task
	// that narrows its graded diff to src/** must still be able to notice that
	// tests/ was rewritten.
	changed, err := baseline.ChangedPaths(ctx, sandbox.DiffOptions{IgnorePaths: grader.HarnessIgnore})
	if err != nil {
		v.Warnings = append(v.Warnings, fmt.Sprintf("guards: list changed paths: %v", err))
		return
	}
	if len(t.ForbidChangedPaths) > 0 {
		if hits := sandbox.FilterPaths(changed, t.ForbidChangedPaths, nil); len(hits) > 0 {
			v.Violations = append(v.Violations,
				fmt.Sprintf("forbidden_path_changed: %s", strings.Join(hits, ", ")))
		}
	}
	if len(t.RestorePaths) > 0 {
		restored, removed, rerr := baseline.RestorePaths(ctx, t.RestorePaths)
		if rerr != nil {
			v.Warnings = append(v.Warnings, fmt.Sprintf("guards: restore paths: %v", rerr))
		}
		v.Restored = append(append([]string{}, restored...), removed...)
		sort.Strings(v.Restored)
	}
}

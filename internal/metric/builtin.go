package metric

import "strings"

// Built-in metrics.
//
// Naming follows the convention used across agent and code benchmarks so that a
// number can be read without consulting this file:
//
//	*_rate          a fraction in [0,1]
//	mean_*, p50_*, p95_*   distribution summaries over samples
//	*_count         an absolute count
//	per_resolved    normalised by the number of fully passing attempts, which is
//	                the only denominator that makes cost comparable across runs
//	                with different success rates.
func init() {
	registerRun("resolve_rate", UnitRatio,
		"Fraction of attempts that fully passed. The headline number, equivalent to SWE-bench's resolved rate.",
		func(a *Aggregate) (float64, map[string]any, error) {
			if len(a.Sample) == 0 {
				return 0, nil, nil
			}
			var passed int
			for i := range a.Sample {
				if a.Sample[i].Passed() {
					passed++
				}
			}
			return float64(passed) / float64(len(a.Sample)), map[string]any{
				"passed": passed, "total": len(a.Sample),
			}, nil
		})

	registerRun("mean_score", UnitRatio,
		"Mean weighted grader score, preserving partial credit that resolve_rate discards.",
		func(a *Aggregate) (float64, map[string]any, error) {
			if len(a.Sample) == 0 {
				return 0, nil, nil
			}
			scores := make([]float64, 0, len(a.Sample))
			for i := range a.Sample {
				scores = append(scores, a.Sample[i].Score())
			}
			return mean(scores), nil, nil
		})

	registerRun("pass_at_k", UnitRatio,
		"Unbiased pass@k estimator (Chen et al. 2021): probability that at least one of k samples of a task succeeds.",
		func(a *Aggregate) (float64, map[string]any, error) {
			k := a.K()
			if k <= 0 {
				return 0, nil, nil
			}
			groups := a.ByTask()
			perTask := map[string]float64{}
			var sum float64
			for id, group := range groups {
				c := 0
				for _, s := range group {
					if s.Passed() {
						c++
					}
				}
				v := PassAtK(len(group), c, k)
				perTask[id] = v
				sum += v
			}
			if len(groups) == 0 {
				return 0, nil, nil
			}
			return sum / float64(len(groups)), map[string]any{
				"k": k, "per_task": perTask,
			}, nil
		})

	registerRun("pass_hat_k", UnitRatio,
		"pass^k (tau-bench convention): estimated probability of succeeding k times in a row, averaged over tasks. Always <= pass@k.",
		func(a *Aggregate) (float64, map[string]any, error) {
			k := a.K()
			if k <= 0 {
				return 0, nil, nil
			}
			groups := a.ByTask()
			if len(groups) == 0 {
				return 0, nil, nil
			}
			var sum float64
			for _, group := range groups {
				successes := 0
				for _, s := range group {
					if s.Passed() {
						successes++
					}
				}
				sum += PassHatK(successes, len(group), k)
			}
			return sum / float64(len(groups)), map[string]any{"k": k, "tasks": len(groups)}, nil
		})

	registerRun("task_count", UnitCount,
		"Number of distinct tasks in the run.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return float64(len(a.ByTask())), nil, nil
		})

	// ---------------------------------------------------------------------
	// Reward
	//
	// reward is not score. They coincide only while every task uses the dense
	// density and no guard fires. These three metrics exist so that a run whose
	// reward was quietly zero -- because the density is sparse and nothing
	// passed, or because a guard rejected the attempts -- is visible without
	// reading every cell.
	// ---------------------------------------------------------------------

	registerRun("mean_reward", UnitRatio,
		"Mean reward per attempt: the average scalar a trainer would have received. Differs from mean_score whenever a task's reward contract is sparse or an attempt was rejected.",
		func(a *Aggregate) (float64, map[string]any, error) {
			if len(a.Sample) == 0 {
				return 0, nil, nil
			}
			var sum float64
			for i := range a.Sample {
				// Rejected and never-started cells count, with their zero. They
				// are the cells whose reward the policy must actually learn to
				// avoid, so excluding them would report the reward of the
				// subsets that behaved -- precisely the number a reward-hacking
				// policy would prefer to be judged on.
				sum += a.Sample[i].Reward
			}
			return sum / float64(len(a.Sample)), map[string]any{
				"total": sum, "samples": len(a.Sample),
			}, nil
		})

	registerRun("reward_signal_rate", UnitRatio,
		"Fraction of attempts with a non-zero reward. At or near zero the run carries no learning signal, however good its score looks.",
		func(a *Aggregate) (float64, map[string]any, error) {
			if len(a.Sample) == 0 {
				return 0, nil, nil
			}
			var n int
			for i := range a.Sample {
				if a.Sample[i].Reward != 0 {
					n++
				}
			}
			return float64(n) / float64(len(a.Sample)), map[string]any{"non_zero": n}, nil
		})

	registerRun("guard_violation_rate", UnitRatio,
		"Fraction of attempts rejected by an anti-gaming guard. Non-zero means part of the reward is measuring compliance rather than capability.",
		func(a *Aggregate) (float64, map[string]any, error) {
			if len(a.Sample) == 0 {
				return 0, nil, nil
			}
			var n int
			reasons := map[string]int{}
			for i := range a.Sample {
				s := &a.Sample[i]
				if !s.Rejected() {
					continue
				}
				n++
				for _, v := range s.Violations {
					reasons[guardReason(v)]++
				}
			}
			return float64(n) / float64(len(a.Sample)), map[string]any{
				"rejected": n, "by_kind": reasons,
			}, nil
		})

	registerRun("sample_count", UnitCount,
		"Number of graded attempts. Exceeds task_count when sampling more than once per task.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return float64(len(a.Sample)), nil, nil
		})

	registerRun("resolved_count", UnitCount,
		"Number of attempts that fully passed.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var n int
			for i := range a.Sample {
				if a.Sample[i].Passed() {
					n++
				}
			}
			return float64(n), nil, nil
		})

	// ---------------------------------------------------------------------
	// Cost
	// ---------------------------------------------------------------------

	registerRun("total_cost_usd", UnitUSD,
		"Sum of reported cost across all attempts.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var sum float64
			for _, s := range a.Sample {
				if s.Attempt != nil {
					sum += s.Attempt.CostUSD()
				}
			}
			return sum, nil, nil
		})

	registerRun("mean_cost_usd", UnitUSD,
		"Mean reported cost per attempt.",
		func(a *Aggregate) (float64, map[string]any, error) {
			vals := collect(a, func(s *Sample) (float64, bool) {
				if s.Attempt == nil {
					return 0, false
				}
				return s.Attempt.CostUSD(), true
			})
			return mean(vals), nil, nil
		})

	registerRun("cost_per_resolved_usd", UnitUSD,
		"Cost divided by the number of fully passing attempts. Zero when nothing passed; see extra.reason.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var cost float64
			var resolved int
			for i := range a.Sample {
				if a.Sample[i].Attempt != nil {
					cost += a.Sample[i].Attempt.CostUSD()
				}
				if a.Sample[i].Passed() {
					resolved++
				}
			}
			if resolved == 0 {
				return 0, map[string]any{
					"reason":   "no attempt fully passed, so cost per resolved is undefined",
					"cost":     cost,
					"resolved": 0,
				}, nil
			}
			return cost / float64(resolved), map[string]any{"cost": cost, "resolved": resolved}, nil
		})

	// ---------------------------------------------------------------------
	// Tokens
	// ---------------------------------------------------------------------

	for _, spec := range []struct {
		name, desc string
		pick       func(s *Sample) (float64, bool)
	}{
		{"mean_input_tokens", "Mean input tokens per attempt.",
			tokenPicker(func(u usageView) float64 { return float64(u.input) })},
		{"mean_output_tokens", "Mean output tokens per attempt.",
			tokenPicker(func(u usageView) float64 { return float64(u.output) })},
		{"mean_cache_read_tokens", "Mean cache-read input tokens per attempt, a proxy for prompt-cache effectiveness.",
			tokenPicker(func(u usageView) float64 { return float64(u.cacheRead) })},
		{"mean_total_tokens", "Mean total tokens (input + output + cache) per attempt.",
			tokenPicker(func(u usageView) float64 { return float64(u.total) })},
	} {
		pick := spec.pick
		registerRun(spec.name, UnitTokens, spec.desc,
			func(a *Aggregate) (float64, map[string]any, error) {
				return mean(collect(a, pick)), nil, nil
			})
	}

	registerRun("tokens_per_resolved", UnitTokens,
		"Tokens divided by fully passing attempts. Zero when nothing passed; see extra.reason.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var tokens float64
			var resolved int
			for i := range a.Sample {
				if u, ok := totalTokens(&a.Sample[i]); ok {
					tokens += float64(u)
				}
				if a.Sample[i].Passed() {
					resolved++
				}
			}
			if resolved == 0 {
				return 0, map[string]any{
					"reason":   "no attempt fully passed, so tokens per resolved is undefined",
					"tokens":   tokens,
					"resolved": 0,
				}, nil
			}
			return tokens / float64(resolved), map[string]any{"tokens": tokens, "resolved": resolved}, nil
		})

	// ---------------------------------------------------------------------
	// Effort: turns, tool calls
	// ---------------------------------------------------------------------

	registerRun("mean_turns", UnitCount,
		"Mean assistant turns per attempt. A rising value often means the agent is looping.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return mean(collect(a, pickTurns)), nil, nil
		})

	registerRun("p95_turns", UnitCount,
		"95th percentile of turns per attempt, exposing long-tail thrashing.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return percentile(sortedCopy(collect(a, pickTurns)), 95), nil, nil
		})

	registerRun("mean_tool_calls", UnitCount,
		"Mean number of tool invocations per attempt.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return mean(collect(a, pickToolCalls)), nil, nil
		})

	registerRun("tool_call_error_rate", UnitRatio,
		"Fraction of tool invocations that returned an error, across all attempts.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var calls, errs int
			for i := range a.Sample {
				if a.Sample[i].Attempt == nil || a.Sample[i].Attempt.Trace == nil {
					continue
				}
				calls += len(a.Sample[i].Attempt.Trace.ToolCalls)
				errs += a.Sample[i].Attempt.Trace.ToolErrorCount()
			}
			if calls == 0 {
				return 0, map[string]any{"tool_calls": 0}, nil
			}
			return float64(errs) / float64(calls), map[string]any{
				"tool_calls": calls, "tool_errors": errs,
			}, nil
		})

	// ---------------------------------------------------------------------
	// Latency
	// ---------------------------------------------------------------------

	for _, spec := range []struct {
		name string
		p    float64
		desc string
	}{
		{"mean_latency_ms", 0, "Mean wall-clock duration of an attempt."},
		{"p50_latency_ms", 50, "Median wall-clock duration of an attempt."},
		{"p95_latency_ms", 95, "95th percentile wall-clock duration, the user-visible tail."},
	} {
		spec := spec
		registerRun(spec.name, UnitMs, spec.desc,
			func(a *Aggregate) (float64, map[string]any, error) {
				vals := sortedCopy(collect(a, pickLatency))
				if spec.p == 0 {
					return mean(vals), nil, nil
				}
				return percentile(vals, spec.p), nil, nil
			})
	}

	for _, spec := range []struct {
		name string
		p    float64
		desc string
	}{
		{"mean_ttft_ms", 0, "Mean time to first token."},
		{"p95_ttft_ms", 95, "95th percentile time to first token."},
	} {
		spec := spec
		registerRun(spec.name, UnitMs, spec.desc,
			func(a *Aggregate) (float64, map[string]any, error) {
				vals := sortedCopy(collect(a, pickTTFT))
				if spec.p == 0 {
					return mean(vals), nil, nil
				}
				return percentile(vals, spec.p), nil, nil
			})
	}

	registerRun("total_latency_ms", UnitMs,
		"Sum of attempt durations; the serial cost of the run.",
		func(a *Aggregate) (float64, map[string]any, error) {
			var sum float64
			for _, s := range collect(a, pickLatency) {
				sum += s
			}
			return sum, nil, nil
		})

	// ---------------------------------------------------------------------
	// Degenerate-outcome rates
	//
	// These exist so a headline resolve rate cannot hide a run that mostly
	// consisted of timeouts or crashes. A 60% resolve rate built on 30% timeouts
	// is a different result from a clean 60%.
	//
	// They are derived from the stop reason rather than from fields on the
	// attempt. That matters: when the agent never starts there is no attempt at
	// all, so an attempt-based predicate reports 0 for precisely the case
	// harness_error_rate exists to measure. The stop reason is the one
	// normalised vocabulary that covers both a real attempt and its absence, and
	// the engine is responsible for keeping it truthful.
	// ---------------------------------------------------------------------

	for _, spec := range []struct {
		name, desc string
		reasons    []string
	}{
		{"timeout_rate", "Fraction of cells killed by a timeout, whether the agent's own or the task budget.", []string{
			"timeout", "task_timeout",
		}},
		{"max_turns_rate", "Fraction of cells stopped because the turn budget ran out.", []string{
			"max_turns",
		}},
		{"harness_error_rate", "Fraction of cells the harness failed to measure: the agent never started, or produced no usable stream.", []string{
			"harness_error", "no_attempt", "no_trace",
		}},
		{"incomplete_rate", "Fraction of cells whose event stream ended without a terminal result.", []string{
			"incomplete",
		}},
		{"grader_error_rate", "Fraction of cells with at least one grader infrastructure error; these verdicts are unreliable.", nil},
	} {
		spec := spec
		registerRun(spec.name, UnitRatio, spec.desc,
			func(a *Aggregate) (float64, map[string]any, error) {
				if len(a.Sample) == 0 {
					return 0, nil, nil
				}
				var n int
				for i := range a.Sample {
					s := &a.Sample[i]
					var hit bool
					if spec.reasons == nil {
						// grader_error_rate is about the grading step, not about
						// how the attempt ended.
						hit = s.Grade != nil && len(s.Grade.Errors) > 0
					} else {
						hit = sampleEndedForReason(s, spec.reasons)
					}
					if hit {
						n++
					}
				}
				return float64(n) / float64(len(a.Sample)), map[string]any{"count": n}, nil
			})
	}

	// ---------------------------------------------------------------------
	// Grouped breakdowns
	// ---------------------------------------------------------------------

	registerRun("resolve_rate_by_category", UnitRatio,
		"Resolve rate per task category, reported in Extra.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return groupedBreakdown(a, func(s *Sample) string { return s.Category })
		})

	registerRun("resolve_rate_by_difficulty", UnitRatio,
		"Resolve rate per difficulty label, reported in Extra.",
		func(a *Aggregate) (float64, map[string]any, error) {
			return groupedBreakdown(a, func(s *Sample) string { return s.Difficulty })
		})

	registerRun("mean_score_by_category", UnitRatio,
		"Mean grader score per task category, reported in Extra.",
		func(a *Aggregate) (float64, map[string]any, error) {
			groups := map[string][]float64{}
			for i := range a.Sample {
				k := a.Sample[i].Category
				if k == "" {
					k = "(uncategorised)"
				}
				groups[k] = append(groups[k], a.Sample[i].Score())
			}
			extra := map[string]any{}
			for k, vals := range groups {
				extra[k] = mean(vals)
			}
			return mean(collect(a, func(s *Sample) (float64, bool) { return s.Score(), true })), extra, nil
		})

	registerRun("stop_reason_mix", UnitText,
		"Distribution of stop reasons, reported in Extra.",
		func(a *Aggregate) (float64, map[string]any, error) {
			counts := map[string]int{}
			for i := range a.Sample {
				counts[a.Sample[i].StopReason()]++
			}
			extra := map[string]any{"total": len(a.Sample)}
			for k, v := range counts {
				extra[k] = v
			}
			return float64(len(counts)), extra, nil
		})
}

func registerRun(name string, unit Unit, desc string,
	fn func(*Aggregate) (float64, map[string]any, error)) {
	Register(&FuncMetric{Nm: name, K: KindRun, U: unit, Desc: desc, Fn: fn})
}

// groupedBreakdown computes a resolve rate per group and returns the overall
// rate as the headline value with the per-group split in Extra.
func groupedBreakdown(a *Aggregate, key func(*Sample) string) (float64, map[string]any, error) {
	type tally struct{ passed, total int }
	groups := map[string]*tally{}
	var passed, total int
	for i := range a.Sample {
		k := key(&a.Sample[i])
		if k == "" {
			k = "(unset)"
		}
		g, ok := groups[k]
		if !ok {
			g = &tally{}
			groups[k] = g
		}
		g.total++
		total++
		if a.Sample[i].Passed() {
			g.passed++
			passed++
		}
	}
	extra := map[string]any{}
	for k, g := range groups {
		extra[k] = map[string]any{
			"passed":       g.passed,
			"total":        g.total,
			"resolve_rate": safeDiv(float64(g.passed), float64(g.total)),
		}
	}
	if total == 0 {
		return 0, extra, nil
	}
	return float64(passed) / float64(total), extra, nil
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// guardReason strips a violation message down to its kind.
//
// Violations are stored as "kind: detail" (a list of paths can be long), so the
// by_kind breakdown stays a small, stable dictionary instead of one bucket per
// distinct path list.
func guardReason(v string) string {
	if i := strings.IndexByte(v, ':'); i > 0 {
		return v[:i]
	}
	if v == "" {
		return "(unknown)"
	}
	return v
}

// ---------------------------------------------------------------------------
// Field pickers
// ---------------------------------------------------------------------------

type usageView struct {
	input, output, cacheRead, cacheCreation, total int
}

func totalTokens(s *Sample) (int, bool) {
	if s.Attempt == nil || s.Attempt.Trace == nil {
		return 0, false
	}
	return s.Attempt.Trace.Result.Usage.Total(), true
}

func tokenPicker(pick func(usageView) float64) func(*Sample) (float64, bool) {
	return func(s *Sample) (float64, bool) {
		if s.Attempt == nil || s.Attempt.Trace == nil {
			return 0, false
		}
		u := s.Attempt.Trace.Result.Usage
		return pick(usageView{
			input: u.InputTokens, output: u.OutputTokens,
			cacheRead: u.CacheReadInputTokens, cacheCreation: u.CacheCreationInputTokens,
			total: u.Total(),
		}), true
	}
}

func pickTurns(s *Sample) (float64, bool) {
	if s.Attempt == nil || s.Attempt.Trace == nil {
		return 0, false
	}
	return float64(s.Attempt.Trace.Turns()), true
}

func pickToolCalls(s *Sample) (float64, bool) {
	if s.Attempt == nil || s.Attempt.Trace == nil {
		return 0, false
	}
	return float64(len(s.Attempt.Trace.ToolCalls)), true
}

func pickLatency(s *Sample) (float64, bool) {
	if s.Attempt == nil {
		return 0, false
	}
	return float64(s.Attempt.Duration.Milliseconds()), true
}

func pickTTFT(s *Sample) (float64, bool) {
	if s.Attempt == nil || s.Attempt.Trace == nil {
		return 0, false
	}
	return float64(s.Attempt.Trace.Result.TTFTMs), true
}

// collect gathers a numeric field across samples, skipping samples where the
// field is unavailable rather than treating "missing" as zero, which would bias
// every mean downward.
func collect(a *Aggregate, pick func(*Sample) (float64, bool)) []float64 {
	out := make([]float64, 0, len(a.Sample))
	for i := range a.Sample {
		if v, ok := pick(&a.Sample[i]); ok {
			out = append(out, v)
		}
	}
	return out
}

// sampleEndedForReason reports whether a cell ended for one of the given
// reasons.
//
// Reasons are matched against Sample.StopReason, which covers both a real
// attempt and its absence -- see the comment on the degenerate-outcome rates.
func sampleEndedForReason(s *Sample, reasons []string) bool {
	if len(reasons) == 0 {
		return false
	}
	got := s.StopReason()
	for _, r := range reasons {
		if got == r {
			return true
		}
	}
	return false
}

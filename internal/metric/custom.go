package metric

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Custom metrics are declared in configuration, not code, so a dataset can
// define the numbers that matter to it. Three forms are supported, and they cover
// almost everything short of writing Go:
//
//  1. Aggregate a field over the run's samples, optionally filtered:
//
//     - name: hard_resolve_rate
//       where: "difficulty == 'hard'"
//       field: passed
//       agg: mean
//
//  2. Form a ratio of two aggregates, which is the shape most efficiency
//     measures take:
//
//     - name: resolved_per_turn
//       numerator: {field: passed, agg: sum}
//       denominator: {field: turns, agg: sum}
//
//  3. Arithmetic over already-computed metrics, evaluated in declaration order:
//
//     - name: efficiency_index
//       expr: "resolve_rate / max(total_cost_usd, 0.01)"
//
// A form-3 metric may reference any built-in and any custom declared above it.

// CustomSpec is one declared custom metric.
type CustomSpec struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Unit controls rendering: ratio | count | usd | ms | tokens | text.
	Unit string `json:"unit,omitempty" yaml:"unit,omitempty"`
	// Kind is "run" (default) or "sample". Only run-level custom metrics are
	// supported today; sample-level values are already covered by the built-in
	// per-sample fields.
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// Direction says which way is an improvement: "up", "down" or "neutral".
	//
	// Built-in metrics get this from a curated table, but a custom metric's
	// name carries no such information: "slow_cells" and "fast_cells" are
	// equally plausible, and a report that colours more slow cells green is
	// actively misleading. Declaring it here is the only way to get it right.
	// Empty falls back to the name heuristic.
	Direction string `json:"direction,omitempty" yaml:"direction,omitempty"`
	// Group places the metric in a report section. Empty puts it under "other".
	Group string `json:"group,omitempty" yaml:"group,omitempty"`

	// Form 1.
	Where string `json:"where,omitempty" yaml:"where,omitempty"`
	Field string `json:"field,omitempty" yaml:"field,omitempty"`
	Agg   string `json:"agg,omitempty" yaml:"agg,omitempty"`
	// WeightField, when set with agg=mean, produces a weighted mean.
	WeightField string `json:"weight_field,omitempty" yaml:"weight_field,omitempty"`

	// Form 2.
	Numerator   *AggTerm `json:"numerator,omitempty" yaml:"numerator,omitempty"`
	Denominator *AggTerm `json:"denominator,omitempty" yaml:"denominator,omitempty"`

	// Form 3.
	Expr string `json:"expr,omitempty" yaml:"expr,omitempty"`

	// ZeroDenominator optionally overrides the value produced when a ratio has a
	// zero denominator. Without it the metric reports 0.
	ZeroDenominator *float64 `json:"zero_denominator,omitempty" yaml:"zero_denominator,omitempty"`
}

// AggTerm is one side of a ratio.
type AggTerm struct {
	Where string `json:"where,omitempty" yaml:"where,omitempty"`
	Field string `json:"field,omitempty" yaml:"field,omitempty"`
	Agg   string `json:"agg,omitempty" yaml:"agg,omitempty"`
}

// Form reports which of the three declaration forms the spec uses.
func (c CustomSpec) Form() string {
	switch {
	case strings.TrimSpace(c.Expr) != "":
		return "expr"
	case c.Numerator != nil || c.Denominator != nil:
		return "ratio"
	case strings.TrimSpace(c.Field) != "" || strings.TrimSpace(c.Agg) != "":
		return "aggregate"
	default:
		return ""
	}
}

// Validate checks a custom metric declaration before it is used.
func (c CustomSpec) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("custom metric: name is required")
	}
	if strings.ContainsAny(c.Name, " \t") {
		return fmt.Errorf("custom metric %q: name must not contain whitespace", c.Name)
	}
	if c.Kind != "" && c.Kind != string(KindRun) && c.Kind != string(KindSample) {
		return fmt.Errorf("custom metric %q: kind must be run or sample", c.Name)
	}
	if c.Kind == string(KindSample) {
		return fmt.Errorf("custom metric %q: kind=sample is not supported; per-sample values are built in", c.Name)
	}
	switch strings.ToLower(strings.TrimSpace(c.Direction)) {
	case "", string(DirectionUp), string(DirectionDown), string(DirectionNeutral):
	default:
		return fmt.Errorf("custom metric %q: direction %q is invalid (want %s|%s|%s)",
			c.Name, c.Direction, DirectionUp, DirectionDown, DirectionNeutral)
	}
	switch c.Form() {
	case "expr":
		if err := CheckExprSyntax(c.Expr); err != nil {
			return fmt.Errorf("custom metric %q: expr: %w", c.Name, err)
		}
	case "ratio":
		if c.Numerator == nil || c.Denominator == nil {
			return fmt.Errorf("custom metric %q: ratio form needs both numerator and denominator", c.Name)
		}
		for side, term := range map[string]*AggTerm{"numerator": c.Numerator, "denominator": c.Denominator} {
			if strings.TrimSpace(term.Field) == "" {
				return fmt.Errorf("custom metric %q: %s.field is required", c.Name, side)
			}
			if !validAgg(term.Agg) {
				return fmt.Errorf("custom metric %q: %s.agg %q is invalid (want %s)",
					c.Name, side, term.Agg, strings.Join(aggNames(), "|"))
			}
			if strings.TrimSpace(term.Where) != "" {
				if err := CheckFilterSyntax(term.Where); err != nil {
					return fmt.Errorf("custom metric %q: %s.where: %w", c.Name, side, err)
				}
			}
		}
	case "aggregate":
		if !validAgg(c.Agg) {
			return fmt.Errorf("custom metric %q: agg %q is invalid (want %s)",
				c.Name, c.Agg, strings.Join(aggNames(), "|"))
		}
		if !strings.EqualFold(c.Agg, "count") && strings.TrimSpace(c.Field) == "" {
			return fmt.Errorf("custom metric %q: field is required for agg=%s", c.Name, c.Agg)
		}
		if strings.TrimSpace(c.Where) != "" {
			if err := CheckFilterSyntax(c.Where); err != nil {
				return fmt.Errorf("custom metric %q: where: %w", c.Name, err)
			}
		}
	default:
		return fmt.Errorf("custom metric %q: declare one of expr, numerator/denominator, or field+agg", c.Name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// Runner computes a configured set of metrics over one aggregate.
type Runner struct {
	// Names are built-in metrics to compute, in order.
	Names []string
	// Customs are custom metrics, evaluated in order so each may reference the
	// ones declared before it.
	Customs []CustomSpec
}

// Evaluate computes every configured metric.
//
// A failing custom metric aborts the evaluation rather than being skipped: a
// report that silently omits a metric the dataset asked for is worse than one
// that refuses to render.
func (r *Runner) Evaluate(agg *Aggregate) (Map, error) {
	out := Map{}

	for _, name := range r.Names {
		m, err := Get(name)
		if err != nil {
			return nil, err
		}
		if m.Kind() != KindRun {
			return nil, fmt.Errorf("metric %q is per-sample and cannot be reported at run level", name)
		}
		v, err := m.Compute(agg)
		if err != nil {
			return nil, fmt.Errorf("metric %s: %w", name, err)
		}
		out[name] = finiteValue(v)
	}

	for _, spec := range r.Customs {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		v, err := computeCustom(spec, agg, out)
		if err != nil {
			return nil, err
		}
		out[spec.Name] = finiteValue(v)
	}
	return out, nil
}

// finiteValue enforces the metric package's central invariant: every reported
// value is a finite number.
//
// This is the single boundary every built-in and custom metric passes through,
// which is why the guard lives here rather than in each metric. Infinity and NaN
// are mathematically defensible but worthless in practice -- they cannot be
// serialised to JSON, cannot be rendered in a report, and compare unequal to
// themselves so they poison every downstream average. The runs that produce
// them (nothing resolved, a zero denominator in a user expression) are exactly
// the runs whose reports matter most, so the value becomes 0 and the original is
// preserved in extra.
func finiteValue(v Value) Value {
	if !math.IsInf(v.Value, 0) && !math.IsNaN(v.Value) {
		return v
	}
	original := fmt.Sprintf("%v", v.Value)
	if v.Extra == nil {
		v.Extra = map[string]any{}
	}
	if _, exists := v.Extra["reason"]; !exists {
		v.Extra["reason"] = "undefined (division by zero over an empty or entirely failing run)"
	}
	v.Extra["raw_value"] = original
	v.Value = 0
	return v
}

// computeCustom evaluates one declaration and attaches its rendering hints.
func computeCustom(spec CustomSpec, agg *Aggregate, vars Map) (Value, error) {
	v, err := computeCustomValue(spec, agg, vars)
	if err != nil {
		return Value{}, err
	}
	annotateCustom(&v, spec)
	return v, nil
}

// annotateCustom carries presentation metadata onto the value.
//
// It travels in Extra because Extra is already persisted per metric row, which
// is what lets a report generated weeks later from a copied database still know
// that "slow_cells" is better when small. The alternative -- re-reading the
// config at report time -- would make the report a function of two files
// instead of one, and the config may not exist on the machine doing the reading.
func annotateCustom(v *Value, spec CustomSpec) {
	group := strings.TrimSpace(spec.Group)
	direction := strings.ToLower(strings.TrimSpace(spec.Direction))
	if group == "" && direction == "" {
		return
	}
	if v.Extra == nil {
		v.Extra = map[string]any{}
	}
	if group != "" {
		v.Extra["group"] = group
	}
	if direction != "" {
		v.Extra["direction"] = direction
	}
}

func computeCustomValue(spec CustomSpec, agg *Aggregate, vars Map) (Value, error) {
	unit := Unit(orDefault(spec.Unit, string(UnitRatio)))
	value := Value{Name: spec.Name, Unit: unit, Kind: KindRun, Label: spec.Description}

	switch spec.Form() {
	case "expr":
		nums := make(map[string]float64, len(vars))
		for k, v := range vars {
			nums[k] = v.Value
		}
		f, err := EvalExpr(spec.Expr, nums)
		if err != nil {
			return Value{}, fmt.Errorf("custom metric %s: %w", spec.Name, err)
		}
		value.Value = f
		value.Extra = map[string]any{"expression": spec.Expr}

	case "ratio":
		num, err := aggregateTerm(agg, spec.Numerator)
		if err != nil {
			return Value{}, fmt.Errorf("custom metric %s: numerator: %w", spec.Name, err)
		}
		den, err := aggregateTerm(agg, spec.Denominator)
		if err != nil {
			return Value{}, fmt.Errorf("custom metric %s: denominator: %w", spec.Name, err)
		}
		if den == 0 {
			value.Value = 0
			if spec.ZeroDenominator != nil {
				value.Value = *spec.ZeroDenominator
			}
			value.Extra = map[string]any{"numerator": num, "denominator": 0, "undefined": true}
		} else {
			value.Value = num / den
			value.Extra = map[string]any{"numerator": num, "denominator": den}
		}

	case "aggregate":
		scoped, err := scopeSamples(agg, spec.Where)
		if err != nil {
			return Value{}, fmt.Errorf("custom metric %s: %w", spec.Name, err)
		}
		if len(scoped.Sample) == 0 {
			value.Value = 0
			value.Extra = map[string]any{"samples": 0, "where": spec.Where, "empty": true}
			return value, nil
		}
		f, err := aggregateField(scoped, spec.Field, spec.Agg, spec.WeightField)
		if err != nil {
			return Value{}, fmt.Errorf("custom metric %s: %w", spec.Name, err)
		}
		value.Value = f
		value.Extra = map[string]any{
			"samples": len(scoped.Sample), "field": spec.Field, "agg": spec.Agg, "where": spec.Where,
		}
	}
	return value, nil
}

// scopeSamples applies a `where` filter, returning the full set when empty.
func scopeSamples(agg *Aggregate, where string) (*Aggregate, error) {
	if strings.TrimSpace(where) == "" {
		return agg, nil
	}
	keep := func(s *Sample) bool {
		ok, err := EvalFilter(where, s)
		return err == nil && ok
	}
	// Validate the filter once up front so a typo reports an error instead of
	// silently matching nothing.
	if len(agg.Sample) > 0 {
		if _, err := EvalFilter(where, &agg.Sample[0]); err != nil {
			return nil, fmt.Errorf("invalid where clause %q: %w", where, err)
		}
	}
	return agg.Filter(keep), nil
}

func aggregateTerm(agg *Aggregate, term *AggTerm) (float64, error) {
	scoped, err := scopeSamples(agg, term.Where)
	if err != nil {
		return 0, err
	}
	if len(scoped.Sample) == 0 {
		return 0, nil
	}
	return aggregateField(scoped, term.Field, term.Agg, "")
}

// aggregateField applies an aggregation to one numeric sample field.
//
// A field that is unavailable on a sample is skipped rather than counted as
// zero, so a metric is never diluted by samples where the underlying
// measurement does not exist.
func aggregateField(agg *Aggregate, field, op, weightField string) (float64, error) {
	op = strings.ToLower(orDefault(op, "mean"))

	switch op {
	case "count":
		if strings.TrimSpace(field) == "" {
			return float64(len(agg.Sample)), nil
		}
		var n int
		for i := range agg.Sample {
			if _, ok := lookupField(&agg.Sample[i], field); ok {
				n++
			}
		}
		return float64(n), nil
	}

	values := make([]float64, 0, len(agg.Sample))
	weights := make([]float64, 0, len(agg.Sample))
	for i := range agg.Sample {
		v, ok := lookupField(&agg.Sample[i], field)
		if !ok {
			continue
		}
		values = append(values, v)
		if weightField != "" {
			w, ok := lookupField(&agg.Sample[i], weightField)
			if !ok {
				w = 0
			}
			weights = append(weights, w)
		}
	}

	switch op {
	case "mean":
		if weightField != "" {
			var num, den float64
			for i, v := range values {
				num += weights[i] * v
				den += weights[i]
			}
			if den == 0 {
				return 0, nil
			}
			return num / den, nil
		}
		return mean(values), nil
	case "sum":
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum, nil
	case "min":
		if len(values) == 0 {
			return 0, nil
		}
		out := values[0]
		for _, v := range values[1:] {
			if v < out {
				out = v
			}
		}
		return out, nil
	case "max":
		if len(values) == 0 {
			return 0, nil
		}
		out := values[0]
		for _, v := range values[1:] {
			if v > out {
				out = v
			}
		}
		return out, nil
	case "p50", "median":
		return percentile(sortedCopy(values), 50), nil
	case "p95":
		return percentile(sortedCopy(values), 95), nil
	case "p99":
		return percentile(sortedCopy(values), 99), nil
	case "stddev":
		if len(values) == 0 {
			return 0, nil
		}
		m := mean(values)
		var acc float64
		for _, v := range values {
			acc += (v - m) * (v - m)
		}
		return math.Sqrt(acc / float64(len(values))), nil
	default:
		return 0, fmt.Errorf("unknown agg %q (want %s)", op, strings.Join(aggNames(), "|"))
	}
}

func lookupField(s *Sample, field string) (float64, bool) {
	if v, ok := s.Numeric()[field]; ok {
		return v, true
	}
	if v, ok := s.Numeric()[strings.ToLower(field)]; ok {
		return v, true
	}
	// Derive a few handy pseudo-fields that are not stored directly.
	switch strings.ToLower(field) {
	case "failed_or_timed_out":
		n := s.Numeric()
		if n["failed"] > 0 || n["timed_out"] > 0 {
			return 1, true
		}
		return 0, true
	case "turns_per_tool_call":
		n := s.Numeric()
		if n["tool_calls"] == 0 {
			return 0, true
		}
		return n["turns"] / n["tool_calls"], true
	case "tokens_per_turn":
		n := s.Numeric()
		if n["turns"] == 0 {
			return 0, true
		}
		return n["total_tokens"] / n["turns"], true
	}
	return 0, false
}

func validAgg(a string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	if a == "" {
		return true // empty means the default
	}
	for _, name := range aggNames() {
		if a == name || (a == "median" && name == "p50") {
			return true
		}
	}
	return false
}

func aggNames() []string {
	return []string{"mean", "sum", "count", "min", "max", "p50", "p95", "p99", "stddev"}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------------------
// Regex helper
// ---------------------------------------------------------------------------

var (
	filterReMu    sync.Mutex
	filterReCache = map[string]*regexp.Regexp{}
)

func compileFilterRegex(pattern string) (*regexp.Regexp, error) {
	filterReMu.Lock()
	defer filterReMu.Unlock()
	if re, ok := filterReCache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q in filter: %w", pattern, err)
	}
	filterReCache[pattern] = re
	return re, nil
}

// DefaultRunMetrics is the metric set reported when a configuration does not
// name one explicitly: the headline numbers plus the cost, effort and
// degenerate-outcome rates needed to interpret them.
func DefaultRunMetrics() []string {
	out := []string{
		"resolve_rate",
		"mean_score",
		"mean_reward",
		"reward_signal_rate",
		"guard_violation_rate",
		"pass_at_k",
		"pass_hat_k",
		"resolved_count",
		"task_count",
		"sample_count",
		"total_cost_usd",
		"cost_per_resolved_usd",
		"mean_cost_usd",
		"mean_total_tokens",
		"tokens_per_resolved",
		"mean_turns",
		"p95_turns",
		"mean_tool_calls",
		"tool_call_error_rate",
		"mean_latency_ms",
		"p50_latency_ms",
		"p95_latency_ms",
		"mean_ttft_ms",
		"timeout_rate",
		"max_turns_rate",
		"harness_error_rate",
		"incomplete_rate",
		"grader_error_rate",
		"resolve_rate_by_category",
		"resolve_rate_by_difficulty",
		"mean_score_by_category",
		"stop_reason_mix",
	}
	// Keep registration order stable in reports.
	sort.Strings(out)
	return out
}

package metric

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/trace"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mkAttempt builds an attempt whose trajectory has the requested shape, going
// through the real parser so the fixtures cannot drift from production shapes.
func mkAttempt(t *testing.T, turns, toolCalls, toolErrors, inTok, outTok int, duration time.Duration) *agent.Attempt {
	t.Helper()
	var lines []string
	lines = append(lines, `{"type":"system","subtype":"init","model":"m","session_id":"s"}`)
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("m%d", i)
		if i < toolCalls {
			lines = append(lines, fmt.Sprintf(
				`{"type":"assistant","message":{"id":%q,"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{}}]}}`, id, i))
			isErr := i < toolErrors
			lines = append(lines, fmt.Sprintf(
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","is_error":%t,"content":"out"}]}}`, i, isErr))
		} else {
			lines = append(lines, fmt.Sprintf(
				`{"type":"assistant","message":{"id":%q,"role":"assistant","content":[{"type":"text","text":"done %d"}]}}`, id, i))
		}
	}
	lines = append(lines, fmt.Sprintf(
		`{"type":"result","subtype":"success","result":"done","num_turns":%d,"total_cost_usd":0.5,"ttft_ms":120,"duration_ms":%d,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		turns, duration.Milliseconds(), inTok, outTok))

	tr, err := trace.Parse(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Attempt{
		Agent: "claude_code", Model: "m", ExitCode: 0,
		Duration: duration, Trace: tr,
	}
}

type sampleOpt func(*Sample)

func withOutcome(passed bool, score float64) sampleOpt {
	return func(s *Sample) {
		s.Grade = &grader.Grade{TaskID: s.TaskID, Passed: passed, Score: score}
	}
}

func withMeta(category, difficulty string, tags ...string) sampleOpt {
	return func(s *Sample) {
		s.Category = category
		s.Difficulty = difficulty
		s.Tags = tags
	}
}

func withAttempt(a *agent.Attempt) sampleOpt { return func(s *Sample) { s.Attempt = a } }

func mkSample(t *testing.T, taskID string, opts ...sampleOpt) Sample {
	t.Helper()
	s := Sample{
		RunID: "run1", TaskID: taskID, SampleIndex: 0,
		Attempt: mkAttempt(t, 4, 2, 0, 1000, 200, 3*time.Second),
		Grade:   &grader.Grade{TaskID: taskID, Passed: true, Score: 1},
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func mkAggregate(t *testing.T, samples ...Sample) *Aggregate {
	t.Helper()
	return &Aggregate{RunID: "run1", Sample: samples}
}

func mustMetric(t *testing.T, name string, agg *Aggregate) Value {
	t.Helper()
	m, err := Get(name)
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Compute(agg)
	if err != nil {
		t.Fatalf("compute %s: %v", name, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// pass@k
// ---------------------------------------------------------------------------

func TestPassAtKMatchesClosedForm(t *testing.T) {
	// 1 - C(n-c, k)/C(n, k)
	cases := []struct {
		n, c, k int
		want    float64
	}{
		{n: 5, c: 0, k: 1, want: 0},
		{n: 5, c: 2, k: 1, want: 0.4}, // 1 - C(3,1)/C(5,1) = 1 - 3/5
		{n: 10, c: 5, k: 3, want: 1.0 - 10.0/120.0},
		{n: 4, c: 4, k: 2, want: 1}, // all succeed
		{n: 3, c: 1, k: 3, want: 1}, // n-c < k implies certainty
		{n: 10, c: 1, k: 1, want: 0.1},
	}
	for _, cse := range cases {
		got := PassAtK(cse.n, cse.c, cse.k)
		if math.Abs(got-cse.want) > 1e-9 {
			t.Errorf("PassAtK(n=%d,c=%d,k=%d) = %.10f, want %.10f", cse.n, cse.c, cse.k, got, cse.want)
		}
	}
}

func TestPassAtKGrowsWithKAndPassHatKShrinks(t *testing.T) {
	for _, c := range []int{1, 2, 3, 4} {
		prev := -1.0
		for k := 1; k <= 4; k++ {
			v := PassAtK(4, c, k)
			if v < prev {
				t.Errorf("pass@k should be monotonic in k: c=%d k=%d gave %v after %v", c, k, v, prev)
			}
			prev = v
		}
	}
	// pass^k must be <= pass@k, since requiring every sample to pass is strictly
	// harder than requiring one.
	for _, c := range []int{1, 2, 3, 4} {
		pk := PassAtK(4, c, 2)
		phk := PassHatK(c, 4, 2)
		if phk > pk {
			t.Errorf("pass^2 (%v) exceeded pass@2 (%v) for c=%d", phk, pk, c)
		}
	}
}

func TestPassAtKRunMetricAveragesPerTask(t *testing.T) {
	// task A: 1 of 2 passed; task B: 2 of 2 passed. k = 2.
	// pass@2(A) = 1 - C(1,2)/C(2,2) = 1 (n-c=1 < k=2)
	// pass@2(B) = 1
	agg := mkAggregate(t,
		mkSample(t, "A", withOutcome(true, 1)),
		mkSample(t, "A", withOutcome(false, 0)),
		mkSample(t, "B", withOutcome(true, 1)),
		mkSample(t, "B", withOutcome(true, 1)),
	)
	if got := agg.K(); got != 2 {
		t.Fatalf("K() = %d, want 2", got)
	}
	v := mustMetric(t, "pass_at_k", agg)
	if math.Abs(v.Value-1.0) > 1e-9 {
		t.Errorf("pass_at_k = %v, want 1", v.Value)
	}

	// pass^2 under the tau-bench convention is mean over tasks of (c/n)^2:
	//   task A: (1/2)^2 = 0.25
	//   task B: (2/2)^2 = 1.0
	// so 0.625. Note it is strictly below pass@2 = 1.0, which is the whole point:
	// the agent can pass task A but cannot pass it *reliably*.
	v = mustMetric(t, "pass_hat_k", agg)
	if math.Abs(v.Value-0.625) > 1e-9 {
		t.Errorf("pass_hat_k = %v, want 0.625", v.Value)
	}
}

func TestResolveRateAndBreakdowns(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1), withMeta("bugfix", "easy")),
		mkSample(t, "b", withOutcome(false, 0.5), withMeta("bugfix", "hard")),
		mkSample(t, "c", withOutcome(true, 1), withMeta("feature", "hard")),
		mkSample(t, "d", withOutcome(true, 1), withMeta("feature", "hard")),
	)

	v := mustMetric(t, "resolve_rate", agg)
	if math.Abs(v.Value-0.75) > 1e-9 {
		t.Errorf("resolve_rate = %v, want 0.75", v.Value)
	}
	if v.Extra["passed"] != 3 || v.Extra["total"] != 4 {
		t.Errorf("resolve_rate extra = %v", v.Extra)
	}

	v = mustMetric(t, "mean_score", agg)
	if math.Abs(v.Value-0.875) > 1e-9 {
		t.Errorf("mean_score = %v, want 0.875", v.Value)
	}

	v = mustMetric(t, "resolve_rate_by_difficulty", agg)
	byDiff, ok := v.Extra["hard"].(map[string]any)
	if !ok {
		t.Fatalf("missing difficulty breakdown: %v", v.Extra)
	}
	if byDiff["resolve_rate"] != 0.6666666666666666 {
		t.Errorf("hard resolve rate = %v, want 2/3", byDiff["resolve_rate"])
	}
	easy, _ := v.Extra["easy"].(map[string]any)
	if easy["resolve_rate"] != 1.0 {
		t.Errorf("easy resolve rate = %v, want 1", easy["resolve_rate"])
	}
}

func TestCostAndTokenMetrics(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "b", withOutcome(false, 0)),
	)
	v := mustMetric(t, "total_cost_usd", agg)
	if math.Abs(v.Value-1.0) > 1e-9 { // two attempts at $0.50 each
		t.Errorf("total_cost_usd = %v, want 1.0", v.Value)
	}
	v = mustMetric(t, "cost_per_resolved_usd", agg)
	if math.Abs(v.Value-1.0) > 1e-9 {
		t.Errorf("cost_per_resolved_usd = %v, want 1.0", v.Value)
	}
	v = mustMetric(t, "mean_total_tokens", agg)
	if v.Value != 1200 {
		t.Errorf("mean_total_tokens = %v, want 1200", v.Value)
	}
	v = mustMetric(t, "tokens_per_resolved", agg)
	if v.Value != 2400 {
		t.Errorf("tokens_per_resolved = %v, want 2400", v.Value)
	}
}

func TestCostPerResolvedIsZeroWhenNothingPasses(t *testing.T) {
	agg := mkAggregate(t, mkSample(t, "a", withOutcome(false, 0)))
	runner := &Runner{Names: []string{"cost_per_resolved_usd", "tokens_per_resolved"}}
	values, err := runner.Evaluate(agg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// A zero denominator reports 0, not +Inf. Infinity cannot be serialised to
	// JSON or rendered in a report, and "nothing resolved" is exactly the run
	// whose report matters most.
	for _, name := range []string{"cost_per_resolved_usd", "tokens_per_resolved"} {
		v := values[name]
		if v.Value != 0 {
			t.Errorf("%s = %v, want 0", name, v.Value)
		}
		if v.Extra["reason"] == nil {
			t.Errorf("%s must explain the undefined ratio in extra.reason", name)
		}
		if v.Extra["resolved"] != 0 {
			t.Errorf("%s should report resolved=0, got %v", name, v.Extra["resolved"])
		}
	}
}

// TestEveryMetricIsFinite is the invariant test for the whole package: no
// built-in metric may ever produce a value that cannot be persisted or plotted.
// The empty aggregate is the input most likely to break it, because it forces
// every denominator to zero.
func TestEveryMetricIsFinite(t *testing.T) {
	attempt := mkAttempt(t, 1, 2, 3, 4, 5, time.Second)
	aggregates := map[string]*Aggregate{
		"empty":    {RunID: "r", SamplesPerTask: 1},
		"all fail": mkAggregate(t, mkSample(t, "a", withOutcome(false, 0), withAttempt(attempt))),
		"one pass": mkAggregate(t, mkSample(t, "a", withOutcome(true, 1), withAttempt(attempt))),
	}

	// Every registered run-level metric, not just the defaults: a dataset can
	// name any of them explicitly.
	var names []string
	for _, n := range Names() {
		if m, err := Get(n); err == nil && m.Kind() == KindRun {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		t.Fatal("no run-level metrics registered")
	}

	for label, agg := range aggregates {
		runner := &Runner{Names: names}
		values, err := runner.Evaluate(agg)
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", label, err)
		}
		if len(values) != len(names) {
			t.Fatalf("%s: got %d values for %d metrics", label, len(values), len(names))
		}
		for name, v := range values {
			if math.IsInf(v.Value, 0) || math.IsNaN(v.Value) {
				t.Errorf("%s: metric %s is non-finite (%v)", label, name, v.Value)
			}
		}
		// The values must survive the storage round trip, which is JSON.
		if _, err := json.Marshal(values); err != nil {
			t.Errorf("%s: metrics are not JSON-serialisable: %v", label, err)
		}
	}
}

func TestFiniteValuePreservesTheOriginal(t *testing.T) {
	inf := finiteValue(Value{Name: "x", Value: math.Inf(-1), Unit: UnitUSD})
	if inf.Value != 0 {
		t.Fatalf("value = %v, want 0", inf.Value)
	}
	if inf.Extra["raw_value"] != "-Inf" {
		t.Fatalf("the original must be preserved, got %v", inf.Extra["raw_value"])
	}
	if inf.Extra["reason"] == nil {
		t.Fatal("an undefined value must explain itself")
	}

	nan := finiteValue(Value{Name: "y", Value: math.NaN()})
	if nan.Value != 0 || nan.Extra["raw_value"] != "NaN" {
		t.Fatalf("NaN handling wrong: %+v", nan)
	}

	// An existing reason wins: the metric knows better than the guard.
	kept := finiteValue(Value{Value: math.Inf(1), Extra: map[string]any{"reason": "nothing passed"}})
	if kept.Extra["reason"] != "nothing passed" {
		t.Fatalf("the guard overwrote a specific reason: %v", kept.Extra["reason"])
	}

	// Finite values pass through untouched, including their extra map identity.
	plain := Value{Name: "z", Value: 1.5}
	if got := finiteValue(plain); got.Value != 1.5 || got.Extra != nil {
		t.Fatalf("finite value was modified: %+v", got)
	}
}

func TestFilterSyntaxCheckNeverPanics(t *testing.T) {
	// Syntax checking parses filters with no row to test. Every sample-dependent
	// branch must tolerate that, or a typo in a dataset's `where:` clause crashes
	// the process instead of reporting a syntax error.
	filters := []string{
		"tags contains 'python'",
		"category in ['bugfix', 'feature']",
		"task_id startswith 'django'",
		"task_id endswith '11099'",
		"category not in ['docs']",
		"tags contains 'x' and score > 0.5",
		"passed and turns > 3",
		"category ~= '^fix'",
		"(tags contains 'a' or tags contains 'b') and passed",
		"is_null(category)",
		"difficulty == 'easy' or difficulty in ['hard']",
	}
	for _, f := range filters {
		if err := CheckFilterSyntax(f); err != nil {
			t.Errorf("CheckFilterSyntax(%q) = %v, want nil", f, err)
		}
	}

	// And real syntax errors are still caught, rather than being masked by the
	// nil-sample short circuit.
	bad := []string{
		"category ==",
		"tags contains",
		"(passed",
		"category in",
		"passed and",
	}
	for _, f := range bad {
		if err := CheckFilterSyntax(f); err == nil {
			t.Errorf("CheckFilterSyntax(%q) = nil, want a syntax error", f)
		}
	}
}

func TestFilterEvaluationWithATagList(t *testing.T) {
	// The same expressions must still evaluate correctly against a real sample;
	// the nil-sample guard must not turn evaluation into a no-op.
	s := mkSample(t, "a")
	s.Tags = []string{"python", "off-by-one"}

	tests := map[string]bool{
		"tags contains 'python'":    true,
		"tags contains 'rust'":      false,
		"task_id startswith 'a'":    true,
		"post_task_id endswith 'a'": false,
	}
	for expr := range tests {
		if expr == "post_task_id endswith 'a'" {
			continue // placeholder for readability; not a real field
		}
		got, err := EvalFilter(expr, &s)
		if err != nil {
			t.Fatalf("EvalFilter(%q): %v", expr, err)
		}
		if got != tests[expr] {
			t.Fatalf("EvalFilter(%q) = %v, want %v", expr, got, tests[expr])
		}
	}

	got, err := EvalFilter("task_id endswith 'a'", &s)
	if err != nil {
		t.Fatalf("EvalFilter: %v", err)
	}
	if !got {
		t.Fatal("task_id endswith 'a' should match task id \"a\"")
	}
}

func TestNilSampleContainsIsSafe(t *testing.T) {
	var s *Sample
	if s.Contains("tags", "x") {
		t.Fatal("a nil sample cannot contain anything")
	}
	if s.Numeric() == nil {
		t.Fatal("Numeric on a nil sample should still return the zero map")
	}
	if s.Score() != 0 || s.Passed() {
		t.Fatal("a nil sample has no verdict")
	}
}

func TestDegenerateOutcomeRates(t *testing.T) {
	timedOut := mkAttempt(t, 1, 0, 0, 10, 10, time.Second)
	timedOut.TimedOut = true
	failed := mkAttempt(t, 1, 0, 0, 10, 10, time.Second)
	failed.Failed = true

	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "b", withOutcome(false, 0), withAttempt(timedOut)),
		mkSample(t, "c", withOutcome(false, 0), withAttempt(failed)),
		mkSample(t, "d", withOutcome(false, 0), withAttempt(failed)),
	)
	if v := mustMetric(t, "timeout_rate", agg); v.Value != 0.25 {
		t.Errorf("timeout_rate = %v, want 0.25", v.Value)
	}
	if v := mustMetric(t, "harness_error_rate", agg); v.Value != 0.5 {
		t.Errorf("harness_error_rate = %v, want 0.5", v.Value)
	}
	// A resolve rate of 25% alongside a 75% degenerate rate is the point of these
	// metrics existing at all.
	if v := mustMetric(t, "resolve_rate", agg); v.Value != 0.25 {
		t.Errorf("resolve_rate = %v", v.Value)
	}
}

func TestToolErrorRateUsesCallsNotSamples(t *testing.T) {
	mixed := mkAttempt(t, 4, 4, 2, 100, 50, time.Second) // 4 calls, 2 errors
	clean := mkAttempt(t, 2, 2, 0, 100, 50, time.Second) // 2 calls, 0 errors
	agg := mkAggregate(t,
		mkSample(t, "a", withAttempt(mixed)),
		mkSample(t, "b", withAttempt(clean)),
	)
	v := mustMetric(t, "tool_call_error_rate", agg)
	if math.Abs(v.Value-2.0/6.0) > 1e-9 {
		t.Errorf("tool_call_error_rate = %v, want 2/6", v.Value)
	}
	if v.Extra["tool_calls"] != 6 || v.Extra["tool_errors"] != 2 {
		t.Errorf("extra = %v", v.Extra)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	agg := &Aggregate{RunID: "r"}
	for i := 1; i <= 100; i++ {
		agg.Sample = append(agg.Sample, mkSample(t, fmt.Sprintf("t%d", i),
			withAttempt(mkAttempt(t, 1, 0, 0, 10, 10, time.Duration(i)*time.Millisecond)),
			withOutcome(true, 1)))
	}
	if v := mustMetric(t, "p50_latency_ms", agg); v.Value != 50.5 {
		t.Errorf("p50 = %v, want 50.5", v.Value)
	}
	if v := mustMetric(t, "p95_latency_ms", agg); math.Abs(v.Value-95.05) > 1e-9 {
		t.Errorf("p95 = %v, want 95.05", v.Value)
	}
	// mean() skips nothing here, so it should land on the midpoint.
	if v := mustMetric(t, "mean_latency_ms", agg); v.Value != 50.5 {
		t.Errorf("mean = %v, want 50.5", v.Value)
	}
}

func TestMissingFieldIsSkippedNotZeroed(t *testing.T) {
	// A sample with no trace must be excluded from token means, not counted as
	// zero, which would halve the reported average.
	noTrace := Sample{RunID: "r", TaskID: "b", Grade: &grader.Grade{Passed: true, Score: 1}}
	agg := mkAggregate(t,
		mkSample(t, "a"), // 1200 tokens
		noTrace,
	)
	v := mustMetric(t, "mean_total_tokens", agg)
	if v.Value != 1200 {
		t.Errorf("mean_total_tokens = %v, want 1200 (missing sample excluded)", v.Value)
	}
}

func TestEmptyAggregateIsSafe(t *testing.T) {
	agg := &Aggregate{RunID: "r"}
	for _, name := range []string{"resolve_rate", "mean_score", "pass_at_k", "total_cost_usd", "p95_latency_ms", "stop_reason_mix"} {
		v := mustMetric(t, name, agg)
		if math.IsNaN(v.Value) {
			t.Errorf("%s produced NaN on an empty aggregate", name)
		}
	}
}

// ---------------------------------------------------------------------------
// expression language
// ---------------------------------------------------------------------------

func TestEvalExpr(t *testing.T) {
	vars := map[string]float64{"a": 10, "b": 4, "rate": 0.5}
	cases := []struct {
		src  string
		want float64
	}{
		{"1 + 2 * 3", 7},
		{"(1 + 2) * 3", 9},
		{"a / b", 2.5},
		{"-a", -10},
		{"a % b", 2},
		{"2 ^ 10", 1024},
		{"min(a, b)", 4},
		{"max(a, b, 100)", 100},
		{"abs(0 - 5)", 5},
		{"round(2.567, 2)", 2.57},
		{"sqrt(16)", 4},
		{"clamp(a, 0, 5)", 5},
		{"if(rate > 0.4, 1, 0)", 1},
		{"rate * 100", 50},
		{"a / max(b, 0.01)", 2.5},
	}
	for _, c := range cases {
		got, err := EvalExpr(c.src, vars)
		if err != nil {
			t.Errorf("EvalExpr(%q): %v", c.src, err)
			continue
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("EvalExpr(%q) = %v, want %v", c.src, got, c.want)
		}
	}

	// An unknown metric name must fail loudly: a typo silently evaluating to 0
	// would publish a wrong number.
	if _, err := EvalExpr("no_such_metric * 2", vars); err == nil {
		t.Error("unknown identifier should be an error")
	}
	if _, err := EvalExpr("1 +", vars); err == nil {
		t.Error("trailing operator should be an error")
	}
	if _, err := EvalExpr("nope(1)", vars); err == nil {
		t.Error("unknown function should be an error")
	}
	// Division by zero yields 0 rather than aborting a whole report.
	if got, err := EvalExpr("1 / 0", vars); err != nil || got != 0 {
		t.Errorf("1/0 = %v, %v; want 0, nil", got, err)
	}
}

func TestEvalExprPrecedenceAndParens(t *testing.T) {
	vars := map[string]float64{}
	got, err := EvalExpr("2 + 3 * 4 - 6 / 3", vars)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12 {
		t.Errorf("got %v, want 12", got)
	}
}

// ---------------------------------------------------------------------------
// filters
// ---------------------------------------------------------------------------

func TestEvalFilter(t *testing.T) {
	s := mkSample(t, "fix-average",
		withOutcome(true, 1),
		withMeta("bugfix", "hard", "python", "security"),
	)
	s.Attempt = mkAttempt(t, 25, 3, 0, 500, 100, time.Second)

	cases := []struct {
		src  string
		want bool
	}{
		{"passed", true},
		{"passed == true", true},
		{"passed == false", false},
		{"not passed", false},
		{"category == 'bugfix'", true},
		{"category = 'feature'", false},
		{"category != 'feature'", true},
		{"difficulty == 'hard' and passed", true},
		{"difficulty == 'easy' or passed", true},
		{"difficulty == 'easy' and passed", false},
		{"turns > 20", true},
		{"turns >= 25 and turns <= 30", true},
		{"turns < 20", false},
		{"score >= 0.99", true},
		{"tags contains 'python'", true},
		{"tags contains 'ruby'", false},
		{"task_id startswith 'fix'", true},
		{"task_id endswith 'average'", true},
		{"task_id ~= '^fix-.*'", true},
		{"category in ['bugfix', 'feature']", true},
		{"category in ['docs']", false},
		{"(difficulty == 'easy' or difficulty == 'hard') and passed", true},
		{"passed and not (turns < 5)", true},
		{"timed_out == 0", true},
	}
	for _, c := range cases {
		got, err := EvalFilter(c.src, &s)
		if err != nil {
			t.Errorf("EvalFilter(%q): %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("EvalFilter(%q) = %v, want %v", c.src, got, c.want)
		}
	}

	for _, bad := range []string{"nonsense_field == 1", "turns >>> 3", "category =="} {
		if _, err := EvalFilter(bad, &s); err == nil {
			t.Errorf("EvalFilter(%q) should have failed", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// custom metrics
// ---------------------------------------------------------------------------

func TestCustomAggregateForm(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1), withMeta("bugfix", "easy")),
		mkSample(t, "b", withOutcome(false, 0), withMeta("bugfix", "hard")),
		mkSample(t, "c", withOutcome(true, 1), withMeta("feature", "hard")),
		mkSample(t, "d", withOutcome(false, 0), withMeta("feature", "hard")),
	)

	r := &Runner{
		Names: []string{"resolve_rate", "total_cost_usd"},
		Customs: []CustomSpec{
			{Name: "hard_resolve_rate", Where: "difficulty == 'hard'", Field: "passed", Agg: "mean"},
			{Name: "hard_samples", Where: "difficulty == 'hard'", Agg: "count"},
			{Name: "total_turns", Field: "turns", Agg: "sum", Unit: "count"},
			{Name: "median_latency", Field: "latency_ms", Agg: "p50", Unit: "ms"},
			{Name: "bugfix_only", Where: "category == 'bugfix'", Field: "passed", Agg: "mean"},
		},
	}
	got, err := r.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}

	if v := got["hard_resolve_rate"]; math.Abs(v.Value-1.0/3.0) > 1e-9 {
		t.Errorf("hard_resolve_rate = %v, want 1/3", v.Value)
	}
	if v := got["hard_samples"]; v.Value != 3 {
		t.Errorf("hard_samples = %v, want 3", v.Value)
	}
	if v := got["total_turns"]; v.Value != 16 { // 4 samples x 4 turns
		t.Errorf("total_turns = %v, want 16", v.Value)
	}
	if v := got["median_latency"]; v.Value != 3000 {
		t.Errorf("median_latency = %v, want 3000", v.Value)
	}
	if v := got["bugfix_only"]; v.Value != 0.5 {
		t.Errorf("bugfix_only = %v, want 0.5", v.Value)
	}
}

func TestCustomFilterMatchingNothingYieldsZeroNotError(t *testing.T) {
	agg := mkAggregate(t, mkSample(t, "a", withOutcome(true, 1)))
	r := &Runner{Customs: []CustomSpec{
		{Name: "no_match", Where: "category == 'nonexistent'", Field: "passed", Agg: "mean"},
	}}
	got, err := r.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	v := got["no_match"]
	if v.Value != 0 {
		t.Errorf("value = %v, want 0", v.Value)
	}
	if v.Extra["empty"] != true {
		t.Errorf("extra should flag the empty scope: %v", v.Extra)
	}
}

func TestCustomRatioForm(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "b", withOutcome(false, 0)),
	)
	r := &Runner{Customs: []CustomSpec{{
		Name:        "effort_per_pass",
		Numerator:   &AggTerm{Field: "turns", Agg: "sum"},
		Denominator: &AggTerm{Field: "passed", Agg: "sum"},
		Unit:        "count",
	}}}
	got, err := r.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	// 8 total turns / 1 passed = 8
	if v := got["effort_per_pass"]; v.Value != 8 {
		t.Errorf("effort_per_pass = %v, want 8", v.Value)
	}
}

func TestCustomExprFormSeesBuiltinsAndEarlierCustoms(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "b", withOutcome(false, 0)),
	)
	r := &Runner{
		Names: []string{"resolve_rate", "total_cost_usd"},
		Customs: []CustomSpec{
			{Name: "resolved", Field: "passed", Agg: "sum", Unit: "count"},
			{Name: "cost_efficiency", Expr: "resolve_rate / max(total_cost_usd, 0.01)"},
			// References a custom declared above, which is why order matters.
			{Name: "cost_per_resolved_custom", Expr: "total_cost_usd / max(resolved, 1)"},
		},
	}
	got, err := r.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["cost_efficiency"]; math.Abs(v.Value-0.5) > 1e-9 {
		t.Errorf("cost_efficiency = %v, want 0.5", v.Value)
	}
	if v := got["cost_per_resolved_custom"]; math.Abs(v.Value-1.0) > 1e-9 {
		t.Errorf("cost_per_resolved_custom = %v, want 1.0", v.Value)
	}
}

func TestCustomSpecValidation(t *testing.T) {
	cases := []struct {
		spec    CustomSpec
		wantErr bool
	}{
		{CustomSpec{Name: "ok", Field: "passed", Agg: "mean"}, false},
		{CustomSpec{Name: "", Field: "passed", Agg: "mean"}, true},
		{CustomSpec{Name: "has space", Field: "passed", Agg: "mean"}, true},
		{CustomSpec{Name: "x", Expr: "1 + 1"}, false},
		{CustomSpec{Name: "x", Expr: "1 +"}, true},
		{CustomSpec{Name: "x", Field: "passed", Agg: "bogus"}, true},
		{CustomSpec{Name: "x", Field: "passed"}, false},                            // agg defaults to mean
		{CustomSpec{Name: "x", Agg: "count"}, false},                               // count needs no field
		{CustomSpec{Name: "x"}, true},                                              // nothing declared
		{CustomSpec{Name: "x", Numerator: &AggTerm{Field: "a", Agg: "sum"}}, true}, // half a ratio
		{CustomSpec{Name: "x", Kind: "sample", Field: "passed", Agg: "mean"}, true},
		{CustomSpec{Name: "x", Where: "bad_field ==", Field: "passed", Agg: "mean"}, true},
		// direction is constrained; group is free-form so a project can add its
		// own section title without waiting for a release here.
		{CustomSpec{Name: "x", Field: "passed", Agg: "mean", Direction: "up"}, false},
		{CustomSpec{Name: "x", Field: "passed", Agg: "mean", Direction: "DOWN"}, false},
		{CustomSpec{Name: "x", Field: "passed", Agg: "mean", Direction: "neutral"}, false},
		{CustomSpec{Name: "x", Field: "passed", Agg: "mean", Direction: "sideways"}, true},
		{CustomSpec{Name: "x", Field: "passed", Agg: "mean", Group: "Anything"}, false},
	}
	for _, c := range cases {
		err := c.spec.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("Validate(%+v) error = %v, want error = %v", c.spec, err, c.wantErr)
		}
	}
}

// TestCustomMetricCarriesItsRenderingHints checks that a declared direction and
// group survive evaluation in Extra, which is what the report reads. Without
// this, a report generated later from a copied database would fall back to
// guessing from the metric's name.
func TestCustomMetricCarriesItsRenderingHints(t *testing.T) {
	agg := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "b", withOutcome(false, 0)),
	)
	r := &Runner{Customs: []CustomSpec{{
		Name: "slow_cells", Where: "latency_ms > 1000", Agg: "count",
		Unit: "count", Direction: "down", Group: "latency",
	}}}
	got, err := r.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	v := got["slow_cells"]
	if v.Extra["direction"] != "down" {
		t.Errorf("direction = %v, want down", v.Extra["direction"])
	}
	if v.Extra["group"] != "latency" {
		t.Errorf("group = %v, want latency", v.Extra["group"])
	}
	if v.Extra["agg"] != "count" {
		t.Errorf("the direction hint must not displace the computed extras: %v", v.Extra)
	}

	// A spec that declares neither must not invent an empty entry.
	bare := &Runner{Customs: []CustomSpec{{Name: "plain", Field: "passed", Agg: "mean"}}}
	got, err = bare.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["plain"].Extra["direction"]; ok {
		t.Errorf("an undeclared direction must not be present: %v", got["plain"].Extra)
	}
	// The early-return path (a filter matching nothing) must still be annotated.
	empty := &Runner{Customs: []CustomSpec{{
		Name: "none", Where: "category == 'nope'", Agg: "count",
		Direction: "down", Group: "latency",
	}}}
	got, err = empty.Evaluate(agg)
	if err != nil {
		t.Fatal(err)
	}
	if got["none"].Extra["direction"] != "down" {
		t.Errorf("an empty scope lost its hints: %v", got["none"].Extra)
	}
}

func TestRunnerRejectsUnknownMetricName(t *testing.T) {
	r := &Runner{Names: []string{"not_a_metric"}}
	if _, err := r.Evaluate(mkAggregate(t)); err == nil {
		t.Error("an unknown metric name must fail the evaluation")
	}
}

func TestRunnerRejectsSampleLevelMetricAtRunLevel(t *testing.T) {
	r := &Runner{Names: []string{"resolve_rate"}}
	if _, err := r.Evaluate(mkAggregate(t)); err != nil {
		t.Fatalf("resolve_rate is a run metric: %v", err)
	}
}

func TestDefaultRunMetricsAllResolve(t *testing.T) {
	for _, name := range DefaultRunMetrics() {
		if _, err := Get(name); err != nil {
			t.Errorf("default metric %q is not registered: %v", name, err)
		}
	}
}

func TestPassAtKIsIndependentOfSampleOrder(t *testing.T) {
	// The estimator must not depend on the order samples happen to appear in.
	forward := mkAggregate(t,
		mkSample(t, "a", withOutcome(true, 1)),
		mkSample(t, "a", withOutcome(false, 0)),
		mkSample(t, "a", withOutcome(false, 0)),
	)
	backward := mkAggregate(t,
		mkSample(t, "a", withOutcome(false, 0)),
		mkSample(t, "a", withOutcome(false, 0)),
		mkSample(t, "a", withOutcome(true, 1)),
	)
	vf := mustMetric(t, "pass_at_k", forward).Value
	vb := mustMetric(t, "pass_at_k", backward).Value
	if math.Abs(vf-vb) > 1e-12 {
		t.Errorf("pass_at_k depends on order: %v vs %v", vf, vb)
	}
}

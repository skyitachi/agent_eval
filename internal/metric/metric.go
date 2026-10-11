// Package metric turns graded attempts into the numbers a benchmark report is
// made of, and lets a dataset define its own.
//
// # Alignment with common practice
//
// The built-in names follow the conventions used by widely cited agent
// benchmarks, so results are comparable in kind if not in dataset:
//
//	resolve_rate        the primary headline number: fraction of attempts that
//	                    fully passed. Equivalent to SWE-bench's "resolved" rate.
//	pass_at_k           unbiased pass@k (Chen et al., 2021): the probability that
//	                    at least one of k samples of a task succeeds. Measures
//	                    capability given retries.
//	pass_hat_k          pass^k: every one of k samples succeeds. Measures
//	                    reliability, and is always <= pass@k.
//	mean_score          mean weighted grader score, retaining partial credit.
//
// Everything else (cost, tokens, turns, latency, tool error rate) exists so a
// headline number can be judged against the resources it took to earn it. A
// resolve rate without a cost figure is not actionable.
//
// # Custom metrics
//
// Two declarative forms cover most needs without writing Go (see custom.go):
//
//	aggregate over samples, optionally filtered
//	arithmetic over other metrics
//
// A Go implementation can also be registered via Register.
package metric

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Kind distinguishes per-sample metrics from run-level aggregates.
type Kind string

const (
	// KindSample is computed for each graded attempt and persisted per row.
	KindSample Kind = "sample"
	// KindRun is aggregated across all attempts of a run.
	KindRun Kind = "run"
)

// Unit describes how a value should be rendered.
type Unit string

const (
	UnitRatio  Unit = "ratio"
	UnitCount  Unit = "count"
	UnitUSD    Unit = "usd"
	UnitMs     Unit = "ms"
	UnitTokens Unit = "tokens"
	UnitText   Unit = "text"
)

// Direction says which way a change in a metric counts as an improvement, so a
// report can colour a delta correctly instead of guessing from the name.
type Direction string

const (
	// DirectionUp means a larger value is better (resolve rate).
	DirectionUp Direction = "up"
	// DirectionDown means a smaller value is better (cost, latency).
	DirectionDown Direction = "down"
	// DirectionNeutral means neither: the number is a size descriptor, and a
	// run with more tasks is not thereby a better run.
	DirectionNeutral Direction = "neutral"
)

// Sample is one graded attempt: the unit every metric is derived from.
type Sample struct {
	RunID       string
	TaskID      string
	Category    string
	Difficulty  string
	Tags        []string
	SampleIndex int
	Attempt     *agent.Attempt
	Grade       *grader.Grade

	// EndReason is why the cell ended, normalised by the orchestrator.
	//
	// It exists because the attempt cannot describe every outcome: when the
	// agent never starts (bad credentials, a sandbox that will not come up, a
	// task-level timeout) there is no attempt to inspect, and an attempt-derived
	// reason would report a healthy "no_attempt" for the very cases the
	// degenerate-outcome rates need to count. Empty falls back to the attempt.
	EndReason string

	// Reward is the scalar training signal for this attempt, already projected
	// through the task's reward contract by the orchestrator.
	//
	// It is carried rather than derived here because the contract belongs to the
	// dataset: mean_reward over a stored run has to mean what the trainer
	// actually received, not what today's default density would have produced.
	Reward float64
	// RewardKind names the density that produced Reward (score|pass).
	RewardKind string
	// Violations lists the anti-gaming guards that fired. A non-empty list means
	// the attempt was rejected: its reward is zero and its grade is not a
	// measurement of capability.
	Violations []string
}

// A *Sample is an optional row: an attempt that never started has no attempt, a
// cancelled cell has no grade, and the filter parser evaluates expressions
// against no row at all while validating syntax. The accessors below therefore
// treat a nil receiver as an all-zero row instead of panicking, matching
// grader.Context.Trace -- callers never need a nil check.

// Passed reports whether the attempt fully passed.
func (s *Sample) Passed() bool { return s != nil && s.Grade != nil && s.Grade.Passed }

// Score returns the weighted grader score in [0,1].
func (s *Sample) Score() float64 {
	if s == nil || s.Grade == nil {
		return 0
	}
	return s.Grade.Score
}

// Trace returns the attempt's trajectory, never nil.
func (s *Sample) Trace() *trace.Trace {
	if s == nil || s.Attempt == nil || s.Attempt.Trace == nil {
		return &trace.Trace{}
	}
	return s.Attempt.Trace
}

// Rejected reports whether an anti-gaming guard rejected this attempt.
func (s *Sample) Rejected() bool { return s != nil && len(s.Violations) > 0 }

// StopReason returns why the attempt ended.
//
// An orchestrator-supplied EndReason wins: it is the only source that can
// describe a cell where no attempt exists at all.
func (s *Sample) StopReason() string {
	if s == nil {
		return "no_attempt"
	}
	if s.EndReason != "" {
		return s.EndReason
	}
	if s.Attempt == nil {
		return "no_attempt"
	}
	return s.Attempt.StopReason()
}

// Numeric exposes every numeric field a filter or aggregate can reference.
//
// Booleans are surfaced as 1/0 so that `mean(passed)` and `sum(timed_out)` work
// without special cases.
func (s *Sample) Numeric() map[string]float64 {
	m := map[string]float64{
		"sample_index": 0,
		"passed":       0,
		"score":        0,
		"turns":        0,
		"tool_calls":   0,
		"tool_errors":  0,
		"cost_usd":     0,
		"latency_ms":   0,
		"ttft_ms":      0,
		"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "cache_read_tokens": 0,
		"timed_out":      0,
		"turns_exceeded": 0,
		"failed":         0,
		"incomplete":     0,
		"grader_errors":  0,
	}
	if s == nil {
		return m
	}
	m["sample_index"] = float64(s.SampleIndex)
	if s.Grade != nil {
		if s.Grade.Passed {
			m["passed"] = 1
		}
		m["score"] = s.Grade.Score
		m["grader_errors"] = float64(len(s.Grade.Errors))
	}
	if s.Attempt != nil {
		tr := s.Attempt.Trace
		if tr != nil {
			m["turns"] = float64(tr.Turns())
			m["tool_calls"] = float64(len(tr.ToolCalls))
			m["tool_errors"] = float64(tr.ToolErrorCount())
			m["input_tokens"] = float64(tr.Result.Usage.InputTokens)
			m["output_tokens"] = float64(tr.Result.Usage.OutputTokens)
			m["total_tokens"] = float64(tr.Result.Usage.Total())
			m["cache_read_tokens"] = float64(tr.Result.Usage.CacheReadInputTokens)
			m["ttft_ms"] = float64(tr.Result.TTFTMs)
			if tr.Incomplete {
				m["incomplete"] = 1
			}
		}
		m["cost_usd"] = s.Attempt.CostUSD()
		m["latency_ms"] = float64(s.Attempt.Duration.Milliseconds())
		if s.Attempt.TimedOut {
			m["timed_out"] = 1
		}
		if s.Attempt.TurnsExceeded {
			m["turns_exceeded"] = 1
		}
		if s.Attempt.Failed {
			m["failed"] = 1
		}
	}
	return m
}

// Strings exposes the textual fields a filter can reference.
func (s *Sample) Strings() map[string]string {
	return map[string]string{
		"task_id":     s.TaskID,
		"category":    s.Category,
		"difficulty":  s.Difficulty,
		"stop_reason": s.StopReason(),
		"run_id":      s.RunID,
	}
}

// Contains reports membership, used by the `tags contains` filter form.
func (s *Sample) Contains(field, value string) bool {
	// A nil sample is a legitimate state: filters are also parsed without a row
	// in order to validate their syntax, and that path must not panic.
	if s == nil {
		return false
	}
	switch field {
	case "tags":
		for _, t := range s.Tags {
			if t == value {
				return true
			}
		}
		return false
	case "task_id", "category", "difficulty", "stop_reason":
		return strings.Contains(s.Strings()[field], value)
	default:
		return false
	}
}

// Aggregate is the full set of samples for one run, plus run metadata.
type Aggregate struct {
	RunID  string
	Label  string
	Sample []Sample

	// SamplesPerTask is the requested k for pass@k. Zero means "infer from the
	// data", which is the number of samples of the most-sampled task.
	SamplesPerTask int

	mu       sync.Mutex
	byTask   map[string][]*Sample
	taskOnce bool
}

// ByTask groups samples by task id, preserving input order within each task.
func (a *Aggregate) ByTask() map[string][]*Sample {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taskOnce {
		return a.byTask
	}
	a.byTask = map[string][]*Sample{}
	for i := range a.Sample {
		s := &a.Sample[i]
		a.byTask[s.TaskID] = append(a.byTask[s.TaskID], s)
	}
	a.taskOnce = true
	return a.byTask
}

// K returns the sample count used for pass@k.
func (a *Aggregate) K() int {
	if a.SamplesPerTask > 0 {
		return a.SamplesPerTask
	}
	max := 0
	for _, group := range a.ByTask() {
		if len(group) > max {
			max = len(group)
		}
	}
	return max
}

// Filter returns the samples matching a predicate.
func (a *Aggregate) Filter(keep func(*Sample) bool) *Aggregate {
	out := &Aggregate{RunID: a.RunID, Label: a.Label, SamplesPerTask: a.SamplesPerTask}
	for i := range a.Sample {
		if keep(&a.Sample[i]) {
			out.Sample = append(out.Sample, a.Sample[i])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

// Value is one computed metric.
type Value struct {
	Name  string         `json:"name"`
	Value float64        `json:"value"`
	Unit  Unit           `json:"unit"`
	Kind  Kind           `json:"kind"`
	Label string         `json:"label,omitempty"`
	Extra map[string]any `json:"extra,omitempty"`
}

// Display renders the value for humans.
func (v Value) Display() string {
	switch v.Unit {
	case UnitRatio:
		return fmt.Sprintf("%.4f", v.Value)
	case UnitUSD:
		return fmt.Sprintf("$%.4f", v.Value)
	case UnitMs:
		return fmt.Sprintf("%.0fms", v.Value)
	case UnitTokens:
		return fmt.Sprintf("%.0f", v.Value)
	case UnitCount:
		return fmt.Sprintf("%.0f", v.Value)
	default:
		return fmt.Sprintf("%g", v.Value)
	}
}

// Map is a name -> value lookup with helpers for derived metrics.
type Map map[string]Value

// Number returns a metric's value, or ok=false when absent.
func (m Map) Number(name string) (float64, bool) {
	if v, ok := m[name]; ok {
		return v.Value, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Metric computes one number from a run's samples.
type Metric interface {
	Name() string
	Kind() Kind
	Unit() Unit
	Description() string
	Compute(agg *Aggregate) (Value, error)
}

// FuncMetric adapts a function into a Metric.
type FuncMetric struct {
	Nm   string
	K    Kind
	U    Unit
	Desc string
	Fn   func(agg *Aggregate) (float64, map[string]any, error)
}

func (m *FuncMetric) Name() string        { return m.Nm }
func (m *FuncMetric) Kind() Kind          { return m.K }
func (m *FuncMetric) Unit() Unit          { return m.U }
func (m *FuncMetric) Description() string { return m.Desc }

func (m *FuncMetric) Compute(agg *Aggregate) (Value, error) {
	v, extra, err := m.Fn(agg)
	if err != nil {
		return Value{}, err
	}
	return Value{Name: m.Nm, Value: v, Unit: m.U, Kind: m.K, Extra: extra}, nil
}

var (
	regMu    sync.RWMutex
	registry = map[string]Metric{}
)

// Register adds a metric. It panics on an empty or duplicate name, both of which
// would make a configured metric resolve to the wrong implementation.
func Register(m Metric) {
	n := strings.TrimSpace(m.Name())
	if n == "" {
		panic("metric: Register called with an empty name")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[n]; dup {
		panic("metric: duplicate registration for " + n)
	}
	registry[n] = m
}

// Get resolves a metric by name.
func Get(name string) (Metric, error) {
	regMu.RLock()
	m, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown metric %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return m, nil
}

// Names lists registered metric names, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NamesOfKind lists registered metrics filtered by kind, sorted.
func NamesOfKind(k Kind) []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n, m := range registry {
		if m.Kind() == k {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Statistics helpers
// ---------------------------------------------------------------------------

// percentile returns the p-th percentile (0..100) using linear interpolation
// between the two closest ranks, matching numpy's default.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func sortedCopy(in []float64) []float64 {
	out := append([]float64(nil), in...)
	sort.Float64s(out)
	return out
}

func mean(in []float64) float64 {
	if len(in) == 0 {
		return 0
	}
	var sum float64
	for _, v := range in {
		sum += v
	}
	return sum / float64(len(in))
}

// PassAtK is the unbiased pass@k estimator of Chen et al. (2021).
//
// It is computed as 1 - C(n-c, k)/C(n, k), evaluated with the product form
// 1 - Π(1 - k/i) for i in (n-c, n] to avoid the overflow that the binomial
// form suffers at realistic n.
func PassAtK(n, c, k int) float64 {
	if n <= 0 || k <= 0 {
		return 0
	}
	if c <= 0 {
		return 0
	}
	if k > n {
		k = n
	}
	if n-c < k {
		// Fewer than k failures means every size-k draw must contain a success.
		return 1
	}
	prod := 1.0
	for i := n - c + 1; i <= n; i++ {
		prod *= 1 - float64(k)/float64(i)
	}
	if prod < 0 {
		prod = 0
	}
	return 1 - prod
}

// PassHatK is pass^k under the tau-bench convention: the estimated probability
// that the agent succeeds k times in a row on this task, computed as (c/n)^k
// from the observed c successes out of n samples.
//
// Note this is a different question from pass@k. pass@k asks "can it ever do
// this", pass^k asks "will it do this reliably", so pass^k <= pass@k always and
// it decreases as k grows. With a single sample (n = k = 1) it reduces to the
// plain pass rate.
func PassHatK(successes, n, k int) float64 {
	if n <= 0 || k <= 0 {
		return 0
	}
	p := float64(successes) / float64(n)
	return math.Pow(p, float64(k))
}

// traceView was removed in favour of returning the concrete trace type, which
// callers need anyway for tool-call and turn introspection.

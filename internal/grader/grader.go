// Package grader decides whether an agent attempt succeeded, and with what
// partial credit.
//
// # Scoring model
//
// Every grader returns a Result with a Score in [0,1]. A task's score is the
// weighted mean over its graders whose weight is positive:
//
//	score = Σ(weight_i × score_i) / Σ(weight_i)
//
// A task passes when every grader marked `required` passed AND the score reaches
// the task threshold (default 1.0). Requiring a full score by default matches how
// benchmarks such as SWE-bench define "resolved" -- all target tests must pass --
// while the weighted score still exposes partial progress for analysis. Lower the
// threshold to accept partial credit as a pass.
//
// # Extending
//
// Custom graders register through Register and are then selectable by name from a
// dataset's `graders[].type`. The `command` grader covers the common case of
// shelling out to a project-specific checker without writing Go at all.
package grader

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
)

// HarnessDir is a dotted directory the harness owns inside the workspace.
//
// It holds generated support files (injected suites, settings). Because it is
// created by the harness and not by the agent, it is always excluded from the
// diff so it can never influence a patch-based score.
//
// The name lives in the sandbox package because that is what lays out a
// workspace, and a run that works in a directory it did not create has to be
// able to remove this one without importing the graders.
const HarnessDir = sandbox.HarnessDir

// HarnessIgnore are glob patterns always removed from diffs.
var HarnessIgnore = []string{HarnessDir + "/**", HarnessDir}

// ---------------------------------------------------------------------------
// Grade context
// ---------------------------------------------------------------------------

// Context carries everything a grader may inspect. It is constructed once per
// graded attempt and shared by all graders for that attempt.
//
// The diff and changed-path views are computed lazily and memoised, because a
// task typically has several patch-oriented graders and running `git diff` once
// per grader is wasteful.
type Context struct {
	Task     dataset.TaskSpec
	Sandbox  sandbox.Sandbox
	Baseline *sandbox.Baseline
	Attempt  *agent.Attempt

	// DiffOverride, when non-nil, replaces the computed diff. Used by tests and
	// by replaying a stored run without a live sandbox.
	DiffOverride *string

	mu           sync.Mutex
	diff         *string
	changed      []string
	diffErr      error
	computedDiff bool
	memo         map[string]any
}

// Memo computes a value once per grade context and caches it.
//
// Grading a task runs several graders against the same sandbox, and some
// preparation is expensive or has side effects (probing for a Python
// interpreter that has pytest installed). Memoising on the context rather than
// globally keeps the cache correct across different sandboxes and backends.
func (c *Context) Memo(key string, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if c.memo != nil {
		if v, ok := c.memo[key]; ok {
			c.mu.Unlock()
			return v, nil
		}
	}
	c.mu.Unlock()

	v, err := fn()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.memo == nil {
		c.memo = map[string]any{}
	}
	c.memo[key] = v
	c.mu.Unlock()
	return v, nil
}

// NewContext builds a grade context.
func NewContext(task dataset.TaskSpec, sb sandbox.Sandbox, base *sandbox.Baseline, att *agent.Attempt) *Context {
	return &Context{Task: task, Sandbox: sb, Baseline: base, Attempt: att}
}

// Trace returns the agent trajectory, or an empty trace when none exists so
// callers never need a nil check.
func (c *Context) Trace() *trace.Trace {
	if c.Attempt == nil || c.Attempt.Trace == nil {
		return &trace.Trace{}
	}
	return c.Attempt.Trace
}

// FinalAnswer is the agent's terminal text output.
func (c *Context) FinalAnswer() string {
	if c.Attempt == nil {
		return ""
	}
	return c.Attempt.FinalText()
}

// diffOptions derives diff filters from the task's output contract.
func (c *Context) diffOptions() sandbox.DiffOptions {
	opts := sandbox.DiffOptions{IgnorePaths: append([]string{}, HarnessIgnore...)}
	if c.Task.Output != nil {
		opts.Paths = c.Task.Output.Paths
		opts.IgnorePaths = append(opts.IgnorePaths, c.Task.Output.IgnorePaths...)
	}
	return opts
}

// prepare computes the diff and changed-path list once.
func (c *Context) prepare() (string, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.computedDiff {
		return deref(c.diff), c.changed, c.diffErr
	}
	c.computedDiff = true

	if c.DiffOverride != nil {
		c.diff = c.DiffOverride
		// Derive the changed file list from the patch itself so a replayed run
		// behaves the same as a live one.
		c.changed = pathsFromDiff(*c.DiffOverride)
		return *c.diff, c.changed, nil
	}
	if c.Baseline == nil {
		c.diffErr = fmt.Errorf("no baseline available to diff against")
		return "", nil, c.diffErr
	}
	opts := c.diffOptions()
	changed, err := c.Baseline.ChangedPaths(context.Background(), opts)
	if err != nil {
		c.diffErr = err
		return "", nil, err
	}
	d, err := c.Baseline.Diff(context.Background(), opts)
	if err != nil {
		c.diffErr = err
		return "", nil, err
	}
	c.diff = &d
	c.changed = changed
	return d, changed, nil
}

// Diff returns the (filtered) unified diff produced by the agent.
func (c *Context) Diff() (string, error) {
	d, _, err := c.prepare()
	return d, err
}

// ChangedPaths returns workspace-relative paths the agent modified, created or
// deleted, after the task's output filters are applied.
func (c *Context) ChangedPaths() ([]string, error) {
	_, ch, err := c.prepare()
	return ch, err
}

// Env returns the workspace env declared by the task, as a flat map.
func (c *Context) Env() map[string]string {
	if len(c.Task.Workspace.Env) == 0 {
		return nil
	}
	m := make(map[string]string, len(c.Task.Workspace.Env))
	for _, e := range c.Task.Workspace.Env {
		m[e.Name] = e.Value
	}
	return m
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// pathsFromDiff extracts the touched paths from a unified diff, handling the
// quoted form git emits for non-ASCII or special filenames.
func pathsFromDiff(diff string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+++ ") && !strings.HasPrefix(line, "--- ") {
			continue
		}
		p := strings.TrimSpace(line[4:])
		if p == "/dev/null" {
			continue
		}
		p = strings.TrimPrefix(p, "b/")
		p = strings.TrimPrefix(p, "a/")
		if unquoted, ok := unquoteGitPath(p); ok {
			p = unquoted
		}
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// unquoteGitPath decodes git's C-style quoting of paths containing unusual
// bytes (Chinese filenames, spaces, control characters).
func unquoteGitPath(s string) (string, bool) {
	if len(s) < 2 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", false
	}
	var sb strings.Builder
	body := s[1 : len(s)-1]
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 >= len(body) {
			sb.WriteByte(body[i])
			continue
		}
		i++
		switch body[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case '\\', '"':
			sb.WriteByte(body[i])
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// Octal escape: up to three digits.
			j := i
			for j < len(body) && j < i+3 && body[j] >= '0' && body[j] <= '7' {
				j++
			}
			var v int
			for _, c := range body[i:j] {
				v = v*8 + int(c-'0')
			}
			sb.WriteByte(byte(v))
			i = j - 1
		default:
			sb.WriteByte(body[i])
		}
	}
	return sb.String(), true
}

// ---------------------------------------------------------------------------
// Grader interface and registry
// ---------------------------------------------------------------------------

// Result is one grader's verdict.
type Result struct {
	// Grader is the configured name (GraderSpec.Name).
	Grader string `json:"grader"`
	// Type is the registered grader type.
	Type string `json:"type"`
	// Passed is the pass/fail verdict for this grader in isolation.
	Passed bool `json:"passed"`
	// Score is partial credit in [0,1].
	Score float64 `json:"score"`
	// Weight is copied from the spec so reports can reproduce the composite.
	Weight float64 `json:"weight"`
	// Required marks a grader that must pass for the task to pass.
	Required bool `json:"required"`
	// Reason is a short human-readable justification.
	Reason string `json:"reason,omitempty"`
	// Details carries grader-specific evidence (counts, extracted answers, ...).
	Details map[string]any `json:"details,omitempty"`
	// Duration is the grader's own runtime.
	Duration time.Duration `json:"duration"`
	// Error is set when the grader could not run at all. Such a result is
	// treated as a failure and flagged for the report, because silently scoring
	// 0 would hide a broken harness.
	Error string `json:"error,omitempty"`
}

// Grader is implemented by every grading strategy.
type Grader interface {
	// Type is the registry key used in a dataset's graders[].type.
	Type() string
	// Grade evaluates one attempt.
	Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Grader{}
)

// Register adds a grader to the global registry. It panics on duplicates or an
// empty type, both of which are programming errors.
func Register(g Grader) {
	t := strings.TrimSpace(g.Type())
	if t == "" {
		panic("grader: Register called with an empty type")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[t]; dup {
		panic("grader: duplicate registration for type " + t)
	}
	registry[t] = g
}

// Get resolves a grader by type.
func Get(typ string) (Grader, error) {
	regMu.RLock()
	g, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown grader type %q (available: %s)", typ, strings.Join(Registered(), ", "))
	}
	return g, nil
}

// Registered lists known grader types in sorted order.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Config decoding
// ---------------------------------------------------------------------------

// DecodeConfig maps a grader's untyped Config map onto a typed struct.
//
// It round-trips through JSON, which means graders get the same strictness and
// the same error messages as the rest of the configuration surface, and an
// unknown key is reported instead of silently ignored.
func DecodeConfig(spec dataset.GraderSpec, out any) error {
	if len(spec.Config) == 0 {
		return nil
	}
	raw, err := json.Marshal(spec.Config)
	if err != nil {
		return fmt.Errorf("grader %s: encode config: %w", spec.Name, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("grader %s (%s): invalid config: %w", spec.Name, spec.Type, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Running a task's graders
// ---------------------------------------------------------------------------

// Threshold is the default score required for a task to count as passed.
const Threshold = 1.0

// Grade is the aggregated verdict for one task attempt.
type Grade struct {
	TaskID  string   `json:"task_id"`
	Results []Result `json:"results"`

	// Score is the weighted mean of the graders with positive weight.
	Score float64 `json:"score"`
	// Passed is true when all required graders passed and Score >= Threshold.
	Passed bool `json:"passed"`
	// Threshold is the cutoff applied to Score.
	Threshold float64 `json:"threshold"`

	Duration time.Duration `json:"duration"`
	// Errors lists grader-level infrastructure failures. A non-empty list means
	// the verdict should be treated as unreliable.
	Errors []string `json:"errors,omitempty"`
}

// RunTask executes every grader configured on the task and aggregates them.
//
// Graders run sequentially and share the context's memoised diff. They are not
// parallelised because several of them mutate the sandbox (injecting suites,
// running test commands), and interleaving those would make failures
// non-reproducible.
func RunTask(ctx context.Context, gc *Context, threshold float64) (*Grade, error) {
	start := time.Now()
	grade := &Grade{TaskID: gc.Task.ID, Threshold: threshold}

	num, den := 0.0, 0.0
	allRequiredPassed := true
	weightedCount := 0

	for _, spec := range gc.Task.Graders {
		g, err := Get(spec.Type)
		if err != nil {
			// A misconfigured dataset must not look like an agent failure.
			return nil, fmt.Errorf("task %s: %w", gc.Task.ID, err)
		}

		res, err := runOne(ctx, g, gc, spec)
		if err != nil {
			return nil, fmt.Errorf("task %s grader %s: %w", gc.Task.ID, spec.Name, err)
		}

		grade.Results = append(grade.Results, *res)
		if res.Error != "" {
			grade.Errors = append(grade.Errors, fmt.Sprintf("%s: %s", res.Grader, res.Error))
		}
		if spec.Required && !res.Passed {
			allRequiredPassed = false
		}
		if w := spec.EffectiveWeight(); w > 0 {
			num += w * res.Score
			den += w
			weightedCount++
			if res.Error != "" {
				// An errored grader cannot be scored; treat its contribution as
				// zero, which is already the case, but do not let it pass.
				allRequiredPassed = allRequiredPassed && false
			}
		}
	}

	if den > 0 {
		grade.Score = num / den
	} else if weightedCount == 0 {
		return nil, fmt.Errorf("task %s: no grader has a positive weight", gc.Task.ID)
	}

	grade.Passed = allRequiredPassed && grade.Score >= threshold && len(grade.Errors) == 0
	grade.Duration = time.Since(start)
	return grade, nil
}

// runOne executes a single grader with its own timeout, converting panics and
// timeouts into an errored Result rather than aborting the whole run.
func runOne(ctx context.Context, g Grader, gc *Context, spec dataset.GraderSpec) (res *Result, err error) {
	runCtx := ctx
	if spec.TimeoutS > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutS)*time.Second)
		defer cancel()
	}

	res = &Result{
		Grader:   spec.Name,
		Type:     spec.Type,
		Weight:   spec.EffectiveWeight(),
		Required: spec.Required,
	}

	done := make(chan struct{})
	var inner *Result
	var innerErr error
	go func() {
		defer func() {
			if r := recover(); r != nil {
				innerErr = fmt.Errorf("grader panicked: %v", r)
			}
			close(done)
		}()
		inner, innerErr = g.Grade(runCtx, gc, spec)
	}()

	select {
	case <-done:
	case <-runCtx.Done():
		res.Error = fmt.Sprintf("grader timed out after %ds", spec.TimeoutS)
		res.Reason = "grading timed out"
		return res, nil
	}

	if innerErr != nil {
		res.Error = innerErr.Error()
		res.Reason = "grader failed to run; verdict is unreliable"
		return res, nil
	}
	if inner == nil {
		res.Error = "grader returned no result"
		return res, nil
	}
	// Preserve identity fields even if the grader left them empty.
	if inner.Grader == "" {
		inner.Grader = res.Grader
	}
	if inner.Type == "" {
		inner.Type = res.Type
	}
	inner.Weight = res.Weight
	inner.Required = res.Required
	inner.Score = clamp01(inner.Score)
	return inner, nil
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

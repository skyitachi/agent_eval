package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/metric"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// Durable artifacts
// ---------------------------------------------------------------------------

// Artifact file names written for every cell.
//
// The workspace itself is usually deleted after a task, so this bundle is what
// survives a run. It does not duplicate what the database already stores
// losslessly: the raw stream in particular is deliberately kept out of SQLite
// (it is large and append-only) and is exactly what a trajectory viewer needs to
// re-parse a run without re-running it.
const (
	artifactStream  = "stream.jsonl"
	artifactAttempt = "attempt.json"
	artifactGrade   = "grade.json"
	artifactDiff    = "diff.patch"
	artifactAnswer  = "answer.txt"
	artifactTask    = "task.json"
)

// writeCellArtifacts writes the durable artifact bundle outside the sandbox.
//
// Failures are recorded on the result rather than returned: losing a debugging
// convenience must not turn a graded attempt into a failed one.
func (e *Engine) writeCellArtifacts(tr *TaskResult, task dataset.TaskSpec, baseline *sandbox.Baseline) {
	if tr.ArtifactDir == "" {
		return
	}
	write := func(name string, data []byte) {
		if len(data) == 0 {
			return
		}
		if err := os.WriteFile(filepath.Join(tr.ArtifactDir, name), data, 0o644); err != nil {
			tr.Error = appendErr(tr.Error, fmt.Errorf("write %s: %w", name, err))
		}
	}

	write(artifactAttempt, indentJSON(tr.Attempt))
	write(artifactGrade, indentJSON(tr.Grade))
	write(artifactDiff, []byte(tr.Diff))
	write(artifactAnswer, []byte(tr.Answer))
	// The baseline SHA is recorded so a stored diff can be traced back to the
	// exact commit it was taken against.
	write(artifactTask, indentJSON(struct {
		dataset.TaskSpec
		Baseline string `json:"_baseline,omitempty"`
	}{TaskSpec: task, Baseline: baseline.String()}))
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// createRunRow inserts the run row so progress is visible while the run is still
// going, and so results can reference it by foreign key.
//
// When the run already exists -- which is exactly the resume case -- the row is
// left alone apart from flipping it back to running: the original created_at and
// provenance describe when the work actually started, and re-inserting would
// fail on the primary key.
func (e *Engine) createRunRow(ctx context.Context, ds *dataset.Dataset, total int) error {
	if e.cfg.Store == nil {
		return nil
	}
	if _, err := e.cfg.Store.GetRun(ctx, e.cfg.RunID); err == nil {
		if err := e.cfg.Store.ReopenRun(ctx, e.cfg.RunID); err != nil {
			return fmt.Errorf("reopen run: %w", err)
		}
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("look up run: %w", err)
	}

	cfgJSON := marshalString(e.redactedConfig())
	rec := &storage.RunRecord{
		ID:             e.cfg.RunID,
		Name:           e.cfg.RunName,
		Status:         storage.StatusRunning,
		DatasetName:    ds.Manifest.Name,
		DatasetVersion: ds.Manifest.Version,
		DatasetHash:    ds.Hash,
		DatasetPath:    ds.Dir,
		AgentKind:      e.cfg.Agent.Kind,
		AgentModel:     e.cfg.Agent.Model,
		SandboxBackend: string(e.cfg.Backend.Kind),
		SandboxImage:   e.cfg.Backend.Image,
		Mode:           string(e.cfg.Mode),
		SamplesPerTask: e.cfg.SamplesPerTask,
		Concurrency:    e.cfg.Concurrency,
		Seed:           e.cfg.Seed,
		ConfigJSON:     cfgJSON,
		ConfigHash:     hashString(cfgJSON, ds.Hash, e.cfg.Agent.Model, fmt.Sprint(total)),
		Notes:          e.cfg.Notes,
		ToolVersion:    e.cfg.ToolVersion,
		HostArch:       runtime.GOOS + "/" + runtime.GOARCH,
	}
	if err := e.cfg.Store.CreateRun(ctx, rec); err != nil {
		return fmt.Errorf("record run: %w", err)
	}
	return nil
}

// saveCell persists one graded attempt.
func (e *Engine) saveCell(ctx context.Context, tr *TaskResult) error {
	if e.cfg.Store == nil || tr == nil {
		return nil
	}
	if err := e.cfg.Store.SaveTaskRun(ctx, e.cellRecord(tr)); err != nil {
		return fmt.Errorf("save task %s: %w", tr.TaskID, err)
	}
	return nil
}

// cellRecord converts a result into its storage row.
//
// The JSON blobs are the marshalled attempt, trace and grader results, so a
// stored run can be re-rendered -- and its verdicts audited -- without the
// sandbox that produced it.
func (e *Engine) cellRecord(tr *TaskResult) *storage.TaskRunRecord {
	rec := &storage.TaskRunRecord{
		// The row id must be unique per (task, sample) and stable across
		// re-saves; the resume key already has exactly those properties.
		ID:          e.cfg.RunID + "/" + storage.TaskKey(tr.TaskID, tr.SampleIndex),
		RunID:       e.cfg.RunID,
		TaskID:      tr.TaskID,
		SampleIndex: tr.SampleIndex,
		Category:    tr.Category,
		Difficulty:  tr.Difficulty,
		Tags:        tr.Tags,
		Status:      tr.Status,
		StopReason:  tr.StopReason,
		Passed:      tr.Passed(),
		Score:       tr.Score(),
		Threshold:   e.cfg.GraderThreshold,
		Reward:      tr.Reward,
		RewardKind:  tr.RewardKind,
		Violations:  tr.Violations,
		Restored:    tr.Restored,
		Diff:        tr.Diff,
		Answer:      tr.Answer,
		Error:       tr.Error,

		WorkspaceDir:  tr.WorkspaceDir,
		RawStreamPath: tr.RawStreamPath,

		StartedAt:  timePtr(tr.StartedAt),
		EndedAt:    timePtr(tr.EndedAt),
		DurationMs: tr.Duration().Milliseconds(),
	}

	if att := tr.Attempt; att != nil {
		rec.SessionID = att.SessionID
		rec.Model = att.Model
		rec.CostUSD = att.CostUSD()
		u := att.Usage()
		rec.InputTokens = u.InputTokens
		rec.OutputTokens = u.OutputTokens
		rec.CacheReadTokens = u.CacheReadInputTokens
		rec.CacheCreateTokens = u.CacheCreationInputTokens
		if t := att.Trace; t != nil {
			rec.Turns = t.Turns()
			rec.ToolCalls = len(t.ToolCalls)
			rec.ToolErrors = t.ToolErrorCount()
			rec.CostReliable = t.CostReliable()
			rec.TTFTMs = t.Result.TTFTMs
			rec.APIDurationMs = t.Result.APIDurationMs
		}
		rec.AttemptJSON = marshalString(att)
		rec.TraceJSON = marshalString(att.Trace)
	}
	if g := tr.Grade; g != nil {
		rec.GradesJSON = marshalString(g.Results)
		// The applied threshold can differ from the run default when a task
		// overrode it, so prefer the grade's own record.
		rec.Threshold = g.Threshold
	}
	return rec
}

// computeMetrics evaluates the built-in and custom metrics over the run.
func (e *Engine) computeMetrics(res *Result) error {
	agg := metricAggregate(res)

	runner := &metric.Runner{Names: e.cfg.Metrics, Customs: e.cfg.CustomMetrics}
	values, err := runner.Evaluate(agg)
	if err != nil {
		return fmt.Errorf("compute metrics: %w", err)
	}
	res.Metrics = values

	if e.cfg.Store == nil {
		return nil
	}
	rows := make([]storage.MetricRecord, 0, len(values))
	for name, v := range values {
		extra := ""
		if len(v.Extra) > 0 {
			extra = marshalString(v.Extra)
		}
		rows = append(rows, storage.MetricRecord{
			RunID:     res.RunID,
			Name:      name,
			Value:     v.Value,
			Unit:      string(v.Unit),
			Kind:      string(v.Kind),
			Label:     v.Label,
			ExtraJSON: extra,
		})
	}
	// Detached: metrics are computed after the run context may already have been
	// cancelled, and a partial run's numbers are still worth storing.
	if err := e.cfg.Store.SaveMetrics(context.WithoutCancel(context.Background()), res.RunID, rows); err != nil {
		return fmt.Errorf("save metrics: %w", err)
	}
	return nil
}

// finishRun marks the run terminal.
func (e *Engine) finishRun(ctx context.Context, res *Result) error {
	if e.cfg.Store == nil {
		return nil
	}
	// Detached for the same reason as the metrics write: recording *that* a run
	// was cancelled must survive the cancellation.
	ctx = context.WithoutCancel(ctx)
	if err := e.cfg.Store.FinishRun(ctx, res.RunID, res.Status, res.Error, res.Duration); err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if e.cfg.ToolVersion != "" {
		_ = e.cfg.Store.SetRunTag(ctx, res.RunID, "tool_version", e.cfg.ToolVersion)
	}
	return nil
}

// redactedConfig renders the effective configuration for storage.
//
// Secret values are dropped: a run row is meant to be shareable, and env maps
// routinely carry API keys.
func (e *Engine) redactedConfig() map[string]any {
	agentCopy := e.cfg.Agent
	agentCopy.Env = redactEnv(agentCopy.Env)
	backend := e.cfg.Backend
	backend.Env = redactEnv(backend.Env)
	return map[string]any{
		"mode":             string(e.cfg.Mode),
		"agent":            agentCopy,
		"backend":          backend,
		"samples_per_task": e.cfg.SamplesPerTask,
		"concurrency":      e.cfg.Concurrency,
		"seed":             e.cfg.Seed,
		"agent_timeout":    e.cfg.AgentTimeout.String(),
		"task_timeout":     e.cfg.TaskTimeout.String(),
		"grader_threshold": e.cfg.GraderThreshold,
		"retries":          e.cfg.Retries,
		"metrics":          e.cfg.Metrics,
		"custom_metrics":   e.cfg.CustomMetrics,
	}
}

// redactEnv replaces values with a marker, keeping the key names: key names are
// provenance ("this run had ANTHROPIC_BASE_URL set"), values are credentials.
func redactEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k := range env {
		out[k] = "***"
	}
	return out
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// metricAggregate projects a run result onto the metric package's view.
//
// Cells that never ran still become samples: they carry a nil attempt and grade,
// which the metric layer reads as zeroes and as a distinct stop reason. Dropping
// them would inflate every rate by shrinking the denominator.
func metricAggregate(res *Result) *metric.Aggregate {
	agg := &metric.Aggregate{
		RunID:          res.RunID,
		Label:          res.RunName,
		SamplesPerTask: res.SamplesPerTask,
		Sample:         make([]metric.Sample, 0, len(res.Tasks)),
	}
	for _, tr := range res.Tasks {
		agg.Sample = append(agg.Sample, metric.Sample{
			RunID:       res.RunID,
			TaskID:      tr.TaskID,
			Category:    tr.Category,
			Difficulty:  tr.Difficulty,
			Tags:        tr.Tags,
			SampleIndex: tr.SampleIndex,
			Attempt:     tr.Attempt,
			Grade:       tr.Grade,
			// The engine's normalised reason is authoritative: it is the only
			// value that exists when the agent never started.
			EndReason: tr.StopReason,
			// Reward is carried, not re-derived: see metric.Sample.Reward.
			Reward:     tr.Reward,
			RewardKind: tr.RewardKind,
			Violations: tr.Violations,
		})
	}
	return agg
}

func marshalString(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

func indentJSON(v any) []byte {
	// A typed nil pointer inside an interface (`var a *Agent.Attempt; indentJSON(a)`)
	// must not be marshalled into the literal string "null".
	if v == nil {
		return nil
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil || string(raw) == "null" {
		return nil
	}
	return raw
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func hashString(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

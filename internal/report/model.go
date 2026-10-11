// Package report renders evaluation results as self-contained HTML.
//
// # Why a view model instead of the storage types
//
// The report is a *function of the database*, not of the process that produced
// it. A report generated two weeks later from a copied .db file must be
// byte-identical to one generated on the machine that ran the evaluation. That
// only holds if the renderer reads nothing but persisted state, so the storage
// rows are projected onto the types below first and everything downstream --
// HTML, the web UI, the comparison page -- works on that projection.
//
// # Why the data travels as JSON inside the page
//
// A report is opened from a file:// URL or an email attachment as often as from
// a server, and the interesting interactions (filter, sort, expand a
// trajectory) must work with no network. So the page ships a shell plus one JSON
// document and lets the browser do the rest. The same JSON is what the web UI's
// API returns, which is why the file report and the served report cannot drift.
package report

import "time"

// DataVersion guards the embedded payload against a stale renderer. Bump it when
// the JSON shape changes in a way an older app.js cannot read.
const DataVersion = 1

// Options controls what a report embeds.
//
// Every limit exists because reports have two opposing failure modes: too big
// to open (a 900-task SWE-bench run with full trajectories is hundreds of
// megabytes) and too lossy to debug. The defaults favour completeness and the
// builder degrades deterministically -- dropping *passing* task detail before
// failing detail -- rather than silently producing an unusable file.
type Options struct {
	// Title overrides the document title.
	Title string
	// GeneratedAt stamps the report; zero means time.Now.
	GeneratedAt time.Time

	// MaxEventsPerTask bounds how many normalized trajectory events are kept
	// per task. The head and the tail are kept, because both the opening
	// instructions and the final answer matter.
	MaxEventsPerTask int
	// MaxToolCallsPerTask bounds the per-task tool-call table.
	MaxToolCallsPerTask int
	// MaxFieldBytes clips a single text field (diff, answer, tool output,
	// event text). Kept generous: a truncated stack trace is worthless.
	MaxFieldBytes int

	// MaxInlineBytes budgets the whole serialized payload. Once the budget is
	// exhausted, remaining *passing* tasks lose their trajectory/diff detail;
	// failing tasks are always detailed first.
	MaxInlineBytes int

	// Artifacts maps a cell key ("taskID#sampleIndex") to the directory holding
	// the engine's artifact bundle. When absent the directory is derived from
	// the stored raw-stream path. This is what lets the report show the task
	// prompt and definition, which live in task.json rather than in SQLite.
	Artifacts map[string]string

	// IncludeRawConfig keeps the redacted configuration JSON on the run. It is
	// already stored, so this only decides whether the report carries it.
	IncludeRawConfig bool
}

// withDefaults resolves an Options into a usable one.
func (o Options) withDefaults() Options {
	if o.MaxEventsPerTask <= 0 {
		o.MaxEventsPerTask = 400
	}
	if o.MaxToolCallsPerTask <= 0 {
		o.MaxToolCallsPerTask = 200
	}
	if o.MaxFieldBytes <= 0 {
		o.MaxFieldBytes = 20000
	}
	// 8 MiB of JSON inflates to roughly 12 MiB of HTML once the page is
	// pretty-printed by a browser. That opens instantly on any machine made in
	// the last decade and still fits a 300-task run with every failure detailed.
	if o.MaxInlineBytes <= 0 {
		o.MaxInlineBytes = 8 << 20
	}
	if o.GeneratedAt.IsZero() {
		o.GeneratedAt = time.Now()
	}
	return o
}

// ---------------------------------------------------------------------------
// Payload
// ---------------------------------------------------------------------------

// Data is everything a rendered report needs.
type Data struct {
	Version     int         `json:"version"`
	Generated   string      `json:"generated"`
	Title       string      `json:"title"`
	Run         RunInfo     `json:"run"`
	Metrics     []Metric    `json:"metrics"`
	Breakdowns  []Breakdown `json:"breakdowns"`
	Tasks       []Task      `json:"tasks"`
	Counts      Counts      `json:"counts"`
	Warnings    []string    `json:"warnings,omitempty"`
	Compare     *Compare    `json:"compare,omitempty"`
	Description []string    `json:"description,omitempty"`
}

// RunInfo is the run's provenance and headline metadata.
type RunInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMs int64  `json:"duration_ms"`

	DatasetName    string `json:"dataset_name"`
	DatasetVersion string `json:"dataset_version,omitempty"`
	DatasetHash    string `json:"dataset_hash,omitempty"`
	DatasetPath    string `json:"dataset_path,omitempty"`

	AgentKind      string `json:"agent_kind"`
	AgentModel     string `json:"agent_model,omitempty"`
	SandboxBackend string `json:"sandbox_backend"`
	SandboxImage   string `json:"sandbox_image,omitempty"`

	// Mode says who acted: agent, oracle, baseline or rl. It is rendered rather
	// than hidden because the four produce identically shaped results, and an
	// unlabelled oracle run reads as an unusually good model.
	Mode string `json:"mode,omitempty"`

	SamplesPerTask int   `json:"samples_per_task"`
	Concurrency    int   `json:"concurrency"`
	Seed           int64 `json:"seed"`

	ConfigHash  string `json:"config_hash,omitempty"`
	ConfigJSON  string `json:"config_json,omitempty"`
	Notes       string `json:"notes,omitempty"`
	ToolVersion string `json:"tool_version,omitempty"`
	HostArch    string `json:"host_arch,omitempty"`
}

// Counts is the cell tally, kept separate so the header never has to reduce the
// task list in JavaScript.
type Counts struct {
	// Samples counts cells; Tasks counts distinct task ids. They differ whenever
	// SamplesPerTask > 1, and both are worth showing.
	Samples        int `json:"samples"`
	Tasks          int `json:"tasks"`
	Passed         int `json:"passed"`
	TasksPassed    int `json:"tasks_passed"`
	Failed         int `json:"failed"`
	Skipped        int `json:"skipped"`
	Errored        int `json:"errored"`
	SamplesPerTask int `json:"samples_per_task"`
}

// Metric is one computed number, pre-rendered so the browser needs no unit
// formatting logic of its own.
type Metric struct {
	Name        string         `json:"name"`
	Label       string         `json:"label,omitempty"`
	Value       float64        `json:"value"`
	Display     string         `json:"display"`
	Unit        string         `json:"unit"`
	Kind        string         `json:"kind"`
	Description string         `json:"description,omitempty"`
	Group       string         `json:"group"`
	Direction   string         `json:"direction,omitempty"` // up | down | neutral
	Extra       map[string]any `json:"extra,omitempty"`
}

// Breakdown is a named distribution rendered as bars (resolve rate per
// category, stop-reason mix, ...).
type Breakdown struct {
	Title string         `json:"title"`
	Note  string         `json:"note,omitempty"`
	Rows  []BreakdownRow `json:"rows"`
}

// BreakdownRow is one bar.
type BreakdownRow struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Text  string  `json:"text"`
	Total int     `json:"total,omitempty"`
	Hits  int     `json:"hits,omitempty"`
}

// Task is one graded cell, the unit the detail panel renders.
type Task struct {
	Key         string   `json:"key"`
	ID          string   `json:"id"`
	SampleIndex int      `json:"sample_index"`
	Category    string   `json:"category,omitempty"`
	Difficulty  string   `json:"difficulty,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	Status     string  `json:"status"`
	StopReason string  `json:"stop_reason,omitempty"`
	Passed     bool    `json:"passed"`
	Score      float64 `json:"score"`
	Threshold  float64 `json:"threshold"`

	// Reward is the scalar a training loop is paid, projected through the
	// task's declared contract. It is reported next to Passed and Score rather
	// than instead of them: the two answer different questions, and a reader
	// comparing a reward against a score needs to see both to tell a sparse
	// contract from a wrong answer.
	Reward     float64 `json:"reward"`
	RewardKind string  `json:"reward_kind,omitempty"`
	// Rejected marks an attempt that tripped an anti-gaming guard. Its reward is
	// zero, and the distinction between "wrong" and "tampered" is the point.
	Rejected   bool     `json:"rejected,omitempty"`
	Violations []string `json:"violations,omitempty"`
	// Restored names the paths reset to the baseline before grading, so a
	// reader can see that a shortcut was removed rather than that the agent
	// obeyed.
	Restored []string `json:"restored,omitempty"`

	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	DurationMs int64  `json:"duration_ms"`

	CostUSD      float64 `json:"cost_usd"`
	CostReliable bool    `json:"cost_reliable"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CacheRead    int     `json:"cache_read_tokens"`
	CacheCreate  int     `json:"cache_create_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	Turns        int     `json:"turns"`
	ToolCalls    int     `json:"tool_calls"`
	ToolErrors   int     `json:"tool_errors"`
	TTFTMs       int64   `json:"ttft_ms"`
	APIDuration  int64   `json:"api_duration_ms"`

	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Error     string `json:"error,omitempty"`

	// Attempt-level diagnostics, from the stored attempt blob.
	ExitCode      *int     `json:"exit_code,omitempty"`
	TimedOut      bool     `json:"timed_out,omitempty"`
	TurnsExceeded bool     `json:"turns_exceeded,omitempty"`
	AgentFailed   bool     `json:"agent_failed,omitempty"`
	CommandLine   string   `json:"command_line,omitempty"`
	Stderr        string   `json:"stderr,omitempty"`
	ParseErrors   []string `json:"parse_errors,omitempty"`
	Incomplete    bool     `json:"incomplete,omitempty"`

	// Definition is the task contract, read from the artifact bundle when it is
	// reachable; nil when the bundle is gone.
	Definition *Definition `json:"definition,omitempty"`

	Diff   string `json:"diff,omitempty"`
	Answer string `json:"answer,omitempty"`

	Graders []Grader   `json:"graders,omitempty"`
	Tools   []ToolCall `json:"tools,omitempty"`
	Events  []Event    `json:"events,omitempty"`

	// Detail records why this task carries less than the full trajectory.
	// Empty means nothing was dropped.
	Detail string `json:"detail,omitempty"`

	ArtifactDir   string `json:"artifact_dir,omitempty"`
	WorkspaceDir  string `json:"workspace_dir,omitempty"`
	RawStreamPath string `json:"raw_stream_path,omitempty"`
}

// Definition is the task as declared in the dataset, recovered from task.json.
type Definition struct {
	Prompt       string         `json:"prompt,omitempty"`
	SystemPrompt string         `json:"system_prompt,omitempty"`
	Version      string         `json:"version,omitempty"`
	Workspace    string         `json:"workspace,omitempty"`
	Output       string         `json:"output,omitempty"`
	Limits       string         `json:"limits,omitempty"`
	Graders      []string       `json:"graders,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Baseline     string         `json:"baseline,omitempty"`
	Raw          string         `json:"raw,omitempty"`
}

// Grader is one grader verdict.
type Grader struct {
	Grader     string         `json:"grader"`
	Type       string         `json:"type"`
	Passed     bool           `json:"passed"`
	Score      float64        `json:"score"`
	Weight     float64        `json:"weight"`
	Required   bool           `json:"required"`
	Reason     string         `json:"reason,omitempty"`
	Error      string         `json:"error,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	Details    map[string]any `json:"details,omitempty"`
}

// ToolCall is one flattened tool invocation.
type ToolCall struct {
	Seq        int    `json:"seq"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	IsError    bool   `json:"is_error"`
	DurationMs int64  `json:"duration_ms"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
}

// Event is one normalized trajectory step.
type Event struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolInput string `json:"tool_input,omitempty"`
	ToolOut   string `json:"tool_output,omitempty"`
	ToolError bool   `json:"tool_error,omitempty"`
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

// Compare is a side-by-side of two runs. It reuses the task and metric types so
// the same client-side renderer handles both modes.
type Compare struct {
	LeftRunID  string `json:"left_run_id"`
	RightRunID string `json:"right_run_id"`
	LeftLabel  string `json:"left_label"`
	RightLabel string `json:"right_label"`

	Metrics []MetricDelta `json:"metrics"`
	Tasks   []TaskDelta   `json:"tasks"`
	Summary CompareCounts `json:"summary"`
}

// MetricDelta is one metric across two runs.
type MetricDelta struct {
	Name        string  `json:"name"`
	Label       string  `json:"label,omitempty"`
	Unit        string  `json:"unit"`
	Description string  `json:"description,omitempty"`
	Direction   string  `json:"direction,omitempty"`
	Left        float64 `json:"left"`
	Right       float64 `json:"right"`
	LeftText    string  `json:"left_text"`
	RightText   string  `json:"right_text"`
	Delta       float64 `json:"delta"`
	DeltaText   string  `json:"delta_text"`
	Verdict     string  `json:"verdict"` // better | worse | same | unknown
	// OnlyRight / OnlyLeft mark a metric present in one run but not the other,
	// which is what happens when a custom metric was added between runs.
	OnlyLeft  bool `json:"only_left,omitempty"`
	OnlyRight bool `json:"only_right,omitempty"`
}

// TaskDelta is one task across two runs.
type TaskDelta struct {
	Key       string  `json:"key"`
	TaskID    string  `json:"id"`
	Category  string  `json:"category,omitempty"`
	Verdict   string  `json:"verdict"` // fixed | regressed | unchanged | added | removed
	Left      TaskRef `json:"left"`
	Right     TaskRef `json:"right"`
	LeftDiff  string  `json:"left_diff,omitempty"`
	RightDiff string  `json:"right_diff,omitempty"`
}

// TaskRef is the compact per-run view of a task inside a comparison.
type TaskRef struct {
	Present    bool    `json:"present"`
	Passed     bool    `json:"passed"`
	Status     string  `json:"status,omitempty"`
	StopReason string  `json:"stop_reason,omitempty"`
	Score      float64 `json:"score"`
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Turns      int     `json:"turns"`
	Error      string  `json:"error,omitempty"`
}

// CompareCounts summarises a comparison.
type CompareCounts struct {
	Common    int `json:"common"`
	Fixed     int `json:"fixed"`
	Regressed int `json:"regressed"`
	BothPass  int `json:"both_pass"`
	BothFail  int `json:"both_fail"`
	OnlyLeft  int `json:"only_left"`
	OnlyRight int `json:"only_right"`
}

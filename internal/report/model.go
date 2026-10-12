// Package report renders evaluation results as self-contained HTML.
//
// # Why a view model instead of the storage types
//
// The report is a *function of the database*, not of the process that produced
// it. A report generated two weeks later from a copied .db must be the document
// the run produced. That only holds if the renderer reads nothing but persisted
// state, so storage rows are projected onto the types below first and everything
// downstream -- the file, the served page, the comparison -- works on the
// projection.
//
// # Why some types are borrowed rather than mirrored
//
// The run's arithmetic is runner.Summary and a sweep's answer is
// attrib.Attribution, both embedded here rather than copied into report-local
// equivalents. A mirror struct is a second definition of the same thing, and the
// two drift: a field added to the runner's summary would silently never reach
// the page. Borrowing the type means the only way to change the shape is to
// change it in one place.
//
// # Why the data travels as JSON inside the page
//
// A report is opened from a file:// URL or an email attachment as often as from
// a server, and its interactions -- filter, sort, expand a trajectory -- must
// work with no network. So the page ships a shell plus one JSON document and
// lets the browser do the rest.
package report

import (
	"time"

	"github.com/skyitachi/agenteval/internal/attrib"
	"github.com/skyitachi/agenteval/internal/runner"
)

// DataVersion guards the embedded payload against a stale renderer. Bump it when
// the JSON shape changes in a way an older app.js cannot read.
//
// v2 is the minimal model: the reward, guard and oracle fields are gone, and a
// task carries what it cost against its recorded baseline instead of what a
// training loop would have paid for it.
const DataVersion = 2

// Options controls what a report embeds.
//
// Every limit exists because reports have two opposing failure modes: too big to
// open (a 300-task run with full trajectories is tens of megabytes) and too
// lossy to debug. The defaults favour completeness and the builder degrades
// deterministically -- dropping *passing* task detail before failing detail --
// rather than silently producing an unusable file.
type Options struct {
	// Title overrides the document title.
	Title string
	// GeneratedAt stamps the report; zero means time.Now.
	GeneratedAt time.Time

	// MaxToolCallsPerTask bounds the per-task tool-call table.
	MaxToolCallsPerTask int
	// MaxEventsPerTask bounds the per-task trajectory. Both ends are kept: the
	// opening events carry the task framing and the last ones carry the answer,
	// and a viewer that shows only the head is a viewer that never shows what
	// happened.
	MaxEventsPerTask int
	// MaxFieldBytes clips a single text field (diff, answer, tool output).
	// Kept generous: a truncated stack trace is worthless.
	MaxFieldBytes int

	// MaxInlineBytes budgets the whole serialized payload. Once the budget is
	// exhausted, remaining *passing* tasks lose their trajectory and diff;
	// failing tasks keep their evidence longest.
	MaxInlineBytes int

	// NoTaskDetail drops per-task diffs, answers and trajectories entirely,
	// producing a small summary report rather than a truncated one. This is
	// what a reader who only wants the numbers is asking for; it is also the
	// only way to get a bounded document out of a run whose cells are enormous.
	NoTaskDetail bool
}

func (o Options) withDefaults() Options {
	if o.MaxToolCallsPerTask <= 0 {
		o.MaxToolCallsPerTask = 200
	}
	if o.MaxEventsPerTask <= 0 {
		o.MaxEventsPerTask = 400
	}
	if o.MaxFieldBytes <= 0 {
		o.MaxFieldBytes = 20000
	}
	// 8 MiB of JSON inflates to roughly 12 MiB of HTML once a browser has
	// pretty-printed it. That opens instantly on any machine made in the last
	// decade and still fits a 300-task run with every failure detailed.
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

// Data is everything a rendered report needs. Exactly one of Compare and Sweep
// is set on a report that is not about a single run.
type Data struct {
	Version   int      `json:"version"`
	Generated string   `json:"generated"`
	Title     string   `json:"title"`
	Run       RunInfo  `json:"run"`
	Metrics   []Metric `json:"metrics"`
	Tasks     []Task   `json:"tasks"`
	Counts    Counts   `json:"counts"`

	// Breakdowns render as bars: pass rate per category, stop-reason mix, and
	// the split between graded, timed-out and unmeasured cells.
	Breakdowns []Breakdown `json:"breakdowns,omitempty"`
	// Warnings record where the report is less complete than the run was.
	Warnings []string `json:"warnings,omitempty"`

	Compare *Compare            `json:"compare,omitempty"`
	Sweep   *attrib.Attribution `json:"sweep,omitempty"`
}

// RunInfo is the run's provenance and headline metadata.
type RunInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at,omitempty"`
	DurationMs int64  `json:"duration_ms"`

	DatasetName string `json:"dataset_name"`
	DatasetHash string `json:"dataset_hash,omitempty"`
	DatasetPath string `json:"dataset_path,omitempty"`

	AgentKind  string `json:"agent_kind"`
	AgentModel string `json:"agent_model,omitempty"`
	AgentBin   string `json:"agent_bin,omitempty"`
	Backend    string `json:"backend"`

	Samples     int `json:"samples_per_task"`
	Concurrency int `json:"concurrency"`

	// The sweep coordinates, rendered because a run that is one point of a
	// sweep reads very differently from a standalone one.
	FactorAxis  string `json:"factor_axis,omitempty"`
	FactorLevel string `json:"factor_level,omitempty"`
	SweepID     string `json:"sweep_id,omitempty"`

	Notes       string `json:"notes,omitempty"`
	ToolVersion string `json:"tool_version,omitempty"`

	// Summary is the runner's own arithmetic, borrowed so that the CLI's
	// printed totals and the page's totals cannot disagree.
	Summary runner.Summary `json:"summary"`
}

// Counts is the cell tally, kept separate so the header never has to reduce the
// task list in JavaScript.
type Counts struct {
	// Cells counts attempts; Tasks counts distinct task ids. They differ
	// whenever SamplesPerTask > 1 and both are worth showing.
	Cells          int `json:"cells"`
	Tasks          int `json:"tasks"`
	Graded         int `json:"graded"`
	Passed         int `json:"passed"`
	Failed         int `json:"failed"`
	Ungraded       int `json:"ungraded"`
	Timeouts       int `json:"timeouts"`
	HarnessErrors  int `json:"harness_errors"`
	SamplesPerTask int `json:"samples_per_task"`
}

// Metric is one headline number, pre-formatted so the browser needs no unit
// logic of its own.
type Metric struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Display string `json:"display"`
	// Unit is for the tooltip; Display already carries it where it reads well.
	Unit string `json:"unit,omitempty"`
	// Direction says which way is good, and is the reason a metric is not just
	// a number: painting a token increase green is worse than not painting it.
	Direction string `json:"direction,omitempty"` // up | down | neutral
	// Note explains a number that would otherwise read as a defect.
	Note string `json:"note,omitempty"`
}

// Breakdown is a named distribution rendered as bars.
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

// Task is one cell, the unit the detail drawer renders.
type Task struct {
	Key         string   `json:"key"`
	ID          string   `json:"id"`
	SampleIndex int      `json:"sample_index"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	// Graded is false when the task had nothing to score it against. Kept
	// distinct from Passed so that "measured but unscored" is not rendered as a
	// failure.
	Graded    bool    `json:"graded"`
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`

	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheRead    int `json:"cache_read_tokens"`
	CacheCreate  int `json:"cache_create_tokens"`
	TotalTokens  int `json:"total_tokens"`

	Turns      int `json:"turns"`
	ToolCalls  int `json:"tool_calls"`
	ToolErrors int `json:"tool_errors"`

	// Baseline is the comparison the runner resolved at run time, so the page
	// needs no access to the dataset.
	Baseline *BaselineView `json:"baseline,omitempty"`

	Error string `json:"error,omitempty"`

	Diff   string `json:"diff,omitempty"`
	Answer string `json:"answer,omitempty"`

	Graders []Grader   `json:"graders,omitempty"`
	Tools   []ToolCall `json:"tools,omitempty"`
	Events  []Event    `json:"events,omitempty"`

	// Detail records why this task carries less than it did at run time. Empty
	// means nothing was dropped.
	Detail string `json:"detail,omitempty"`
}

// BaselineView is a cell's numbers next to the recorded session's.
type BaselineView struct {
	ID  string `json:"id,omitempty"`
	Ref string `json:"ref,omitempty"`
	// Tokens and Ms are the baseline's own totals; BusyMs rather than wall
	// clock for the time, because the recorded session had a human in the loop.
	Tokens int     `json:"tokens"`
	Ms     int64   `json:"ms"`
	Cost   float64 `json:"cost_usd"`
	Turns  int     `json:"turns"`
	// Deltas are this attempt minus the baseline. Negative means cheaper.
	//
	// HasTokens and HasTime are false when the recording carried nothing to
	// subtract, in which case the delta is meaningless and has to be rendered
	// as a dash. A recording that never captured busy time is not a recording
	// that took no time.
	TokenDelta int   `json:"token_delta"`
	MsDelta    int64 `json:"ms_delta"`
	HasTokens  bool  `json:"has_tokens"`
	HasTime    bool  `json:"has_time"`

	// Output is the recorded session's final answer, rendered beside this
	// attempt's so the reader can compare conclusions and not only spend.
	// Empty means the recording kept none -- which is every row written before
	// schema v2, and any recording whose session ended without assistant text.
	Output string `json:"output,omitempty"`
}

// Grader is one grader verdict.
type Grader struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Passed   bool           `json:"passed"`
	Score    float64        `json:"score"`
	Weight   float64        `json:"weight"`
	Required bool           `json:"required"`
	Reason   string         `json:"reason,omitempty"`
	Error    string         `json:"error,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
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

// Event is one step of the normalized trajectory. Kind matches the trace
// package's vocabulary, which is what the client switches on.
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

// Compare is a side-by-side of two runs.
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
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Unit      string  `json:"unit,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Left      float64 `json:"left"`
	Right     float64 `json:"right"`
	LeftText  string  `json:"left_text"`
	RightText string  `json:"right_text"`
	Delta     float64 `json:"delta"`
	DeltaText string  `json:"delta_text"`
	// Verdict is better | worse | same | unknown. Unknown is the honest answer
	// for a metric whose direction is not known, which is every custom one.
	Verdict string `json:"verdict"`
}

// TaskDelta is one task across two runs.
type TaskDelta struct {
	Key      string `json:"key"`
	ID       string `json:"id"`
	Category string `json:"category,omitempty"`
	// Verdict is fixed | regressed | unchanged | added | removed | ungraded.
	Verdict string  `json:"verdict"`
	Left    TaskRef `json:"left"`
	Right   TaskRef `json:"right"`
}

// TaskRef is the compact per-run view of a task inside a comparison.
type TaskRef struct {
	Present    bool    `json:"present"`
	Graded     bool    `json:"graded"`
	Passed     bool    `json:"passed"`
	Status     string  `json:"status,omitempty"`
	StopReason string  `json:"stop_reason,omitempty"`
	Score      float64 `json:"score"`
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Tokens     int     `json:"total_tokens"`
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
	Ungraded  int `json:"ungraded"`
	OnlyLeft  int `json:"only_left"`
	OnlyRight int `json:"only_right"`
}

// ---------------------------------------------------------------------------
// Run index
// ---------------------------------------------------------------------------

// Index is the run listing.
type Index struct {
	Version   int          `json:"version"`
	Generated string       `json:"generated"`
	Title     string       `json:"title"`
	Runs      []IndexEntry `json:"runs"`
	// Live is true when the listing was produced while a run was still going,
	// which is what makes the page reload itself instead of going stale.
	Live bool `json:"live,omitempty"`
}

// IndexEntry is one row of the listing.
type IndexEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	DurationMs int64  `json:"duration_ms"`

	DatasetName string `json:"dataset_name"`
	AgentKind   string `json:"agent_kind,omitempty"`
	AgentModel  string `json:"agent_model,omitempty"`

	FactorAxis  string `json:"factor_axis,omitempty"`
	FactorLevel string `json:"factor_level,omitempty"`
	SweepID     string `json:"sweep_id,omitempty"`

	Counts   Counts   `json:"counts"`
	Tokens   int      `json:"tokens"`
	CostUSD  float64  `json:"cost_usd"`
	PassRate *float64 `json:"pass_rate,omitempty"`

	Error string `json:"error,omitempty"`
}

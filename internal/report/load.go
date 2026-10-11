package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/metric"
	"github.com/skyitachi/agenteval/internal/storage"
	"github.com/skyitachi/agenteval/internal/trace"
)

// Load builds a report payload for one run.
//
// Everything it reads comes from the store, so the result depends only on the
// database contents and the Options -- never on whether the process that
// produced the run is still alive.
func Load(ctx context.Context, store *storage.Store, runID string, opts Options) (*Data, error) {
	if store == nil {
		return nil, fmt.Errorf("report: store is nil")
	}
	opts = opts.withDefaults()

	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	rows, err := store.TaskRuns(ctx, storage.TaskRunsQuery{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("load task runs: %w", err)
	}
	graders, err := store.GraderResults(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load grader results: %w", err)
	}
	metrics, err := store.Metrics(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load metrics: %w", err)
	}

	data, err := build(*run, rows, graders, metrics, opts)
	if err != nil {
		return nil, err
	}
	data.Title = titleFor(opts, *run)
	return data, nil
}

// LoadComparison builds a two-run payload. Both runs are loaded with the same
// options so the comparison cannot be skewed by one side being more detailed.
func LoadComparison(ctx context.Context, store *storage.Store, leftID, rightID string, opts Options) (*Data, error) {
	left, err := Load(ctx, store, leftID, opts)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", leftID, err)
	}
	right, err := Load(ctx, store, rightID, opts)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", rightID, err)
	}
	if left.Run.DatasetHash != "" && right.Run.DatasetHash != "" &&
		left.Run.DatasetHash != right.Run.DatasetHash {
		// A cross-dataset comparison is legal but almost always a mistake, and
		// silently reporting "task X regressed" across two different datasets is
		// exactly the kind of wrong number a benchmark tool must not produce.
		left.Warnings = append(left.Warnings, fmt.Sprintf(
			"the two runs used different dataset revisions (%s vs %s); task-level verdicts are only meaningful when the dataset is identical",
			shortHash(left.Run.DatasetHash), shortHash(right.Run.DatasetHash)))
	}

	left.Compare = compare(left, right)
	left.Title = fmt.Sprintf("Compare %s vs %s", leftTitle(left), leftTitle(right))
	return left, nil
}

// ---------------------------------------------------------------------------
// Index (run listing)
// ---------------------------------------------------------------------------

// Index is the run list a server or an index page shows.
type Index struct {
	Version   int                  `json:"version"`
	Generated string               `json:"generated"`
	Title     string               `json:"title"`
	Runs      []storage.RunSummary `json:"runs"`
	Stats     map[string]int       `json:"stats,omitempty"`

	// LiveRefreshSeconds asks the page to reload itself, which is how a run in
	// progress can be watched. Zero disables it.
	LiveRefreshSeconds int `json:"live_refresh_seconds,omitempty"`
	// Capabilities tells the client which actions the host actually supports,
	// so the UI never offers something that would be refused. A standalone file
	// advertises none and a read-only server advertises none.
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

// TitleOf returns a payload's effective title, falling back to the run name and
// then its id. It exists so a caller can build navigation labels without
// re-deriving the same fallback chain.
func TitleOf(d *Data) string {
	if d == nil {
		return "Evaluation report"
	}
	if d.Title != "" {
		return d.Title
	}
	if d.Run.Name != "" {
		return d.Run.Name
	}
	return d.Run.ID
}

// LoadIndex lists recent runs.
func LoadIndex(ctx context.Context, store *storage.Store, limit int, opts Options) (*Index, error) {
	if store == nil {
		return nil, fmt.Errorf("report: store is nil")
	}
	opts = opts.withDefaults()
	runs, err := store.ListRuns(ctx, limit)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		Version:   DataVersion,
		Generated: opts.GeneratedAt.Format("2006-01-02 15:04:05 MST"),
		Title:     titleFor(opts, storage.RunRecord{}),
		Runs:      runs,
	}
	if stats, err := store.Stats(ctx); err == nil {
		idx.Stats = stats
	}
	return idx, nil
}

// ---------------------------------------------------------------------------
// Building
// ---------------------------------------------------------------------------

func build(run storage.RunRecord, rows []storage.TaskRunRecord,
	graders []storage.GraderResultRecord, metrics []storage.MetricRecord,
	opts Options) (*Data, error) {

	data := &Data{
		Version:   DataVersion,
		Generated: opts.GeneratedAt.Format("2006-01-02 15:04:05 MST"),
		Run:       runInfo(run, opts),
	}

	byTask := map[string][]storage.GraderResultRecord{}
	for _, g := range graders {
		byTask[g.TaskRunID] = append(byTask[g.TaskRunID], g)
	}

	data.Tasks = make([]Task, 0, len(rows))
	var warnings []string
	for _, row := range rows {
		t, warn := taskFromRow(row, byTask[row.ID], opts)
		warnings = append(warnings, warn...)
		data.Tasks = append(data.Tasks, t)
	}

	data.Metrics = metricsFrom(metrics)
	data.Breakdowns = breakdowns(data.Tasks)
	data.Counts = counts(data.Tasks, run.SamplesPerTask)

	data = applyBudget(data, opts)
	data.Warnings = append(data.Warnings, warnings...)
	return data, nil
}

func runInfo(run storage.RunRecord, opts Options) RunInfo {
	info := RunInfo{
		ID:             run.ID,
		Name:           run.Name,
		Status:         run.Status,
		Error:          run.Error,
		CreatedAt:      formatTime(run.CreatedAt),
		DurationMs:     run.DurationMs,
		DatasetName:    run.DatasetName,
		DatasetVersion: run.DatasetVersion,
		DatasetHash:    run.DatasetHash,
		DatasetPath:    run.DatasetPath,
		AgentKind:      run.AgentKind,
		AgentModel:     run.AgentModel,
		Mode:           run.Mode,
		SandboxBackend: run.SandboxBackend,
		SandboxImage:   run.SandboxImage,
		SamplesPerTask: run.SamplesPerTask,
		Concurrency:    run.Concurrency,
		Seed:           run.Seed,
		ConfigHash:     run.ConfigHash,
		Notes:          run.Notes,
		ToolVersion:    run.ToolVersion,
		HostArch:       run.HostArch,
	}
	if run.FinishedAt != nil {
		info.FinishedAt = formatTime(*run.FinishedAt)
	}
	if opts.IncludeRawConfig {
		info.ConfigJSON = run.ConfigJSON
	}
	return info
}

// taskFromRow projects one stored attempt, returning warnings for anything that
// could not be read rather than failing the whole report. A report with one
// unreadable trajectory is still worth having.
func taskFromRow(row storage.TaskRunRecord, graders []storage.GraderResultRecord, opts Options) (Task, []string) {
	t := Task{
		Key:           storage.TaskKey(row.TaskID, row.SampleIndex),
		ID:            row.TaskID,
		SampleIndex:   row.SampleIndex,
		Category:      row.Category,
		Difficulty:    row.Difficulty,
		Tags:          row.Tags,
		Status:        row.Status,
		StopReason:    row.StopReason,
		Passed:        row.Passed,
		Score:         row.Score,
		Threshold:     row.Threshold,
		Reward:        row.Reward,
		RewardKind:    row.RewardKind,
		Rejected:      len(row.Violations) > 0,
		Violations:    row.Violations,
		Restored:      row.Restored,
		DurationMs:    row.DurationMs,
		CostUSD:       row.CostUSD,
		CostReliable:  row.CostReliable,
		InputTokens:   row.InputTokens,
		OutputTokens:  row.OutputTokens,
		CacheRead:     row.CacheReadTokens,
		CacheCreate:   row.CacheCreateTokens,
		TotalTokens:   row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheCreateTokens,
		Turns:         row.Turns,
		ToolCalls:     row.ToolCalls,
		ToolErrors:    row.ToolErrors,
		TTFTMs:        row.TTFTMs,
		APIDuration:   row.APIDurationMs,
		SessionID:     row.SessionID,
		Model:         row.Model,
		Error:         row.Error,
		ArtifactDir:   artifactDir(row, opts),
		WorkspaceDir:  row.WorkspaceDir,
		RawStreamPath: row.RawStreamPath,
	}
	// Clipping records its own degradation on the task, so it has to happen
	// after the struct exists.
	t.Answer = clip(row.Answer, opts.MaxFieldBytes, &t)
	t.Diff = clip(row.Diff, opts.MaxFieldBytes, &t)
	if row.StartedAt != nil {
		t.StartedAt = formatTime(*row.StartedAt)
	}
	if row.EndedAt != nil {
		t.EndedAt = formatTime(*row.EndedAt)
	}

	sort.SliceStable(graders, func(i, j int) bool { return graders[i].Seq < graders[j].Seq })
	for _, g := range graders {
		t.Graders = append(t.Graders, Grader{
			Grader:     g.Grader,
			Type:       g.Type,
			Passed:     g.Passed,
			Score:      g.Score,
			Weight:     g.Weight,
			Required:   g.Required,
			Reason:     g.Reason,
			Error:      g.Error,
			DurationMs: g.DurationMs,
			Details:    decodeMap(g.DetailsJSON),
		})
	}

	var warnings []string
	if row.AttemptJSON != "" {
		var att agent.Attempt
		if err := json.Unmarshal([]byte(row.AttemptJSON), &att); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: stored attempt is unreadable (%v)", t.Key, err))
		} else {
			exit := att.ExitCode
			t.ExitCode = &exit
			t.TimedOut = att.TimedOut
			t.TurnsExceeded = att.TurnsExceeded
			t.AgentFailed = att.Failed
			t.CommandLine = att.CommandLine
			t.Stderr = clip(att.Stderr, opts.MaxFieldBytes, &t)
		}
	}
	if row.TraceJSON != "" {
		var tr trace.Trace
		if err := json.Unmarshal([]byte(row.TraceJSON), &tr); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: stored trace is unreadable (%v)", t.Key, err))
		} else {
			t.Incomplete = tr.Incomplete
			t.ParseErrors = tr.ParseErrors
			t.Events = eventsOf(tr, opts, &t)
			t.Tools = toolsOf(tr, opts, &t)
		}
	}
	t.Definition = definitionOf(t.ArtifactDir)
	return t, warnings
}

// eventsOf converts the normalized trajectory, bounding the count.
//
// When the trajectory is longer than the budget the head and the tail are kept
// rather than a prefix: the opening events carry the task framing and the last
// events carry the answer, and a truncated tail is the single most misleading
// thing a trajectory view can show. The gap is represented by an explicit
// placeholder event so the reader sees that something is missing.
func eventsOf(tr trace.Trace, opts Options, t *Task) []Event {
	evs := tr.Events
	if len(evs) == 0 {
		return nil
	}
	out := make([]Event, 0, min(len(evs), opts.MaxEventsPerTask))
	emit := func(e trace.Event) {
		out = append(out, Event{
			Index:     e.Index,
			Kind:      string(e.Kind),
			Timestamp: formatOptTime(e.Timestamp),
			Text:      clip(e.Text, opts.MaxFieldBytes, t),
			ToolUseID: e.ToolUseID,
			ToolName:  e.ToolName,
			ToolInput: clip(string(e.ToolInput), opts.MaxFieldBytes, t),
			ToolOut:   clip(e.ToolOutput, opts.MaxFieldBytes, t),
			ToolError: e.ToolError,
		})
	}

	if len(evs) <= opts.MaxEventsPerTask {
		for _, e := range evs {
			emit(e)
		}
		return out
	}
	head := opts.MaxEventsPerTask / 2
	tail := opts.MaxEventsPerTask - head
	for _, e := range evs[:head] {
		emit(e)
	}
	skipped := len(evs) - head - tail
	out = append(out, Event{
		Index: evs[head].Index,
		Kind:  "truncated",
		Text: fmt.Sprintf("%d event(s) omitted (%d of %d shown)",
			skipped, opts.MaxEventsPerTask, len(evs)),
	})
	for _, e := range evs[len(evs)-tail:] {
		emit(e)
	}
	markDetail(t, fmt.Sprintf("trajectory truncated to %d of %d events", opts.MaxEventsPerTask, len(evs)))
	return out
}

func toolsOf(tr trace.Trace, opts Options, t *Task) []ToolCall {
	if len(tr.ToolCalls) == 0 {
		return nil
	}
	calls := tr.ToolCalls
	n := min(len(calls), opts.MaxToolCallsPerTask)
	out := make([]ToolCall, 0, n)
	for _, c := range calls[:n] {
		out = append(out, ToolCall{
			Seq:        c.Seq,
			ID:         c.ID,
			Name:       c.Name,
			IsError:    c.IsError,
			DurationMs: c.DurationMs,
			Input:      clip(string(c.Input), opts.MaxFieldBytes, t),
			Output:     clip(c.Output, opts.MaxFieldBytes, t),
		})
	}
	if len(calls) > n {
		markDetail(t, fmt.Sprintf("tool calls truncated to %d of %d", n, len(calls)))
	}
	return out
}

// definitionOf recovers the task contract from the artifact bundle.
//
// The prompt is deliberately not stored in SQLite, so this is the only way the
// report can answer "what was actually asked". A missing bundle is normal --
// artifacts live under the run's work root and do not travel with the database
// -- so it degrades to a nil definition rather than a warning.
func definitionOf(dir string) *Definition {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, "task.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var spec struct {
		dataset.TaskSpec
		Baseline string `json:"_baseline"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil
	}
	prompt := spec.ResolvedPrompt
	if prompt == "" {
		prompt = spec.Prompt
	}
	def := &Definition{
		Prompt:       prompt,
		SystemPrompt: spec.SystemPrompt,
		Version:      spec.Version,
		Workspace:    describeWorkspace(spec.Workspace),
		Output:       describeOutput(spec.Output),
		Limits:       describeLimits(spec.Limits),
		Metadata:     spec.Metadata,
		Baseline:     spec.Baseline,
	}
	for _, g := range spec.Graders {
		name := g.Name
		if name == "" {
			name = g.Type
		}
		def.Graders = append(def.Graders, fmt.Sprintf("%s (%s, weight %g%s)",
			name, g.Type, g.EffectiveWeight(), requiredSuffix(g.Required)))
	}
	return def
}

// artifactDir locates the bundle: an explicit mapping wins, otherwise it is the
// directory holding the raw stream, which the engine writes next to it.
func artifactDir(row storage.TaskRunRecord, opts Options) string {
	if opts.Artifacts != nil {
		if dir, ok := opts.Artifacts[storage.TaskKey(row.TaskID, row.SampleIndex)]; ok && dir != "" {
			return dir
		}
	}
	if row.RawStreamPath != "" {
		return filepath.Dir(row.RawStreamPath)
	}
	return ""
}

// markDetail records a degradation reason once.
func markDetail(t *Task, reason string) {
	if strings.Contains(t.Detail, reason) {
		return
	}
	if t.Detail == "" {
		t.Detail = reason
		return
	}
	t.Detail += "; " + reason
}

// clip bounds a text field and records the truncation on the task.
func clip(s string, limit int, t *Task) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	markDetail(t, "long text fields clipped")
	// Cut on a rune boundary so the JSON stays valid UTF-8.
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n... [truncated, %d bytes total]", len(s))
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

// applyBudget enforces MaxInlineBytes by dropping per-task *detail* -- never the
// table row itself -- and always before dropping the detail of a failure.
//
// The order is deliberate: a report is opened to find out what went wrong, so a
// passing task's five-hundred-event trajectory is the cheapest thing to lose. If
// the budget cannot even hold the summary rows the report says so explicitly
// instead of quietly shipping a file that will not open.
func applyBudget(data *Data, opts Options) *Data {
	// Detach every task's detail so the summary size can be measured on its own.
	type detail struct {
		idx  int
		ev   []Event
		tl   []ToolCall
		diff string
		ans  string
		def  *Definition
	}
	base := 0
	var pending []detail
	for i := range data.Tasks {
		t := &data.Tasks[i]
		d := detail{idx: i, ev: t.Events, tl: t.Tools, diff: t.Diff, ans: t.Answer, def: t.Definition}
		t.Events, t.Tools, t.Diff, t.Answer, t.Definition = nil, nil, "", "", nil
		base += jsonLen(t)
		if d.ev != nil || d.tl != nil || d.diff != "" || d.ans != "" || d.def != nil {
			pending = append(pending, d)
		}
	}

	remaining := opts.MaxInlineBytes - base
	if remaining < 0 {
		data.Warnings = append(data.Warnings, fmt.Sprintf(
			"the run has %d cells; even without per-task detail the report is %s, which exceeds the %s budget",
			len(data.Tasks), humanBytes(base), humanBytes(opts.MaxInlineBytes)))
		return data
	}

	// Failing and errored cells first, then the rest in plan order.
	sort.SliceStable(pending, func(a, b int) bool {
		pa := detailPriority(&data.Tasks[pending[a].idx])
		pb := detailPriority(&data.Tasks[pending[b].idx])
		if pa != pb {
			return pa > pb
		}
		return pending[a].idx < pending[b].idx
	})

	restored := 0
	for _, d := range pending {
		// Cost the detail, not the whole task: the row itself is already
		// accounted for in base. Measuring the parts in isolation slightly
		// over-estimates (the fields' own quoting is not counted), which can
		// only make the budget more conservative -- and it avoids re-marshalling
		// every task twice.
		cost := jsonLen(d.ev) + jsonLen(d.tl) + len(d.diff) + len(d.ans) + jsonLen(d.def)
		if cost > remaining {
			markDetail(&data.Tasks[d.idx], "detail omitted to keep the report within its size budget")
			continue
		}
		t := &data.Tasks[d.idx]
		t.Events, t.Tools, t.Diff, t.Answer, t.Definition = d.ev, d.tl, d.diff, d.ans, d.def
		remaining -= cost
		restored++
	}
	if restored < len(pending) {
		data.Warnings = append(data.Warnings, fmt.Sprintf(
			"per-task trajectories and diffs were dropped for %d of %d cells to stay within the %s report budget; failing cells were kept first",
			len(pending)-restored, len(pending), humanBytes(opts.MaxInlineBytes)))
	}
	return data
}

// detailPriority ranks which cells most deserve their detail.
func detailPriority(t *Task) int {
	switch {
	case t.Status == storage.TaskStatusFailed || t.Error != "":
		return 3
	case !t.Passed:
		return 2
	case t.Skipped():
		return 0
	default:
		return 1
	}
}

// Skipped reports whether the cell never ran.
func (t *Task) Skipped() bool { return t.Status == storage.TaskStatusSkipped }

func jsonLen(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// metricGroups orders the metric cards. An unlisted metric lands in "other",
// which is what a custom metric gets.
var metricGroups = []struct {
	title string
	names []string
}{
	{"outcome", []string{"resolve_rate", "mean_score", "pass_at_k", "pass_hat_k", "resolved_count", "task_count", "sample_count"}},
	{"cost", []string{"total_cost_usd", "cost_per_resolved_usd", "mean_cost_usd"}},
	{"token", []string{"mean_total_tokens", "tokens_per_resolved", "mean_input_tokens", "mean_output_tokens", "mean_cache_read_tokens"}},
	{"effort", []string{"mean_turns", "p95_turns", "mean_tool_calls", "tool_call_error_rate"}},
	{"latency", []string{"mean_latency_ms", "p50_latency_ms", "p95_latency_ms", "mean_ttft_ms", "p95_ttft_ms", "total_latency_ms"}},
	{"health", []string{"timeout_rate", "max_turns_rate", "harness_error_rate", "incomplete_rate", "grader_error_rate"}},
}

// lowerIsBetter lists metrics where a decrease is an improvement. Everything
// not listed is treated as "higher is better" except for the pure size
// descriptors below, which are neutral: a run with more tasks is not a better
// run.
var lowerIsBetter = []string{
	"cost", "latency", "ttft", "tokens", "turns", "error_rate", "tool_errors",
	"timeout", "incomplete",
}

var neutralMetrics = map[string]bool{
	"task_count": true, "sample_count": true, "total_latency_ms": true,
}

func metricGroup(name string) string {
	for _, g := range metricGroups {
		for _, n := range g.names {
			if n == name {
				return g.title
			}
		}
	}
	return "other"
}

func metricDirection(name string) string {
	if neutralMetrics[name] {
		return "neutral"
	}
	if strings.HasPrefix(name, "mean_") && strings.Contains(name, "tokens") {
		return "down"
	}
	for _, s := range lowerIsBetter {
		if strings.Contains(name, s) {
			return "down"
		}
	}
	if strings.HasSuffix(name, "_count") {
		return "neutral"
	}
	return "up"
}

func metricsFrom(rows []storage.MetricRecord) []Metric {
	out := make([]Metric, 0, len(rows))
	for _, r := range rows {
		unit := metric.Unit(r.Unit)
		if unit == "" {
			unit = metric.UnitText
		}
		v := metric.Value{Name: r.Name, Value: r.Value, Unit: unit, Kind: metric.Kind(r.Kind)}
		extra := decodeMap(r.ExtraJSON)
		// A custom metric carries its own rendering hints, and they win over the
		// name heuristics below. The heuristics exist because a built-in
		// metric's name was chosen by whoever wrote it and can be curated in
		// one place; a user's "slow_cells" has no such guarantee, so guessing
		// that more is better would colour a regression green.
		group := orString(stringFrom(extra, "group"), metricGroup(r.Name))
		direction := orString(stringFrom(extra, "direction"), metricDirection(r.Name))
		m := Metric{
			Name:        r.Name,
			Label:       r.Label,
			Value:       r.Value,
			Display:     v.Display(),
			Unit:        r.Unit,
			Kind:        r.Kind,
			Description: describeMetric(r.Name),
			Group:       group,
			Direction:   direction,
			Extra:       extra,
		}
		out = append(out, m)
	}
	// Storage returns them alphabetically; card order should follow the
	// semantic grouping instead so the headline numbers come first.
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := groupRank(out[i].Group), groupRank(out[j].Group)
		if gi != gj {
			return gi < gj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func groupRank(group string) int {
	for i, g := range metricGroups {
		if g.title == group {
			return i
		}
	}
	return len(metricGroups)
}

// describeMetric pulls the documentation from the registry. A custom metric is
// not registered, so it simply has no description.
func describeMetric(name string) string {
	if m, err := metric.Get(name); err == nil {
		return m.Description()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Breakdowns
// ---------------------------------------------------------------------------

func breakdowns(tasks []Task) []Breakdown {
	var out []Breakdown
	if b := rateBreakdown("Resolve rate by category", tasks, func(t *Task) string { return t.Category }); b != nil {
		out = append(out, *b)
	}
	if b := rateBreakdown("Resolve rate by difficulty", tasks, func(t *Task) string { return t.Difficulty }); b != nil {
		out = append(out, *b)
	}
	if b := stopReasonBreakdown(tasks); b != nil {
		out = append(out, *b)
	}
	if b := toolUsageBreakdown(tasks); b != nil {
		out = append(out, *b)
	}
	if b := scoreHistogram(tasks); b != nil {
		out = append(out, *b)
	}
	if b := latencyHistogram(tasks); b != nil {
		out = append(out, *b)
	}
	return out
}

func rateBreakdown(title string, tasks []Task, key func(*Task) string) *Breakdown {
	type tally struct{ passed, total int }
	groups := map[string]*tally{}
	for i := range tasks {
		k := key(&tasks[i])
		if k == "" {
			k = "(unset)"
		}
		g, ok := groups[k]
		if !ok {
			g = &tally{}
			groups[k] = g
		}
		g.total++
		if tasks[i].Passed {
			g.passed++
		}
	}
	if len(groups) == 0 {
		return nil
	}
	b := &Breakdown{Title: title}
	for _, k := range sortedGroupKeys(groups) {
		g := groups[k]
		b.Rows = append(b.Rows, BreakdownRow{
			Label: k,
			Value: safeDiv(float64(g.passed), float64(g.total)),
			Text:  fmt.Sprintf("%d/%d", g.passed, g.total),
			Total: g.total,
			Hits:  g.passed,
		})
	}
	return b
}

func stopReasonBreakdown(tasks []Task) *Breakdown {
	counts := map[string]int{}
	for i := range tasks {
		r := tasks[i].StopReason
		if r == "" {
			r = "(none)"
		}
		counts[r]++
	}
	if len(counts) == 0 {
		return nil
	}
	b := &Breakdown{Title: "Stop reason mix", Note: "how each cell ended; a large timeout or harness_error share invalidates the headline rate"}
	for _, k := range sortedGroupKeys(counts) {
		n := counts[k]
		b.Rows = append(b.Rows, BreakdownRow{
			Label: k,
			Value: safeDiv(float64(n), float64(len(tasks))),
			Text:  fmt.Sprintf("%d (%.1f%%)", n, safeDiv(float64(n), float64(len(tasks)))*100),
			Total: len(tasks),
			Hits:  n,
		})
	}
	return b
}

func toolUsageBreakdown(tasks []Task) *Breakdown {
	counts := map[string]int{}
	var total int
	for i := range tasks {
		for _, c := range tasks[i].Tools {
			counts[c.Name]++
			total++
		}
	}
	if total == 0 {
		return nil
	}
	type kv struct {
		name string
		n    int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].name < all[j].name
	})
	const top = 12
	b := &Breakdown{Title: "Tool usage", Note: "invocations across the cells whose trajectories are included"}
	for i, e := range all {
		if i >= top {
			b.Note = fmt.Sprintf("%s; showing top %d of %d tools", b.Note, top, len(all))
			break
		}
		b.Rows = append(b.Rows, BreakdownRow{
			Label: e.name,
			Value: float64(e.n),
			Text:  fmt.Sprintf("%d (%.1f%%)", e.n, float64(e.n)/float64(total)*100),
			Total: total,
			Hits:  e.n,
		})
	}
	return b
}

func scoreHistogram(tasks []Task) *Breakdown {
	const buckets = 10
	hist := make([]int, buckets)
	var n int
	for i := range tasks {
		s := tasks[i].Score
		if s < 0 {
			s = 0
		}
		if s > 1 {
			s = 1
		}
		idx := int(s * buckets)
		if idx >= buckets {
			idx = buckets - 1
		}
		hist[idx]++
		n++
	}
	if n == 0 {
		return nil
	}
	b := &Breakdown{Title: "Score distribution", Note: "weighted grader score per cell; the bar at 1.0 is the pass rate, the middle bars are partial credit"}
	for i := 0; i < buckets; i++ {
		lo, hi := float64(i)/buckets, float64(i+1)/buckets
		b.Rows = append(b.Rows, BreakdownRow{
			Label: fmt.Sprintf("%.1f-%.1f", lo, hi),
			Value: float64(hist[i]),
			Text:  fmt.Sprintf("%d", hist[i]),
			Total: n,
			Hits:  hist[i],
		})
	}
	return b
}

func latencyHistogram(tasks []Task) *Breakdown {
	var vals []float64
	for i := range tasks {
		if tasks[i].DurationMs > 0 {
			vals = append(vals, float64(tasks[i].DurationMs))
		}
	}
	if len(vals) < 2 {
		return nil
	}
	sort.Float64s(vals)
	lo, hi := vals[0], vals[len(vals)-1]
	if hi <= lo {
		hi = lo + 1
	}
	const buckets = 10
	hist := make([]int, buckets)
	for _, v := range vals {
		idx := int((v - lo) / (hi - lo) * buckets)
		if idx >= buckets {
			idx = buckets - 1
		}
		hist[idx]++
	}
	b := &Breakdown{Title: "Cell duration distribution", Note: "wall clock per cell, including sandbox setup and grading"}
	for i := 0; i < buckets; i++ {
		from := lo + (hi-lo)*float64(i)/buckets
		to := lo + (hi-lo)*float64(i+1)/buckets
		b.Rows = append(b.Rows, BreakdownRow{
			Label: fmt.Sprintf("%s-%s", humanDuration(from), humanDuration(to)),
			Value: float64(hist[i]),
			Text:  fmt.Sprintf("%d", hist[i]),
			Total: len(vals),
			Hits:  hist[i],
		})
	}
	return b
}

func counts(tasks []Task, k int) Counts {
	c := Counts{Samples: len(tasks), SamplesPerTask: k}
	seen := map[string]bool{}
	seenPassed := map[string]bool{}
	for i := range tasks {
		t := &tasks[i]
		seen[t.ID] = true
		switch {
		case t.Skipped():
			c.Skipped++
		case t.Passed:
			c.Passed++
			seenPassed[t.ID] = true
		default:
			c.Failed++
			if t.Error != "" {
				c.Errored++
			}
		}
	}
	c.Tasks = len(seen)
	c.TasksPassed = len(seenPassed)
	return c
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

func compare(left, right *Data) *Compare {
	cmp := &Compare{
		LeftRunID:  left.Run.ID,
		RightRunID: right.Run.ID,
		LeftLabel:  leftTitle(left),
		RightLabel: leftTitle(right),
	}
	cmp.Metrics = compareMetrics(left, right)

	leftIdx := map[string]*Task{}
	for i := range left.Tasks {
		leftIdx[left.Tasks[i].Key] = &left.Tasks[i]
	}
	rightIdx := map[string]*Task{}
	for i := range right.Tasks {
		rightIdx[right.Tasks[i].Key] = &right.Tasks[i]
	}

	// Iterate the union in a stable order so two comparisons of the same pair
	// produce the same page.
	keys := make([]string, 0, len(leftIdx)+len(rightIdx))
	seen := map[string]bool{}
	for i := range left.Tasks {
		k := left.Tasks[i].Key
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for i := range right.Tasks {
		k := right.Tasks[i].Key
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	for _, k := range keys {
		l, r := leftIdx[k], rightIdx[k]
		d := TaskDelta{Key: k}
		id := k
		if l != nil {
			id = l.ID
			d.TaskID = l.ID
			d.Category = l.Category
			d.Left = taskRef(l)
			d.LeftDiff = l.Diff
		}
		if r != nil {
			id = r.ID
			d.Right = taskRef(r)
			d.RightDiff = r.Diff
		}
		if d.TaskID == "" {
			d.TaskID = id
		}
		switch {
		case l == nil:
			d.Verdict = "added"
			cmp.Summary.OnlyRight++
		case r == nil:
			d.Verdict = "removed"
			cmp.Summary.OnlyLeft++
		case !l.Passed && r.Passed:
			d.Verdict = "fixed"
		case l.Passed && !r.Passed:
			d.Verdict = "regressed"
		case l.Passed && r.Passed:
			d.Verdict = "both_pass"
			cmp.Summary.BothPass++
		default:
			d.Verdict = "both_fail"
			cmp.Summary.BothFail++
		}
		switch d.Verdict {
		case "fixed":
			cmp.Summary.Fixed++
		case "regressed":
			cmp.Summary.Regressed++
		}
		if l != nil && r != nil {
			cmp.Summary.Common++
		}
		cmp.Tasks = append(cmp.Tasks, d)
	}
	return cmp
}

func taskRef(t *Task) TaskRef {
	return TaskRef{
		Present:    true,
		Passed:     t.Passed,
		Status:     t.Status,
		StopReason: t.StopReason,
		Score:      t.Score,
		DurationMs: t.DurationMs,
		CostUSD:    t.CostUSD,
		Turns:      t.Turns,
		Error:      t.Error,
	}
}

func compareMetrics(left, right *Data) []MetricDelta {
	lm, rm := map[string]Metric{}, map[string]Metric{}
	for _, m := range left.Metrics {
		lm[m.Name] = m
	}
	for _, m := range right.Metrics {
		rm[m.Name] = m
	}
	names := make([]string, 0, len(lm)+len(rm))
	seen := map[string]bool{}
	for _, m := range left.Metrics {
		if !seen[m.Name] {
			seen[m.Name] = true
			names = append(names, m.Name)
		}
	}
	for _, m := range right.Metrics {
		if !seen[m.Name] {
			seen[m.Name] = true
			names = append(names, m.Name)
		}
	}
	sort.Strings(names)

	out := make([]MetricDelta, 0, len(names))
	for _, name := range names {
		l, hasL := lm[name]
		r, hasR := rm[name]
		base := l
		if !hasL {
			base = r
		}
		d := MetricDelta{
			Name:        name,
			Label:       base.Label,
			Unit:        base.Unit,
			Description: base.Description,
			Direction:   base.Direction,
			OnlyLeft:    hasL && !hasR,
			OnlyRight:   hasR && !hasL,
		}
		if hasL {
			d.Left, d.LeftText = l.Value, l.Display
		} else {
			d.LeftText = "-"
		}
		if hasR {
			d.Right, d.RightText = r.Value, r.Display
		} else {
			d.RightText = "-"
		}
		if !hasL || !hasR {
			d.Verdict = "unknown"
			out = append(out, d)
			continue
		}
		d.Delta = r.Value - l.Value
		d.DeltaText = signedDelta(d.Delta, base.Unit)
		d.Verdict = verdict(d.Delta, base.Direction)
		out = append(out, d)
	}
	return out
}

func verdict(delta float64, direction string) string {
	// Guard against float noise: a 1e-15 change is not an improvement.
	const epsilon = 1e-9
	if delta > -epsilon && delta < epsilon {
		return "same"
	}
	switch direction {
	case "down":
		if delta < 0 {
			return "better"
		}
		return "worse"
	case "neutral":
		return "same"
	default:
		if delta > 0 {
			return "better"
		}
		return "worse"
	}
}

func signedDelta(d float64, unit string) string {
	sign := "+"
	if d < 0 {
		sign = ""
	}
	switch metric.Unit(unit) {
	case metric.UnitUSD:
		return fmt.Sprintf("%s$%.4f", sign, d)
	case metric.UnitMs:
		return fmt.Sprintf("%s%.0fms", sign, d)
	case metric.UnitRatio:
		return fmt.Sprintf("%s%.4f", sign, d)
	case metric.UnitTokens, metric.UnitCount:
		return fmt.Sprintf("%s%.0f", sign, d)
	default:
		return fmt.Sprintf("%s%.4g", sign, d)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func titleFor(opts Options, run storage.RunRecord) string {
	if opts.Title != "" {
		return opts.Title
	}
	if run.Name != "" {
		return fmt.Sprintf("Evaluation report: %s", run.Name)
	}
	if run.ID != "" {
		return fmt.Sprintf("Evaluation report: %s", run.ID)
	}
	return "Evaluation report"
}

func leftTitle(d *Data) string {
	if d.Run.Name != "" {
		return d.Run.Name
	}
	return shortHash(d.Run.ID)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatOptTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func decodeMap(raw string) map[string]any {
	if raw == "" || raw == "null" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// stringFrom reads a non-empty string out of a decoded extras map.
func stringFrom(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// orString returns first when it is non-empty, else fallback.
func orString(first, fallback string) string {
	if first != "" {
		return first
	}
	return fallback
}

func describeWorkspace(w dataset.Workspace) string {
	parts := []string{string(w.Type)}
	if w.Repo != "" {
		parts = append(parts, "repo="+w.Repo)
	}
	if w.Ref != "" {
		parts = append(parts, "ref="+w.Ref)
	}
	if w.Assets != "" {
		parts = append(parts, "assets="+w.Assets)
	}
	if len(w.Files) > 0 {
		parts = append(parts, fmt.Sprintf("%d inline file(s)", len(w.Files)))
	}
	if len(w.Setup) > 0 {
		parts = append(parts, fmt.Sprintf("%d setup command(s)", len(w.Setup)))
	}
	return strings.Join(parts, ", ")
}

func describeOutput(o *dataset.Output) string {
	if o == nil {
		return ""
	}
	parts := []string{string(o.Type)}
	if len(o.Paths) > 0 {
		parts = append(parts, "paths="+strings.Join(o.Paths, ","))
	}
	if len(o.IgnorePaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d ignore pattern(s)", len(o.IgnorePaths)))
	}
	return strings.Join(parts, ", ")
}

func describeLimits(l *dataset.Limits) string {
	if l == nil {
		return ""
	}
	var parts []string
	if l.MaxTurns > 0 {
		parts = append(parts, fmt.Sprintf("max_turns=%d", l.MaxTurns))
	}
	if l.TimeoutS > 0 {
		parts = append(parts, fmt.Sprintf("timeout=%ds", l.TimeoutS))
	}
	if l.MaxCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("max_cost=$%.4f", l.MaxCostUSD))
	}
	return strings.Join(parts, ", ")
}

func requiredSuffix(required bool) string {
	if required {
		return ", required"
	}
	return ""
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func humanDuration(ms float64) string {
	switch {
	case ms >= 60000:
		return fmt.Sprintf("%.1fm", ms/60000)
	case ms >= 1000:
		return fmt.Sprintf("%.1fs", ms/1000)
	default:
		return fmt.Sprintf("%.0fms", ms)
	}
}

func sortedGroupKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

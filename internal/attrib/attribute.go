package attrib

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/store"
)

// Meta is what a sweep records about itself on every run row it creates. It
// lives in the run's config blob so that an attribution months later needs the
// database and nothing else -- the matrix document that produced the sweep may
// have been edited or deleted by then, and a comparison whose control level is
// guessed is not a comparison.
type Meta struct {
	Name    string `json:"name,omitempty"`
	Axis    Axis   `json:"axis"`
	Control string `json:"control"`
	Notes   string `json:"notes,omitempty"`
	// Changed lists the configuration keys this run's level varied, which is
	// the evidence that the sweep moved one thing.
	//
	// It travels with the run instead of being recomputed at attribution time
	// because the run row keeps only the resolved kind/model/bin, not the spec
	// the diff was taken from. Decoding it back is what lets a reader tell a
	// level that varied nothing from a level whose diff was simply never
	// recorded -- two very different sweeps that look identical without it.
	Changed []string `json:"changed,omitempty"`
}

// Encode renders the metadata for storage in runs.config_json.
func (m Meta) Encode() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("attrib: encode sweep metadata: %w", err)
	}
	return string(b), nil
}

// DecodeMeta reads sweep metadata back out of a run's config blob. It returns
// false when the blob carries no sweep metadata, which is the normal state of a
// plain `aeval run`.
func DecodeMeta(configJSON string) (Meta, bool) {
	if strings.TrimSpace(configJSON) == "" || configJSON == "{}" {
		return Meta{}, false
	}
	var m Meta
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return Meta{}, false
	}
	if m.Axis == "" || m.Control == "" {
		return Meta{}, false
	}
	return m, true
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metric keys. They are the columns a comparison is made along, and the reason
// they are named rather than positional is that a report has to say which one
// moved.
const (
	MetricPassRate  = "pass_rate"
	MetricScore     = "score"
	MetricTokens    = "tokens"
	MetricCost      = "cost_usd"
	MetricDuration  = "duration_ms"
	MetricTurns     = "turns"
	MetricToolCalls = "tool_calls"
)

// MetricDef describes one measurable difference.
type MetricDef struct {
	Key   string
	Label string
	Unit  string
	// LowerIsBetter inverts which side of zero counts as a win. Getting this
	// wrong is not a cosmetic bug: it paints a token saving as a regression.
	LowerIsBetter bool
	// Primary marks the metric a change is judged on. There is exactly one, and
	// it is correctness, because a cheaper wrong answer is not an improvement.
	Primary bool
}

// Metrics is the ordered set a comparison covers. Correctness first, then what
// it cost, then how much work it took.
var Metrics = []MetricDef{
	{Key: MetricPassRate, Label: "pass rate", Unit: "pp", Primary: true},
	{Key: MetricScore, Label: "mean score"},
	{Key: MetricTokens, Label: "tokens", LowerIsBetter: true},
	{Key: MetricCost, Label: "cost", Unit: "USD", LowerIsBetter: true},
	{Key: MetricDuration, Label: "time", Unit: "ms", LowerIsBetter: true},
	{Key: MetricTurns, Label: "turns", LowerIsBetter: true},
	{Key: MetricToolCalls, Label: "tool calls", LowerIsBetter: true},
}

// Metric returns the definition for a key.
func Metric(key string) (MetricDef, bool) {
	for _, m := range Metrics {
		if m.Key == key {
			return m, true
		}
	}
	return MetricDef{}, false
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

// LevelResult is one level's run, summarised.
type LevelResult struct {
	Name      string         `json:"name"`
	Notes     string         `json:"notes,omitempty"`
	RunID     string         `json:"run_id"`
	Status    string         `json:"status"`
	IsControl bool           `json:"is_control"`
	Changed   []string       `json:"changed,omitempty"`
	Summary   runner.Summary `json:"summary"`
	// PassRate is nil when the level had nothing graded, which is a different
	// statement from a pass rate of zero.
	PassRate *float64 `json:"pass_rate,omitempty"`
}

// PairedStat is one metric's difference across the levels, measured per task.
type PairedStat struct {
	Metric        string `json:"metric"`
	Label         string `json:"label"`
	Unit          string `json:"unit,omitempty"`
	LowerIsBetter bool   `json:"lower_is_better"`
	// Primary marks correctness, the metric a change is judged on.
	Primary     bool    `json:"primary"`
	N           int     `json:"n"`
	ControlMean float64 `json:"control_mean"`
	LevelMean   float64 `json:"level_mean"`
	MeanDelta   float64 `json:"mean_delta"`
	MedianDelta float64 `json:"median_delta"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	Ties        int     `json:"ties"`
	// P is the two-sided exact sign test over the non-tied pairs: could a split
	// this lopsided come from a fair coin? It is a significance test and only
	// that -- it makes no claim about how large the difference is, and with
	// fewer than six non-tied pairs it cannot reach 0.05 however lopsided the
	// split.
	P float64 `json:"p"`
	// Testable is false when every pair tied, so no test exists. A saturated
	// dataset -- everything passing on both sides -- lands here, and saying so
	// is more useful than reporting p = 1.
	Testable bool `json:"testable"`
}

// TaskDelta is one task's numbers on both sides of the comparison.
type TaskDelta struct {
	TaskID   string             `json:"task_id"`
	Category string             `json:"category,omitempty"`
	Control  map[string]float64 `json:"control"`
	Level    map[string]float64 `json:"level"`
	Delta    map[string]float64 `json:"delta"`
	// Verdict is about correctness only: "fixed", "broke", "same" or
	// "not_graded". It deliberately ignores cost, because a task that became
	// cheaper while getting the wrong answer did not improve.
	Verdict string `json:"verdict"`
	// OnlyIn is non-empty when the task exists on one side only; such a pair is
	// dropped rather than counted as zero.
	OnlyIn string `json:"only_in,omitempty"`
}

// Comparison is one level measured against the control.
type Comparison struct {
	Level      string       `json:"level"`
	LevelNotes string       `json:"level_notes,omitempty"`
	Control    string       `json:"control"`
	RunID      string       `json:"run_id"`
	RunStatus  string       `json:"run_status"`
	Common     int          `json:"common"`
	Stats      []PairedStat `json:"stats"`
	Pairs      []TaskDelta  `json:"pairs"`
	// Fixed and Broke list the tasks whose correctness changed, by id. These are
	// the pairs a reader should look at first.
	Fixed []string `json:"fixed,omitempty"`
	Broke []string `json:"broke,omitempty"`
}

// Finding is a ranked statement about one metric.
type Finding struct {
	Level     string  `json:"level"`
	Control   string  `json:"control"`
	Metric    string  `json:"metric"`
	Label     string  `json:"label"`
	Direction string  `json:"direction"` // better | worse | no_clear_difference
	Unit      string  `json:"unit,omitempty"`
	MeanDelta float64 `json:"mean_delta"`
	N         int     `json:"n"`
	P         float64 `json:"p"`
	Testable  bool    `json:"testable"`
	// PassWins and PassLosses carry the correctness result alongside every
	// finding, so a token saving cannot be read on its own.
	PassWins   int    `json:"pass_wins"`
	PassLosses int    `json:"pass_losses"`
	Text       string `json:"text"`
}

// Attribution is the whole answer.
type Attribution struct {
	SweepID     string        `json:"sweep_id"`
	Name        string        `json:"name,omitempty"`
	Notes       string        `json:"notes,omitempty"`
	Axis        Axis          `json:"axis"`
	Control     string        `json:"control"`
	Levels      []LevelResult `json:"levels"`
	Comparisons []Comparison  `json:"comparisons"`
	Findings    []Finding     `json:"findings"`
	Caveats     []string      `json:"caveats"`
	// DatasetDrift is true when the levels were not run over the same dataset
	// content, in which case no comparison in here means anything.
	DatasetDrift bool `json:"dataset_drift"`
}

// Alpha is the significance level findings are called at. It is a constant
// rather than a flag because a reader comparing two reports has to know it.
const Alpha = 0.05

// ---------------------------------------------------------------------------
// Attribution
// ---------------------------------------------------------------------------

// Attribute compares the levels of a sweep. It reads only the database: which
// levels exist, which is the control, and which axis was varied all come from
// the rows the sweep wrote about itself.
func Attribute(ctx context.Context, st *store.Store, sweepID string) (*Attribution, error) {
	runs, err := st.ListRuns(ctx, store.RunFilter{SweepID: sweepID})
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("attrib: no runs recorded for sweep %q", sweepID)
	}

	meta, ok := DecodeMeta(runs[0].ConfigJSON)
	if !ok {
		return nil, fmt.Errorf(
			"attrib: sweep %q did not record which level is the control;"+
				" a difference needs a reference to be a difference", sweepID)
	}

	byLevel := map[string]store.Run{}
	for _, r := range runs {
		level := r.FactorLevel
		if level == "" {
			return nil, fmt.Errorf("attrib: run %s is in sweep %q but names no factor level", r.ID, sweepID)
		}
		if prev, dup := byLevel[level]; dup {
			return nil, fmt.Errorf(
				"attrib: sweep %q has two runs for level %q (%s and %s);"+
					" a sweep is one run per level, so start a new sweep instead of re-running a level",
				sweepID, level, prev.ID, r.ID)
		}
		byLevel[level] = r
	}
	control, ok := byLevel[meta.Control]
	if !ok {
		return nil, fmt.Errorf("attrib: sweep %q has no run for its control level %q", sweepID, meta.Control)
	}

	att := &Attribution{
		SweepID: sweepID,
		Name:    meta.Name,
		Notes:   meta.Notes,
		Axis:    meta.Axis,
		Control: meta.Control,
	}

	// A sweep is only interpretable if every level saw the same tasks. A run
	// whose dataset hash differs is a different question, and comparing it would
	// silently mix two datasets into one number.
	drift := false
	for _, r := range runs {
		if r.DatasetHash != control.DatasetHash {
			drift = true
		}
	}
	att.DatasetDrift = drift

	levels := make([]string, 0, len(byLevel))
	for l := range byLevel {
		levels = append(levels, l)
	}
	sort.Strings(levels)

	aggs := map[string]map[string]TaskAgg{}
	for _, level := range levels {
		r := byLevel[level]
		attempts, err := st.ListAttempts(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		aggs[level] = aggregate(attempts)
		att.Levels = append(att.Levels, levelResult(level, r, attempts, level == meta.Control))
	}

	controlAgg := aggs[meta.Control]
	for _, level := range levels {
		if level == meta.Control {
			continue
		}
		cmp := compare(meta.Control, controlAgg, byLevel[meta.Control], level, aggs[level], byLevel[level])
		att.Comparisons = append(att.Comparisons, cmp)
		att.Findings = append(att.Findings, findings(cmp)...)
	}

	rank(att.Findings)
	att.Caveats = caveats(att, runs)
	return att, nil
}

func levelResult(name string, r store.Run, attempts []store.Attempt, isControl bool) LevelResult {
	lr := LevelResult{
		Name:      name,
		RunID:     r.ID,
		Status:    r.Status,
		IsControl: isControl,
		Summary:   runner.Summarize(attempts, distinctTasks(attempts)),
	}
	// The run's own blob, not the sweep's: which keys a level varies is a
	// property of the level.
	if m, ok := DecodeMeta(r.ConfigJSON); ok {
		lr.Changed = m.Changed
	}
	if pr, ok := lr.Summary.PassRate(); ok {
		lr.PassRate = &pr
	}
	return lr
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// TaskAgg is one task's numbers inside one level, averaged over samples.
//
// Averaging per task before comparing is what makes the comparison paired: with
// three samples, one level's lucky run does not count three times.
type TaskAgg struct {
	TaskID   string
	Category string
	Samples  int
	Graded   int
	Passed   int
	ScoreSum float64
	TokenSum int
	CostSum  float64
	DurSum   int64
	TurnsSum int
	ToolsSum int
	// NotOK counts cells that timed out or errored. They stay in the numbers --
	// a timeout is a datum -- but their presence is reported.
	NotOK int
}

func aggregate(attempts []store.Attempt) map[string]TaskAgg {
	out := make(map[string]TaskAgg, len(attempts))
	for _, a := range attempts {
		agg := out[a.TaskID]
		agg.TaskID = a.TaskID
		if a.Category != "" {
			agg.Category = a.Category
		}
		agg.Samples++
		if a.Graded {
			agg.Graded++
			agg.ScoreSum += a.Score
			if a.Passed {
				agg.Passed++
			}
		}
		agg.TokenSum += a.TokenTotal()
		agg.CostSum += a.CostUSD
		agg.DurSum += a.DurationMs
		agg.TurnsSum += a.Turns
		agg.ToolsSum += a.ToolCalls
		if a.Status != store.CellOK {
			agg.NotOK++
		}
		out[a.TaskID] = agg
	}
	return out
}

// values projects the aggregate onto the metric keys. A key is absent when the
// task has no value for it, which is how an ungraded task is excluded from
// correctness comparisons rather than being scored as zero.
func (a TaskAgg) values() map[string]float64 {
	v := map[string]float64{}
	if a.Samples == 0 {
		return v
	}
	n := float64(a.Samples)
	v[MetricTokens] = float64(a.TokenSum) / n
	v[MetricCost] = a.CostSum / n
	v[MetricDuration] = float64(a.DurSum) / n
	v[MetricTurns] = float64(a.TurnsSum) / n
	v[MetricToolCalls] = float64(a.ToolsSum) / n
	if a.Graded > 0 {
		v[MetricPassRate] = 100 * float64(a.Passed) / float64(a.Graded)
		v[MetricScore] = a.ScoreSum / float64(a.Graded)
	}
	return v
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

func compare(controlName string, controlAgg map[string]TaskAgg, controlRun store.Run,
	levelName string, levelAgg map[string]TaskAgg, levelRun store.Run) Comparison {

	cmp := Comparison{
		Level:     levelName,
		Control:   controlName,
		RunID:     levelRun.ID,
		RunStatus: levelRun.Status,
	}

	ids := make([]string, 0, len(controlAgg)+len(levelAgg))
	seen := map[string]bool{}
	for id := range controlAgg {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range levelAgg {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	acc := map[string]*pairAcc{}
	for _, def := range Metrics {
		acc[def.Key] = &pairAcc{}
	}

	for _, id := range ids {
		c, inControl := controlAgg[id]
		l, inLevel := levelAgg[id]
		switch {
		case !inControl:
			cmp.Pairs = append(cmp.Pairs, TaskDelta{TaskID: id, OnlyIn: levelName})
			continue
		case !inLevel:
			cmp.Pairs = append(cmp.Pairs, TaskDelta{TaskID: id, OnlyIn: controlName})
			continue
		}

		cv, lv := c.values(), l.values()
		td := TaskDelta{
			TaskID:   id,
			Category: firstNonEmpty(c.Category, l.Category),
			Control:  map[string]float64{},
			Level:    map[string]float64{},
			Delta:    map[string]float64{},
		}
		for _, def := range Metrics {
			a, okA := cv[def.Key]
			b, okB := lv[def.Key]
			if !okA || !okB {
				continue
			}
			td.Control[def.Key] = a
			td.Level[def.Key] = b
			td.Delta[def.Key] = b - a
			acc[def.Key].add(a, b, def.LowerIsBetter)
		}
		td.Verdict = verdict(cv, lv)
		switch td.Verdict {
		case "fixed":
			cmp.Fixed = append(cmp.Fixed, id)
		case "broke":
			cmp.Broke = append(cmp.Broke, id)
		}
		cmp.Common++
		cmp.Pairs = append(cmp.Pairs, td)
	}

	for _, def := range Metrics {
		if a := acc[def.Key]; a.n > 0 {
			cmp.Stats = append(cmp.Stats, a.stat(def))
		}
	}
	return cmp
}

// verdict reads the correctness change only. Cost is not part of it on purpose:
// a task that got cheaper and wrong did not improve.
func verdict(control, level map[string]float64) string {
	c, okC := control[MetricPassRate]
	l, okL := level[MetricPassRate]
	if !okC || !okL {
		return "not_graded"
	}
	switch {
	case l > c:
		return "fixed"
	case l < c:
		return "broke"
	default:
		return "same"
	}
}

type pairAcc struct {
	n     int
	sum   float64
	cSum  float64
	lSum  float64
	wins  int
	loses int
	ties  int
	delta []float64
}

func (a *pairAcc) add(control, level float64, lowerIsBetter bool) {
	delta := level - control
	a.n++
	a.sum += delta
	a.cSum += control
	a.lSum += level
	a.delta = append(a.delta, delta)
	switch {
	case delta == 0:
		a.ties++
	case (delta < 0) == lowerIsBetter:
		// A negative delta helps when lower is better; a positive one helps
		// when higher is better.
		a.wins++
	default:
		a.loses++
	}
}

func (a *pairAcc) stat(def MetricDef) PairedStat {
	s := PairedStat{
		Metric:        def.Key,
		Label:         def.Label,
		Unit:          def.Unit,
		LowerIsBetter: def.LowerIsBetter,
		Primary:       def.Primary,
		N:             a.n,
		ControlMean:   a.cSum / float64(a.n),
		LevelMean:     a.lSum / float64(a.n),
		MeanDelta:     a.sum / float64(a.n),
		MedianDelta:   median(a.delta),
		Wins:          a.wins,
		Losses:        a.loses,
		Ties:          a.ties,
	}
	// The test runs over the pairs that were not ties, because a tie is not
	// evidence in either direction and counting it as a loss (or a win) would
	// make a metric that barely moves look decisive.
	tested := a.wins + a.loses
	if tested > 0 {
		s.Testable = true
		k := a.wins
		if a.loses < k {
			k = a.loses
		}
		s.P = math.Min(1, 2*signTail(tested, k))
	} else {
		s.P = 1
	}
	return s
}

// ---------------------------------------------------------------------------
// Findings
// ---------------------------------------------------------------------------

func findings(cmp Comparison) []Finding {
	var out []Finding
	passWins, passLosses := 0, 0
	for _, s := range cmp.Stats {
		if s.Metric == MetricPassRate {
			passWins, passLosses = s.Wins, s.Losses
		}
	}
	for _, s := range cmp.Stats {
		f := Finding{
			Level:      cmp.Level,
			Control:    cmp.Control,
			Metric:     s.Metric,
			Label:      s.Label,
			Unit:       s.Unit,
			MeanDelta:  s.MeanDelta,
			N:          s.N,
			P:          s.P,
			Testable:   s.Testable,
			PassWins:   passWins,
			PassLosses: passLosses,
		}
		f.Direction, f.Text = describe(cmp, s)
		out = append(out, f)
	}
	return out
}

// phrasing gives each metric a verb for each direction, so a token saving and a
// regression do not come out of one generic word like "improved".
type phrasing struct{ good, bad string }

var phrasings = map[string]phrasing{
	MetricPassRate:  {"passes more often", "passes less often"},
	MetricScore:     {"scores higher", "scores lower"},
	MetricTokens:    {"uses fewer tokens", "uses more tokens"},
	MetricCost:      {"costs less", "costs more"},
	MetricDuration:  {"finishes faster", "finishes slower"},
	MetricTurns:     {"takes fewer turns", "takes more turns"},
	MetricToolCalls: {"makes fewer tool calls", "makes more tool calls"},
}

// phrasingFor falls back to a generic pair rather than to an empty verb, so a
// metric added without a phrasing reads awkwardly instead of reading as fact.
func phrasingFor(key string) phrasing {
	if p, ok := phrasings[key]; ok {
		return p
	}
	return phrasing{"does better", "does worse"}
}

func describe(cmp Comparison, s PairedStat) (string, string) {
	if !s.Testable {
		return "no_clear_difference", fmt.Sprintf(
			"all %d paired tasks scored identically for %q and %q on %s, so this dataset cannot separate them on it",
			s.N, cmp.Level, cmp.Control, s.Label)
	}

	// The direction comes from the win/loss split rather than from the mean,
	// because a single outlier can move a mean far enough to describe a metric
	// that the level lost on six of eight tasks as an improvement. Both numbers
	// are printed, so a disagreement between them is visible rather than hidden.
	wins := s.Wins > s.Losses
	amount := trimNum(math.Abs(s.MeanDelta))
	if s.Unit != "" {
		amount += " " + s.Unit
	}

	if s.P >= Alpha {
		return "no_clear_difference", fmt.Sprintf(
			"%q and %q are not separable on %s: %s apart on average, with %d of %d pairs favouring %q (p=%.3f)",
			cmp.Level, cmp.Control, s.Label, amount, s.Wins, s.N, cmp.Level, s.P)
	}
	if wins {
		return "better", fmt.Sprintf(
			"%q %s than %q on %s: %s apart on average, in %d of %d pairs (p=%.3f)",
			cmp.Level, phrasingFor(s.Metric).good, cmp.Control, s.Label, amount, s.Wins, s.N, s.P)
	}
	return "worse", fmt.Sprintf(
		"%q %s than %q on %s: %s apart on average, in %d of %d pairs (p=%.3f)",
		cmp.Level, phrasingFor(s.Metric).bad, cmp.Control, s.Label, amount, s.Losses, s.N, s.P)
}

func trimNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

// rank orders findings so the most solid ones come first: significance, then
// size. A finding that has not separated from noise is still listed, at the
// bottom, because "we could not tell" is a result.
func rank(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		aSig, bSig := a.Testable && a.P < Alpha, b.Testable && b.P < Alpha
		if aSig != bSig {
			return aSig
		}
		aPrimary := a.Metric == MetricPassRate
		bPrimary := b.Metric == MetricPassRate
		if aPrimary != bPrimary {
			return aPrimary
		}
		if a.P != b.P {
			return a.P < b.P
		}
		return math.Abs(a.MeanDelta) > math.Abs(b.MeanDelta)
	})
}

// ---------------------------------------------------------------------------
// Caveats
// ---------------------------------------------------------------------------

func caveats(att *Attribution, runs []store.Run) []string {
	others := []string{}
	for _, a := range Axes {
		if a != att.Axis {
			others = append(others, string(a))
		}
	}
	out := []string{
		fmt.Sprintf("one axis at a time: every level ran with %s held fixed, so this sweep cannot see an"+
			" interaction between %s and them -- a difference that only appears in combination will not show up here",
			strings.Join(others, " and "), att.Axis),
		fmt.Sprintf("the sign test asks whether a split is consistent with chance, at alpha=%.2f; it says nothing about"+
			" whether the difference is large enough to care about, and with fewer than six non-tied pairs it cannot"+
			" reach significance however lopsided the split", Alpha),
	}
	if att.Axis == AxisSkill {
		out = append(out, "a skill only affects an attempt if the agent chooses to use it; a null result means the agent"+
			" did not benefit from it, which is not the same as the skill being broken")
	}
	if att.DatasetDrift {
		out = append(out, "THE LEVELS DID NOT RUN THE SAME DATASET: their dataset hashes differ, so the comparisons below"+
			" mix two questions and should not be read")
	}

	var notOK []string
	for _, r := range runs {
		if r.Status != store.RunCompleted {
			notOK = append(notOK, fmt.Sprintf("%s=%s", r.FactorLevel, r.Status))
		}
	}
	if len(notOK) > 0 {
		out = append(out, fmt.Sprintf("some levels did not finish (%s); their numbers cover part of the dataset,"+
			" and a partial level is not comparable with a complete one", strings.Join(notOK, ", ")))
	}

	samples := 0
	for _, r := range runs {
		samples = r.Samples
		break
	}
	if samples < 2 {
		out = append(out, "each task ran once per level, so a difference of one task and noise are the same number;"+
			" raise samples to separate them")
	}
	return out
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func distinctTasks(attempts []store.Attempt) int {
	seen := map[string]bool{}
	for _, a := range attempts {
		seen[a.TaskID] = true
	}
	return len(seen)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/skyitachi/agenteval/internal/dataset"
)

// Split selects how much of a session one task covers.
type Split string

const (
	// SplitSession emits one task per session: the opening request, measured
	// against the whole recorded trajectory. This is the default because a
	// session is what the human actually asked for and what the agent actually
	// did.
	SplitSession Split = "session"
	// SplitTurn emits one task per human turn. Use it when a session holds
	// several independent requests -- the follow-up "now do X too" is its own
	// task, and folding it into the first one would attribute the whole
	// session's cost to the first prompt.
	SplitTurn Split = "turn"
)

// Options controls how a session becomes evaluation data.
type Options struct {
	// Split decides the task granularity. Empty means SplitSession.
	Split Split

	// Grader, when set, attaches a grader to every emitted task. When empty no
	// grader is attached, and the task is measurable but not scorable.
	//
	// That default is deliberate. A session has no ground truth: the human
	// asked a question, the agent answered, and nothing in the transcript says
	// whether the answer was right. Inventing a grader here would fabricate a
	// correctness criterion the data does not support, and a resolve rate over
	// invented criteria is worse than no resolve rate. What the transcript
	// *does* support is a cost and time comparison, and that is what a
	// grader-less task is for.
	Grader string
	// GraderName names the grader. Empty derives it from the type.
	GraderName string
	// GraderConfig is the grader's config block, verbatim.
	GraderConfig map[string]any

	// Workspace binds the task to a repository or asset directory. With neither
	// set, the task runs in an empty workspace and the agent's answer is what is
	// measured -- which is the honest reading of a transcript that does not
	// record the commit it started from.
	WorkspaceRepo   string
	WorkspaceRef    string
	WorkspaceSubdir string
	WorkspaceAssets string

	// OutputType overrides the deliverable contract. Empty derives it: a
	// workspace-bound task expects a patch, an empty-workspace task expects an
	// answer on stdout.
	OutputType dataset.OutputType

	// TimeoutS and MaxTurns bound an attempt. Zero derives them from the
	// baseline (see deriveLimits), which is usually what you want: a task whose
	// recorded attempt took nine minutes should not inherit a budget sized for
	// a toy.
	TimeoutS int
	MaxTurns int

	// MinPromptRunes drops turns shorter than this under SplitTurn. Follow-ups
	// like "ok" or "try again" are turns but not tasks.
	MinPromptRunes int

	// IDPrefix is prepended to generated task ids, so two collections can share
	// one database without colliding.
	IDPrefix string
	// Category and Tags are applied to every emitted task.
	Category string
	Tags     []string
}

// Item is one emitted task together with the baseline it is measured against.
type Item struct {
	Task dataset.TaskSpec
	// Baseline is the recorded attempt.
	Baseline dataset.Baseline
	// BaselineFile is where Baseline is written, relative to the dataset
	// directory. Task.Baseline.File points at it.
	BaselineFile string
}

// Collect converts parsed sessions into tasks and baselines.
//
// A session with no human prompt is an error rather than a skipped entry: it
// means the capture was machine-driven or the transcript format moved, and
// silently producing nothing would look like "the session had nothing in it".
func Collect(sessions []*Session, opts Options) ([]Item, error) {
	if opts.Split == "" {
		opts.Split = SplitSession
	}
	switch opts.Split {
	case SplitSession, SplitTurn:
	default:
		return nil, fmt.Errorf("unknown split %q (want %s|%s)", opts.Split, SplitSession, SplitTurn)
	}

	var out []Item
	used := map[string]bool{}
	for _, s := range sessions {
		if len(s.Prompts) == 0 {
			return nil, fmt.Errorf("session %s: no human prompt found; the capture may be machine-driven", shortID(s.ID))
		}
		switch opts.Split {
		case SplitSession:
			it, err := buildItem(s, s.Prompts[0], s.Turns, s.Usage, s.CostUSD, s.BusyMs, s.DurationMs, "", opts, used)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
		case SplitTurn:
			for _, p := range s.Prompts {
				if runeLen(p.Text) < opts.MinPromptRunes {
					continue
				}
				slice := s.Turns[p.TurnIndex:]
				usage := sumTurns(slice)
				duration := turnDuration(s, p.Index)
				note := "per-turn baseline inside a longer session: " +
					"cost is not attributable to a single turn, so it is left unset"
				it, err := buildItem(s, p, slice, usage, 0, duration, duration, note, opts, used)
				if err != nil {
					return nil, err
				}
				out = append(out, it)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tasks collected (every prompt was shorter than %d runes)", opts.MinPromptRunes)
	}
	return out, nil
}

// CollectFile parses one transcript and converts it.
func CollectFile(path string, opts Options) ([]Item, error) {
	s, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	return Collect([]*Session{s}, opts)
}

func buildItem(s *Session, p Prompt, turns []Turn, usage Usage, cost float64, busyMs, wallMs int64, note string, opts Options, used map[string]bool) (Item, error) {
	id := taskID(s, p, opts)
	base := id
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	used[id] = true

	ws := workspace(opts)
	output := opts.OutputType
	if output == "" {
		if ws.Type == dataset.WorkspaceEmpty {
			output = dataset.OutputStdout
		} else {
			output = dataset.OutputPatch
		}
	}

	baselineFile := filepath.ToSlash(filepath.Join("baselines", id+".json"))
	limits := deriveLimits(opts, busyMs, turns)

	task := dataset.TaskSpec{
		ID:        id,
		Category:  opts.Category,
		Tags:      append([]string(nil), opts.Tags...),
		Prompt:    p.Text,
		Workspace: ws,
		Output:    &dataset.Output{Type: output},
		Limits:    &limits,
		Baseline: &dataset.BaselineRef{
			File:  baselineFile,
			Notes: "recorded session " + shortID(s.ID),
		},
		Metadata: metadata(s, p, turns, usage, busyMs),
	}
	if opts.Grader != "" {
		name := opts.GraderName
		if name == "" {
			name = opts.Grader
		}
		task.Graders = []dataset.GraderSpec{{
			Type:   opts.Grader,
			Name:   name,
			Config: opts.GraderConfig,
		}}
	}

	return Item{
		Task: task,
		Baseline: dataset.Baseline{
			ID:          s.ID,
			Source:      SourceName,
			Notes:       note,
			TaskID:      id,
			Cwd:         s.Cwd,
			GitBranch:   s.GitBranch,
			GitHead:     s.GitHead,
			AgentName:   "claude-code",
			AgentModel:  strings.Join(s.Models, ","),
			StartedAt:   s.StartedAt,
			EndedAt:     s.EndedAt,
			FinalOutput: lastAssistantText(turns),
			Turns:       baselineTurns(turns),
			Usage: dataset.BaselineUsage{
				InputTokens:       usage.InputTokens,
				OutputTokens:      usage.OutputTokens,
				CacheReadTokens:   usage.CacheReadTokens,
				CacheCreateTokens: usage.CacheCreateTokens,
			},
			CostUSD:     cost,
			DurationMs:  wallMs,
			BusyMs:      busyMs,
			EditedFiles: editedFiles(s, turns),
		},
		BaselineFile: baselineFile,
	}, nil
}

func workspace(opts Options) dataset.Workspace {
	switch {
	case opts.WorkspaceRepo != "":
		return dataset.Workspace{
			Type:   dataset.WorkspaceGit,
			Repo:   opts.WorkspaceRepo,
			Ref:    opts.WorkspaceRef,
			Subdir: opts.WorkspaceSubdir,
		}
	case opts.WorkspaceAssets != "":
		return dataset.Workspace{Type: dataset.WorkspaceFiles, Assets: opts.WorkspaceAssets}
	default:
		return dataset.Workspace{Type: dataset.WorkspaceEmpty}
	}
}

// deriveLimits sizes an attempt against the recorded one.
//
// The failure this prevents is quiet and expensive: a task whose baseline spent
// nine minutes getting to the answer, run under a default 30-second timeout,
// reports a timeout and looks like the agent could not do it. The budget is
// therefore anchored to the baseline, with a floor so that a cheap task still
// gets enough room to think.
func deriveLimits(opts Options, busyMs int64, turns []Turn) dataset.Limits {
	lim := dataset.Limits{TimeoutS: opts.TimeoutS, MaxTurns: opts.MaxTurns}
	if lim.TimeoutS == 0 {
		derived := int(busyMs/1000) * 3
		if derived < 300 {
			derived = 300
		}
		lim.TimeoutS = derived
	}
	if lim.MaxTurns == 0 {
		assistant := 0
		for _, t := range turns {
			if t.Role == RoleAssistant {
				assistant++
			}
		}
		derived := assistant * 2
		if derived < 20 {
			derived = 20
		}
		lim.MaxTurns = derived
	}
	return lim
}

// metadata is the free-form provenance block on a task. The engine never reads
// it; a human does.
//
// The token and time fields describe the same baseline the accompanying
// baseline file does, so they take the sliced values rather than re-reading the
// session. Under SplitTurn those differ -- the session total is not this task's
// baseline -- and publishing both in one record is how the two came to
// contradict each other.
func metadata(s *Session, p Prompt, turns []Turn, usage Usage, busyMs int64) map[string]any {
	m := map[string]any{
		"source":          SourceName,
		"session_id":      s.ID,
		"prompt_index":    p.Index,
		"recorded_at":     s.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
		"turns":           len(turns),
		"tool_calls":      countTools(turns),
		"baseline_ms":     busyMs,
		"baseline_tokens": usage.Total(),
	}
	if s.Cwd != "" {
		m["cwd"] = s.Cwd
	}
	if s.GitBranch != "" {
		m["git_branch"] = s.GitBranch
	}
	if s.Version != "" {
		m["claude_version"] = s.Version
	}
	if len(s.Models) > 0 {
		m["models"] = s.Models
	}
	if s.Path != "" {
		m["transcript"] = s.Path
	}
	return m
}

func taskID(s *Session, p Prompt, opts Options) string {
	id := opts.IDPrefix + shortID(s.ID)
	if opts.Split == SplitTurn {
		id = fmt.Sprintf("%s-p%d", id, p.Index)
	}
	return sanitizeID(id)
}

// sanitizeID keeps generated ids inside the dataset id pattern
// (^[A-Za-z0-9][A-Za-z0-9._-]*$).
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "task"
	}
	return out
}

func shortID(id string) string {
	if id == "" {
		return "unknown"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func turnDuration(s *Session, promptIndex int) int64 {
	if promptIndex >= 0 && promptIndex < len(s.TurnDurations) {
		return s.TurnDurations[promptIndex]
	}
	return 0
}

func sumTurns(turns []Turn) Usage {
	var u Usage
	for _, t := range turns {
		if t.Usage == nil {
			continue
		}
		u.InputTokens += t.Usage.InputTokens
		u.OutputTokens += t.Usage.OutputTokens
		u.CacheReadTokens += t.Usage.CacheReadTokens
		u.CacheCreateTokens += t.Usage.CacheCreateTokens
	}
	return u
}

func countTools(turns []Turn) int {
	n := 0
	for _, t := range turns {
		n += len(t.Tools)
	}
	return n
}

func lastAssistantText(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == RoleAssistant && strings.TrimSpace(turns[i].Text) != "" {
			return strings.TrimSpace(turns[i].Text)
		}
	}
	return ""
}

func baselineTurns(turns []Turn) []dataset.BaselineTurn {
	out := make([]dataset.BaselineTurn, 0, len(turns))
	for _, t := range turns {
		bt := dataset.BaselineTurn{
			Role:       t.Role,
			Text:       t.Text,
			Thinking:   t.Thinking,
			DurationMs: t.DurationMs,
		}
		for _, c := range t.Tools {
			bt.Tools = append(bt.Tools, dataset.BaselineToolCall{Name: c.Name, Input: c.Input})
		}
		out = append(out, bt)
	}
	return out
}

// editedFiles reports which files the recorded session touched.
//
// A per-turn slice cannot answer this: the transcript's file snapshots name the
// message they belong to, but they carry the cumulative set of tracked files
// rather than a per-turn delta, so attributing an edit to a single turn would be
// a guess. Session-scoped slices get the real list; the rest get nothing.
func editedFiles(s *Session, turns []Turn) []string {
	if len(turns) != len(s.Turns) {
		return nil
	}
	return append([]string(nil), s.EditedFiles...)
}

func runeLen(s string) int { return len([]rune(s)) }

// ---------------------------------------------------------------------------
// Writing a dataset directory
// ---------------------------------------------------------------------------

// WriteDataset writes items into dir as a loadable dataset: a manifest, a
// tasks file, and one baseline document per task.
//
// The manifest is emitted rather than expected because a collected dataset
// should be immediately runnable -- `collect` then `run` with nothing in
// between.
func WriteDataset(dir string, name string, description string, items []Item) error {
	if len(items) == 0 {
		return fmt.Errorf("nothing to write")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tasksPath := filepath.Join(dir, "tasks.jsonl")
	var buf strings.Builder
	for _, it := range items {
		// The resolved-only fields must not leak into the file: they are
		// derived at load time and a stale copy would be silently ignored.
		t := it.Task
		t.ResolvedPrompt = ""
		t.ResolvedBaseline = nil
		t.DatasetDir = ""
		line, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(tasksPath, []byte(buf.String()), 0o644); err != nil {
		return err
	}

	for _, it := range items {
		p := filepath.Join(dir, filepath.FromSlash(it.BaselineFile))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		body, err := json.MarshalIndent(it.Baseline, "", "  ")
		if err != nil {
			return fmt.Errorf("baseline %s: %w", it.Baseline.TaskID, err)
		}
		if err := os.WriteFile(p, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}

	manifest := dataset.Manifest{
		Name:        name,
		Description: description,
		TasksFile:   "tasks.jsonl",
	}
	head := "# Generated by `aeval collect` from recorded Claude Code sessions.\n" +
		"# Edit freely: the tasks and baselines below are ordinary dataset files.\n"
	body, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "dataset.yaml"), append([]byte(head), body...), 0o644)
}

// Summarize describes what was collected, for the CLI to print.
func Summarize(items []Item) string {
	var tools, tokens int
	edited := map[string]bool{}
	models := map[string]bool{}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.Task.ID)
		tools += len(it.Baseline.Turns)
		tokens += it.Baseline.Usage.Total()
		for _, f := range it.Baseline.EditedFiles {
			edited[f] = true
		}
		if it.Baseline.AgentModel != "" {
			models[it.Baseline.AgentModel] = true
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s): %s\n", len(items), strings.Join(ids, ", "))
	fmt.Fprintf(&b, "baseline tokens: %d   edited files: %d   models: %s\n",
		tokens, len(edited), strings.Join(keys(models), ", "))
	return b.String()
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

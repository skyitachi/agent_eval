package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// against the turns that answered it. This is the default because a session
	// is what the human actually asked for and what the agent actually did.
	//
	// On a session the human kept asking in, that is the first prompt only. The
	// slice stops at the next prompt because the whole trajectory answers
	// questions this task never asks, and a replay measured against it would be
	// reported as cheaper than a recording it does not correspond to. So the
	// prompts past the first are not collected at all -- which is a fact about
	// the dataset nothing else records, so the baseline says it (see buildItem)
	// and the command warns about it. Use SplitTurn to collect each prompt as
	// its own task.
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

	// WorkspaceCwd binds every task to the directory its own recording ran in,
	// instead of to one directory named up front.
	//
	// This is the only way to reproduce those files at all: a transcript
	// records the branch a session ran on and not the commit, so nothing in the
	// capture can reconstruct the tree. What this flag can do is put the
	// directory back as it stands *now*, which is a decision the caller makes
	// rather than one this package makes for them -- see bindCwd for what that
	// is and is not.
	WorkspaceCwd bool

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
	// SnapshotFrom is a host directory to copy into Task.Workspace.Assets when
	// the dataset is written. Empty for every other kind of workspace.
	//
	// It is a field rather than something the writer re-derives because the
	// copy has to happen where the dataset directory is known, which is not
	// here -- and because a dry run must plan the workspace without performing
	// it.
	SnapshotFrom string
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
			// The session is one task: the opening request, measured against the
			// turns that answered it. Those are the turns SplitTurn would give
			// the first prompt, and the two agree on a session with one prompt --
			// which is the session this split is for. On a session where the
			// human kept asking, the whole trajectory is the answer to a
			// question the task never asks, and comparing against it would
			// report every replay as cheaper than a recording it does not
			// correspond to.
			it, err := buildItem(s, 0, opts, used)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
		case SplitTurn:
			for i, p := range s.Prompts {
				if runeLen(p.Text) < opts.MinPromptRunes {
					continue
				}
				it, err := buildItem(s, i, opts, used)
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

// turnSlice is the part of the conversation that answers prompts[from].
//
// It ends at the next prompt, not at the end of the session: everything between
// two prompts is the answer to the earlier one, and letting it run on charges
// this task for every turn that follows it. A prompt dropped by MinPromptRunes
// still ends the slice, because the turns after it answer it rather than the
// task before it.
func turnSlice(s *Session, from int) []Turn {
	end := len(s.Turns)
	if from+1 < len(s.Prompts) {
		end = s.Prompts[from+1].TurnIndex
	}
	return s.Turns[s.Prompts[from].TurnIndex:end]
}

// buildItem turns one prompt and the turns answering it into a task.
func buildItem(s *Session, promptIndex int, opts Options, used map[string]bool) (Item, error) {
	p := s.Prompts[promptIndex]
	turns := turnSlice(s, promptIndex)

	// The session-scoped figures describe the whole conversation, so they apply
	// only where the slice is the whole conversation. A session with one prompt
	// is that case; on a longer one they would report the cost of work this task
	// never asked for, and a replay would look cheaper than a recording it does
	// not correspond to.
	var (
		usage  = sumTurns(turns)
		busyMs = sliceBusyMs(turns)
		wallMs = sliceSpanMs(turns)
		cost   float64
		note   string
	)
	if len(turns) == len(s.Turns) {
		usage, busyMs, wallMs, cost = s.Usage, s.BusyMs, s.DurationMs, s.CostUSD
	} else {
		note = "per-turn baseline inside a longer session: cost is not attributable to a single turn, so it is left unset"
	}

	id := taskID(s, p, opts)
	base := id
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	used[id] = true

	ws := workspace(opts)
	// A binding note describes what the workspace is, so it leads: it says
	// whether the replay starts where the recording did, which is the first
	// thing a reader of the comparison needs to know.
	var (
		boundNote string
		snapshot  string
	)
	if opts.WorkspaceCwd {
		var err error
		ws, boundNote, snapshot, err = bindCwd(s, opts)
		if err != nil {
			return Item{}, err
		}
	}
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

	// The capture's own caveats lead, then the split's. Both are facts about
	// what the numbers beside them do and do not mean, so they travel together
	// with the baseline rather than living only in the parser that produced it.
	notes := append([]string{}, s.BaselineNotes...)
	if note != "" {
		notes = append(notes, note)
	}
	// The split note above says the baseline is a slice rather than the session.
	// This says the slice is all the dataset holds: under the session split the
	// prompts past the first are not collected, and nothing in a one-task
	// collection otherwise says how many prompts the recording had. A reader
	// who assumed the dataset covers the session would be wrong in the
	// direction that matters -- they would take a third of a session for all of
	// it.
	if opts.Split == SplitSession && len(s.Prompts) > 1 {
		notes = append(notes, fmt.Sprintf(
			"the recorded session holds %d prompts and this dataset collected the first one only, "+
				"so the other %d are not in it (--split turn collects each prompt as its own task)",
			len(s.Prompts), len(s.Prompts)-1))
	}
	if boundNote != "" {
		notes = append(notes, boundNote)
	}
	// A replay starts from no workspace unless the caller bound one, and the
	// transcript does not contain the files the session worked on. Recording
	// that on the baseline is what stops the report from presenting the two
	// attempts as the same task: the recording had a working directory and the
	// replay has an empty one, and nothing else in the record says so.
	//
	// The claim is kept to what is known -- the session ran somewhere, and the
	// replay does not start there. Whether the session actually needed those
	// files is not knowable from a transcript, and saying "the files it worked
	// on are missing" would be inventing that part.
	if ws.Type == dataset.WorkspaceEmpty && s.Cwd != "" {
		notes = append(notes, "the recorded session ran in "+s.Cwd+
			"; this task replays in an empty workspace")
	}

	task := dataset.TaskSpec{
		ID:        id,
		Category:  opts.Category,
		Tags:      append([]string(nil), opts.Tags...),
		Prompt:    p.Text,
		Workspace: ws,
		Output:    &dataset.Output{Type: output},
		Limits:    &limits,
		// The session and the prompt's place in it, as fields. The same two
		// facts also go into Metadata below, for the human reading the task;
		// these are the copies the engine may act on, which is what lets
		// `run --chain-sessions` put a recording's turns back in one workspace
		// in the order they were asked.
		Session:     s.ID,
		PromptIndex: intPtr(p.Index),
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
			Source:      s.sourceName(),
			Notes:       strings.Join(notes, "; "),
			TaskID:      id,
			Cwd:         s.Cwd,
			GitBranch:   s.GitBranch,
			GitHead:     s.GitHead,
			AgentName:   s.agentName(),
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
		SnapshotFrom: snapshot,
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

// ---------------------------------------------------------------------------
// Binding a task to the directory its recording ran in
// ---------------------------------------------------------------------------

// maxSnapshotBytes caps what collect will copy into a dataset for one recording.
//
// The copy exists so a replay can have the files the recording had, and the
// cost of that is a dataset that carries them. A directory past this size is
// refused rather than silently copied: a gigabyte of assets turns every
// `validate`, every `run` and every report into a copy of it, and the caller is
// better off binding a repository instead.
const maxSnapshotBytes = 256 << 20

// bindCwd binds one task to the directory its recording ran in, and says what
// the binding is.
//
// Two things are being decided, and both follow from what a transcript holds.
// It records the directory a session ran in and the branch it was on, but not
// the commit, so the tree the agent actually started from is not recoverable
// from the capture -- for any workspace at all. What is recoverable is:
//
//   - a repository with a commit: cloned at the recorded branch, so the replay
//     starts from that branch's current tip. The history is dropped by the
//     materializer, which is what stops the agent reading the fix out of it.
//   - anything else: copied whole, exactly as it stands now.
//
// The second is the one to be careful with, and the note says so: "as it stands
// now" is after the recorded session finished, so if that session fixed
// something in there, the replay starts with it already fixed. That is the
// caller's decision to make and not this package's, which is why it is printed
// and recorded rather than assumed -- and it is also why the first case is
// preferred whenever a commit exists to clone.
//
// The error for a directory that is gone is deliberate. --workspace-cwd means
// "use the directory the recording ran in", and quietly falling back to an
// empty workspace would produce a task that looks bound and measures nothing.
func bindCwd(s *Session, opts Options) (dataset.Workspace, string, string, error) {
	cwd := s.Cwd
	if cwd == "" {
		return dataset.Workspace{}, "", "", fmt.Errorf(
			"session %s records no working directory, so --workspace-cwd has nothing to bind "+
				"(use --workspace-repo to name one)", shortID(s.ID))
	}
	if info, err := os.Stat(cwd); err != nil {
		return dataset.Workspace{}, "", "", fmt.Errorf(
			"the directory session %s ran in (%s) is not there any more, so --workspace-cwd cannot "+
				"reproduce it (pass --workspace-repo to point somewhere that exists): %w",
			shortID(s.ID), cwd, err)
	} else if !info.IsDir() {
		return dataset.Workspace{}, "", "", fmt.Errorf(
			"session %s ran in %s, which is not a directory", shortID(s.ID), cwd)
	}

	if hasCommit(cwd) {
		ref := s.GitBranch
		note := "bound to the repository the recorded session ran in (" + cwd + ")"
		if ref != "" {
			note += " at branch " + ref
		}
		note += "; the transcript records the branch and not the commit, so this is that branch as it stands now"
		return dataset.Workspace{Type: dataset.WorkspaceGit, Repo: cwd, Ref: ref}, note, "", nil
	}

	// Not a repository, or a repository with no commits -- which clones to
	// nothing. Copying is the only way to get those files back.
	assets := filepath.ToSlash(filepath.Join("assets", sanitizeID(s.ID)))
	note := "copied from the directory the recorded session ran in (" + cwd + ") as it stands now, " +
		"which is after that session finished: the transcript does not record the commit it started from"
	return dataset.Workspace{Type: dataset.WorkspaceFiles, Assets: assets}, note, cwd, nil
}

// hasCommit reports whether dir is a git repository with at least one commit.
//
// The question decides how a directory can be reproduced, and the second half of
// it is the part that is easy to get wrong: a repository with no commits is not
// an empty directory, but cloning it produces one.
func hasCommit(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD")
	// The environment is sanitised for the same reason the materializer
	// sanitises it: a developer's global config is an uncontrolled input, and a
	// trace2 hook here would write into the repository being inspected.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return cmd.Run() == nil
}

// snapshotTree copies src into dst, leaving .git behind.
//
// The history is dropped for the same reason the materializer drops it from a
// clone: a .git in the workspace is a history the agent can read the answer out
// of, and it would also be committed as though it were part of the baseline the
// agent started from.
func snapshotTree(src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip sockets, devices, fifos
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxSnapshotBytes {
			return fmt.Errorf("the directory is larger than the %d MiB collect will copy into a "+
				"dataset; bind a repository with --workspace-repo instead", maxSnapshotBytes>>20)
		}
		return copyFileInto(path, filepath.Join(dst, rel))
	})
}

func copyFileInto(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
		"source":          s.sourceName(),
		"session_id":      s.ID,
		"prompt_index":    p.Index,
		"recorded_at":     s.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
		"turns":           len(turns),
		"tool_calls":      countTools(turns),
		"baseline_ms":     busyMs,
		"baseline_tokens": usage.Total(),
		// The whole session beside the slice above. Where they differ the slice
		// is the baseline and these are context: the session is what the human
		// and the agent did around this request, which is worth keeping and is
		// not what the replay is measured against.
		"session_turns":    len(s.Turns),
		"session_tokens":   s.Usage.Total(),
		"session_prompts":  len(s.Prompts),
		"session_busy_ms":  s.BusyMs,
		"session_wall_ms":  s.DurationMs,
		"session_cost_usd": s.CostUSD,
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

// sourceName is the capture that produced this session, as recorded on the
// baseline. A session that names its own producer wins; otherwise it is a
// Claude Code transcript, which is what this package was first written for.
func (s *Session) sourceName() string {
	if s.Source != "" {
		return s.Source
	}
	return SourceName
}

// agentName is the agent the recorded session actually drove, which is not the
// same question as how the record was captured.
func (s *Session) agentName() string {
	if s.sourceName() == CursorSourceName {
		return "cursor"
	}
	return "claude-code"
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

// sliceDurationMs totals the turn clocks inside a slice. A turn whose build
// recorded no clock contributes nothing rather than a guess.
func sliceDurationMs(turns []Turn) int64 {
	var ms int64
	for _, t := range turns {
		ms += t.DurationMs
	}
	return ms
}

// sliceBusyMs is the machine time inside a slice.
//
// A slice is one turn: the prompt at its head and the assistant entries
// answering it, so the clock it carries sits on the last of those. Where the
// build recorded none, the message timestamps still say when the machine was
// working, and busyFromGaps reads them. Without that fallback every capture
// from a build that records no timings would hand its task a zero baseline and
// a default timeout -- which is what deriveLimits exists to prevent.
func sliceBusyMs(turns []Turn) int64 {
	if n := len(turns); n > 0 && turns[n-1].DurationMs > 0 {
		return sliceDurationMs(turns)
	}
	return busyFromGaps(turns)
}

// sliceSpanMs is the wall clock a slice covers, from its first message to its
// last. Zero when the capture carries no timestamps to measure it from.
func sliceSpanMs(turns []Turn) int64 {
	if len(turns) < 2 {
		return 0
	}
	first, last := turns[0].At, turns[len(turns)-1].At
	if first.IsZero() || last.IsZero() {
		return 0
	}
	if d := last.Sub(first); d > 0 {
		return d.Milliseconds()
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

// intPtr takes the address of an int, for the optional fields on a task.
func intPtr(v int) *int { return &v }

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

	lines, err := encodeTasks(items)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(lines), 0o644); err != nil {
		return err
	}
	if err := writeBaselines(dir, items); err != nil {
		return err
	}
	if err := writeSnapshots(dir, items); err != nil {
		return err
	}

	manifest := dataset.Manifest{
		Name:        name,
		Description: description,
		TasksFile:   "tasks.jsonl",
	}
	head := "# Generated by `aeval collect` from recorded " + captureLabel(items) + " sessions.\n" +
		"# Edit freely: the tasks and baselines below are ordinary dataset files.\n"
	body, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "dataset.yaml"), append([]byte(head), body...), 0o644)
}

// AppendResult reports what an append did, or would do, with each item.
type AppendResult struct {
	// Fresh is set when the directory held no dataset, so the items were
	// written as a new one.
	Fresh bool
	// Added are the items written, under their final ids.
	Added []Item
	// Skipped are the ids of items whose recording the dataset already holds.
	Skipped []string
	// Renamed maps a generated id to the one the item was written under, for
	// items whose id was already taken by a different task.
	Renamed map[string]string
}

// PlanAppend works out what AppendDataset would do, without writing anything.
//
// An item is skipped when the dataset already holds its recording: the same
// session and the same prompt. Collecting a working directory again after a few
// more sessions is what appending is for, and it has to add the new sessions
// without duplicating the old ones -- two copies of one recording would count
// it twice in every aggregate a run reports.
//
// An item whose id is taken by a *different* task keeps its recording and gets
// the next free suffix instead. Ids are an 8-character session prefix, so two
// sessions can collide, and refusing would make the second one uncollectable.
func PlanAppend(dir string, items []Item) (AppendResult, error) {
	ex, err := readExisting(dir)
	if err != nil {
		return AppendResult{}, err
	}
	if ex == nil {
		return AppendResult{Fresh: true, Added: items}, nil
	}

	res := AppendResult{Renamed: map[string]string{}}
	taken := func(id string) bool {
		if ex.ids[id] {
			return true
		}
		// A baseline document nothing points at is still someone's file.
		_, err := os.Stat(filepath.Join(dir, "baselines", id+".json"))
		return err == nil
	}
	for _, it := range items {
		if key := recordingKey(it.Task.Metadata); key != "" && ex.recordings[key] {
			res.Skipped = append(res.Skipped, it.Task.ID)
			continue
		}
		if taken(it.Task.ID) {
			base := it.Task.ID
			id := base
			for n := 2; taken(id); n++ {
				id = fmt.Sprintf("%s-%d", base, n)
			}
			it = renamed(it, id)
			res.Renamed[base] = id
		}
		ex.ids[it.Task.ID] = true
		if key := recordingKey(it.Task.Metadata); key != "" {
			ex.recordings[key] = true
		}
		res.Added = append(res.Added, it)
	}
	return res, nil
}

// AppendDataset adds items to the dataset already in dir, or writes a new one
// when dir holds none. See PlanAppend for what is skipped and what is renamed.
//
// The existing tasks are kept byte for byte, and so is the manifest: both may
// have been edited by hand since they were collected, which is what the
// generated header invites. Baselines are written before the task lines that
// point at them, so an interrupted append leaves an unreferenced file rather
// than a task whose baseline is missing.
func AppendDataset(dir, name, description string, items []Item) (AppendResult, error) {
	if len(items) == 0 {
		return AppendResult{}, fmt.Errorf("nothing to write")
	}
	res, err := PlanAppend(dir, items)
	if err != nil {
		return res, err
	}
	if res.Fresh {
		return res, WriteDataset(dir, name, description, items)
	}
	if len(res.Added) == 0 {
		return res, nil
	}

	ex, err := readExisting(dir)
	if err != nil {
		return res, err
	}
	if err := writeBaselines(dir, res.Added); err != nil {
		return res, err
	}
	if err := writeSnapshots(dir, res.Added); err != nil {
		return res, err
	}
	lines, err := encodeTasks(res.Added)
	if err != nil {
		return res, err
	}
	if ex.missingNewline {
		lines = "\n" + lines
	}
	f, err := os.OpenFile(ex.tasksPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return res, err
	}
	if _, err := f.WriteString(lines); err != nil {
		f.Close()
		return res, err
	}
	return res, f.Close()
}

// existingDataset is what an append needs to know about the dataset it adds to.
type existingDataset struct {
	tasksPath string
	ids       map[string]bool
	// recordings holds the recordingKey of every task that has one.
	recordings map[string]bool
	// missingNewline is set when the tasks file does not end in one, so the
	// first appended line would otherwise be glued to the last existing one.
	missingNewline bool
}

// readExisting reads the dataset in dir. A directory with no manifest holds no
// dataset and yields nil.
func readExisting(dir string) (*existingDataset, error) {
	var manifestPath string
	for _, n := range []string{"dataset.yaml", "dataset.yml"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			manifestPath = filepath.Join(dir, n)
			break
		}
	}
	if manifestPath == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var m dataset.Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}
	if m.TasksFile == "" {
		// Appending means rewriting the manifest, which would lose whatever
		// was written by hand in it.
		return nil, fmt.Errorf("%s declares its tasks inline, and collect only appends to a tasks_file; "+
			"use a new --out, or --overwrite to replace it", manifestPath)
	}

	ex := &existingDataset{
		tasksPath:  filepath.Join(dir, m.TasksFile),
		ids:        map[string]bool{},
		recordings: map[string]bool{},
	}
	note := func(id string, meta map[string]any) {
		ex.ids[id] = true
		if key := recordingKey(meta); key != "" {
			ex.recordings[key] = true
		}
	}
	for _, t := range m.Tasks {
		note(t.ID, t.Metadata)
	}

	body, err := os.ReadFile(ex.tasksPath)
	if errors.Is(err, os.ErrNotExist) {
		return ex, nil
	}
	if err != nil {
		return nil, err
	}
	ex.missingNewline = len(body) > 0 && body[len(body)-1] != '\n'
	for i, line := range strings.Split(string(body), "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "//") || strings.HasPrefix(text, "#") {
			continue
		}
		var t struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(text), &t); err != nil {
			// Appending past a line the loader cannot read would hand back a
			// dataset that does not load, with the cause buried in old data.
			return nil, fmt.Errorf("%s:%d: %w", filepath.Base(ex.tasksPath), i+1, err)
		}
		note(t.ID, t.Metadata)
	}
	return ex, nil
}

// recordingKey names the recording a collected task was measured against: its
// source, its session and its prompt. A task written by hand carries none of
// these and has no key, so it is never mistaken for a collected one.
func recordingKey(meta map[string]any) string {
	id, _ := meta["session_id"].(string)
	if id == "" {
		return ""
	}
	var prompt int
	switch v := meta["prompt_index"].(type) {
	case int:
		prompt = v
	case float64: // decoded from JSON
		prompt = int(v)
	default:
		return ""
	}
	source, _ := meta["source"].(string)
	return fmt.Sprintf("%s/%s#%d", orDefaultString(source, SourceName), id, prompt)
}

// renamed moves an item to a new id, along with every path derived from it.
func renamed(it Item, id string) Item {
	it.Task.ID = id
	it.BaselineFile = filepath.ToSlash(filepath.Join("baselines", id+".json"))
	if it.Task.Baseline != nil {
		ref := *it.Task.Baseline
		ref.File = it.BaselineFile
		it.Task.Baseline = &ref
	}
	it.Baseline.TaskID = id
	return it
}

// encodeTasks renders items as tasks.jsonl lines.
func encodeTasks(items []Item) (string, error) {
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
			return "", fmt.Errorf("task %s: %w", t.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.String(), nil
}

// writeSnapshots copies each item's SnapshotFrom into the dataset.
//
// The destination is Task.Workspace.Assets, which the loader resolves against
// the dataset directory, so a task whose assets are missing is a task that
// silently materializes as an empty workspace. Copying before the task lines
// that name them is the same ordering the baselines use: an interrupted write
// leaves an unreferenced directory rather than a task pointing at nothing.
//
// A directory that is already there is copied over rather than replaced. Two
// tasks from one recording share a destination, and the alternative -- removing
// the directory first -- would delete whatever else a reader had put beside the
// snapshot in order to keep a copy fresh that is about to hold the same files.
func writeSnapshots(dir string, items []Item) error {
	done := map[string]bool{}
	for _, it := range items {
		if it.SnapshotFrom == "" || it.Task.Workspace.Assets == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(it.Task.Workspace.Assets))
		if done[target] {
			continue
		}
		done[target] = true
		if err := snapshotTree(it.SnapshotFrom, target); err != nil {
			return fmt.Errorf("snapshot %s into %s: %w", it.SnapshotFrom, target, err)
		}
	}
	return nil
}

// writeBaselines writes one baseline document per item.
func writeBaselines(dir string, items []Item) error {
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
	return nil
}

// captureLabel names the source a collection came from, for the manifest header.
//
// The header used to say "from recorded Claude Code sessions" unconditionally,
// which became a lie the moment `collect` could read a second source. A dataset
// that misnames its own provenance is worse than one that says nothing: the
// reader has no reason to doubt it.
func captureLabel(items []Item) string {
	seen := map[string]bool{}
	for _, it := range items {
		seen[orDefaultString(it.Baseline.Source, SourceName)] = true
	}
	names := keys(seen)
	switch len(names) {
	case 0:
		return "agent"
	case 1:
		return sourceLabel(names[0])
	default:
		labels := make([]string, 0, len(names))
		for _, n := range names {
			labels = append(labels, sourceLabel(n))
		}
		return strings.Join(labels, " and ")
	}
}

// sourceLabel is the human name for a capture. The stored name is an
// identifier -- `cursor-composer` distinguishes the database-keyed capture from
// the file-based one -- and reads badly in a sentence.
func sourceLabel(source string) string {
	switch source {
	case CursorSourceName:
		return "Cursor"
	case SourceName:
		return "Claude Code"
	default:
		return source
	}
}

func orDefaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
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

// Package dataset defines the on-disk dataset format for agent evaluation and
// provides loaders/validators for it.
//
// A dataset is a directory containing:
//
//	dataset.yaml   -- manifest (name, version, defaults, task file reference)
//	tasks.jsonl    -- one TaskSpec per line (JSON), or `tasks:` inline in the manifest
//	workspaces/    -- optional per-task asset directories referenced by path
//
// The format is intentionally explicit: every field that influences a score
// (graders, weights, limits, output contract) lives in the spec so a run is
// fully reproducible from (dataset_hash, config_hash).
package dataset

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Workspace
// ---------------------------------------------------------------------------

// WorkspaceType describes how the task's initial workspace is materialized.
type WorkspaceType string

const (
	// WorkspaceEmpty starts from an empty directory.
	WorkspaceEmpty WorkspaceType = "empty"
	// WorkspaceFiles starts from inline files and/or a local asset directory.
	WorkspaceFiles WorkspaceType = "files"
	// WorkspaceGit clones (or copies) a repository at a specific ref.
	WorkspaceGit WorkspaceType = "git"
)

// Command is a single setup or verification command executed inside the sandbox.
type Command struct {
	// Run is the shell command line. Executed with the sandbox's default shell.
	Run string `json:"run" yaml:"run"`
	// Cwd is relative to the workspace root. Empty means the workspace root.
	Cwd string `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	// TimeoutS overrides the sandbox default command timeout.
	TimeoutS int `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`
	// Optional makes a non-zero exit status non-fatal for setup commands.
	Optional bool `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// EnvVar is one environment variable applied to workspace commands.
type EnvVar struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// Workspace describes how to build the initial state the agent will see.
type Workspace struct {
	Type WorkspaceType `json:"type" yaml:"type"`

	// Repo is a git URL or a local path (absolute, or relative to the dataset dir).
	Repo string `json:"repo,omitempty" yaml:"repo,omitempty"`
	// Ref is a commit/branch/tag. Empty means the remote default branch.
	Ref string `json:"ref,omitempty" yaml:"ref,omitempty"`
	// Subdir restricts the working directory to a subdirectory of the repo.
	Subdir string `json:"subdir,omitempty" yaml:"subdir,omitempty"`

	// Files are written into the workspace verbatim (path -> content).
	Files map[string]string `json:"files,omitempty" yaml:"files,omitempty"`
	// Assets is a directory copied into the workspace (relative to dataset dir).
	Assets string `json:"assets,omitempty" yaml:"assets,omitempty"`
	// AssetsTarget is the destination of Assets inside the workspace. Defaults to ".".
	AssetsTarget string `json:"assets_target,omitempty" yaml:"assets_target,omitempty"`

	// Setup runs after the base workspace exists and before the agent starts.
	Setup []Command `json:"setup,omitempty" yaml:"setup,omitempty"`
	// Env is applied to setup commands and the agent process.
	Env []EnvVar `json:"env,omitempty" yaml:"env,omitempty"`

	// KeepGitHistory keeps .git when the workspace is materialized from Files/Assets
	// into a fresh repo. Defaults to true for Type=git.
	KeepGitHistory *bool `json:"keep_git_history,omitempty" yaml:"keep_git_history,omitempty"`
}

// ---------------------------------------------------------------------------
// Output contract
// ---------------------------------------------------------------------------

// OutputType describes what the agent is expected to produce.
type OutputType string

const (
	// OutputPatch expects working-tree modifications in the workspace.
	OutputPatch OutputType = "patch"
	// OutputFiles expects a set of files to exist.
	OutputFiles OutputType = "files"
	// OutputStdout expects a textual answer (used by exact_match / llm_judge).
	OutputStdout OutputType = "stdout"
	// OutputNone declares no output contract (graders inspect raw state).
	OutputNone OutputType = "none"
)

// Output declares the deliverable contract, which decides what the graders see.
type Output struct {
	Type OutputType `json:"type" yaml:"type"`
	// Paths limits the diff/collection to these glob patterns. Empty means "all".
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	// IgnorePaths removes matching paths from the diff/collection.
	IgnorePaths []string `json:"ignore_paths,omitempty" yaml:"ignore_paths,omitempty"`
	// ExpectAnswer marks that stdout is a scored answer rather than free-form text.
	ExpectAnswer bool `json:"expect_answer,omitempty" yaml:"expect_answer,omitempty"`
}

// ---------------------------------------------------------------------------
// Graders
// ---------------------------------------------------------------------------

// GraderSpec is a declarative grader binding. Config carries grader-specific
// options and is decoded by the concrete grader implementation.
type GraderSpec struct {
	// Type selects a registered grader, e.g. "patch", "pytest", "exact_match",
	// "llm_judge", "command", or a user-registered name.
	Type string `json:"type" yaml:"type"`
	// Name labels the grader in reports. Defaults to Type, disambiguated by index.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Weight is the contribution inside the composite score.
	//
	// It is a pointer so that "unset" (defaults to 1) is distinguishable from an
	// explicit 0, which means "run and record, but do not contribute to the
	// score" -- useful for observability-only checks.
	Weight *float64 `json:"weight,omitempty" yaml:"weight,omitempty"`
	// Required forces a hard failure of the whole task when this grader fails,
	// regardless of the composite score.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
	// TimeoutS bounds the grader's own execution.
	TimeoutS int `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`
	// Config is grader-specific configuration.
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

// EffectiveWeight returns the grader's weight, defaulting to 1 when unset.
func (g GraderSpec) EffectiveWeight() float64 {
	if g.Weight == nil {
		return 1
	}
	return *g.Weight
}

// Float64 is a helper for building specs in Go with an explicit weight.
func Float64(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

// Limits bounds a single agent attempt. Zero values mean "inherit".
type Limits struct {
	MaxTurns   int     `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	TimeoutS   int     `json:"timeout_s,omitempty" yaml:"timeout_s,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty" yaml:"max_cost_usd,omitempty"`
	// MaxOutputTokens is advisory; passed through to the agent when supported.
	MaxOutputTokens int `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Reward contract
// ---------------------------------------------------------------------------

// RewardType selects how a rollout's scalar reward is derived from the grade.
//
// The choice is the density of the training signal, which is the first thing an
// RL consumer has to decide between:
//
//   - a sparse 0/1 signal, which is hard to game but slow to learn from;
//   - the dense weighted score, which pays out partial credit and therefore
//     provides a gradient on almost every attempt, at the cost of rewarding
//     shapes of progress the task author never intended to reward.
//
// Declaring it explicitly, rather than leaving it implied by grader_threshold,
// is what makes a reward auditable: "why did this rollout score 0.7?" has to be
// answerable from the dataset alone, not from the reader's guess about defaults.
type RewardType string

const (
	// RewardScore is the weighted grader score in [0,1]. The default.
	RewardScore RewardType = "score"
	// RewardPass is 1 when the task passes and 0 otherwise.
	RewardPass RewardType = "pass"
)

// RewardSpec declares a task's reward contract.
type RewardSpec struct {
	Type RewardType `json:"type" yaml:"type"`
	// Notes documents intent for humans and is never interpreted.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// EffectiveType resolves the reward type, defaulting to the dense score.
//
// The nil receiver is deliberate: a task without a reward block must behave
// exactly like one that spells out `type: score`, because that is the behaviour
// every dataset written before this field existed already has.
func (r *RewardSpec) EffectiveType() RewardType {
	if r == nil || r.Type == "" {
		return RewardScore
	}
	return r.Type
}

// ---------------------------------------------------------------------------
// Oracle
// ---------------------------------------------------------------------------

// Oracle is a reference solution: what a correct attempt looks like.
//
// It is never handed to the agent, and it does not participate in scoring an
// agent attempt. Its only job is to make the task's own health checkable --
// `aeval audit` applies it and requires the task to pass, which is what turns a
// resolve rate into an interpretable number. Without it, a task that nothing can
// solve is indistinguishable from an agent that solved nothing.
type Oracle struct {
	// Patch is a unified diff applied to the workspace after setup.
	Patch string `json:"patch,omitempty" yaml:"patch,omitempty"`
	// PatchFile loads the patch from a file, relative to the dataset directory.
	PatchFile string `json:"patch_file,omitempty" yaml:"patch_file,omitempty"`
	// Files are written verbatim after the patch is applied. They exist for the
	// cases a patch cannot express, such as adding a file to an empty directory.
	Files map[string]string `json:"files,omitempty" yaml:"files,omitempty"`
	// Apply runs additional commands after the patch and files, inside the
	// sandbox, with the workspace as the working directory.
	Apply []Command `json:"apply,omitempty" yaml:"apply,omitempty"`

	// Notes records provenance (upstream PR, commit, author) for humans.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// TaskSpec
// ---------------------------------------------------------------------------

// TaskSpec is a single evaluation instance. One TaskSpec maps to one sandbox and
// one agent attempt (repeated `samples` times when pass@k is requested).
type TaskSpec struct {
	// ID must be unique within a dataset and stable across versions.
	ID string `json:"id" yaml:"id"`
	// Version is an optional per-task revision marker.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	// Category is a coarse bucket used for grouped reporting (e.g. "bugfix").
	Category string `json:"category,omitempty" yaml:"category,omitempty"`
	// Difficulty is a free-form label (e.g. "easy" | "medium" | "hard").
	Difficulty string `json:"difficulty,omitempty" yaml:"difficulty,omitempty"`
	// Tags are free-form labels for filtering.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`

	// Prompt is the task text handed to the agent.
	Prompt string `json:"prompt" yaml:"prompt"`
	// PromptFile loads the prompt from a file (relative to dataset dir) when
	// Prompt is empty.
	PromptFile string `json:"prompt_file,omitempty" yaml:"prompt_file,omitempty"`
	// SystemPrompt is an optional per-task system prompt addition.
	SystemPrompt string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`

	Workspace Workspace    `json:"workspace" yaml:"workspace"`
	Output    *Output      `json:"output,omitempty" yaml:"output,omitempty"`
	Graders   []GraderSpec `json:"graders,omitempty" yaml:"graders,omitempty"`
	Limits    *Limits      `json:"limits,omitempty" yaml:"limits,omitempty"`

	// Reward declares how the scalar training signal is derived. Nil means the
	// dense weighted score.
	Reward *RewardSpec `json:"reward,omitempty" yaml:"reward,omitempty"`
	// Oracle is the reference solution, used only by `aeval audit`.
	Oracle *Oracle `json:"oracle,omitempty" yaml:"oracle,omitempty"`

	// RestorePaths are glob patterns reset to their baseline state before the
	// task is graded, so the attempt is judged on the code it left behind rather
	// than on the tests it rewrote. Set this to the test files (and conftest, and
	// any test configuration) whenever the agent could otherwise edit them.
	//
	// This is not anti-cheating for its own sake. Under RL a task with editable
	// tests has a shortest path to reward that is "make the assertion vacuous",
	// and a policy optimising against it will find that path: the guard removes
	// the shortcut before the policy can learn it.
	//
	// Restored paths are also removed from the graded diff, because a file that
	// has been reset to the baseline cannot have contributed anything to the
	// score.
	RestorePaths []string `json:"restore_paths,omitempty" yaml:"restore_paths,omitempty"`
	// ForbidChangedPaths fails the attempt when the agent modified any matching
	// path, without undoing the change. Use it where touching a file should be
	// reported as a violation rather than silently neutralised -- it is the
	// "did it obey the constraint?" question, whereas RestorePaths answers "can
	// it do the work?".
	ForbidChangedPaths []string `json:"forbid_changed_paths,omitempty" yaml:"forbid_changed_paths,omitempty"`

	// Metadata is free-form provenance: source benchmark, upstream instance id,
	// PR links, license, etc. Never interpreted by the engine.
	Metadata map[string]any `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Resolved relative path of the dataset that owns this task (set by loader).
	DatasetDir string `json:"-" yaml:"-"`
	// Resolved prompt text after PromptFile handling (set by loader).
	ResolvedPrompt string `json:"-" yaml:"-"`
}

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

// Manifest is dataset.yaml.
type Manifest struct {
	Name        string `json:"name" yaml:"name"`
	Version     string `json:"version,omitempty" yaml:"version,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	License     string `json:"license,omitempty" yaml:"license,omitempty"`
	// Maintainers / provenance, free-form.
	Homepage string `json:"homepage,omitempty" yaml:"homepage,omitempty"`

	// TasksFile is a JSONL file relative to the manifest directory.
	TasksFile string `json:"tasks_file,omitempty" yaml:"tasks_file,omitempty"`
	// Tasks allows small datasets to be declared inline.
	Tasks []TaskSpec `json:"tasks,omitempty" yaml:"tasks"`

	// Defaults are merged into every task (task values win).
	Defaults *TaskDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`

	// Filter narrows the dataset at load time.
	Filter *Filter `json:"filter,omitempty" yaml:"filter,omitempty"`
}

// TaskDefaults holds dataset-wide defaults merged into each TaskSpec.
type TaskDefaults struct {
	Output       *Output      `json:"output,omitempty" yaml:"output,omitempty"`
	Graders      []GraderSpec `json:"graders,omitempty" yaml:"graders,omitempty"`
	Limits       *Limits      `json:"limits,omitempty" yaml:"limits,omitempty"`
	Workspace    *Workspace   `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	SystemPrompt string       `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	// Reward is usually a dataset-wide decision -- one density for the whole
	// training set -- so it is inheritable rather than per-task only.
	Reward *RewardSpec `json:"reward,omitempty" yaml:"reward,omitempty"`
}

// Filter narrows which tasks a dataset exposes.
type Filter struct {
	IDs        []string `json:"ids,omitempty" yaml:"ids,omitempty"`
	Categories []string `json:"categories,omitempty" yaml:"categories,omitempty"`
	Tags       []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Difficulty []string `json:"difficulty,omitempty" yaml:"difficulty,omitempty"`
	// Limit caps the number of tasks after filtering (0 = no cap, applied in file order).
	Limit int `json:"limit,omitempty" yaml:"limit,omitempty"`
}

// ---------------------------------------------------------------------------
// Loaded dataset
// ---------------------------------------------------------------------------

// Dataset is a fully resolved, validated collection of tasks.
type Dataset struct {
	Manifest Manifest   `json:"manifest"`
	Tasks    []TaskSpec `json:"tasks"`
	// Dir is the absolute directory of the manifest.
	Dir string `json:"dir"`
	// Hash identifies the exact task content (see Hash()).
	Hash string `json:"hash"`
}

// TaskIndex returns task id -> index.
func (d *Dataset) TaskIndex() map[string]int {
	m := make(map[string]int, len(d.Tasks))
	for i, t := range d.Tasks {
		m[t.ID] = i
	}
	return m
}

// Categories lists distinct categories in stable order.
func (d *Dataset) Categories() []string {
	set := map[string]struct{}{}
	for _, t := range d.Tasks {
		if t.Category != "" {
			set[t.Category] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// TagSet lists distinct tags in stable order.
func (d *Dataset) TagSet() []string {
	set := map[string]struct{}{}
	for _, t := range d.Tasks {
		for _, tag := range t.Tags {
			set[tag] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// Summary renders a short human-readable description.
func (d *Dataset) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dataset %s@%s (%d tasks", d.Manifest.Name, d.Manifest.Version, len(d.Tasks))
	if cats := d.Categories(); len(cats) > 0 {
		fmt.Fprintf(&b, ", categories: %s", strings.Join(cats, "/"))
	}
	b.WriteString(")")
	return b.String()
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

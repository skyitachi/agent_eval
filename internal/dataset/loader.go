package dataset

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxJSONLLine bounds a single JSONL record (prompts can be large).
const maxJSONLLine = 32 << 20 // 32 MiB

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Load reads a dataset from a directory or from a manifest file path.
func Load(path string) (*Dataset, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("dataset path %s: %w", path, err)
	}
	dir, manifestPath := abs, filepath.Join(abs, "dataset.yaml")
	if !info.IsDir() {
		dir, manifestPath = filepath.Dir(abs), abs
	}
	if _, err := os.Stat(manifestPath); err != nil {
		// Accept dataset.yml as an alias.
		alt := filepath.Join(dir, "dataset.yml")
		if _, err2 := os.Stat(alt); err2 == nil {
			manifestPath = alt
		} else {
			return nil, fmt.Errorf("no dataset.yaml in %s", dir)
		}
	}
	return LoadManifest(manifestPath)
}

// LoadManifest reads and fully resolves a dataset from a manifest file.
func LoadManifest(manifestPath string) (*Dataset, error) {
	dir := filepath.Dir(manifestPath)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestPath, err)
	}

	tasks := append([]TaskSpec(nil), m.Tasks...)
	if m.TasksFile != "" {
		fileTasks, err := loadJSONL(filepath.Join(dir, m.TasksFile))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, fileTasks...)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("%s: dataset declares no tasks (set tasks_file or tasks)", manifestPath)
	}

	for i := range tasks {
		tasks[i].DatasetDir = dir
		if err := resolvePrompt(&tasks[i]); err != nil {
			return nil, err
		}
		if err := resolveOracle(&tasks[i]); err != nil {
			return nil, err
		}
		applyDefaults(&tasks[i], m.Defaults)
		normalize(&tasks[i])
	}

	tasks = applyFilter(tasks, m.Filter)

	d := &Dataset{Manifest: m, Tasks: tasks, Dir: dir}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	h, err := d.computeHash()
	if err != nil {
		return nil, err
	}
	d.Hash = h
	return d, nil
}

func loadJSONL(path string) ([]TaskSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tasks file: %w", err)
	}
	defer f.Close()

	var out []TaskSpec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxJSONLLine)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "//") || strings.HasPrefix(text, "#") {
			continue
		}
		var t TaskSpec
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&t); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", filepath.Base(path), line, err)
		}
		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func resolvePrompt(t *TaskSpec) error {
	if t.Prompt != "" || t.PromptFile == "" {
		t.ResolvedPrompt = t.Prompt
		return nil
	}
	p := t.PromptFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.DatasetDir, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("task %s: prompt_file: %w", t.ID, err)
	}
	t.ResolvedPrompt = strings.TrimRight(string(b), "\n")
	return nil
}

// resolveOracle folds oracle.patch_file into oracle.patch.
//
// Doing it here rather than at audit time means the patch is part of the
// resolved task, and therefore part of dataset_hash: a run can be traced back to
// the exact reference solution it was validated against. An inline patch wins,
// matching how prompt_file behaves.
func resolveOracle(t *TaskSpec) error {
	o := t.Oracle
	if o == nil || o.PatchFile == "" || o.Patch != "" {
		return nil
	}
	p := o.PatchFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.DatasetDir, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("task %s: oracle.patch_file: %w", t.ID, err)
	}
	o.Patch = strings.TrimRight(string(b), "\n")
	return nil
}

func applyDefaults(t *TaskSpec, d *TaskDefaults) {
	if d == nil {
		return
	}
	if t.Output == nil && d.Output != nil {
		cp := *d.Output
		t.Output = &cp
	}
	if len(t.Graders) == 0 && len(d.Graders) > 0 {
		t.Graders = append([]GraderSpec(nil), d.Graders...)
	}
	if d.Limits != nil {
		if t.Limits == nil {
			cp := *d.Limits
			t.Limits = &cp
		} else {
			mergeLimits(t.Limits, d.Limits)
		}
	}
	if d.Workspace != nil {
		mergeWorkspace(&t.Workspace, d.Workspace)
	}
	if t.SystemPrompt == "" {
		t.SystemPrompt = d.SystemPrompt
	}
	if t.Reward == nil && d.Reward != nil {
		cp := *d.Reward
		t.Reward = &cp
	}
}

func mergeLimits(dst, src *Limits) {
	if dst.MaxTurns == 0 {
		dst.MaxTurns = src.MaxTurns
	}
	if dst.TimeoutS == 0 {
		dst.TimeoutS = src.TimeoutS
	}
	if dst.MaxCostUSD == 0 {
		dst.MaxCostUSD = src.MaxCostUSD
	}
	if dst.MaxOutputTokens == 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
}

// mergeWorkspace fills empty task-level workspace fields from defaults, but only
// when the task did not pin a fully-formed workspace of its own.
func mergeWorkspace(dst, src *Workspace) {
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if dst.Repo == "" {
		dst.Repo = src.Repo
	}
	if dst.Ref == "" {
		dst.Ref = src.Ref
	}
	if dst.Subdir == "" {
		dst.Subdir = src.Subdir
	}
	if dst.Assets == "" {
		dst.Assets = src.Assets
	}
	if dst.AssetsTarget == "" {
		dst.AssetsTarget = src.AssetsTarget
	}
	if dst.Files == nil {
		dst.Files = src.Files
	}
	if len(dst.Setup) == 0 {
		dst.Setup = src.Setup
	}
	if len(dst.Env) == 0 {
		dst.Env = src.Env
	}
	if dst.KeepGitHistory == nil {
		dst.KeepGitHistory = src.KeepGitHistory
	}
}

func normalize(t *TaskSpec) {
	if t.Workspace.Type == "" {
		t.Workspace.Type = WorkspaceEmpty
	}
	if t.Output == nil {
		t.Output = &Output{Type: OutputPatch}
	}
	if t.Output.Type == "" {
		t.Output.Type = OutputPatch
	}
	if t.Limits == nil {
		t.Limits = &Limits{}
	}
	for i := range t.Graders {
		if t.Graders[i].Name == "" {
			if len(t.Graders) == 1 {
				t.Graders[i].Name = t.Graders[i].Type
			} else {
				t.Graders[i].Name = fmt.Sprintf("%s#%d", t.Graders[i].Type, i+1)
			}
		}
	}
	sort.Strings(t.Tags)
}

func applyFilter(tasks []TaskSpec, f *Filter) []TaskSpec {
	if f == nil {
		return tasks
	}
	ids := toSet(f.IDs)
	cats := toSet(f.Categories)
	tags := toSet(f.Tags)
	diffs := toSet(f.Difficulty)

	out := make([]TaskSpec, 0, len(tasks))
	for _, t := range tasks {
		if len(ids) > 0 {
			if _, ok := ids[t.ID]; !ok {
				continue
			}
		}
		if len(cats) > 0 {
			if _, ok := cats[t.Category]; !ok {
				continue
			}
		}
		if len(diffs) > 0 {
			if _, ok := diffs[t.Difficulty]; !ok {
				continue
			}
		}
		if len(tags) > 0 && !hasAny(t.Tags, tags) {
			continue
		}
		out = append(out, t)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

func hasAny(have []string, want map[string]struct{}) bool {
	for _, h := range have {
		if _, ok := want[h]; ok {
			return true
		}
	}
	return false
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

// computeHash digests the resolved task content. Runs are reproducible from
// (dataset_hash, config_hash) as long as this value is unchanged.
func (d *Dataset) computeHash() (string, error) {
	type hashTask struct {
		ID         string         `json:"id"`
		Version    string         `json:"version,omitempty"`
		Category   string         `json:"category,omitempty"`
		Difficulty string         `json:"difficulty,omitempty"`
		Tags       []string       `json:"tags,omitempty"`
		Prompt     string         `json:"prompt"`
		System     string         `json:"system_prompt,omitempty"`
		Workspace  Workspace      `json:"workspace"`
		Output     *Output        `json:"output,omitempty"`
		Graders    []GraderSpec   `json:"graders,omitempty"`
		Limits     *Limits        `json:"limits,omitempty"`
		Reward     *RewardSpec    `json:"reward,omitempty"`
		Oracle     *Oracle        `json:"oracle,omitempty"`
		Restore    []string       `json:"restore_paths,omitempty"`
		Forbid     []string       `json:"forbid_changed_paths,omitempty"`
		Metadata   map[string]any `json:"metadata,omitempty"`
	}
	wrapped := struct {
		Name    string     `json:"name"`
		Version string     `json:"version,omitempty"`
		Tasks   []hashTask `json:"tasks"`
	}{Name: d.Manifest.Name, Version: d.Manifest.Version}

	for _, t := range d.Tasks {
		h := hashTask{
			ID: t.ID, Version: t.Version, Category: t.Category, Difficulty: t.Difficulty,
			Tags: t.Tags, Prompt: t.ResolvedPrompt, System: t.SystemPrompt,
			Workspace: t.Workspace, Output: t.Output, Graders: t.Graders,
			Limits: t.Limits, Reward: t.Reward, Oracle: t.Oracle,
			Restore: t.RestorePaths, Forbid: t.ForbidChangedPaths,
			Metadata: t.Metadata,
		}
		wrapped.Tasks = append(wrapped.Tasks, h)
	}
	b, err := json.Marshal(wrapped)
	if err != nil {
		return "", fmt.Errorf("hash dataset: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16], nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// ValidationIssue is a single problem found while validating a dataset.
type ValidationIssue struct {
	TaskID  string
	Field   string
	Message string
}

func (v ValidationIssue) String() string {
	if v.TaskID == "" {
		if v.Field == "" {
			return v.Message
		}
		return fmt.Sprintf("%s: %s", v.Field, v.Message)
	}
	if v.Field == "" {
		return fmt.Sprintf("task %s: %s", v.TaskID, v.Message)
	}
	return fmt.Sprintf("task %s.%s: %s", v.TaskID, v.Field, v.Message)
}

// ValidationError aggregates all validation issues.
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dataset validation failed with %d issue(s):", len(e.Issues))
	for _, i := range e.Issues {
		b.WriteString("\n  - ")
		b.WriteString(i.String())
	}
	return b.String()
}

var validWorkspaceTypes = map[WorkspaceType]struct{}{
	WorkspaceEmpty: {}, WorkspaceFiles: {}, WorkspaceGit: {},
}

var validOutputTypes = map[OutputType]struct{}{
	OutputPatch: {}, OutputFiles: {}, OutputStdout: {}, OutputNone: {},
}

// validRewardTypes mirrors RewardType. The empty value is accepted and means the
// dense score, so a dataset that never mentions reward stays valid.
var validRewardTypes = map[RewardType]struct{}{
	"": {}, RewardScore: {}, RewardPass: {},
}

// validateGuards checks the two anti-gaming path lists.
//
// The overlap check only compares patterns literally. Deciding whether two
// globs intersect in general is a different problem, and pretending to solve it
// would produce false confidence; the literal case catches the mistake people
// actually make, which is listing the same path in both.
func validateGuards(t TaskSpec, add func(id, field, format string, args ...any), id string) {
	for i, p := range t.RestorePaths {
		if strings.TrimSpace(p) == "" {
			add(id, fmt.Sprintf("restore_paths[%d]", i), "pattern is empty")
		}
	}
	for i, p := range t.ForbidChangedPaths {
		if strings.TrimSpace(p) == "" {
			add(id, fmt.Sprintf("forbid_changed_paths[%d]", i), "pattern is empty")
		}
	}
	forbidden := toSet(t.ForbidChangedPaths)
	for i, p := range t.RestorePaths {
		if _, dup := forbidden[p]; dup {
			add(id, fmt.Sprintf("restore_paths[%d]", i),
				"%q is also in forbid_changed_paths; a path can either be reset "+
					"before grading or treated as a violation, not both", p)
		}
	}
}

// Validate checks the resolved dataset and returns every problem it finds.
func (d *Dataset) Validate() error {
	var issues []ValidationIssue
	add := func(id, field, format string, args ...any) {
		issues = append(issues, ValidationIssue{TaskID: id, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if strings.TrimSpace(d.Manifest.Name) == "" {
		add("", "name", "manifest name is required")
	}

	seen := map[string]int{}
	for _, t := range d.Tasks {
		id := t.ID
		switch {
		case strings.TrimSpace(id) == "":
			add(id, "id", "task id is required")
			id = "<empty>"
		case !idPattern.MatchString(t.ID):
			add(id, "id", "must match %s", idPattern.String())
		}
		if prev, dup := seen[id]; dup {
			add(id, "id", "duplicate task id (first seen at index %d)", prev)
		}
		seen[id] = len(seen)

		if strings.TrimSpace(t.ResolvedPrompt) == "" {
			add(id, "prompt", "prompt is empty (set prompt or prompt_file)")
		}
		if _, ok := validWorkspaceTypes[t.Workspace.Type]; !ok {
			add(id, "workspace.type", "unknown type %q (want empty|files|git)", t.Workspace.Type)
		}
		if t.Workspace.Type == WorkspaceGit && t.Workspace.Repo == "" {
			add(id, "workspace.repo", "required when workspace.type=git")
		}
		if t.Workspace.Type == WorkspaceFiles && len(t.Workspace.Files) == 0 && t.Workspace.Assets == "" {
			add(id, "workspace", "type=files requires files or assets")
		}
		if _, ok := validOutputTypes[t.Output.Type]; !ok {
			add(id, "output.type", "unknown type %q (want patch|files|stdout|none)", t.Output.Type)
		}
		if len(t.Graders) == 0 {
			add(id, "graders", "at least one grader is required")
		}
		totalWeight := 0.0
		for i, g := range t.Graders {
			if strings.TrimSpace(g.Type) == "" {
				add(id, fmt.Sprintf("graders[%d].type", i), "grader type is required")
			}
			if g.Weight != nil && *g.Weight < 0 {
				add(id, fmt.Sprintf("graders[%d].weight", i), "must be >= 0, got %v", *g.Weight)
			}
			totalWeight += g.EffectiveWeight()
		}
		if len(t.Graders) > 0 && totalWeight <= 0 {
			add(id, "graders", "at least one grader must have weight > 0, otherwise no task can ever pass")
		}
		if t.Limits.TimeoutS < 0 {
			add(id, "limits.timeout_s", "must be >= 0")
		}
		if _, ok := validRewardTypes[t.Reward.EffectiveType()]; !ok {
			add(id, "reward.type", "unknown type %q (want %s|%s)",
				t.Reward.EffectiveType(), RewardScore, RewardPass)
		}
		if o := t.Oracle; o != nil {
			// An oracle that applies nothing is worse than none at all: audit
			// would report the task as solvable on the strength of the
			// untouched baseline, which is exactly the check it is meant to be.
			if strings.TrimSpace(o.Patch) == "" && len(o.Files) == 0 && len(o.Apply) == 0 {
				add(id, "oracle", "declared but empty (set patch, patch_file, files or apply)")
			}
			for i, c := range o.Apply {
				if strings.TrimSpace(c.Run) == "" {
					add(id, fmt.Sprintf("oracle.apply[%d].run", i), "command is empty")
				}
			}
		}
		validateGuards(t, add, id)
		for i, c := range t.Workspace.Setup {
			if strings.TrimSpace(c.Run) == "" {
				add(id, fmt.Sprintf("workspace.setup[%d].run", i), "command is empty")
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// ValidatePath loads a dataset and reports validation issues as a
// *ValidationError when they are the cause of the failure.
func ValidatePath(path string) (*Dataset, error) {
	return Load(path)
}

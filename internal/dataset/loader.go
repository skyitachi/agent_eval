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
		if err := resolveBaseline(&tasks[i]); err != nil {
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

// resolveBaseline loads the recorded reference attempt a task is compared
// against.
//
// It is resolved here, rather than lazily at run time, for the same reason the
// prompt is: the comparison a run makes is part of what the run means, so it
// belongs in the resolved task and therefore in dataset_hash. Two runs that
// report the same hash were measured against the same reference.
func resolveBaseline(t *TaskSpec) error {
	b := t.Baseline
	if b == nil || b.File == "" {
		return nil
	}
	p := b.File
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.DatasetDir, p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("task %s: baseline.file: %w", t.ID, err)
	}
	var sb Baseline
	if err := json.Unmarshal(raw, &sb); err != nil {
		return fmt.Errorf("task %s: baseline.file: %s: %w", t.ID, filepath.Base(p), err)
	}
	if sb.TaskID == "" {
		sb.TaskID = t.ID
	}
	t.ResolvedBaseline = &sb
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
		Baseline   *Baseline      `json:"baseline,omitempty"`
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
			Limits: t.Limits, Baseline: t.ResolvedBaseline,
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
		// A task with no grader is legal only when it carries a baseline.
		//
		// Two different things can be missing here. A prompt with neither a
		// grader nor a baseline is just an unmeasured prompt: nothing can ever
		// pass or fail it and no comparison exists, so it contributes nothing
		// but runtime. A prompt with a baseline is a real measurement -- the
		// attempt's cost and time are compared against a recorded one -- and it
		// is exactly what `aeval collect` produces, because a transcript has no
		// ground truth to build a grader from.
		if len(t.Graders) == 0 && t.Baseline == nil {
			add(id, "graders", "at least one grader is required (or declare a baseline for a cost-only task)")
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
		if t.Baseline != nil && strings.TrimSpace(t.Baseline.File) == "" {
			// A baseline block that points at nothing is worse than none: the
			// report would render a comparison against a reference that was
			// never recorded.
			add(id, "baseline.file", "declared but empty")
		}
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

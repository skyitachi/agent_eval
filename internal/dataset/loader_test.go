package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const demoDir = "../../datasets/demo"

func TestLoadDemoDataset(t *testing.T) {
	d, err := Load(demoDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(d.Tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(d.Tasks))
	}
	if d.Manifest.Name != "demo-basic" {
		t.Errorf("manifest name = %q", d.Manifest.Name)
	}
	if !strings.HasPrefix(d.Hash, "sha256:") {
		t.Errorf("hash = %q, want sha256: prefix", d.Hash)
	}

	byID := map[string]TaskSpec{}
	for _, task := range d.Tasks {
		byID[task.ID] = task
	}

	// prompt_file must be resolved into ResolvedPrompt.
	task, ok := byID["fix-average-off-by-one"]
	if !ok {
		t.Fatal("missing fix-average-off-by-one")
	}
	if !strings.Contains(task.ResolvedPrompt, "arithmetic mean") {
		t.Errorf("prompt_file not resolved, got %q", truncate(task.ResolvedPrompt, 80))
	}
	// Task-level limits override dataset defaults.
	if task.Limits.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want 20 (task override)", task.Limits.MaxTurns)
	}
	// Task-level graders replace defaults entirely.
	if len(task.Graders) != 1 || task.Graders[0].Type != "pytest" {
		t.Errorf("graders = %+v, want single pytest grader", task.Graders)
	}
	if task.Graders[0].Name != "visible-suite" {
		t.Errorf("grader name = %q", task.Graders[0].Name)
	}

	// A task without its own output contract inherits the dataset default.
	answer := byID["read-and-answer"]
	if answer.Output.Type != OutputStdout {
		t.Errorf("output.type = %q, want stdout", answer.Output.Type)
	}
	if !answer.Output.ExpectAnswer {
		t.Error("expect_answer should be true")
	}
}

func TestHashIsStableAndContentSensitive(t *testing.T) {
	a, err := Load(demoDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(demoDir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash not stable: %s vs %s", a.Hash, b.Hash)
	}

	// Reordering tasks must change nothing but editing content must.
	dir := t.TempDir()
	writeManifest(t, dir, `
name: t
version: 1.0.0
tasks:
  - id: a
    prompt: hello
    workspace: {type: empty}
    output: {type: patch}
    graders: [{type: patch}]
`)
	orig, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	writeManifest(t, dir, `
name: t
version: 1.0.0
tasks:
  - id: a
    prompt: hello there
    workspace: {type: empty}
    output: {type: patch}
    graders: [{type: patch}]
`)
	edited, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if orig.Hash == edited.Hash {
		t.Error("hash should change when prompt content changes")
	}
}

// A task may carry the line it is graded against, and the hash has to cover it:
// two datasets that differ only in a cutoff reach different verdicts, so they
// are not the same dataset.
func TestATaskCanDeclareTheLineItIsGradedAgainst(t *testing.T) {
	dir := t.TempDir()
	write := func(threshold string) *Dataset {
		t.Helper()
		writeManifest(t, dir, `
name: t
version: 1.0.0
tasks:
  - id: a
    prompt: hello
    workspace: {type: empty}
    output: {type: patch}
    graders: [{type: llm_judge}]
`+threshold)
		d, err := Load(dir)
		if err != nil {
			t.Fatalf("load with %q: %v", threshold, err)
		}
		return d
	}

	unset := write("")
	if got := unset.Tasks[0].Threshold; got != 0 {
		t.Errorf("threshold = %v on a task that declared none", got)
	}
	// Zero is "the run decides", so the fallback has to survive untouched.
	if got := unset.Tasks[0].EffectiveThreshold(0.6); got != 0.6 {
		t.Errorf("EffectiveThreshold(0.6) = %v for a task that declared none", got)
	}

	declared := write("    threshold: 0.85\n")
	if got := declared.Tasks[0].Threshold; got != 0.85 {
		t.Errorf("threshold = %v, want 0.85", got)
	}
	if got := declared.Tasks[0].EffectiveThreshold(0.6); got != 0.85 {
		t.Errorf("EffectiveThreshold(0.6) = %v, want the declared 0.85", got)
	}
	if declared.Hash == unset.Hash {
		t.Error("the hash ignores the threshold, so two differently graded datasets look identical")
	}

	for _, bad := range []string{"1.5", "-0.2"} {
		writeManifest(t, dir, `
name: t
version: 1.0.0
tasks:
  - id: a
    prompt: hello
    workspace: {type: empty}
    output: {type: patch}
    graders: [{type: patch}]
    threshold: `+bad+"\n")
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "threshold") {
			t.Errorf("threshold %s was accepted or reported without naming the field: %v", bad, err)
		}
	}
}

func TestDefaultsAreInheritedWhenTaskIsSilent(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
name: t
defaults:
  limits: {max_turns: 7, timeout_s: 99}
  output: {type: stdout}
  graders: [{type: exact_match, config: {expected: "x"}}]
tasks:
  - id: inherits
    prompt: p
    workspace: {type: empty}
  - id: overrides
    prompt: p
    workspace: {type: empty}
    limits: {max_turns: 3}
    graders: [{type: patch}]
`)
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskSpec{}
	for _, task := range d.Tasks {
		byID[task.ID] = task
	}
	inh := byID["inherits"]
	if inh.Limits.MaxTurns != 7 || inh.Limits.TimeoutS != 99 {
		t.Errorf("default limits not inherited: %+v", inh.Limits)
	}
	if inh.Output.Type != OutputStdout {
		t.Errorf("default output not inherited: %+v", inh.Output)
	}
	if len(inh.Graders) != 1 || inh.Graders[0].Type != "exact_match" {
		t.Errorf("default graders not inherited: %+v", inh.Graders)
	}

	// Partial override: unspecified limit fields still inherit.
	ovr := byID["overrides"]
	if ovr.Limits.MaxTurns != 3 {
		t.Errorf("MaxTurns = %d, want 3", ovr.Limits.MaxTurns)
	}
	if ovr.Limits.TimeoutS != 99 {
		t.Errorf("TimeoutS = %d, want 99 (inherited)", ovr.Limits.TimeoutS)
	}
}

func TestValidationReportsEveryIssue(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
name: ""
tasks_file: tasks.jsonl
`)
	writeFile(t, filepath.Join(dir, "tasks.jsonl"), `{"id":"dup","prompt":"a","workspace":{"type":"empty"},"graders":[{"type":"patch"}]}
{"id":"dup","prompt":"","workspace":{"type":"nope"},"graders":[]}
{"id":"bad id!","prompt":"c","workspace":{"type":"git"},"graders":[{"type":"patch","weight":0}]}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected validation error")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	got := err.Error()
	for _, want := range []string{
		"name is required",
		"duplicate task id",
		"prompt is empty",
		"unknown type \"nope\"",
		"workspace.repo",
		"must match",
		"at least one grader must have weight > 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("validation output missing %q\n---\n%s", want, got)
		}
	}
	if len(ve.Issues) < 6 {
		t.Errorf("want >= 6 issues, got %d", len(ve.Issues))
	}
}

func TestUnknownTaskFieldIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
name: t
tasks_file: tasks.jsonl
`)
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],"typo_field":1}`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for unknown field (typos must not silently pass)")
	}
	if !strings.Contains(err.Error(), "typo_field") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestFilterNarrowsDataset(t *testing.T) {
	d, err := Load(demoDir)
	if err != nil {
		t.Fatal(err)
	}
	filtered := applyFilter(d.Tasks, &Filter{Tags: []string{"strings"}})
	if len(filtered) != 1 || filtered[0].ID != "implement-slugify" {
		t.Fatalf("tag filter = %+v", filtered)
	}
	filtered = applyFilter(d.Tasks, &Filter{Categories: []string{"bugfix", "reasoning"}})
	if len(filtered) != 2 {
		t.Fatalf("category filter returned %d tasks", len(filtered))
	}
	filtered = applyFilter(d.Tasks, &Filter{Limit: 2})
	if len(filtered) != 2 {
		t.Fatalf("limit filter returned %d tasks", len(filtered))
	}
}

// ---------------------------------------------------------------------------

func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "dataset.yaml"), body)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

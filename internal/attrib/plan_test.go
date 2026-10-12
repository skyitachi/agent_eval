package attrib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a matrix document on disk and returns its path. The document is
// written beside a dataset so that relative paths have something to resolve
// against.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dataset.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "matrix.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const modelSweep = `
name: model-sweep
dataset: dataset.yaml
axis: model
agent:
  kind: claude_code
  isolation:
    bare: true
levels:
  - name: sonnet
    agent: {model: claude-sonnet-4-5}
  - name: haiku
    notes: the cheap one
    agent: {model: claude-haiku-4-5}
`

func TestLoadModelSweep(t *testing.T) {
	p, err := Load(write(t, modelSweep))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Axis != AxisModel {
		t.Errorf("axis = %q, want %q", p.Axis, AxisModel)
	}
	if p.Control != "sonnet" {
		t.Errorf("control = %q, want the first level by default", p.Control)
	}
	if got := p.LevelNames(); strings.Join(got, ",") != "sonnet,haiku" {
		t.Errorf("levels = %v", got)
	}
	if !filepath.IsAbs(p.DatasetPath) {
		t.Errorf("dataset path %q is not absolute", p.DatasetPath)
	}
	// The dataset path must resolve against the document, not the process's
	// working directory, or the same matrix file would mean different things
	// depending on where it was invoked from.
	if filepath.Dir(p.DatasetPath) != p.Dir {
		t.Errorf("dataset path %q does not sit beside the document %q", p.DatasetPath, p.Dir)
	}
	for _, l := range p.Levels {
		if strings.Join(l.Changed, ",") != "model" {
			t.Errorf("level %q changed %v, want [model]", l.Name, l.Changed)
		}
	}
	if p.Levels[1].Notes != "the cheap one" {
		t.Errorf("level notes = %q", p.Levels[1].Notes)
	}
}

func TestLoadRejectsCrossAxisChange(t *testing.T) {
	_, err := Load(write(t, `
dataset: dataset.yaml
axis: model
levels:
  - {name: a, agent: {model: sonnet}}
  - {name: b, agent: {model: haiku, append_system_prompt: "be terse"}}
`))
	if err == nil {
		t.Fatal("a model sweep that also changes the system prompt was accepted")
	}
	if !strings.Contains(err.Error(), "append_system_prompt") || !strings.Contains(err.Error(), "does not own") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestHarnessAxisOwnsImplementation(t *testing.T) {
	p, err := Load(write(t, `
dataset: dataset.yaml
axis: harness
agent:
  model: sonnet
levels:
  - name: plain
    agent: {isolation: {bare: true, strict_mcp_config: true}}
  - name: with-hooks
    agent:
      bin: /opt/claude
      isolation: {bare: false, strict_mcp_config: true}
      append_system_prompt: "work carefully"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := strings.Join(p.Levels[1].Changed, ",")
	if got != "append_system_prompt,bin,isolation" {
		t.Errorf("changed = %q", got)
	}

	// bin and isolation are the implementation, so harness owns them; the model
	// belongs to another axis even though this sweep happens to declare one.
	if _, err := Load(write(t, `
dataset: dataset.yaml
axis: harness
levels:
  - {name: a, agent: {model: sonnet}}
  - {name: b, agent: {model: haiku}}
`)); err == nil {
		t.Fatal("a harness sweep that changes the model was accepted")
	}
}

func TestSkillSweepRequiresSlashCommands(t *testing.T) {
	blocked := `
dataset: dataset.yaml
axis: skill
agent:
  isolation: {bare: true, disable_slash_commands: true}
levels:
  - {name: none}
  - {name: with-skill, agent: {plugin_dirs: [/tmp/skill]}}
`
	_, err := Load(write(t, blocked))
	if err == nil {
		t.Fatal("a skill level that cannot load its skill was accepted")
	}
	if !strings.Contains(err.Error(), "disable_slash_commands") {
		t.Fatalf("the error does not name the flag that causes the problem: %v", err)
	}

	ok := strings.Replace(blocked, "disable_slash_commands: true", "disable_slash_commands: false", 1)
	p, err := Load(write(t, ok))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(p.Levels[1].Changed, ","); got != "plugin_dirs" {
		t.Errorf("changed = %q", got)
	}
	// The control is the reference, so it is the one level that is supposed to
	// differ on nothing.
	if len(p.Levels[0].Changed) != 0 {
		t.Errorf("the control changed %v", p.Levels[0].Changed)
	}
}

func TestLoadRejectsUnknownLevelKey(t *testing.T) {
	// A typo has to be an error: a silently ignored plugin_dir would produce a
	// skill sweep in which the skill never loaded, and the sweep would report
	// that skills make no difference.
	_, err := Load(write(t, `
dataset: dataset.yaml
axis: skill
agent:
  isolation: {disable_slash_commands: false}
levels:
  - {name: none}
  - {name: typo, agent: {plugin_dir: [/tmp/skill]}}
`))
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "plugin_dir") {
		t.Fatalf("the error does not name the offending key: %v", err)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown axis", `
dataset: dataset.yaml
axis: temperature
levels: [{name: a}, {name: b}]
`, "unknown axis"},
		{"one level", `
dataset: dataset.yaml
axis: model
levels: [{name: a, agent: {model: sonnet}}]
`, "at least two levels"},
		{"duplicate names", `
dataset: dataset.yaml
axis: model
levels:
  - {name: a, agent: {model: sonnet}}
  - {name: a, agent: {model: haiku}}
`, "duplicate level"},
		{"control not a level", `
dataset: dataset.yaml
axis: model
control: nope
levels:
  - {name: a, agent: {model: sonnet}}
  - {name: b, agent: {model: haiku}}
`, "not one of the levels"},
		{"no dataset", `
axis: model
levels: [{name: a}, {name: b}]
`, "does not name a dataset"},
		{"level name with a slash", `
dataset: dataset.yaml
axis: model
levels:
  - {name: "a/b", agent: {model: sonnet}}
  - {name: b, agent: {model: haiku}}
`, "path separators"},
		{"model level with no model", `
dataset: dataset.yaml
axis: model
levels: [{name: a}, {name: b}]
`, "leaves the model unset"},
		{"unknown top-level key", `
dataset: dataset.yaml
axis: model
samples_: 3
levels: [{name: a, agent: {model: sonnet}}, {name: b, agent: {model: haiku}}]
`, "samples_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatalf("accepted an invalid matrix")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestStepsPutsControlFirst(t *testing.T) {
	p, err := Load(write(t, `
dataset: dataset.yaml
axis: model
control: haiku
levels:
  - {name: sonnet, agent: {model: sonnet}}
  - {name: haiku, agent: {model: haiku}}
  - {name: opus, agent: {model: opus}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	steps := p.Steps()
	if len(steps) != 3 {
		t.Fatalf("got %d steps", len(steps))
	}
	// The control goes first so that a sweep interrupted after one level still
	// has the reference every comparison needs.
	if !steps[0].IsControl || steps[0].Level.Name != "haiku" {
		t.Fatalf("first step = %+v, want the control", steps[0])
	}
	for i, s := range steps[1:] {
		if s.IsControl {
			t.Errorf("step %d is marked control", i+1)
		}
	}
	if steps[1].Level.Name != "sonnet" || steps[2].Level.Name != "opus" {
		t.Errorf("remaining steps out of order: %s, %s", steps[1].Level.Name, steps[2].Level.Name)
	}
}

func TestPlanWarnsAboutSampleSize(t *testing.T) {
	p, err := Load(write(t, modelSweep))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("a one-sample-per-level sweep produced no warning")
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "samples") {
		t.Errorf("warnings do not mention samples: %v", p.Warnings)
	}

	withSamples, err := Load(write(t, strings.Replace(modelSweep, "axis: model", "axis: model\nsamples: 3", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(withSamples.Warnings) != 0 {
		t.Errorf("samples=3 still warns: %v", withSamples.Warnings)
	}
}

func TestPlanWarnsWhenALevelChangesNothing(t *testing.T) {
	p, err := Load(write(t, `
dataset: dataset.yaml
axis: model
samples: 2
agent: {model: sonnet}
levels:
  - {name: a}
  - {name: b}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "change nothing") {
		t.Errorf("a sweep of two identical levels did not warn: %v", p.Warnings)
	}
}

func TestControlChangingNothingIsNotAWarning(t *testing.T) {
	// The control's whole job is to change nothing. Counting it as an idle
	// level would make the warning fire on every well-formed sweep, which
	// trains the reader to skip the line that is supposed to flag a level that
	// measures nothing but variance.
	p, err := Load(write(t, `
dataset: dataset.yaml
axis: harness
samples: 2
agent: {kind: mock, bin: /bin/true}
levels:
  - {name: base}
  - {name: verbose, agent: {append_system_prompt: be terse}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("a well-formed sweep warned about itself: %v", p.Warnings)
	}
}

// TestLevelPathsResolveAgainstTheMatrixDirectory pins the rule that a sweep
// behaves the same wherever it is invoked from. A relative plugin_dirs that
// resolved to nothing would load no skills and the sweep would report that
// skills make no difference, so this is a correctness rule, not a convenience.
func TestLevelPathsResolveAgainstTheMatrixDirectory(t *testing.T) {
	// Bin and add_dirs belong to the harness axis, plugin_dirs to the skill
	// axis, so exercising both in one document would be a cross-axis change --
	// which the loader rejects, correctly.
	harnessPath := write(t, `
dataset: dataset.yaml
axis: harness
agent: {kind: mock, bin: claude}
levels:
  - name: stock
  - name: scripted
    agent: {bin: ../tools/agent.sh}
`)
	p, err := Load(harnessPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := filepath.Dir(harnessPath)

	if p.Base.Bin != "claude" {
		t.Errorf("base bin = %q; a bare name must stay a PATH lookup", p.Base.Bin)
	}
	if p.Levels[0].Agent.Bin != "claude" {
		t.Errorf("control bin = %q, want the shared bare name", p.Levels[0].Agent.Bin)
	}
	if want := filepath.Join(filepath.Dir(dir), "tools", "agent.sh"); p.Levels[1].Agent.Bin != want {
		t.Errorf("level bin = %q, want %q", p.Levels[1].Agent.Bin, want)
	}

	skillPath := write(t, `
dataset: dataset.yaml
axis: skill
agent:
  kind: claude_code
  isolation: {disable_slash_commands: false}
levels:
  - name: no-skills
  - name: with-skills
    agent: {plugin_dirs: [skills]}
`)
	sp, err := Load(skillPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := sp.Levels[1].Agent.PluginDirs
	if len(got) != 1 {
		t.Fatalf("plugin dirs = %v, want one entry", got)
	}
	if want := filepath.Join(filepath.Dir(skillPath), "skills"); got[0] != want {
		t.Errorf("plugin dir = %q, want %q", got[0], want)
	}
}

func TestAxisOwnershipIsDisjointAndComplete(t *testing.T) {
	seen := map[string]Axis{}
	for _, a := range Axes {
		for k := range a.Owned() {
			if prev, dup := seen[k]; dup {
				t.Errorf("key %q is owned by both %s and %s", k, prev, a)
			}
			seen[k] = a
		}
	}
	for _, k := range specKeys {
		if _, ok := seen[k]; !ok {
			t.Errorf("agent.Spec key %q belongs to no axis, so it can never be varied", k)
		}
	}
	// The derivation reads YAML tags; if a field were renamed without its tag
	// being updated, the key set would be right and the ownership wrong.
	if len(specKeys) != 25 {
		t.Logf("agent.Spec exposes %d keys: %v", len(specKeys), specKeys)
	}
	for _, must := range []string{"plugin_dirs", "model", "isolation", "bin", "append_system_prompt"} {
		if !contains(specKeys, must) {
			t.Errorf("key %q missing from specKeys: %v", must, specKeys)
		}
	}
	if AxisSkill.Owned()["model"] || AxisModel.Owned()["plugin_dirs"] || AxisHarness.Owned()["model"] {
		t.Error("the axes share a field")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

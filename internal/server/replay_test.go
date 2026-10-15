package server

// Tests for the replay preset document: what a preset may say, and what the
// loader refuses.
//
// The refusals are the substance here. A preset is named on a form and the
// name is all a reader sees, so a preset that silently changes a field its
// label does not describe -- a "harness" that changes the model, a file that
// misspells a key -- would make the run and the page disagree about what was
// asked for. Every case below pins one of those refusals.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
)

// writeConfig writes a replay config into its own directory and returns the
// path, so each case's relative paths resolve against files beside it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "replays.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAReplayConfigLoadsItsPresets is the happy path: names survive, blocks
// decode into agent specs, and relative paths are resolved against the
// document's own directory rather than wherever the server was started.
func TestAReplayConfigLoadsItsPresets(t *testing.T) {
	path := writeConfig(t, `
harnesses:
  - name: clean-room
    agent:
      isolation:
        bare: true
        disable_slash_commands: true
skills:
  - name: repo
    agent:
      plugin_dirs: ["./skills"]
models: ["sonnet", "opus"]
`)
	presets, err := LoadReplayConfig(path)
	if err != nil {
		t.Fatalf("LoadReplayConfig: %v", err)
	}
	if got := presets.HarnessNames(); strings.Join(got, ",") != "clean-room" {
		t.Errorf("harnesses = %v", got)
	}
	if got := presets.SkillNames(); strings.Join(got, ",") != "repo" {
		t.Errorf("skills = %v", got)
	}

	h, ok := presets.Harness("clean-room")
	if !ok {
		t.Fatal("the clean-room harness is missing")
	}
	if h.Agent.Isolation == nil || !h.Agent.Isolation.Bare || !h.Agent.Isolation.DisableSlashCommands {
		t.Errorf("the clean-room harness lost its isolation: %+v", h.Agent.Isolation)
	}

	// The plugin dir is written relative to the config, and it must come out
	// absolute against that directory: a preset means the same thing wherever
	// the server was started from.
	s, ok := presets.Skill("repo")
	if !ok {
		t.Fatal("the repo skill is missing")
	}
	want := filepath.Join(filepath.Dir(path), "skills")
	if len(s.Agent.PluginDirs) != 1 || s.Agent.PluginDirs[0] != want {
		t.Errorf("plugin dirs = %v, want [%s]", s.Agent.PluginDirs, want)
	}

	if len(presets.ModelSuggestions) != 2 || presets.ModelSuggestions[0] != "sonnet" {
		t.Errorf("model suggestions = %v", presets.ModelSuggestions)
	}
}

// TestAReplayConfigRefusesUnknownKeys pins the strict decode on both levels:
// a document key the schema does not have, and an agent key agent.Spec does
// not have. Either would otherwise be a setting someone wrote that never
// reached the agent, which is a run that does not do what its file says.
func TestAReplayConfigRefusesUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"an unknown top-level key", `
harnesses: [{name: h, agent: {}}]
skills: [{name: none, agent: {}}]
harneses: []
`, "field harneses"},
		{"an unknown agent key", `
harnesses: [{name: h, agent: {tool: ["Bash"]}}]
skills: [{name: none, agent: {}}]
`, "field tool"},
		{"an unknown nested key", `
harnesses: [{name: h, agent: {isolation: {bareness: true}}}]
skills: [{name: none, agent: {}}]
`, "bareness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadReplayConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("the config loaded; want a refusal\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %q:\n%v", tc.want, err)
			}
		})
	}
}

// TestAReplayConfigEnforcesAxisOwnership pins the one rule the doc calls its
// own: a harness owns everything except the model fields and plugin_dirs, a
// skill owns only plugin_dirs. The two trespassers are refused with a message
// that says which preset stepped over, so a person editing the file can find
// the line from the error alone.
func TestAReplayConfigEnforcesAxisOwnership(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"a harness that sets the model", `
harnesses: [{name: h, agent: {model: sonnet}}]
skills: [{name: none, agent: {}}]
`, `harness preset "h" sets "model"`},
		{"a harness that sets plugin dirs", `
harnesses: [{name: h, agent: {plugin_dirs: ["./s"]}}]
skills: [{name: none, agent: {}}]
`, `harness preset "h" sets "plugin_dirs"`},
		{"a skill that sets the model", `
harnesses: [{name: h, agent: {}}]
skills: [{name: s, agent: {model: sonnet}}]
`, `skills preset "s" sets "model"`},
		{"a skill that sets isolation", `
harnesses: [{name: h, agent: {}}]
skills: [{name: s, agent: {isolation: {bare: true}}}]
`, `skills preset "s" sets "isolation"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadReplayConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("the config loaded; want a refusal\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the trespass:\n%v", err)
			}
		})
	}
}

// TestAReplayConfigRefusesDuplicatesAndEmpties pins the remaining shape rules:
// duplicate names in one group (the form's selects could not tell them apart),
// an empty name (there would be nothing to select), and a group with no
// presets at all (the form's select would be empty, and a replay with no
// harness would be one the server invented itself).
func TestAReplayConfigRefusesDuplicatesAndEmpties(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"duplicate harnesses", `
harnesses: [{name: h, agent: {}}, {name: h, agent: {}}]
skills: [{name: none, agent: {}}]
`, `duplicate harness preset "h"`},
		{"duplicate skills", `
harnesses: [{name: h, agent: {}}]
skills: [{name: s, agent: {}}, {name: s, agent: {}}]
`, `duplicate skills preset "s"`},
		{"a nameless preset", `
harnesses: [{agent: {}}]
skills: [{name: none, agent: {}}]
`, "harnesses[0] has no name"},
		{"no harnesses", `
harnesses: []
skills: [{name: none, agent: {}}]
`, "names no harnesses"},
		{"no skills", `
harnesses: [{name: h, agent: {}}]
skills: []
`, "names no skills"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadReplayConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("the config loaded; want a refusal\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

// TestTheDefaultReplayPresets pins the built-ins' names: a replay row records
// the preset *name*, so the two names below are part of what a row says and
// cannot drift without the rows becoming unreadable.
func TestTheDefaultReplayPresets(t *testing.T) {
	p := DefaultReplayPresets()
	if got := strings.Join(p.HarnessNames(), ","); got != "clean-room,host" {
		t.Errorf("default harnesses = %q", got)
	}
	if got := strings.Join(p.SkillNames(), ","); got != "none" {
		t.Errorf("default skills = %q", got)
	}
}

// TestCheckSkillUnderHarness covers the vacuous-skill refusal at its source:
// a harness that disables slash commands removes every skill from the agent's
// context, so a skill preset that injects plugin dirs under it would run with
// no skills and report that skills do not matter.
func TestCheckSkillUnderHarness(t *testing.T) {
	withDirs := ReplayPreset{
		Name:  "repo",
		Agent: agent.Spec{PluginDirs: []string{"/tmp/skills"}},
	}
	plain := ReplayPreset{Name: "none", Agent: agent.Spec{}}

	// Without slash commands disabled the combination is fine.
	if err := checkSkillUnderHarness(withDirs, plain); err != nil {
		t.Errorf("a skill under a plain harness was refused: %v", err)
	}
	// An empty skill under a disabling harness is fine: there is nothing to
	// remove.
	disabling := ReplayPreset{Name: "clean-room", Agent: agent.Spec{
		Isolation: &agent.Isolation{DisableSlashCommands: true},
	}}
	if err := checkSkillUnderHarness(plain, disabling); err != nil {
		t.Errorf("the empty skill under a disabling harness was refused: %v", err)
	}
	err := checkSkillUnderHarness(withDirs, disabling)
	if err == nil {
		t.Fatal("a skill with plugin dirs under a disabling harness was accepted")
	}
	for _, want := range []string{`skill "repo"`, `harness "clean-room"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// TestTheReplayPostRefusesAVacuousSkill is the same rule seen from the form:
// posting a skill that injects plugin dirs with a harness that disables slash
// commands is a 400 that names both presets, not a replay that runs without
// the skills it said it used.
func TestTheReplayPostRefusesAVacuousSkill(t *testing.T) {
	srv, _ := sessionServer(t, func(o *Options) {
		o.Replays.Skills = append(o.Replays.Skills, ReplayPreset{
			Name:  "repo",
			Agent: agent.Spec{PluginDirs: []string{t.TempDir()}},
		})
	})
	h := srv.Handler()

	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()
	form := formFor(t, doc)
	form.Set("skill", "repo") // under clean-room, whose isolation disables them
	rec := postReplay(t, h, "claude", claudeSession, form, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clean-room") {
		t.Errorf("the refusal does not name the harness:\n%s", rec.Body.String())
	}
}

package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/attrib"
)

// ReplayConfig is the on-disk replay preset document, loaded from
// `serve --replay-config <yaml>`.
//
// The vocabulary is deliberately the matrix level one: a harness or a skill is
// a partial agent block, so a preset means the same thing here as it does in a
// sweep. What differs is which fields a preset may touch -- the axis ownership
// rules decide that, because a "harness" preset that changed the model would be
// a second place to configure the model, and the model the form asks for is the
// one the run must drive.
type ReplayConfig struct {
	Harnesses []ReplayPresetDoc `yaml:"harnesses"`
	Skills    []ReplayPresetDoc `yaml:"skills"`
	// Models is a suggestion list for the form's free-text model field. It is
	// not a restriction: the field accepts any single-line model name.
	Models []string `yaml:"models"`
}

// ReplayPresetDoc is one named preset as written on disk.
type ReplayPresetDoc struct {
	Name string `yaml:"name"`
	// Agent is a partial agent.Spec, in the matrix level vocabulary.
	Agent map[string]any `yaml:"agent"`
}

// ReplayPreset is one resolved preset: a partial agent block plus the names it
// is forbidden to touch.
type ReplayPreset struct {
	Name  string
	Agent agent.Spec
}

// ReplayPresets is the validated document the server keeps in memory.
//
// The named presets are looked up by name at execution time rather than being
// copied into the replay row, so editing the file takes effect on the next
// replay without a restart-by-comparison.
type ReplayPresets struct {
	Harnesses        []ReplayPreset
	Skills           []ReplayPreset
	ModelSuggestions []string
}

// Harness returns the named harness preset.
func (p *ReplayPresets) Harness(name string) (ReplayPreset, bool) {
	for _, h := range p.Harnesses {
		if h.Name == name {
			return h, true
		}
	}
	return ReplayPreset{}, false
}

// Skill returns the named skill preset.
func (p *ReplayPresets) Skill(name string) (ReplayPreset, bool) {
	for _, s := range p.Skills {
		if s.Name == name {
			return s, true
		}
	}
	return ReplayPreset{}, false
}

// HarnessNames lists the presets the form offers, in file order.
func (p *ReplayPresets) HarnessNames() []string {
	out := make([]string, 0, len(p.Harnesses))
	for _, h := range p.Harnesses {
		out = append(out, h.Name)
	}
	return out
}

// SkillNames lists the skill presets the form offers, in file order.
func (p *ReplayPresets) SkillNames() []string {
	out := make([]string, 0, len(p.Skills))
	for _, s := range p.Skills {
		out = append(out, s.Name)
	}
	return out
}

// DefaultReplayPresets is what serve uses without a --replay-config file: the
// two harness postures and no skills, which is the smallest set that still
// makes the replay form meaningful. The names are fixed so that a replay row
// written by a default-config server reads the same later on.
func DefaultReplayPresets() *ReplayPresets {
	strict := agent.Isolation{Bare: true, StrictMCPConfig: true, DisableSlashCommands: true}
	host := agent.Isolation{Bare: false, StrictMCPConfig: false, DisableSlashCommands: false}
	return &ReplayPresets{
		Harnesses: []ReplayPreset{
			{Name: "clean-room", Agent: agent.Spec{Isolation: &strict}},
			{Name: "host", Agent: agent.Spec{Isolation: &host}},
		},
		Skills: []ReplayPreset{
			{Name: "none", Agent: agent.Spec{PluginDirs: []string{}}},
		},
	}
}

// LoadReplayConfig reads and validates a replay preset document.
//
// The validation failures are fatal at startup (the caller exits 2) rather than
// at replay time, because a preset that is wrong is wrong for every replay
// asked of it, and a form that offers a broken preset would accept the request
// and fail it later with a message about a file the person never opened.
func LoadReplayConfig(path string) (*ReplayPresets, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("server: read replay config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc ReplayConfig
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("server: parse replay config %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)

	out := &ReplayPresets{ModelSuggestions: doc.Models}
	out.Harnesses, err = resolvePresets(dir, "harnesses", doc.Harnesses, attrib.AxisHarness.Owned())
	if err != nil {
		return nil, err
	}
	out.Skills, err = resolvePresets(dir, "skills", doc.Skills, attrib.AxisSkill.Owned())
	if err != nil {
		return nil, err
	}
	if len(out.Harnesses) == 0 {
		return nil, fmt.Errorf("server: replay config %s names no harnesses", path)
	}
	if len(out.Skills) == 0 {
		return nil, fmt.Errorf("server: replay config %s names no skills", path)
	}
	return out, nil
}

// resolvePresets validates one group of presets against the fields its axis
// owns, and resolves the paths inside them against the document's directory.
func resolvePresets(dir, what string, docs []ReplayPresetDoc, owned map[string]bool) ([]ReplayPreset, error) {
	seen := map[string]bool{}
	out := make([]ReplayPreset, 0, len(docs))
	for i, doc := range docs {
		where := fmt.Sprintf("%s[%d]", what, i)
		if strings.TrimSpace(doc.Name) == "" {
			return nil, fmt.Errorf("server: replay preset %s has no name", where)
		}
		if seen[doc.Name] {
			return nil, fmt.Errorf("server: duplicate %s preset %q", strings.TrimSuffix(what, "es"), doc.Name)
		}
		seen[doc.Name] = true

		// The partial block is re-decoded through agent.Spec with
		// KnownFields(true), so a key the vocabulary does not have is an error
		// here rather than a field that silently never reaches the agent. The
		// matrix loader does the same thing, for the same reason.
		blob, err := yaml.Marshal(doc.Agent)
		if err != nil {
			return nil, fmt.Errorf("server: replay preset %q: %w", doc.Name, err)
		}
		tdec := yaml.NewDecoder(bytes.NewReader(blob))
		tdec.KnownFields(true)
		var spec agent.Spec
		if err := tdec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("server: replay preset %q: %w", doc.Name, err)
		}

		// A preset is a partial block: any key it does not name stays at the
		// run's own value. But the key it names has to be one its axis owns,
		// or the form's labels stop describing the run -- a "skill" preset
		// that quietly changed the model would defeat the model field beside
		// it.
		for k := range doc.Agent {
			if !owned[k] {
				return nil, fmt.Errorf(
					"server: %s preset %q sets %q, which it does not own (a harness may change everything but model, fallback_model, effort and plugin_dirs; a skill may only change plugin_dirs)",
					strings.TrimSuffix(what, "es"), doc.Name, k)
			}
		}

		resolvePresetPaths(dir, &spec)
		out = append(out, ReplayPreset{Name: doc.Name, Agent: spec})
	}
	return out, nil
}

// resolvePresetPaths makes the paths inside a preset absolute against the
// document's directory, the way a matrix level's are, so that a preset means
// the same thing wherever the server was started from. bin and directories use
// different rules for the same reasons resolveAgentPaths documents.
func resolvePresetPaths(dir string, s *agent.Spec) {
	s.Bin = agent.ResolveCommand(dir, s.Bin)
	for i, d := range s.AddDirs {
		s.AddDirs[i] = agent.ResolveDir(dir, d)
	}
	for i, d := range s.PluginDirs {
		s.PluginDirs[i] = agent.ResolveDir(dir, d)
	}
}

// checkSkillUnderHarness rejects the combination that makes a skill preset
// silently vacuous under a harness preset, for the same reason attrib's
// checkSkillLevel does: --disable-slash-commands removes every skill from the
// agent's context, including the ones --plugin-dir names, so the replay would
// run with no skills while its row said it ran with one.
func checkSkillUnderHarness(skill ReplayPreset, harness ReplayPreset) error {
	if len(skill.Agent.PluginDirs) == 0 {
		return nil
	}
	if harness.Agent.Isolation != nil && harness.Agent.Isolation.DisableSlashCommands {
		return fmt.Errorf(
			"skill %q injects %d plugin dir(s) but harness %q sets isolation.disable_slash_commands, "+
				"which removes every skill from the context including those; the replay would run with "+
				"no skills and report that skills do not matter",
			skill.Name, len(skill.Agent.PluginDirs), harness.Name)
	}
	return nil
}

// combineReplaySpec builds the agent spec for one replay from the harness and
// skill presets and the model the form asked for.
//
// The model is applied last and after the ownership check, so a preset file
// cannot sneak a model in through a field the axis rules leave open. The two
// presets do not overlap by construction -- a harness may not set plugin_dirs
// and a skill may not set isolation -- so the order between them is only about
// legibility, not about which wins.
func combineReplaySpec(harness, skill ReplayPreset, model string) (agent.Spec, error) {
	if err := checkSkillUnderHarness(skill, harness); err != nil {
		return agent.Spec{}, err
	}
	spec := harness.Agent
	spec.PluginDirs = skill.Agent.PluginDirs
	spec.Model = model
	return spec, nil
}

// validReplayModel reports whether m is safe to forward as --model.
//
// The value is echoed into command lines the report prints back, so a model
// name carrying shell metacharacters would spend its effect there. A tight
// character set rather than a length limit: the idiom it covers (vendor alias,
// provider/name, a path-like id) is all of the shapes a model actually takes.
func validReplayModel(m string) bool {
	if m == "" || strings.ContainsAny(m, " \t\r\n") {
		return false
	}
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._:/-", r):
		default:
			return false
		}
	}
	return true
}

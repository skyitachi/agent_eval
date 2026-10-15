// Package replay turns a recorded session into a run from a web request: it
// collects the session into a dataset, runs it under the harness, skill set and
// model that were picked, and registers the run's report.
//
// # What a request is allowed to choose
//
// A replay launches an agent on this machine with permission prompts bypassed,
// so what a request may choose is the security boundary of the feature. The
// harness and the skill set are named presets from a file the operator wrote
// (or the two built-in ones); a request names one and cannot supply its
// contents. That keeps the executable, its arguments and the directories
// handed to it out of anything a browser can send. The model is the one free
// value, and it is forwarded to --model as a single quoted argument, which is
// what `aeval run --model` already does.
package replay

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/attrib"
)

// Built-in preset names, used when no presets file is given.
const (
	HarnessCleanRoom = "clean-room"
	HarnessHost      = "host"
	SkillNone        = "none"
)

// Harness is one way of driving the agent: everything about the process except
// the model and the skills.
type Harness struct {
	Name  string
	Notes string
	Spec  agent.Spec
}

// Skill is one set of skill directories.
type Skill struct {
	Name  string
	Notes string
	Dirs  []string
}

// Presets are the choices a replay can be made under.
type Presets struct {
	// Path is the file they were read from; empty for the built-ins.
	Path      string
	Harnesses []Harness
	Skills    []Skill
	// Models are suggestions shown beside the model field. The field itself is
	// free text, because model ids are the one thing an operator cannot
	// enumerate in advance.
	Models       []string
	DefaultModel string
}

// presetFile is the on-disk document.
type presetFile struct {
	Models       []string     `yaml:"models,omitempty"`
	DefaultModel string       `yaml:"default_model,omitempty"`
	Harnesses    []harnessDoc `yaml:"harnesses"`
	Skills       []skillDoc   `yaml:"skills,omitempty"`
}

type harnessDoc struct {
	Name  string `yaml:"name"`
	Notes string `yaml:"notes,omitempty"`
	// Agent is a partial agent.Spec, in the vocabulary of a matrix document's
	// agent block.
	Agent map[string]any `yaml:"agent,omitempty"`
}

type skillDoc struct {
	Name  string   `yaml:"name"`
	Notes string   `yaml:"notes,omitempty"`
	Dirs  []string `yaml:"plugin_dirs,omitempty"`
}

// DefaultPresets are what a server offers with no presets file: Claude Code
// in the clean room every other run uses, the same with the host's own
// configuration let in, and no skills.
func DefaultPresets() *Presets {
	host := agent.Isolation{Bare: false, StrictMCPConfig: false, DisableSlashCommands: false}
	return &Presets{
		Harnesses: []Harness{
			{
				Name:  HarnessCleanRoom,
				Notes: "claude with --bare, --strict-mcp-config and --disable-slash-commands: no user hooks, memory, MCP servers or CLAUDE.md",
				Spec:  agent.Spec{Kind: agent.KindClaudeCode, Bin: "claude"},
			},
			{
				Name:  HarnessHost,
				Notes: "claude with this machine's own settings, hooks, MCP servers and skills",
				Spec:  agent.Spec{Kind: agent.KindClaudeCode, Bin: "claude", Isolation: &host},
			},
		},
		Skills: []Skill{{Name: SkillNone, Notes: "no skill directories"}},
	}
}

var presetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LoadPresets reads a presets file. Paths in it resolve against the file's
// directory, as in a matrix document.
//
// The file is checked the way a matrix document is: unknown keys are errors,
// a harness may not set the model or the skills (those are the other two
// choices, and a harness that set them would make the picker lie), and a skill
// set that names a directory which does not exist is refused, because it would
// load nothing and the replay would report that the skill did not matter.
func LoadPresets(path string) (*Presets, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay presets: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f presetFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("replay presets: parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)

	p := &Presets{Path: abs, Models: f.Models, DefaultModel: strings.TrimSpace(f.DefaultModel)}
	if len(f.Harnesses) == 0 {
		return nil, fmt.Errorf("replay presets: %s defines no harness", path)
	}
	owned := attrib.AxisHarness.Owned()
	seen := map[string]bool{}
	for i, h := range f.Harnesses {
		where := fmt.Sprintf("harnesses[%d]", i)
		if !presetName.MatchString(h.Name) {
			return nil, fmt.Errorf("replay presets: %s: name %q must match %s", where, h.Name, presetName)
		}
		if seen["h:"+h.Name] {
			return nil, fmt.Errorf("replay presets: harness %q is defined twice", h.Name)
		}
		seen["h:"+h.Name] = true
		var trespass []string
		for k := range h.Agent {
			if !owned[k] {
				trespass = append(trespass, k)
			}
		}
		if len(trespass) > 0 {
			sort.Strings(trespass)
			return nil, fmt.Errorf("replay presets: harness %q sets %s, which the model and skill choices own; "+
				"a harness that set them would override what the person picked", h.Name, strings.Join(trespass, ", "))
		}
		spec, err := decodeSpec(h.Agent)
		if err != nil {
			return nil, fmt.Errorf("replay presets: harness %q: %w", h.Name, err)
		}
		spec.Bin = agent.ResolveCommand(dir, spec.Bin)
		for i, d := range spec.AddDirs {
			spec.AddDirs[i] = agent.ResolveDir(dir, d)
		}
		p.Harnesses = append(p.Harnesses, Harness{Name: h.Name, Notes: strings.TrimSpace(h.Notes), Spec: spec})
	}
	if len(f.Skills) == 0 {
		p.Skills = []Skill{{Name: SkillNone, Notes: "no skill directories"}}
	}
	for i, s := range f.Skills {
		where := fmt.Sprintf("skills[%d]", i)
		if !presetName.MatchString(s.Name) {
			return nil, fmt.Errorf("replay presets: %s: name %q must match %s", where, s.Name, presetName)
		}
		if seen["s:"+s.Name] {
			return nil, fmt.Errorf("replay presets: skill %q is defined twice", s.Name)
		}
		seen["s:"+s.Name] = true
		var dirs []string
		for _, d := range s.Dirs {
			d = agent.ResolveDir(dir, d)
			if info, err := os.Stat(d); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("replay presets: skill %q names %s, which is not a directory; "+
					"a replay with it would load nothing and report that the skill made no difference", s.Name, d)
			}
			dirs = append(dirs, d)
		}
		p.Skills = append(p.Skills, Skill{Name: s.Name, Notes: strings.TrimSpace(s.Notes), Dirs: dirs})
	}
	return p, nil
}

func decodeSpec(m map[string]any) (agent.Spec, error) {
	if len(m) == 0 {
		return agent.Spec{}, nil
	}
	blob, err := yaml.Marshal(m)
	if err != nil {
		return agent.Spec{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(blob))
	dec.KnownFields(true)
	var spec agent.Spec
	if err := dec.Decode(&spec); err != nil {
		return agent.Spec{}, err
	}
	return spec, nil
}

// Harness returns the named harness.
func (p *Presets) Harness(name string) (Harness, bool) {
	for _, h := range p.Harnesses {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}

// Skill returns the named skill set.
func (p *Presets) Skill(name string) (Skill, bool) {
	for _, s := range p.Skills {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// modelPattern is what a model value may look like. Model ids are vendor
// aliases ("sonnet"), dated ids ("claude-sonnet-5-20260101"), provider paths
// ("us.anthropic.claude-…", "openrouter/x/y") and bracketed variants
// ("opus[1m]"). The value reaches the CLI as one quoted word either way; the
// pattern is there so that a value which is plainly not a model id -- a
// newline, a shell fragment -- is refused with a sentence instead of being
// handed to claude to refuse.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]+-]{0,127}$`)

// Choice is what a replay request picks.
type Choice struct {
	Harness string
	Skill   string
	Model   string
}

// Resolve turns a choice into the agent spec it means, or says which part of
// it is not on offer.
func (p *Presets) Resolve(c Choice) (agent.Spec, Choice, error) {
	c.Harness = strings.TrimSpace(c.Harness)
	c.Skill = strings.TrimSpace(c.Skill)
	c.Model = strings.TrimSpace(c.Model)
	if c.Harness == "" && len(p.Harnesses) > 0 {
		c.Harness = p.Harnesses[0].Name
	}
	if c.Skill == "" && len(p.Skills) > 0 {
		c.Skill = p.Skills[0].Name
	}
	if c.Model == "" {
		c.Model = p.DefaultModel
	}

	h, ok := p.Harness(c.Harness)
	if !ok {
		return agent.Spec{}, c, fmt.Errorf("there is no harness %q (offered: %s)", c.Harness, p.harnessNames())
	}
	s, ok := p.Skill(c.Skill)
	if !ok {
		return agent.Spec{}, c, fmt.Errorf("there is no skill set %q (offered: %s)", c.Skill, p.skillNames())
	}
	if c.Model != "" && !modelPattern.MatchString(c.Model) {
		return agent.Spec{}, c, fmt.Errorf("%q does not look like a model id: letters, digits and . _ : / @ [ ] + - only, "+
			"up to 128 characters", c.Model)
	}

	spec := h.Spec
	// Copies, so that one replay's spec cannot alias the preset's slices.
	spec.Tools = append([]string(nil), spec.Tools...)
	spec.AllowedTools = append([]string(nil), spec.AllowedTools...)
	spec.DisallowedTools = append([]string(nil), spec.DisallowedTools...)
	spec.AddDirs = append([]string(nil), spec.AddDirs...)
	spec.MCPServers = append([]string(nil), spec.MCPServers...)
	spec.ExtraArgs = append([]string(nil), spec.ExtraArgs...)
	spec.EnvFromHost = append([]string(nil), spec.EnvFromHost...)
	if spec.Isolation != nil {
		iso := *spec.Isolation
		spec.Isolation = &iso
	}
	if spec.Kind == "" {
		spec.Kind = agent.KindClaudeCode
	}
	spec.Model = c.Model
	spec.PluginDirs = append([]string(nil), s.Dirs...)
	if spec.PermissionMode == "" {
		spec.PermissionMode = "bypassPermissions"
	}

	// --disable-slash-commands removes every skill, the picked ones included.
	// A replay with skills under it would load nothing and its report would
	// say the skills made no difference, which is the false conclusion
	// attrib's skill check exists to prevent; refused here for the same reason.
	if len(spec.PluginDirs) > 0 && spec.EffectiveIsolation().DisableSlashCommands {
		return agent.Spec{}, c, fmt.Errorf("skill set %q cannot load under harness %q: the harness passes "+
			"--disable-slash-commands, which removes every skill including these. Pick a harness that "+
			"leaves slash commands on, or the %q skill set", c.Skill, c.Harness, SkillNone)
	}
	return spec, c, nil
}

func (p *Presets) harnessNames() string {
	names := make([]string, 0, len(p.Harnesses))
	for _, h := range p.Harnesses {
		names = append(names, h.Name)
	}
	return strings.Join(names, ", ")
}

func (p *Presets) skillNames() string {
	names := make([]string, 0, len(p.Skills))
	for _, s := range p.Skills {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

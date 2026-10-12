// Package attrib turns a sweep over one factor into an answer about that factor.
//
// # The three axes
//
// The tool claims that three things can be varied and told apart:
//
//   - harness -- the implementation and its configuration: which binary, which
//     agent kind, which isolation flags, which appended system prompt. This is
//     the axis that changes without any model or skill changing, which is the
//     whole reason it is defined as "implementation + configuration" rather
//     than as "the tool".
//   - model -- which model the attempt is made with.
//   - skill -- which skill directories the attempt is allowed to load.
//
// The axes have to be orthogonal or a difference cannot be attributed. That
// claim is cheap to write in a comment and expensive to trust, so this package
// enforces it: a level may only touch the fields its axis owns, and a plan that
// varies two axes at once is rejected before anything runs.
//
// # What a sweep cannot show
//
// One factor at a time. Each level of a sweep is run with the other two axes
// held at the same value, and the comparison is between levels, not between
// combinations. That design cannot see an interaction: if a skill only helps on
// one model, a skill sweep run on the other model will report "no effect", and
// it will be right about what it measured and wrong about what the reader will
// hear. Every attribution this package produces carries that caveat, because the
// alternative -- a number that looks like an answer -- is worse than a caveat.
//
// # Pairing
//
// Comparison is per task, never between run totals. Two runs over the same
// dataset have the same task mix; comparing their totals would hide a level that
// fixed three hard tasks and broke three easy ones. Pairing makes that show up
// as three wins and three losses instead of as no change.
package attrib

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/skyitachi/agenteval/internal/agent"
)

// Axis is one of the three things a sweep may vary.
type Axis string

// The three axes. They are named rather than indexed because a report that says
// "axis 2" attributes nothing.
const (
	// AxisHarness varies the implementation and its configuration.
	AxisHarness Axis = "harness"
	// AxisModel varies the model the attempt is made with.
	AxisModel Axis = "model"
	// AxisSkill varies the skill directories available to the attempt.
	AxisSkill Axis = "skill"
)

// Axes lists the valid axes in a stable order, for help text and validation.
var Axes = []Axis{AxisHarness, AxisModel, AxisSkill}

// Valid reports whether a is a known axis.
func (a Axis) Valid() bool {
	switch a {
	case AxisHarness, AxisModel, AxisSkill:
		return true
	}
	return false
}

// Owned returns the agent-spec fields this axis is allowed to change.
//
// The sets are disjoint by construction: model fields and the skill field are
// named, and everything else falls to harness. That way a new field added to
// agent.Spec belongs to harness by default rather than to nothing, and adding it
// cannot silently become a second free axis inside a model sweep.
func (a Axis) Owned() map[string]bool {
	switch a {
	case AxisModel:
		return map[string]bool{"model": true, "fallback_model": true, "effort": true}
	case AxisSkill:
		return map[string]bool{"plugin_dirs": true}
	default:
		owned := map[string]bool{}
		for _, k := range specKeys {
			if !modelOwned[k] && !skillOwned[k] {
				owned[k] = true
			}
		}
		return owned
	}
}

var (
	modelOwned = map[string]bool{"model": true, "fallback_model": true, "effort": true}
	skillOwned = map[string]bool{"plugin_dirs": true}
)

// specKeys is every key agent.Spec serialises to, read from the struct's own
// YAML tags.
//
// It is derived rather than hand-written, and derived from the tags rather than
// from a marshalled zero value, because every field is `omitempty`: a marshalled
// empty spec is the empty document, which would leave the harness axis owning
// nothing and rejecting every harness sweep as a trespass. Reading the tags also
// means a field added to agent.Spec joins an axis automatically instead of
// silently becoming unvaryable.
var specKeys = func() []string {
	t := reflect.TypeOf(agent.Spec{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}()

// ---------------------------------------------------------------------------
// The on-disk document
// ---------------------------------------------------------------------------

// File is a matrix document: the base configuration plus one entry per level.
//
// Every path in it is resolved relative to the document, not to the working
// directory, so that the same file means the same thing wherever it is invoked
// from.
type File struct {
	Name  string `yaml:"name,omitempty"`
	Notes string `yaml:"notes,omitempty"`
	// Dataset is the path to the dataset manifest to sweep over.
	Dataset string `yaml:"dataset"`
	// Axis is the one thing the levels are allowed to vary.
	Axis Axis `yaml:"axis"`
	// Control names the level every other level is compared against. Optional:
	// it defaults to the first level, which is the one a reader would assume.
	Control string `yaml:"control,omitempty"`

	Samples     int     `yaml:"samples,omitempty"`
	Concurrency int     `yaml:"concurrency,omitempty"`
	Threshold   float64 `yaml:"threshold,omitempty"`
	TimeoutS    int     `yaml:"timeout_s,omitempty"`
	WorkRoot    string  `yaml:"work_root,omitempty"`
	// KeepWorkspaces leaves every cell's directory behind for inspection.
	KeepWorkspaces bool `yaml:"keep_workspaces,omitempty"`

	// Agent is the configuration shared by every level.
	Agent agent.Spec `yaml:"agent,omitempty"`
	// Levels each override part of Agent. A level's agent block is a partial
	// document: only the keys it names are replaced, and a nested block such as
	// isolation replaces the whole block rather than merging into it, so what a
	// level changes is always visible by reading it.
	Levels []LevelDoc `yaml:"levels"`
}

// LevelDoc is one level as written on disk.
type LevelDoc struct {
	Name  string `yaml:"name"`
	Notes string `yaml:"notes,omitempty"`
	// Agent is a partial agent.Spec. Unknown keys are an error, not a silent
	// no-op: a misspelled plugin_dirs would otherwise produce a sweep in which
	// the skill axis appears not to matter.
	Agent map[string]any `yaml:"agent,omitempty"`
}

// Level is a resolved level.
type Level struct {
	Name  string
	Notes string
	// Agent is the base configuration with this level's overrides applied.
	Agent agent.Spec
	// Changed lists the spec keys this level differs on. It is the evidence for
	// the orthogonality claim: a level whose Changed is a subset of its axis's
	// owned fields is a level that varied one thing.
	Changed []string
}

// Plan is a validated sweep, ready to run.
type Plan struct {
	// ID names the sweep. Assigned by the caller so that a resumed sweep keeps
	// the id it started with.
	ID   string
	Name string
	// Notes are free-form remarks carried into the report.
	Notes string

	// Path is the matrix document, and Dir the directory paths resolve against.
	Path string
	Dir  string

	DatasetPath string
	Axis        Axis
	Control     string

	Samples     int
	Concurrency int
	Threshold   float64
	Timeout     time.Duration
	WorkRoot    string
	KeepWork    bool

	Base   agent.Spec
	Levels []Level

	// Warnings are things that are legal but worth saying out loud, such as a
	// sweep with a single sample per level that cannot separate signal from
	// noise.
	Warnings []string
}

// Step is one level of the plan, in the order it should be run.
type Step struct {
	Level     Level
	IsControl bool
}

// Steps orders the levels for execution with the control first, so that a sweep
// interrupted early still has the reference every comparison needs.
func (p *Plan) Steps() []Step {
	steps := make([]Step, 0, len(p.Levels))
	for _, l := range p.Levels {
		if l.Name == p.Control {
			steps = append(steps, Step{Level: l, IsControl: true})
		}
	}
	for _, l := range p.Levels {
		if l.Name != p.Control {
			steps = append(steps, Step{Level: l})
		}
	}
	return steps
}

// LevelNames lists the level names in declaration order.
func (p *Plan) LevelNames() []string {
	out := make([]string, 0, len(p.Levels))
	for _, l := range p.Levels {
		out = append(out, l.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// Load reads and validates a matrix document.
func Load(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("attrib: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("attrib: parse %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)

	p := &Plan{
		Name:        f.Name,
		Notes:       strings.TrimSpace(f.Notes),
		Path:        abs,
		Dir:         dir,
		Axis:        f.Axis,
		Control:     f.Control,
		Samples:     f.Samples,
		Concurrency: f.Concurrency,
		Threshold:   f.Threshold,
		Timeout:     time.Duration(f.TimeoutS) * time.Second,
		Base:        f.Agent,
		KeepWork:    f.KeepWorkspaces,
	}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	}
	if f.Dataset == "" {
		return nil, errors.New("attrib: the matrix does not name a dataset")
	}
	p.DatasetPath = resolve(dir, f.Dataset)
	if f.WorkRoot != "" {
		p.WorkRoot = resolve(dir, f.WorkRoot)
	}
	if p.Samples <= 0 {
		p.Samples = 1
	}
	if p.Concurrency <= 0 {
		p.Concurrency = 1
	}
	if p.Timeout <= 0 {
		p.Timeout = 10 * time.Minute
	}

	baseMap, err := specToMap(f.Agent)
	if err != nil {
		return nil, err
	}
	resolveAgentPaths(dir, &p.Base)
	for i, doc := range f.Levels {
		lv, err := resolveLevel(doc, baseMap, i)
		if err != nil {
			return nil, err
		}
		resolveAgentPaths(dir, &lv.Agent)
		p.Levels = append(p.Levels, lv)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// resolveAgentPaths makes the paths inside an agent spec absolute against the
// matrix document's directory, so that a sweep behaves the same wherever it is
// invoked from.
//
// Only fields that are unambiguously paths are touched. settings and
// mcp_servers are left alone because both also accept inline JSON, and putting a
// JSON blob through path joining would turn `{"args":["a/b"]}` into a path.
//
// bin and the directory fields use different rules on purpose: a bare bin names
// a program to find on PATH, whereas a bare directory has no PATH to fall back
// on and must be read relative to the document.
func resolveAgentPaths(dir string, s *agent.Spec) {
	s.Bin = agent.ResolveCommand(dir, s.Bin)
	for i, d := range s.AddDirs {
		s.AddDirs[i] = agent.ResolveDir(dir, d)
	}
	for i, d := range s.PluginDirs {
		s.PluginDirs[i] = agent.ResolveDir(dir, d)
	}
}

func resolveLevel(doc LevelDoc, baseMap map[string]any, index int) (Level, error) {
	where := fmt.Sprintf("levels[%d]", index)
	if strings.TrimSpace(doc.Name) == "" {
		return Level{}, fmt.Errorf("attrib: %s has no name", where)
	}
	where = fmt.Sprintf("level %q", doc.Name)

	merged := make(map[string]any, len(baseMap)+len(doc.Agent))
	for k, v := range baseMap {
		merged[k] = v
	}
	for k, v := range doc.Agent {
		merged[k] = v
	}
	// Re-decode through the struct rather than reading the map, so that a key
	// the struct does not have is an error and type mismatches are caught here
	// rather than by the CLI at run time.
	blob, err := yaml.Marshal(merged)
	if err != nil {
		return Level{}, fmt.Errorf("attrib: %s: %w", where, err)
	}
	tdec := yaml.NewDecoder(bytes.NewReader(blob))
	tdec.KnownFields(true)
	var spec agent.Spec
	if err := tdec.Decode(&spec); err != nil {
		return Level{}, fmt.Errorf("attrib: %s: %w", where, err)
	}

	changed := diffMaps(baseMap, merged)
	return Level{Name: doc.Name, Notes: strings.TrimSpace(doc.Notes), Agent: spec, Changed: changed}, nil
}

func (p *Plan) validate() error {
	if !p.Axis.Valid() {
		return fmt.Errorf("attrib: unknown axis %q (want one of %s)", p.Axis, axisList())
	}
	if len(p.Levels) < 2 {
		return fmt.Errorf("attrib: a sweep needs at least two levels, got %d", len(p.Levels))
	}

	seen := map[string]bool{}
	for _, l := range p.Levels {
		if err := checkLevelName(l.Name); err != nil {
			return err
		}
		if seen[l.Name] {
			return fmt.Errorf("attrib: duplicate level %q", l.Name)
		}
		seen[l.Name] = true
	}

	if p.Control == "" {
		p.Control = p.Levels[0].Name
	}
	if !seen[p.Control] {
		return fmt.Errorf("attrib: control %q is not one of the levels %v", p.Control, p.LevelNames())
	}

	owned := p.Axis.Owned()
	for _, l := range p.Levels {
		var trespass []string
		for _, k := range l.Changed {
			if !owned[k] {
				trespass = append(trespass, k)
			}
		}
		if len(trespass) > 0 {
			return fmt.Errorf(
				"attrib: level %q changes %s, which the %s axis does not own (owns: %s)\n"+
					"       one axis at a time, or the difference cannot be attributed",
				l.Name, strings.Join(trespass, ", "), p.Axis, strings.Join(sortedKeys(owned), ", "))
		}
		if p.Axis == AxisSkill {
			if err := checkSkillLevel(l); err != nil {
				return err
			}
		}
	}

	if p.Axis == AxisModel && p.Base.Model == "" {
		for _, l := range p.Levels {
			if l.Agent.Model == "" {
				return fmt.Errorf("attrib: level %q leaves the model unset, so it cannot be a model level", l.Name)
			}
		}
	}

	if p.Samples < 2 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"each task runs once per level (samples=%d), so a one-task difference and noise "+
				"are the same number; raise samples to separate them", p.Samples))
	}
	// A non-control level that changes nothing is not a level of the sweep at
	// all: measuring it twice just samples the noise floor. The control is
	// excluded, because changing nothing is precisely what a control is for --
	// warning about it would make the warning fire on every well-formed sweep
	// and train the reader to ignore it.
	idle := 0
	for _, l := range p.Levels {
		if l.Name != p.Control && len(l.Changed) == 0 {
			idle++
		}
	}
	if idle > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d non-control level(s) change nothing, so they measure run-to-run variance rather than a factor", idle))
	}
	return nil
}

// checkSkillLevel rejects the combination that makes a skill sweep silently
// vacuous. --disable-slash-commands removes every skill from the agent's
// context, including the ones named by --plugin-dir, so a skill sweep that
// leaves it on would load nothing and report that skills make no difference.
func checkSkillLevel(l Level) error {
	if len(l.Agent.PluginDirs) == 0 {
		return nil
	}
	if l.Agent.Isolation != nil && l.Agent.Isolation.DisableSlashCommands {
		return fmt.Errorf(
			"attrib: level %q injects %d plugin dir(s) but isolation.disable_slash_commands is true,\n"+
				"       which removes every skill from the context including those; the level would run\n"+
				"       with no skills and the sweep would report that skills do not matter",
			l.Name, len(l.Agent.PluginDirs))
	}
	return nil
}

func checkLevelName(name string) error {
	if strings.ContainsAny(name, "/\\ \t") {
		return fmt.Errorf("attrib: level name %q may not contain whitespace or path separators", name)
	}
	if strings.ContainsRune(name, '=') {
		return fmt.Errorf("attrib: level name %q may not contain '='", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolve makes a path absolute against dir. A path that is already absolute is
// returned unchanged, so an absolute entry in a matrix file keeps meaning what
// it says.
func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(dir, path))
}

// specToMap projects a spec onto the keys it actually serialises to. Going
// through YAML rather than reflection means the projection is the serialisation
// contract itself, so a field hidden from the document is hidden here too.
func specToMap(s agent.Spec) (map[string]any, error) {
	blob, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("attrib: encode spec: %w", err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("attrib: decode spec: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// diffMaps lists the keys of b that differ from a, plus keys present in a and
// missing from b. Values are compared structurally, so re-stating a base value
// is not a change -- a level that writes `model` and gets the same model as the
// base did not vary the model.
func diffMaps(a, b map[string]any) []string {
	var out []string
	for k, v := range b {
		if !reflect.DeepEqual(a[k], v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func axisList() string {
	parts := make([]string, 0, len(Axes))
	for _, a := range Axes {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, " | ")
}

package dataset

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// graderFile is the document a grader declaration holds.
//
// Deliberately narrower than GraderSpec: weight, required and timeout_s are
// task-level decisions, and the two commands that read this file (`collect`,
// `serve`) have nowhere to put them -- a line here would be accepted and then
// dropped, which reads as configured. They are unknown keys instead, so the
// mistake is an error at parse time rather than a judge that behaves unlike its
// file says.
type graderFile struct {
	Type   string         `yaml:"type"`
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config"`
}

// LoadGraderSpec reads one grader declaration from a file.
//
// The shape is the one a dataset's graders[] entry has -- type, name, config --
// so a declaration can move between a file and a manifest without being
// rewritten, and the endpoint a judge calls is recorded next to the run instead
// of in whichever shell happened to export it.
//
// YAML is the documented form; JSON decodes too, because YAML is a superset and
// `--grader-config` already takes JSON.
//
// Unknown keys are an error for the same reason they are in a manifest: a
// misspelled `confg:` would leave a judge with none of the config it was meant
// to have. Keys *inside* config are the grader's own business -- it decodes
// them with DisallowUnknownFields too, so `auth_tokn_env` is refused there
// rather than silently ignored.
func LoadGraderSpec(path string) (GraderSpec, error) {
	var spec GraderSpec
	raw, err := os.ReadFile(path)
	if err != nil {
		return spec, fmt.Errorf("read the grader declaration: %w", err)
	}
	var f graderFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return spec, fmt.Errorf("%s is empty; it has to declare at least a type (e.g. `type: llm_judge`)", path)
		}
		return spec, fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(f.Type) == "" {
		return spec, fmt.Errorf("%s declares no grader type; the first key has to be one, e.g. `type: llm_judge`", path)
	}
	return GraderSpec{Type: strings.TrimSpace(f.Type), Name: strings.TrimSpace(f.Name), Config: f.Config}, nil
}

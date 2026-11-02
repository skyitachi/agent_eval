package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGraderFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "judge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// The point of the file is that the endpoint and the credential live next to
// the run. If the four keys a judge needs to call a model do not survive the
// read, the file is decoration.
func TestAGraderFileCarriesTheWholeDeclaration(t *testing.T) {
	path := writeGraderFile(t, `type: llm_judge
name: judge
config:
  input: both
  base_url: https://api.deepseek.com/anthropic
  auth_token_env: DEEPSEEK_API_KEY
  model: deepseek-chat
  max_tokens: 1024
`)
	spec, err := LoadGraderSpec(path)
	if err != nil {
		t.Fatalf("LoadGraderSpec: %v", err)
	}
	if spec.Type != "llm_judge" || spec.Name != "judge" {
		t.Errorf("type/name = %q/%q, want llm_judge/judge", spec.Type, spec.Name)
	}
	want := map[string]any{
		"input":          "both",
		"base_url":       "https://api.deepseek.com/anthropic",
		"auth_token_env": "DEEPSEEK_API_KEY",
		"model":          "deepseek-chat",
		// Numbers arrive through YAML as ints; the grader decodes them through
		// JSON, which does the same, so this is the shape the judge expects.
		"max_tokens": 1024,
	}
	for k, v := range want {
		if got := spec.Config[k]; got != v {
			t.Errorf("config[%s] = %#v, want %#v", k, got, v)
		}
	}
}

// `--grader-config` takes JSON, and the file is read by a YAML parser, so the
// same object has to work in both spellings -- otherwise moving a working
// command line into a file silently changes what it means.
func TestAGraderFileAcceptsJSONToo(t *testing.T) {
	path := writeGraderFile(t, `{"type": "llm_judge", "config": {"input": "diff"}}`)
	spec, err := LoadGraderSpec(path)
	if err != nil {
		t.Fatalf("LoadGraderSpec: %v", err)
	}
	if spec.Type != "llm_judge" || spec.Config["input"] != "diff" {
		t.Errorf("spec = %+v, want the JSON declaration", spec)
	}
}

// A typo in a top-level key would leave a judge with none of the config it was
// meant to have: the file would be accepted and the judge would look at an
// empty rubric. It is an error here instead. (Keys inside `config` belong to
// the grader, which decodes them with the same strictness when it runs.)
func TestAGraderFileRefusesAKeyItDoesNotUnderstand(t *testing.T) {
	path := writeGraderFile(t, `type: llm_judge
confg:
  input: both
`)
	_, err := LoadGraderSpec(path)
	if err == nil {
		t.Fatal("accepted a misspelled key")
	}
	// The key names where it is wrong, not only that something is.
	if !strings.Contains(err.Error(), "confg") || !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should name the key and the file", err)
	}
}

// weight, required and timeout_s are task-level, and both callers of this file
// would drop them. Refusing them is the only way "written" stays the same as
// "in effect".
func TestAGraderFileRefusesTaskLevelKeys(t *testing.T) {
	path := writeGraderFile(t, "type: llm_judge\ntimeout_s: 300\n")
	if _, err := LoadGraderSpec(path); err == nil {
		t.Fatal("accepted timeout_s, which the callers would have dropped")
	}
}

func TestAGraderFileNeedsAType(t *testing.T) {
	for _, body := range []string{"", "name: judge\nconfig:\n  input: both\n"} {
		path := writeGraderFile(t, body)
		_, err := LoadGraderSpec(path)
		if err == nil {
			t.Fatalf("accepted a declaration with no type: %q", body)
		}
		if !strings.Contains(err.Error(), "type") {
			t.Errorf("error %q should say the type is what is missing", err)
		}
	}
}

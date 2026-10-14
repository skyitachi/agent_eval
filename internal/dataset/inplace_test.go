package dataset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The inplace-workspace tests.
//
// This workspace type is the only one that names a directory the harness did not
// create, which makes its validation load-bearing rather than cosmetic. A
// relative path here would be resolved against whatever directory the dataset
// happens to be sitting in -- so the same manifest would replay in a different
// place on a different machine, and the run would still succeed. Every rule
// below is one that fails silently if it is dropped, so each is asserted on its
// own.

// writeDataset writes a manifest and its tasks file into a fresh directory and
// returns the manifest path.
func writeDataset(t *testing.T, manifest string, tasks ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dataset.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(tasks, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "dataset.yaml")
}

const inplaceManifest = "name: inplace\nversion: 0.0.1\ntasks_file: tasks.jsonl\n"

// loadErr returns whatever ValidatePath makes of a manifest built from tasks,
// or nil when it is accepted.
func loadErr(t *testing.T, manifest string, tasks ...string) error {
	t.Helper()
	path := writeDataset(t, manifest, tasks...)
	_, err := ValidatePath(path)
	return err
}

// loadIssues returns the validation issues for a manifest built from tasks, or
// nil when the manifest is accepted.
func loadIssues(t *testing.T, manifest string, tasks ...string) []ValidationIssue {
	t.Helper()
	err := loadErr(t, manifest, tasks...)
	if err == nil {
		return nil
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("ValidatePath: %v (%T), want a *ValidationError", err, err)
	}
	return vErr.Issues
}

func TestInplaceAcceptsADirectoryAndTheStateItStartsFrom(t *testing.T) {
	issues := loadIssues(t, inplaceManifest,
		`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","dir":"/tmp/somewhere",`+
			`"ref":"abc123","session":"sess-1"},"output":{"type":"patch"},`+
			`"graders":[{"type":"patch","name":"g","weight":1}]}`)
	if len(issues) != 0 {
		t.Fatalf("a well-formed inplace task was rejected:\n%v", issues)
	}
}

func TestInplaceRequiresADir(t *testing.T) {
	issues := loadIssues(t, inplaceManifest,
		`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","ref":"abc123"},`+
			`"output":{"type":"patch"},"graders":[{"type":"patch","name":"g","weight":1}]}`)
	if !anyIssueMentions(issues, "workspace.dir") {
		t.Errorf("issues = %v, want one about the missing workspace.dir", issues)
	}
}

func TestInplaceRefusesARelativeDir(t *testing.T) {
	// A relative path would be resolved against the dataset, so the same
	// manifest would replay somewhere else on another machine -- and the run
	// would succeed either way, which is what makes this worth refusing.
	issues := loadIssues(t, inplaceManifest,
		`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","dir":"../project",`+
			`"ref":"abc123"},"output":{"type":"patch"},`+
			`"graders":[{"type":"patch","name":"g","weight":1}]}`)
	if !anyIssueMentions(issues, "must be absolute") {
		t.Errorf("issues = %v, want one about the path not being absolute", issues)
	}
}

func TestInplaceRefusesRepoAssetsAndSetup(t *testing.T) {
	// Each of these describes a directory the harness builds. An inplace replay
	// deliberately does not build one, so accepting them would promise something
	// that never happens.
	for field, snippet := range map[string]string{
		"repo":   `"repo":"https://example.com/x.git"`,
		"assets": `"assets":"somewhere"`,
		"setup":  `"setup":[{"run":"make"}]`,
	} {
		issues := loadIssues(t, inplaceManifest,
			`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","dir":"/tmp/somewhere",`+
				`"ref":"abc123",`+snippet+`},"output":{"type":"patch"},`+
				`"graders":[{"type":"patch","name":"g","weight":1}]}`)
		if len(issues) == 0 {
			t.Errorf("workspace.%s was accepted on an inplace task", field)
			continue
		}
		joined := issues[0].String()
		if !strings.Contains(joined, field) {
			t.Errorf("the issue for %s does not name it: %s", field, joined)
		}
	}
}

func TestInplaceRefusesAnUnknownWorkspaceKey(t *testing.T) {
	// Unknown keys are errors everywhere else in a dataset, and this type is no
	// exception: a misspelled "session" would leave the task naming no session,
	// and the discard directory and the baseline are keyed by it.
	err := loadErr(t, inplaceManifest,
		`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","dir":"/tmp/somewhere",`+
			`"ref":"abc123","sesssion":"sess-1"},"output":{"type":"patch"},`+
			`"graders":[{"type":"patch","name":"g","weight":1}]}`)
	if err == nil {
		t.Fatal("a misspelled workspace key was accepted")
	}
	if !strings.Contains(err.Error(), "sesssion") {
		t.Errorf("error = %v, want it to name the key that was not understood", err)
	}
}

func issueStringsForTest(issues []ValidationIssue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.String())
	}
	return out
}

func anyIssueMentions(issues []ValidationIssue, want string) bool {
	for _, i := range issues {
		if strings.Contains(i.String(), want) {
			return true
		}
	}
	return false
}

func TestInplaceKeepsTheSessionOnTheTask(t *testing.T) {
	// The session id is what ties a task back to the start-of-session record,
	// the discard directory and (for a directory that is not a repository) the
	// baseline. It is a field of the task, not a metadata entry: metadata is
	// carried through rather than interpreted, and this one is read.
	path := writeDataset(t, inplaceManifest,
		`{"id":"t1","prompt":"do it","workspace":{"type":"inplace","dir":"/tmp/somewhere",`+
			`"ref":"abc123","session":"sess-1"},"output":{"type":"patch"},`+
			`"graders":[{"type":"patch","name":"g","weight":1}]}`)
	ds, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ds.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(ds.Tasks))
	}
	ws := ds.Tasks[0].Workspace
	if ws.Session != "sess-1" {
		t.Errorf("session = %q, want it preserved on the workspace", ws.Session)
	}
	if ws.Ref != "abc123" {
		t.Errorf("ref = %q, want it preserved", ws.Ref)
	}
}

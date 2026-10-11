package dataset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Reward contract
// ---------------------------------------------------------------------------

func TestRewardDefaultsToTheDenseScore(t *testing.T) {
	// The nil receiver is the compatibility guarantee: a dataset that predates
	// the reward contract must behave exactly like one that says `type: score`.
	var missing *RewardSpec
	if got := missing.EffectiveType(); got != RewardScore {
		t.Errorf("nil receiver = %q, want %q", got, RewardScore)
	}
	if got := (&RewardSpec{}).EffectiveType(); got != RewardScore {
		t.Errorf("empty type = %q, want %q", got, RewardScore)
	}
	if got := (&RewardSpec{Type: RewardPass}).EffectiveType(); got != RewardPass {
		t.Errorf("declared type = %q, want %q", got, RewardPass)
	}
}

func TestRewardIsInheritedFromDefaults(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
name: r
tasks_file: tasks.jsonl
defaults:
  reward:
    type: pass
`)
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"inherits","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}]}`+"\n"+
			`{"id":"overrides","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],"reward":{"type":"score"}}`)

	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := index(d)
	if got := byID["inherits"].Reward.EffectiveType(); got != RewardPass {
		t.Errorf("inherited type = %q, want pass", got)
	}
	if got := byID["overrides"].Reward.EffectiveType(); got != RewardScore {
		t.Errorf("task override = %q, want score", got)
	}
}

func TestUnknownRewardTypeIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: r\ntasks_file: tasks.jsonl\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],"reward":{"type":"dense-ish"}}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("an unknown reward type must be rejected, not silently defaulted")
	}
	if !strings.Contains(err.Error(), "dense-ish") {
		t.Errorf("error should name the offending type, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Oracle
// ---------------------------------------------------------------------------

func TestOraclePatchFileIsFoldedIn(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: o\ntasks_file: tasks.jsonl\n")
	if err := os.MkdirAll(filepath.Join(dir, "oracles"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "oracles", "fix.patch"),
		"--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],`+
			`"oracle":{"patch_file":"oracles/fix.patch"}}`)

	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(d.Tasks[0].Oracle.Patch, "+new") {
		t.Errorf("patch_file not folded into patch: %q", d.Tasks[0].Oracle.Patch)
	}
}

func TestAnEmptyOracleIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: o\ntasks_file: tasks.jsonl\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],`+
			`"oracle":{"notes":"I promise this task is fine"}}`)

	// An oracle that applies nothing is worse than none at all: `aeval audit`
	// would report the task as solvable on the strength of the untouched
	// baseline, which is precisely the check it exists to perform.
	_, err := Load(dir)
	if err == nil {
		t.Fatal("an empty oracle must be rejected")
	}
	if !strings.Contains(err.Error(), "oracle") {
		t.Errorf("error should point at the oracle, got: %v", err)
	}
}

func TestOracleApplyCommandNeedsARun(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: o\ntasks_file: tasks.jsonl\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],`+
			`"oracle":{"apply":[{"cwd":"."}]}}`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "run") {
		t.Fatalf("want an error about the missing command, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Guards
// ---------------------------------------------------------------------------

func TestGuardsAreLoadedAndValidated(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: g\ntasks_file: tasks.jsonl\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"ok","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],`+
			`"restore_paths":["tests/**"],"forbid_changed_paths":["Makefile"]}`)

	d, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	task := d.Tasks[0]
	if len(task.RestorePaths) != 1 || task.RestorePaths[0] != "tests/**" {
		t.Errorf("RestorePaths = %v", task.RestorePaths)
	}
	if len(task.ForbidChangedPaths) != 1 || task.ForbidChangedPaths[0] != "Makefile" {
		t.Errorf("ForbidChangedPaths = %v", task.ForbidChangedPaths)
	}
}

func TestGuardListsRejectEmptyPatternsAndOverlap(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "name: g\ntasks_file: tasks.jsonl\n")
	writeFile(t, filepath.Join(dir, "tasks.jsonl"),
		// An empty pattern matches everything, which would either reset the whole
		// workspace or fail every attempt depending on which list it lands in.
		`{"id":"empty","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],"restore_paths":[""]}`+"\n"+
			// The same path in both lists has no coherent meaning: reset it or
			// reject it, not both.
			`{"id":"overlap","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}],`+
			`"restore_paths":["tests/**"],"forbid_changed_paths":["tests/**"]}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected validation issues")
	}
	msg := err.Error()
	if !strings.Contains(msg, "restore_paths") {
		t.Errorf("empty pattern not reported:\n%s", msg)
	}
	if !strings.Contains(msg, "overlap") && !strings.Contains(msg, "both") {
		t.Errorf("overlapping lists not reported:\n%s", msg)
	}
}

func TestGuardsAndOracleEnterTheDatasetHash(t *testing.T) {
	// Anything that can change a reward must change the hash: two runs whose
	// only difference is a guard are not the same experiment.
	base := `{"id":"a","prompt":"p","workspace":{"type":"empty"},"graders":[{"type":"patch"}]`
	cases := []string{
		base + `}`,
		base + `,"restore_paths":["tests/**"]}`,
		base + `,"forbid_changed_paths":["Makefile"]}`,
		base + `,"reward":{"type":"pass"}}`,
		base + `,"oracle":{"files":{"x":"y"}}}`,
	}
	seen := map[string]string{}
	for _, task := range cases {
		dir := t.TempDir()
		writeManifest(t, dir, "name: h\ntasks_file: tasks.jsonl\n")
		writeFile(t, filepath.Join(dir, "tasks.jsonl"), task)
		d, err := Load(dir)
		if err != nil {
			t.Fatalf("Load(%s): %v", task, err)
		}
		if prev, dup := seen[d.Hash]; dup {
			t.Errorf("two different tasks share hash %s:\n  %s\n  %s", d.Hash, prev, task)
		}
		seen[d.Hash] = task
	}
}

// ---------------------------------------------------------------------------

func index(d *Dataset) map[string]TaskSpec {
	out := make(map[string]TaskSpec, len(d.Tasks))
	for _, t := range d.Tasks {
		out[t.ID] = t
	}
	return out
}

// ensure the demo dataset keeps its oracles wired up: it is the documentation,
// so a broken path there is a broken example.
func TestDemoOraclesArePresentAndLoadable(t *testing.T) {
	d, err := Load(demoDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := index(d)
	for _, id := range []string{"fix-average-off-by-one", "implement-slugify"} {
		task, ok := byID[id]
		if !ok {
			t.Fatalf("demo task %s missing", id)
		}
		if task.Oracle == nil || strings.TrimSpace(task.Oracle.Patch) == "" {
			t.Errorf("%s: oracle patch was not loaded", id)
		}
	}
	// The answer-only task deliberately has none, which is what makes it a
	// demonstration of the audit's `unknown` status.
	if byID["read-and-answer"].Oracle != nil {
		t.Error("read-and-answer should not declare an oracle")
	}
	if len(byID["fix-average-off-by-one"].RestorePaths) == 0 {
		t.Error("fix-average-off-by-one should guard its visible test file")
	}
}

func TestRewardSpecSurvivesJSONRoundTrip(t *testing.T) {
	// The dataset hash and the stored config both go through encoding/json, so a
	// field that does not round-trip would change a hash without changing a task.
	spec := RewardSpec{Type: RewardPass, Notes: "all or nothing"}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var back RewardSpec
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != RewardPass || back.Notes != spec.Notes {
		t.Errorf("round trip = %+v", back)
	}
}

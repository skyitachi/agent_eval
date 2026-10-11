package grader

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/trace"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// fixture is a ready-to-grade sandbox with a real git baseline.
type fixture struct {
	t     *testing.T
	sb    *sandbox.Local
	base  *sandbox.Baseline
	task  dataset.TaskSpec
	input string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	sb, err := sandbox.NewLocal(sandbox.LocalConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close(context.Background()) })
	if err := sb.WriteFile("seed.txt", []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := sandbox.InitBaseline(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, sb: sb, base: base, input: "core"}
}

func (f *fixture) write(rel, content string) *fixture {
	f.t.Helper()
	if err := f.sb.WriteFile(rel, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fixture) answer(text string) *fixture {
	f.input = text
	return f
}

func (f *fixture) ctx() *Context {
	f.t.Helper()
	att := &agent.Attempt{Agent: "test", Trace: mustTrace(f.t, f.input)}
	return NewContext(f.task, f.sb, f.base, att)
}

func (f *fixture) spec(typ string, cfg map[string]any) dataset.GraderSpec {
	w := 1.0
	return dataset.GraderSpec{Type: typ, Name: typ, Weight: &w, Config: cfg}
}

func (f *fixture) grade(typ string, cfg map[string]any) *Result {
	f.t.Helper()
	g, err := Get(typ)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := g.Grade(context.Background(), f.ctx(), f.spec(typ, cfg))
	if err != nil {
		f.t.Fatalf("grade(%s): %v", typ, err)
	}
	return res
}

// mustTrace builds a real trace whose final answer is text, by feeding a
// synthesized stream through the production parser.
func mustTrace(t *testing.T, text string) *trace.Trace {
	t.Helper()
	stream := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":` +
		mustJSON(t, text) + `}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":` + mustJSON(t, text) + `,"num_turns":1}` + "\n"
	tr, err := trace.Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// patch grader
// ---------------------------------------------------------------------------

func TestPatchGraderDetectsNoChange(t *testing.T) {
	f := newFixture(t)
	res := f.grade("patch", nil)
	if res.Passed {
		t.Error("a pristine workspace should not pass a patch grader")
	}
	if !strings.Contains(res.Reason, "no changes") {
		t.Errorf("reason = %q", res.Reason)
	}
}

func TestPatchGraderRequiresAndForbidsPaths(t *testing.T) {
	f := newFixture(t).write("slugify.py", "def slugify(): pass\n")
	res := f.grade("patch", map[string]any{
		"require_changed_paths": []string{"slugify.py"},
	})
	if !res.Passed {
		t.Errorf("expected pass, got %q", res.Reason)
	}

	// A required pattern that nothing matches must fail.
	res = f.grade("patch", map[string]any{
		"require_changed_paths": []string{"other.py"},
	})
	if res.Passed {
		t.Error("unmatched required pattern should fail")
	}

	// Editing the visible test suite must be caught.
	f.write("test_calc.py", "def test_x(): pass\n")
	res = f.grade("patch", map[string]any{
		"require_changed_paths": []string{"slugify.py"},
		"forbid_changed_paths":  []string{"test_*.py"},
	})
	if res.Passed {
		t.Errorf("editing a protected path should fail, reason = %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "protected path") {
		t.Errorf("reason should explain the protected path violation: %q", res.Reason)
	}
}

func TestPatchGraderSizeLimits(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 12; i++ {
		f.write(filepath.Join("many", strings.Repeat("x", i+1)+".txt"), "x\n")
	}
	res := f.grade("patch", map[string]any{"max_changed_files": 5})
	if res.Passed {
		t.Error("max_changed_files should reject a sprawling change")
	}
	if !strings.Contains(res.Reason, "limit is 5") {
		t.Errorf("reason = %q", res.Reason)
	}

	res = f.grade("patch", map[string]any{"min_changed_files": 100})
	if res.Passed {
		t.Error("min_changed_files should reject a too-small change")
	}
}

func TestPatchGraderExpectChangedFalse(t *testing.T) {
	f := newFixture(t)
	res := f.grade("patch", map[string]any{"expect_changed": false})
	if !res.Passed {
		t.Errorf("an untouched workspace should pass expect_changed=false, got %q", res.Reason)
	}
	f.write("touched.txt", "x\n")
	res = f.grade("patch", map[string]any{"expect_changed": false})
	if res.Passed {
		t.Error("a touched workspace should fail expect_changed=false")
	}
}

func TestDiffLineCountingIgnoresHeaders(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/x b/x",
		"index 111..222 100644",
		"--- a/x",
		"+++ b/x",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		" context",
	}, "\n")
	if got := countDiffLines(diff); got != 2 {
		t.Errorf("countDiffLines = %d, want 2 (headers excluded)", got)
	}
}

// ---------------------------------------------------------------------------
// exact_match grader
// ---------------------------------------------------------------------------

func TestExactMatchBasics(t *testing.T) {
	f := newFixture(t).answer("platypus")
	res := f.grade("exact_match", map[string]any{"expected": "platypus"})
	if !res.Passed || res.Score != 1 {
		t.Errorf("expected exact pass, got %+v", res)
	}

	res = f.grade("exact_match", map[string]any{"expected": "PLATYPUS"})
	if !res.Passed {
		t.Error("comparison should be case-insensitive by default")
	}
	res = f.grade("exact_match", map[string]any{"expected": "PLATYPUS", "case_sensitive": true})
	if res.Passed {
		t.Error("case_sensitive should reject a case mismatch")
	}

	res = f.grade("exact_match", map[string]any{"expected": "otter"})
	if res.Passed {
		t.Error("wrong answer should fail")
	}
}

// TestExactMatchExtractionStrategies documents the three extraction strategies a
// dataset author can choose from, and why the strict one is the default advice.
//
// Pulling a number out of free-form prose is inherently ambiguous: the text
// "compute(5) returns 31, since 0+1+4+9+16 = 30 and then +1" contains 5, 31, 30
// and 1. No positional rule (first, last) recovers 31 reliably. The robust
// answer is to pin the output contract in the prompt and anchor the pattern to
// it, using a marker or a whole-line match; positional extraction is offered for
// benchmarks that follow a looser convention.
func TestExactMatchExtractionStrategies(t *testing.T) {
	f := newFixture(t)

	// Strategy 1: the prompt demands a bare integer, so require a line that is
	// nothing but an integer.
	strict := map[string]any{
		"expected":  "31",
		"regex":     `(?m)^\s*(-?\d+)\s*$`,
		"normalize": []string{"trim", "strip_code_fence"},
	}
	for _, reply := range []string{"31", "  31  ", "```\n31\n```"} {
		if res := f.answer(reply).grade("exact_match", strict); !res.Passed {
			t.Errorf("strict extraction should accept %q, got %v", reply, res.Details["extracted"])
		}
	}
	// Prose violates the stated contract and must fail rather than be guessed at.
	for _, reply := range []string{
		"The answer is 31.",
		"compute(5) returns 31, since 0+1+4+9+16 = 30 and then +1.",
	} {
		if res := f.answer(reply).grade("exact_match", strict); res.Passed {
			t.Errorf("strict extraction should reject contract-breaking reply %q", reply)
		}
	}

	// Strategy 2: a marker in the output, which is what GSM8K-style evaluators
	// use and which survives otherwise verbose answers.
	marker := map[string]any{"expected": "31", "regex": `(?i)answer:\s*(-?\d+)`}
	if res := f.answer("Reasoning here.\nFinal answer: 31").grade("exact_match", marker); !res.Passed {
		t.Errorf("marker extraction should pass, got %v", res.Details["extracted"])
	}
	if res := f.answer("I am not sure.").grade("exact_match", marker); res.Passed {
		t.Error("marker extraction should fail when the marker is absent")
	}

	// Strategy 3: positional extraction, which is the loosest and the only one
	// that can be fooled by numbers appearing after the real answer.
	twoNums := map[string]any{"expected": "7", "regex": `(\d+)`}
	if res := f.answer("first 7 then 9").grade("exact_match", twoNums); !res.Passed {
		t.Errorf("first-match should take 7, got %v", res.Details["extracted"])
	}
	lastNum := map[string]any{"expected": "9", "regex": `(\d+)`, "regex_match": "last"}
	if res := f.answer("first 7 then 9").grade("exact_match", lastNum); !res.Passed {
		t.Errorf("last-match should take 9, got %v", res.Details["extracted"])
	}

	// A reply with no digits at all is an extraction failure, not a crash.
	res := f.answer("I cannot tell.").grade("exact_match", strict)
	if res.Passed {
		t.Error("no digits should fail")
	}
	if !strings.Contains(res.Reason, "extraction pattern") {
		t.Errorf("reason should mention extraction failure: %q", res.Reason)
	}
}

func TestExactMatchNormalizeOps(t *testing.T) {
	cases := []struct {
		op, in, out string
	}{
		{"trim", "  x  ", "x"},
		{"lower", "AbC", "abc"},
		{"upper", "abc", "ABC"},
		{"collapse_ws", "a   b\n c", "a b c"},
		{"last_line", "a\n\nb  ", "b"},
		{"first_line", "\n a \nb", "a"},
		{"strip_punct", `"abc."`, "abc"},
		{"strip_quotes", `  "abc"  `, "abc"},
		{"strip_code_fence", "```python\n42\n```", "42"},
		{"strip_code_fence", "42", "42"},
	}
	for _, c := range cases {
		got, err := applyNormalize(c.in, c.op, false)
		if err != nil {
			t.Errorf("applyNormalize(%q, %q): %v", c.in, c.op, err)
			continue
		}
		if got != c.out {
			t.Errorf("applyNormalize(%q, %q) = %q, want %q", c.in, c.op, got, c.out)
		}
	}

	if _, err := applyNormalize("x", "nope", false); err == nil {
		t.Error("unknown normalize op should error rather than silently no-op")
	}
}

func TestExactMatchExpectedAnyAndModes(t *testing.T) {
	f := newFixture(t).answer("green")
	res := f.grade("exact_match", map[string]any{"expected_any": []string{"red", "green"}})
	if !res.Passed {
		t.Errorf("expected_any should accept a listed value: %q", res.Reason)
	}

	f.answer("the colour is greenish")
	res = f.grade("exact_match", map[string]any{"expected_any": []string{"green"}, "match_mode": "contains"})
	if !res.Passed {
		t.Error("contains mode should match a substring")
	}

	f.answer("abc123")
	res = f.grade("exact_match", map[string]any{"expected": `^[a-z]+\d+$`, "match_mode": "regex"})
	if !res.Passed {
		t.Errorf("regex mode should match a pattern: %q", res.Reason)
	}
}

func TestExactMatchRequiresAnExpectation(t *testing.T) {
	g, _ := Get("exact_match")
	_, err := g.Grade(context.Background(), newFixture(t).ctx(), dataset.GraderSpec{Type: "exact_match"})
	if err == nil {
		t.Error("a missing expectation is a configuration error and must be reported")
	}
}

// ---------------------------------------------------------------------------
// pytest output parsing
// ---------------------------------------------------------------------------

func TestParsePytestOutput(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		exit       int
		wantPassed int
		wantFailed int
		wantErrors int
		wantOK     bool
		wantScore  float64
	}{
		{
			name:       "all pass",
			out:        "....\n4 passed in 0.03s\n",
			exit:       0,
			wantPassed: 4,
			wantOK:     true,
			wantScore:  1,
		},
		{
			name:       "mixed",
			out:        "..F.\n1 failed, 3 passed in 0.05s\n",
			exit:       1,
			wantPassed: 3,
			wantFailed: 1,
			wantOK:     true,
			wantScore:  0.75,
		},
		{
			name:       "collection error",
			out:        "ERROR test_x.py\n1 error in 0.02s\n",
			exit:       2,
			wantErrors: 1,
			wantOK:     true,
			wantScore:  0,
		},
		{
			name:   "no tests collected",
			out:    "no tests ran in 0.01s\n",
			exit:   5,
			wantOK: true,
		},
		{
			name:   "unparseable",
			out:    "Traceback (most recent call last):\n  ImportError\n",
			exit:   4,
			wantOK: false,
		},
	}
	for _, c := range cases {
		got := ParsePytestOutput(c.out, c.exit)
		if got.ParseOK != c.wantOK {
			t.Errorf("%s: ParseOK = %v, want %v", c.name, got.ParseOK, c.wantOK)
		}
		if got.Passed != c.wantPassed || got.Failed != c.wantFailed || got.Errors != c.wantErrors {
			t.Errorf("%s: passed/failed/errors = %d/%d/%d, want %d/%d/%d",
				c.name, got.Passed, got.Failed, got.Errors, c.wantPassed, c.wantFailed, c.wantErrors)
		}
		if got.Score() != c.wantScore {
			t.Errorf("%s: Score = %v, want %v", c.name, got.Score(), c.wantScore)
		}
	}
}

func TestPytestSuccessPolicy(t *testing.T) {
	ok := PytestOutcome{Passed: 5, Collected: 5, ParseOK: true}
	if passed, _ := ok.Success(0, 0); !passed {
		t.Error("5/5 should pass")
	}
	if passed, reason := ok.Success(10, 0); passed {
		t.Error("min_passed should be enforced")
	} else if !strings.Contains(reason, "at least 10") {
		t.Errorf("reason = %q", reason)
	}

	partial := PytestOutcome{Passed: 4, Failed: 1, Collected: 5, ParseOK: true}
	if passed, _ := partial.Success(0, 0); passed {
		t.Error("a failure should fail by default")
	}
	if passed, _ := partial.Success(0, 1); !passed {
		t.Error("max_failed=1 should tolerate one failure")
	}

	none := PytestOutcome{ParseOK: true}
	if passed, _ := none.Success(0, 0); passed {
		t.Error("a suite that collected nothing must not pass")
	}
	if passed, _ := ok.Success(0, 0); !passed {
		t.Error("sanity")
	}
	unparsed := PytestOutcome{}
	if passed, _ := unparsed.Success(0, 0); passed {
		t.Error("an unparseable run must not pass")
	}
}

// ---------------------------------------------------------------------------
// pytest grader, end to end
// ---------------------------------------------------------------------------

func TestPytestGraderRunsSuite(t *testing.T) {
	requirePytest(t)
	f := newFixture(t)
	f.write("calc.py", "def add(a, b):\n    return a - b\n")
	f.write("test_calc.py", "from calc import add\n\ndef test_add():\n    assert add(1, 2) == 3\n")

	res := f.grade("pytest", map[string]any{"paths": []string{"test_calc.py"}, "timeout_s": 60})
	if res.Passed {
		t.Errorf("broken code should fail the suite, got %+v", res.Details)
	}
	if res.Score != 0 {
		t.Errorf("score = %v, want 0", res.Score)
	}
	if res.Details["failed"] != 1 {
		t.Errorf("failed = %v, want 1", res.Details["failed"])
	}

	// Fixing the code must flip the verdict.
	f.write("calc.py", "def add(a, b):\n    return a + b\n")
	res = f.grade("pytest", map[string]any{"paths": []string{"test_calc.py"}, "timeout_s": 60})
	if !res.Passed {
		t.Errorf("fixed code should pass, got reason=%q details=%v", res.Reason, res.Details)
	}
	if res.Score != 1 {
		t.Errorf("score = %v, want 1", res.Score)
	}
}

func TestPytestGraderInjectsHiddenSuite(t *testing.T) {
	requirePytest(t)
	f := newFixture(t)
	// The agent writes an implementation...
	f.write("slugify.py", "def slugify(t):\n    return t.lower().replace(' ', '-')\n")

	// ...and the suite arrives only at grade time from the dataset.
	suiteDir := filepath.Join(t.TempDir(), "suite")
	if err := os.MkdirAll(suiteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(suiteDir, "test_hidden.py"), []byte(
		"from slugify import slugify\n\ndef test_ok():\n    assert slugify('Hello World') == 'hello-world'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CopyInto resolves relative to the dataset dir, so point the fixture there.
	f.task.DatasetDir = filepath.Dir(suiteDir)
	rel := filepath.Base(suiteDir) + "/test_hidden.py"

	if f.ctx().Sandbox.Exists("test_hidden.py") {
		t.Fatal("hidden suite should not exist before grading")
	}
	res := f.grade("pytest", map[string]any{"suite": rel, "paths": []string{"test_hidden.py"}, "timeout_s": 60})
	if !res.Passed {
		t.Errorf("hidden suite should pass, got %q details=%v", res.Reason, res.Details)
	}
	if !f.ctx().Sandbox.Exists("test_hidden.py") {
		t.Error("suite should have been injected into the workspace")
	}
}

func TestPytestGraderFailsLoudlyWhenSuiteMissing(t *testing.T) {
	f := newFixture(t)
	g, _ := Get("pytest")
	_, err := g.Grade(context.Background(), f.ctx(), dataset.GraderSpec{
		Type: "pytest", Name: "pytest",
		Config: map[string]any{"suite": "does/not/exist"},
	})
	if err == nil {
		t.Error("a missing hidden suite is a configuration error and must be reported")
	}
}

// ---------------------------------------------------------------------------
// command grader
// ---------------------------------------------------------------------------

func TestCommandGraderExitCodeMode(t *testing.T) {
	f := newFixture(t)
	res := f.grade("command", map[string]any{"command": "true"})
	if !res.Passed {
		t.Errorf("exit 0 should pass, got %q", res.Reason)
	}
	res = f.grade("command", map[string]any{"command": "exit 7"})
	if res.Passed {
		t.Error("exit 7 should fail by default")
	}
	res = f.grade("command", map[string]any{"command": "exit 7", "pass_exit_codes": []int{0, 7}})
	if !res.Passed {
		t.Error("exit 7 should pass when listed in pass_exit_codes")
	}
}

func TestCommandGraderJSONMode(t *testing.T) {
	f := newFixture(t)
	res := f.grade("command", map[string]any{
		"parse":   "json",
		"command": `printf '{"score":0.75,"passed":true,"reason":"mostly right"}'`,
	})
	if !res.Passed || res.Score != 0.75 {
		t.Errorf("json verdict = %+v", res)
	}
	if res.Reason != "mostly right" {
		t.Errorf("reason = %q", res.Reason)
	}

	// Prose around the JSON must not break the parse.
	res = f.grade("command", map[string]any{
		"parse":   "json",
		"command": `printf 'i think:\n{"passed":false,"reason":"nope"}\ndone'`,
	})
	if res.Passed {
		t.Error("passed=false should be honoured")
	}
	if res.Reason != "nope" {
		t.Errorf("reason = %q", res.Reason)
	}

	// Invalid output is a failure with an explanation, not a panic.
	res = f.grade("command", map[string]any{
		"parse":   "json",
		"command": "printf 'no json here'",
	})
	if res.Passed || !strings.Contains(res.Reason, "no JSON object") {
		t.Errorf("unparseable output = %+v", res)
	}
}

func TestCommandGraderPlaceholdersAndArtifacts(t *testing.T) {
	f := newFixture(t).write("changed.py", "x = 1\n")
	res := f.grade("command", map[string]any{
		"command": "cat {diff} | grep -c changed.py && cat {answer} > /dev/null",
	})
	if !res.Passed {
		t.Errorf("placeholder substitution failed: %q details=%v", res.Reason, res.Details)
	}

	// The artifact bundle must exist inside the harness directory.
	for _, name := range []string{ArtifactDiff, ArtifactAnswer, ArtifactTask, ArtifactTrace} {
		if !f.ctx().Sandbox.Exists(HarnessDir + "/" + name) {
			t.Errorf("artifact %s was not written", name)
		}
	}
}

func TestHarnessDirNeverAppearsInDiff(t *testing.T) {
	f := newFixture(t)
	// Simulate the grader writing artifacts, then verify the diff stays clean.
	if _, err := WriteArtifacts(f.ctx()); err != nil {
		t.Fatal(err)
	}
	diff, err := f.ctx().Diff()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, HarnessDir) {
		t.Errorf("harness artifacts leaked into the diff:\n%s", diff)
	}
}

// ---------------------------------------------------------------------------
// diff parsing helpers
// ---------------------------------------------------------------------------

func TestPathsFromDiffHandlesQuotedNames(t *testing.T) {
	diff := strings.Join([]string{
		`diff --git a/normal.py b/normal.py`,
		`--- a/normal.py`,
		`+++ b/normal.py`,
		`@@ -1 +1 @@`,
		`-a`,
		`+b`,
		`diff --git "a/\351\205\215\347\275\256.txt" "b/\351\205\215\347\275\256.txt"`,
		`--- "a/\351\205\215\347\275\256.txt"`,
		`+++ "b/\351\205\215\347\275\256.txt"`,
		`@@ -0,0 +1 @@`,
		`+x`,
		`diff --git a/gone.txt b/gone.txt`,
		`--- a/gone.txt`,
		`+++ /dev/null`,
	}, "\n")

	got := pathsFromDiff(diff)
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "normal.py") || !strings.Contains(joined, "gone.txt") {
		t.Errorf("plain paths missing: %v", got)
	}
	// git C-quotes non-ASCII paths; the grader must see the real filename.
	if !strings.Contains(joined, "配置.txt") {
		t.Errorf("quoted non-ASCII path was not decoded: %v", got)
	}
}

func TestUnquoteGitPath(t *testing.T) {
	got, ok := unquoteGitPath(`"a\tb.txt"`)
	if !ok || got != "a\tb.txt" {
		t.Errorf("unquoteGitPath = %q, %v", got, ok)
	}
	if _, ok := unquoteGitPath("plain.txt"); ok {
		t.Error("an unquoted path should not be treated as quoted")
	}
}

// ---------------------------------------------------------------------------
// composition
// ---------------------------------------------------------------------------

func TestRunTaskWeightedComposite(t *testing.T) {
	f := newFixture(t).write("slugify.py", "def slugify(): pass\n").answer("31")
	f.task.Graders = []dataset.GraderSpec{
		{Type: "patch", Name: "patch", Weight: floatPtr(0.2), Config: map[string]any{
			"require_changed_paths": []string{"slugify.py"},
		}},
		{Type: "exact_match", Name: "answer", Weight: floatPtr(0.8), Config: map[string]any{"expected": "31"}},
	}
	grade, err := RunTask(context.Background(), f.ctx(), Threshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(grade.Results) != 2 {
		t.Fatalf("results = %d", len(grade.Results))
	}
	if grade.Score != 1 {
		t.Errorf("score = %v, want 1", grade.Score)
	}
	if !grade.Passed {
		t.Errorf("expected pass, errors=%v", grade.Errors)
	}

	// A zero-weight grader is recorded but must not move the score.
	f.task.Graders = append(f.task.Graders, dataset.GraderSpec{
		Type: "exact_match", Name: "informational", Weight: floatPtr(0),
		Config: map[string]any{"expected": "definitely-wrong"},
	})
	grade, err = RunTask(context.Background(), f.ctx(), Threshold)
	if err != nil {
		t.Fatal(err)
	}
	if grade.Score != 1 {
		t.Errorf("a zero-weight grader changed the score to %v", grade.Score)
	}
	if !grade.Passed {
		t.Error("a zero-weight grader must not fail the task")
	}
}

func TestRunTaskRequiredGraderForcesFailure(t *testing.T) {
	f := newFixture(t).write("slugify.py", "x\n").answer("31")
	f.task.Graders = []dataset.GraderSpec{
		{Type: "exact_match", Name: "answer", Weight: floatPtr(1), Config: map[string]any{"expected": "31"}},
		{Type: "exact_match", Name: "gate", Weight: floatPtr(0), Required: true,
			Config: map[string]any{"expected": "must-not-match"}},
	}
	grade, err := RunTask(context.Background(), f.ctx(), Threshold)
	if err != nil {
		t.Fatal(err)
	}
	if grade.Passed {
		t.Error("a failing required grader must fail the task even at weight 0")
	}
	if grade.Score != 1 {
		t.Errorf("score should still reflect the weighted graders: %v", grade.Score)
	}
}

func TestRunTaskThresholdAllowsPartialCredit(t *testing.T) {
	f := newFixture(t).write("a.py", "x\n")
	f.task.Graders = []dataset.GraderSpec{
		{Type: "patch", Name: "p", Weight: floatPtr(1), Config: map[string]any{"require_changed_paths": []string{"a.py", "b.py"}}},
	}
	// The patch grader is binary, so a partial hit still scores 0; use a
	// threshold-based pass check to confirm the threshold is honoured.
	grade, err := RunTask(context.Background(), f.ctx(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !grade.Passed {
		t.Errorf("threshold 0 should accept anything non-erroring, errors=%v", grade.Errors)
	}
}

func TestRunTaskRejectsUnknownGraderType(t *testing.T) {
	f := newFixture(t)
	f.task.Graders = []dataset.GraderSpec{{Type: "no_such_grader", Name: "x"}}
	if _, err := RunTask(context.Background(), f.ctx(), Threshold); err == nil {
		t.Error("an unknown grader type is a dataset bug and must be reported")
	}
}

func TestRunTaskFlagsGraderRuntimeError(t *testing.T) {
	f := newFixture(t)
	// A pytest grader pointed at a missing hidden suite errors out; the task
	// must be failed and the error surfaced, never silently scored 0.
	f.task.Graders = []dataset.GraderSpec{
		{Type: "pytest", Name: "broken", Weight: floatPtr(1), Config: map[string]any{"suite": "nope/missing"}},
	}
	grade, err := RunTask(context.Background(), f.ctx(), Threshold)
	if err != nil {
		t.Fatal(err)
	}
	if grade.Passed {
		t.Error("an errored grader must not pass the task")
	}
	if len(grade.Errors) == 0 {
		t.Error("the grader error must be recorded on the grade")
	}
}

func TestRegisteredGradersArePresent(t *testing.T) {
	want := []string{"command", "exact_match", "llm_judge", "patch", "pytest"}
	for _, w := range want {
		if _, err := Get(w); err != nil {
			t.Errorf("grader %q not registered: %v", w, err)
		}
	}
}

func floatPtr(v float64) *float64 { return &v }

// requirePytest points the pytest grader at an interpreter that actually has
// pytest, or skips the test.
//
// pytest is a *task runtime* dependency, not a framework dependency: the
// framework must work on a machine with no Python at all, so these tests degrade
// to a skip rather than a failure. A machine with pytest exercises them for
// real.
func requirePytest(t *testing.T) string {
	t.Helper()

	var candidates []string
	if v := strings.TrimSpace(os.Getenv(EnvPython)); v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates,
		"/Users/skyitachi/.workbuddy/binaries/python/envs/default/bin/python",
		"python3", "python")

	for _, c := range candidates {
		cmd := exec.Command(c, "-m", "pytest", "--version")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Setenv(EnvPython, c)
			_ = out
			return c
		}
	}
	t.Skipf("no interpreter with pytest found (tried %s); set %s to run the pytest grader tests",
		strings.Join(candidates, ", "), EnvPython)
	return ""
}

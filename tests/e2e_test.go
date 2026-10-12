// Package tests holds end-to-end tests that drive the built aeval binary the way
// a user would: a dataset on disk, a real subprocess, a real SQLite file, a real
// HTML report.
//
// The unit tests in internal/ each prove one component in isolation. This file
// exists to catch the failures that only appear when the components are wired
// together -- a path resolved against the wrong base directory, a payload the
// report writes but the CLI does not, a verdict that is correct in memory and
// inverted once persisted.
//
// Everything here runs against the scripted agents in testdata/agents and, for
// the sweep, against the checked-in example matrix, so it needs no model, no
// network and no credentials. Tasks that need pytest are skipped rather than
// failed when no suitable interpreter is present: pytest is a task runtime
// dependency, not a framework dependency.
package tests

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	// The version test below rewrites a database into an older shape, which
	// needs a driver of its own: the point is to be a client of the file, not
	// to go through the code under test.
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

var (
	repoRoot string
	cliPath  string
	// python is an interpreter with pytest, or "" when none was found.
	python string
	// demo is the checked-in demo dataset, which every test that actually runs
	// an agent works over.
	demo string
	// agents holds the two scripted stand-ins: one that gets the demo right and
	// one that gets it wrong for a *different* reason on each task.
	agents string
	// matrixExample is the checked-in matrix document.
	matrixExample string
)

func TestMain(m *testing.M) {
	root, err := filepath.Abs("..")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve repo root:", err)
		os.Exit(1)
	}
	repoRoot = root
	demo = filepath.Join(root, "datasets", "demo")
	agents = filepath.Join(root, "tests", "testdata", "agents")
	matrixExample = filepath.Join(root, "examples", "matrix-harness.yaml")

	dir, err := os.MkdirTemp("", "aeval-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	cliPath = filepath.Join(dir, "aeval")
	build := exec.Command("go", "build", "-o", cliPath, "./cmd/aeval")
	build.Dir = repoRoot
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build aeval:", err)
		os.Exit(1)
	}

	python = findPythonWithPytest()
	os.Exit(m.Run())
}

// findPythonWithPytest returns the first interpreter that can import pytest.
//
// AGENTEVAL_PYTHON is honoured first so the same override the grader respects
// works here, which is what lets this suite run on a machine whose default
// python has no pytest.
func findPythonWithPytest() string {
	var candidates []string
	if env := strings.TrimSpace(os.Getenv("AGENTEVAL_PYTHON")); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, "python3", "python")
	for _, c := range candidates {
		cmd := exec.Command(c, "-m", "pytest", "--version")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err == nil {
			return c
		}
	}
	return ""
}

// requirePytest skips the test when nothing can run a pytest task.
func requirePytest(t *testing.T) {
	t.Helper()
	if python == "" {
		t.Skip("no interpreter with pytest found; set AGENTEVAL_PYTHON to run this test")
	}
}

type result struct {
	stdout   string
	stderr   string
	exitCode int
}

// runCLI executes aeval with a hermetic-ish environment and returns the result.
//
// A non-zero exit is data, not a failure: half of what these tests check is that
// the exit code says what it should.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(cliPath, args...)
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	// Only export the override when one was found. Exporting an empty value
	// would leave the grader probing for its own interpreter and reporting the
	// absence of pytest as an agent failure.
	if python != "" {
		cmd.Env = append(cmd.Env, "AGENTEVAL_PYTHON="+python)
	}
	var out, errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return result{stdout: out.String(), stderr: errBuf.String(), exitCode: code}
}

// runAgent executes one full evaluation over the demo dataset and returns the
// payload its report carries. It also covers the claim that a run's own report
// and a regenerated one agree, by reading the report the run itself wrote.
func runAgent(t *testing.T, work, db, name, model string) payload {
	t.Helper()
	requirePytest(t)

	reportPath := filepath.Join(work, name+".html")
	bin := filepath.Join(agents, name+".sh")

	r := runCLI(t, "run",
		"--dataset", demo,
		"--agent", "mock",
		"--bin", bin,
		"--model", model,
		"--name", name,
		"--db", db,
		"--work-root", filepath.Join(work, "work-"+name),
		"--report", reportPath,
	)
	if r.exitCode != 0 {
		t.Fatalf("%s run exited %d\nstdout:\n%s\nstderr:\n%s", name, r.exitCode, r.stdout, r.stderr)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("%s run did not write its report: %v\nstderr:\n%s", name, err, r.stderr)
	}
	p := payloadFromHTML(t, reportPath)
	if p.Run.Name != name {
		t.Errorf("report is for run name %q, want %q", p.Run.Name, name)
	}
	if p.Run.AgentModel != model {
		t.Errorf("recorded model = %q, want %q", p.Run.AgentModel, model)
	}
	if p.Run.Status != "completed" {
		t.Errorf("run status = %q, want completed\nstderr:\n%s", p.Run.Status, r.stderr)
	}
	if p.Run.DatasetName == "" {
		t.Error("the run row should name the dataset it evaluated")
	}
	if len(p.Warnings) != 0 {
		t.Errorf("an ordinary run produced warnings: %v", p.Warnings)
	}
	return p
}

// ---------------------------------------------------------------------------
// Payload projection
// ---------------------------------------------------------------------------

// payload is a deliberately partial view of the report JSON.
//
// It is declared here instead of reusing internal/report so the test is
// decoupled from the production types: if a JSON key is renamed, decoding into
// this struct yields zero values and the assertions below fail. Reusing
// report.Data would make the test agree with any rename, which is exactly the
// class of breakage an end-to-end test is supposed to catch.
//
// `generated` is omitted on purpose. It is the one field that is a property of
// when the report was written rather than of the run it describes, so leaving it
// out is what makes two payloads comparable.
type payload struct {
	Version  int      `json:"version"`
	Title    string   `json:"title"`
	Run      runInfo  `json:"run"`
	Counts   counts   `json:"counts"`
	Metrics  []metric `json:"metrics"`
	Tasks    []task   `json:"tasks"`
	Compare  *compare `json:"compare"`
	Sweep    *sweep   `json:"sweep"`
	Warnings []string `json:"warnings"`
}

type runInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	DatasetName string `json:"dataset_name"`
	DatasetHash string `json:"dataset_hash"`

	AgentKind  string `json:"agent_kind"`
	AgentModel string `json:"agent_model"`
	AgentBin   string `json:"agent_bin"`
	Backend    string `json:"backend"`

	FactorAxis  string `json:"factor_axis"`
	FactorLevel string `json:"factor_level"`
	SweepID     string `json:"sweep_id"`

	Summary summary `json:"summary"`
}

type summary struct {
	Tasks         int   `json:"tasks"`
	Cells         int   `json:"cells"`
	Graded        int   `json:"graded"`
	Passed        int   `json:"passed"`
	Ungraded      int   `json:"ungraded"`
	Timeouts      int   `json:"timeouts"`
	HarnessErrors int   `json:"harness_errors"`
	Tokens        int   `json:"tokens"`
	DurationMs    int64 `json:"duration_ms"`
	BaselineCells int   `json:"baseline_cells"`
	TokenDelta    int   `json:"token_delta"`
	TimeDeltaMs   int64 `json:"time_delta_ms"`
}

type counts struct {
	Cells         int `json:"cells"`
	Tasks         int `json:"tasks"`
	Graded        int `json:"graded"`
	Passed        int `json:"passed"`
	Failed        int `json:"failed"`
	Ungraded      int `json:"ungraded"`
	Timeouts      int `json:"timeouts"`
	HarnessErrors int `json:"harness_errors"`
}

type metric struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Display   string `json:"display"`
	Unit      string `json:"unit"`
	Direction string `json:"direction"`
	Note      string `json:"note"`
}

type task struct {
	Key         string  `json:"key"`
	ID          string  `json:"id"`
	Category    string  `json:"category"`
	Status      string  `json:"status"`
	StopReason  string  `json:"stop_reason"`
	Graded      bool    `json:"graded"`
	Passed      bool    `json:"passed"`
	Score       float64 `json:"score"`
	Threshold   float64 `json:"threshold"`
	TotalTokens int     `json:"total_tokens"`
	Turns       int     `json:"turns"`
	ToolCalls   int     `json:"tool_calls"`
	ToolErrors  int     `json:"tool_errors"`

	Baseline *struct {
		ID         string `json:"id"`
		Ref        string `json:"ref"`
		Tokens     int    `json:"tokens"`
		TokenDelta int    `json:"token_delta"`
		MsDelta    int64  `json:"ms_delta"`
		Output     string `json:"output"`
	} `json:"baseline"`

	Error   string `json:"error"`
	Diff    string `json:"diff"`
	Answer  string `json:"answer"`
	Graders []struct {
		Name   string  `json:"name"`
		Type   string  `json:"type"`
		Passed bool    `json:"passed"`
		Score  float64 `json:"score"`
		Weight float64 `json:"weight"`
		Reason string  `json:"reason"`
		Error  string  `json:"error"`
	} `json:"graders"`
	Events []struct {
		Index    int    `json:"index"`
		Kind     string `json:"kind"`
		Text     string `json:"text"`
		ToolName string `json:"tool_name"`
	} `json:"events"`
}

type compare struct {
	LeftRunID  string `json:"left_run_id"`
	RightRunID string `json:"right_run_id"`
	Summary    struct {
		Common    int `json:"common"`
		Fixed     int `json:"fixed"`
		Regressed int `json:"regressed"`
		BothPass  int `json:"both_pass"`
		BothFail  int `json:"both_fail"`
		Ungraded  int `json:"ungraded"`
		OnlyLeft  int `json:"only_left"`
		OnlyRight int `json:"only_right"`
	} `json:"summary"`
	Tasks []struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
	} `json:"tasks"`
	Metrics []struct {
		Key       string  `json:"key"`
		Delta     float64 `json:"delta"`
		DeltaText string  `json:"delta_text"`
		Verdict   string  `json:"verdict"`
	} `json:"metrics"`
}

// sweep mirrors attrib.Attribution, the payload the sweep page renders.
type sweep struct {
	SweepID      string   `json:"sweep_id"`
	Name         string   `json:"name"`
	Axis         string   `json:"axis"`
	Control      string   `json:"control"`
	Caveats      []string `json:"caveats"`
	DatasetDrift bool     `json:"dataset_drift"`

	Levels []struct {
		Name      string   `json:"name"`
		RunID     string   `json:"run_id"`
		Status    string   `json:"status"`
		IsControl bool     `json:"is_control"`
		Changed   []string `json:"changed"`
		PassRate  *float64 `json:"pass_rate"`
		Summary   summary  `json:"summary"`
	} `json:"levels"`

	Comparisons []struct {
		Level   string   `json:"level"`
		Control string   `json:"control"`
		Common  int      `json:"common"`
		Fixed   []string `json:"fixed"`
		Broke   []string `json:"broke"`
		Stats   []struct {
			Metric        string  `json:"metric"`
			Label         string  `json:"label"`
			Primary       bool    `json:"primary"`
			LowerIsBetter bool    `json:"lower_is_better"`
			ControlMean   float64 `json:"control_mean"`
			LevelMean     float64 `json:"level_mean"`
			MeanDelta     float64 `json:"mean_delta"`
			Wins          int     `json:"wins"`
			Losses        int     `json:"losses"`
			Ties          int     `json:"ties"`
			P             float64 `json:"p"`
			Testable      bool    `json:"testable"`
		} `json:"stats"`
	} `json:"comparisons"`

	Findings []struct {
		Level     string  `json:"level"`
		Metric    string  `json:"metric"`
		Direction string  `json:"direction"`
		MeanDelta float64 `json:"mean_delta"`
		P         float64 `json:"p"`
		Testable  bool    `json:"testable"`
		PassWins  int     `json:"pass_wins"`
		PassLoss  int     `json:"pass_losses"`
		Text      string  `json:"text"`
	} `json:"findings"`
}

func (sw sweep) level(t *testing.T, name string) int {
	t.Helper()
	for i, l := range sw.Levels {
		if l.Name == name {
			return i
		}
	}
	t.Fatalf("sweep has no level %q", name)
	return -1
}

// payloadFromHTML extracts the JSON the report embeds for its own UI.
//
// The payload is embedded as a JavaScript object literal rather than as text in
// a data attribute, so it is read back the way the browser's JS engine would:
// everything between the assignment and the closing tag, minus the trailing
// semicolon.
func payloadFromHTML(t *testing.T, path string) payload {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	body, err := extractPayload(t, string(raw))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var p payload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode payload from %s: %v", path, err)
	}
	return p
}

// extractPayload finds the embedded JSON and returns it without the trailing
// semicolon.
//
// The search stops at the first ";</script>" rather than the first "</script>":
// the payload is followed by the inlined application script, so looking for the
// closing tag alone runs past the JSON and into the code.
func extractPayload(t *testing.T, doc string) (string, error) {
	t.Helper()
	const marker = "window.__AEVAL_PAYLOAD__ = "
	start := strings.Index(doc, marker)
	if start < 0 {
		return "", errors.New("no embedded payload")
	}
	body := doc[start+len(marker):]
	end := strings.Index(body, ";</script>")
	if end < 0 {
		return "", errors.New("unterminated payload script")
	}
	return strings.TrimSpace(body[:end]), nil
}

// taskByID returns the single-sample task with the given id.
func (p payload) taskByID(t *testing.T, id string) task {
	t.Helper()
	for _, cell := range p.Tasks {
		if cell.ID == id {
			return cell
		}
	}
	t.Fatalf("run has no task %q; got %v", id, p.taskIDs())
	return task{}
}

func (p payload) taskIDs() []string {
	out := make([]string, 0, len(p.Tasks))
	for _, cell := range p.Tasks {
		out = append(out, cell.ID)
	}
	return out
}

// metricDisplay returns the pre-formatted value of one metric.
func (p payload) metricDisplay(t *testing.T, key string) metric {
	t.Helper()
	for _, m := range p.Metrics {
		if m.Key == key {
			return m
		}
	}
	t.Fatalf("run has no metric %q", key)
	return metric{}
}

// samePayload compares two run payloads as serialised documents.
//
// It round-trips through JSON so the comparison is over the artefact a reader
// would actually receive rather than over Go values that might agree while
// their encodings differ.
func samePayload(t *testing.T, a, b payload) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ja) == string(jb) {
		return true
	}
	// Report the first differing task so a failure is diagnosable.
	for i := range a.Tasks {
		if i >= len(b.Tasks) {
			break
		}
		ta, _ := json.Marshal(a.Tasks[i])
		tb, _ := json.Marshal(b.Tasks[i])
		if string(ta) != string(tb) {
			t.Logf("task %d differs:\n  during run: %s\n  from db   : %s", i, ta, tb)
		}
	}
	if len(a.Tasks) != len(b.Tasks) {
		t.Logf("task count differs: %d vs %d", len(a.Tasks), len(b.Tasks))
	}
	return false
}

// ---------------------------------------------------------------------------
// The suite
// ---------------------------------------------------------------------------

// TestCLIEndToEnd is the load-bearing test: one dataset, two agents, and the
// claim that the pipeline tells them apart for the right reasons.
func TestCLIEndToEnd(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	good := runAgent(t, work, db, "good", "scripted-good")
	bad := runAgent(t, work, db, "bad", "scripted-bad")

	// --- the good agent resolves everything ---------------------------------
	if good.Counts.Cells != 3 || good.Counts.Tasks != 3 || good.Counts.Graded != 3 {
		t.Errorf("good run counts = %+v, want 3 cells over 3 graded tasks", good.Counts)
	}
	if good.Counts.Passed != 3 || good.Counts.Failed != 0 {
		t.Errorf("good run passed = %d passed / %d failed, want 3/0", good.Counts.Passed, good.Counts.Failed)
	}
	if good.Counts.HarnessErrors != 0 || good.Counts.Timeouts != 0 {
		t.Errorf("the harness should not be the thing failing: %+v", good.Counts)
	}
	if got := good.metricDisplay(t, "pass_rate").Display; got != "100.0%" {
		t.Errorf("pass rate = %q, want 100.0%%", got)
	}
	if got := good.metricDisplay(t, "pass_rate").Direction; got != "up" {
		t.Errorf("pass rate direction = %q; a higher pass rate is good", got)
	}

	// --- and it does so for the right reasons -------------------------------
	//
	// A run can reach 3/3 because the graders work, or because the graders are
	// broken in the permissive direction. These assertions pin the first.
	fix := good.taskByID(t, "fix-average-off-by-one")
	if !fix.Graded || !fix.Passed || len(fix.Graders) != 1 || !fix.Graders[0].Passed {
		t.Errorf("fix-average should pass its visible suite: %+v", fix.Graders)
	}
	if fix.Diff == "" {
		t.Error("a passing patch task should still carry its diff for review")
	}
	if !strings.Contains(fix.Diff, "len(nums)") {
		t.Errorf("the recorded diff does not contain the fix:\n%s", fix.Diff)
	}

	slug := good.taskByID(t, "implement-slugify")
	// The composite is the point: a hidden suite at weight 0.8 plus a patch
	// constraint at 0.2, both of which must pass.
	if len(slug.Graders) != 2 {
		t.Fatalf("implement-slugify should be a two-grader composite, got %d", len(slug.Graders))
	}
	for _, g := range slug.Graders {
		if !g.Passed {
			t.Errorf("grader %s failed on a correct solution: %s", g.Name, g.Reason)
		}
	}

	if answer := good.taskByID(t, "read-and-answer"); answer.Answer != "31" {
		t.Errorf("answer = %q, want 31", answer.Answer)
	}

	// --- the bad agent resolves nothing, per task ---------------------------
	if bad.Counts.Passed != 0 || bad.Counts.Failed != 3 {
		t.Errorf("bad run counts = %+v, want 0 passed / 3 failed", bad.Counts)
	}
	if got := bad.metricDisplay(t, "pass_rate").Display; got != "0.0%" {
		t.Errorf("bad pass rate = %q, want 0.0%%", got)
	}

	// Each task fails for its own recorded reason. This is what makes the run
	// evidence rather than a number: three failures that all read "score 0"
	// would not tell a reader whether the grader ran at all.
	if cell := bad.taskByID(t, "fix-average-off-by-one"); cell.Passed {
		t.Error("a patch that changes the wrong thing must not pass")
	} else if cell.Score <= 0 || cell.Score >= 1 {
		t.Errorf("a partially failing suite should score between 0 and 1, got %v", cell.Score)
	}
	if cell := bad.taskByID(t, "read-and-answer"); cell.Answer != "30" {
		t.Errorf("recorded answer = %q, want the agent's actual (wrong) answer 30", cell.Answer)
	}
	if cell := bad.taskByID(t, "implement-slugify"); cell.Passed {
		t.Error("a slugify that only replaces spaces must not pass the hidden suite")
	}

	// --- the report is a function of the database ---------------------------
	regen := filepath.Join(work, "regen.html")
	if r := runCLI(t, "report", "--db", db, "--run", bad.Run.ID, "-o", regen); r.exitCode != 0 {
		t.Fatalf("report regeneration failed (%d): %s", r.exitCode, r.stderr)
	}
	if fromDB := payloadFromHTML(t, regen); !samePayload(t, bad, fromDB) {
		t.Error("a report regenerated from the database differs from the one written during the run")
	}

	// --- the comparison classifies every transition -------------------------
	cmp := filepath.Join(work, "compare.html")
	r := runCLI(t, "report", "--db", db, "--compare", good.Run.ID, bad.Run.ID, "-o", cmp)
	if r.exitCode != 0 {
		t.Fatalf("compare failed (%d): %s", r.exitCode, r.stderr)
	}
	delta := payloadFromHTML(t, cmp)
	if delta.Compare == nil {
		t.Fatal("the comparison report has no compare section")
	}
	if delta.Compare.Summary.Common != 3 || delta.Compare.Summary.Regressed != 3 {
		t.Errorf("good -> bad over 3 common tasks = %+v, want 3 regressed",
			delta.Compare.Summary)
	}
	if delta.Compare.Summary.Fixed != 0 || delta.Compare.Summary.BothFail != 0 {
		t.Errorf("nothing should be fixed or both-failing: %+v", delta.Compare.Summary)
	}
	for _, td := range delta.Compare.Tasks {
		if td.Verdict != "regressed" {
			t.Errorf("task %s verdict = %q, want regressed", td.ID, td.Verdict)
		}
	}
	// The metric delta carries a direction, and a pass rate that halved is
	// "worse" -- not merely "different".
	for _, m := range delta.Compare.Metrics {
		if m.Key == "pass_rate" && m.Verdict != "worse" {
			t.Errorf("pass rate verdict = %q, want worse", m.Verdict)
		}
	}

	// --- listing sees both runs ---------------------------------------------
	l := runCLI(t, "list", "--db", db)
	if l.exitCode != 0 {
		t.Fatalf("list failed (%d): %s", l.exitCode, l.stderr)
	}
	for _, id := range []string{good.Run.ID, bad.Run.ID} {
		if !strings.Contains(l.stdout, id) {
			t.Errorf("list does not mention run %s:\n%s", id, l.stdout)
		}
	}
}

// TestCLICollectsASessionIntoAReplayableDataset covers the second half of the
// design goal: a dataset can come from a session that already happened, and what
// it produces is a task with a baseline rather than a task with a score.
//
// The distinction matters. There is no ground truth in a transcript, so the
// collected task is measurable and *ungraded*; calling it a failure would invent
// a verdict the data does not contain, and calling it a pass would do the same
// in the other direction.
func TestCLICollectsASessionIntoAReplayableDataset(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(work, "collected")
	session := filepath.Join(repoRoot, "tests", "testdata", "session", "basic.jsonl")
	db := filepath.Join(work, "runs.db")

	r := runCLI(t, "collect",
		"--session", session,
		"--out", out,
		"--name", "collected-demo",
		"--description", "from a recorded session",
		"--tag", "collected",
	)
	if r.exitCode != 0 {
		t.Fatalf("collect exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	for _, want := range []string{"dataset.yaml", "tasks.jsonl", filepath.Join("baselines", "aaaabbbb.json")} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("collect did not write %s: %v", want, err)
		}
	}

	// The transcript path recorded in the dataset must survive being read from
	// anywhere: it is written before the dataset is, so a relative one would
	// only resolve from the directory collect happened to run in.
	rawTasks, err := os.ReadFile(filepath.Join(out, "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var collected struct {
		ID       string `json:"id"`
		Prompt   string `json:"prompt"`
		Baseline struct {
			File string `json:"file"`
		} `json:"baseline"`
		Graders  []any          `json:"graders"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(rawTasks))), &collected); err != nil {
		t.Fatalf("the collected task does not decode: %v\n%s", err, rawTasks)
	}
	if got, _ := collected.Metadata["transcript"].(string); !filepath.IsAbs(got) {
		t.Errorf("recorded transcript = %q, want an absolute path", got)
	}
	// A transcript carries no ground truth, so the task is measurable and
	// unscorable. Emitting a grader anyway would invent a verdict.
	if len(collected.Graders) != 0 {
		t.Errorf("a collected session task got graders: %+v", collected.Graders)
	}
	if collected.Baseline.File == "" {
		t.Error("the collected task has no baseline, which is the whole point of collecting")
	}

	// The output is an ordinary dataset, so the ordinary validator must accept
	// it -- otherwise collect produces something only it can read.
	if v := runCLI(t, "validate", "--dataset", out); v.exitCode != 0 {
		t.Fatalf("the collected dataset does not validate (%d):\n%s\n%s", v.exitCode, v.stdout, v.stderr)
	}

	// Replaying a session with a fresh agent measures it against the recording.
	run := runCLI(t, "run",
		"--dataset", out,
		"--agent", "mock",
		"--bin", "/bin/true",
		"--model", "replay",
		"--name", "collected-replay",
		"--db", db,
		"--work-root", filepath.Join(work, "work"),
		"--report", filepath.Join(work, "collected.html"),
	)
	if run.exitCode != 0 {
		t.Fatalf("replaying the collected dataset exited %d\nstdout:\n%s\nstderr:\n%s",
			run.exitCode, run.stdout, run.stderr)
	}

	p := payloadFromHTML(t, filepath.Join(work, "collected.html"))
	if p.Counts.Cells != 1 || p.Counts.Graded != 0 || p.Counts.Ungraded != 1 {
		t.Errorf("counts = %+v, want one measured but ungraded cell", p.Counts)
	}
	if p.Counts.Failed != 0 {
		t.Errorf("an ungraded cell must not be counted as a failure: %+v", p.Counts)
	}
	if got := p.metricDisplay(t, "pass_rate").Display; got != "—" {
		t.Errorf("pass rate = %q, want an em dash: there was nothing to score", got)
	}
	if got := p.metricDisplay(t, "pass_rate").Note; !strings.Contains(got, "nothing in this run was graded") {
		t.Errorf("the absent pass rate needs a reason, got %q", got)
	}

	// The baseline is the point of collecting: the recorded session's own token
	// count is what the replay is measured against.
	cell := p.taskByID(t, "aaaabbbb")
	if cell.Baseline == nil {
		t.Fatal("the collected task lost its baseline")
	}
	if cell.Baseline.Tokens != 280 {
		t.Errorf("baseline tokens = %d, want the recorded session's 280", cell.Baseline.Tokens)
	}
	if cell.Baseline.TokenDelta != -280 {
		t.Errorf("token delta = %d, want -280 (the replay spent nothing)", cell.Baseline.TokenDelta)
	}
	// The recording's own answer travels with its numbers. A replay that spent
	// less and concluded something else is the comparison a reader is after,
	// and the spend alone cannot express it.
	if !strings.Contains(cell.Baseline.Output, "好的。") {
		t.Errorf("baseline output = %q, want the recorded session's last assistant text", cell.Baseline.Output)
	}
	if p.Run.Summary.BaselineCells != 1 {
		t.Errorf("summary counted %d baseline cells, want 1", p.Run.Summary.BaselineCells)
	}
	if _, ok := metricByKey(p, "baseline_tokens"); !ok {
		t.Error("a run with a baseline should headline its token delta, not bury it in the task")
	}
}

func metricByKey(p payload, key string) (metric, bool) {
	for _, m := range p.Metrics {
		if m.Key == key {
			return m, true
		}
	}
	return metric{}, false
}

// TestCLIMatrixAttributesASweep covers the attribution path end to end: one
// axis varied, a control everything is subtracted from, and an honest statement
// of what the numbers cannot support.
//
// It needs an interpreter with pytest: the demo dataset settles two of its three
// tasks with a pytest grader, so without one the control passes one task out of
// three and this test reads as a failure of the sweep rather than of the
// machine it is running on.
func TestCLIMatrixAttributesASweep(t *testing.T) {
	requirePytest(t)
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")

	r := runCLI(t, "matrix",
		"--file", matrixExample,
		"--db", db,
		"--sweep-id", "s-demo",
		"--quiet",
		"--report", filepath.Join(work, "sweep.html"),
	)
	if r.exitCode != 0 {
		t.Fatalf("matrix exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	p := payloadFromHTML(t, filepath.Join(work, "sweep.html"))
	sw := p.Sweep
	if sw == nil {
		t.Fatal("the sweep report carries no attribution")
	}
	if sw.Axis != "harness" || sw.Control != "stock" {
		t.Errorf("attribution identity = axis %q control %q, want harness/stock", sw.Axis, sw.Control)
	}
	if len(sw.Levels) != 3 {
		t.Fatalf("attributed %d levels, want 3", len(sw.Levels))
	}
	if sw.DatasetDrift {
		t.Error("every level ran the same dataset, but drift was reported")
	}

	// The orthogonality evidence: each level must say what it varied. Without
	// it a reader cannot tell a level that moved one thing from a level that
	// moved three, and the whole attribution is unreadable.
	control := sw.level(t, "stock")
	if !sw.Levels[control].IsControl {
		t.Error("the control level is not marked as one")
	}
	if len(sw.Levels[control].Changed) != 0 {
		t.Errorf("the control varies nothing, but reports %v", sw.Levels[control].Changed)
	}
	wantChanged := map[string]string{
		"terse-prompt":     "append_system_prompt",
		"scripted-failure": "bin",
	}
	for name, key := range wantChanged {
		i := sw.level(t, name)
		if len(sw.Levels[i].Changed) != 1 || sw.Levels[i].Changed[0] != key {
			t.Errorf("level %s changed %v, want [%s]", name, sw.Levels[i].Changed, key)
		}
	}

	// Every level is a run in its own right, tagged with its coordinates, which
	// is what lets `list --sweep` and a later `matrix --sweep` find them.
	for _, l := range sw.Levels {
		if l.RunID == "" || l.Status != "completed" {
			t.Errorf("level %s = %+v, want a completed run", l.Name, l)
		}
	}

	// Correctness is judged on pass rate and the numbers travel beside it.
	if len(sw.Comparisons) != 2 {
		t.Fatalf("got %d comparisons, want one per non-control level", len(sw.Comparisons))
	}
	for _, cmp := range sw.Comparisons {
		if cmp.Control != "stock" || cmp.Common != 3 {
			t.Errorf("comparison %s vs %s over %d tasks, want stock over 3", cmp.Level, cmp.Control, cmp.Common)
		}
		passIdx := -1
		for i := range cmp.Stats {
			if cmp.Stats[i].Metric == "pass_rate" {
				passIdx = i
			}
		}
		if passIdx < 0 {
			t.Fatalf("comparison for %s has no pass rate stat", cmp.Level)
		}
		pass := cmp.Stats[passIdx]
		if !pass.Primary {
			t.Errorf("pass rate must be marked primary: it is what a change is judged on")
		}
		if pass.LowerIsBetter {
			t.Errorf("pass rate is not lower-is-better")
		}
		if cmp.Level == "scripted-failure" {
			if pass.MeanDelta != -100 || pass.Losses != 3 || pass.Wins != 0 {
				t.Errorf("a level that fails everything = %+v, want a 3-0 loss of 100pp", pass)
			}
			if len(cmp.Broke) != 3 {
				t.Errorf("every task should be listed as broken, got %v", cmp.Broke)
			}
			// Three non-tied pairs cannot reach p < 0.05 however lopsided, and
			// the tool must say so rather than call a 3-0 split significant.
			if pass.P < 0.05 {
				t.Errorf("p = %v on three pairs; the exact test cannot reach 0.05 there", pass.P)
			}
		}
	}

	// The verdicts and the limits of the method are stated, not implied.
	if len(sw.Caveats) == 0 {
		t.Fatal("an attribution with no caveats is an attribution nobody can check")
	}
	joined := strings.Join(sw.Caveats, " ")
	for _, want := range []string{"one axis at a time", "fewer than six non-tied pairs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the caveats do not mention %q:\n%s", want, joined)
		}
	}
	if len(sw.Findings) == 0 {
		t.Error("the attribution produced no findings at all")
	}
	for _, f := range sw.Findings {
		// Every finding carries the correctness result beside it, so a token
		// saving is never read on its own.
		if f.Text == "" {
			t.Errorf("finding %s/%s has no text to render", f.Level, f.Metric)
		}
	}

	// --- attributing is separate from running -------------------------------
	//
	// The attribution is computed from the database, so it must be reproducible
	// without re-running anything. This is also what makes a sweep inspectable
	// after the fact.
	again := runCLI(t, "matrix", "--sweep", "s-demo", "--db", db, "--json")
	if again.exitCode != 0 {
		t.Fatalf("attributing an existing sweep exited %d: %s", again.exitCode, again.stderr)
	}
	var fromJSON sweep
	if err := json.Unmarshal([]byte(again.stdout), &fromJSON); err != nil {
		t.Fatalf("--json did not print an attribution: %v\n%s", err, again.stdout)
	}
	if len(fromJSON.Levels) != len(sw.Levels) || len(fromJSON.Findings) != len(sw.Findings) {
		t.Errorf("the JSON attribution and the report disagree: %d vs %d levels, %d vs %d findings",
			len(fromJSON.Levels), len(sw.Levels), len(fromJSON.Findings), len(sw.Findings))
	}

	// And the sweep's runs are reachable by their coordinates.
	l := runCLI(t, "list", "--db", db, "--sweep", "s-demo")
	if l.exitCode != 0 {
		t.Fatalf("list --sweep exited %d: %s", l.exitCode, l.stderr)
	}
	for _, lvl := range sw.Levels {
		if !strings.Contains(l.stdout, lvl.RunID) {
			t.Errorf("list --sweep does not mention run %s:\n%s", lvl.RunID, l.stdout)
		}
	}
	if !strings.Contains(l.stdout, "harness=stock") {
		t.Errorf("list --sweep should show the factor coordinates:\n%s", l.stdout)
	}
	if other := runCLI(t, "list", "--db", db, "--axis", "model"); strings.Contains(other.stdout, sw.Levels[0].RunID) {
		t.Errorf("list --axis model claimed a harness run:\n%s", other.stdout)
	}
}

// TestCLIMatrixRejectsWhatItCannotAttribute covers the guard rails. A sweep
// that varies two things at once produces a number that looks like a finding
// and is not one, so the refusals are the feature.
func TestCLIMatrixRejectsWhatItCannotAttribute(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")

	write := func(name, body string) string {
		path := filepath.Join(work, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// dataset is absolute so the fixture does not depend on where the temp
	// directory sits relative to the repository.
	head := "dataset: " + demo + "\n"

	cases := []struct {
		name     string
		body     string
		wantWord string
	}{
		{
			name: "a harness level that changes the model",
			body: head + `axis: harness
levels:
  - name: control
  - name: other
    agent:
      model: something-else
`,
			wantWord: "does not own",
		},
		{
			name: "a model level that changes the prompt",
			body: head + `axis: model
agent:
  model: base
levels:
  - name: base
  - name: other
    agent:
      append_system_prompt: try harder
`,
			wantWord: "does not own",
		},
		{
			// The trap this guard exists for: the level itself looks fine, but
			// the configuration it inherits turns slash commands off, which
			// silently discards every --plugin-dir. The sweep would then
			// measure two configurations with no skills and report that skills
			// make no difference.
			name: "a skill level inheriting disabled slash commands",
			body: head + `axis: skill
control: no-skills
agent:
  kind: claude_code
  bin: claude
  isolation:
    disable_slash_commands: true
levels:
  - name: no-skills
  - name: with-skills
    agent:
      plugin_dirs: [` + work + `/skills]
`,
			wantWord: "disable_slash_commands",
		},
		{
			name: "a level that names a field nobody owns",
			body: head + `axis: harness
levels:
  - name: control
  - name: other
    agent:
      not_a_real_key: 1
`,
			wantWord: "not_a_real_key",
		},
		{
			name: "a sweep with one level",
			body: head + `axis: harness
levels:
  - name: only
`,
			wantWord: "at least two levels",
		},
		{
			name: "a control that is not a level",
			body: head + `axis: harness
control: nowhere
levels:
  - name: a
    agent: {append_system_prompt: x}
  - name: b
    agent: {append_system_prompt: y}
`,
			wantWord: "not one of the levels",
		},
		{
			name: "an unknown axis",
			body: head + `axis: temperature
levels:
  - name: a
  - name: b
`,
			wantWord: "unknown axis",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runCLI(t, "matrix", "--file", write("m.yaml", c.body), "--db", db, "--dry-run")
			if r.exitCode != 1 {
				t.Fatalf("exit %d, want 1 (a refusal is an operational failure)\nstdout:\n%s\nstderr:\n%s",
					r.exitCode, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stderr, c.wantWord) {
				t.Errorf("the refusal does not explain itself; want %q in:\n%s", c.wantWord, r.stderr)
			}
		})
	}

	// A dry run must not leave anything behind: it is the cheap way to find out
	// whether a document is well formed.
	if l := runCLI(t, "list", "--db", db); !strings.Contains(l.stdout, "no runs recorded") {
		t.Errorf("a dry run recorded runs:\n%s", l.stdout)
	}
}

// TestCLIMatrixPlansEveryAxis checks that all three factors are expressible,
// which is the design's central claim: harness, model and skill each get a
// sweep, and each sweeps only what it owns.
func TestCLIMatrixPlansEveryAxis(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")

	write := func(name, body string) string {
		path := filepath.Join(work, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	head := "dataset: " + demo + "\n"

	cases := []struct {
		axis  string
		body  string
		level string
	}{
		{
			axis: "harness",
			body: head + `axis: harness
control: stock
levels:
  - name: stock
  - name: terse
    agent:
      append_system_prompt: be brief
`,
			level: "terse",
		},
		{
			axis: "model",
			body: head + `axis: model
control: sonnet
agent:
  kind: claude_code
  bin: claude
  model: claude-sonnet-4
levels:
  - name: sonnet
  - name: haiku
    agent:
      model: claude-haiku-4
`,
			level: "haiku",
		},
		{
			// The skill axis is the one with a trap: --plugin-dir is silently
			// wiped by --disable-slash-commands, so a skill sweep that leaves it
			// on measures nothing and reports that skills do not matter.
			axis: "skill",
			body: head + `axis: skill
control: no-skills
agent:
  kind: claude_code
  bin: claude
  isolation:
    disable_slash_commands: false
levels:
  - name: no-skills
  - name: with-skills
    agent:
      plugin_dirs: [` + work + `/skills]
`,
			level: "with-skills",
		},
	}
	for _, c := range cases {
		t.Run(c.axis, func(t *testing.T) {
			r := runCLI(t, "matrix", "--file", write(c.axis+".yaml", c.body), "--db", db, "--dry-run")
			if r.exitCode != 0 {
				t.Fatalf("planning a %s sweep exited %d:\n%s\n%s", c.axis, r.exitCode, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stdout, "axis   "+c.axis) {
				t.Errorf("the plan does not name the axis it sweeps:\n%s", r.stdout)
			}
			if !strings.Contains(r.stdout, c.level) {
				t.Errorf("the plan does not list level %q:\n%s", c.level, r.stdout)
			}
			// The control is what every other level is subtracted from, so the
			// plan should say which one it is rather than leave it to be
			// inferred from the order.
			if !strings.Contains(r.stdout, "control") {
				t.Errorf("the plan does not name the control:\n%s", r.stdout)
			}
		})
	}
	if l := runCLI(t, "list", "--db", db); !strings.Contains(l.stdout, "no runs recorded") {
		t.Errorf("planning recorded runs:\n%s", l.stdout)
	}
}

// TestExampleMatrixIsValid keeps the checked-in example honest. A reference
// document that no longer loads is worse than none, because it is the thing
// people copy.
func TestExampleMatrixIsValid(t *testing.T) {
	work := t.TempDir()
	r := runCLI(t, "matrix", "--file", matrixExample, "--db", filepath.Join(work, "runs.db"), "--dry-run")
	if r.exitCode != 0 {
		t.Fatalf("examples/matrix-harness.yaml does not plan (%d):\n%s\n%s", r.exitCode, r.stdout, r.stderr)
	}
	// The dataset path in the example is written relative to the example's own
	// directory; if that rule regresses, this is where it shows up.
	if !strings.Contains(r.stdout, filepath.Join("datasets", "demo")) {
		t.Errorf("the example did not resolve the demo dataset:\n%s", r.stdout)
	}
	for _, level := range []string{"stock", "terse-prompt", "scripted-failure"} {
		if !strings.Contains(r.stdout, level) {
			t.Errorf("the example no longer plans level %q:\n%s", level, r.stdout)
		}
	}
}

// TestCLIReportsAFailingRunAsSuccess is the exit-code contract stated as its own
// claim, because it is the one most likely to be "fixed" into nonsense by a
// well-meaning change.
func TestCLIReportsAFailingRunAsSuccess(t *testing.T) {
	requirePytest(t)
	work := t.TempDir()

	r := runCLI(t, "run",
		"--dataset", demo,
		"--agent", "mock", "--bin", filepath.Join(agents, "bad.sh"),
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
	)
	if r.exitCode != 0 {
		t.Fatalf("a run whose tasks failed exited %d, want 0\nstderr:\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stdout, "0 (0.0%)") {
		t.Errorf("the summary should say the run resolved nothing:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "status  completed") {
		t.Errorf("the run itself completed:\n%s", r.stdout)
	}
	// The next step is printed, because a run id nobody can use is a run id
	// nobody will look at.
	if !strings.Contains(r.stdout, "aeval report --run") {
		t.Errorf("the summary does not say how to see the result:\n%s", r.stdout)
	}
}

// TestRunHintsACommandThatWorks takes the "Next:" line literally and runs it.
//
// The hint is a promise, and it used to be a false one: it named the run but not
// the database, so `report` went looking in aeval.db and answered "not found"
// for a run the user had just watched finish. A hint that cannot be pasted is
// worse than no hint, so this test executes what was printed rather than
// checking that a line was printed.
func TestRunHintsACommandThatWorks(t *testing.T) {
	requirePytest(t)
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")

	r := runCLI(t, "run",
		"--dataset", demo,
		"--agent", "mock", "--bin", filepath.Join(agents, "good.sh"),
		"--db", db,
		"--work-root", filepath.Join(work, "work"),
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}

	// A non-default --db has to survive into the hint: without it the reader
	// re-runs the command in the default database and gets "not found".
	if !strings.Contains(r.stdout, "--db "+db) {
		t.Errorf("the hint drops the database it just wrote to:\n%s", r.stdout)
	}

	args := hintArgs(t, r.stdout)
	// -o keeps the artifact in the test's directory rather than beside the
	// database, which is where the bare hint would put it. It does not change
	// whether the run can be found, which is what this test is about.
	full := append(args, "-o", filepath.Join(work, "hint.html"))
	got := runCLI(t, full...)
	if got.exitCode != 0 {
		t.Fatalf("the printed hint failed with %d: aeval %s\nstderr:\n%s",
			got.exitCode, strings.Join(args, " "), got.stderr)
	}
	if _, err := os.Stat(filepath.Join(work, "hint.html")); err != nil {
		t.Errorf("the hint reported success but wrote nothing: %v", err)
	}
}

// hintArgs pulls the command out of a "Next:" line and drops the program name,
// so what comes back can be handed straight back to runCLI.
func hintArgs(t *testing.T, stdout string) []string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Next: aeval ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			t.Fatalf("empty hint: %q", line)
		}
		return fields
	}
	t.Fatalf("no Next: line in:\n%s", stdout)
	return nil
}

// TestCLIExitCodes pins the contract automation depends on. The important one is
// that a run whose tasks all fail is still a *successful command*: an evaluation
// that fails its own process whenever the model does badly cannot be used in CI,
// where a red build has to mean "the harness broke".
func TestCLIExitCodes(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, 2},
		{"version", []string{"version"}, 0},
		{"help", []string{"help"}, 0},
		{"validate a good dataset", []string{"validate", "--dataset", demo}, 0},
		{"list an empty database", []string{"list", "--db", db}, 0},
		{"a missing dataset", []string{"validate", "--dataset", filepath.Join(work, "nope")}, 1},
		{"a missing run", []string{"report", "--db", db, "--run", "nope", "-o", filepath.Join(work, "x.html")}, 1},
		{"an unknown agent", []string{"run", "--dataset", demo, "--agent", "nope", "--db", db}, 1},
		{"run without a dataset", []string{"run", "--db", db}, 2},
		{"matrix without a document", []string{"matrix", "--db", db}, 2},
		{"an unknown flag", []string{"list", "--nope"}, 2},
		{"an unknown command", []string{"frobnicate"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runCLI(t, c.args...).exitCode; got != c.want {
				t.Errorf("exit code = %d, want %d for %v", got, c.want, c.args)
			}
		})
	}
}

// TestRelativeAgentPathResolvesAgainstTheShellDirectory pins a failure mode
// that was both silent and misleading. The agent runs inside the sandbox with
// the task workspace as its working directory, so a --bin written relative to
// the shell was looked for in the wrong tree and surfaced as an opaque
// agent_error, indistinguishable from an agent that crashed. runCLI runs with
// the repository root as its working directory, so the path below is relative
// in exactly the way a user would type it.
func TestRelativeAgentPathResolvesAgainstTheShellDirectory(t *testing.T) {
	requirePytest(t)
	work := t.TempDir()

	r := runCLI(t, "run",
		"--dataset", demo,
		"--agent", "mock", "--bin", filepath.Join("tests", "testdata", "agents", "good.sh"),
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
		"--quiet",
	)
	if r.exitCode != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "passed      3 (100.0%)") {
		t.Errorf("a correct agent named by a relative path should resolve every task:\n%s", r.stdout)
	}
}

// TestCLIVersionNamesTheSchemaAndPayloadVersions makes the two version numbers a
// user needs when something looks wrong discoverable without opening the source.
func TestCLIVersionNamesTheSchemaAndPayloadVersions(t *testing.T) {
	r := runCLI(t, "version")
	if r.exitCode != 0 {
		t.Fatalf("version exited %d: %s", r.exitCode, r.stderr)
	}
	for _, want := range []string{"schema", "payload", "agents", "claude_code", "mock"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("version output does not mention %q:\n%s", want, r.stdout)
		}
	}

	j := runCLI(t, "version", "--json")
	if j.exitCode != 0 {
		t.Fatalf("version --json exited %d: %s", j.exitCode, j.stderr)
	}
	var info struct {
		Version       string   `json:"version"`
		SchemaVersion int      `json:"schema_version"`
		DataVersion   int      `json:"data_version"`
		Agents        []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(j.stdout), &info); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, j.stdout)
	}
	if info.SchemaVersion <= 0 || info.DataVersion <= 0 {
		t.Errorf("version = %+v; both versions must be positive", info)
	}
	if len(info.Agents) < 2 {
		t.Errorf("agents = %v, want at least the Claude Code driver and the mock", info.Agents)
	}
}

// TestCLIReportIndexListsRuns covers the one artifact a person opens first: the
// index. It has to name every run and link to it in a way that works from a
// file:// URL, since that is how an index gets shared.
func TestCLIReportIndexListsRuns(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	good := runAgent(t, work, db, "good", "scripted-index")
	bad := runAgent(t, work, db, "bad", "scripted-index")

	outDir := filepath.Join(work, "site")
	r := runCLI(t, "report", "--db", db, "--index", "--out-dir", outDir)
	if r.exitCode != 0 {
		t.Fatalf("report --index exited %d: %s", r.exitCode, r.stderr)
	}

	indexPath := filepath.Join(outDir, "index.html")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("the index was not written: %v", err)
	}
	for _, id := range []string{good.Run.ID, bad.Run.ID} {
		if !strings.Contains(string(raw), id) {
			t.Errorf("the index does not mention run %s", id)
		}
		// The per-run page must exist under the name the index links to, or
		// every link is broken.
		page := filepath.Join(outDir, reportFileName(id))
		if _, err := os.Stat(page); err != nil {
			t.Errorf("the index links to %s but it was not written: %v", filepath.Base(page), err)
		}
	}

	// The listing is a projection of the database too, so attributing it back
	// has to agree with what the reports say.
	var idx struct {
		Version int `json:"version"`
		Runs    []struct {
			ID       string   `json:"id"`
			Status   string   `json:"status"`
			PassRate *float64 `json:"pass_rate"`
		} `json:"runs"`
	}
	body, err := extractPayload(t, string(raw))
	if err != nil {
		t.Fatalf("the index has no payload: %v", err)
	}
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		t.Fatalf("the index payload does not decode: %v", err)
	}
	if len(idx.Runs) != 2 {
		t.Fatalf("the index lists %d runs, want 2", len(idx.Runs))
	}
	for _, entry := range idx.Runs {
		if entry.PassRate == nil {
			t.Errorf("index entry %s has no pass rate; both runs were fully graded", entry.ID)
			continue
		}
		switch entry.ID {
		case good.Run.ID:
			if *entry.PassRate != 1 {
				t.Errorf("good pass rate = %v, want 1", *entry.PassRate)
			}
		case bad.Run.ID:
			if *entry.PassRate != 0 {
				t.Errorf("bad pass rate = %v, want 0", *entry.PassRate)
			}
		}
	}
}

// reportFileName mirrors report.FileName. It is duplicated rather than imported
// so that a change to the naming convention shows up as a failing link in this
// test rather than being silently agreed with.
func reportFileName(runID string) string {
	return "report-" + runID + ".html"
}

// TestCLIReadsADatabaseFromBeforeTheRecordedAnswer is the version rule that
// matters to anyone who already has runs on disk.
//
// Schema v2 added a column, so the question is what happens to databases
// written before it. The answer has to be "they keep working": the column is
// added on open and reads back empty for rows that predate it, and the report
// says the recording kept no answer rather than failing to render. The other
// answer -- point --db at a fresh file -- asks someone to throw away a run they
// already paid for because a text column was added.
//
// It goes through the CLI rather than the store alone because the column is
// read by the report's projection, so a migration that adds it and a read path
// that forgets it would still pass a store-level test.
func TestCLIReadsADatabaseFromBeforeTheRecordedAnswer(t *testing.T) {
	work := t.TempDir()
	ds := writeBaselineDataset(t, filepath.Join(work, "ds"))
	db := filepath.Join(work, "before-v2.db")

	now := filepath.Join(work, "now.html")
	r := runCLI(t, "run",
		"--dataset", ds,
		"--agent", "mock",
		"--bin", filepath.Join(agents, "good.sh"),
		"--name", "before-v2",
		"--db", db,
		"--work-root", filepath.Join(work, "work"),
		"--report", now,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	fresh := payloadFromHTML(t, now)
	if fresh.Run.ID == "" {
		t.Fatal("the report does not name the run it describes")
	}
	if cell := fresh.taskByID(t, "recorded"); cell.Baseline == nil || cell.Baseline.Output != "the recorded answer" {
		t.Fatalf("a run written by this build should carry the recording's answer, got %+v", cell.Baseline)
	}

	downgradeToV1(t, db)

	// Rendering the same run from the older file has to work, and has to keep
	// every number that was already in it.
	older := filepath.Join(work, "older.html")
	again := runCLI(t, "report", "--run", fresh.Run.ID, "--db", db, "-o", older)
	if again.exitCode != 0 {
		t.Fatalf("report from a pre-v2 database exited %d\nstdout:\n%s\nstderr:\n%s",
			again.exitCode, again.stdout, again.stderr)
	}
	re := payloadFromHTML(t, older)
	cell := re.taskByID(t, "recorded")
	if cell.Baseline == nil {
		t.Fatal("reading the pre-v2 database lost the baseline the run recorded")
	}
	if cell.Baseline.Tokens != 160 {
		t.Errorf("baseline tokens = %d, want the recorded 160 kept through the upgrade", cell.Baseline.Tokens)
	}
	// Empty is the honest reading of a row written before the column existed,
	// and the client renders it as a sentence saying so.
	if cell.Baseline.Output != "" {
		t.Errorf("baseline output = %q, want empty for a row written before v2", cell.Baseline.Output)
	}
}

// downgradeToV1 rewrites a database into the shape v1 had: the recorded-answer
// column gone, the version stamped back.
//
// It removes what v2 added rather than writing out a v1 DDL, so the fixture
// cannot drift away from what v1 actually was.
func downgradeToV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`ALTER TABLE attempts DROP COLUMN baseline_output`,
		`UPDATE schema_meta SET value='1' WHERE key='version'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("rewrite %s into v1 (%s): %v", filepath.Base(path), stmt, err)
		}
	}
}

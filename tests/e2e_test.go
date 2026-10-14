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
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// Skills is what the run's own configuration says it loaded. Absent means
	// the row predates the harness recording its configuration, which the page
	// states rather than rendering as "none".
	Skills *runSkills `json:"skills"`

	// ChainSessions is whether each session's tasks shared one workspace. The
	// page has to be able to say so, because the cells of a chain are one
	// continuation rather than independent samples.
	ChainSessions bool `json:"chain_sessions"`

	// InPlace is whether the run worked in the directories the tasks record
	// rather than in copies. The page has to be able to say so, because a cell
	// measured in someone's working repository is not the same experiment as
	// one measured in a copy with the history stripped.
	InPlace bool `json:"in_place"`

	// Model is which model the agent used, as far as the record goes. The label
	// above is only what the command line asked for, and it is empty on every
	// run that did not pass --model.
	Model *runModel `json:"model"`

	Summary summary `json:"summary"`
}

type runModel struct {
	Declared   string   `json:"declared"`
	Observed   []string `json:"observed"`
	Substitute bool     `json:"substitute"`
	Text       string   `json:"text"`
}

type runSkills struct {
	Dirs      []string `json:"dirs"`
	Active    bool     `json:"active"`
	RemovedBy string   `json:"removed_by"`
	Text      string   `json:"text"`
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

	// Model is which model this cell's own stream named, in the shape the page
	// renders: a statement, never a blank.
	Model string `json:"model"`

	Baseline *struct {
		ID         string `json:"id"`
		Ref        string `json:"ref"`
		Tokens     int    `json:"tokens"`
		TokenDelta int    `json:"token_delta"`
		MsDelta    int64  `json:"ms_delta"`
		Output     string `json:"output"`
		// The recording's provenance. Without these the report cannot say that
		// a replay is a cross-agent comparison rather than a like-for-like one.
		AgentName      string `json:"agent_name"`
		AgentModel     string `json:"agent_model"`
		Source         string `json:"source"`
		DifferentAgent bool   `json:"different_agent"`
		Notes          string `json:"notes"`
	} `json:"baseline"`

	Error string `json:"error"`
	// WorkspaceDir and RestoreNotes are what an in-place cell records about the
	// directory it worked in and what happened to it afterwards.
	WorkspaceDir string `json:"workspace_dir"`
	RestoreNotes string `json:"restore_notes"`

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
	// The two runs differ in their model and in their binary, so every delta
	// above is a bundle. Saying nothing would read as "these two numbers
	// measured the same thing", which is the one reading that is certainly
	// false -- so the comparison has to name what it bundled.
	bundle := strings.Join(delta.Warnings, "\n")
	for _, want := range []string{
		"differ in more than one thing",
		"the model (scripted-good vs scripted-bad)",
		"the agent binary",
	} {
		if !strings.Contains(bundle, want) {
			t.Errorf("the comparison should say %q, got:\n%s", want, bundle)
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

// TestCLIRecordsWhichModelActuallyRan covers what the page can say about the
// model, which is more than the label column knows.
//
// `--model` is optional, so runs.agent_model is empty on every run that did not
// pass it -- and an empty label used to render as nothing at all, leaving a
// reader to choose between "no model was requested", "the CLI's default was
// used" and "nobody recorded it". The stream is the only record that answers
// it, and this is the level at which "the model on the page is the model the
// stream named" can be observed at all.
//
// The default agent is driven rather than the mock, and that is not incidental:
// the mock synthesizes its own stream and names the model it was told to use, so
// it can never answer this question. Only the real agent path parses a stream a
// fixture wrote.
func TestCLIRecordsWhichModelActuallyRan(t *testing.T) {
	work := t.TempDir()
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds"))
	// Only this fixture emits a stream that names a model.
	naming := filepath.Join(agents, "chain.sh")
	silent := filepath.Join(agents, "good.sh")

	run := func(name, bin string, extra ...string) payload {
		t.Helper()
		report := filepath.Join(work, name+".html")
		args := append([]string{"run",
			"--dataset", ds,
			"--bin", bin,
			"--name", name,
			"--db", filepath.Join(work, "runs.db"),
			"--work-root", filepath.Join(work, "work-"+name),
			"--report", report,
		}, extra...)
		r := runCLI(t, args...)
		if r.exitCode != 0 {
			t.Fatalf("%s run exited %d\nstdout:\n%s\nstderr:\n%s", name, r.exitCode, r.stdout, r.stderr)
		}
		return payloadFromHTML(t, report)
	}

	// The ordinary case, and the one the label cannot describe: nobody passed
	// --model, the CLI used its own default, and only the stream says which.
	def := run("default", naming)
	if def.Run.Model == nil {
		t.Fatal("the report has no model view, so the page cannot state one")
	}
	if def.Run.Model.Text != "default (the CLI chose it): stand-in" {
		t.Errorf("model text = %q, want the model the stream named", def.Run.Model.Text)
	}
	if def.Run.AgentModel != "" {
		t.Errorf("agent_model = %q, want the empty label this is here to make readable", def.Run.AgentModel)
	}
	if len(def.Tasks) != 1 || def.Tasks[0].Model != "stand-in" {
		t.Errorf("the cell should name its own model, got %+v", def.Tasks)
	}

	// Asked for the model that answered. The stream's name is not a difference,
	// and the page must not imply one.
	asked := run("asked", naming, "--model", "stand-in")
	if asked.Run.Model.Substitute {
		t.Errorf("the run got what it asked for: %q", asked.Run.Model.Text)
	}
	if asked.Run.Model.Text != "stand-in" {
		t.Errorf("model text = %q, want the requested model once", asked.Run.Model.Text)
	}

	// Asked for something else and served this. Every number on the page
	// measures a model the run did not choose, and this is the only place that
	// can say so.
	other := run("other", naming, "--model", "some-other-model")
	if !other.Run.Model.Substitute {
		t.Fatalf("a run served a model other than the one it asked for: %q", other.Run.Model.Text)
	}
	if !strings.Contains(other.Run.Model.Text, "some-other-model") ||
		!strings.Contains(other.Run.Model.Text, "stand-in") {
		t.Errorf("the substitution should name both models, got %q", other.Run.Model.Text)
	}

	// Nothing named a model and nothing was asked for. The absence is stated
	// rather than left as a gap where the chip should be.
	quiet := run("quiet", silent)
	if quiet.Run.Model.Text != "not recorded" {
		t.Errorf("model text = %q, want the absence stated", quiet.Run.Model.Text)
	}
	if quiet.Run.Model.Substitute {
		t.Error("nothing was asked for, so nothing could have been substituted")
	}
	if len(quiet.Tasks) != 1 || quiet.Tasks[0].Model != "not named by the stream" {
		t.Errorf("a cell whose stream named nothing must say so: %+v", quiet.Tasks)
	}
}

// TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered covers a spelling that is
// natural to write, is what --help invites, and used to fail.
//
// Go's flag package stops at the first non-flag word, so everything after
// `--compare a b` sits unparsed until the command re-parses from that word. The
// store used to be opened before that re-parse, so `--db` arrived after the file
// had already been opened at its default path: the runs were looked for in
// aeval.db and reported missing. Writing `--db` first worked -- and that is the
// shape every other test in this file uses, which is exactly why nothing
// noticed. The failure was not silent, but it was also not the user's mistake,
// so the flag order must not be load-bearing.
func TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered(t *testing.T) {
	work := t.TempDir()
	ds := writeChainedTurnDataset(t, filepath.Join(work, "ds"))
	db := filepath.Join(work, "runs.db")

	run := func(name string, extra ...string) string {
		t.Helper()
		report := filepath.Join(work, name+".html")
		args := append([]string{"run",
			"--dataset", ds,
			"--agent", "mock",
			"--bin", filepath.Join(agents, "chain.sh"),
			"--name", name,
			"--db", db,
			"--work-root", filepath.Join(work, "work-"+name),
			"--report", report,
		}, extra...)
		r := runCLI(t, args...)
		if r.exitCode != 0 {
			t.Fatalf("%s run exited %d\nstdout:\n%s\nstderr:\n%s", name, r.exitCode, r.stdout, r.stderr)
		}
		return payloadFromHTML(t, report).Run.ID
	}
	idPlain := run("plain")
	idChained := run("chained", "--chain-sessions")

	// The order that used to fail: the database comes after the run ids.
	after := filepath.Join(work, "after.html")
	r := runCLI(t, "report", "--compare", idPlain, idChained, "--db", db, "-o", after)
	if r.exitCode != 0 {
		t.Fatalf("--db after the positional run ids failed (%d): %s", r.exitCode, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("a comparison over a database that holds both runs should say nothing to stderr:\n%s", r.stderr)
	}

	// The order that always worked, both so the two are pinned as equivalent
	// and so a fix for one cannot quietly break the other.
	before := filepath.Join(work, "before.html")
	r = runCLI(t, "report", "--db", db, "--compare", idPlain, idChained, "-o", before)
	if r.exitCode != 0 {
		t.Fatalf("--db before the positional run ids failed (%d): %s", r.exitCode, r.stderr)
	}
	if !samePayload(t, payloadFromHTML(t, after), payloadFromHTML(t, before)) {
		t.Error("the same comparison rendered differently depending on where --db was written")
	}

	// And a comparison that really is looking in the wrong file has to name
	// that file: an error reading "not found in aeval.db" while the caller typed
	// another path is the diagnostic that points at --db, and it was unavailable
	// while the store was opened from a half-parsed command line.
	empty := filepath.Join(work, "empty.db")
	r = runCLI(t, "report", "--compare", idPlain, idChained, "--db", empty, "-o", filepath.Join(work, "x.html"))
	if r.exitCode == 0 {
		t.Fatal("comparing runs in a database that does not hold them must fail")
	}
	if !strings.Contains(r.stderr, "not found in "+empty) {
		t.Errorf("the failure must name the database it looked in, got:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, "aeval.db") {
		t.Errorf("the failure named the default database rather than the one that was passed:\n%s", r.stderr)
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

	// The fixture session holds two prompts and the default split collects the
	// opening request, so half of it is not in the dataset. Nothing in a
	// one-task collection says that on its own -- the reader who assumed it
	// covered the session would take one question for two -- so collect
	// declares it, on stderr and on the baseline that travels with the task.
	for _, want := range []string{"hold more prompts than this dataset collected", "(1 of 2)", "--split turn"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("collect should say %q, got:\n%s", want, r.stderr)
		}
	}
	base, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(collected.Baseline.File)))
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}
	var baseline struct {
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal(base, &baseline); err != nil {
		t.Fatalf("the baseline does not decode: %v", err)
	}
	for _, want := range []string{"holds 2 prompts", "collected the first one only", "the other 1 are not in it"} {
		if !strings.Contains(baseline.Notes, want) {
			t.Errorf("the baseline should say %q, got:\n%s", want, baseline.Notes)
		}
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

	// The baseline is the point of collecting: what the recorded attempt spent
	// on *this* request is what the replay is measured against. The fixture
	// session holds two prompts, and only the first is the task, so the
	// follow-up's turn is not charged to it.
	cell := p.taskByID(t, "aaaabbbb")
	if cell.Baseline == nil {
		t.Fatal("the collected task lost its baseline")
	}
	if cell.Baseline.Tokens != 280 {
		t.Errorf("baseline tokens = %d, want the opening request's 280", cell.Baseline.Tokens)
	}
	if cell.Baseline.TokenDelta != -280 {
		t.Errorf("token delta = %d, want -280 (the replay spent nothing)", cell.Baseline.TokenDelta)
	}
	// The recording's own answer travels with its numbers. A replay that spent
	// less and concluded something else is the comparison a reader is after,
	// and the spend alone cannot express it. "好的。" is the answer to the
	// follow-up, which this task never asks.
	if !strings.Contains(cell.Baseline.Output, "已修复，减号写反了。") {
		t.Errorf("baseline output = %q, want the recorded answer to the opening request", cell.Baseline.Output)
	}
	if p.Run.Summary.BaselineCells != 1 {
		t.Errorf("summary counted %d baseline cells, want 1", p.Run.Summary.BaselineCells)
	}
	if _, ok := metricByKey(p, "baseline_tokens"); !ok {
		t.Error("a run with a baseline should headline its token delta, not bury it in the task")
	}
}

// TestCLIRefusesToReplayOnAnotherMachine moves a collected dataset to "another
// machine" by rewriting the identity its baseline carries, which is the only
// part of a move the harness can observe.
func TestCLIRefusesToReplayOnAnotherMachine(t *testing.T) {
	work := t.TempDir()
	out := filepath.Join(work, "collected")
	db := filepath.Join(work, "runs.db")
	session := filepath.Join(repoRoot, "tests", "testdata", "session", "basic.jsonl")
	if r := runCLI(t, "collect", "--session", session, "--out", out); r.exitCode != 0 {
		t.Fatalf("collect exited %d\n%s", r.exitCode, r.stderr)
	}

	basePath := filepath.Join(out, "baselines", "aaaabbbb.json")
	raw, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	m, ok := doc["machine"].(map[string]any)
	if !ok || m["id"] == "" {
		t.Fatalf("collect wrote no machine identity onto the baseline:\n%s", raw)
	}

	replay := func(name string) result {
		return runCLI(t, "run", "--dataset", out, "--agent", "mock", "--bin", "/bin/true",
			"--name", name, "--db", db, "--work-root", filepath.Join(work, "work"), "--quiet")
	}
	// On the machine that collected it, the replay runs.
	if r := replay("here"); r.exitCode != 0 {
		t.Fatalf("the collecting machine could not replay its own dataset (%d):\n%s\n%s",
			r.exitCode, r.stdout, r.stderr)
	}

	m["id"] = "0000000000000000000000000000beef"
	m["hostname"] = "elsewhere"
	moved, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(basePath, moved, 0o644); err != nil {
		t.Fatal(err)
	}
	r := replay("there")
	if r.exitCode != 2 {
		t.Fatalf("a replay on another machine exited %d, want 2\nstdout:\n%s\nstderr:\n%s",
			r.exitCode, r.stdout, r.stderr)
	}
	for _, want := range []string{"different machine", "aaaabbbb", "recorded on: elsewhere", "running on:"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the refusal should say %q, got:\n%s", want, r.stderr)
		}
	}
	// Refused before anything ran: only the first replay is in the database.
	if l := runCLI(t, "list", "--db", db); strings.Contains(l.stdout, "there") {
		t.Errorf("the refused replay left a run behind:\n%s", l.stdout)
	}
}

// TestCLICollectAppendsToAnExistingDataset collects into the same --out twice,
// the way a user grows a dataset from a working directory over time.
func TestCLICollectAppendsToAnExistingDataset(t *testing.T) {
	out := filepath.Join(t.TempDir(), "grown")
	session := filepath.Join(repoRoot, "tests", "testdata", "session", "basic.jsonl")
	tasks := filepath.Join(out, "tasks.jsonl")

	if r := runCLI(t, "collect", "--session", session, "--out", out); r.exitCode != 0 {
		t.Fatalf("first collect exited %d\n%s", r.exitCode, r.stderr)
	}

	// Per-turn tasks from the same session: the first prompt is already there.
	dry := runCLI(t, "collect", "--session", session, "--split", "turn", "--out", out, "--dry-run")
	if dry.exitCode != 0 {
		t.Fatalf("--dry-run exited %d\n%s", dry.exitCode, dry.stderr)
	}
	if !strings.Contains(dry.stdout, "would append 1 task(s)") || !strings.Contains(dry.stdout, "skipped 1 already collected: aaaabbbb-p0") {
		t.Errorf("--dry-run should describe the append, got:\n%s", dry.stdout)
	}
	if got := taskIDsIn(t, tasks); strings.Join(got, ",") != "aaaabbbb" {
		t.Errorf("--dry-run changed the dataset: %v", got)
	}

	r := runCLI(t, "collect", "--session", session, "--split", "turn", "--out", out)
	if r.exitCode != 0 {
		t.Fatalf("append exited %d\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stdout, "appended 1 task(s)") {
		t.Errorf("append should say what it did, got:\n%s", r.stdout)
	}
	if got := taskIDsIn(t, tasks); strings.Join(got, ",") != "aaaabbbb,aaaabbbb-p1" {
		t.Errorf("tasks after append = %v, want the original kept and the follow-up added", got)
	}
	if v := runCLI(t, "validate", "--dataset", out); v.exitCode != 0 {
		t.Fatalf("the appended dataset does not validate (%d):\n%s\n%s", v.exitCode, v.stdout, v.stderr)
	}

	// Running the same collection again is a no-op, not an error.
	again := runCLI(t, "collect", "--session", session, "--split", "turn", "--out", out)
	if again.exitCode != 0 || !strings.Contains(again.stdout, "appended 0 task(s)") {
		t.Errorf("re-collecting should add nothing (exit %d):\n%s", again.exitCode, again.stdout)
	}

	// --overwrite is the old behaviour: the dataset is exactly this collection.
	ow := runCLI(t, "collect", "--session", session, "--split", "turn", "--out", out, "--overwrite")
	if ow.exitCode != 0 {
		t.Fatalf("--overwrite exited %d\n%s", ow.exitCode, ow.stderr)
	}
	if got := taskIDsIn(t, tasks); strings.Join(got, ",") != "aaaabbbb-p0,aaaabbbb-p1" {
		t.Errorf("tasks after --overwrite = %v, want only this collection", got)
	}
}

// ---------------------------------------------------------------------------
// collect --source cursor
// ---------------------------------------------------------------------------

// TestCLICollectsACursorConversation drives the Cursor path the way a user
// would: point at a state database, ask what is in it, collect one
// conversation, and replay it.
//
// The fixture is synthesised rather than taken from the Cursor installation on
// this machine. A copied database would pin the reader to one Cursor version
// and one person's private chat history; a synthesised one states exactly which
// shapes collect claims to understand.
func TestCLICollectsACursorConversation(t *testing.T) {
	work := t.TempDir()
	cursorDB := filepath.Join(work, "state.vscdb")
	writeCursorFixture(t, cursorDB)

	const (
		full     = "aaaaaaaa-1111-2222-3333-444444444444"
		second   = "bbbbbbbb-1111-2222-3333-444444444444"
		noPrompt = "cccccccc-1111-2222-3333-444444444444"
	)

	// --list answers "what is in there" and needs no --out: looking is the whole
	// point of it.
	list := runCLI(t, "collect", "--source", "cursor", "--cursor-db", cursorDB, "--list")
	if list.exitCode != 0 {
		t.Fatalf("--list exited %d\nstderr:\n%s", list.exitCode, list.stderr)
	}
	if !strings.Contains(list.stdout, "3 cursor conversation(s)") {
		t.Errorf("--list should count what the database holds, got:\n%s", list.stdout)
	}
	row := cursorListRow(t, list.stdout, full)
	if row[3] != "5" {
		t.Errorf("--list msgs = %s, want the conversation's 5 bubbles: %v", row[3], row)
	}
	// The prompt count is what tells a reader whether a conversation is
	// collectable at all.
	if row[4] != "1" {
		t.Errorf("--list prompts = %s, want the conversation's 1 human turn: %v", row[4], row)
	}
	if got := cursorListRow(t, list.stdout, noPrompt)[4]; got != "0" {
		t.Errorf("a draft with no request should list 0 prompts, got %s", got)
	}

	// The two flags are not interchangeable, and saying so is cheaper than a
	// collection that silently read the wrong history.
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"cursor-db without --source cursor", []string{"--cursor-db", cursorDB, "--out", filepath.Join(work, "x")},
			"--cursor-db only means something with --source cursor"},
		{"--root with --source cursor", []string{"--source", "cursor", "--cursor-db", cursorDB, "--root", work, "--out", filepath.Join(work, "x")},
			"--root only means something with --source claude"},
		{"an unknown source", []string{"--source", "vscode", "--out", filepath.Join(work, "x")},
			`--source must be "claude" or "cursor"`},
		{"neither --out nor --dry-run", []string{"--source", "cursor", "--cursor-db", cursorDB, "--latest"},
			"--out is required"},
		{"no way to name a conversation", []string{"--source", "cursor", "--cursor-db", cursorDB, "--out", filepath.Join(work, "x")},
			"name what to collect"},
	} {
		r := runCLI(t, append([]string{"collect"}, tc.args...)...)
		if r.exitCode != 2 {
			t.Errorf("%s: exit %d, want the usage code 2\nstderr:\n%s", tc.name, r.exitCode, r.stderr)
		}
		if !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%s: stderr should say %q, got:\n%s", tc.name, tc.want, r.stderr)
		}
	}

	// --- one conversation, selected by id prefix -------------------------
	out := filepath.Join(work, "from-cursor")
	r := runCLI(t, "collect",
		"--source", "cursor",
		"--cursor-db", cursorDB,
		"--session", "aaaaaaaa",
		"--out", out,
		"--name", "cursor-demo",
	)
	if r.exitCode != 0 {
		t.Fatalf("collect --source cursor exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	for _, want := range []string{"dataset.yaml", "tasks.jsonl", filepath.Join("baselines", "aaaaaaaa.json")} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("collect did not write %s: %v", want, err)
		}
	}
	// The manifest names the capture. "from recorded Claude Code sessions" was
	// true only while there was one source.
	manifest, err := os.ReadFile(filepath.Join(out, "dataset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "Cursor") {
		t.Errorf("the manifest should name the source it was collected from, got:\n%s", manifest)
	}

	task := readOneTask(t, out)
	if task.ID != "aaaaaaaa" {
		t.Errorf("task id = %q, want the conversation's 8-character prefix", task.ID)
	}
	if task.Prompt != "add a slugify helper" {
		t.Errorf("task prompt = %q, want the first human turn verbatim", task.Prompt)
	}
	if task.Output == nil || task.Output.Type != "stdout" {
		t.Errorf("a Cursor task has no workspace, so it expects an answer on stdout: %+v", task.Output)
	}
	// A transcript carries no ground truth. Emitting a grader would invent one.
	if len(task.Graders) != 0 {
		t.Errorf("a collected cursor task got graders: %+v", task.Graders)
	}
	if task.Baseline == nil || task.Baseline.File != "baselines/aaaaaaaa.json" {
		t.Errorf("task baseline = %+v, want the sibling baseline document", task.Baseline)
	}
	// The budget is derived from the recording, so a conversation that took a
	// minute of real work does not get a toy's 30-second timeout: 60s of busy
	// time triples to 180s, which the floor lifts to 300s, and four assistant
	// bubbles give a turn budget of 8, which the floor lifts to 20.
	if task.Limits == nil || task.Limits.TimeoutS != 300 || task.Limits.MaxTurns != 20 {
		t.Errorf("limits = %+v, want {timeout_s:300 max_turns:20}", task.Limits)
	}
	for key, want := range map[string]string{
		"source":     "cursor-composer",
		"session_id": full,
		"cwd":        "/Users/x/lab/alpha",
	} {
		if got, _ := task.Metadata[key].(string); got != want {
			t.Errorf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
	if got, _ := task.Metadata["baseline_tokens"].(float64); got != 3000 {
		t.Errorf("metadata[baseline_tokens] = %v, want the recorded 3000", task.Metadata["baseline_tokens"])
	}

	// The baseline document is where the recording's caveats have to survive,
	// because they describe what the numbers beside them do and do not mean.
	base := readBaseline(t, out, "aaaaaaaa")
	if base.Source != "cursor-composer" {
		t.Errorf("baseline source = %q, want the capture named", base.Source)
	}
	if base.AgentName != "cursor" {
		t.Errorf("baseline agent_name = %q, want the agent the recording actually drove", base.AgentName)
	}
	if base.AgentModel != "claude-opus-4-7" {
		t.Errorf("baseline agent_model = %q", base.AgentModel)
	}
	if base.Usage.InputTokens != 2600 || base.Usage.OutputTokens != 400 {
		t.Errorf("baseline usage = %+v, want the recorded 2600/400", base.Usage)
	}
	if base.FinalOutput != "Done." {
		t.Errorf("baseline final_output = %q, want the recording's last answer", base.FinalOutput)
	}
	// Busy time is the gaps that end at an assistant bubble -- 10s + 20s + 20s
	// + 10s -- and not the 360s the conversation reports as its span, which
	// runs on past its last message.
	if base.BusyMs != 60000 {
		t.Errorf("baseline busy_ms = %d, want 60000; the recorded span was %d", base.BusyMs, base.DurationMs)
	}
	if base.DurationMs != 360000 {
		t.Errorf("baseline duration_ms = %d, want the conversation's 360000 span", base.DurationMs)
	}
	var roles []string
	for _, tn := range base.Turns {
		roles = append(roles, tn.Role)
	}
	if got, want := strings.Join(roles, ","), "user,assistant,assistant,assistant,assistant"; got != want {
		t.Errorf("baseline turns = %s, want %s", got, want)
	}
	if len(base.EditedFiles) != 1 || !strings.HasSuffix(base.EditedFiles[0], "slugify.py") {
		t.Errorf("edited files = %v, want the one file the recording wrote", base.EditedFiles)
	}
	for _, want := range []string{"no cost", "finer unit"} {
		if !strings.Contains(base.Notes, want) {
			t.Errorf("baseline notes should mention %q, got %q", want, base.Notes)
		}
	}
	// A fully counted conversation must not be labelled a lower bound, or the
	// caveat stops meaning anything.
	if strings.Contains(base.Notes, "lower bound") {
		t.Errorf("a fully counted baseline should not claim a lower bound, got %q", base.Notes)
	}

	// The output is an ordinary dataset, so the ordinary validator must accept
	// it -- otherwise collect produces something only it can read.
	if v := runCLI(t, "validate", "--dataset", out); v.exitCode != 0 {
		t.Fatalf("the collected cursor dataset does not validate (%d):\n%s\n%s", v.exitCode, v.stdout, v.stderr)
	}

	// Replaying it measures a fresh attempt against the recording.
	reportPath := filepath.Join(work, "cursor.html")
	replay := runCLI(t, "run",
		"--dataset", out,
		"--agent", "mock",
		"--bin", "/bin/true",
		"--model", "replay",
		"--name", "cursor-replay",
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", reportPath,
	)
	if replay.exitCode != 0 {
		t.Fatalf("replaying the cursor dataset exited %d\nstdout:\n%s\nstderr:\n%s",
			replay.exitCode, replay.stdout, replay.stderr)
	}
	p := payloadFromHTML(t, reportPath)
	cell := p.taskByID(t, "aaaaaaaa")
	if cell.Baseline == nil {
		t.Fatal("the cursor task lost its baseline in the report")
	}
	if cell.Baseline.Tokens != 3000 {
		t.Errorf("baseline tokens = %d, want the recorded 2600+400", cell.Baseline.Tokens)
	}
	if cell.Baseline.TokenDelta != -3000 {
		t.Errorf("token delta = %d, want -3000 (the replay spent nothing)", cell.Baseline.TokenDelta)
	}
	if !strings.Contains(cell.Baseline.Output, "Done.") {
		t.Errorf("baseline output = %q, want the recording's answer", cell.Baseline.Output)
	}
	// The recording's identity has to survive the whole chain -- collect,
	// dataset, runner, database, report -- because the report is a function of
	// the database and never sees the baseline document beside the dataset.
	// Without it, a Cursor session replayed by another agent renders as a
	// like-for-like comparison, which is the one thing it is not.
	if cell.Baseline.AgentName != "cursor" || cell.Baseline.Source != "cursor-composer" {
		t.Errorf("the report does not name the recording: agent_name=%q source=%q",
			cell.Baseline.AgentName, cell.Baseline.Source)
	}
	if !cell.Baseline.DifferentAgent {
		t.Errorf("a cursor recording replayed by a %s run must be flagged cross-agent",
			p.Run.AgentKind)
	}
	// The capture's own caveats replace the framework's generic sentence about
	// busy time, rather than sitting beside it: two caveat rows in one card is
	// how both stop being read.
	if !strings.Contains(cell.Baseline.Notes, "no cost") {
		t.Errorf("the card should carry the recording's own caveats, got %q", cell.Baseline.Notes)
	}

	// --- a whole working directory, with a draft dropped -------------------
	byCwd := filepath.Join(work, "by-cwd")
	all := runCLI(t, "collect",
		"--source", "cursor",
		"--cursor-db", cursorDB,
		"--cwd", "/Users/x/lab/alpha",
		"--out", byCwd,
	)
	if all.exitCode != 0 {
		t.Fatalf("collect --cwd exited %d\nstdout:\n%s\nstderr:\n%s", all.exitCode, all.stdout, all.stderr)
	}
	// A conversation with no human prompt is not a task. Dropping it with a
	// count is what keeps one draft from aborting a whole collection.
	if !strings.Contains(all.stderr, "skipped 1 conversation(s) with no human prompt") {
		t.Errorf("dropping a prompt-less conversation should be reported, got stderr:\n%s", all.stderr)
	}
	if got := taskIDsIn(t, filepath.Join(byCwd, "tasks.jsonl")); strings.Join(got, ",") != "aaaaaaaa,bbbbbbbb" {
		t.Errorf("collected tasks = %v, want the two conversations that hold a request", got)
	}
	// The second conversation has two prompts, so its task is the first prompt
	// and the turns answering it -- and none of those turns carries a token
	// count. Both facts have to be stated: a lower bound presented as a
	// measurement, or a slice presented as the session, would each be a number
	// the recording does not support.
	second2 := readBaseline(t, byCwd, "bbbbbbbb")
	if !strings.Contains(second2.Notes, "lower bound") {
		t.Errorf("a baseline with no token counts should say so, got %q", second2.Notes)
	}
	if !strings.Contains(second2.Notes, "per-turn baseline inside a longer session") {
		t.Errorf("a sliced baseline should say it is a slice, got %q", second2.Notes)
	}
	// The slice is the baseline: two turns answered the first prompt, and the
	// two that answered the follow-up are not this task's cost.
	if len(second2.Turns) != 2 {
		t.Errorf("sliced baseline has %d turns, want the 2 that answered the first prompt", len(second2.Turns))
	}
	if b := readBaseline(t, byCwd, "aaaaaaaa"); strings.Contains(b.Notes, "lower bound") {
		t.Errorf("a fully counted baseline should not claim a lower bound, got %q", b.Notes)
	}

	// --- --latest ---------------------------------------------------------
	latestDir := filepath.Join(work, "latest")
	latest := runCLI(t, "collect", "--source", "cursor", "--cursor-db", cursorDB, "--latest",
		"--out", latestDir, "--dry-run")
	if latest.exitCode != 0 {
		t.Fatalf("--latest exited %d\nstderr:\n%s", latest.exitCode, latest.stderr)
	}
	if !strings.Contains(latest.stdout, "would write 1 task(s) from 1 session(s)") {
		t.Errorf("--latest should pick exactly one conversation, got:\n%s", latest.stdout)
	}
	if !strings.Contains(latest.stdout, "aaaaaaaa") {
		t.Errorf("--latest should pick the most recently updated conversation, got:\n%s", latest.stdout)
	}
	if _, err := os.Stat(latestDir); err == nil {
		t.Error("--dry-run wrote the dataset it said it would not write")
	}
}

// cursorListRow finds the --list row for one conversation and returns its
// columns: id, touched, mode, msgs, prompts, name.
func cursorListRow(t *testing.T, out, id string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[0] == id {
			return fields
		}
	}
	t.Fatalf("no --list row for %s in:\n%s", id, out)
	return nil
}

// cursorTask is the slice of a collected task these tests care about.
type cursorTask struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Output *struct {
		Type string `json:"type"`
	} `json:"output"`
	Limits *struct {
		TimeoutS int `json:"timeout_s"`
		MaxTurns int `json:"max_turns"`
	} `json:"limits"`
	Baseline *struct {
		File string `json:"file"`
	} `json:"baseline"`
	Graders  []any          `json:"graders"`
	Metadata map[string]any `json:"metadata"`
}

func readOneTask(t *testing.T, dir string) cursorTask {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "tasks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one collected task, got %d", len(lines))
	}
	var tk cursorTask
	if err := json.Unmarshal([]byte(lines[0]), &tk); err != nil {
		t.Fatalf("the collected task does not decode: %v\n%s", err, lines[0])
	}
	return tk
}

func taskIDsIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var tk struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &tk); err != nil {
			t.Fatalf("decode task: %v\n%s", err, line)
		}
		ids = append(ids, tk.ID)
	}
	return ids
}

// cursorBaseline is the slice of a baseline document these tests care about.
type cursorBaseline struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	AgentName   string `json:"agent_name"`
	AgentModel  string `json:"agent_model"`
	Cwd         string `json:"cwd"`
	FinalOutput string `json:"final_output"`
	Notes       string `json:"notes"`
	BusyMs      int64  `json:"busy_ms"`
	DurationMs  int64  `json:"duration_ms"`
	Usage       struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Turns []struct {
		Role string `json:"role"`
	} `json:"turns"`
	EditedFiles []string `json:"edited_files"`
}

func readBaseline(t *testing.T, dir, id string) cursorBaseline {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "baselines", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var b cursorBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("the baseline does not decode: %v\n%s", err, raw)
	}
	return b
}

// writeCursorFixture builds a state.vscdb with three conversations.
//
// The bubbles are laid out so the derived numbers are checkable by hand. The
// first conversation's four gaps run 10s, 20s, 20s and 10s, so 60s ends at an
// assistant bubble and is counted as machine time; its lastUpdatedAt sits five
// minutes past its final message, so its wall clock is 360s. The two figures
// are kept apart on purpose: one is an estimate from the gaps, the other is
// what the conversation says its span was.
func writeCursorFixture(t *testing.T, path string) {
	t.Helper()
	const cwd = "/Users/x/lab/alpha"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open cursor fixture: %v", err)
	}
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, workspaceId TEXT,
		 createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER,
		 recency INTEGER, checkpointAt INTEGER, value TEXT, subagentTypeName TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create cursor fixture: %v", err)
		}
	}
	put := func(key, value string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}

	// aaaaaaaa -- one prompt, and a trajectory of a read, an edit and an
	// answer. One prompt is what makes the whole conversation the baseline of
	// the task rather than a slice of it.
	// Newest, so it is what --latest picks.
	const a = "aaaaaaaa-1111-2222-3333-444444444444"
	put("composerData:"+a, cursorComposer(t, a, cwd, "claude-opus-4-7", 1767225960000,
		"user:u1", "assistant:a1", "assistant:a2", "assistant:a3", "assistant:a4"))
	put("bubbleId:"+a+":u1", cursorBubble(t, "u1", false, "2026-01-01T00:00:00Z",
		map[string]any{"text": "add a slugify helper"}))
	put("bubbleId:"+a+":a1", cursorBubble(t, "a1", true, "2026-01-01T00:00:10Z",
		map[string]any{
			"text":       "I will look at the repo first.",
			"tokenCount": map[string]any{"inputTokens": 300, "outputTokens": 50},
		}))
	put("bubbleId:"+a+":a2", cursorBubble(t, "a2", true, "2026-01-01T00:00:30Z",
		map[string]any{
			"toolFormerData": map[string]any{
				"toolCallId": "call-1", "name": "read_file_v2", "status": "completed",
				"params": `{"targetFile":"/Users/x/lab/alpha/app.py"}`,
				"result": `{"contents":"..."}`,
			},
			"tokenCount": map[string]any{"inputTokens": 500, "outputTokens": 20},
		}))
	// edit_file_v2 fills neither params nor rawArgs, so its target only exists
	// in codeBlocks -- which is what the reader falls back to.
	put("bubbleId:"+a+":a3", cursorBubble(t, "a3", true, "2026-01-01T00:00:50Z",
		map[string]any{
			"toolFormerData": map[string]any{
				"toolCallId": "call-2", "name": "edit_file_v2", "status": "completed",
				"result": `{"success":true}`,
			},
			"codeBlocks": []map[string]any{{
				"uri":     map[string]any{"path": cwd + "/slugify.py", "scheme": "file"},
				"content": "def slugify(s): ...",
			}},
			"tokenCount": map[string]any{"inputTokens": 600, "outputTokens": 30},
		}))
	put("bubbleId:"+a+":a4", cursorBubble(t, "a4", true, "2026-01-01T00:01:00Z",
		map[string]any{
			"text":       "Done.",
			"tokenCount": map[string]any{"inputTokens": 1200, "outputTokens": 300},
		}))

	// bbbbbbbb -- two prompts, and no token count anywhere. The baseline of a
	// task is the turns that answer *its* prompt, so this one is sliced at the
	// follow-up and its zero total is a lower bound rather than a measurement.
	const b = "bbbbbbbb-1111-2222-3333-444444444444"
	put("composerData:"+b, cursorComposer(t, b, cwd, "grok-4.5", 1767225000000,
		"user:u1", "assistant:a1", "user:u2", "assistant:a2"))
	put("bubbleId:"+b+":u1", cursorBubble(t, "u1", false, "2026-01-01T00:00:00Z",
		map[string]any{"text": "为什么这个测试会失败"}))
	put("bubbleId:"+b+":a1", cursorBubble(t, "a1", true, "2026-01-01T00:00:30Z",
		map[string]any{"text": "因为夹具没有初始化。"}))
	put("bubbleId:"+b+":u2", cursorBubble(t, "u2", false, "2026-01-01T00:01:00Z",
		map[string]any{"text": "那怎么办"}))
	put("bubbleId:"+b+":a2", cursorBubble(t, "a2", true, "2026-01-01T00:01:30Z",
		map[string]any{"text": "改夹具。"}))

	// cccccccc -- attached context and nothing else. A history is full of
	// these, and none of them is a task.
	const c = "cccccccc-1111-2222-3333-444444444444"
	put("composerData:"+c, cursorComposer(t, c, cwd, "grok-4.5", 1767224000000,
		"assistant:a1"))
	put("bubbleId:"+c+":a1", cursorBubble(t, "a1", true, "2026-01-01T00:00:00Z",
		map[string]any{"text": "attached: main.go"}))
}

func cursorComposer(t *testing.T, id, cwd, model string, updated int64, bubbles ...string) string {
	t.Helper()
	headers := make([]map[string]any, 0, len(bubbles))
	for _, b := range bubbles {
		typ, bid, _ := strings.Cut(b, ":")
		n := 1
		if typ == "assistant" {
			n = 2
		}
		headers = append(headers, map[string]any{"bubbleId": bid, "type": n})
	}
	doc := map[string]any{
		"_v":                          18,
		"composerId":                  id,
		"createdAt":                   int64(1767225600000), // 2026-01-01T00:00:00Z
		"lastUpdatedAt":               updated,
		"fullConversationHeadersOnly": headers,
		"workspaceIdentifier": map[string]any{
			"uri": map[string]any{"$mid": 1, "fsPath": cwd, "path": cwd, "scheme": "file"},
		},
		"modelConfig": map[string]any{"modelName": model},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal composer: %v", err)
	}
	return string(body)
}

func cursorBubble(t *testing.T, id string, assistant bool, at string, extra map[string]any) string {
	t.Helper()
	typ := 1
	if assistant {
		typ = 2
	}
	doc := map[string]any{"_v": 3, "type": typ, "bubbleId": id, "createdAt": at}
	for k, v := range extra {
		doc[k] = v
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal bubble: %v", err)
	}
	return string(body)
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

// writeSlashCommandDataset is a one-task dataset whose prompt is a recorded
// slash-command invocation, with a recording that took real work.
//
// The baseline is what makes the mistake visible rather than merely wrong: with
// nothing to compare against, a run that did nothing has nothing to be
// favourably measured against, and the assertion below would hold either way.
func writeSlashCommandDataset(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "baselines"), 0o755); err != nil {
		t.Fatalf("create dataset dir: %v", err)
	}
	manifest := `name: slash-command
description: a prompt that opens with a slash command, recorded on another agent
tasks:
  - id: oncall
    prompt: "/bff-oncall  测试环境 新服务 76be4c8d-add2-4fc4-b576-4b3741638b2f"
    workspace: {type: empty}
    output: {type: stdout}
    baseline:
      file: baselines/oncall.json
      notes: recorded from cursor
`
	baseline := `{
  "id": "rec-oncall",
  "source": "cursor-composer",
  "agent_name": "cursor",
  "final_output": "fetched 72780201.json",
  "turns": [
    {"role": "user", "text": "/bff-oncall  测试环境"},
    {"role": "assistant", "text": "fetched 72780201.json"}
  ],
  "busy_ms": 32664,
  "duration_ms": 32664
}`
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("dataset.yaml", manifest)
	write(filepath.Join("baselines", "oncall.json"), baseline)
	return dir
}

// TestASlashCommandPromptThatCannotResolveIsNotScoredAsARun covers the two ends
// of one failure.
//
// A task collected from one agent carries that agent's invocation syntax, so the
// prompt here opens with "/bff-oncall" -- a command Claude Code resolves only if
// the harness loaded the skill that provides it. Under the clean-room defaults it
// does not, and the CLI then answers "Unknown command" *locally*: no API call,
// exit zero, subtype "success", is_error false.
//
// The stand-in agent replays the stream a real Claude Code 2.1.205 emitted in
// exactly that situation, captured verbatim, so this runs offline while still
// testing the shape that broke. Recorded as a measurement, such an attempt is
// indistinguishable from one that answered in zero tokens -- and it earned a
// favourable delta against a recording that took half a minute of real work.
//
// So: the CLI warns before the attempt is paid for, and the run reports a
// harness error rather than a measurement.
func TestASlashCommandPromptThatCannotResolveIsNotScoredAsARun(t *testing.T) {
	work := t.TempDir()
	ds := writeSlashCommandDataset(t, filepath.Join(work, "ds"))
	out := filepath.Join(work, "run.html")

	r := runCLI(t, "run",
		"--dataset", ds,
		"--bin", filepath.Join(agents, "slash-command.sh"),
		"--model", "recorded-model",
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", out,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	// The warning is the only part that can still change the outcome, so it has
	// to name the command and the flags that would load it.
	for _, want := range []string{"1 task(s)", "/bff-oncall", "--disable-slash-commands=false"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr should mention %q before the attempt is paid for:\n%s", want, r.stderr)
		}
	}

	if !strings.Contains(r.stdout, "harness errors  1") {
		t.Errorf("a cell that never reached the model is a harness error:\n%s", r.stdout)
	}
	// The recording took 32.6s of real work. Reporting a saving against it is
	// the specific mistake this test exists for.
	if strings.Contains(r.stdout, "vs baseline") {
		t.Errorf("a non-measurement was reported as a saving against the recording:\n%s", r.stdout)
	}

	p := payloadFromHTML(t, out)
	if p.Counts.HarnessErrors != 1 {
		t.Errorf("counts.harness_errors = %d, want 1", p.Counts.HarnessErrors)
	}
	if p.Counts.Ungraded != 0 {
		t.Errorf("a harness error is not a measurement, so counts.ungraded = %d, want 0", p.Counts.Ungraded)
	}
	if m, ok := metricByKey(p, "harness_errors"); !ok || m.Display != "1" {
		t.Errorf("harness_errors metric = %+v (present %v), want a headline 1", m, ok)
	}
	if len(p.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(p.Tasks))
	}
	task := p.Tasks[0]
	if task.Status != "error" || task.StopReason != "harness_error" {
		t.Errorf("task = %s/%s, want error/harness_error", task.Status, task.StopReason)
	}
	// The reason has to name the command, because that is the only part of this
	// a reader can act on.
	if !strings.Contains(task.Error, "/bff-oncall") {
		t.Errorf("the failure does not name the command it could not resolve: %q", task.Error)
	}
	// The recording is not put beside the failure at all: store.Attempt
	// .ComparableBaseline excludes harness errors, so there is neither a delta
	// to be read as a saving nor a card to be read as a match. What the
	// recording cost stays readable on the dataset, just not here.
	if task.Baseline != nil {
		t.Errorf("a cell that was never measured should carry no comparison, got %+v", task.Baseline)
	}
}

// TestTheSlashCommandWarningIsTiedToTheIsolation is the other half of the guard.
//
// The warning is about the harness, not about the prompt: the same dataset under
// a run that loads the user's skills has nothing to warn about. A warning that
// fired regardless would be the kind nobody reads.
func TestTheSlashCommandWarningIsTiedToTheIsolation(t *testing.T) {
	work := t.TempDir()
	ds := writeSlashCommandDataset(t, filepath.Join(work, "ds"))

	r := runCLI(t, "run",
		"--dataset", ds,
		"--bin", filepath.Join(agents, "slash-command.sh"),
		"--model", "recorded-model",
		"--bare=false", "--disable-slash-commands=false",
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}
	if strings.Contains(r.stderr, "open with a slash command") {
		t.Errorf("a run that loads the user's skills warned about them anyway:\n%s", r.stderr)
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

// downgradeToV1 rewrites a database into the shape v1 had: the columns every
// later revision added gone, the version stamped back.
//
// It removes what v2, v3 and v4 added rather than writing out a v1 DDL, so the
// fixture cannot drift away from what v1 actually was. Dropping only the column
// named in the test's title is how it did drift: the result was a database
// labelled v1 that already carried the columns the upgrade steps exist to add,
// which passes whether or not those steps work.
func downgradeToV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`ALTER TABLE attempts DROP COLUMN baseline_output`,
		`ALTER TABLE attempts DROP COLUMN baseline_agent_name`,
		`ALTER TABLE attempts DROP COLUMN baseline_agent_model`,
		`ALTER TABLE attempts DROP COLUMN baseline_source`,
		`ALTER TABLE attempts DROP COLUMN baseline_notes`,
		`ALTER TABLE runs DROP COLUMN agent_spec_json`,
		`ALTER TABLE runs DROP COLUMN chain_sessions`,
		`UPDATE schema_meta SET value='1' WHERE key='version'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("rewrite %s into v1 (%s): %v", filepath.Base(path), stmt, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The skill set a run drove
// ---------------------------------------------------------------------------

// TestCLIProvesWhichSkillsARunLoaded is the end of the chain the skill factor
// needs: a run records the configuration it drove, the page states what that
// configuration loaded, and a comparison names skills as a difference.
//
// None of that used to be checkable. The run row carried the agent, the model,
// the binary and the backend, and a skill set is none of those: it is the plugin
// directories plus the isolation flags that decide whether they survive. Two
// runs differing only in their skills therefore compared as identical, and the
// report had no way to say otherwise -- while the reading a silent comparison
// invites, that the two measured the same thing, is the one that is certainly
// wrong.
//
// The two runs here differ in exactly one thing, so the comparison has to name
// it as the single cause rather than as part of a bundle.
func TestCLIProvesWhichSkillsARunLoaded(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds"))
	skills := filepath.Join(work, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}

	runNamed := func(name, id string, extra ...string) {
		t.Helper()
		args := append([]string{"run",
			"--dataset", ds, "--bin", filepath.Join(agents, "one-turn.sh"),
			"--model", "stand-in", "--name", name, "--run-id", id,
			"--db", db, "--work-root", filepath.Join(work, "work-"+name),
		}, extra...)
		if r := runCLI(t, args...); r.exitCode != 0 {
			t.Fatalf("run %s exited %d\nstderr:\n%s", name, r.exitCode, r.stderr)
		}
	}
	runNamed("with-skills", "skills-on",
		"--plugin-dir", skills, "--bare=false", "--disable-slash-commands=false")
	runNamed("without-skills", "skills-off")

	// The page can state the configuration: one declared directory, and it
	// reached the agent because both flags that would have removed it were off.
	only := filepath.Join(work, "skills-on.html")
	if r := runCLI(t, "report", "--db", db, "--run", "skills-on", "-o", only); r.exitCode != 0 {
		t.Fatalf("report failed (%d): %s", r.exitCode, r.stderr)
	}
	on := payloadFromHTML(t, only)
	if on.Run.Skills == nil {
		t.Fatal("the run page must state what the run loaded, not leave it out")
	}
	if !on.Run.Skills.Active || on.Run.Skills.Text != "1 plugin dir(s)" {
		t.Errorf("skills = %+v, want the one declared directory reported as loaded", on.Run.Skills)
	}
	if len(on.Run.Skills.Dirs) != 1 || on.Run.Skills.Dirs[0] != skills {
		t.Errorf("skills.dirs = %v, want the declared directory %s", on.Run.Skills.Dirs, skills)
	}

	cmp := filepath.Join(work, "compare.html")
	r := runCLI(t, "report", "--db", db, "--compare", "skills-on", "skills-off", "-o", cmp)
	if r.exitCode != 0 {
		t.Fatalf("compare failed (%d): %s", r.exitCode, r.stderr)
	}
	delta := payloadFromHTML(t, cmp)

	// The comparison page describes the run it is about -- the right-hand one --
	// so this reads "none declared" beside a warning that says the left one
	// loaded a directory. Both statements are about a real difference and the
	// page makes neither of them up.
	if delta.Run.Skills == nil || delta.Run.Skills.Active {
		t.Errorf("skills = %+v, want the right-hand run's own configuration", delta.Run.Skills)
	}

	// And the comparison names it. Everything else about these two runs is the
	// same -- agent, model, binary, backend and dataset -- so if the skill
	// difference is not the named cause, the deltas below read as the effect of
	// nothing at all.
	bundle := strings.Join(delta.Warnings, "\n")
	for _, want := range []string{"differ in the skills loaded", skills, "vs none",
		"that one difference and nothing else"} {
		if !strings.Contains(bundle, want) {
			t.Errorf("the comparison should say %q, got:\n%s", want, bundle)
		}
	}
}

// TestCLIReadsTheSkillFlags: declaring a directory is not loading it, and the
// page has to keep the two apart. The clean-room default is precisely the state
// in which a declared directory does nothing, so a run that declares one and
// leaves the flags alone must not be reported as having skills.
func TestCLIReadsTheSkillFlags(t *testing.T) {
	work := t.TempDir()
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds"))
	skills := filepath.Join(work, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}

	report := filepath.Join(work, "r.html")
	r := runCLI(t, "run",
		"--dataset", ds, "--bin", filepath.Join(agents, "one-turn.sh"),
		"--plugin-dir", skills, // and the clean-room defaults, which remove it
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"), "--report", report)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}
	p := payloadFromHTML(t, report)
	if p.Run.Skills == nil {
		t.Fatal("the run page must state what the run loaded")
	}
	if p.Run.Skills.Active {
		t.Errorf("skills = %+v, want them reported as declared but not loaded: the clean-room flags are on",
			p.Run.Skills)
	}
	if !strings.Contains(p.Run.Skills.Text, "not loaded") ||
		!strings.Contains(p.Run.Skills.RemovedBy, "--bare") {
		t.Errorf("skills = %+v, want the flags that removed it named", p.Run.Skills)
	}
}

// TestCLIChainsASessionsTasksIntoOneWorkspace is the end of the chain the
// turn split needs.
//
// `collect --split turn` makes each prompt of a session its own task, and the
// later prompts refer to what the earlier ones produced -- one of them here is
// literally "why was this sent", about a file the first one wrote. Replayed in
// separate, freshly materialised workspaces, every follow-up asks the agent
// about files that were never there, and the run measures the harness's choice
// of workspace rather than the agent.
//
// So the test runs the same dataset twice and checks the one thing that
// separates the two: whether the second task can see what the first one left.
// An assertion on the run's own flag would pass even if the workspaces were
// still separate.
func TestCLIChainsASessionsTasksIntoOneWorkspace(t *testing.T) {
	work := t.TempDir()
	ds := writeChainedTurnDataset(t, filepath.Join(work, "ds"))
	db := filepath.Join(work, "runs.db")
	agent := filepath.Join(agents, "chain.sh")

	run := func(name string, extra ...string) payload {
		t.Helper()
		report := filepath.Join(work, name+".html")
		args := append([]string{"run",
			"--dataset", ds, "--bin", agent,
			"--model", "stand-in", "--name", name, "--run-id", name,
			"--db", db, "--work-root", filepath.Join(work, "work-"+name),
			"--report", report,
		}, extra...)
		if r := runCLI(t, args...); r.exitCode != 0 {
			t.Fatalf("run %s exited %d\nstderr:\n%s", name, r.exitCode, r.stderr)
		}
		return payloadFromHTML(t, report)
	}

	// Off: two tasks, two workspaces, and the follow-up finds nothing.
	plain := run("unchained")
	if plain.Run.ChainSessions {
		t.Error("the page claims a run chained its sessions when the flag was not passed")
	}
	if got := plain.taskByID(t, "s1-p1").Answer; got != "missing" {
		t.Errorf("unchained follow-up answered %q, want it to find nothing: the two tasks must not share a workspace", got)
	}
	// The verdict says the same thing the answer does, and it is the verdict a
	// reader acts on: the same task, graded, fails here and passes below.
	if cell := plain.taskByID(t, "s1-p1"); !cell.Graded || cell.Passed {
		t.Errorf("unchained follow-up graded=%v passed=%v, want a graded failure", cell.Graded, cell.Passed)
	}

	// On: one workspace per session, in recorded order, so the follow-up reads
	// what the opening request wrote.
	chained := run("chained", "--chain-sessions")
	if !chained.Run.ChainSessions {
		t.Error("the page has no way to say the run chained its sessions, so a reader cannot tell a sequence from a set")
	}
	if got := chained.taskByID(t, "s1-p1").Answer; got != "found: the-first-turn-wrote-this" {
		t.Errorf("chained follow-up answered %q, want what the first task wrote", got)
	}
	// Same dataset, same agent, same prompt: the only thing that changed is
	// whether the two tasks shared a workspace, and it is what decides the
	// verdict.
	if cell := chained.taskByID(t, "s1-p1"); !cell.Graded || !cell.Passed {
		t.Errorf("chained follow-up graded=%v passed=%v, want a graded pass", cell.Graded, cell.Passed)
	}
	// The first task of the chain is unaffected either way -- it starts from the
	// workspace in both runs, which is what makes the second one the evidence.
	if got := chained.taskByID(t, "s1-p0").Answer; got != "wrote handoff.txt" {
		t.Errorf("first task answered %q", got)
	}
	// The opening task still materialises: chaining must not cost the chain its
	// starting state.
	if chained.Counts.Cells != 2 {
		t.Errorf("cells = %d, want both tasks of the session", chained.Counts.Cells)
	}

	// And the flag cannot be paired with a run that keeps several cells in
	// flight: two cells of one chain would write over each other and no column
	// would record that it happened.
	bad := runCLI(t, "run", "--dataset", ds, "--bin", agent, "--model", "stand-in",
		"--db", db, "--chain-sessions", "--concurrency", "4",
		"--work-root", filepath.Join(work, "w-bad"))
	if bad.exitCode != 2 {
		t.Errorf("chaining with four cells in flight exited %d, want the usage error 2\nstderr:\n%s", bad.exitCode, bad.stderr)
	}
	if !strings.Contains(bad.stderr, "in flight") {
		t.Errorf("the refusal must say what it cannot honour, got:\n%s", bad.stderr)
	}
}

// writeChainedTurnDataset writes what `collect --split turn` writes for a
// session of two prompts: two tasks that name the same recording, each with its
// place in it.
//
// It is written out by hand rather than produced by collect because the subject
// is the run, and going through collect would put the parser and the cursor
// database between the test and the thing it is asserting on.
func writeChainedTurnDataset(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `name: chained-turns
description: two prompts of one session, collected turn by turn
tasks_file: tasks.jsonl
`
	if err := os.WriteFile(filepath.Join(dir, "dataset.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks := `{"id":"s1-p0","prompt":"WRITE the handoff file","session":"s1","prompt_index":0,"workspace":{"type":"empty"},"output":{"type":"stdout"},"graders":[{"type":"exact_match","config":{"expected":"wrote handoff.txt","match_mode":"contains"}}]}
{"id":"s1-p1","prompt":"READ the handoff file back","session":"s1","prompt_index":1,"workspace":{"type":"empty"},"output":{"type":"stdout"},"graders":[{"type":"exact_match","config":{"expected":"found: the-first-turn-wrote-this","match_mode":"contains"}}]}
`
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "dataset.yaml")
}

// writeOneAnswerDataset writes a one-task dataset whose agent answers on stdout.
//
// It exists so a test about configuration can measure a run that succeeds: with
// a cell that passes, a difference in the report is a difference the test put
// there rather than one a failing fixture happened to make.
func writeOneAnswerDataset(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "baselines"), 0o755); err != nil {
		t.Fatalf("create dataset dir: %v", err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dataset.yaml", `name: one-answer
description: one task, answered on stdout, recorded elsewhere
tasks:
  - id: answer
    prompt: say done
    workspace: {type: empty}
    output: {type: stdout}
    baseline:
      file: baselines/answer.json
      notes: recorded elsewhere
`)
	write(filepath.Join("baselines", "answer.json"), `{
  "id": "rec-answer",
  "agent_name": "claude-code",
  "final_output": "done",
  "turns": [
    {"role": "user", "text": "say done"},
    {"role": "assistant", "text": "done"}
  ],
  "busy_ms": 90000,
  "duration_ms": 90000
}`)
	return dir
}

// ---------------------------------------------------------------------------
// Recorded reports and the server that hands them back
// ---------------------------------------------------------------------------

// runOneAnswer produces one run with one measured cell, without an interpreter.
//
// The demo dataset's tasks are graded by pytest, so driving the registry
// through it would skip these tests on every machine without an interpreter --
// which is exactly where the coverage is cheapest to have. The one-answer
// dataset is graded on stdout and the fixture emits a real stream, so the cell
// is a measurement rather than the "never ran" case.
func runOneAnswer(t *testing.T, work, db, name string) payload {
	t.Helper()
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds-"+name))
	reportPath := filepath.Join(work, name+".html")
	r := runCLI(t, "run",
		"--dataset", ds,
		"--bin", filepath.Join(agents, "one-turn.sh"),
		"--name", name,
		"--db", db,
		"--work-root", filepath.Join(work, "work-"+name),
		"--report", reportPath,
	)
	if r.exitCode != 0 {
		t.Fatalf("%s run exited %d\nstdout:\n%s\nstderr:\n%s", name, r.exitCode, r.stdout, r.stderr)
	}
	p := payloadFromHTML(t, reportPath)
	if p.Run.ID == "" {
		t.Fatal("the run's own report does not name the run")
	}
	if len(p.Tasks) != 1 {
		t.Fatalf("the fixture produced %d cells, want one to report on", len(p.Tasks))
	}
	return p
}

// TestCLIRecordsTheReportsItRenders covers the first half of the registry: what
// gets recorded, and what does not.
//
// The rule is that `report` records and nothing else does. A run that wrote its
// own HTML has not been *asked* for a report, and recording it would fill the
// registry with documents nobody chose; the row is a record of a request, which
// is why only the command that answers one writes it.
func TestCLIRecordsTheReportsItRenders(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	p := runOneAnswer(t, work, db, "recorded")

	if n := countReports(t, db); n != 0 {
		t.Errorf("a run that wrote its own report left %d row(s) in the registry; "+
			"only `report` should record one", n)
	}

	out := filepath.Join(work, "again.html")
	r := runCLI(t, "report", "--db", db, "--run", p.Run.ID, "--out", out)
	if r.exitCode != 0 {
		t.Fatalf("report exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("report wrote no file: %v", err)
	}
	id := recordedID(t, r.stdout)

	rows := reportRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("the registry holds %d row(s), want 1:\n%v", len(rows), rows)
	}
	row := rows[0]
	if row["id"] != id {
		t.Errorf("the command printed id %q but the row is %q", id, row["id"])
	}
	if row["kind"] != "run" {
		t.Errorf("kind = %q, want run", row["kind"])
	}
	// The hash has to be there and has to look like a sha256, because it is
	// the only thing that makes the row checkable later.
	hash, _ := row["payload_hash"].(string)
	if len(hash) != 64 {
		t.Errorf("payload_hash = %q, want a 64-character sha256", hash)
	}
	// And the request has to be one a rebuild can honour, which is the failure
	// this row is otherwise silent about.
	if row["params_json"] != `{"run":"`+p.Run.ID+`"}` {
		t.Errorf("params_json = %v, want the run the report was about", row["params_json"])
	}
	if s, _ := row["subject"].(string); s == "" {
		t.Error("the registration recorded no subject, so a later deletion would leave it unable to say what it was about")
	}
	// Registering again is a second row, not an update: the two hashes side by
	// side are the evidence that the document did or did not change between
	// them, and collapsing them would throw that away.
	second := runCLI(t, "report", "--db", db, "--run", p.Run.ID, "--out", filepath.Join(work, "twice.html"))
	if second.exitCode != 0 {
		t.Fatalf("the second report exited %d: %s", second.exitCode, second.stderr)
	}
	if n := countReports(t, db); n != 2 {
		t.Errorf("the registry holds %d row(s) after two renders, want both", n)
	}
	if recordedID(t, second.stdout) == id {
		t.Error("two registrations share an id, so one of them cannot be cited")
	}
}

// TestCLIServesTheReportsItRecorded is the join, and the reason it is one test
// rather than two.
//
// `report` writes a row and `serve` reads it, but either half passing its own
// tests says nothing about the join: a registration keyed one way and a lookup
// keyed another, or a listing whose links point at a route the server does not
// register, both pass in isolation and break the one thing the feature is for.
// The last of those is only checkable against a real router, so this runs a
// real server on a real port.
func TestCLIServesTheReportsItRecorded(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	p := runOneAnswer(t, work, db, "served")

	r := runCLI(t, "report", "--db", db, "--run", p.Run.ID, "--out", filepath.Join(work, "r.html"))
	if r.exitCode != 0 {
		t.Fatalf("report exited %d: %s", r.exitCode, r.stderr)
	}
	id := recordedID(t, r.stdout)

	base, stop := startServer(t, db)
	defer stop()

	listing := httpGet(t, base)
	if !strings.Contains(listing, id) {
		t.Errorf("the listing does not mention the recorded report %s", id)
	}
	// The link the listing carries has to be one the server answers, which is
	// the part no unit test of either half can see.
	if !strings.Contains(listing, `"href":"/r/`+id+`"`) {
		t.Errorf("the listing carries no link to /r/%s", id)
	}

	page, status := httpGetStatus(t, base+"r/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /r/%s returned %d\n%s", id, status, page)
	}
	if !strings.Contains(page, "Recorded report.") {
		t.Error("the served report does not say it still reproduces")
	}
	if !strings.Contains(page, `data-mode="run"`) {
		t.Error("the served report did not render in the run mode")
	}
	if !strings.Contains(page, p.Run.Name) {
		t.Error("the served report is not about the run it was recorded for")
	}
	if matches := httpHeader(t, base+"r/"+id+".json", "X-Aeval-Hash-Matches"); matches != "true" {
		t.Errorf("the served document does not reproduce the recorded hash (X-Aeval-Hash-Matches: %q)", matches)
	}

	// A link to something that was never recorded is a page, not a status code.
	_, missing := httpGetStatus(t, base+"r/nosuchreport")
	if missing != http.StatusNotFound {
		t.Errorf("GET /r/nosuchreport returned %d, want 404", missing)
	}
}

// countReports is how many rows the registry holds, read as a client of the
// file rather than through the code under test.
func countReports(t *testing.T, db string) int {
	t.Helper()
	return len(reportRows(t, db))
}

// reportRows reads the registry straight out of the database.
//
// It does not go through internal/store on purpose: the assertion is about what
// was written, and a reader that agreed with the writer by construction would
// agree with a wrong column name too.
func reportRows(t *testing.T, db string) []map[string]any {
	t.Helper()
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("open %s: %v", db, err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT id, kind, title, subject, params_json, payload_hash, payload_bytes,
		data_version FROM reports ORDER BY created_at DESC`)
	if err != nil {
		t.Fatalf("the reports table does not exist: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out
}

// recordedID reads the id a report printed. The line is the command's only
// handle on the row, so it has to be there and has to be parseable.
func recordedID(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "recorded") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[1]) != 16 {
			t.Fatalf("the recorded line is %q, want `recorded  <16 hex characters>`", line)
		}
		return fields[1]
	}
	t.Fatalf("report did not print the id it recorded:\n%s", stdout)
	return ""
}

// startServer runs aeval serve on a port the kernel picks and returns its base
// URL.
//
// Port 0 rather than a fixed one: a test that needs a specific port fails when
// something else is already on it, and that failure says nothing about the
// code. The address printed is the one that actually came back, which is the
// only way to find out which port that was.
func startServer(t *testing.T, db string) (string, func()) {
	t.Helper()
	cmd := exec.Command(cliPath, "serve", "--db", db, "--addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}

	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("serve did not stop on SIGINT\nstderr:\n%s", errBuf.String())
		}
	}
	t.Cleanup(stop)

	type firstLine struct {
		line string
		err  error
	}
	ch := make(chan firstLine, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		ch <- firstLine{line, err}
	}()
	select {
	case fl := <-ch:
		if fl.err != nil {
			t.Fatalf("reading serve's output: %v\nstderr:\n%s", fl.err, errBuf.String())
		}
		fields := strings.Fields(fl.line)
		if len(fields) < 2 || !strings.HasPrefix(fields[len(fields)-1], "http://") {
			t.Fatalf("serve first printed %q, want the address it is listening on", fl.line)
		}
		return fields[len(fields)-1], stop
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("serve never printed an address\nstderr:\n%s", errBuf.String())
		return "", stop
	}
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	body, status := httpGetStatus(t, url)
	if status != http.StatusOK {
		t.Fatalf("GET %s returned %d\n%s", url, status, body)
	}
	return body
}

func httpGetStatus(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body), resp.StatusCode
}

func httpHeader(t *testing.T, url, header string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get(header)
}

// TestCLIRefusesToServeAnAddressItCannotBind covers the one startup failure a
// person actually hits: a port already in use.
//
// It has to fail before anything is printed, or a script reading the address
// gets a line about a server that is not running.
func TestCLIRefusesToServeAnAddressItCannotBind(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	// The failure under test is about the address, not the data, but serve
	// still has to open a real database first.
	runOneAnswer(t, work, db, "bind-probe")

	// Take a port, then ask for it.
	busy, stopBusy := startServer(t, db)
	defer stopBusy()
	host := strings.TrimPrefix(busy, "http://")
	host = strings.TrimSuffix(host, "/")

	r := runCLI(t, "serve", "--db", db, "--addr", host)
	if r.exitCode != 1 {
		t.Fatalf("serve on a taken port exited %d, want 1\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout, "serving") {
		t.Errorf("serve claimed to be up on a port it could not bind:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, host) {
		t.Errorf("the error does not name the address it could not bind:\n%s", r.stderr)
	}
}

// ---------------------------------------------------------------------------
// Working in the recorded repository
// ---------------------------------------------------------------------------

// gitInRepo runs git in a test repository with the developer's own configuration
// kept out of it, so a global hook cannot decide whether these tests pass.
func gitInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false",
		"-c", "user.name=agenteval", "-c", "user.email=eval@agenteval.local"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
	return strings.TrimSpace(string(out))
}

// writeInPlaceDataset writes a repository with two commits and a one-task
// dataset bound to the first, which is the shape `collect --workspace-cwd`
// produces for a session whose capture named a commit.
func writeInPlaceDataset(t *testing.T, dir string) (dsPath, repo, first, second string) {
	t.Helper()
	repo = filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "-A")
	gitInRepo(t, repo, "commit", "--quiet", "-m", "first")
	first = gitInRepo(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "-A")
	gitInRepo(t, repo, "commit", "--quiet", "-m", "second")
	second = gitInRepo(t, repo, "rev-parse", "HEAD")

	ds := filepath.Join(dir, "ds")
	if err := os.MkdirAll(ds, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: in-place\ntasks_file: tasks.jsonl\n"
	if err := os.WriteFile(filepath.Join(ds, "dataset.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks := fmt.Sprintf(
		`{"id":"t1","prompt":"change the file","workspace":{"type":"git","repo":%q,"ref":%q},`+
			`"output":{"type":"patch"},"limits":{"timeout_s":60},`+
			`"graders":[{"type":"command","config":{"command":"test -f calc.py"}}]}`+"\n", repo, first)
	if err := os.WriteFile(filepath.Join(ds, "tasks.jsonl"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(ds, "dataset.yaml"), repo, first, second
}

// TestCLIRunsInPlaceAndPutsTheRepositoryBack is the end-to-end claim of the
// whole mode: an agent was let into a working repository, and the repository is
// where it was found.
func TestCLIRunsInPlaceAndPutsTheRepositoryBack(t *testing.T) {
	work := t.TempDir()
	ds, repo, first, second := writeInPlaceDataset(t, work)
	db := filepath.Join(work, "runs.db")
	report := filepath.Join(work, "run.html")

	r := runCLI(t, "run", "--dataset", ds, "--in-place",
		"--bin", filepath.Join(agents, "good.sh"),
		"--db", db, "--name", "inplace", "--run-id", "inplace",
		"--report", report)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}

	// What the run did to the repository, which is the half that cannot be
	// undone if it is wrong.
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("the repository must still exist: %v", err)
	}
	if got := gitInRepo(t, repo, "rev-parse", "HEAD"); got != second {
		t.Errorf("HEAD = %s, want %s: the repository was not put back", got, second)
	}
	if got := gitInRepo(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := gitInRepo(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("the tree is not clean: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "calc.py")); err != nil || string(got) != "second\n" {
		t.Errorf("calc.py = %q (%v), want the content the run started with", got, err)
	}
	// The lock is gone, so the next run is not refused by this one.
	if _, err := os.Stat(filepath.Join(repo, ".git", "agenteval")); !os.IsNotExist(err) {
		t.Errorf("the lock must not outlive the run: %v", err)
	}

	// And what the page says about it, because a reader who is not told will
	// take the cells for the same experiment as a copy-based run's.
	p := payloadFromHTML(t, report)
	if !p.Run.InPlace {
		t.Error("the page must say the run worked in the recorded directory")
	}
	cell := p.taskByID(t, "t1")
	if cell.WorkspaceDir != repo {
		t.Errorf("workspace_dir = %q, want %s", cell.WorkspaceDir, repo)
	}
	if !strings.Contains(cell.RestoreNotes, "put back") {
		t.Errorf("restore_notes = %q, want the restore recorded", cell.RestoreNotes)
	}
	// The diff is still measured against the commit the task names, which is
	// the whole reason the checkout is worth doing.
	if !strings.Contains(cell.Diff, "calc.py") {
		t.Errorf("the agent's change is not in the diff: %q", cell.Diff)
	}
	// The banner says what the run is about to do, before it does it.
	for _, want := range []string{"in place", "puts each one back", repo} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the banner should mention %q, got:\n%s", want, r.stderr)
		}
	}
	_ = first
}

func TestCLIRefusesInPlaceWithConcurrency(t *testing.T) {
	work := t.TempDir()
	ds, repo, _, second := writeInPlaceDataset(t, work)
	r := runCLI(t, "run", "--dataset", ds, "--in-place", "--concurrency", "4",
		"--bin", filepath.Join(agents, "one-turn.sh"),
		"--db", filepath.Join(work, "runs.db"))
	if r.exitCode != 2 {
		t.Errorf("in-place with four cells in flight exited %d, want the usage error 2\nstderr:\n%s",
			r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "in flight") {
		t.Errorf("the refusal must say what it cannot honour, got:\n%s", r.stderr)
	}
	// Nothing was touched: the refusal is before the first cell.
	if got := gitInRepo(t, repo, "rev-parse", "HEAD"); got != second {
		t.Errorf("a refused run moved the repository to %s", got)
	}
}

func TestCLIInPlaceRefusesADirtyRepository(t *testing.T) {
	// Uncommitted work is the one thing a restore cannot reconstruct, so it is
	// refused before anything runs rather than discovered afterwards.
	work := t.TempDir()
	ds, repo, _, _ := writeInPlaceDataset(t, work)
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("someone's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "run", "--dataset", ds, "--in-place",
		"--bin", filepath.Join(agents, "one-turn.sh"),
		"--db", filepath.Join(work, "runs.db"))
	if r.exitCode != 2 {
		t.Errorf("a dirty repository exited %d, want the usage error 2\nstderr:\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "uncommitted changes") {
		t.Errorf("the refusal must say what is wrong, got:\n%s", r.stderr)
	}
	// The uncommitted work is still there, which is the point of refusing.
	if got, _ := os.ReadFile(filepath.Join(repo, "calc.py")); string(got) != "someone's work\n" {
		t.Errorf("the refusal must leave the worktree alone, got %q", got)
	}
}

func TestCLIRefusesASecondInPlaceRunOnALockedRepository(t *testing.T) {
	// Two runs in one repository would check out over each other, and each
	// would put back a state neither of them started from. The lock is not
	// stolen, and the refusal carries the commands that repair a repository a
	// killed run left behind.
	work := t.TempDir()
	ds, repo, _, second := writeInPlaceDataset(t, work)
	lockDir := filepath.Join(repo, ".git", "agenteval")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf(`{"repo":%q,"head":%q,"branch":"main","pid":1,`+
		`"started_at":"2026-01-02T03:04:05Z","tool":"aeval"}`, repo, second)
	if err := os.WriteFile(filepath.Join(lockDir, "inplace.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "run", "--dataset", ds, "--in-place",
		"--bin", filepath.Join(agents, "one-turn.sh"),
		"--db", filepath.Join(work, "runs.db"))
	if r.exitCode == 0 {
		t.Fatalf("a locked repository must be refused\nstderr:\n%s", r.stderr)
	}
	for _, want := range []string{"update-ref refs/heads/main", "checkout --force main", "clean -ffd"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the refusal must carry the recovery command %q, got:\n%s", want, r.stderr)
		}
	}
}

// TestCLIOnlyWorksInPlaceWhenAsked covers the default, which is the whole
// safety of the mode.
//
// `--in-place` is off unless it is asked for, and the default lives in the flag
// registration rather than in anything a reader of the run would see. A default
// of true would put every ordinary run inside somebody's working repository,
// with the history readable and the tree rewritten underneath them, and the
// only thing that would look different is a chip on a page.
func TestCLIOnlyWorksInPlaceWhenAsked(t *testing.T) {
	work := t.TempDir()
	ds, repo, _, second := writeInPlaceDataset(t, work)
	report := filepath.Join(work, "copy.html")

	r := runCLI(t, "run", "--dataset", ds,
		"--bin", filepath.Join(agents, "good.sh"),
		"--db", filepath.Join(work, "runs.db"), "--name", "copy", "--run-id", "copy",
		"--work-root", filepath.Join(work, "work"),
		"--report", report)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstderr:\n%s", r.exitCode, r.stderr)
	}
	p := payloadFromHTML(t, report)
	if p.Run.InPlace {
		t.Error("a run that did not ask for --in-place claims it worked in the recorded directory")
	}
	if got := p.taskByID(t, "t1").RestoreNotes; got != "" {
		t.Errorf("a copy-based run has nothing to restore, got notes: %q", got)
	}
	// And the recorded repository is untouched: the agent's edit went into a
	// copy under the work root, which is the difference the flag buys.
	if got := gitInRepo(t, repo, "rev-parse", "HEAD"); got != second {
		t.Errorf("HEAD = %s, want %s: an ordinary run moved the recorded repository", got, second)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "calc.py")); err != nil || string(got) != "second\n" {
		t.Errorf("calc.py = %q (%v), want it untouched", got, err)
	}
	if got := gitInRepo(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("the recorded repository is not clean: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "agenteval")); !os.IsNotExist(err) {
		t.Errorf("an ordinary run must not leave an in-place lock behind: %v", err)
	}
}

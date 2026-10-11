// Package tests holds end-to-end tests that drive the built aeval binary the way
// a user would: a dataset on disk, a real subprocess, a real SQLite file, a real
// HTML report, a real HTTP server.
//
// The unit tests in internal/ each prove one component in isolation. This file
// exists to catch the failures that only appear when the components are wired
// together -- a path resolved against the wrong base directory, a payload the
// report writes but the API does not, a verdict that is correct in memory and
// inverted once persisted.
//
// Everything here runs against the scripted agents in testdata/agents, so it
// needs no model, no network and no credentials. Tasks that need pytest are
// skipped rather than failed when no suitable interpreter is present: pytest is
// a task runtime dependency, not a framework dependency.
package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

var (
	repoRoot string
	cliPath  string
	// python is an interpreter with pytest, or "" when none was found.
	python string
)

func TestMain(m *testing.M) {
	root, err := filepath.Abs("..")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve repo root:", err)
		os.Exit(1)
	}
	repoRoot = root

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
	cmd.Env = append(os.Environ(), "AGENTEVAL_PYTHON="+python)
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
type payload struct {
	Version int    `json:"version"`
	Title   string `json:"title"`
	Run     struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		AgentKind  string `json:"agent_kind"`
		AgentModel string `json:"agent_model"`
		Backend    string `json:"sandbox_backend"`
		Dataset    string `json:"dataset_name"`
		DatasetRef string `json:"dataset_hash"`
	} `json:"run"`
	Counts struct {
		Samples     int `json:"samples"`
		Tasks       int `json:"tasks"`
		Passed      int `json:"passed"`
		TasksPassed int `json:"tasks_passed"`
		Failed      int `json:"failed"`
		Errored     int `json:"errored"`
	} `json:"counts"`
	Metrics []struct {
		Name      string  `json:"name"`
		Value     float64 `json:"value"`
		Display   string  `json:"display"`
		Group     string  `json:"group"`
		Direction string  `json:"direction"`
		Extra     map[string]any
	} `json:"metrics"`
	Tasks   []task `json:"tasks"`
	Compare *struct {
		Left   string `json:"left_run_id"`
		Right  string `json:"right_run_id"`
		Counts struct {
			Fixed     int `json:"fixed"`
			Regressed int `json:"regressed"`
			StillPass int `json:"still_passing"`
			StillFail int `json:"still_failing"`
		} `json:"summary"`
	} `json:"compare"`
	Warnings []string `json:"warnings"`
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
	html := string(raw)

	const marker = "window.__AEVAL_PAYLOAD__ = "
	start := strings.Index(html, marker)
	if start < 0 {
		t.Fatalf("%s has no embedded payload", path)
	}
	body := html[start+len(marker):]
	end := strings.Index(body, "</script>")
	if end < 0 {
		t.Fatalf("%s has an unterminated payload script", path)
	}
	body = strings.TrimRight(body[:end], "; \n\t")

	var p payload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode payload from %s: %v", path, err)
	}
	return p
}

// task is one graded cell as the report describes it.
type task struct {
	ID      string  `json:"id"`
	Key     string  `json:"key"`
	Passed  bool    `json:"passed"`
	Score   float64 `json:"score"`
	Answer  string  `json:"answer"`
	Diff    string  `json:"diff"`
	Detail  string  `json:"detail"`
	Graders []struct {
		Grader string  `json:"grader"`
		Type   string  `json:"type"`
		Passed bool    `json:"passed"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	} `json:"graders"`
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

func (p payload) metricValue(t *testing.T, name string) float64 {
	t.Helper()
	for _, m := range p.Metrics {
		if m.Name == name {
			return m.Value
		}
	}
	t.Fatalf("run has no metric %q", name)
	return 0
}

// ---------------------------------------------------------------------------
// The suite
// ---------------------------------------------------------------------------

// TestCLIEndToEnd is the load-bearing test: one dataset, two agents, and the
// claim that the pipeline tells them apart for the right reasons.
func TestCLIEndToEnd(t *testing.T) {
	requirePytest(t)

	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	good := runAgent(t, work, db, "good", "scripted-good")
	bad := runAgent(t, work, db, "bad", "scripted-bad")

	// --- the good agent resolves everything ---------------------------------
	if good.Counts.Samples != 3 || good.Counts.Tasks != 3 {
		t.Errorf("good run counts = %+v, want 3 samples over 3 tasks", good.Counts)
	}
	if good.Counts.Passed != 3 || good.Counts.TasksPassed != 3 {
		t.Errorf("good run passed = %d/%d, want 3/3", good.Counts.Passed, good.Counts.TasksPassed)
	}
	if good.Counts.Failed != 0 || good.Counts.Errored != 0 {
		t.Errorf("a correct agent must produce no failures: %+v", good.Counts)
	}
	if got := good.metricValue(t, "resolve_rate"); got != 1 {
		t.Errorf("resolve_rate = %v, want 1", got)
	}
	if got := good.metricValue(t, "harness_error_rate"); got != 0 {
		t.Errorf("harness_error_rate = %v; the harness should not be the thing failing", got)
	}

	// --- and it does so for the right reasons -------------------------------
	//
	// A run can reach 3/3 because the graders work, or because the graders are
	// broken in the permissive direction. These assertions pin the first.
	fix := good.taskByID(t, "fix-average-off-by-one")
	if !fix.Passed || len(fix.Graders) != 1 || !fix.Graders[0].Passed {
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
			t.Errorf("grader %s failed on a correct solution: %s", g.Grader, g.Reason)
		}
	}
	if !strings.Contains(slug.Graders[0].Reason, "passed") {
		t.Errorf("hidden suite reason = %q, want a pytest passing count", slug.Graders[0].Reason)
	}

	answer := good.taskByID(t, "read-and-answer")
	if answer.Answer != "31" {
		t.Errorf("answer = %q, want 31", answer.Answer)
	}

	// --- the bad agent resolves nothing, per task ---------------------------
	if bad.Counts.Passed != 0 || bad.Counts.Failed != 3 {
		t.Errorf("bad run counts = %+v, want 0 passed / 3 failed", bad.Counts)
	}
	if got := bad.metricValue(t, "resolve_rate"); got != 0 {
		t.Errorf("bad resolve_rate = %v, want 0", got)
	}
	// Partial credit is still recorded, which is what makes mean_score a
	// different number from resolve_rate rather than a duplicate of it.
	if got := bad.metricValue(t, "mean_score"); got <= 0 || got >= 1 {
		t.Errorf("bad mean_score = %v, want something strictly between 0 and 1", got)
	}

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
	//
	// Regenerating from the same database must reproduce the run exactly. The
	// payload struct deliberately has no "generated" field, so the render
	// timestamp -- the only field that is a property of *when* the report was
	// made rather than of the run it describes -- is dropped by decoding.
	regen := filepath.Join(work, "regen.html")
	if r := runCLI(t, "report", "--db", db, "--run", bad.Run.ID, "--out", regen); r.exitCode != 0 {
		t.Fatalf("report regeneration failed (%d): %s", r.exitCode, r.stderr)
	}
	if fromDB := payloadFromHTML(t, regen); !samePayload(t, bad, fromDB) {
		t.Error("a report regenerated from the database differs from the one written during the run")
	}

	// --- the comparison classifies every transition -------------------------
	cmp := filepath.Join(work, "compare.html")
	r := runCLI(t, "compare", "--db", db, "--out", cmp, good.Run.ID, bad.Run.ID)
	if r.exitCode != 0 {
		t.Fatalf("compare failed (%d): %s", r.exitCode, r.stderr)
	}
	delta := payloadFromHTML(t, cmp)
	if delta.Compare == nil {
		t.Fatal("the comparison report has no compare section")
	}
	if delta.Compare.Counts.Regressed != 3 {
		t.Errorf("good -> bad regressed %d cells, want 3", delta.Compare.Counts.Regressed)
	}
	if delta.Compare.Counts.Fixed != 0 || delta.Compare.Counts.StillFail != 0 {
		t.Errorf("nothing should be fixed or still failing: %+v", delta.Compare.Counts)
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

// runAgent executes one full evaluation and returns the payload its report
// carries. It also covers the claim that a run's own report and a regenerated
// one agree, by reading the report the run itself wrote.
func runAgent(t *testing.T, work, db, name, model string) payload {
	t.Helper()

	reportPath := filepath.Join(work, name+".html")
	bin, err := filepath.Abs(filepath.Join("testdata", "agents", name+".sh"))
	if err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, "run",
		"--dataset", filepath.Join(repoRoot, "datasets", "demo"),
		"--agent", "mock",
		"--bin", bin,
		"--model", model,
		"--name", name,
		"--db", db,
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
	if p.Run.Dataset == "" {
		t.Error("the run row should name the dataset it evaluated")
	}
	if len(p.Warnings) != 0 {
		t.Errorf("an ordinary run produced warnings: %v", p.Warnings)
	}
	return p
}

// samePayload compares two run payloads, ignoring the generation timestamp.
//
// It round-trips through JSON so the comparison is over the serialised form --
// which is the artefact a reader would actually receive -- rather than over Go
// values that might agree while their encodings differ.
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
// Server
// ---------------------------------------------------------------------------

// TestCLIServeServesTheSameNumbers checks the HTTP surface against the file
// report: the API is documented as returning exactly the payload the page
// embeds, and that promise is only worth anything if it is exercised through the
// real binary rather than a handler under test.
func TestCLIServeServesTheSameNumbers(t *testing.T) {
	requirePytest(t)

	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	good := runAgent(t, work, db, "good", "scripted-serve")

	addr := freeAddr(t)
	cmd := exec.Command(cliPath, "serve", "--db", db, "--addr", addr)
	cmd.Dir = repoRoot
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	// The server is killed rather than asked to stop: it is a subprocess under
	// test, and a graceful shutdown path would be one more thing this test
	// depends on that the user never runs.
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + addr
	waitForHealth(t, base, &logs)

	// --- page and API agree, byte for byte ---------------------------------
	pageBody := httpGet(t, base+"/runs/"+good.Run.ID)
	apiBody := httpGet(t, base+"/api/runs/"+good.Run.ID)
	if extractPayload(t, pageBody) != extractPayload(t, apiBody) {
		t.Error("the run page and the API return different payloads")
	}

	// --- the API's numbers are the run's numbers ---------------------------
	var api struct {
		Counts struct {
			Passed int `json:"passed"`
			Tasks  int `json:"tasks"`
		} `json:"counts"`
		Tasks []struct {
			ID     string `json:"id"`
			Passed bool   `json:"passed"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(apiBody), &api); err != nil {
		t.Fatalf("decode API payload: %v\n%s", err, apiBody)
	}
	if api.Counts.Tasks != 3 || api.Counts.Passed != 3 {
		t.Errorf("API counts = %+v, want 3/3", api.Counts)
	}

	// --- routing rejects the wrong things ----------------------------------
	if code, body := httpStatus(t, base+"/runs/does-not-exist"); code != http.StatusNotFound {
		t.Errorf("unknown run: status %d, want 404 (%s)", code, body)
	} else if strings.Contains(body, "window.__AEVAL_PAYLOAD__") {
		t.Error("an unknown run fell back to rendering something")
	}
	if code, _ := httpStatus(t, base+"/api/runs/does-not-exist"); code != http.StatusNotFound {
		t.Errorf("unknown run over the API: status %d, want 404", code)
	}
	if code, _ := httpStatus(t, base+"/compare"); code != http.StatusBadRequest {
		t.Errorf("compare without both ids: status %d, want 400", code)
	}
	if code, _ := httpStatus(t, base+"/static/app.js"); code != http.StatusOK {
		t.Errorf("asset: status %d, want 200", code)
	}

	// --- the index links to the run ----------------------------------------
	index := httpGet(t, base+"/")
	if !strings.Contains(index, good.Run.ID) {
		t.Error("the index does not link to the recorded run")
	}

	// --- deletion is off unless asked for ----------------------------------
	//
	// This is the one destructive route in the tool. It is disabled by default
	// and requires a same-origin request when enabled, because the server can
	// both render model-generated text and destroy data.
	req, err := http.NewRequest(http.MethodPost, base+"/api/runs/"+good.Run.ID+"/delete", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete without --allow-delete: status %d, want 403", resp.StatusCode)
	}
	// And the run is still there afterwards.
	if code, _ := httpStatus(t, base+"/api/runs/"+good.Run.ID); code != http.StatusOK {
		t.Errorf("run disappeared after a refused delete: status %d", code)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	// Bind an ephemeral port and hand it over. There is a small window where
	// something else could take it, which is preferable to hard-coding a port
	// that collides with whatever the developer already has running.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForHealth(t *testing.T, base string, logs *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server never became healthy\nserver output:\n%s", logs.String())
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	code, body := httpStatus(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, code)
	}
	return body
}

func httpStatus(t *testing.T, url string) (int, string) {
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
	return resp.StatusCode, string(body)
}

// extractPayload pulls the run payload out of either document that carries one:
// an HTML page, where it is an assignment to a global, or the JSON API, where it
// is the body itself. Both are trimmed to the same normalised form so the two
// can be compared on content rather than on surrounding markup.
func extractPayload(t *testing.T, doc string) string {
	t.Helper()
	body := doc
	// The embedded form is an object *literal*, so the byte that ends it is the
	// closing tag rather than a brace; scanning for the tag keeps a `</script>`
	// inside a string from being mistaken for the terminator, which is exactly
	// what the encoder's escaping guarantees cannot happen.
	if start := strings.Index(doc, "window.__AEVAL_PAYLOAD__ = "); start >= 0 {
		const marker = "window.__AEVAL_PAYLOAD__ = "
		body = doc[start+len(marker):]
		end := strings.Index(body, "</script>")
		if end < 0 {
			t.Fatalf("document has an unterminated payload script")
		}
		body = body[:end]
	}
	body = strings.TrimRight(body, "; \n\t")

	// The generation timestamp legitimately differs between two renders of the
	// same run, so it is normalised away before comparing.
	normalised, err := stripGenerated(body)
	if err != nil {
		t.Fatalf("document payload is not JSON: %v\n%s", err, body)
	}
	return normalised
}

// stripGenerated removes the "generated" key from a run payload.
func stripGenerated(raw string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", err
	}
	delete(m, "generated")
	out, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// Exit codes
// ---------------------------------------------------------------------------

// TestCLIExitCodes pins the contract automation depends on. The important one is
// that a run whose tasks all fail is still a *successful command*: an evaluation
// that fails its own process whenever the model does badly cannot be used in CI,
// where a red build has to mean "the harness broke".
func TestCLIExitCodes(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "runs.db")
	demo := filepath.Join(repoRoot, "datasets", "demo")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"version", []string{"version"}, 0},
		{"validate a good dataset", []string{"validate", "--dataset", demo}, 0},
		{"list an empty database", []string{"list", "--db", db}, 0},
		{"a missing dataset", []string{"validate", "--dataset", filepath.Join(work, "nope")}, 1},
		{"a missing run", []string{"report", "--db", db, "--run", "nope", "--out", filepath.Join(work, "x.html")}, 1},
		{"an unknown agent", []string{"run", "--dataset", demo, "--agent", "nope", "--db", db}, 1},
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
		"--dataset", filepath.Join(repoRoot, "datasets", "demo"),
		"--agent", "mock", "--bin", filepath.Join("tests", "testdata", "agents", "good.sh"),
		"--db", filepath.Join(work, "runs.db"),
	)
	if r.exitCode != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "resolved  3/3") {
		t.Errorf("a correct agent named by a relative path should resolve every task:\n%s", r.stdout)
	}
}

// TestCLIReportsAFailingRunAsSuccess is the exit-code contract stated as its own
// claim, because it is the one most likely to be "fixed" into nonsense by a
// well-meaning change.
func TestCLIReportsAFailingRunAsSuccess(t *testing.T) {
	requirePytest(t)

	work := t.TempDir()
	bin, err := filepath.Abs(filepath.Join("testdata", "agents", "bad.sh"))
	if err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "run",
		"--dataset", filepath.Join(repoRoot, "datasets", "demo"),
		"--agent", "mock", "--bin", bin,
		"--db", filepath.Join(work, "runs.db"),
	)
	if r.exitCode != 0 {
		t.Fatalf("a run whose tasks failed exited %d, want 0\nstderr:\n%s", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stdout, "resolved  0/3") {
		t.Errorf("the summary should say the run resolved nothing:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "[completed]") {
		t.Errorf("the run itself completed:\n%s", r.stdout)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// TestExampleConfigIsValid keeps the checked-in example honest. A reference
// config that no longer parses is worse than no reference config, because it is
// the thing people copy.
func TestExampleConfigIsValid(t *testing.T) {
	r := runCLI(t, "validate", "--config", filepath.Join(repoRoot, "examples", "aeval.yaml"))
	if r.exitCode != 0 {
		t.Fatalf("examples/aeval.yaml does not validate (%d):\n%s\n%s", r.exitCode, r.stdout, r.stderr)
	}
	// The dataset path in the example is written relative to the example's own
	// directory; if that rule regresses, this is where it shows up.
	if !strings.Contains(r.stdout, "demo-basic") {
		t.Errorf("the example config did not resolve the demo dataset:\n%s", r.stdout)
	}
}

// TestRegistryIsDiscoverableWithoutARun checks the introspection path a user
// reaches for when writing a dataset. It must work on a machine with no dataset,
// no database and no credentials, because that is when it is needed.
func TestRegistryIsDiscoverableWithoutARun(t *testing.T) {
	r := runCLI(t, "registry")
	if r.exitCode != 0 {
		t.Fatalf("registry exited %d: %s", r.exitCode, r.stderr)
	}
	for _, want := range []string{"claude_code", "mock", "pytest", "exact_match", "patch", "resolve_rate"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("registry output does not mention %q:\n%s", want, r.stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// aeval audit
//
// The audit is the answer to "can this dataset be trusted as a reward source",
// so these tests drive it against a fixture whose three tasks each trip exactly
// one check. A fixture where everything is healthy would only prove the command
// runs.
// ---------------------------------------------------------------------------

// auditReport is a partial view of `aeval audit --json`.
type auditReport struct {
	Group   string `json:"group"`
	Repeats int    `json:"repeats"`
	Tasks   []struct {
		TaskID  string `json:"task_id"`
		Healthy bool   `json:"healthy"`
		Reason  string `json:"reason"`
		Checks  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	} `json:"tasks"`
	Runs []struct {
		Role  string `json:"role"`
		RunID string `json:"run_id"`
	} `json:"runs"`
}

func (r auditReport) check(t *testing.T, taskID, name string) (string, string) {
	t.Helper()
	for _, task := range r.Tasks {
		if task.TaskID != taskID {
			continue
		}
		for _, c := range task.Checks {
			if c.Name == name {
				return c.Status, c.Detail
			}
		}
		t.Fatalf("task %s has no %s check", taskID, name)
	}
	t.Fatalf("task %s is missing from the report", taskID)
	return "", ""
}

func (r auditReport) healthyCount() int {
	n := 0
	for _, task := range r.Tasks {
		if task.Healthy {
			n++
		}
	}
	return n
}

func auditFixture() string { return filepath.Join(repoRoot, "tests", "testdata", "audit") }

func TestCLIAuditFindsTheDefectiveTasks(t *testing.T) {
	work := t.TempDir()
	db := filepath.Join(work, "audit.db")

	r := runCLI(t, "audit", "--dataset", auditFixture(), "--db", db,
		"--repeats", "2", "--concurrency", "3", "--json")
	if r.exitCode != 0 {
		t.Fatalf("audit exited %d; a defective *task* is a finding, not a failed command:\n%s\n%s",
			r.exitCode, r.stdout, r.stderr)
	}
	var rep auditReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("decode audit report: %v\n%s", err, r.stdout)
	}
	if len(rep.Tasks) != 3 || rep.Repeats != 2 {
		t.Fatalf("report covers %d task(s) with %d repeat(s)", len(rep.Tasks), rep.Repeats)
	}

	// Exactly one task is sound, so a check that simply said "ok" everywhere
	// would fail here.
	if got := rep.healthyCount(); got != 1 {
		t.Errorf("healthy = %d, want 1", got)
	}

	if status, detail := rep.check(t, "unsolvable", "solvability"); status != "failed" {
		t.Errorf("unsolvable/solvability = %q (%s), want failed", status, detail)
	} else if !strings.Contains(detail, "does not pass") {
		t.Errorf("the detail must explain the failure, got %q", detail)
	}
	if status, detail := rep.check(t, "already-solved", "necessity"); status != "failed" {
		t.Errorf("already-solved/necessity = %q (%s), want failed", status, detail)
	} else if !strings.Contains(detail, "reward for doing nothing") {
		t.Errorf("the detail must say why free reward is a defect, got %q", detail)
	}

	// The sound task must come out clean on every check; otherwise the audit is
	// only reporting that auditing happened.
	for _, name := range []string{"solvability", "necessity", "stability"} {
		if status, detail := rep.check(t, "sound", name); status != "ok" {
			t.Errorf("sound/%s = %q (%s), want ok", name, status, detail)
		}
	}

	// Three child runs: one baseline for necessity, two oracle repeats.
	roles := map[string]int{}
	for _, run := range rep.Runs {
		roles[run.Role]++
		if run.RunID == "" {
			t.Errorf("run %q has no id, so its evidence cannot be found", run.Role)
		}
	}
	if roles["baseline"] != 1 || roles["oracle-0"] != 1 || roles["oracle-1"] != 1 {
		t.Errorf("audit runs = %v, want one baseline and two oracle repeats", roles)
	}

	// The child runs are ordinary runs, labelled with a mode that says no agent
	// took part. Reading one of them as a model score is the mistake the column
	// exists to prevent.
	list := runCLI(t, "list", "--db", db)
	if list.exitCode != 0 {
		t.Fatalf("list exited %d: %s", list.exitCode, list.stderr)
	}
	for _, want := range []string{"(oracle)", "(baseline)"} {
		if !strings.Contains(list.stdout, want) {
			t.Errorf("`aeval list` does not mark %s runs:\n%s", want, list.stdout)
		}
	}
}

func TestCLIAuditStrictIsOptIn(t *testing.T) {
	work := t.TempDir()
	args := []string{"audit", "--dataset", auditFixture(), "--db", filepath.Join(work, "strict.db"),
		"--repeats", "1", "--only", "solvability"}

	// By default a defective dataset is a report, not a failure: otherwise CI
	// could not run an audit to find out what it has.
	if r := runCLI(t, args...); r.exitCode != 0 {
		t.Errorf("default audit exited %d, want 0:\n%s\n%s", r.exitCode, r.stdout, r.stderr)
	}
	// --strict turns the same finding into a non-zero exit.
	r := runCLI(t, append(args, "--strict")...)
	if r.exitCode != 1 {
		t.Errorf("--strict exited %d, want 1:\n%s\n%s", r.exitCode, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "unsolvable") {
		t.Errorf("--strict must still print the finding:\n%s", r.stdout)
	}
}

func TestCLIAuditRejectsAnUnknownCheckName(t *testing.T) {
	// A typo must not silently produce an audit that ran nothing and reported
	// every check as unknown, which reads like a broken dataset.
	r := runCLI(t, "audit", "--dataset", auditFixture(), "--only", "solvabilitiy",
		"--db", filepath.Join(t.TempDir(), "audit.db"))
	if r.exitCode != 2 {
		t.Errorf("exit code = %d, want 2 for a command-line error", r.exitCode)
	}
	if !strings.Contains(r.stderr, "solvabilitiy") {
		t.Errorf("the error should name the unknown check, got %q", r.stderr)
	}
}

// ---------------------------------------------------------------------------
// aeval serve --rl
//
// The environment service is the one part of the tool that acts rather than
// renders, and the only part a training loop talks to over the network. This
// drives it as a client would: over HTTP, against the real subprocess, with a
// real database behind it.
// ---------------------------------------------------------------------------

// writeRLConfig writes a configuration whose paths are absolute, so nothing
// depends on the shell's working directory.
//
// The agent is deliberately pointed at a binary that does not exist: a rollout
// must fail as a harness error, and no test may reach a real model.
func writeRLConfig(t *testing.T, work string) string {
	t.Helper()
	body := fmt.Sprintf(`dataset: %s
database: %s

run:
  work_root: %s
  backend:
    kind: local
  agent:
    kind: claude_code
    bin: %s
    model: e2e-no-model
`,
		filepath.Join(repoRoot, "tests", "testdata", "rlenv"),
		filepath.Join(work, "runs.db"),
		filepath.Join(work, "work"),
		filepath.Join(work, "no-such-agent"),
	)
	path := filepath.Join(work, "aeval.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// rlClient is a thin HTTP client for the RL routes.
type rlClient struct {
	t    *testing.T
	base string
}

func (c rlClient) do(method, path, body string, headers map[string]string) (int, string) {
	c.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(raw)
}

func (c rlClient) expect(want int, method, path, body string, headers map[string]string) string {
	c.t.Helper()
	code, raw := c.do(method, path, body, headers)
	if code != want {
		c.t.Fatalf("%s %s = %d, want %d:\n%s", method, path, code, want, raw)
	}
	return raw
}

func TestCLIServeDrivesAnRLEnvironmentEndToEnd(t *testing.T) {
	work := t.TempDir()
	cfgPath := writeRLConfig(t, work)

	addr := freeAddr(t)
	cmd := exec.Command(cliPath, "serve", "--config", cfgPath, "--addr", addr,
		"--rl", "--rl-allow-exec", "--rl-max-envs", "1")
	cmd.Dir = repoRoot
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + addr
	waitForHealth(t, base, &logs)
	c := rlClient{t: t, base: base}

	// --- discovery ---------------------------------------------------------
	var info struct {
		Dataset string `json:"dataset"`
		Tasks   int    `json:"tasks"`
		Stats   struct {
			MaxEnvs     int  `json:"max_envs"`
			ExecEnabled bool `json:"exec_enabled"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(c.expect(http.StatusOK, http.MethodGet, "/api/rl", "", nil)), &info); err != nil {
		t.Fatalf("decode /api/rl: %v", err)
	}
	if info.Dataset != "rlenv-fixture" || info.Tasks != 3 {
		t.Errorf("info = %+v", info)
	}
	if info.Stats.MaxEnvs != 1 || !info.Stats.ExecEnabled {
		t.Errorf("the budget a client is working within must be visible: %+v", info.Stats)
	}

	var tasks struct {
		Tasks []struct {
			ID         string `json:"id"`
			RewardKind string `json:"reward_kind"`
			HasOracle  bool   `json:"has_oracle"`
			Prompt     string `json:"prompt"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(c.expect(http.StatusOK, http.MethodGet, "/api/rl/tasks", "", nil)), &tasks); err != nil {
		t.Fatalf("decode /api/rl/tasks: %v", err)
	}
	if len(tasks.Tasks) != 3 || tasks.Tasks[0].ID != "subtract" || tasks.Tasks[2].RewardKind != "pass" {
		t.Errorf("tasks = %+v", tasks.Tasks)
	}
	for _, task := range tasks.Tasks {
		if task.Prompt == "" {
			t.Errorf("task %s carries no prompt, so every rollout costs a second round trip", task.ID)
		}
	}

	// --- refusals ----------------------------------------------------------
	c.expect(http.StatusNotFound, http.MethodGet, "/api/rl/envs/env-nope", "", nil)
	c.expect(http.StatusNotFound, http.MethodPost, "/api/rl/envs", `{"task_id":"nope"}`, nil)
	// A cross-origin request is refused even though the service is loopback-only:
	// any page the user has open can reach it.
	code, body := c.do(http.MethodGet, "/api/rl/tasks", "", map[string]string{"Origin": "http://evil.example"})
	if code != http.StatusForbidden {
		t.Errorf("cross-origin = %d, want 403: %s", code, body)
	}

	// --- the loop: open, act, verify ---------------------------------------
	var opened struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(c.expect(http.StatusCreated, http.MethodPost, "/api/rl/envs",
		`{"task_id":"subtract"}`, nil)), &opened); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if opened.ID == "" || opened.State != "open" {
		t.Fatalf("opened = %+v", opened)
	}
	envPath := "/api/rl/envs/" + opened.ID

	// The cap is enforced and reported as a cap, not as a task failure.
	if code, body := c.do(http.MethodPost, "/api/rl/envs", `{"task_id":"sparse"}`, nil); code != http.StatusTooManyRequests {
		t.Errorf("open past --rl-max-envs = %d, want 429: %s", code, body)
	}

	// A command runs and its exit code comes back as a result.
	c.expect(http.StatusOK, http.MethodPost, envPath+"/exec", `{"command":"echo e2e"}`, nil)

	// The policy submits its answer.
	c.expect(http.StatusOK, http.MethodPut, envPath+"/files",
		`{"path":"thing.py","content":"\"\"\"Two-argument arithmetic.\"\"\"\n\n\ndef calc(a, b):\n    \"\"\"Return the difference.\"\"\"\n    return a - b\n"}`,
		nil)

	var result struct {
		TaskID     string  `json:"task_id"`
		Reward     float64 `json:"reward"`
		RewardKind string  `json:"reward_kind"`
		Passed     bool    `json:"passed"`
		Rejected   bool    `json:"rejected"`
		StopReason string  `json:"stop_reason"`
		RunID      string  `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(c.expect(http.StatusOK, http.MethodPost, envPath+"/verify", "", nil)), &result); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if result.Reward != 1 || !result.Passed || result.RewardKind != "score" {
		t.Errorf("result = %+v, want the benchmark's own verdict", result)
	}
	if result.Rejected {
		t.Error("nothing was tampered with")
	}
	if result.RunID == "" {
		t.Error("the verdict must be stored, or its reward cannot be audited later")
	}

	// Verification is terminal: a policy must not be able to act after seeing
	// the reward, and the reward must not change if it asks again.
	if code, body := c.do(http.MethodPost, envPath+"/verify", "", nil); code != http.StatusConflict {
		t.Errorf("second verify = %d, want 409: %s", code, body)
	} else if !strings.Contains(body, `"reward"`) {
		t.Errorf("the 409 must carry the original verdict back: %s", body)
	}
	if code, _ := c.do(http.MethodPost, envPath+"/exec", `{"command":"true"}`, nil); code != http.StatusConflict {
		t.Errorf("exec after verify = %d, want 409", code)
	}

	// --- the evidence survives the environment ------------------------------
	//
	// The verified environment is a run like any other, so the report can render
	// the reward rather than asking the trainer to take it on trust.
	index := httpGet(t, base+"/")
	if !strings.Contains(index, result.RunID) {
		t.Errorf("the verified environment's run is not listed:\n%s", index)
	}
	runPayload := httpGet(t, base+"/api/runs/"+result.RunID)
	if !strings.Contains(runPayload, `"reward"`) {
		t.Errorf("the run payload carries no reward:\n%s", runPayload)
	}

	// --- a rollout with no reachable agent is a harness error --------------
	//
	// The configured binary does not exist, so the agent cannot start. That is a
	// measurement of the harness, not of a policy, and it must be reported as an
	// error the trainer discards rather than as a zero reward it learns from.
	var rollout struct {
		Reward float64 `json:"reward"`
		Error  string  `json:"error"`
	}
	if err := json.Unmarshal([]byte(c.expect(http.StatusOK, http.MethodPost, "/api/rl/rollouts",
		`{"task_id":"subtract"}`, nil)), &rollout); err != nil {
		t.Fatalf("decode rollout: %v", err)
	}
	if rollout.Error == "" {
		t.Errorf("rollout = %+v, want the harness failure reported", rollout)
	}
	if rollout.Reward != 0 {
		t.Errorf("reward = %v; a rollout that never ran must not be paid", rollout.Reward)
	}
}

// TestCLIServeTakesTheRLServiceFromTheConfigFile checks the second way to turn
// the service on.
//
// The flags and the `serve.rl` section are merged field by field, so this
// asserts the merge rather than just "the config file is read": one budget
// comes from the file, one from the command line, and the exec switch comes
// from the file alone. A section-level merge would silently drop one of them.
func TestCLIServeTakesTheRLServiceFromTheConfigFile(t *testing.T) {
	work := t.TempDir()
	base := writeRLConfig(t, work)

	// Append the section rather than rewriting the fixture: the interesting
	// part is that an existing config keeps working, not that a new one does.
	f, err := os.OpenFile(base, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := f.WriteString("serve:\n  rl:\n    enabled: true\n    allow_exec: true\n    max_execs: 17\n"); err != nil {
		t.Fatalf("append config: %v", err)
	}
	_ = f.Close()

	addr := freeAddr(t)
	cmd := exec.Command(cliPath, "serve", "--config", base, "--addr", addr, "--rl-max-envs", "2")
	cmd.Dir = repoRoot
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	baseURL := "http://" + addr
	waitForHealth(t, baseURL, &logs)

	var info struct {
		Stats struct {
			MaxEnvs     int  `json:"max_envs"`
			MaxExecs    int  `json:"max_execs"`
			ExecEnabled bool `json:"exec_enabled"`
		} `json:"stats"`
	}
	c := rlClient{t: t, base: baseURL}
	if err := json.Unmarshal([]byte(c.expect(http.StatusOK, http.MethodGet, "/api/rl", "", nil)), &info); err != nil {
		t.Fatalf("decode /api/rl: %v", err)
	}
	if info.Stats.MaxEnvs != 2 {
		t.Errorf("max_envs = %d, want 2 from the flag", info.Stats.MaxEnvs)
	}
	if info.Stats.MaxExecs != 17 {
		t.Errorf("max_execs = %d, want 17 from the config file; a flag for one field must not replace the section", info.Stats.MaxExecs)
	}
	if !info.Stats.ExecEnabled {
		t.Error("allow_exec came from the config file and was dropped")
	}

	// The flag route must still work when the config says nothing.
	if _, body := c.do(http.MethodGet, "/api/rl/tasks", "", nil); !strings.Contains(body, "subtract") {
		t.Errorf("the service started but serves no tasks: %s", body)
	}
}

// TestCLIServeRefusesToStartRLWithoutADataset checks the one piece of
// configuration that cannot be guessed. Serving no environments would look like
// a client-side bug from the trainer's side.
func TestCLIServeRefusesToStartRLWithoutADataset(t *testing.T) {
	r := runCLI(t, "serve", "--rl", "--db", filepath.Join(t.TempDir(), "runs.db"))
	if r.exitCode == 0 {
		t.Fatalf("serve --rl without a dataset started:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "dataset") {
		t.Errorf("the refusal should name what is missing, got %q", r.stderr)
	}
}

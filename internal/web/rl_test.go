package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/rlenv"
)

// ---------------------------------------------------------------------------
// Fixture
//
// The RL routes are the only ones on this server that start sandboxes, run the
// agent or execute commands, so the tests below are mostly about what the
// routes refuse to do: without a manager, from a foreign origin, without exec
// enabled, past a cap, on an environment that is already finished.
// ---------------------------------------------------------------------------

const rlFixtureDir = "../../tests/testdata/rlenv"

// rlFixedThing answers the `subtract` task.
const rlFixedThing = `"""Two-argument arithmetic."""


def calc(a, b):
    """Return the difference of a and b."""
    return a - b
`

func rlDataset(t *testing.T) *dataset.Dataset {
	t.Helper()
	ds, err := dataset.Load(rlFixtureDir)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(ds.Tasks) != 3 {
		t.Fatalf("fixture has %d tasks, want 3", len(ds.Tasks))
	}
	return ds
}

// rlEngineConfig is deliberately pointed at an agent that does not exist, so
// every test that does not install a mock agent proves by omission that the
// hand-driven routes never reach for it.
func rlEngineConfig(t *testing.T) engine.Config {
	t.Helper()
	inherit := false
	cfg := engine.Config{
		Agent: agent.Spec{Kind: agent.KindClaudeCode, Bin: filepath.Join(t.TempDir(), "no-such-agent")},
		Backend: engine.BackendConfig{
			Kind:       engine.BackendLocal,
			InheritEnv: &inherit,
		},
		WorkRoot:    t.TempDir(),
		ToolVersion: "test",
	}
	return cfg
}

// rlServer builds a server with the RL service mounted. mutate may adjust the
// manager options or the engine configuration before the manager is built.
func rlServer(t *testing.T, mutate func(*rlenv.Options, *engine.Config)) *httptest.Server {
	t.Helper()
	cfg := rlEngineConfig(t)
	opts := rlenv.Options{}
	if mutate != nil {
		mutate(&opts, &cfg)
	}
	m, err := rlenv.New(cfg, rlDataset(t), opts)
	if err != nil {
		t.Fatalf("rlenv.New: %v", err)
	}
	// Registered before the server so it runs after it: the listener is closed
	// first, then the environments are torn down.
	t.Cleanup(func() { m.CloseAll(context.Background()) })
	return newServer(t, func(c *Config) { c.RL = m })
}

// send issues a request with an optional JSON body and optional headers.
func send(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(raw)
}

func decodeInto(t *testing.T, body string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func rlOpen(t *testing.T, ts *httptest.Server, taskID string) string {
	t.Helper()
	res, body := send(t, ts, http.MethodPost, "/api/rl/envs",
		fmt.Sprintf(`{"task_id":%q}`, taskID), nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("open %s = %d %s", taskID, res.StatusCode, body)
	}
	var info struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	decodeInto(t, body, &info)
	if info.ID == "" || info.State != "open" {
		t.Fatalf("opened environment = %+v", info)
	}
	return info.ID
}

func rlWrite(t *testing.T, ts *httptest.Server, id, path, content string) {
	t.Helper()
	body := fmt.Sprintf(`{"path":%q,"content":%q}`, path, content)
	res, raw := send(t, ts, http.MethodPut, "/api/rl/envs/"+id+"/files", body, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("write %s = %d %s", path, res.StatusCode, raw)
	}
}

// ---------------------------------------------------------------------------
// Gate 1: no manager, no service
// ---------------------------------------------------------------------------

func TestRLRoutesRefuseEveryCallWithoutAManager(t *testing.T) {
	// The default server has no manager. Every RL path must answer the same way
	// -- a JSON 404 that names the flag -- rather than a bare 404 for GET and a
	// 405 for everything else, which a client would read as its own typo.
	ts := newServer(t, nil)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/rl"},
		{http.MethodGet, "/api/rl/tasks"},
		{http.MethodGet, "/api/rl/tasks/subtract"},
		{http.MethodGet, "/api/rl/envs"},
		{http.MethodPost, "/api/rl/envs"},
		{http.MethodGet, "/api/rl/envs/env-1"},
		{http.MethodGet, "/api/rl/envs/env-1/archive"},
		{http.MethodPost, "/api/rl/envs/env-1/exec"},
		{http.MethodPut, "/api/rl/envs/env-1/files"},
		{http.MethodPost, "/api/rl/envs/env-1/verify"},
		{http.MethodPost, "/api/rl/envs/env-1/delete"},
		{http.MethodPost, "/api/rl/rollouts"},
	}
	for _, p := range paths {
		res, body := send(t, ts, p.method, p.path, "", nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", p.method, p.path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s %s: content-type = %q, want JSON", p.method, p.path, ct)
		}
		if !strings.Contains(body, "--rl") {
			t.Errorf("%s %s: the refusal should name the flag that enables it, got %q", p.method, p.path, body)
		}
	}
}

func TestRLRoutesDoNotExistOnTheHTMLSide(t *testing.T) {
	// A refusal is a refusal: nothing about the RL service may appear in the
	// index page of a server that is not running one.
	ts := newServer(t, nil)
	res, body := get(t, ts, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index = %d", res.StatusCode)
	}
	if strings.Contains(body, "/api/rl") {
		t.Error("the run report must not advertise an RL service that is not running")
	}
}

// ---------------------------------------------------------------------------
// Gate 2: same-origin
// ---------------------------------------------------------------------------

func TestRLRoutesRejectAForeignOrigin(t *testing.T) {
	ts := rlServer(t, nil)
	for _, path := range []string{"/api/rl", "/api/rl/tasks", "/api/rl/envs"} {
		res, body := send(t, ts, http.MethodGet, path, "", map[string]string{
			"Origin": "http://evil.example",
		})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with a foreign origin = %d, want 403", path, res.StatusCode)
		}
		if !strings.Contains(body, "cross-origin") {
			t.Errorf("GET %s: body = %q", path, body)
		}
	}

	// The rule is a header check, not a method check: a state-changing request
	// from someone else's page must be refused too.
	res, _ := send(t, ts, http.MethodPost, "/api/rl/envs", `{"task_id":"subtract"}`,
		map[string]string{"Origin": "https://attacker.test"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("POST /api/rl/envs with a foreign origin = %d, want 403", res.StatusCode)
	}
}

func TestRLRoutesAcceptTheServersOwnOrigin(t *testing.T) {
	ts := rlServer(t, nil)
	res, body := send(t, ts, http.MethodGet, "/api/rl/tasks", "", map[string]string{
		"Origin": ts.URL,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("same-origin request = %d %s", res.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func TestRLInfoDescribesTheServiceAndItsLimits(t *testing.T) {
	ts := rlServer(t, nil)
	res, body := send(t, ts, http.MethodGet, "/api/rl", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/rl = %d %s", res.StatusCode, body)
	}
	var info struct {
		Dataset    string           `json:"dataset"`
		Tasks      int              `json:"tasks"`
		Stats      rlenv.Stats      `json:"stats"`
		Operations []map[string]any `json:"operations"`
	}
	decodeInto(t, body, &info)

	if info.Dataset != "rlenv-fixture" || info.Tasks != 3 {
		t.Errorf("info = %+v", info)
	}
	if info.Stats.MaxEnvs <= 0 || info.Stats.TTLSeconds <= 0 {
		t.Errorf("a client must be able to see the budget it is working within: %+v", info.Stats)
	}
	for _, op := range info.Operations {
		if op["path"] == "/api/rl/envs/{id}/exec" {
			t.Error("exec must not be advertised while it is disabled")
		}
	}
	if !hasOperation(info.Operations, "POST", "/api/rl/envs/{id}/verify") {
		t.Error("the verify route must be discoverable")
	}
}

func TestRLInfoAdvertisesExecOnlyWhenItIsEnabled(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, _ *engine.Config) { o.AllowExec = true })
	_, body := send(t, ts, http.MethodGet, "/api/rl", "", nil)

	var info struct {
		Operations []map[string]any `json:"operations"`
		Stats      rlenv.Stats      `json:"stats"`
	}
	decodeInto(t, body, &info)
	if !info.Stats.ExecEnabled {
		t.Error("stats must say exec is on, so a client knows why the routes exist")
	}
	if !hasOperation(info.Operations, "POST", "/api/rl/envs/{id}/exec") {
		t.Error("exec is enabled, so it must be advertised")
	}
	if !hasOperation(info.Operations, "PUT", "/api/rl/envs/{id}/files") {
		t.Error("the file route needs exec too, so it must be advertised alongside it")
	}
}

func TestRLTasksListsTheCurriculumInDatasetOrder(t *testing.T) {
	ts := rlServer(t, nil)
	res, body := send(t, ts, http.MethodGet, "/api/rl/tasks", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/rl/tasks = %d %s", res.StatusCode, body)
	}
	var payload struct {
		Tasks []rlenv.TaskInfo `json:"tasks"`
	}
	decodeInto(t, body, &payload)
	want := []string{"subtract", "tamper", "sparse"}
	if len(payload.Tasks) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(payload.Tasks), len(want))
	}
	for i, id := range want {
		if payload.Tasks[i].ID != id {
			t.Errorf("position %d = %q, want %q", i, payload.Tasks[i].ID, id)
		}
	}
	if payload.Tasks[0].RewardKind != "score" || payload.Tasks[2].RewardKind != "pass" {
		t.Errorf("the density must travel with the task: %+v", payload.Tasks)
	}
	if strings.Contains(body, "a - b") {
		t.Error("the task listing leaked the reference answer")
	}
}

func TestRLTaskMissingIsAJSON404(t *testing.T) {
	ts := rlServer(t, nil)
	res, body := send(t, ts, http.MethodGet, "/api/rl/tasks/no-such-task", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q; a program is the caller here", ct)
	}
	if !strings.Contains(body, "no-such-task") {
		t.Errorf("body = %q, want the id echoed back", body)
	}
}

// ---------------------------------------------------------------------------
// The rollout loop
// ---------------------------------------------------------------------------

func TestRLOpenVerifyFlow(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, _ *engine.Config) { o.AllowExec = true })

	id := rlOpen(t, ts, "subtract")
	rlWrite(t, ts, id, "thing.py", rlFixedThing)

	res, body := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/verify", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("verify = %d %s", res.StatusCode, body)
	}
	var result rlenv.Result
	decodeInto(t, body, &result)
	if result.Reward != 1 || !result.Passed || result.RewardKind != "score" {
		t.Errorf("result = %+v, want the dense contract paid in full", result)
	}
	if result.EnvID != id {
		t.Errorf("env id = %q, want %q", result.EnvID, id)
	}

	// A verification is terminal, and asking again returns the same verdict
	// rather than grading twice: this is what stops a policy seeing the reward
	// and then acting again.
	res, body = send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/verify", "", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second verify = %d %s, want 409", res.StatusCode, body)
	}
	var conflict struct {
		Error  string        `json:"error"`
		Result *rlenv.Result `json:"result"`
	}
	decodeInto(t, body, &conflict)
	if conflict.Result == nil || conflict.Result.Reward != result.Reward {
		t.Errorf("409 must carry the original verdict, got %+v", conflict.Result)
	}

	res, body = send(t, ts, http.MethodGet, "/api/rl/envs/"+id, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET env = %d %s", res.StatusCode, body)
	}
	var info rlenv.EnvInfo
	decodeInto(t, body, &info)
	if info.State != "verified" || info.Result == nil {
		t.Errorf("env after verify = %+v", info)
	}
}

func TestRLOpenRejectsAnUnknownTaskWith404(t *testing.T) {
	ts := rlServer(t, nil)
	res, body := send(t, ts, http.MethodPost, "/api/rl/envs", `{"task_id":"nope"}`, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; a client that got a 201 here would drive an environment that is not the task it asked for", res.StatusCode)
	}
	if !strings.Contains(body, "nope") {
		t.Errorf("body = %q", body)
	}
}

func TestRLControlPlaneRefusesBodiesItCannotTrust(t *testing.T) {
	ts := rlServer(t, nil)

	// A misspelled field: a trainer that gets a 201 for the wrong environment
	// is worse off than one that gets a 400 and finds out immediately.
	res, body := send(t, ts, http.MethodPost, "/api/rl/envs", `{"task":"subtract"}`, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field = %d %s, want 400", res.StatusCode, body)
	}

	res, body = send(t, ts, http.MethodPost, "/api/rl/envs", "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body = %d %s, want 400", res.StatusCode, body)
	}

	// Malformed JSON must not reach the manager.
	res, body = send(t, ts, http.MethodPost, "/api/rl/envs", `{"task_id":`, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("truncated JSON = %d %s, want 400", res.StatusCode, body)
	}

	// The body is bounded, so a training loop that posts a dataset by mistake
	// cannot make the harness allocate it.
	huge := fmt.Sprintf(`{"task_id":%q}`, strings.Repeat("x", 200<<10))
	res, body = send(t, ts, http.MethodPost, "/api/rl/envs", huge, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body = %d, want 400", res.StatusCode)
	}
	_ = body
}

func TestRLNonJSONBodyIsRefused(t *testing.T) {
	ts := rlServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/rl/envs", bytes.NewBufferString("task_id=subtract"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Gate 3: exec
// ---------------------------------------------------------------------------

func TestRLExecIsForbiddenUntilItIsEnabled(t *testing.T) {
	ts := rlServer(t, nil)
	id := rlOpen(t, ts, "subtract")

	res, body := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/exec", `{"command":"true"}`, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("exec = %d %s, want 403", res.StatusCode, body)
	}
	if !strings.Contains(body, "--rl-allow-exec") {
		t.Errorf("the refusal must name the flag, got %q", body)
	}

	res, body = send(t, ts, http.MethodPut, "/api/rl/envs/"+id+"/files", `{"path":"x","content":"y"}`, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("write file = %d %s, want 403", res.StatusCode, body)
	}
}

func TestRLExecReportsTheExitCodeAsAResult(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, _ *engine.Config) { o.AllowExec = true })
	id := rlOpen(t, ts, "subtract")

	res, body := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/exec", `{"command":"echo hi"}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("exec = %d %s", res.StatusCode, body)
	}
	var ok struct {
		Exec *rlenv.ExecLog `json:"exec"`
	}
	decodeInto(t, body, &ok)
	if ok.Exec == nil || ok.Exec.ExitCode != 0 || !strings.Contains(ok.Exec.Output, "hi") {
		t.Errorf("exec = %+v", ok.Exec)
	}

	// A command that ran and failed is a normal result: the exit code is the
	// answer. Turning it into an HTTP error would tell a training loop its
	// request was broken when the policy simply ran a failing command.
	res, body = send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/exec", `{"command":"exit 4"}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("failing command = %d %s, want 200", res.StatusCode, body)
	}
	var bad struct {
		Exec *rlenv.ExecLog `json:"exec"`
	}
	decodeInto(t, body, &bad)
	if bad.Exec == nil || bad.Exec.ExitCode != 4 {
		t.Errorf("exec = %+v, want exit code 4", bad.Exec)
	}
}

func TestRLArchiveCarriesTheEvidence(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, _ *engine.Config) { o.AllowExec = true })
	id := rlOpen(t, ts, "subtract")
	rlWrite(t, ts, id, "thing.py", rlFixedThing)
	if res, body := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/exec", `{"command":"echo evidence"}`, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("exec = %d %s", res.StatusCode, body)
	}

	res, body := send(t, ts, http.MethodGet, "/api/rl/envs/"+id+"/archive", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("archive = %d %s", res.StatusCode, body)
	}
	var arch rlenv.Archive
	decodeInto(t, body, &arch)
	if len(arch.Execs) != 1 || !strings.Contains(arch.Execs[0].Output, "evidence") {
		t.Errorf("execs = %+v, want the command kept as evidence", arch.Execs)
	}
	if !containsPath(arch.Changed, "thing.py") {
		t.Errorf("changed = %v, want the write reflected", arch.Changed)
	}
}

func TestRLArchiveOfAnUnknownEnvIs404(t *testing.T) {
	ts := rlServer(t, nil)
	res, _ := send(t, ts, http.MethodGet, "/api/rl/envs/env-nope/archive", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Caps
// ---------------------------------------------------------------------------

func TestRLEnvironmentCapIsReportedAsTooManyRequests(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, _ *engine.Config) { o.MaxEnvs = 1 })
	rlOpen(t, ts, "subtract")

	res, body := send(t, ts, http.MethodPost, "/api/rl/envs", `{"task_id":"sparse"}`, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second open = %d %s, want 429", res.StatusCode, body)
	}
	// A cap is an operational event. Reporting it as a task failure would
	// corrupt the training signal, and as a 500 would make a client retry a
	// call that cannot succeed until something else closes.
	if !strings.Contains(body, "already open") {
		t.Errorf("body = %q, want the reason spelled out", body)
	}
}

func TestRLRolloutCapIsReportedAsTooManyRequests(t *testing.T) {
	ts := rlServer(t, func(o *rlenv.Options, cfg *engine.Config) {
		o.MaxRollouts = 1
		cfg.Agent = agent.Spec{Kind: agent.KindMock, Bin: `printf 'x' > thing.py`, Model: "mock"}
	})

	res, body := send(t, ts, http.MethodPost, "/api/rl/rollouts", `{"task_id":"subtract"}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first rollout = %d %s", res.StatusCode, body)
	}
	res, body = send(t, ts, http.MethodPost, "/api/rl/rollouts", `{"task_id":"subtract"}`, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second rollout = %d %s, want 429", res.StatusCode, body)
	}
}

func TestRLDeleteDiscardsAnEnvironment(t *testing.T) {
	ts := rlServer(t, nil)
	id := rlOpen(t, ts, "subtract")

	res, body := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/delete", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, id) {
		t.Errorf("body = %q, want the id echoed", body)
	}
	if res, _ := send(t, ts, http.MethodGet, "/api/rl/envs/"+id, "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", res.StatusCode)
	}

	// The delete is idempotent from a client's point of view in the sense that
	// it is a 404 the second time, not a 500.
	if res, _ := send(t, ts, http.MethodPost, "/api/rl/envs/"+id+"/delete", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", res.StatusCode)
	}
}

func TestRLVerifyOnAnUnknownEnvironmentIs404(t *testing.T) {
	ts := rlServer(t, nil)
	res, _ := send(t, ts, http.MethodPost, "/api/rl/envs/env-nope/verify", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestRLListShowsOpenEnvironments(t *testing.T) {
	ts := rlServer(t, nil)
	id := rlOpen(t, ts, "tamper")

	res, body := send(t, ts, http.MethodGet, "/api/rl/envs", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	var payload struct {
		Envs  []rlenv.EnvInfo `json:"envs"`
		Stats rlenv.Stats     `json:"stats"`
	}
	decodeInto(t, body, &payload)
	if len(payload.Envs) != 1 || payload.Envs[0].ID != id {
		t.Fatalf("envs = %+v", payload.Envs)
	}
	if payload.Stats.OpenEnvs != 1 {
		t.Errorf("open envs = %d, want 1", payload.Stats.OpenEnvs)
	}
	if payload.Envs[0].Prompt == "" {
		t.Error("the listing must carry the prompt, or every rollout costs a second round trip")
	}
}

func TestRLRolloutReturnsTheBenchmarkReward(t *testing.T) {
	ts := rlServer(t, func(_ *rlenv.Options, cfg *engine.Config) {
		// A mock agent that writes the answer, standing in for a policy.
		cfg.Agent = agent.Spec{
			Kind:  agent.KindMock,
			Bin:   `printf '"""Two-argument arithmetic."""\n\n\ndef calc(a, b):\n    """Return the difference."""\n    return a - b\n' > thing.py`,
			Model: "mock",
		}
	})

	res, body := send(t, ts, http.MethodPost, "/api/rl/rollouts", `{"task_id":"subtract"}`, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rollout = %d %s", res.StatusCode, body)
	}
	var result rlenv.Result
	decodeInto(t, body, &result)
	if result.Reward != 1 || !result.Passed {
		t.Errorf("result = %+v, want the same reward an evaluated cell would get", result)
	}
	if result.RunID == "" {
		t.Error("a rollout must be stored, so its reward can be audited later")
	}
}

func TestRLRolloutOfAnUnknownTaskIs404(t *testing.T) {
	ts := rlServer(t, nil)
	res, _ := send(t, ts, http.MethodPost, "/api/rl/rollouts", `{"task_id":"nope"}`, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hasOperation(ops []map[string]any, method, path string) bool {
	for _, op := range ops {
		if op["method"] == method && op["path"] == path {
			return true
		}
	}
	return false
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want || strings.HasSuffix(p, "/"+want) {
			return true
		}
	}
	return false
}

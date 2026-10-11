package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/storage"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func seededStore(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	run := &storage.RunRecord{
		ID: "run-1", Name: "first run", Status: storage.StatusRunning,
		DatasetName: "demo", DatasetVersion: "1.0.0", DatasetHash: "sha256:aaa",
		AgentKind: "mock", SandboxBackend: "local", SamplesPerTask: 1,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	grades := mustJSON(t, []grader.Result{{Grader: "patch-applied", Type: "patch", Passed: true, Score: 1, Weight: 1, Required: true, Reason: "ok"}})
	for i, id := range []string{"alpha", "beta"} {
		rec := &storage.TaskRunRecord{
			ID: "run-1/" + id + "#0", RunID: "run-1", TaskID: id, SampleIndex: 0,
			Status: storage.TaskStatusCompleted, StopReason: "completed",
			Passed: i == 0, Score: float64(i), Category: "bugfix", Threshold: 1,
			Diff: "diff --git a/x b/x\n", GradesJSON: grades,
		}
		if err := store.SaveTaskRun(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishRun(ctx, "run-1", storage.StatusCompleted, "", time.Second); err != nil {
		t.Fatal(err)
	}
	metrics := []storage.MetricRecord{
		{RunID: "run-1", Name: "resolve_rate", Value: 0.5, Unit: "ratio", Kind: "run", ExtraJSON: `{"passed":1,"total":2}`},
		{RunID: "run-1", Name: "total_cost_usd", Value: 0.5, Unit: "usd", Kind: "run"},
	}
	if err := store.SaveMetrics(ctx, "run-1", metrics); err != nil {
		t.Fatal(err)
	}
	return store
}

func secondRun(t *testing.T, store *storage.Store) {
	t.Helper()
	ctx := context.Background()
	// Make the second run strictly newer so ListRuns orders it first.
	time.Sleep(5 * time.Millisecond)
	run := &storage.RunRecord{
		ID: "run-2", Name: "second run", Status: storage.StatusRunning,
		DatasetName: "demo", DatasetHash: "sha256:aaa", AgentKind: "mock", SandboxBackend: "local",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	rec := &storage.TaskRunRecord{
		ID: "run-2/alpha#0", RunID: "run-2", TaskID: "alpha", Status: storage.TaskStatusCompleted,
		Passed: true, Score: 1, Threshold: 1,
	}
	if err := store.SaveTaskRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, "run-2", storage.StatusCompleted, "", time.Second)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func newServer(t *testing.T, mutate func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{
		Store:  seededStore(t),
		Title:  "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

func TestIndexPageListsRuns(t *testing.T) {
	ts := newServer(t, func(c *Config) { secondRun(t, c.Store) })
	res, body := get(t, ts, "/")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(body, `data-mode="index"`) {
		t.Error("index mode missing")
	}
	for _, id := range []string{"run-1", "run-2"} {
		if !strings.Contains(body, id) {
			t.Errorf("run %s missing from the listing", id)
		}
	}
	// Newest first: the order matters because it is how someone finds the run
	// they just finished.
	if strings.Index(body, "run-2") > strings.Index(body, "run-1") {
		t.Error("runs are not ordered newest first")
	}
	if !strings.Contains(body, `href="/static/app.css"`) {
		t.Error("served pages should link the stylesheet rather than inline it")
	}
	if strings.Contains(body, "data-delete=") {
		t.Error("the delete control must not be offered when deletion is disabled")
	}
}

func TestRunPageRenders(t *testing.T) {
	ts := newServer(t, nil)
	res, body := get(t, ts, "/runs/run-1")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(body, `data-mode="run"`) {
		t.Error("run mode missing")
	}
	if !strings.Contains(body, "window.__AEVAL_PAYLOAD__ =") {
		t.Error("payload missing")
	}
	if !strings.Contains(body, `/runs/run-1`) {
		t.Error("navigation link missing")
	}
	if !strings.Contains(body, `class="active"`) {
		t.Error("active navigation state missing")
	}
}

func TestRunPageReportsAMissingRun(t *testing.T) {
	ts := newServer(t, nil)
	res, body := get(t, ts, "/runs/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(body, "no such run") {
		t.Fatalf("body = %q", body)
	}
}

func TestUnknownPathIsNotFoundRatherThanTheIndex(t *testing.T) {
	// ServeMux routes every unmatched path to "GET /", so without an explicit
	// check a typo would silently render the index.
	ts := newServer(t, nil)
	res, _ := get(t, ts, "/whatever")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestRunningRunIsVisibleWhileInFlight(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(":memory:")
	defer store.Close()
	if err := store.CreateRun(ctx, &storage.RunRecord{ID: "live", Status: storage.StatusRunning, DatasetName: "d"}); err != nil {
		t.Fatal(err)
	}
	// One cell saved, the run still marked running: a person watching a long
	// evaluation must see partial progress, which is why nothing is cached.
	if err := store.SaveTaskRun(ctx, &storage.TaskRunRecord{
		ID: "live/a#0", RunID: "live", TaskID: "a", Status: storage.TaskStatusCompleted, Passed: true,
	}); err != nil {
		t.Fatal(err)
	}

	srv, _ := New(Config{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	_, body := get(t, ts, "/runs/live")
	if !strings.Contains(body, `"status":"running"`) {
		t.Error("a run in flight should report its status truthfully")
	}
	if !strings.Contains(body, `"passed":1`) {
		t.Error("partial results missing")
	}
}

func TestComparePage(t *testing.T) {
	ts := newServer(t, func(c *Config) { secondRun(t, c.Store) })

	res, body := get(t, ts, "/compare")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing parameters should be rejected, got %d", res.StatusCode)
	}

	res, body = get(t, ts, "/compare?left=run-1&right=run-2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `data-mode="compare"`) {
		t.Error("compare mode missing")
	}
	if !strings.Contains(body, `"compare"`) {
		t.Error("comparison payload missing")
	}
}

func TestAssetsAreServed(t *testing.T) {
	ts := newServer(t, nil)
	res, body := get(t, ts, "/static/app.css")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "--accent") {
		t.Fatalf("css: status=%d len=%d", res.StatusCode, len(body))
	}
	res, body = get(t, ts, "/static/app.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "__AEVAL_PAYLOAD__") {
		t.Fatalf("js: status=%d len=%d", res.StatusCode, len(body))
	}
}

func TestHealthz(t *testing.T) {
	ts := newServer(t, nil)
	res, body := get(t, ts, "/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(body, `"ok": true`) {
		t.Fatalf("body = %q", body)
	}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// TestAPIAndPageCarryTheSamePayload is the property that lets the file report
// and the served report be treated as one artifact: the JSON endpoint must
// return exactly the bytes the page embeds.
func TestAPIAndPageCarryTheSamePayload(t *testing.T) {
	ts := newServer(t, nil)

	res, apiBody := get(t, ts, "/api/runs/run-1")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}

	_, pageBody := get(t, ts, "/runs/run-1")
	embedded, err := embeddedPayload(pageBody)
	if err != nil {
		t.Fatalf("extract embedded payload: %v", err)
	}

	// The API response is pretty-printed for humans; compare parsed values.
	var fromAPI, fromPage map[string]any
	if err := json.Unmarshal([]byte(apiBody), &fromAPI); err != nil {
		t.Fatalf("api payload is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(embedded), &fromPage); err != nil {
		t.Fatalf("embedded payload is not JSON: %v", err)
	}
	apiNorm, _ := json.Marshal(fromAPI)
	pageNorm, _ := json.Marshal(fromPage)
	if string(apiNorm) != string(pageNorm) {
		t.Fatalf("the API and the page disagree about the same run")
	}
	if fromAPI["run"].(map[string]any)["id"] != "run-1" {
		t.Fatalf("wrong run returned: %v", fromAPI["run"])
	}
}

func TestAPIRunNotFoundIsJSON(t *testing.T) {
	ts := newServer(t, nil)
	res, body := get(t, ts, "/api/runs/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("an API error should be JSON, got %q", ct)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if payload["error"] == nil {
		t.Fatalf("error body = %v", payload)
	}
}

func TestAPITask(t *testing.T) {
	ts := newServer(t, nil)

	res, body := get(t, ts, "/api/runs/run-1/tasks/alpha")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}
	var task map[string]any
	if err := json.Unmarshal([]byte(body), &task); err != nil {
		t.Fatal(err)
	}
	if task["id"] != "alpha" {
		t.Fatalf("task = %v", task["id"])
	}
	if task["passed"] != true {
		t.Fatalf("passed = %v", task["passed"])
	}

	res, body = get(t, ts, "/api/runs/run-1/tasks/missing")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task status = %d", res.StatusCode)
	}
	var errBody map[string]any
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if errBody["error"] == nil {
		t.Fatalf("error body = %v", errBody)
	}
}

func TestAPITaskRejectsABadSample(t *testing.T) {
	ts := newServer(t, nil)
	res, _ := get(t, ts, "/api/runs/run-1/tasks/alpha?sample=-1")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	res, _ = get(t, ts, "/api/runs/run-1/tasks/alpha?sample=x")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestAPIListIncludesCapabilities(t *testing.T) {
	ts := newServer(t, func(c *Config) { c.AllowDelete = true })
	_, body := get(t, ts, "/api/runs")
	var idx map[string]any
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		t.Fatal(err)
	}
	caps, _ := idx["capabilities"].(map[string]any)
	if caps == nil || caps["delete"] != true {
		t.Fatalf("capabilities = %v", idx["capabilities"])
	}
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

func post(t *testing.T, ts *httptest.Server, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func TestDeleteIsOffByDefault(t *testing.T) {
	ts := newServer(t, nil)
	res, body := post(t, ts, "/api/runs/run-1/delete", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if !strings.Contains(body, "disabled") {
		t.Fatalf("body = %q", body)
	}
	// And the run must still be there.
	if res, _ := get(t, ts, "/api/runs/run-1"); res.StatusCode != http.StatusOK {
		t.Fatal("a refused delete must not have removed anything")
	}
}

func TestDeleteRejectsAForeignOrigin(t *testing.T) {
	// A page the user happens to have open must not be able to POST to a
	// loopback service and wipe their results.
	ts := newServer(t, func(c *Config) { c.AllowDelete = true })
	res, body := post(t, ts, "/api/runs/run-1/delete", map[string]string{"Origin": "https://evil.example"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "cross-origin") {
		t.Fatalf("body = %q", body)
	}
	if res, _ := get(t, ts, "/api/runs/run-1"); res.StatusCode != http.StatusOK {
		t.Fatal("a refused delete must not have removed anything")
	}
}

func TestDeleteRemovesTheRunAndItsChildren(t *testing.T) {
	store := seededStore(t)
	srv, err := New(Config{Store: store, AllowDelete: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	res, body := post(t, ts, "/api/runs/run-1/delete", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}

	ctx := context.Background()
	if _, err := store.GetRun(ctx, "run-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("run survived deletion: %v", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"runs", "task_runs", "grader_results"} {
		if stats[table] != 0 {
			t.Errorf("%s still has %d row(s) after a cascading delete", table, stats[table])
		}
	}

	res, _ = post(t, ts, "/api/runs/run-1/delete", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting twice should report not-found, got %d", res.StatusCode)
	}
}

func TestDeleteAllowsTheServersOwnOrigin(t *testing.T) {
	ts := newServer(t, func(c *Config) { c.AllowDelete = true })
	host := strings.TrimPrefix(ts.URL, "http://")
	res, body := post(t, ts, "/api/runs/run-1/delete", map[string]string{"Origin": "http://" + host})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Unit
// ---------------------------------------------------------------------------

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:8787", "127.0.0.1:8787", true},
		{"https://127.0.0.1:8787", "127.0.0.1:8787", true},
		{"http://LOCALHOST:8787", "localhost:8787", true},
		{"http://evil.example", "127.0.0.1:8787", false},
		{"http://127.0.0.1:9999", "127.0.0.1:8787", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.origin, c.host); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestNewRequiresAStore(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a server without a store would serve nothing")
	}
}

func TestAddrDefaultsToLoopback(t *testing.T) {
	srv, err := New(Config{Store: seededStore(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr() != DefaultAddr {
		t.Fatalf("addr = %q, want %q", srv.Addr(), DefaultAddr)
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Fatalf("the default bind address must be loopback, got %q", srv.Addr())
	}
}

func TestLiveRefreshIsConfiguredByCapability(t *testing.T) {
	srv, err := New(Config{
		Store:              seededStore(t),
		LiveRefreshSeconds: 5,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, body := get(t, ts, "/")
	if !strings.Contains(body, `http-equiv="refresh" content="5"`) {
		t.Fatal("live refresh not applied")
	}
	if !strings.Contains(body, `"live_refresh_seconds":5`) {
		t.Fatal("live refresh missing from the payload")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func embeddedPayload(html string) (string, error) {
	const marker = "window.__AEVAL_PAYLOAD__ = "
	i := strings.Index(html, marker)
	if i < 0 {
		return "", errors.New("payload marker not found")
	}
	rest := html[i+len(marker):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		return "", errors.New("payload terminator not found")
	}
	seg := strings.TrimRight(rest[:end], " \t\r\n")
	return strings.TrimSuffix(seg, ";"), nil
}

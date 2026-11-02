package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// These are the shape a collected task has: a run that recorded its dataset, a
// cell that ran and was not scored, and a dataset that names the grader which
// can score it. That is exactly the state the grade button exists for.
// ---------------------------------------------------------------------------

func gradableDataset(t *testing.T, graders string) string {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "dataset.yaml"),
		"name: grade-fixture\nversion: 1.0.0\ntasks_file: tasks.jsonl\n")
	writeFixture(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"t1","prompt":"answer the question",`+
			`"workspace":{"type":"files","files":{"a.txt":"x"}},`+
			`"output":{"type":"stdout"},"graders":`+graders+`}`+"\n")
	return dir
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// addGradableRun records a finished run whose one cell ran but was never
// scored, which is what collect produces without a grader.
func addGradableRun(t *testing.T, st *store.Store, id, datasetPath string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateRun(ctx, store.Run{
		ID: id, Name: "collected", Status: store.RunRunning,
		StartedAt:   time.Now(),
		DatasetName: "grade-fixture", DatasetPath: datasetPath,
		AgentKind: "claude_code", Backend: "local", Samples: 1, Concurrency: 1,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if err := st.FinishRun(ctx, id, store.RunCompleted, ""); err != nil {
		t.Fatalf("finish run %s: %v", id, err)
	}
	if err := st.SaveAttempt(ctx, store.Attempt{
		RunID: id, TaskID: "t1", Status: store.CellOK, StopReason: "completed",
		Graded: false, DurationMs: 1200, Answer: "done\n",
	}); err != nil {
		t.Fatalf("save cell: %v", err)
	}
}

// registerCompare records a comparison report, which is the kind of page that
// has no single cell to grade.
func registerCompare(t *testing.T, st *store.Store, left, right string) store.Report {
	t.Helper()
	ctx := context.Background()
	opts := report.Options{}.Resolve()
	data, err := report.LoadComparison(ctx, st, left, right, opts)
	if err != nil {
		t.Fatalf("load comparison: %v", err)
	}
	reg, err := report.Save(ctx, st, report.Request{
		Kind:    store.ReportKindCompare,
		Title:   data.Title,
		Subject: left + " vs " + right,
		Params:  map[string]string{"left": left, "right": right},
		Options: opts,
	}, data)
	if err != nil {
		t.Fatalf("register comparison: %v", err)
	}
	return reg
}

var csrfPattern = regexp.MustCompile(`data-grade-csrf="([0-9a-f]+)"`)

// waitForGrades blocks until the server has finished what it accepted.
//
// Grading is off the request now, so a test that reads the row the moment the
// POST returns would be asserting on a job that has not run yet. A browser has
// the same gap and the page closes it by refreshing; a test closes it here.
func waitForGrades(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.WaitGrades(ctx); err != nil {
		t.Fatalf("grading did not finish: %v", err)
	}
}

// tokenOf reads the form token out of the page, which is how a browser gets it:
// the test has no other channel, in the same way an attacker on another origin
// has none.
func tokenOf(t *testing.T, page string) string {
	t.Helper()
	m := csrfPattern.FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatalf("the page carries no form token:\n%s", page)
	}
	return m[1]
}

func attempt(t *testing.T, st *store.Store, runID, taskID string) store.Attempt {
	t.Helper()
	rows, err := st.ListAttempts(context.Background(), runID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	for _, a := range rows {
		if a.TaskID == taskID {
			return a
		}
	}
	t.Fatalf("no cell for %s", taskID)
	return store.Attempt{}
}

// ---------------------------------------------------------------------------
// What the page offers
// ---------------------------------------------------------------------------

// The control has to be on a served page and absent from a saved one. The
// difference is the presence of a server, not a flag: a report attached to an
// email has nothing to post to, and a button that cannot work is worse than no
// button.
func TestTheGradeControlIsOfferedOnlyWhereThereIsAServer(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()

	page := get(t, h, "/r/"+reg.ID, nil).Body.String()
	if !strings.Contains(page, `data-grade-url="/r/`+reg.ID+`/grade"`) {
		t.Errorf("the served page does not offer to grade:\n%s", page)
	}
	if got := tokenOf(t, page); got == "" {
		t.Error("the form token is empty, so every submission would be refused")
	}
	if !strings.Contains(page, `data-grade-graders="auto,llm_judge,patch"`) {
		t.Errorf("the picker is not told which graders exist:\n%s", page)
	}

	// The same payload rendered as a file: no server, so no control.
	opts := report.Options{}.Resolve()
	data, err := report.Load(context.Background(), st, "r1", opts)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	var buf bytes.Buffer
	if err := report.RenderRun(&buf, data, report.RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "data-grade-") {
		t.Error("a document meant to be inert carries a grade control")
	}
}

// A comparison spans two runs, so "which cell" is a question the page does not
// answer. Refusing at the page is better than accepting a POST that would have
// to guess.
func TestAComparisonOffersNoGradeControl(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	addRun(t, st, "r2", "release")
	reg := registerCompare(t, st, "r1", "r2")

	page := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil).Body.String()
	if strings.Contains(page, "data-grade-url") {
		t.Error("a comparison page offers a control for a cell it does not have")
	}
}

// ---------------------------------------------------------------------------
// Grading
// ---------------------------------------------------------------------------

// The case the feature exists for: a cell that ran and was never scored, scored
// from its own page, with the run left alone.
func TestGradingACellFromItsPageWritesTheVerdict(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done","normalize":["trim"]}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()

	page := get(t, h, "/r/"+reg.ID, nil).Body.String()
	token := tokenOf(t, page)
	if before := attempt(t, st, "r1", "t1"); before.Graded {
		t.Fatal("the fixture cell is already graded, so this test proves nothing")
	}

	rec := post(h, "/r/"+reg.ID+"/grade", url.Values{
		"csrf": {token}, "task": {"t1"}, "sample": {"0"}, "grader": {"auto"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	// The redirect carries the cell and the anchor, so the reader lands on the
	// table rather than at the top of a document they have already read.
	if want := "/r/" + reg.ID + "?graded=t1&sample=0#tasks"; rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}

	// The request is answered before the verdict exists: that is the point of
	// the change. The page the reader lands on says so.
	if graded := attempt(t, st, "r1", "t1"); graded.Graded {
		t.Error("the POST waited for the verdict; grading is supposed to be off the request")
	}
	waitForGrades(t, srv)

	after := attempt(t, st, "r1", "t1")
	if !after.Graded || !after.Passed || after.Score != 1 {
		t.Errorf("cell = graded %v passed %v score %v, want 1/true/1", after.Graded, after.Passed, after.Score)
	}
	if len(after.Grades) != 1 || after.Grades[0].Type != "exact_match" {
		t.Errorf("graders = %+v, want the dataset's own exact_match", after.Grades)
	}
	// The measurement is what the run made, and grading does not re-measure it.
	if after.DurationMs != 1200 || after.Answer != "done\n" {
		t.Errorf("the re-grade rewrote the measurement: %+v", after)
	}

	// And the page the reader comes back to says what happened: the verdict,
	// and that the document no longer reproduces the registration.
	back := get(t, h, rec.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(back, "The verdict on record is PASS.") {
		t.Errorf("the page after grading does not report the verdict:\n%s", back)
	}
	if !strings.Contains(back, "not the recorded document") {
		t.Errorf("the page still claims to be the recorded document after it changed:\n%s", back)
	}
}

// The banner is composed from the row, never from the URL. A link can carry an
// id; it cannot make the page assert a verdict the database does not hold.
func TestTheNoticeStatesTheRowAndNotTheURL(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()

	page := get(t, h, "/r/"+reg.ID+"?graded=t1&sample=0", nil).Body.String()
	if !strings.Contains(page, "The cell still holds no verdict.") {
		t.Errorf("an ungraded row was described as something else:\n%s", page)
	}
	if strings.Contains(page, "The verdict on record is PASS.") {
		t.Error("a URL claimed a verdict the row does not hold")
	}

	unknown := get(t, h, "/r/"+reg.ID+"?graded=no-such-task", nil).Body.String()
	if strings.Contains(unknown, "The verdict on record") || strings.Contains(unknown, "still holds no verdict") {
		t.Error("a task the run never had produced a verdict banner")
	}
}

// Grading writes, so the three checks that guard a write apply. Each of these
// is a real way a browser reaches this route without the reader meaning to
// press the button.
func TestGradingRefusesWhatTheBrowserShouldNotSend(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())
	path := "/r/" + reg.ID + "/grade"

	t.Run("no token", func(t *testing.T) {
		rec := post(h, path, url.Values{"task": {"t1"}}, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("a token from another server", func(t *testing.T) {
		rec := post(h, path, url.Values{"csrf": {"deadbeef"}, "task": {"t1"}}, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("another origin", func(t *testing.T) {
		rec := post(h, path, url.Values{"csrf": {token}, "task": {"t1"}},
			map[string]string{"Origin": "https://evil.example"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("a rebound host name", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(url.Values{"csrf": {token}, "task": {"t1"}}.Encode()))
		req.Host = "eval.attacker.example" // resolved to 127.0.0.1 by the attacker
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	// None of the four wrote anything, which is the point of refusing them.
	if got := attempt(t, st, "r1", "t1"); got.Graded {
		t.Error("a refused request wrote a verdict")
	}
}

func TestGradingRefusesAnUnknownGraderByName(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	rec := post(h, "/r/"+reg.ID+"/grade",
		url.Values{"csrf": {token}, "task": {"t1"}, "grader": {"magic"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "magic") {
		t.Errorf("the refusal does not name what was sent:\n%s", body)
	}
	// It has to say what it would have accepted, or the reader is left guessing
	// at a vocabulary the page never showed them.
	if !strings.Contains(body, "llm_judge") || !strings.Contains(body, "auto") {
		t.Errorf("the refusal does not list the accepted graders:\n%s", body)
	}
	// And the way back, because the reader arrived from a report.
	if !strings.Contains(body, "/r/"+reg.ID) {
		t.Errorf("the refusal has no link back to the report:\n%s", body)
	}
	if got := attempt(t, st, "r1", "t1"); got.Graded {
		t.Error("a refused grader wrote a verdict")
	}
}

func TestGradingAComparisonIsRefusedWithTheReason(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	addRun(t, st, "r2", "release")
	reg := registerCompare(t, st, "r1", "r2")
	h := New(st, Options{}).Handler()

	// The page offers no control, so the token has to come from a run report --
	// which is what a hand-written POST would do.
	run := register(t, st, "r1")
	token := tokenOf(t, get(t, h, "/r/"+run.ID, nil).Body.String())

	rec := post(h, "/r/"+reg.ID+"/grade", url.Values{"csrf": {token}, "task": {"t1"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "single-run report") {
		t.Errorf("the refusal does not say why:\n%s", rec.Body.String())
	}
}

// A judge with no key would fail every call, and the runner records a grader
// error as a failed task. Refusing before anything is written is the difference
// between "you need to set a variable" and a row that now says the agent failed.
func TestGradingWithAKeylessJudgeNamesTheVariable(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	rec := post(h, "/r/"+reg.ID+"/grade",
		url.Values{"csrf": {token}, "task": {"t1"}, "grader": {"llm_judge"}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ANTHROPIC_API_KEY") || !strings.Contains(body, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the refusal does not name the variables to set:\n%s", body)
	}
	if got := attempt(t, st, "r1", "t1"); got.Graded {
		t.Error("a judge that could not run still wrote a verdict")
	}
}

// A task that declares no graders is what collect produces, and pressing grade
// on it means "reach a verdict" -- not "tell me the dataset has none".
func TestGradingACollectedTaskFallsBackToTheJudge(t *testing.T) {
	st := newStore(t)
	// No graders at all, which the loader accepts only when the task carries a
	// baseline; that is the shape a collected task has.
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "dataset.yaml"),
		"name: collected\nversion: 1.0.0\ntasks_file: tasks.jsonl\n")
	writeFixture(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"t1","prompt":"answer the question",`+
			`"workspace":{"type":"files","files":{"a.txt":"x"}},`+
			`"output":{"type":"stdout"},"baseline":{"file":"baseline.json"}}`+"\n")
	writeFixture(t, filepath.Join(dir, "baseline.json"), `{}`)
	addGradableRun(t, st, "r1", dir)
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	// Without a key the fallback cannot run, and the refusal proves which
	// grader was selected: the judge, not an error about an empty grader list.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	rec := post(h, "/r/"+reg.ID+"/grade", url.Values{"csrf": {token}, "task": {"t1"}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "declares no graders") {
		t.Errorf("the page asked the reader to edit a dataset they may never have seen:\n%s", body)
	}
	if !strings.Contains(body, "llm_judge") || !strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Errorf("the fallback is not the judge, or the reason is not actionable:\n%s", body)
	}
}

// A server started with a judge declaration grades with that declaration.
//
// The declaration is more than a type name: the endpoint, the model and the
// name of the credential variable are what make a call possible at all. Grading
// with the built-in defaults while the replays use the declared judge is the
// state this test exists to prevent -- the page would send a key for one
// gateway to api.anthropic.com and report the result as "no credentials", which
// is the least useful description of a problem that has already been solved in
// the shell that started the server.
func TestTheJudgeAServerWasStartedWithIsTheJudgeItGradesWith(t *testing.T) {
	st := newStore(t)
	// A collected task: no graders, so the fallback is what grades it.
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "dataset.yaml"),
		"name: collected\nversion: 1.0.0\ntasks_file: tasks.jsonl\n")
	writeFixture(t, filepath.Join(dir, "tasks.jsonl"),
		`{"id":"t1","prompt":"answer the question",`+
			`"workspace":{"type":"files","files":{"a.txt":"x"}},`+
			`"output":{"type":"stdout"},"baseline":{"file":"baseline.json"}}`+"\n")
	writeFixture(t, filepath.Join(dir, "baseline.json"), `{}`)
	addGradableRun(t, st, "r1", dir)
	reg := register(t, st, "r1")

	// A stub that speaks the Messages protocol, so the test observes which
	// endpoint was called rather than trusting a config value.
	var (
		mu     sync.Mutex
		auth   string
		model  string
		called string
	)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		auth, model, called = r.Header.Get("Authorization"), body.Model, r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"stub-judge","content":[{"type":"text",`+
			`"text":"{\"score\":1,\"passed\":true,\"reason\":\"the stub judged it\"}"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	defer stub.Close()

	declared := dataset.GraderSpec{
		Type: "llm_judge",
		Name: "judge",
		Config: map[string]any{
			"base_url":       stub.URL,
			"auth_token_env": "AEVAL_JUDGE_TOKEN",
			"model":          "stub-judge",
			"input":          "both",
			"rubric":         "correct if it does what the prompt asked",
		},
	}
	srv := New(st, Options{Judge: &declared})
	// The built-in judge's variable is empty on purpose: if the server graded
	// with it, the request would be refused instead of reaching the stub.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("AEVAL_JUDGE_TOKEN", "token-from-the-declaration")
	h := srv.Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	rec := post(h, "/r/"+reg.ID+"/grade",
		url.Values{"csrf": {token}, "task": {"t1"}, "grader": {"auto"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: the declared judge cannot authenticate\n%s", rec.Code, rec.Body.String())
	}
	waitForGrades(t, srv)

	after := attempt(t, st, "r1", "t1")
	if !after.Graded || !after.Passed {
		t.Fatalf("cell = graded %v passed %v, want the stub's verdict", after.Graded, after.Passed)
	}
	if len(after.Grades) != 1 || after.Grades[0].Type != "llm_judge" {
		t.Errorf("graders = %+v, want the declared llm_judge", after.Grades)
	}

	mu.Lock()
	defer mu.Unlock()
	if called != "/v1/messages" {
		t.Errorf("the judge called %q, want the declared endpoint's /v1/messages", called)
	}
	if auth != "Bearer token-from-the-declaration" {
		t.Errorf("Authorization = %q, want the token the declaration names", auth)
	}
	if model != "stub-judge" {
		t.Errorf("model = %q, want the one the declaration names", model)
	}
}

// The JSON endpoint is the document the hash covers. Grading changes the row,
// so the endpoint must report the drift rather than hide it -- that is the
// claim that survived the withdrawal of read-only.
func TestGradingMakesTheJSONEndpointSayTheHashMoved(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	if matches := get(t, h, "/r/"+reg.ID+".json", nil).Header().Get("X-Aeval-Hash-Matches"); matches != "true" {
		t.Fatalf("before grading the document does not reproduce: %q", matches)
	}
	if rec := post(h, "/r/"+reg.ID+"/grade",
		url.Values{"csrf": {token}, "task": {"t1"}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("grade: status %d\n%s", rec.Code, rec.Body.String())
	}
	waitForGrades(t, srv)
	rec := get(t, h, "/r/"+reg.ID+".json", nil)
	if rec.Header().Get("X-Aeval-Hash-Matches") != "false" {
		t.Error("the JSON endpoint claims the graded document is the recorded one")
	}
	var payload struct {
		Counts struct {
			Graded int `json:"graded"`
			Passed int `json:"passed"`
		} `json:"counts"`
		Tasks []struct {
			Graded bool `json:"graded"`
			Passed bool `json:"passed"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, rec.Body.String())
	}
	if payload.Counts.Graded != 1 || payload.Counts.Passed != 1 {
		t.Errorf("counts = %+v, want 1 graded and 1 passed", payload.Counts)
	}
}

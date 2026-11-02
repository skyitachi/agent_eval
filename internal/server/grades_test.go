package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/store"
)

// gated is a grader that stops where a real one stops: in the middle of a call
// that takes time and can be interrupted.
//
// A test cannot use exact_match to prove that a request returns before the work
// does -- it finishes in microseconds, so the race is invisible -- and a test
// that sleeps instead would be asserting on timing. This grader blocks until the
// test lets it go, which makes "the answer came back while the grading had not"
// a fact rather than a hope.
type gated struct {
	arrived chan struct{}
	release chan struct{}
}

// gateDispatcher is what the registry holds. The registry is global and takes
// one value per type, so registering a per-test gate would leave every test
// after the first waiting on a channel its own test never closes.
type gateDispatcher struct{}

func (gateDispatcher) Type() string { return "gated" }

func (gateDispatcher) Grade(ctx context.Context, gc *grader.Context, spec dataset.GraderSpec) (*grader.Result, error) {
	g := currentGate()
	if g == nil {
		return nil, context.Canceled
	}
	return g.Grade(ctx, gc, spec)
}

var (
	gatedOnce   sync.Once
	gatedLock   sync.Mutex
	gatedLatest *gated
)

func (g *gated) Grade(ctx context.Context, gc *grader.Context, spec dataset.GraderSpec) (*grader.Result, error) {
	select {
	case g.arrived <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return &grader.Result{Grader: "gated", Type: "gated", Passed: true, Score: 1, Weight: 1}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// newGate registers the blocking grader once per test binary and returns a
// fresh gate for the test that needs one.
func newGate() *gated {
	gatedOnce.Do(func() { grader.Register(gateDispatcher{}) })
	g := &gated{arrived: make(chan struct{}, 1), release: make(chan struct{})}
	gatedLock.Lock()
	gatedLatest = g
	gatedLock.Unlock()
	return g
}

// currentGate is the gate the running grader is using. The registry keeps one
// value for the type, so the test that registered it owns it.
func currentGate() *gated {
	gatedLock.Lock()
	defer gatedLock.Unlock()
	return gatedLatest
}

// The change this file is about: the press is answered while the judgement is
// still running, the page says so, and the row it wrote records the request.
func TestAGradeIsAnsweredBeforeItFinishes(t *testing.T) {
	gate := newGate()
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"gated"}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())
	path := "/r/" + reg.ID + "/grade"
	form := url.Values{"csrf": {token}, "task": {"t1"}, "sample": {"0"}, "grader": {"auto"}}

	started := time.Now()
	rec := post(h, path, form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 while the grader is still blocked\n%s", rec.Code, rec.Body.String())
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("the press took %v; it is supposed to return without waiting for the grader", waited)
	}
	// Proof that the answer really did arrive first: the grader has not
	// returned, so nothing can have written a verdict.
	select {
	case <-gate.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the grader was never asked to run; the request was accepted and forgotten")
	}
	if row := attempt(t, st, "r1", "t1"); row.Graded {
		t.Error("the cell already holds a verdict while the grader is still blocked")
	}

	// The page the browser lands on: it says grading, it marks the row, and it
	// reloads itself until the row has a verdict.
	back := get(t, h, rec.Header().Get("Location"), nil).Body.String()
	for _, want := range []string{
		"Grading this cell.",
		`<meta http-equiv="refresh" content="3">`,
		`data-grade-task="t1"`,
	} {
		if !strings.Contains(back, want) {
			t.Errorf("the page while grading is missing %q:\n%s", want, back)
		}
	}

	// A second press, while the first is still running, must not buy a second
	// judgement of the same cell.
	again := post(h, path, form, nil)
	if again.Code != http.StatusSeeOther {
		t.Fatalf("a repeated press = %d, want the redirect to the running request\n%s", again.Code, again.Body.String())
	}
	jobs, err := st.ListGradeJobs(context.Background(), "r1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d requests recorded for one cell, want 1: a repeated press paid twice", len(jobs))
	}
	if !jobs[0].Active() {
		t.Errorf("the recorded request = %+v, want it still live", jobs[0])
	}

	close(gate.release)
	waitForGrades(t, srv)

	if row := attempt(t, st, "r1", "t1"); !row.Graded || !row.Passed {
		t.Errorf("cell = graded %v passed %v, want the gated grader's pass", row.Graded, row.Passed)
	}
	done, err := st.GetGradeJob(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != store.GradeDone || done.LatencyMs <= 0 {
		t.Errorf("finished request = %+v, want done and timed", done)
	}
	if done.InputTokens != 0 || done.OutputTokens != 0 {
		t.Errorf("a grader that calls no model recorded %d/%d tokens", done.InputTokens, done.OutputTokens)
	}

	// And the page settles on the verdict, no longer refreshing.
	settled := get(t, h, rec.Header().Get("Location"), nil).Body.String()
	if strings.Contains(settled, "Grading this cell.") || strings.Contains(settled, `http-equiv="refresh"`) {
		t.Error("the page still says grading after the request finished")
	}
	if !strings.Contains(settled, "The verdict on record is PASS.") {
		t.Errorf("the finished page does not report the verdict:\n%s", settled)
	}
	if strings.Contains(settled, "output tokens on") {
		t.Error("the page claims a token cost for a grader that calls no model")
	}
}

// The tokens the judgement spent are recorded with the request, not folded into
// the agent's spend. This is the number a reader comparing a cheap grader with a
// model had no way to see before.
func TestTheJudgesSpendIsRecordedWithTheRequest(t *testing.T) {
	var mu sync.Mutex
	var calls int
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"test-judge",
		  "content":[{"type":"text","text":"{\"score\": 0.8, \"passed\": true, \"reason\": \"fine\"}"}],
		  "stop_reason":"end_turn","usage":{"input_tokens":1234,"output_tokens":567}}`)
	}))
	defer judge.Close()
	t.Setenv("AEVAL_TEST_JUDGE_TOKEN", "not-a-real-key")

	st := newStore(t)
	// The judge is declared by the dataset, which is the path a report page's
	// "as declared by the dataset" choice takes.
	ds := gradableDataset(t, `[{"type":"llm_judge","name":"judge","config":{`+
		`"base_url":"`+judge.URL+`","auth_token_env":"AEVAL_TEST_JUDGE_TOKEN",`+
		`"model":"test-judge","input":"final_answer","max_tokens":1024,`+
		`"rubric":"The answer is correct."}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	rec := post(h, "/r/"+reg.ID+"/grade", url.Values{
		"csrf": {token}, "task": {"t1"}, "sample": {"0"}, "grader": {"auto"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	waitForGrades(t, srv)

	jobs, err := st.ListGradeJobs(context.Background(), "r1", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("requests = %+v, %v", jobs, err)
	}
	job := jobs[0]
	if job.Status != store.GradeDone {
		t.Fatalf("request = %+v, want it done", job)
	}
	if job.InputTokens != 1234 || job.OutputTokens != 567 {
		t.Errorf("recorded spend = %d in / %d out, want the judge's 1234 / 567", job.InputTokens, job.OutputTokens)
	}
	if job.Model != "test-judge" {
		t.Errorf("recorded model = %q, want test-judge", job.Model)
	}
	if job.LatencyMs <= 0 {
		t.Error("the request records no duration")
	}
	// The spend is the request's, not the cell's: the attempt's own token
	// columns are the agent's measurement and must not move.
	if row := attempt(t, st, "r1", "t1"); row.InputTokens != 0 || row.OutputTokens != 0 {
		t.Errorf("the judge's tokens were written onto the agent's row: %d/%d", row.InputTokens, row.OutputTokens)
	}

	page := get(t, h, rec.Header().Get("Location"), nil).Body.String()
	// The fixture records no cutoff, so the default applies: 0.80 clears 0.60.
	// The verdict is beside the point here, the spend is not.
	for _, want := range []string{
		"The verdict on record is PASS.",
		"0.80",
		"1234 input and 567 output tokens",
		"test-judge",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not report %q:\n%s", want, page)
		}
	}
}

// A cell can fail while its judge's own card reads pass, and the page has to
// say why in one place.
//
// The row is the case that produced this test: an attempt scored 0.80, the
// judge passed it against its own 0.67, the cell failed against the 1.00 the
// run recorded, and the banner mentioned only the cell's number while the green
// tick sat in the drawer. Two verdicts about one row and nothing saying they
// answer different questions reads as a broken page.
func TestTheBannerExplainsTheCutoffAGraderDrewForItself(t *testing.T) {
	row := store.Attempt{
		TaskID: "4eb4178a-p0", Status: store.CellOK,
		Graded: true, Passed: false, Score: 0.8, Threshold: 1.0,
		Grades: []grader.Result{{
			Grader: "judge", Type: "llm_judge", Passed: true, Score: 0.8, Weight: 1,
			Details: map[string]any{"threshold": 0.67},
		}},
	}
	n := verdictNotice(row, nil)
	if n.Severity != "warn" || !strings.Contains(n.Head, "FAIL") {
		t.Fatalf("notice = %+v, want the failure this row records", n)
	}
	for _, want := range []string{
		"scores 0.80 against a threshold of 1.00",
		"judge's own cutoff is 0.67 and it reads pass on this work",
		"the verdict comes from that one",
	} {
		if !strings.Contains(n.Note, want) {
			t.Errorf("the banner does not say %q:\n%s", want, n.Note)
		}
	}

	// The row's cutoff wins over today's default: this is a recorded verdict,
	// not a re-run, and the banner must not silently restate it as 0.60.
	if strings.Contains(n.Note, "0.60") {
		t.Errorf("the banner applied a default to a recorded verdict:\n%s", n.Note)
	}

	// A grader with no cutoff of its own has nothing to reconcile, and the
	// banner is already long: exact_match must not grow a sentence about a
	// number it never used.
	plain := verdictNotice(store.Attempt{
		TaskID: "t1", Status: store.CellOK, Graded: true, Passed: true, Score: 1, Threshold: 1,
		Grades: []grader.Result{{Grader: "exact", Type: "exact_match", Passed: true, Score: 1, Weight: 1}},
	}, nil)
	if strings.Contains(plain.Note, "cutoff") {
		t.Errorf("a grader with no cutoff was explained as having one:\n%s", plain.Note)
	}
}

// A request that failed has to be readable on the page it was pressed on. It
// used to be a 409; now the press is accepted, so the failure must land
// somewhere -- and the row has to be visibly unchanged.
func TestAFailedGradeIsReportedOnThePageAndLeavesTheRowAlone(t *testing.T) {
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"exact_match","config":{"expected":"done"}}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()

	// The state a failed request leaves behind, written directly: this test is
	// about what the page makes of it, not about how it got there.
	job, err := st.CreateGradeJob(context.Background(), store.GradeRequest{
		ReportID: reg.ID, RunID: "r1", TaskID: "t1", Sample: 0, Grader: "llm_judge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartGradeJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FailGradeJob(context.Background(), job.ID, "the judge returned no JSON"); err != nil {
		t.Fatal(err)
	}

	page := get(t, h, "/r/"+reg.ID+"?graded=t1&sample=0#tasks", nil).Body.String()
	for _, want := range []string{"The grade did not finish.", "the judge returned no JSON", "The row below is unchanged"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not report %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("a page about a finished request reloads itself")
	}
	if row := attempt(t, st, "r1", "t1"); row.Graded {
		t.Error("a failed request wrote a verdict")
	}
}

// Stopping the server must not leave a row saying "grading": the work is a
// goroutine of this process, and a reader whose server is gone would wait for a
// verdict nothing is producing.
func TestStoppingTheServerClosesTheRequestsItAccepted(t *testing.T) {
	gate := newGate()
	st := newStore(t)
	ds := gradableDataset(t, `[{"type":"gated"}]`)
	addGradableRun(t, st, "r1", ds)
	reg := register(t, st, "r1")
	srv := New(st, Options{})
	h := srv.Handler()
	token := tokenOf(t, get(t, h, "/r/"+reg.ID, nil).Body.String())

	if rec := post(h, "/r/"+reg.ID+"/grade", url.Values{
		"csrf": {token}, "task": {"t1"}, "sample": {"0"}, "grader": {"auto"},
	}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	select {
	case <-gate.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the grader never ran")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("stopping the server: %v", err)
	}

	jobs, err := st.ListGradeJobs(context.Background(), "r1", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("requests = %+v, %v", jobs, err)
	}
	if jobs[0].Status != store.GradeFailed {
		t.Errorf("after shutdown the request = %+v, want failed so the page stops waiting", jobs[0])
	}
	if !strings.Contains(jobs[0].Error, "server stopped") {
		t.Errorf("the recorded reason = %q, want the shutdown named", jobs[0].Error)
	}
	if row := attempt(t, st, "r1", "t1"); row.Graded {
		t.Error("a cancelled grade wrote a verdict")
	}
}

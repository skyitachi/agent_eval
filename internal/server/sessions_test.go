package server

// Tests for the session and replay pages.
//
// These pages read this machine's own histories and queue agent runs, so the
// tests lean on the refusals as much as on the happy paths: every check in
// the chain -- loopback Host, Origin, CSRF token, preset validity -- exists to
// stop one specific request from reaching a replay, and a check that no test
// reddens is a check that can be deleted without anyone noticing.

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// sessionRoot builds a Claude history root holding the checked-in basic
// fixture, the shape `serve` reads on a real machine.
func sessionRoot(t *testing.T) string {
	t.Helper()
	// The layout is DefaultRoot's: root/<encoded cwd>/<session id>.jsonl,
	// where the encoded cwd is the recording's working directory with
	// separators dashed -- two levels, no "projects" of the projects.
	root := filepath.Join(t.TempDir(), "-work-proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := filepath.Abs(filepath.Join("..", "..", "tests", "testdata", "session", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "aaaabbbb-1111-2222-3333-444455556666.jsonl")
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(root)
}

// The one session the fixture root holds.
const claudeSession = "aaaabbbb-1111-2222-3333-444455556666"

// sessionServer builds a server over a fresh store with the fixture histories
// and the built-in presets, and starts the replay worker on a context the test
// controls so a queued replay can be waited for.
func sessionServer(t *testing.T, mutate func(*Options)) (*Server, *replayWatch) {
	t.Helper()
	st := newStore(t)
	presets := DefaultReplayPresets()
	// The built-in presets drive `claude`; a test cannot (it would spend a
	// real session's tokens on a machine CI has no credentials for), so the
	// same names are re-pointed at the mock agent, the way the e2e suite runs
	// every other command: the shape is the server's, the binary is a no-op.
	for i := range presets.Harnesses {
		presets.Harnesses[i].Agent.Kind = "mock"
		presets.Harnesses[i].Agent.Bin = "/bin/true"
	}
	opts := Options{
		Replays:     presets,
		SessionRoot: sessionRoot(t),
		// No Cursor database: the server treats a missing one as empty, but a
		// path inside the temp dir keeps the test from touching the real one.
		CursorDB:      filepath.Join(t.TempDir(), "state.vscdb"),
		ReplayEnabled: true,
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv := New(st, opts)
	return srv, watchReplays(t, srv)
}

// replayWatch waits for the replay worker to reach a terminal status.
//
// The queue is asynchronous by design, so the tests cannot assert on a status
// directly after the POST; they wait for whichever status the replay lands on
// and then assert it is the one they meant.
type replayWatch struct {
	srv *Server
}

func watchReplays(t *testing.T, srv *Server) *replayWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	_ = srv.HandlerWithContext(ctx)
	return &replayWatch{srv: srv}
}

// await polls one replay to a terminal status, failing after a bound.
func (w *replayWatch) await(t *testing.T, id string) store.Replay {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rep, err := w.srv.st.GetReplay(context.Background(), id)
		if err != nil {
			t.Fatalf("read replay %s: %v", id, err)
		}
		switch rep.Status {
		case store.ReplayDone, store.ReplayFailed, store.ReplayInterrupted:
			return rep
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replay %s never reached a terminal status", id)
	return store.Replay{}
}

// postReplay submits the replay form the way a browser would.
func postReplay(t *testing.T, h http.Handler, source, id string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sessions/%s/%s/replay", source, id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A browser always addresses a host, so the tests do too: the guard sees
	// the request a real one would have made.
	req.Host = "localhost:8080"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getLocal requests a session page as this machine would.
//
// The Host is a request field in Go's http package, not a header: a request
// that sets it through Header.Set carries no Host at all, and the local-only
// guard would refuse it -- which is the one failure these tests must not
// manufacture themselves.
func getLocal(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postLocal posts the replay form as this machine would; see getLocal.
func postLocal(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getForeign requests a session page the way a rebound DNS name would: a Host
// header that is not loopback.
func getForeign(t *testing.T, h http.Handler, path, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// formFor extracts the replay form's fields from a detail page, so a test
// posts what the page itself offers: the token of this process and the
// presets it names.
func formFor(t *testing.T, doc string) url.Values {
	t.Helper()
	val := func(name string) string {
		t.Helper()
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(` name="` + name + `" value="([^"]*)"`), // hidden csrf
			regexp.MustCompile(`name="` + name + `" list="`),           // model's datalist input
		} {
			if m := re.FindStringSubmatch(doc); m != nil {
				if len(m) > 1 {
					return m[1]
				}
				return ""
			}
		}
		t.Fatalf("the form has no %s field:\n%s", name, doc)
		return ""
	}
	if !strings.Contains(doc, `name="csrf"`) {
		t.Fatalf("the page offers no replay form:\n%s", doc)
	}
	return url.Values{
		"csrf":    {val("csrf")},
		"harness": {"clean-room"}, // the form's first option; a browser sends it selected
		"skill":   {"none"},
		"model":   {"test-model"},
		"split":   {"session"},
	}
}

// ---------------------------------------------------------------------------
// The listing and the detail page
// ---------------------------------------------------------------------------

// TestTheSessionListingMergesBothHistories checks the listing's one claim over
// a plain ls: the two histories on a machine are one list, so a reader does
// not have to know which tool recorded what.
func TestTheSessionListingMergesBothHistories(t *testing.T) {
	srv, _ := sessionServer(t, nil)

	rec := getLocal(t, srv.Handler(), "/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	doc := rec.Body.String()
	if !strings.Contains(doc, claudeSession[:12]) {
		t.Errorf("the listing does not show the fixture session id:\n%s", doc)
	}
	// The secondary line is the directory the transcript was found under,
	// whose name is the encoding of the recording's working directory.
	if !strings.Contains(doc, "-work-proj") {
		t.Errorf("the listing does not show the project directory:\n%s", doc)
	}
	// The name is the first human prompt, the convention the whole tool uses,
	// so a session is recognisable in the list before it is opened.
	if !strings.Contains(doc, "修复 calc.py 里的减法错误") {
		t.Errorf("the listing does not show the session's first prompt as its name:\n%s", doc)
	}
	if !strings.Contains(doc, `href="/sessions/claude/`+claudeSession+`"`) {
		t.Errorf("the rows do not link to the detail page:\n%s", doc)
	}
}

// TestTheSessionDetailPageShowsTheRecording covers what a reader opens a
// session for: the environment it ran in, and the turns it recorded.
func TestTheSessionDetailPageShowsTheRecording(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()

	detail := getLocal(t, h, "/sessions/claude/"+claudeSession)
	if detail.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", detail.Code, detail.Body.String())
	}
	doc := detail.Body.String()
	for _, want := range []string{
		"/work/proj",        // cwd
		"main",              // the git branch the recording started on
		"test-model-1",      // the model that answered
		"修复 calc.py 里的减法错误", // a recorded prompt
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the detail page does not show %q:\n%s", want, doc)
		}
	}
	// The replay form is offered with the built-in presets.
	for _, want := range []string{"clean-room", "host", "none"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the form does not offer the built-in preset %q:\n%s", want, doc)
		}
	}
}

// TestAnUnknownSessionIsANamedRefusal mirrors the reports' 404s: a reader who
// follows a dead link should land somewhere that says which session is missing.
func TestAnUnknownSessionIsANamedRefusal(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()

	for _, tc := range []struct{ path, id string }{
		{"/sessions/claude/deadbeef", "deadbeef"},
		{"/sessions/cursor/deadbeef", "deadbeef"},
		{"/sessions/vscode/x", "x"},
	} {
		rec := getLocal(t, h, tc.path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404\n%s", tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.id) {
			t.Errorf("%s: the page does not name the missing session:\n%s", tc.path, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// What the pages refuse
// ---------------------------------------------------------------------------

// TestTheSessionPagesAreLocalOnly is the DNS-rebinding guard.
//
// A page elsewhere can point a hostname at 127.0.0.1 and make a
// same-origin-looking request; only the Host header tells those apart, so
// everything but the literal loopback names is refused.
func TestTheSessionPagesAreLocalOnly(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()

	for _, host := range []string{"localhost", "localhost:8080", "127.0.0.1:8080", "[::1]:8080", "::1"} {
		if host == "::1" {
			continue // a Host header without brackets is not a valid authority
		}
		rec := getForeign(t, h, "/sessions", host)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200\n%s", host, rec.Code, rec.Body.String())
		}
	}
	for _, host := range []string{"evil.example:8080", "192.168.1.5:8080", ""} {
		rec := getForeign(t, h, "/sessions", host)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status = %d, want 403\n%s", host, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "local-only") {
			t.Errorf("Host %q: the refusal does not say the page is local-only:\n%s", host, rec.Body.String())
		}
	}
}

// TestTheReplayPostRefusesOtherOrigins is the CSRF pair: the token binds the
// form to this process, the Origin header binds the request to this host.
//
// Either alone is beatable -- a token copied into another page, or a form on
// another page -- and the two together are what a cross-site post cannot
// satisfy. Each refusal has to name itself, because a person who just used
// the form has to be able to tell a stale tab from a broken server.
func TestTheReplayPostRefusesOtherOrigins(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()

	stub := func(t *testing.T) url.Values {
		return formFor(t, getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String())
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no origin headers at all is fine for a non-browser", nil, ""},
		{"an origin from elsewhere", map[string]string{"Origin": "https://evil.example"}, "came from"},
		{"a referer from elsewhere", map[string]string{"Referer": "https://evil.example/page"}, "came from"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postReplay(t, h, "claude", claudeSession, stub(t), tc.headers)
			if tc.want == "" {
				return // covered below; only the token path is exercised here
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("the refusal does not explain itself:\n%s", rec.Body.String())
			}
		})
	}
}

// TestTheReplayPostRequiresTheProcessToken covers the token's half.
func TestTheReplayPostRequiresTheProcessToken(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()

	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()
	for _, tc := range []struct {
		name string
		csrf string
	}{
		{"a wrong token", "0"},
		{"no token", ""},
	} {
		form := formFor(t, doc)
		form.Set("csrf", tc.csrf)
		rec := postReplay(t, h, "claude", claudeSession, form, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403\n%s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "token did not match") {
			t.Errorf("%s: the refusal does not explain itself:\n%s", tc.name, rec.Body.String())
		}
	}
}

// TestTheReplayPostValidatesItsFields pins the form's own vocabulary: the
// presets are named, not free text, and the model is one shell-safe word.
//
// Every case is a 400 rather than a 500: the caller just used the form, and a
// refusal they can act on is the difference between a wrong field and a
// broken tool.
func TestTheReplayPostValidatesItsFields(t *testing.T) {
	srv, _ := sessionServer(t, nil)
	h := srv.Handler()
	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()

	cases := []struct {
		name   string
		mutate func(url.Values)
		want   string
	}{
		{"an unknown harness", func(v url.Values) { v.Set("harness", "neo") }, "harness"},
		{"an unknown skill", func(v url.Values) { v.Set("skill", "neo") }, "skill"},
		{"a model with a space", func(v url.Values) { v.Set("model", "two words") }, "model"},
		{"a model with shell metacharacters", func(v url.Values) { v.Set("model", "x;rm") }, "model"},
		{"an empty model", func(v url.Values) { v.Set("model", "") }, "model"},
		{"an unknown split", func(v url.Values) { v.Set("split", "half") }, "split"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := formFor(t, doc)
			tc.mutate(form)
			rec := postReplay(t, h, "claude", claudeSession, form, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("the refusal does not name the field:\n%s", rec.Body.String())
			}
		})
	}
}

// TestAReplayIsDisabledRefused covers the off switch: the pages stay, the
// form goes.
func TestAReplayIsDisabledRefused(t *testing.T) {
	srv, _ := sessionServer(t, func(o *Options) { o.ReplayEnabled = false })
	h := srv.Handler()

	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()
	if strings.Contains(doc, `name="csrf"`) {
		t.Error("a disabled server still offers the replay form")
	}
	form := url.Values{"csrf": {srv.csrf}, "model": {"m"}}
	rec := postReplay(t, h, "claude", claudeSession, form, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The replay flow
// ---------------------------------------------------------------------------

// TestAReplayRunsAndIsRecorded is the happy path: a POST becomes rows and a
// report, and the report opens through the ordinary route with a matching
// hash, which is the registry's whole contract.
func TestAReplayRunsAndIsRecorded(t *testing.T) {
	srv, w := sessionServer(t, nil)
	h := srv.Handler()

	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()
	form := formFor(t, doc)
	rec := postReplay(t, h, "claude", claudeSession, form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/replays/") {
		t.Fatalf("Location = %q, want the new replay's status page", loc)
	}
	id := strings.TrimPrefix(loc, "/replays/")

	rep := w.await(t, id)
	if rep.Status != store.ReplayDone {
		t.Fatalf("replay status = %q (%s)\n", rep.Status, rep.Error)
	}
	if rep.RunID == "" {
		t.Fatal("the finished replay recorded no run row")
	}
	if rep.ReportID == "" {
		t.Fatal("the finished replay registered no report")
	}

	// The status page links the report and names the run.
	page := getLocal(t, h, "/replays/"+id).Body.String()
	if !strings.Contains(page, `href="/r/`+rep.ReportID+`"`) {
		t.Errorf("the status page does not link the report:\n%s", page)
	}
	// And the listing links it too.
	list := getLocal(t, h, "/replays").Body.String()
	if !strings.Contains(list, `href="/r/`+rep.ReportID+`"`) {
		t.Errorf("the replay listing does not link the report:\n%s", list)
	}

	// The report opens through the ordinary route and its hash matches, which
	// is the same contract every registered report is held to.
	jsonDoc := getLocal(t, h, "/r/"+rep.ReportID)
	if jsonDoc.Code != http.StatusOK {
		t.Fatalf("the replay's report does not open: %d\n%s", jsonDoc.Code, jsonDoc.Body.String())
	}
	if jsonDoc.Body.Len() == 0 {
		t.Fatal("the replay's report is empty")
	}
}

// TestAFailingReplayRecordsItsReason covers the other terminal status: the
// row says failed and why, and no report is registered for a run that did not
// produce one.
func TestAFailingReplayRecordsItsReason(t *testing.T) {
	// A preset whose harness names an agent kind no build knows: the runner
	// refuses it before any run row exists, which is the ordinary way a replay
	// fails -- its configuration cannot be honoured, rather than its agent
	// trying and losing. (A mock whose bin fails is *not* this: the mock
	// records a failed attempt and the run, and the replay, still finish.)
	srv, w := sessionServer(t, func(o *Options) {
		o.Replays.Harnesses[0].Agent.Kind = "no-such-agent"
	})
	h := srv.Handler()

	doc := getLocal(t, h, "/sessions/claude/"+claudeSession).Body.String()
	form := formFor(t, doc)
	form.Set("harness", "clean-room")
	rec := postReplay(t, h, "claude", claudeSession, form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	id := strings.TrimPrefix(rec.Header().Get("Location"), "/replays/")

	rep := w.await(t, id)
	if rep.Status != store.ReplayFailed {
		t.Fatalf("replay status = %q, want failed\n", rep.Status)
	}
	if rep.Error == "" {
		t.Error("a failed replay recorded no reason")
	}
	if rep.ReportID != "" {
		t.Errorf("a failed replay registered a report %q", rep.ReportID)
	}

	// The status page carries the reason verbatim, because it is the one
	// sentence that explains every other field on the page. The reason is
	// HTML-escaped on the way into the page (a quote becomes &#34;), so the
	// assertion looks for the escaped form: what a reader's browser would
	// show them is the page's text, not the raw bytes.
	page := getLocal(t, h, "/replays/"+id).Body.String()
	esc := html.EscapeString(rep.Error)
	if !strings.Contains(page, esc) {
		t.Errorf("the status page does not show the failure's reason:\n%s", page)
	}
}

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/replay"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

const sessionFixture = "../../tests/testdata/session/basic.jsonl"
const fixtureSession = "aaaabbbb-1111-2222-3333-444455556666"

// sessionServer serves the fixture transcript, with a harness that drives the
// one-turn stand-in so a replay finishes without a model.
func sessionServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	return sessionServerAt(t, t.TempDir())
}

// sessionServerAt is sessionServer over a caller's transcript root, so a test
// can put more transcripts beside the fixture before the server starts.
func sessionServerAt(t *testing.T, root string) (http.Handler, *store.Store) {
	t.Helper()
	st := newStore(t)
	body, err := os.ReadFile(sessionFixture)
	if err != nil {
		t.Fatal(err)
	}
	// A transcript whose text could carry markup, to check it is escaped.
	body = append(body, []byte(`{"type":"ai-title","aiTitle":"<script>alert(1)</script> fix"}`+"\n")...)
	dir := filepath.Join(root, "-work-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureSession+".jsonl"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	bin, _ := filepath.Abs("../../tests/testdata/agents/one-turn.sh")
	presetsPath := filepath.Join(t.TempDir(), "presets.yaml")
	if err := os.WriteFile(presetsPath, []byte("harnesses:\n  - name: scripted\n    agent: {kind: mock, bin: "+bin+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	presets, err := replay.LoadPresets(presetsPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &session.Catalog{ClaudeRoot: root}
	mgr, err := replay.NewManager(replay.Config{Store: st, Catalog: catalog, Presets: presets,
		WorkRoot: t.TempDir(), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	srv := New(st, Options{})
	if err := srv.EnableSessions(SessionOptions{Catalog: catalog, Replays: mgr}); err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), st
}

func post(h http.Handler, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// getLocal is a GET the way a browser on this machine sends one. httptest
// addresses every request to example.com, which the session pages refuse.
func getLocal(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTheSessionListAndDetailShowTheTranscript(t *testing.T) {
	h, _ := sessionServer(t)
	list := getLocal(t, h, "/sessions")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /sessions = %d\n%s", list.Code, list.Body)
	}
	page := list.Body.String()
	if !strings.Contains(page, "/sessions/claude/"+fixtureSession) {
		t.Errorf("the list does not link the fixture session:\n%s", page)
	}
	// The title comes from a transcript, and a transcript is not trusted markup.
	if strings.Contains(page, "<script>alert(1)") || !strings.Contains(page, "&lt;script&gt;") {
		t.Error("the session title is not escaped")
	}

	detail := getLocal(t, h, "/sessions/claude/"+fixtureSession)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET detail = %d\n%s", detail.Code, detail.Body)
	}
	body := detail.Body.String()
	for _, want := range []string{"修复 calc.py 里的减法错误", "已修复，减号写反了。", "Read", "/work/proj",
		`name="harness"`, `value="scripted"`, `name="model"`, "Replay"} {
		if !strings.Contains(body, want) {
			t.Errorf("the session page should show %q", want)
		}
	}

	if r := getLocal(t, h, "/sessions/claude/nope"); r.Code != http.StatusNotFound {
		t.Errorf("an unknown session = %d, want 404", r.Code)
	}
	if r := getLocal(t, h, "/sessions?q=nothing-matches-this"); !strings.Contains(r.Body.String(), "No sessions") {
		t.Error("a filter with no match should say so")
	}
}

// TestTheSessionListDoesNotOfferTheOutputOfAReplay: a replay runs the agent in
// a workspace of its own, and Claude Code records a transcript for that
// workspace. It carries the replayed prompt, so it looks like a session with a
// human request; it is the result of a replay, not another recording to offer.
func TestTheSessionListDoesNotOfferTheOutputOfAReplay(t *testing.T) {
	root := t.TempDir()
	body, err := os.ReadFile(sessionFixture)
	if err != nil {
		t.Fatal(err)
	}
	const producedID = "ccccdddd-1111-2222-3333-444455556666"
	replayCwd := "/tmp/aeval/replays/0123456789abcdef/work/run1/" + fixtureSession + "/s0"
	produced := strings.ReplaceAll(string(body), `"cwd":"/work/proj"`, `"cwd":"`+replayCwd+`"`)
	dir := filepath.Join(root, "-tmp-aeval-replays-0123456789abcdef-work-run1-"+fixtureSession+"-s0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, producedID+".jsonl"), []byte(produced), 0o644); err != nil {
		t.Fatal(err)
	}

	h, _ := sessionServerAt(t, root)
	list := getLocal(t, h, "/sessions")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /sessions = %d\n%s", list.Code, list.Body)
	}
	page := list.Body.String()
	if strings.Contains(page, producedID) {
		t.Errorf("the list offers a transcript a replay produced:\n%s", page)
	}
	if !strings.Contains(page, "/sessions/claude/"+fixtureSession) {
		t.Errorf("the recording beside it is missing:\n%s", page)
	}
	if rec := getLocal(t, h, "/sessions/claude/"+producedID); rec.Code != http.StatusNotFound {
		t.Errorf("a replay's own transcript opened as a session page = %d, want 404", rec.Code)
	}
	// A replay of a real session still starts: only the replay's output is
	// filtered, not the recording that was replayed.
	if rec := getLocal(t, h, "/sessions/claude/"+fixtureSession); rec.Code != http.StatusOK {
		t.Errorf("the recorded session's page = %d, want 200", rec.Code)
	}
}

func TestTheReplayButtonRunsAReplayAndLinksItsReport(t *testing.T) {
	h, st := sessionServer(t)
	detail := getLocal(t, h, "/sessions/claude/"+fixtureSession).Body.String()
	m := csrfField.FindStringSubmatch(detail)
	if m == nil {
		t.Fatal("the replay form carries no token")
	}
	rec := post(h, "/sessions/claude/"+fixtureSession+"/replay",
		url.Values{"csrf": {m[1]}, "harness": {"scripted"}, "skill": {"none"}, "model": {"sonnet"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST replay = %d\n%s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/replays/") {
		t.Fatalf("redirected to %q, want the replay page", loc)
	}
	id := strings.TrimPrefix(loc, "/replays/")

	var rp store.Replay
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rp, _ = st.GetReplay(context.Background(), id)
		if rp.Finished() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rp.Status != store.ReplayCompleted {
		t.Fatalf("replay = %s: %s\n%s", rp.Status, rp.Error, rp.Log)
	}
	page := getLocal(t, h, loc).Body.String()
	if !strings.Contains(page, `href="/r/`+rp.ReportID+`"`) {
		t.Errorf("the finished replay page does not link its report %s", rp.ReportID)
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("a finished replay page must stop refreshing")
	}
	// And the link works: the report is served like any recorded one.
	report := getLocal(t, h, "/r/"+rp.ReportID)
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), "Recorded report.") {
		t.Errorf("GET /r/%s = %d", rp.ReportID, report.Code)
	}
	// The session page lists it.
	if !strings.Contains(getLocal(t, h, "/sessions/claude/"+fixtureSession).Body.String(), "/replays/"+id) {
		t.Error("the session page does not list its replay")
	}
	if !strings.Contains(getLocal(t, h, "/replays").Body.String(), "/replays/"+id) {
		t.Error("the replay list does not list it")
	}
}

func TestAReplayCanBeCancelledFromTheList(t *testing.T) {
	h, st := sessionServer(t)
	ctx := context.Background()
	if err := st.CreateReplay(ctx, store.Replay{
		ID: "waiting", Source: "claude", SessionID: fixtureSession,
		SessionTitle: "fixture", Status: store.ReplayQueued,
	}); err != nil {
		t.Fatal(err)
	}
	list := getLocal(t, h, "/replays")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /replays = %d\n%s", list.Code, list.Body)
	}
	page := list.Body.String()
	if !strings.Contains(page, `action="/replays/waiting/cancel"`) {
		t.Fatalf("an unfinished replay in the list offers no way to cancel it:\n%s", page)
	}
	m := csrfField.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the replay list carries no CSRF token")
	}
	rec := post(h, "/replays/waiting/cancel", url.Values{"csrf": {m[1]}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST cancel = %d\n%s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/replays/waiting" {
		t.Errorf("redirected to %q, want /replays/waiting", loc)
	}
	got, err := st.GetReplay(ctx, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ReplayFailed || got.Error == "" {
		t.Errorf("the cancelled replay = %+v, want failed with a reason", got)
	}
	if strings.Contains(getLocal(t, h, "/replays").Body.String(), `action="/replays/waiting/cancel"`) {
		t.Error("a finished replay must not offer to be cancelled again")
	}
}

func TestAReplayFormFromElsewhereIsRefused(t *testing.T) {
	h, st := sessionServer(t)
	token := csrfField.FindStringSubmatch(getLocal(t, h, "/sessions/claude/"+fixtureSession).Body.String())[1]
	path := "/sessions/claude/" + fixtureSession + "/replay"
	cases := map[string]struct {
		form    url.Values
		headers map[string]string
	}{
		"no token":    {url.Values{"harness": {"scripted"}}, nil},
		"wrong token": {url.Values{"csrf": {"0000"}, "harness": {"scripted"}}, nil},
		// A page on another site that somehow had the token still names
		// itself in Origin.
		"foreign origin": {url.Values{"csrf": {token}}, map[string]string{"Origin": "https://evil.example"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(h, path, tc.form, tc.headers); rec.Code != http.StatusForbidden {
				t.Errorf("= %d, want 403", rec.Code)
			}
		})
	}
	if rows, _ := st.ListReplays(context.Background(), store.ReplayFilter{}); len(rows) != 0 {
		t.Errorf("a refused form started %d replay(s)", len(rows))
	}
	// A bad choice is answered on the form, not as a replay that fails later.
	rec := post(h, path, url.Values{"csrf": {token}, "harness": {"nope"}}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no harness") {
		t.Errorf("an unknown harness = %d, want 400 with the reason", rec.Code)
	}
}

func TestTheSessionPagesAnswerOnlyThisServersHostNames(t *testing.T) {
	// A rebound DNS name is same-origin with this server to the browser, and
	// would let a hostile page read every transcript.
	h, _ := sessionServer(t)
	for host, want := range map[string]int{
		"127.0.0.1:8080":      http.StatusOK,
		"localhost:8080":      http.StatusOK,
		"[::1]:8080":          http.StatusOK,
		"attacker.example":    http.StatusForbidden,
		"attacker.example:80": http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s = %d, want %d", host, rec.Code, want)
		}
	}
}

func TestWithoutSessionsTheRoutesDoNotExist(t *testing.T) {
	h := New(newStore(t), Options{}).Handler()
	if r := getLocal(t, h, "/sessions"); r.Code != http.StatusNotFound {
		t.Errorf("GET /sessions on a reports-only server = %d, want 404", r.Code)
	}
}

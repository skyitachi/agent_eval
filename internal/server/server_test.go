package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// addRun writes one completed run with a single passing cell.
func addRun(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	ctx := context.Background()
	started := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if err := st.CreateRun(ctx, store.Run{
		ID: id, Name: name, Status: store.RunRunning, StartedAt: started,
		DatasetName: "demo", DatasetHash: "hash-1",
		AgentKind: "claude_code", AgentModel: "sonnet", Backend: "local",
		Samples: 1, Concurrency: 1,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if err := st.FinishRun(ctx, id, store.RunCompleted, ""); err != nil {
		t.Fatalf("finish run %s: %v", id, err)
	}
	if err := st.SaveAttempt(ctx, store.Attempt{
		RunID: id, TaskID: "t1", Status: store.CellOK, StopReason: "completed",
		Graded: true, Passed: true, Score: 1, Threshold: 1,
		DurationMs: 1000, Answer: "done",
	}); err != nil {
		t.Fatalf("save cell: %v", err)
	}
}

// register records a run report and returns the row, which is what every served
// page is keyed by.
func register(t *testing.T, st *store.Store, runID string) store.Report {
	t.Helper()
	ctx := context.Background()
	opts := report.Options{}.Resolve()
	data, err := report.Load(ctx, st, runID, opts)
	if err != nil {
		t.Fatalf("load %s: %v", runID, err)
	}
	reg, err := report.Save(ctx, st, report.Request{
		Kind:    store.ReportKindRun,
		Title:   data.Title,
		Subject: data.Title,
		Params:  map[string]string{"run": runID},
		Options: opts,
	}, data)
	if err != nil {
		t.Fatalf("register %s: %v", runID, err)
	}
	return reg
}

// get performs one request through the real router, so the path patterns and
// r.PathValue are exercised rather than bypassed.
func get(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// The listing
// ---------------------------------------------------------------------------

func TestTheListingShowsRecordedReports(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")

	rec := get(t, New(st, Options{}).Handler(), "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	doc := rec.Body.String()
	if !strings.Contains(doc, `data-mode="reports"`) {
		t.Error("the listing is not in the reports mode")
	}
	if !strings.Contains(doc, reg.ID) {
		t.Errorf("the listing does not mention the recorded report %s", reg.ID)
	}
	// The listing is server-rendered, so the rows reach the page the same way
	// every other page's do: as the payload the client repaints. Asserting on
	// the static HTML would be asserting that the client never runs.
	body := extractPayload(t, doc)
	if !strings.Contains(body, `"href":"/r/`+reg.ID+`"`) {
		t.Errorf("the payload does not link to the report it lists:\n%s", body)
	}
	var listing struct {
		Reports []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Subject string `json:"subject"`
			Hash    string `json:"hash"`
			Bytes   int64  `json:"bytes"`
			Href    string `json:"href"`
		} `json:"reports"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("the listing payload does not decode: %v\n%s", err, body)
	}
	if len(listing.Reports) != 1 {
		t.Fatalf("the payload lists %d reports, want 1", len(listing.Reports))
	}
	got := listing.Reports[0]
	if got.Kind != store.ReportKindRun || got.Subject != "nightly" {
		t.Errorf("entry = %+v, want a run report about nightly", got)
	}
	if got.Href != "/r/"+reg.ID {
		t.Errorf("href = %q", got.Href)
	}
	if got.Bytes != reg.PayloadBytes {
		t.Errorf("size = %d, want the recorded %d", got.Bytes, reg.PayloadBytes)
	}
}

func TestAnEmptyRegistrySaysHowToFillIt(t *testing.T) {
	st := newStore(t)
	h := New(st, Options{}).Handler()
	rec := get(t, h, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// An empty registry is a state, not a failure: the page has to render, and
	// what it says about being empty is the client's, because the client is
	// what draws the listing.
	body := extractPayload(t, rec.Body.String())
	if !strings.Contains(body, `"reports":[]`) {
		t.Errorf("an empty registry did not produce an empty listing:\n%s", body)
	}
	if !strings.Contains(rec.Body.String(), `src="/static/app.js"`) {
		t.Error("the page does not load the client that would draw the empty state")
	}
	// And that state has to name the way out: "nothing here" without a next
	// step reads like a broken install.
	if !strings.Contains(report.JS(), "aeval report") {
		t.Error("the empty listing does not say how a report gets recorded")
	}
}

// ---------------------------------------------------------------------------
// Serving one report
// ---------------------------------------------------------------------------

func TestAServedReportSaysItStillReproduces(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")

	rec := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	doc := rec.Body.String()
	if !strings.Contains(doc, `class="recordbar"`) {
		t.Fatal("the page carries no record banner, so nothing says where it came from")
	}
	if !strings.Contains(doc, "Recorded report.") {
		t.Error("the banner does not say the document reproduced")
	}
	// The banner has to state the claim a reader could check: the hash.
	if !strings.Contains(doc, reg.PayloadHash[:12]) {
		t.Errorf("the banner does not name the recorded hash %s", reg.PayloadHash)
	}
	if !strings.Contains(doc, `data-mode="run"`) {
		t.Error("the report did not render in its own mode")
	}
	if !strings.Contains(doc, "nightly") {
		t.Error("the report does not show the run it is about")
	}
}

// TestAServedReportSaysWhenTheDatabaseHasMoved is the whole point of storing a
// hash rather than trusting the rebuild.
//
// The page is regenerated from the rows on every request, so it can always be
// rendered -- but it is not always the document that was recorded, and a page
// that showed the new one under the old link without saying so would make the
// link a lie.
func TestAServedReportSaysWhenTheDatabaseHasMoved(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")

	// Move the database: one more cell, which changes the payload.
	if err := st.SaveAttempt(context.Background(), store.Attempt{
		RunID: "r1", TaskID: "t2", Status: store.CellOK,
		Graded: true, Passed: false, Score: 0, Threshold: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil)
	doc := rec.Body.String()
	if !strings.Contains(doc, "This page is not the recorded document.") {
		t.Errorf("the banner does not report the drift:\n%s", bannerOf(t, doc))
	}
	if !strings.Contains(doc, reg.PayloadHash[:12]) {
		t.Error("the banner does not name the hash that was recorded")
	}
	if !strings.Contains(doc, `class="notice warn"`) {
		t.Error("the banner is not painted as a warning")
	}
	// And it must not claim to know which of the possible causes it is.
	if !strings.Contains(doc, "or the code that projects them has") {
		t.Error("the banner picks a cause it cannot establish")
	}
}

func TestAnOlderPayloadFormatIsNotCalledADataChange(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	// A registration from a build that wrote another payload format: its hash
	// is of a document this build cannot produce, which is exactly what the
	// reader has to be told rather than being told the data moved.
	opts := report.Options{}.Resolve()
	optionsJSON, _ := json.Marshal(opts)
	reg := store.Report{
		ID: "0102030405060708", Kind: store.ReportKindRun, Title: "nightly",
		Subject: "nightly", ParamsJSON: `{"run":"r1"}`, OptionsJSON: string(optionsJSON),
		CreatedAt: time.Now(), PayloadHash: "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		PayloadBytes: 4096, DataVersion: report.DataVersion - 1,
	}
	if err := st.SaveReport(context.Background(), reg); err != nil {
		t.Fatal(err)
	}

	doc := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil).Body.String()
	if !strings.Contains(doc, "Recorded in an older payload format.") {
		t.Errorf("the banner did not name the format:\n%s", bannerOf(t, doc))
	}
	if strings.Contains(doc, "not the recorded document") {
		t.Error("a format change was reported as the data having moved")
	}
	// The banner must say what the reader can do with the hash, which is
	// nothing: the two are not the same measurement.
	if !strings.Contains(doc, "not comparable") {
		t.Error("the banner does not say the two hashes cannot be compared")
	}
}

// TestAMatchingHashOutranksTheRecordedFormat pins the order of the verdicts.
//
// Format first would mislabel a page that reproduces exactly: the hash is the
// stronger claim, and it is the one a reader can verify. The recorded format is
// metadata about how the row was written, and when it disagrees with a matching
// hash it is the metadata that is wrong.
func TestAMatchingHashOutranksTheRecordedFormat(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	opts := report.Options{}.Resolve()
	data, err := report.Load(context.Background(), st, "r1", opts)
	if err != nil {
		t.Fatal(err)
	}
	hash, size, err := report.Fingerprint(data)
	if err != nil {
		t.Fatal(err)
	}
	optionsJSON, _ := json.Marshal(opts)
	reg := store.Report{
		ID: "1111222233334444", Kind: store.ReportKindRun, Title: "nightly",
		ParamsJSON: `{"run":"r1"}`, OptionsJSON: string(optionsJSON),
		CreatedAt: time.Now(), PayloadHash: hash, PayloadBytes: int64(size),
		DataVersion: report.DataVersion - 1,
	}
	if err := st.SaveReport(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	doc := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil).Body.String()
	if !strings.Contains(doc, "Recorded report.") {
		t.Errorf("a page that reproduces exactly was not reported as reproducing:\n%s", bannerOf(t, doc))
	}
}

// ---------------------------------------------------------------------------
// The JSON endpoint
// ---------------------------------------------------------------------------

// TestTheJSONEndpointHashesToTheRecordedHash is the registry's contract,
// checked the only way a reader can check it.
//
// The stored hash is worth something exactly because this holds: fetch the
// document, hash it, and get the number the registration recorded. If the
// endpoint encoded anything the fingerprint did not cover, the hash would be a
// private number.
func TestTheJSONEndpointHashesToTheRecordedHash(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")

	rec := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID+".json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != reg.PayloadHash {
		t.Errorf("the served document hashes to %s, but the registration recorded %s", got, reg.PayloadHash)
	}
	if got := rec.Header().Get("X-Aeval-Hash-Matches"); got != "true" {
		t.Errorf("X-Aeval-Hash-Matches = %q, want true", got)
	}
	if got := rec.Header().Get("X-Aeval-Reported-Hash"); got != reg.PayloadHash {
		t.Errorf("X-Aeval-Reported-Hash = %q, want %s", got, reg.PayloadHash)
	}
}

// TestTheHeadersLetAScriptSkipTheHash covers the other reader of this endpoint:
// a script that wants the verdict without re-implementing the encoding.
func TestTheHeadersLetAScriptSkipTheHash(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")
	if err := st.SaveAttempt(context.Background(), store.Attempt{
		RunID: "r1", TaskID: "t2", Status: store.CellOK, Graded: true, Passed: false,
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID+".json", nil)
	if got := rec.Header().Get("X-Aeval-Hash-Matches"); got != "false" {
		t.Errorf("X-Aeval-Hash-Matches = %q, want false once the rows moved", got)
	}
	current := rec.Header().Get("X-Aeval-Current-Hash")
	sum := sha256.Sum256(rec.Body.Bytes())
	if current != hex.EncodeToString(sum[:]) {
		t.Errorf("X-Aeval-Current-Hash = %s, want the hash of the body", current)
	}
	if current == reg.PayloadHash {
		t.Error("the current hash equals the recorded one, so the drift went unnoticed")
	}
}

// ---------------------------------------------------------------------------
// What it refuses
// ---------------------------------------------------------------------------

func TestAnUnknownReportNamesTheDatabase(t *testing.T) {
	st := newStore(t)
	rec := get(t, New(st, Options{}).Handler(), "/r/deadbeefdeadbeef", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	doc := rec.Body.String()
	for _, want := range []string{"deadbeefdeadbeef", st.Path(), "All recorded reports"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the failure page does not mention %q:\n%s", want, doc)
		}
	}
	// The way back is a real link, not a sentence about one.
	if !strings.Contains(doc, `href="/"`) {
		t.Error("the failure page offers no way back to the listing")
	}
}

// TestAReportWhoseSubjectIsGone covers the case deletions create: the
// registration is still there and the run it describes is not.
//
// It has to be distinguishable from "no such report", and it has to be able to
// say what the report was about -- which is the reason the subject is stored
// rather than looked up.
func TestAReportWhoseSubjectIsGone(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")
	if err := st.DeleteRun(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}

	rec := get(t, New(st, Options{}).Handler(), "/r/"+reg.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	doc := rec.Body.String()
	if !strings.Contains(doc, "The subject of this report is gone.") {
		t.Errorf("the page does not say the subject is gone:\n%s", doc)
	}
	if !strings.Contains(doc, "nightly") {
		t.Error("the page cannot say what the report was about, though the registration recorded it")
	}
	if !strings.Contains(doc, reg.ID) {
		t.Error("the page does not name the registration")
	}
}

// TestAnUnreadableRowIsRefusedRatherThanGuessedAt covers the two ways a stored
// request can be unusable.
//
// Both must refuse. Rebuilding with the defaults would produce a different
// document and then present the difference as the data having moved -- a
// diagnosis the reader could not check, and a worse answer than an error.
func TestAnUnreadableRowIsRefusedRatherThanGuessedAt(t *testing.T) {
	cases := []struct {
		name        string
		optionsJSON string
		want        string
	}{
		{"not json", `{`, "unreadable options"},
		{"no generation time", `{"max_inline_bytes":1024}`, "no generation time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			addRun(t, st, "r1", "nightly")
			if err := st.SaveReport(context.Background(), store.Report{
				ID: "badbadbadbadbad1", Kind: store.ReportKindRun, Title: "nightly",
				ParamsJSON: `{"run":"r1"}`, OptionsJSON: tc.optionsJSON,
				CreatedAt: time.Now(), DataVersion: report.DataVersion,
			}); err != nil {
				t.Fatal(err)
			}
			rec := get(t, New(st, Options{}).Handler(), "/r/badbadbadbadbad1", nil)
			if rec.Code < 500 {
				t.Fatalf("status = %d, want a server-side refusal", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("the page does not explain the refusal (%q):\n%s", tc.want, rec.Body.String())
			}
		})
	}
}

// TestARegistrationKindThisBuildCannotRebuildIsAServerError covers the row a
// future or edited database can hold.
//
// It is not a 404: the registration exists, and saying it does not would send a
// reader looking for the wrong problem.
func TestARegistrationKindThisBuildCannotRebuildIsAServerError(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	if err := st.SaveReport(context.Background(), store.Report{
		ID: "cafecafecafecafe", Kind: "reward-curve", Title: "x",
		ParamsJSON: `{}`, OptionsJSON: `{"generated_at":"2026-01-01T00:00:00Z"}`,
		CreatedAt: time.Now(), DataVersion: report.DataVersion,
	}); err != nil {
		t.Fatal(err)
	}
	rec := get(t, New(st, Options{}).Handler(), "/r/cafecafecafecafe", nil)
	if rec.Code < 500 {
		t.Fatalf("status = %d, want a server-side refusal", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reward-curve") {
		t.Errorf("the page does not name the kind it cannot rebuild:\n%s", rec.Body.String())
	}
}

func TestTheCatchAllExplainsItself(t *testing.T) {
	st := newStore(t)
	rec := get(t, New(st, Options{}).Handler(), "/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/nope") {
		t.Error("the page does not name the path that was asked for")
	}
}

func TestAPostIsNotAccepted(t *testing.T) {
	st := newStore(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	New(st, Options{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

func TestTheAssetsAreServedOnceAndCached(t *testing.T) {
	st := newStore(t)
	h := New(st, Options{}).Handler()

	for _, path := range []string{"/static/app.css", "/static/app.js"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("%s: status %d, %d bytes", path, rec.Code, rec.Body.Len())
		}
		tag := rec.Header().Get("ETag")
		if tag == "" {
			t.Fatalf("%s carries no ETag, so every visit refetches it", path)
		}
		again := get(t, h, path, map[string]string{"If-None-Match": tag})
		if again.Code != http.StatusNotModified {
			t.Errorf("%s: a conditional request was answered %d, want 304", path, again.Code)
		}
		if again.Body.Len() != 0 {
			t.Errorf("%s: a 304 carried a body", path)
		}
	}
}

// TestThePageAndItsAssetsAgreeOnThePrefix covers the served (non-standalone)
// form: the page must point at the routes the server actually registers.
func TestThePageAndItsAssetsAgreeOnThePrefix(t *testing.T) {
	st := newStore(t)
	addRun(t, st, "r1", "nightly")
	reg := register(t, st, "r1")
	h := New(st, Options{}).Handler()

	doc := get(t, h, "/r/"+reg.ID, nil).Body.String()
	if !strings.Contains(doc, `href="/static/app.css"`) {
		t.Error("the page does not link the stylesheet it was served with")
	}
	if !strings.Contains(doc, `src="/static/app.js"`) {
		t.Error("the page does not load the client it was served with")
	}
	// A served page must not inline them: that is the standalone form, and
	// shipping it here would send the same bytes on every request.
	if strings.Contains(doc, "<style>") {
		t.Error("the served page inlined its stylesheet")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// bannerOf returns the record banner, so a failure message shows the sentence
// that was actually rendered instead of the whole document.
func bannerOf(t *testing.T, doc string) string {
	t.Helper()
	i := strings.Index(doc, `class="recordbar"`)
	if i < 0 {
		return "(no record banner)"
	}
	rest := doc[i:]
	if j := strings.Index(rest, "</div>\n</div>"); j > 0 {
		rest = rest[:j]
	}
	if len(rest) > 700 {
		rest = rest[:700]
	}
	return rest
}

func extractPayload(t *testing.T, page string) string {
	t.Helper()
	const marker = "window.__AEVAL_PAYLOAD__ = "
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatal("the page embeds no payload")
	}
	rest := page[i+len(marker):]
	end := strings.Index(rest, ";</script>")
	if end < 0 {
		t.Fatal("the payload literal is not terminated")
	}
	return rest[:end]
}

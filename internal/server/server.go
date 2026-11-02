// Package server serves the reports a database records.
//
// # What it serves, and what it refuses to
//
// Every page here is rebuilt from the database on request. Nothing is cached
// and no rendered document is stored, so a report cannot go stale in the sense
// of showing something the rows no longer say -- but it *can* stop reproducing
// the document that was registered, and that is the one thing the server has to
// make visible rather than paper over. Opening a report re-derives its payload,
// hashes it the same way the registration did, and states on the page whether
// the two agree. A reader therefore gets either the recorded document or an
// explicit note that what follows is the current one instead.
//
// # Why the failure pages are pages
//
// The caller is a browser. A reader who mistyped a link, or who followed one to
// a run that has since been deleted, should land somewhere that looks like the
// tool, says which record was missing, and offers the list. An HTTP status
// alone leaves them on a blank page wondering whether the server is up.
//
// # Why a report page can now write
//
// It could not, and the reason it could not was stated as a guarantee: the
// server rebuilt documents and served them. That guarantee is withdrawn on
// purpose. Looking at a cell that says "not graded" and having no way to score
// it without going back to a shell is the moment the page stops being useful --
// the row already holds everything a verdict needs, and re-running the agent to
// reach one would judge a different attempt.
//
// So POST /r/{id}/grade reaches a verdict for one cell and writes it back. What
// remains true is the part that mattered: every read is still a rebuild from
// the rows, and the banner above the document still says whether the page
// matches the registration. A page that was graded re-reads as "not the
// recorded document", which is exactly what happened -- and that is why the
// write did not need a second, softer promise.
package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/store"
)

// Options shapes the served site.
type Options struct {
	// Title overrides the listing's heading.
	Title string
	// Limit caps how many recorded reports the listing shows. Zero means the
	// default, which is deliberately not "everything": the registry is
	// append-only, so a listing of everything grows without bound.
	Limit int
	// Judge is the grader this server was told to reach verdicts with, when it
	// was told. Nil keeps the built-in one: llm_judge with the default rubric
	// and a credential from the ANTHROPIC_* variables.
	//
	// It is the same declaration a replay attaches, and it has to be, because
	// a page can grade a cell: a judge naming a gateway, a model and its own
	// credential variable is the difference between a button that reaches a
	// verdict and one that answers "no credentials" on a machine whose shell
	// has never heard of ANTHROPIC_API_KEY.
	Judge *dataset.GraderSpec
}

const defaultLimit = 200

// Server serves one database's recorded reports.
type Server struct {
	st   *store.Store
	opts Options
	// The assets are embedded in the binary, so their ETags are computed once.
	// Deriving them per request would hash the same bytes on every hit to
	// arrive at the same answer.
	cssETag string
	jsETag  string

	// sessions is set by EnableSessions; nil serves reports only.
	sessions *SessionOptions
	// csrf is the token every POST must carry. It is per process, so a form
	// rendered by an earlier server is refused rather than honoured.
	csrf string

	// grades runs the re-grades this server accepted. It is not per request
	// because the work outlives the request that asked for it.
	grades *gradeRunner
}

// New builds a server over a store. The store is not copied: every request
// re-reads it, which is what lets a report written after the server started be
// opened without a restart.
func New(st *store.Store, opts Options) *Server {
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	s := &Server{
		st:      st,
		opts:    opts,
		cssETag: etag(report.CSS()),
		jsETag:  etag(report.JS()),
		grades:  newGradeRunner(st),
	}
	// The form token is generated even when the session pages are off, because
	// re-grading a report is a write too. It is per process: a form rendered by
	// an earlier server is refused rather than honoured, which is what keeps a
	// page left open across a restart from submitting a stale token.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition to degrade through: without a
		// token every POST would be refused, so the server would come up
		// looking healthy and refuse every write.
		panic("server: cannot generate a form token: " + err.Error())
	}
	s.csrf = hex.EncodeToString(b[:])
	return s
}

// Handler returns the site's routes.
//
// The patterns are method-qualified, so a POST to a page is answered with 405
// by the mux rather than by each handler remembering to check. The trailing
// "GET /" is the catch-all: it is less specific than every route above it, so
// it only ever sees paths nothing else claimed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleListing)
	mux.HandleFunc("GET /r/{id}", s.handleReport)
	mux.HandleFunc("POST /r/{id}/grade", s.gradeGuard(s.handleGrade))
	mux.HandleFunc("GET /static/app.css", s.handleCSS)
	mux.HandleFunc("GET /static/app.js", s.handleJS)
	s.sessionRoutes(mux)
	mux.HandleFunc("GET /", s.handleMissing)
	return mux
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

// handleListing renders the registry.
func (s *Server) handleListing(w http.ResponseWriter, r *http.Request) {
	listing, err := report.LoadReports(r.Context(), s.st, report.Options{Title: s.opts.Title}, s.opts.Limit, "")
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "The report list could not be read.",
			fmt.Sprintf("Reading the registry from %s failed: %v", s.st.Path(), err))
		return
	}
	opts := report.RenderOptions{Home: "/"}
	if s.sessions != nil {
		// The session pages are the other half of the site, and the listing is
		// where everyone lands, so the way there is a panel and not only a tab.
		opts.Nav = []report.NavLink{
			{Label: "Reports", Href: "/", Active: true},
			{Label: "Sessions", Href: "/sessions"},
			{Label: "Replays", Href: "/replays"},
		}
		opts.Callout = &report.Callout{
			Head: "Replay a recorded session",
			Note: "Browse the Claude Code, Cursor and WorkBuddy sessions on this machine, and replay one under another harness, skill set or model.",
			Links: []report.NavLink{
				{Label: "Browse sessions →", Href: "/sessions"},
				{Label: "Replays", Href: "/replays"},
			},
		}
	}
	writeHTML(w, http.StatusOK, func(out io.Writer) error {
		return report.RenderReports(out, listing, opts)
	})
}

// handleReport serves one recorded report, as a page or as the payload JSON.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The JSON endpoint is the same document under the same id rather than a
	// route of its own, so that the two cannot disagree about what "this
	// report" means. The mux matches whole segments only, so the suffix is
	// stripped here rather than expressed in the pattern.
	wantJSON := strings.HasSuffix(id, ".json")
	if wantJSON {
		id = strings.TrimSuffix(id, ".json")
	}
	if id == "" {
		s.missingReport(w, r, id)
		return
	}

	reg, err := s.st.GetReport(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.missingReport(w, r, id)
			return
		}
		s.failure(w, http.StatusInternalServerError, "The report could not be read.",
			fmt.Sprintf("Reading the registry from %s failed: %v", s.st.Path(), err))
		return
	}

	opts, err := report.ParseOptions(reg)
	if err != nil {
		// Not a fallback to the defaults: rebuilding with a different field
		// budget produces a different document and then presents the difference
		// as the data having moved, which is a claim the reader cannot check.
		s.failure(w, http.StatusInternalServerError, "This report cannot be rebuilt.", err.Error())
		return
	}

	payload, mode, err := report.Derive(r.Context(), s.st, reg, opts)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.gone(w, reg, err)
			return
		}
		s.failure(w, http.StatusInternalServerError, "This report cannot be rebuilt.", err.Error())
		return
	}

	current, _, err := report.Fingerprint(payload)
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "This report cannot be hashed.", err.Error())
		return
	}

	if wantJSON {
		s.writePayload(w, payload, reg, current)
		return
	}
	status := s.gradeStatus(r, reg)
	grade := s.gradeAction(reg, mode)
	if grade != nil && status.Job != nil && status.Job.Active() {
		grade.Grading = &report.Grading{TaskID: status.Job.TaskID, Sample: status.Job.Sample}
	}
	writeHTML(w, http.StatusOK, func(out io.Writer) error {
		return report.RenderPayload(out, mode, payload, report.RenderOptions{
			Home: "/",
			Nav:  []report.NavLink{{Label: "All reports", Href: "/"}},
			// The record is what the registration claimed and what the rebuild
			// produced, side by side. It is the whole reason the hash is stored.
			Record: recordFor(reg, current),
			// The notice is what the page's own last action produced. It comes
			// first because it is the most recent fact on the page, and it is
			// read back from the rows rather than taken from the URL.
			Notice: status.Notice,
			// A page with grading in flight follows it by reloading, which is
			// how the session pages already follow a replay.
			MetaRefresh: status.Refresh,
			Grade:       grade,
		})
	})
}

// writePayload serves the payload as JSON, with the verdict in headers so that
// a script can check a registration without parsing the document.
func (s *Server) writePayload(w http.ResponseWriter, payload any, reg store.Report, current string) {
	raw, err := report.DataJSON(payload)
	if err != nil {
		s.failure(w, http.StatusInternalServerError, "This report cannot be encoded.", err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Aeval-Reported-Hash", reg.PayloadHash)
	h.Set("X-Aeval-Current-Hash", current)
	h.Set("X-Aeval-Hash-Matches", strconv.FormatBool(current == reg.PayloadHash))
	h.Set("X-Aeval-Data-Version", strconv.Itoa(reg.DataVersion))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleMissing answers anything the routes above did not claim.
func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request) {
	s.failure(w, http.StatusNotFound, "No such page.",
		fmt.Sprintf("There is nothing at %s. The recorded reports are listed at /.", r.URL.Path))
}

func (s *Server) missingReport(w http.ResponseWriter, r *http.Request, id string) {
	message := fmt.Sprintf("No report with id %s in %s.", strconv.Quote(id), s.st.Path())
	if id == "" {
		message = fmt.Sprintf("No report id was given in %s.", r.URL.Path)
	}
	s.failure(w, http.StatusNotFound, "No such report.", message+
		" A report is openable only under an id one was registered with, which is the id the command that produced it printed.")
}

// gone answers a registration whose subject is no longer in the database.
//
// It is a 404 and not a 500: nothing is broken, the run the report was about
// simply is not here any more. The page says what the report was, because the
// registration stored that -- which is the reason it stored it.
func (s *Server) gone(w http.ResponseWriter, reg store.Report, err error) {
	what := "what this report was about"
	if reg.Subject != "" {
		what = "the " + reg.Kind + " this report was about (" + reg.Subject + ")"
	}
	s.failure(w, http.StatusNotFound, "The subject of this report is gone.",
		fmt.Sprintf("%s is no longer in %s, so the document cannot be rebuilt. "+
			"Report %s was registered %s. (%v)",
			what, s.st.Path(), reg.ID, stamp(reg.CreatedAt), err))
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, "text/css; charset=utf-8", report.CSS(), s.cssETag)
}

func (s *Server) handleJS(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, "text/javascript; charset=utf-8", report.JS(), s.jsETag)
}

// serveAsset answers one embedded asset.
//
// The body is a constant of this build, so a conditional request is answered
// without hashing anything again: the ETag was computed once, and if it still
// matches, the client's copy is this build's copy.
//
// Which is why the asset is marked no-cache rather than given a lifetime. The
// page that loads it is no-store, so a client that may keep the script for five
// minutes can end up running last build's client against this build's page --
// and the reader's report is the place that shows it. That is not a theory: a
// rebuilt binary served a client whose "grade" button submitted a detached form
// and did nothing, and the fix looked like it had not taken effect until the
// browser's five minutes were up. Revalidating costs one conditional request on
// loopback and answers 304.
func serveAsset(w http.ResponseWriter, r *http.Request, contentType, body, tag string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", tag)
	h.Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func etag(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// ---------------------------------------------------------------------------
// Guards shared by every write
// ---------------------------------------------------------------------------

// guardHeaders are the headers a page that can change something must carry.
//
// X-Frame-Options and frame-ancestors stop the page being framed by another
// site, which matters most for the one control that costs money: a framed
// report is a button a stranger can put under the reader's cursor. The policy
// deliberately constrains framing only -- the report page's own client is an
// inline script, and a default-src that forbade it would break the page to
// protect a control the token already protects.
func guardHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// checkPost enforces the two rules a state-changing request must satisfy: it
// came from a form this server rendered (the token), and from a page on this
// site (the Origin, when the browser sends one).
//
// It answers the request itself when a rule fails, and reports whether the
// caller should carry on. Returning the response instead of writing it is what
// keeps the two entry points -- the session pages and the report's grade
// button -- from drifting into two slightly different checks.
func (s *Server) checkPost(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			s.failure(w, http.StatusForbidden, "This form was sent from another site.",
				fmt.Sprintf("The request came from %s, not from this server. Open the page here and press the button again.", origin))
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.failure(w, http.StatusBadRequest, "The form could not be read.", err.Error())
		return false
	}
	got := r.PostForm.Get("csrf")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
		s.failure(w, http.StatusForbidden, "This form is out of date.",
			"The form token does not match this server's. It changes every time the server starts: "+
				"reload the page and press the button again.")
		return false
	}
	return true
}

// allowedHost accepts the names this server can legitimately be reached by: an
// IP literal (whatever it was bound to) or localhost. A DNS name is refused
// because a rebound one is indistinguishable from a real one.
func allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host) != nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// recordFor states how a registration's stored hash relates to the document
// that just came out of the database.
//
// A disagreement has more than one possible cause and the server cannot tell
// which one it is looking at, so it names them instead of picking. The rows may
// have moved (a run resumed, a run deleted and re-made, another level added to
// a sweep); the code that projects them may have changed without anyone bumping
// the payload format; or the format may have changed, which is the one case
// that *is* knowable, because DataVersion is stored with the row. Reporting a
// single cause would be a guess presented as a diagnosis.
func recordFor(reg store.Report, current string) *report.Record {
	rec := &report.Record{
		ID:          reg.ID,
		CreatedAt:   reg.CreatedAt.UTC().Format(time.RFC3339),
		Hash:        reg.PayloadHash,
		CurrentHash: current,
		Match:       current == reg.PayloadHash,
	}
	switch {
	case rec.Match:
		rec.Severity = "info"
		rec.Head = "Recorded report."
		rec.Note = fmt.Sprintf("Registered %s as sha256 %s. Rebuilding this view just now produced "+
			"the same bytes, so this page is the recorded document.", stamp(reg.CreatedAt), short(reg.PayloadHash))

	case reg.DataVersion != report.DataVersion:
		rec.Severity = "warn"
		rec.Head = "Recorded in an older payload format."
		rec.Note = fmt.Sprintf("Registered %s in payload format v%d; this build writes v%d, so the two hashes "+
			"are not comparable and the disagreement says nothing about the data. What follows is this build's "+
			"document.", stamp(reg.CreatedAt), reg.DataVersion, report.DataVersion)

	default:
		rec.Severity = "warn"
		rec.Head = "This page is not the recorded document."
		rec.Note = fmt.Sprintf("Registered %s as sha256 %s; the same view now hashes to %s. Either the rows this "+
			"report was built from have changed since, or the code that projects them has. What follows is the "+
			"current document.", stamp(reg.CreatedAt), short(reg.PayloadHash), short(current))
	}
	return rec
}

// stamp renders an instant for a sentence. It is RFC 3339 rather than a local
// format because it is the stored value: a reader comparing the banner against
// the database is comparing strings, and a local rendering would be a
// translation of one side only.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "at an unrecorded time"
	}
	return t.UTC().Format(time.RFC3339)
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12] + "..."
}

// writeHTML renders into memory and then sends the result.
//
// Rendering straight into the response would send a 200 and half a document
// before a template error could be noticed, and there is no way to take that
// back: the reader would get a truncated page with a success status. Buffering
// costs the size of one page.
func writeHTML(w http.ResponseWriter, status int, render func(io.Writer) error) {
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		http.Error(w, "aeval: rendering this page failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// failure renders an error page in the report shell.
func (s *Server) failure(w http.ResponseWriter, status int, title, message string) {
	s.failureWith(w, status, title, message, "/", "All recorded reports")
}

// failureWith renders an error page whose way back is the caller's choice.
//
// It exists because a failure reached from a report should return there: the
// reader arrived from a page and should not have to rebuild its URL from the
// address bar to get back to it.
func (s *Server) failureWith(w http.ResponseWriter, status int, title, message, home, homeText string) {
	writeHTML(w, status, func(out io.Writer) error {
		return report.RenderError(out, report.ErrorOptions{
			Title:    title,
			Message:  message,
			Home:     home,
			HomeText: homeText,
		}, report.RenderOptions{})
	})
}

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
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
}

// New builds a server over a store. The store is not copied: every request
// re-reads it, which is what lets a report written after the server started be
// opened without a restart.
func New(st *store.Store, opts Options) *Server {
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	return &Server{
		st:      st,
		opts:    opts,
		cssETag: etag(report.CSS()),
		jsETag:  etag(report.JS()),
	}
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
	mux.HandleFunc("GET /static/app.css", s.handleCSS)
	mux.HandleFunc("GET /static/app.js", s.handleJS)
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
	writeHTML(w, http.StatusOK, func(out io.Writer) error {
		return report.RenderReports(out, listing, report.RenderOptions{Home: "/"})
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
	writeHTML(w, http.StatusOK, func(out io.Writer) error {
		return report.RenderPayload(out, mode, payload, report.RenderOptions{
			Home: "/",
			Nav:  []report.NavLink{{Label: "All reports", Href: "/"}},
			// The record is what the registration claimed and what the rebuild
			// produced, side by side. It is the whole reason the hash is stored.
			Record: recordFor(reg, current),
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
func serveAsset(w http.ResponseWriter, r *http.Request, contentType, body, tag string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", tag)
	h.Set("Cache-Control", "public, max-age=300")
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
	writeHTML(w, status, func(out io.Writer) error {
		return report.RenderError(out, report.ErrorOptions{
			Title:    title,
			Message:  message,
			Home:     "/",
			HomeText: "All recorded reports",
		}, report.RenderOptions{})
	})
}

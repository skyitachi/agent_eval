// Package web serves evaluation results over HTTP.
//
// It is a thin layer over package report: every page it returns is the same
// document the offline report writer produces, and every JSON endpoint returns
// the same payload that document embeds. A client therefore cannot tell whether
// it read a file or called the server, and the two front ends cannot drift.
//
// # Threat model
//
// This is a local, single-user tool, but it is a local tool that renders
// model-generated text and can delete data, so two things are still enforced:
//
//   - The default bind address is the loopback interface. The server has no
//     authentication and no notion of a user, so exposing it to a network is a
//     deliberate act (--addr), not an accident.
//   - Destructive routes are off unless explicitly enabled, and they reject
//     requests that carry a foreign Origin header. A page the user happens to
//     have open cannot silently POST to a loopback service: the browser attaches
//     Origin to a cross-origin POST, and the check rejects it.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/rlenv"
	"github.com/skyitachi/agenteval/internal/storage"
)

// DefaultAddr keeps the server on the loopback interface.
const DefaultAddr = "127.0.0.1:8787"

// Config configures a Server.
type Config struct {
	// Store is the database to read. Required.
	Store *storage.Store
	// Addr is the listen address. Empty uses DefaultAddr.
	Addr string
	// Title is the document title prefix.
	Title string
	// Report controls how runs are projected for rendering.
	Report report.Options
	// AllowDelete enables the destructive run-deletion route.
	AllowDelete bool
	// LiveRefreshSeconds adds a meta refresh to the index so a run in progress
	// can be watched. Zero disables it.
	LiveRefreshSeconds int
	// RL, when set, mounts the RL environment service under /api/rl. Nil is the
	// default and the safe default: these routes start sandboxes, spend money
	// and can execute commands.
	RL *rlenv.Manager
	// Logger receives request and error logs. Nil uses a text handler on stderr.
	Logger *slog.Logger
}

// Server serves an evaluation database.
type Server struct {
	cfg     Config
	log     *slog.Logger
	handler http.Handler
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("web: store is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, log: cfg.Logger}
	s.handler = s.routes()
	return s, nil
}

// Handler returns the HTTP handler, useful for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the resolved listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

// ListenAndServe serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: rendering a large report can legitimately take longer
		// than a typical request budget, and a truncated response is worse than
		// a slow one.
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("serving evaluation results", "addr", ln.Addr().String(), "delete", s.cfg.AllowDelete)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /runs/{id}", s.handleRunPage)
	mux.HandleFunc("GET /compare", s.handleComparePage)
	mux.HandleFunc("GET /static/app.css", s.handleCSS)
	mux.HandleFunc("GET /static/app.js", s.handleJS)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /api/runs", s.handleAPIList)
	mux.HandleFunc("GET /api/runs/{id}", s.handleAPIRun)
	mux.HandleFunc("GET /api/runs/{id}/tasks/{task}", s.handleAPITask)
	mux.HandleFunc("POST /api/runs/{id}/delete", s.handleAPIDelete)

	// The RL environment routes are always mounted; rlRoutes installs the guard
	// that refuses them when no manager is configured. See rlRoutes for why the
	// refusal is a 404 with a reason rather than an absent route.
	s.rlRoutes(mux)

	return s.recoverer(s.logger(mux))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "duration", time.Since(start).Round(time.Millisecond))
	})
}

// recoverer turns a handler panic into a 500.
//
// A report renders whatever is in the database, including rows written by an
// older build. A malformed row must produce an error page, not a dead server.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("handler panic", "path", r.URL.Path, "panic", fmt.Sprint(v))
				http.Error(w, "internal error while rendering the report", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// ServeMux routes everything unmatched to "/", so reject anything that is
	// not the root rather than rendering the index for a typo'd URL.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	idx, err := report.LoadIndex(r.Context(), s.cfg.Store, limit, s.cfg.Report)
	if err != nil {
		s.fail(w, r, "load run index", err)
		return
	}
	idx.Title = s.title("Runs")
	idx.Capabilities = map[string]bool{"delete": s.cfg.AllowDelete}
	if s.cfg.LiveRefreshSeconds > 0 {
		idx.LiveRefreshSeconds = s.cfg.LiveRefreshSeconds
	}

	opts := s.renderOptions(r, "Runs", nil)
	opts.MetaRefresh = refreshMeta(idx.LiveRefreshSeconds)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := report.RenderIndex(w, idx, opts); err != nil {
		s.log.Error("render index", "error", err)
	}
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := s.load(r, id)
	if err != nil {
		s.failLoad(w, r, "run", id, err)
		return
	}
	opts := s.renderOptions(r, report.TitleOf(data), []report.NavLink{
		{Label: "Runs", Href: "/"},
		{Label: "This run", Href: "/runs/" + id, Active: true},
		{Label: "Compare", Href: "/compare?left=" + id + "&right=" + id},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := report.RenderRun(w, data, opts); err != nil {
		s.log.Error("render run", "run", id, "error", err)
	}
}

func (s *Server) handleComparePage(w http.ResponseWriter, r *http.Request) {
	left := r.URL.Query().Get("left")
	right := r.URL.Query().Get("right")
	if left == "" || right == "" {
		http.Error(w, "provide ?left=<run-id>&right=<run-id>", http.StatusBadRequest)
		return
	}
	data, err := report.LoadComparison(r.Context(), s.cfg.Store, left, right, s.cfg.Report)
	if err != nil {
		s.fail(w, r, "load comparison", err)
		return
	}
	opts := s.renderOptions(r, report.TitleOf(data), []report.NavLink{
		{Label: "Runs", Href: "/"},
		{Label: left, Href: "/runs/" + left},
		{Label: right, Href: "/runs/" + right},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := report.RenderComparison(w, data, opts); err != nil {
		s.log.Error("render comparison", "left", left, "right", right, "error", err)
	}
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(report.CSS()))
}

func (s *Server) handleJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(report.JS()))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": report.DataVersion})
}

// ---------------------------------------------------------------------------
// JSON API
// ---------------------------------------------------------------------------

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	idx, err := report.LoadIndex(r.Context(), s.cfg.Store, 200, s.cfg.Report)
	if err != nil {
		s.fail(w, r, "load run index", err)
		return
	}
	idx.Capabilities = map[string]bool{"delete": s.cfg.AllowDelete}
	writeJSON(w, http.StatusOK, idx)
}

func (s *Server) handleAPIRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := s.load(r, id)
	if err != nil {
		s.failLoad(w, r, "run", id, err)
		return
	}
	// The payload is serialised through the same encoder the HTML report embeds,
	// so the two representations of a run stay identical.
	raw, err := report.DataJSON(data)
	if err != nil {
		s.fail(w, r, "encode run payload", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) handleAPITask(w http.ResponseWriter, r *http.Request) {
	runID, taskID := r.PathValue("id"), r.PathValue("task")
	sample := 0
	if v := r.URL.Query().Get("sample"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "sample must be a non-negative integer", http.StatusBadRequest)
			return
		}
		sample = n
	}
	data, err := s.load(r, runID)
	if err != nil {
		s.failLoad(w, r, "run", runID, err)
		return
	}
	for i := range data.Tasks {
		t := &data.Tasks[i]
		if t.ID == taskID && t.SampleIndex == sample {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":     "task not found in run",
		"run_id":    runID,
		"task_id":   taskID,
		"sample":    sample,
		"run_tasks": len(data.Tasks),
	})
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowDelete {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "deletion is disabled; start the server with --allow-delete to enable it",
		})
		return
	}
	// A cross-origin POST carries Origin. Rejecting a foreign one stops a page
	// the user happens to have open from deleting their results.
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request rejected"})
		return
	}
	id := r.PathValue("id")
	if err := s.cfg.Store.DeleteRun(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found", "run_id": id})
			return
		}
		s.fail(w, r, "delete run", err)
		return
	}
	s.log.Warn("deleted run", "run", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// load projects a run. It is deliberately uncached: a report rendered while a
// run is still in flight must show the work completed so far, and a stale cache
// would be the one thing a person watching a run would notice.
func (s *Server) load(r *http.Request, runID string) (*report.Data, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("%w: empty run id", storage.ErrNotFound)
	}
	return report.Load(r.Context(), s.cfg.Store, runID, s.cfg.Report)
}

func (s *Server) renderOptions(r *http.Request, title string, nav []report.NavLink) report.RenderOptions {
	if nav == nil {
		nav = []report.NavLink{{Label: "Runs", Href: "/"}}
	}
	return report.RenderOptions{
		Title:      s.title(title),
		Standalone: false,
		AssetBase:  "/static/",
		Home:       "/",
		Nav:        nav,
	}
}

func (s *Server) title(part string) string {
	if s.cfg.Title == "" {
		return part
	}
	return part + " · " + s.cfg.Title
}

// fail reports an error. The API namespace gets a JSON body, because its client
// is a program; a browser hitting /runs/x gets a plain message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error(what, "path", r.URL.Path, "error", err)
	status := http.StatusInternalServerError
	if errors.Is(err, storage.ErrNotFound) {
		status = http.StatusNotFound
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, status, map[string]any{"error": err.Error(), "operation": what})
		return
	}
	http.Error(w, fmt.Sprintf("%s: %v", what, err), status)
}

func (s *Server) failLoad(w http.ResponseWriter, r *http.Request, kind, id string, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		s.log.Info("not found", "kind", kind, "id", id)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such " + kind, kind + "_id": id})
			return
		}
		http.Error(w, fmt.Sprintf("no such %s: %s", kind, id), http.StatusNotFound)
		return
	}
	s.fail(w, r, "load "+kind, err)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// sameOrigin reports whether an Origin header refers to this server.
func sameOrigin(origin, host string) bool {
	origin = strings.TrimSuffix(origin, "/")
	origin = strings.TrimPrefix(origin, "http://")
	origin = strings.TrimPrefix(origin, "https://")
	return strings.EqualFold(origin, host)
}

func refreshMeta(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return strconv.Itoa(seconds)
}

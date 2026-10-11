package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/rlenv"
)

// ---------------------------------------------------------------------------
// RL environment routes
//
// These routes are the only ones on this server that make the process *act*
// rather than render: they start sandboxes, run the agent and (when exec is
// enabled) run arbitrary commands. Three separate gates therefore apply, in
// order:
//
//  1. a manager must exist at all (--rl), otherwise every route answers 404 and
//     names the flag;
//  2. every request must come from this server's own origin and this machine,
//     the same check the deletion route uses, because a loopback service is
//     reachable from any page the user has open;
//  3. the command and file routes need exec explicitly enabled (--rl-allow-exec),
//     because for the local backend they are arbitrary code execution as the
//     harness's user.
//
// The body size of every request is bounded. A training loop that accidentally
// posts a dataset instead of a patch must not be able to make the harness
// allocate it.
// ---------------------------------------------------------------------------

const (
	// rlSmallBody bounds control-plane requests: ids, commands, timeouts.
	rlSmallBody = 64 << 10
	// rlFileBody bounds a file a harness submits.
	rlFileBody = 8 << 20
)

// rlRoutes mounts the RL environment service.
//
// The routes are mounted unconditionally, even when no manager is configured.
// Mounting them conditionally would make "this server is not an RL service"
// arrive as a bare 404 for GET -- the index handler owns that method -- and as
// a 405 for everything else, which reads like a typo in the client rather than
// a missing flag. rlGuard answers all of them the same way, with the flag name.
//
// The guard is applied by the wrapper rather than by each handler, so it cannot
// be forgotten when a route is added: a handler reached through rlRoutes always
// has a non-nil manager.
func (s *Server) rlRoutes(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.rlGuard(w, r) {
				return
			}
			h(w, r)
		})
	}
	handle("GET /api/rl", s.handleRLInfo)
	handle("GET /api/rl/tasks", s.handleRLTasks)
	handle("GET /api/rl/tasks/{task}", s.handleRLTask)
	handle("GET /api/rl/envs", s.handleRLListEnvs)
	handle("POST /api/rl/envs", s.handleRLOpenEnv)
	handle("GET /api/rl/envs/{id}", s.handleRLEnv)
	handle("GET /api/rl/envs/{id}/archive", s.handleRLArchive)
	handle("POST /api/rl/envs/{id}/exec", s.handleRLExec)
	handle("PUT /api/rl/envs/{id}/files", s.handleRLWriteFile)
	handle("POST /api/rl/envs/{id}/verify", s.handleRLVerify)
	handle("POST /api/rl/envs/{id}/delete", s.handleRLClose)
	handle("POST /api/rl/rollouts", s.handleRLRollout)
}

// rlGuard enforces the two gates that apply to every RL route: a manager must
// exist, and the request must come from this server's own origin.
func (s *Server) rlGuard(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.RL == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "the RL environment service is not enabled; start the server with --rl",
		})
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request rejected"})
		return false
	}
	return true
}

// rlFail maps a manager error onto a status code.
//
// The mapping matters to a training loop: a 404 means a new environment is
// needed, a 409 means this one is finished, and a 429 means slow down. Collapsing
// them into 500 would leave an automated caller retrying a call that can never
// succeed.
func (s *Server) rlFail(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, rlenv.ErrNoSuchEnv):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, rlenv.ErrEnvVerified):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, rlenv.ErrExecDisabled):
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": err.Error(),
			"hint":  "start the server with --rl-allow-exec to run commands inside environments",
		})
	case errors.Is(err, rlenv.ErrLimit):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
	default:
		s.fail(w, r, what, err)
	}
}

// decodeRLBody decodes a JSON body with a size cap.
func decodeRLBody(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
			return errors.New("content-type must be application/json")
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	// Unknown fields are an error on purpose: a trainer that misspells "task_id"
	// and gets a 201 for an environment it did not ask for is worse off than one
	// that gets a 400.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleRLInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset":    s.cfg.RL.DatasetName(),
		"stats":      s.cfg.RL.Stats(),
		"tasks":      len(s.cfg.RL.Tasks()),
		"operations": rlOperations(s.cfg.RL.Options().AllowExec),
	})
}

// rlOperations documents the API for a client that is discovering it.
func rlOperations(execEnabled bool) []map[string]any {
	ops := []map[string]any{
		{"method": "GET", "path": "/api/rl/tasks", "what": "list the tasks available as environments"},
		{"method": "POST", "path": "/api/rl/envs", "what": "open an environment for one task"},
		{"method": "GET", "path": "/api/rl/envs/{id}", "what": "inspect one environment"},
		{"method": "GET", "path": "/api/rl/envs/{id}/archive", "what": "the evidence behind a verdict"},
		{"method": "POST", "path": "/api/rl/envs/{id}/verify", "what": "grade, project the reward, close the environment"},
		{"method": "POST", "path": "/api/rl/envs/{id}/delete", "what": "destroy an environment without grading it"},
		{"method": "POST", "path": "/api/rl/rollouts", "what": "run the configured agent once and return its reward"},
	}
	if execEnabled {
		ops = append(ops,
			map[string]any{"method": "POST", "path": "/api/rl/envs/{id}/exec", "what": "run a command inside the environment"},
			map[string]any{"method": "PUT", "path": "/api/rl/envs/{id}/files", "what": "write a file into the environment"},
		)
	}
	return ops
}

func (s *Server) handleRLTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset": s.cfg.RL.DatasetName(),
		"tasks":   s.cfg.RL.Tasks(),
	})
}

func (s *Server) handleRLTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("task")
	t, ok := s.cfg.RL.Task(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found", "task_id": id})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleRLListEnvs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"envs":  s.cfg.RL.List(),
		"stats": s.cfg.RL.Stats(),
	})
}

type rlOpenRequest struct {
	TaskID string `json:"task_id"`
}

func (s *Server) handleRLOpenEnv(w http.ResponseWriter, r *http.Request) {
	var req rlOpenRequest
	if err := decodeRLBody(w, r, &req, rlSmallBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	env, err := s.cfg.RL.Open(r.Context(), req.TaskID)
	if err != nil {
		s.rlFail(w, r, "open environment", err)
		return
	}
	s.log.Info("rl environment opened", "env", env.ID(), "task", req.TaskID)
	writeJSON(w, http.StatusCreated, s.cfg.RL.Info(env))
}

func (s *Server) handleRLEnv(w http.ResponseWriter, r *http.Request) {
	env, err := s.cfg.RL.Get(r.PathValue("id"))
	if err != nil {
		s.rlFail(w, r, "load environment", err)
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.RL.Info(env))
}

func (s *Server) handleRLArchive(w http.ResponseWriter, r *http.Request) {
	arch, err := s.cfg.RL.Archive(r.Context(), r.PathValue("id"))
	if err != nil {
		s.rlFail(w, r, "load environment archive", err)
		return
	}
	writeJSON(w, http.StatusOK, arch)
}

type rlExecRequest struct {
	Command  string `json:"command"`
	TimeoutS int    `json:"timeout_s,omitempty"`
}

func (s *Server) handleRLExec(w http.ResponseWriter, r *http.Request) {
	var req rlExecRequest
	if err := decodeRLBody(w, r, &req, rlSmallBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var timeout time.Duration
	if req.TimeoutS > 0 {
		timeout = time.Duration(req.TimeoutS) * time.Second
	}
	log, err := s.cfg.RL.Exec(r.Context(), r.PathValue("id"), req.Command, timeout)
	// A command that ran and exited non-zero is a normal result, not an HTTP
	// error: the exit code is the answer. Only a failure to run at all is one.
	if err != nil && log == nil {
		s.rlFail(w, r, "run command", err)
		return
	}
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"exec": log, "error": errText(err)})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type rlWriteFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (s *Server) handleRLWriteFile(w http.ResponseWriter, r *http.Request) {
	var req rlWriteFileRequest
	if err := decodeRLBody(w, r, &req, rlFileBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.cfg.RL.WriteFile(r.Context(), r.PathValue("id"), req.Path, []byte(req.Content)); err != nil {
		s.rlFail(w, r, "write file", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"written": req.Path, "bytes": len(req.Content)})
}

func (s *Server) handleRLVerify(w http.ResponseWriter, r *http.Request) {
	res, err := s.cfg.RL.Verify(r.Context(), r.PathValue("id"))
	if err != nil {
		// A repeat verification returns the original verdict with a 409 rather
		// than grading twice: the reward a policy already saw must not change.
		if errors.Is(err, rlenv.ErrEnvVerified) && res != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "result": res})
			return
		}
		s.rlFail(w, r, "verify environment", err)
		return
	}
	s.log.Info("rl environment verified", "env", res.EnvID, "task", res.TaskID,
		"reward", res.Reward, "passed", res.Passed)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRLClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.cfg.RL.Close(r.Context(), id); err != nil {
		s.rlFail(w, r, "close environment", err)
		return
	}
	s.log.Info("rl environment closed", "env", id)
	writeJSON(w, http.StatusOK, map[string]any{"closed": id})
}

type rlRolloutRequest struct {
	TaskID string `json:"task_id"`
}

func (s *Server) handleRLRollout(w http.ResponseWriter, r *http.Request) {
	var req rlRolloutRequest
	if err := decodeRLBody(w, r, &req, rlSmallBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res, err := s.cfg.RL.Rollout(r.Context(), req.TaskID)
	if err != nil {
		s.rlFail(w, r, "roll out", err)
		return
	}
	s.log.Info("rl rollout finished", "task", res.TaskID, "reward", res.Reward,
		"passed", res.Passed, "cost_usd", res.CostUSD)
	writeJSON(w, http.StatusOK, res)
}

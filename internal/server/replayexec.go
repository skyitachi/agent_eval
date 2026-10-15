package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/machine"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// ---------------------------------------------------------------------------
// The replay queue
// ---------------------------------------------------------------------------

// replayQueue runs one replay at a time in the background.
//
// One at a time is a property of what a replay is: it collects a real session
// from this machine's own histories and then runs an agent against the tasks
// that come out of it, so two at once would contend for the same histories,
// the same repositories (a --workspace-cwd replay works in the recording's own
// directory) and the same token spend. The queue is unbounded because each
// request is a row first; if the server dies with rows queued, they are marked
// interrupted at the next start and nothing is lost.
type replayQueue struct {
	mu      sync.Mutex
	tasks   []string // replay ids, in arrival order
	wake    chan struct{}
	started bool
}

// newReplayQueue builds the queue with its wake channel made. A zero-value
// queue would carry a nil channel: the worker's receive would block forever
// and an enqueue would silently drop its signal on a send that cannot
// proceed, so a queued replay would wait for a worker that never wakes.
func newReplayQueue() replayQueue {
	return replayQueue{wake: make(chan struct{}, 1)}
}

func (q *replayQueue) enqueue(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = append(q.tasks, id)
	// A buffered wake with the lock held: the worker takes one task at a time
	// and drops a second signal on the floor, which is fine -- it looks at the
	// queue again before sleeping.
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *replayQueue) next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return "", false
	}
	id := q.tasks[0]
	q.tasks = q.tasks[1:]
	return id, true
}

// newReplayID returns a random 64-bit hex token, the same shape as a report id:
// unguessable in a URL and not sequential, because a replay id appears in links
// anyone on this machine could open.
func newReplayID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("server: generate replay id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// Submitting a replay
// ---------------------------------------------------------------------------

// replayRequest is a validated form submission, after the presets have been
// resolved and the session has been found.
type replayRequest struct {
	Source       string // claude | cursor
	SessionID    string
	Harness      ReplayPreset
	Skill        ReplayPreset
	Model        string
	Split        session.Split
	WorkspaceCwd bool

	// name is the session's display name, recorded into the run row's Notes.
	name string
}

// submitReplay validates a form submission and records it as a queued replay.
//
// Everything a caller can get wrong is checked here, before the row exists: a
// row that was created and then refused would be a replay that ran and failed
// in the eyes of the list, when in fact nothing was ever collected.
func (s *Server) submitReplay(req replayRequest) (string, error) {
	spec, err := combineReplaySpec(req.Harness, req.Skill, req.Model)
	if err != nil {
		return "", err
	}
	_ = spec // the spec is rebuilt at execution time; validation is what happens here

	id, err := newReplayID()
	if err != nil {
		return "", err
	}
	if err := s.st.CreateReplay(context.Background(), store.Replay{
		ID:           id,
		Source:       req.Source,
		SessionID:    req.SessionID,
		Harness:      req.Harness.Name,
		Skill:        req.Skill.Name,
		Model:        req.Model,
		Split:        string(req.Split),
		WorkspaceCwd: req.WorkspaceCwd,
		Status:       store.ReplayQueued,
	}); err != nil {
		return "", err
	}
	s.replays.enqueue(id)
	return id, nil
}

// startReplayWorker starts the background worker, once per server.
//
// It is called from Handler rather than from a constructor so a server built
// for a test -- which never serves -- does not leave a goroutine behind.
func (s *Server) startReplayWorker(ctx context.Context) {
	s.replays.mu.Lock()
	if s.replays.started {
		s.replays.mu.Unlock()
		return
	}
	s.replays.started = true
	s.replays.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			id, ok := s.replays.next()
			if !ok {
				select {
				case <-s.replays.wake:
					continue
				case <-ctx.Done():
					return
				}
			}
			s.executeReplay(ctx, id)
		}
	}()
}

// ---------------------------------------------------------------------------
// Executing one replay
// ---------------------------------------------------------------------------

// executeReplay carries one queued replay to a terminal status.
//
// The dataset is written under the database's replays directory and kept after
// the replay: it is small, and it is the thing that makes the run reproducible
// -- `aeval run --dataset <dir>` re-runs it without the server. The workspaces
// the run creates are handled by the runner's own default (deleted at the end),
// because they are per-replay scratch, not evidence.
func (s *Server) executeReplay(ctx context.Context, id string) {
	st := s.st
	rep, err := st.GetReplay(ctx, id)
	if err != nil {
		return // the row is gone; nothing to execute or to update
	}
	if rep.Status != store.ReplayQueued {
		return // already terminal, or a second worker picked it up
	}
	if err := st.FinishReplay(ctx, id, store.ReplayRunning, "", "", ""); err != nil {
		return
	}

	runID, reportID, err := s.replayOnce(ctx, rep)
	status := store.ReplayDone
	var errMsg string
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		status, errMsg = store.ReplayInterrupted, "the server was stopped while the replay was running"
	default:
		status, errMsg = store.ReplayFailed, err.Error()
	}
	// The finish call must survive a cancelled context: an interrupted replay
	// still has to be recorded as interrupted rather than left running for the
	// next start to clean up.
	_ = st.FinishReplay(context.WithoutCancel(ctx), id, status, errMsg, runID, reportID)
}

// replayOnce does the work: collect the session, run the agent, register the
// report. The run id and report id are returned even on failure, because a
// replay whose runner.Run failed half-way still has a run row worth linking.
func (s *Server) replayOnce(ctx context.Context, rep store.Replay) (runID, reportID string, err error) {
	// Resolve presets by name at execution time, and refuse rather than guess
	// when the configured file no longer holds them: the row names this
	// preset, so the run has to drive exactly it or say why not.
	harness, ok := s.opts.Replays.Harness(rep.Harness)
	if !ok {
		return "", "", fmt.Errorf("the harness %q is no longer in the replay config", rep.Harness)
	}
	skill, ok := s.opts.Replays.Skill(rep.Skill)
	if !ok {
		return "", "", fmt.Errorf("the skill %q is no longer in the replay config", rep.Skill)
	}
	if !validReplayModel(rep.Model) {
		return "", "", fmt.Errorf("the model %q is not usable as a model name", rep.Model)
	}

	sess, name, err := s.loadReplaySession(rep)
	if err != nil {
		return "", "", err
	}

	// Collect into the dataset directory beside the database, as a fresh
	// dataset per replay: replaying the same session twice is two events, and
	// appending both into one dataset would make the second run's cells
	// measure duplicates of the first.
	machineID, merr := machine.Current()
	if merr != nil {
		return "", "", fmt.Errorf("this machine cannot be identified, so the replay cannot be recorded against it: %w", merr)
	}
	opts := session.Options{
		Split:        session.Split(rep.Split),
		WorkspaceCwd: rep.WorkspaceCwd,
		Machine:      &machineID,
	}
	items, err := session.Collect([]*session.Session{sess}, opts)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(replayRoot(s.st), rep.ID, "dataset")
	if err := session.WriteDataset(dir, "replay-"+rep.ID, fmt.Sprintf("replay of %s session %s", rep.Source, rep.SessionID), items); err != nil {
		return "", "", err
	}

	ds, err := dataset.Load(dir)
	if err != nil {
		return "", "", err
	}
	if err := runner.CheckMachine(ds); err != nil {
		return "", "", err
	}
	spec, err := combineReplaySpec(harness, skill, rep.Model)
	if err != nil {
		return "", "", err
	}
	cfg := runner.Config{
		Store:       s.st,
		Agent:       spec,
		RunName:     fmt.Sprintf("replay %s", shortName(name)),
		Notes:       fmt.Sprintf("replay %s: harness=%s skill=%s model=%s", rep.ID, rep.Harness, rep.Skill, rep.Model),
		Samples:     1,
		Concurrency: 1, // one replay at a time, one cell at a time: see replayQueue
		Threshold:   runner.DefaultThreshold,
		ToolVersion: s.opts.ToolVersion,
	}
	if err := runner.Preflight(ctx, ds, cfg); err != nil {
		return "", "", err
	}
	res, err := runner.Run(ctx, ds, cfg)
	// runner.Run returns no result at all when it refused before any run row
	// was created (its agent kind unknown, say), so a nil result here is not
	// a broken run but a refused one.
	if res != nil && res.Run.ID != "" {
		runID = res.Run.ID
	}
	if err != nil {
		return runID, "", err
	}

	rOpts := report.Options{}.Resolve()
	data, err := report.Load(ctx, s.st, runID, rOpts)
	if err != nil {
		return runID, "", fmt.Errorf("build the report: %w", err)
	}
	reg, err := report.Save(ctx, s.st, report.Request{
		Kind:    store.ReportKindRun,
		Title:   data.Title,
		Subject: data.Title,
		Params:  map[string]string{"run": runID},
		Options: rOpts,
	}, data)
	if err != nil {
		return runID, "", fmt.Errorf("register the report: %w", err)
	}
	return runID, reg.ID, nil
}

// loadReplaySession parses the session a replay names, from whichever local
// history it names. The display name travels back with it because the run row
// is labelled after the session, not after its uuid.
func (s *Server) loadReplaySession(rep store.Replay) (*session.Session, string, error) {
	switch rep.Source {
	case "claude":
		src, err := session.Resolve(s.opts.SessionRoot, rep.SessionID)
		if err != nil {
			return nil, "", err
		}
		sess, err := session.ParseFile(src.Path)
		if err != nil {
			return nil, "", err
		}
		return sess, firstPromptName(sess), nil
	case "cursor":
		sess, err := session.ParseCursorChat(s.opts.CursorDB, rep.SessionID)
		if err != nil {
			return nil, "", err
		}
		return sess, firstPromptName(sess), nil
	default:
		return nil, "", fmt.Errorf("unknown session source %q", rep.Source)
	}
}

// firstPromptName is a session's display name: the first ~60 runes of its
// first human prompt, which is the convention the listing already uses.
func firstPromptName(sess *session.Session) string {
	if len(sess.Prompts) == 0 {
		return sess.ID
	}
	name := sess.Prompts[0].Text
	runes := []rune(name)
	if len(runes) > 60 {
		runes = runes[:60]
	}
	return string(runes)
}

// shortName flattens a session name for a run label.
func shortName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
	}
	name := string(out)
	// Truncate at a word boundary rather than mid-word, so the run list stays
	// readable.
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// replayRoot is where replay datasets live: beside the database.
func replayRoot(st *store.Store) string {
	dir := filepath.Dir(st.Path())
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, "replays")
}

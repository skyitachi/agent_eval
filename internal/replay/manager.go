package replay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/machine"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// Config shapes a Manager.
type Config struct {
	Store   *store.Store
	Catalog *session.Catalog
	Presets *Presets
	// WorkRoot holds one directory per replay: the dataset it collected and
	// the run's workspaces. Default: "replays" beside the database.
	WorkRoot string
	// Timeout is the per-task budget when the collected task derives none.
	Timeout time.Duration
	// ToolVersion is recorded on the run row.
	ToolVersion string
}

// Manager runs replays one at a time, in the order they were asked for.
//
// One at a time because every replay of a recorded session is a measurement:
// two agents running at once on one machine contend for it, and the busy-time
// numbers a replay is compared against were recorded with the machine to
// itself.
type Manager struct {
	cfg   Config
	queue chan string

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	closed  bool

	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// AbandonedReason is the error written onto a replay a previous process left
// unfinished.
const AbandonedReason = "the server that was running this replay stopped before it finished"

// queueDepth bounds the replays waiting behind the one running. A queue is for
// a person pressing the button a few times, not for a backlog: past this, the
// next press is refused with a sentence rather than accepted and left waiting
// for an hour.
const queueDepth = 16

// NewManager starts the worker. Close stops it.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Store == nil || cfg.Catalog == nil {
		return nil, errors.New("replay: a store and a catalog are required")
	}
	if cfg.Presets == nil {
		cfg.Presets = DefaultPresets()
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = filepath.Join(filepath.Dir(cfg.Store.Path()), "replays")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if _, err := cfg.Store.AbandonReplays(context.Background(), AbandonedReason); err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{
		cfg:     cfg,
		queue:   make(chan string, queueDepth),
		cancels: map[string]context.CancelFunc{},
		ctx:     ctx,
		stop:    stop,
	}
	m.wg.Add(1)
	go m.work()
	return m, nil
}

// Presets are the choices this manager offers.
func (m *Manager) Presets() *Presets { return m.cfg.Presets }

// Close cancels the replay in flight, fails the ones still queued, and waits
// for the worker to finish writing.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	m.stop()
	m.wg.Wait()
	_, _ = m.cfg.Store.AbandonReplays(context.Background(), AbandonedReason)
}

// Request is a replay asked for.
type Request struct {
	Source    string
	SessionID string
	Choice    Choice
}

// ErrBusy is returned when the queue is full.
var ErrBusy = errors.New("replay: the queue is full")

// Submit validates a request, records it, and queues it.
//
// Everything that can be known before the agent runs is checked here, so that
// a mistake is answered on the form rather than discovered a minute later on
// the replay's page: the session exists, the choices are on offer, and the
// model looks like a model id.
func (m *Manager) Submit(ctx context.Context, req Request) (store.Replay, error) {
	entry, err := m.cfg.Catalog.Find(req.Source, req.SessionID)
	if err != nil {
		return store.Replay{}, err
	}
	spec, choice, err := m.cfg.Presets.Resolve(req.Choice)
	if err != nil {
		return store.Replay{}, err
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		return store.Replay{}, err
	}
	id, err := newID()
	if err != nil {
		return store.Replay{}, err
	}
	rp := store.Replay{
		ID:            id,
		Source:        entry.Source,
		SessionID:     entry.ID,
		SessionTitle:  entry.Title,
		Harness:       choice.Harness,
		Skill:         choice.Skill,
		Model:         choice.Model,
		AgentSpecJSON: string(blob),
		Status:        store.ReplayQueued,
		CreatedAt:     time.Now().UTC(),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return store.Replay{}, errors.New("replay: the server is stopping")
	}
	if len(m.queue) >= cap(m.queue) {
		return store.Replay{}, fmt.Errorf("%w: %d replays are already waiting; try again when one finishes",
			ErrBusy, len(m.queue))
	}
	if err := m.cfg.Store.CreateReplay(ctx, rp); err != nil {
		return store.Replay{}, err
	}
	m.queue <- rp.ID
	return rp, nil
}

// Cancel stops a queued or running replay.
//
// The decision is taken under the lock the worker registers under, so a replay
// is either cancelled while queued -- its row is closed and the worker, which
// reads the row after registering, skips it -- or cancelled while running, and
// never closed as cancelled and then run anyway.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, running := m.cancels[id]; running {
		cancel()
		return nil
	}
	rp, err := m.cfg.Store.GetReplay(ctx, id)
	if err != nil {
		return err
	}
	if rp.Finished() {
		return fmt.Errorf("replay %s has already %s", id, rp.Status)
	}
	now := time.Now().UTC()
	rp.Status, rp.EndedAt, rp.Error = store.ReplayFailed, &now, "cancelled before it started"
	return m.cfg.Store.UpdateReplay(ctx, rp)
}

func (m *Manager) work() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case id := <-m.queue:
			m.runOne(id)
		}
	}
}

// job is one replay in flight. Its log is written through to the row so that
// the page shows progress while the run is still going.
type job struct {
	m  *Manager
	rp store.Replay
	mu sync.Mutex
}

func (j *job) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	j.rp.Log += time.Now().Format("15:04:05") + "  " + strings.TrimRight(line, "\n") + "\n"
	_ = j.m.cfg.Store.UpdateReplay(context.Background(), j.rp)
}

func (j *job) save() {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.m.cfg.Store.UpdateReplay(context.Background(), j.rp)
}

func (m *Manager) runOne(id string) {
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()
	// Read after registering, so a Cancel that found nothing registered has
	// already closed the row by the time it is read here.
	rp, err := m.cfg.Store.GetReplay(context.Background(), id)
	if err != nil || rp.Status != store.ReplayQueued {
		return
	}

	now := time.Now().UTC()
	rp.Status, rp.StartedAt = store.ReplayRunning, &now
	j := &job{m: m, rp: rp}
	j.save()

	err = m.execute(ctx, j)

	end := time.Now().UTC()
	j.mu.Lock()
	j.rp.EndedAt = &end
	switch {
	case err == nil:
		j.rp.Status = store.ReplayCompleted
	case errors.Is(err, context.Canceled):
		j.rp.Status, j.rp.Error = store.ReplayFailed, "cancelled"
	default:
		j.rp.Status, j.rp.Error = store.ReplayFailed, err.Error()
	}
	j.mu.Unlock()
	j.save()
}

// execute is the replay: collect, run, register.
func (m *Manager) execute(ctx context.Context, j *job) error {
	rp := j.rp
	var spec agent.Spec
	if err := json.Unmarshal([]byte(rp.AgentSpecJSON), &spec); err != nil {
		return fmt.Errorf("the recorded agent spec does not decode: %w", err)
	}

	entry, err := m.cfg.Catalog.Find(rp.Source, rp.SessionID)
	if err != nil {
		return err
	}
	j.logf("reading %s session %s", rp.Source, rp.SessionID)
	sess, err := m.cfg.Catalog.Load(entry)
	if err != nil {
		return fmt.Errorf("read the session: %w", err)
	}
	if len(sess.Prompts) == 0 {
		return errors.New("the session holds no human prompt, so there is nothing to replay")
	}

	dir := filepath.Join(m.cfg.WorkRoot, rp.ID)
	dsDir := filepath.Join(dir, "dataset")
	j.mu.Lock()
	j.rp.DatasetDir = dsDir
	j.mu.Unlock()

	items, notes, err := collect(sess)
	if err != nil {
		return err
	}
	for _, n := range notes {
		j.logf("note: %s", n)
	}
	name := "replay " + shortID(rp.SessionID)
	if err := session.WriteDataset(dsDir, name, "replay of "+rp.Source+" session "+rp.SessionID, items); err != nil {
		return fmt.Errorf("write the dataset: %w", err)
	}
	ds, err := dataset.Load(dsDir)
	if err != nil {
		return fmt.Errorf("load the collected dataset: %w", err)
	}
	j.logf("collected %d task(s) into %s", len(ds.Tasks), dsDir)

	cfg := runner.Config{
		Store:       m.cfg.Store,
		Agent:       spec,
		RunName:     fmt.Sprintf("%s · %s/%s/%s", rp.SessionTitle, rp.Harness, rp.Skill, orDash(rp.Model)),
		Notes:       fmt.Sprintf("web replay %s of %s session %s", rp.ID, rp.Source, rp.SessionID),
		Samples:     1,
		Concurrency: 1,
		WorkRoot:    filepath.Join(dir, "work"),
		Timeout:     m.cfg.Timeout,
		ToolVersion: m.cfg.ToolVersion,
		// Each task asks about what the previous prompt produced, so the turns
		// share one workspace in the order they were asked.
		ChainSessions: len(ds.Tasks) > 1,
		Progress: func(p runner.Progress) {
			j.logf("[%d/%d] %s  %s  %d tokens  %s", p.Done, p.Total, p.Cell.TaskID, cellState(p.Cell),
				p.Cell.TokenTotal(), (time.Duration(p.Cell.DurationMs) * time.Millisecond).Round(time.Second))
		},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := runner.CheckMachine(ds); err != nil {
		return err
	}
	j.logf("running under harness %s, skills %s, model %s", rp.Harness, rp.Skill, orDash(rp.Model))
	res, err := runner.Run(ctx, ds, cfg)
	if res != nil {
		j.mu.Lock()
		j.rp.RunID = res.Run.ID
		j.mu.Unlock()
	}
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.rp.RunID = res.Run.ID
	j.mu.Unlock()
	j.logf("run %s finished", res.Run.ID)

	opts := report.Options{}.Resolve()
	data, err := report.Load(context.WithoutCancel(ctx), m.cfg.Store, res.Run.ID, opts)
	if err != nil {
		return fmt.Errorf("build the report: %w", err)
	}
	reg, err := report.Save(context.WithoutCancel(ctx), m.cfg.Store, report.Request{
		Kind:    store.ReportKindRun,
		Title:   data.Title,
		Subject: data.Title,
		Params:  map[string]string{"run": res.Run.ID},
		Options: opts,
	}, data)
	if err != nil {
		return fmt.Errorf("register the report: %w", err)
	}
	j.mu.Lock()
	j.rp.ReportID = reg.ID
	j.mu.Unlock()
	j.logf("report %s recorded", reg.ID)
	return nil
}

// collect turns the session into tasks the way `aeval collect --split turn
// --workspace-cwd` would, falling back to an empty workspace when the
// recording's directory cannot be bound.
//
// Per turn, because the button replays the session and not its first prompt.
// Bound to the recording's repository when it can be, because a replay in an
// empty directory of a session that worked on files is measuring a different
// task; when it cannot be (no directory, not a repository) the replay still
// runs, and the reason goes into the log and onto every baseline.
func collect(sess *session.Session) ([]session.Item, []string, error) {
	opts := session.Options{Split: session.SplitTurn, WorkspaceCwd: true}
	if id, err := machine.Current(); err == nil {
		opts.Machine = &id
	}
	items, err := session.Collect([]*session.Session{sess}, opts)
	if err == nil {
		return items, nil, nil
	}
	note := "the recording's directory could not be bound, so the replay starts from an empty workspace: " + err.Error()
	opts.WorkspaceCwd = false
	items, err2 := session.Collect([]*session.Session{sess}, opts)
	if err2 != nil {
		return nil, nil, fmt.Errorf("collect the session: %w", err2)
	}
	return items, []string{note}, nil
}

func cellState(a store.Attempt) string {
	switch {
	case a.Status == store.CellError:
		return "error"
	case a.Status == store.CellTimeout:
		return "timeout"
	case a.Graded && a.Passed:
		return "passed"
	case a.Graded:
		return "failed"
	default:
		return "measured"
	}
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("replay: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

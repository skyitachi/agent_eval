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
	"github.com/skyitachi/agenteval/internal/llm"
	"github.com/skyitachi/agenteval/internal/machine"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// DefaultGrader is the grader a web replay attaches to every collected task
// when the caller does not name one.
//
// A replay is pressed by a person looking at a page, not configured by someone
// with a dataset in front of them: there is no grader[] block to fill in, and a
// replay that reports only cost and time answers half the question. The judge
// is the one correctness criterion that needs no ground truth, so it is the
// default. Set Grader to "none" to keep the old measurable-but-unscorable
// behaviour.
const DefaultGrader = "llm_judge"

// DefaultGraderConfig is the rubric sent to the judge when the caller supplies
// none.
//
// A collected task is usually a coding session bound to its repository, so the
// diff is where the work is and the final answer is often just "done". Judging
// both is what makes the verdict about the change rather than the prose.
func DefaultGraderConfig() map[string]any {
	return map[string]any{
		"input": "both",
		"rubric": "The work is correct if it accomplishes what the task prompt asked for, " +
			"the change is coherent and complete, and it is not a no-op, an unrelated rewrite, " +
			"or a deletion of the tests. Judge the outcome, not the length or style of the reply.",
	}
}

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

	// Grader is the grader attached to every task a replay collects. Empty
	// means DefaultGrader; "none" (or "-"/"off") collects measurable but
	// unscorable tasks, which is how tests stay offline.
	Grader string
	// GraderName labels it in the report. Empty derives the name from the type.
	GraderName string
	// GraderConfig is the grader's config block. Nil under the default grader
	// means DefaultGraderConfig.
	GraderConfig map[string]any
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
	// Resolve the grader once, here, so every path that later inspects the
	// config sees the same choice rather than "empty means judge" in one place
	// and "empty means nothing" in another.
	switch strings.ToLower(strings.TrimSpace(cfg.Grader)) {
	case "", "default":
		cfg.Grader = DefaultGrader
		if cfg.GraderConfig == nil {
			cfg.GraderConfig = DefaultGraderConfig()
		}
	case "none", "-", "off":
		cfg.Grader, cfg.GraderName, cfg.GraderConfig = "", "", nil
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
	// Workspace is where the agent works: WorkspaceAuto (the default),
	// WorkspaceInPlace or WorkspaceCopy.
	Workspace string
}

// Where a replay works.
//
// In place is the default because it is what the recording had in front of it:
// the real tree with its history, ignored files and tools, rather than a copy
// with one commit in it. It is only ever done when the directory can be given
// back -- uncommitted changes are set aside first and re-applied at the end --
// so the default falls back to a copy when it cannot be, and saying "in place"
// explicitly turns that fallback into a refusal.
const (
	WorkspaceAuto    = "auto"
	WorkspaceInPlace = "in-place"
	WorkspaceCopy    = "copy"
)

// normalWorkspace validates a requested workspace mode, with empty meaning
// the default.
func normalWorkspace(w string) (string, error) {
	switch w = strings.TrimSpace(w); w {
	case "":
		return WorkspaceAuto, nil
	case WorkspaceAuto, WorkspaceInPlace, WorkspaceCopy:
		return w, nil
	}
	return "", fmt.Errorf("there is no workspace mode %q (offered: %s, %s, %s)",
		w, WorkspaceAuto, WorkspaceInPlace, WorkspaceCopy)
}

// InPlaceStatus says whether a replay of a session can work in the directory
// the session was recorded in, for the form to show before anything runs.
type InPlaceStatus struct {
	// Repo is the directory, when the session names one.
	Repo string
	// OK is whether a replay can work there and give it back.
	OK bool
	// Reason is why not, when it cannot.
	Reason string
	// Dirty counts the uncommitted paths that would be set aside.
	Dirty int
	// Stranded is set when the reason is an earlier run that left the
	// directory mid-way. No replay starts until it is repaired, a copy
	// included, because the user's work is what is out of place.
	Stranded bool
}

// CheckInPlace answers InPlaceStatus for one session.
func (m *Manager) CheckInPlace(ctx context.Context, sess *session.Session) InPlaceStatus {
	st := InPlaceStatus{Repo: sess.Cwd}
	ds, err := m.inPlaceDataset(sess)
	if err == nil {
		err = checkRefs(ds)
	}
	if err == nil {
		err = runner.Preflight(ctx, ds, m.inPlaceConfig())
	}
	if err != nil {
		st.Reason = err.Error()
		st.Stranded = errors.Is(err, sandbox.ErrStranded)
		return st
	}
	st.OK = true
	if paths, err := sandbox.UncommittedPaths(ctx, sess.Cwd, m.protected()); err == nil {
		st.Dirty = len(paths)
	}
	return st
}

// inPlaceDataset collects the session bound to its directory, without writing
// it anywhere, so that the in-place questions can be asked of it. A session
// that cannot be bound cannot be replayed in place.
func (m *Manager) inPlaceDataset(sess *session.Session) (*dataset.Dataset, error) {
	// No grader: this dataset is built to ask the repository a question, not to
	// produce a verdict. Attaching the run's grader here would spend a judge
	// call for a form field, and none of the checks below look at graders.
	items, notes, err := collect(sess, "", "", nil)
	if err != nil {
		return nil, err
	}
	if len(notes) > 0 {
		return nil, errors.New(strings.Join(notes, "; "))
	}
	ds := &dataset.Dataset{}
	for _, it := range items {
		ds.Tasks = append(ds.Tasks, it.Task)
	}
	return ds, nil
}

// inPlaceConfig is the part of an in-place run's configuration the checks
// depend on.
func (m *Manager) inPlaceConfig() runner.Config {
	return runner.Config{InPlace: true, Stash: true, Protect: m.protected()}
}

// protected are the paths this process writes while a replay runs. A
// repository that holds one of them un-ignored cannot be worked in: setting its
// changes aside would carry the database off mid-run, and putting it back would
// delete it.
func (m *Manager) protected() []string {
	db := m.cfg.Store.Path()
	return []string{db, db + "-wal", db + "-shm", db + "-journal", m.cfg.WorkRoot}
}

// ErrBusy is returned when the queue is full.
var ErrBusy = errors.New("replay: the queue is full")

// ErrNotInPlace is returned when a replay asked to work in place cannot.
var ErrNotInPlace = errors.New("this session cannot be replayed in the directory it was recorded in")

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
	mode, err := normalWorkspace(req.Workspace)
	if err != nil {
		return store.Replay{}, err
	}
	if mode != WorkspaceCopy {
		// Asked here so that a refusal is answered on the form. It is asked
		// again when the replay starts, because the directory can change in
		// between, and that answer is the one the run acts on.
		sess, err := m.cfg.Catalog.Load(entry)
		if err != nil {
			return store.Replay{}, fmt.Errorf("read the session: %w", err)
		}
		st := m.CheckInPlace(ctx, sess)
		switch {
		case st.OK:
		case mode == WorkspaceInPlace:
			return store.Replay{}, fmt.Errorf("%w: %s", ErrNotInPlace, st.Reason)
		case st.Stranded:
			return store.Replay{}, errors.New(st.Reason)
		}
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
		WorkspaceMode: mode,
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

	grader, graderName, graderCfg := m.cfg.Grader, m.cfg.GraderName, m.cfg.GraderConfig
	if grader == DefaultGrader && !JudgeAvailable(graderCfg) {
		// Collecting without a grader leaves the old measurable-but-unscorable
		// behaviour, which is honest; attaching a judge that cannot authenticate
		// would mark every task failed and look like the agent's fault.
		j.logf("no judge credentials (%s or %s); collecting tasks that are measurable but not scorable",
			llm.EnvAPIKey, llm.EnvAuthToken)
		grader, graderName, graderCfg = "", "", nil
	}
	items, notes, err := collect(sess, grader, graderName, graderCfg)
	if err != nil {
		return err
	}
	for _, n := range notes {
		j.logf("note: %s", n)
	}
	if grader != "" {
		j.logf("grading with %s", grader)
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

	inPlace, err := m.decideInPlace(ctx, j, ds, len(notes) > 0)
	if err != nil {
		return err
	}
	if inPlace && spec.Kind == agent.KindClaudeCode {
		// The agent now runs in the recorded directory, so the transcript it
		// would write is filed under that directory beside the recordings, and
		// nothing about its path says it is a replay. Not writing one is what
		// keeps it from being listed, collected and replayed as a recording.
		spec.ExtraArgs = append(spec.ExtraArgs, "--no-session-persistence")
	}

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
			if p.Cell.RestoreNotes != "" {
				j.logf("%s", p.Cell.RestoreNotes)
			}
		},
	}
	if inPlace {
		cfg.InPlace, cfg.Stash, cfg.Protect = true, true, m.protected()
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
		for _, n := range res.InPlaceNotes {
			j.logf("%s", n)
		}
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

// decideInPlace settles where the replay works, records it on the row, and
// says why in the log.
//
// It is asked of the dataset the run will use, after it is written, so the
// answer is about the tasks that will actually run. unbound reports that the
// session could not be tied to its directory at all.
func (m *Manager) decideInPlace(ctx context.Context, j *job, ds *dataset.Dataset, unbound bool) (bool, error) {
	j.mu.Lock()
	asked := j.rp.WorkspaceMode
	j.mu.Unlock()
	if asked == "" {
		asked = WorkspaceAuto
	}
	settle := func(mode string) {
		j.mu.Lock()
		j.rp.WorkspaceMode = mode
		j.mu.Unlock()
		j.save()
	}
	if asked == WorkspaceCopy {
		settle(WorkspaceCopy)
		j.logf("working in a copy, as asked")
		return false, nil
	}

	var why error
	if unbound {
		why = errors.New("the session is not tied to a repository on this machine")
	} else {
		why = checkRefs(ds)
		if why == nil {
			why = runner.Preflight(ctx, ds, m.inPlaceConfig())
		}
	}
	switch {
	case why == nil:
		settle(WorkspaceInPlace)
		repos := runner.InPlaceRepos(ds, m.inPlaceConfig())
		note := "working in place in " + strings.Join(repos, ", ")
		if len(repos) == 1 {
			if paths, err := sandbox.UncommittedPaths(ctx, repos[0], m.protected()); err == nil && len(paths) > 0 {
				note += fmt.Sprintf("; its %d uncommitted path(s) are set aside first and re-applied when the "+
					"replay ends", len(paths))
			}
		}
		j.logf("%s", note)
		return true, nil
	case asked == WorkspaceInPlace:
		return false, fmt.Errorf("%w: %v", ErrNotInPlace, why)
	case errors.Is(why, sandbox.ErrStranded):
		// A copy would run, but the directory would stay where the earlier
		// run left it, with nothing on this page saying so.
		return false, why
	}
	settle(WorkspaceCopy)
	j.logf("working in a copy, because this replay cannot work in place: %v", why)
	return false, nil
}

// checkRefs refuses a dataset whose tasks name no commit or branch: in a copy
// that clones the default branch, but in place there is nothing to check out.
func checkRefs(ds *dataset.Dataset) error {
	for _, t := range ds.Tasks {
		if t.Workspace.Type == dataset.WorkspaceGit && strings.TrimSpace(t.Workspace.Ref) == "" {
			return fmt.Errorf("the recording names neither the commit nor the branch it started from, so "+
				"there is nothing in %s to check out", t.Workspace.Repo)
		}
	}
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
//
// A non-empty grader is attached to every emitted task. The caller passes an
// empty one only when it has decided the replay should be measured and not
// scored -- either because it was asked for that, or because the judge has no
// credentials.
func collect(sess *session.Session, grader, graderName string, graderCfg map[string]any) ([]session.Item, []string, error) {
	opts := session.Options{
		Split:        session.SplitTurn,
		WorkspaceCwd: true,
		// The judge is what turns a replay from a cost report into a verdict.
		Grader:       grader,
		GraderName:   graderName,
		GraderConfig: graderCfg,
	}
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

// JudgeAvailable reports whether the judge has credentials to call a model.
//
// It reads the same environment the grader will, including a per-run override
// of the variable names, so "attached" and "can authenticate" cannot disagree.
//
// Exported because the two places that have to agree are in different
// packages: a replay deciding whether to attach a judge at all, and a server
// that grades a stored cell and would otherwise discover the same fact by
// refusing a button press.
func JudgeAvailable(cfg map[string]any) bool {
	c := llm.Config{}
	if v, ok := cfg["api_key_env"].(string); ok {
		c.APIKeyEnv = v
	}
	if v, ok := cfg["auth_token_env"].(string); ok {
		c.AuthTokenEnv = v
	}
	return llm.New(c).Available()
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

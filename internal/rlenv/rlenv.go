// Package rlenv serves a dataset to a training loop as a set of environments.
//
// # What problem this solves
//
// An agent RL trainer needs three things from an evaluation harness, in a loop
// and at rollout speed:
//
//	task     which task, and what its prompt and starting tree are;
//	act      somewhere to let a policy do its work;
//	reward   a scalar, computed by the same graders the benchmark uses.
//
// `aeval run` answers all three, once, for a whole dataset, and writes a report
// for a person to read. This package answers them per rollout, over HTTP, for a
// program.
//
// # The reward is the benchmark's reward
//
// Every reward this package returns comes from (*engine.Engine).Verify, which is
// the same code path an ordinary evaluated cell goes through: the same guards,
// the same graders, the same threshold, the same reward contract. That is the
// point. A trainer optimising a reward that only approximates the benchmark's
// would be optimising something nobody can report on later, and the two numbers
// would diverge for reasons nobody could reconstruct.
//
// # Every rollout is a run
//
// Rollouts performed through the configured agent are ordinary engine runs and
// land in the same SQLite database as any other run, so a training run can be
// inspected, compared and reported on with the tools that already exist.
// Environments driven step by step by an external harness are persisted the same
// way when they are verified, under a run whose mode is "rl".
//
// # Threat model
//
// This is the one part of the tool that can *act* rather than render, so it is
// the one part that does not default to open:
//
//   - Nothing here is reachable unless the caller asked for it (see web.Config).
//   - The command and file routes are additionally off by default, because they
//     are arbitrary code execution as the harness's user -- inside the sandbox
//     only for the docker backend. Enable them with AllowExec, and prefer the
//     docker backend when you do.
//   - Concurrency, lifetime, exec count and cumulative agent cost are all
//     capped, because a training loop with a bug is a machine that spends money
//     in a tight loop. A cap that trips is reported as a cap that tripped, not
//     as a task failure.
package rlenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/engine"
	"github.com/skyitachi/agenteval/internal/grader"
	"github.com/skyitachi/agenteval/internal/sandbox"
	"github.com/skyitachi/agenteval/internal/storage"
)

// RunModeRL labels a stored run whose cells were driven by an external harness
// rather than by the engine's own agent loop.
//
// It is a fourth value alongside agent/oracle/baseline, because it answers a
// fourth question: "a policy acted here, but not through this harness". Reading
// it as a model score would be as wrong as reading an oracle run as one.
const RunModeRL = engine.RunMode("rl")

// Errors callers distinguish by.
var (
	// ErrNoSuchEnv means the environment id is unknown, expired or closed.
	ErrNoSuchEnv = errors.New("no such environment")
	// ErrEnvVerified means the environment has already been verified. A verdict
	// is terminal: re-verifying would let a policy keep acting after seeing the
	// reward, which is exactly how a verifier gets gamed.
	ErrEnvVerified = errors.New("environment already verified")
	// ErrExecDisabled means the command route was not enabled.
	ErrExecDisabled = errors.New("command execution is disabled")
	// ErrLimit means a configured cap was reached. It is deliberately its own
	// error: a capped rollout is an operational event, not a task outcome, and
	// reporting it as a failed task would corrupt the training signal.
	ErrLimit = errors.New("limit reached")
)

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// Options bounds what a training loop can do to the machine.
type Options struct {
	// MaxEnvs caps how many environments may be open at once. Opening beyond it
	// fails with ErrLimit rather than queueing: a trainer handed a half-open
	// environment it cannot use is worse off than one told to slow down.
	MaxEnvs int
	// TTL is how long an environment may stay open. Expired environments are
	// reaped in the background and their sandboxes destroyed.
	TTL time.Duration
	// MaxExecs caps commands run inside one environment.
	MaxExecs int
	// AllowExec enables the command and file routes. Off by default because they
	// are arbitrary code execution as the harness's user.
	AllowExec bool

	// MaxRollouts caps the number of rollouts this process will perform. Zero
	// means unlimited.
	MaxRollouts int
	// MaxCostUSD caps cumulative reported agent cost. Zero means unlimited.
	MaxCostUSD float64

	// ExecTimeout bounds one command; MaxOutputBytes bounds what is captured
	// from it. Output bounds exist because a training loop that asks for `cat`
	// on a large file would otherwise grow the response without limit.
	ExecTimeout    time.Duration
	MaxOutputBytes int
}

func (o Options) withDefaults() Options {
	if o.MaxEnvs <= 0 {
		o.MaxEnvs = 8
	}
	if o.TTL <= 0 {
		o.TTL = 15 * time.Minute
	}
	if o.MaxExecs <= 0 {
		o.MaxExecs = 200
	}
	if o.ExecTimeout <= 0 {
		o.ExecTimeout = 60 * time.Second
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = 64 << 10
	}
	return o
}

// ---------------------------------------------------------------------------
// Projections
// ---------------------------------------------------------------------------

// TaskInfo describes a task to a caller deciding what to train on.
//
// It deliberately excludes the oracle. An oracle is the answer, and a service
// that hands out answers trains a policy to ask for them; HasOracle is enough
// for a trainer to know whether `aeval audit` can vouch for the task.
type TaskInfo struct {
	ID          string   `json:"id"`
	Category    string   `json:"category,omitempty"`
	Difficulty  string   `json:"difficulty,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Prompt      string   `json:"prompt"`
	PromptChars int      `json:"prompt_chars"`
	// RewardKind is the declared density: "score" (dense, default) or "pass".
	RewardKind string `json:"reward_kind"`
	// HasOracle reports whether the task's health is checkable.
	HasOracle bool `json:"has_oracle"`
	// WorkspaceKind is files, git or empty.
	WorkspaceKind string `json:"workspace_kind,omitempty"`
	// Graders names the configured graders, in order.
	Graders []string `json:"graders,omitempty"`
	// Guarded reports whether the task resets or forbids paths. A trainer that
	// does not know a task is guarded will read its reward as approval of an
	// attempt that rewrote the verifier.
	Guarded bool `json:"guarded"`
}

// GraderOutcome is one grader's verdict, flattened so a trainer that wants to
// shape its own reward around the parts can do so.
type GraderOutcome struct {
	Grader   string  `json:"grader"`
	Type     string  `json:"type"`
	Passed   bool    `json:"passed"`
	Score    float64 `json:"score"`
	Weight   float64 `json:"weight"`
	Required bool    `json:"required"`
	Reason   string  `json:"reason,omitempty"`
	Error    string  `json:"error,omitempty"`
	Duration float64 `json:"duration_ms"`
}

// Result is what a caller gets after asking for a reward.
type Result struct {
	// EnvID is set for a hand-driven environment, empty for a one-shot rollout.
	EnvID  string `json:"env_id,omitempty"`
	TaskID string `json:"task_id"`

	// Reward is the scalar to train against, projected through the task's
	// declared contract. RewardKind names the density that produced it.
	Reward     float64 `json:"reward"`
	RewardKind string  `json:"reward_kind"`
	// Score is the raw weighted grader score and Passed the benchmark's own
	// verdict. Both are reported so a trainer can see what it is not being paid.
	Score  float64 `json:"score"`
	Passed bool    `json:"passed"`

	// Rejected is true when an anti-gaming guard fired. Reward is then 0, and a
	// trainer that wants to distinguish "wrong" from "tampered" has its answer.
	Rejected   bool     `json:"rejected"`
	Violations []string `json:"violations,omitempty"`
	Restored   []string `json:"restored,omitempty"`

	Graders []GraderOutcome `json:"graders,omitempty"`

	// StopReason explains how the attempt ended, in the benchmark's vocabulary.
	StopReason string `json:"stop_reason,omitempty"`
	// Error is set when the harness, not the policy, failed. Such a rollout
	// carries no training signal and must be discarded rather than learned from.
	Error string `json:"error,omitempty"`

	ChangedPaths []string `json:"changed_paths,omitempty"`
	Diff         string   `json:"diff,omitempty"`
	Answer       string   `json:"answer,omitempty"`

	CostUSD      float64 `json:"cost_usd"`
	Turns        int     `json:"turns"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	DurationMs   float64 `json:"duration_ms"`

	// RunID and TaskRunID point at the stored rows, so the evidence behind this
	// reward can be rendered by `aeval report` rather than taken on trust.
	RunID     string `json:"run_id,omitempty"`
	TaskRunID string `json:"task_run_id,omitempty"`
}

// EnvState is an environment's lifecycle position.
type EnvState string

const (
	// EnvOpen accepts commands and files.
	EnvOpen EnvState = "open"
	// EnvVerified has a verdict; it accepts nothing further.
	EnvVerified EnvState = "verified"
	// EnvExpired was reaped by the TTL, or failed during verification.
	EnvExpired EnvState = "expired"
)

// EnvInfo describes one environment.
type EnvInfo struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	State     EnvState  `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	AgeS      float64   `json:"age_s"`
	Execs     int       `json:"execs"`
	MaxExecs  int       `json:"max_execs"`
	// WorkspaceDir is a host path, and is therefore only meaningful for the
	// local backend. A docker-backed environment must be driven through Exec.
	WorkspaceDir string `json:"workspace_dir,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	// Result is present once verified.
	Result *Result `json:"result,omitempty"`
}

// ExecLog is one command an external harness ran, kept as evidence.
type ExecLog struct {
	Seq        int       `json:"seq"`
	Command    string    `json:"command"`
	ExitCode   int       `json:"exit_code"`
	TimedOut   bool      `json:"timed_out,omitempty"`
	Output     string    `json:"output,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	DurationMs float64   `json:"duration_ms"`
	At         time.Time `json:"at"`
}

// Archive is the full evidence bundle for an environment: everything a person
// would need to understand why the reward came out the way it did, without
// re-running anything.
type Archive struct {
	Env    EnvInfo   `json:"env"`
	Result *Result   `json:"result,omitempty"`
	Execs  []ExecLog `json:"execs,omitempty"`
	// Changed is the set of paths differing from the baseline, before the task's
	// output filters, so a harness that touched a file outside its declared
	// output still shows up here.
	Changed []string `json:"changed,omitempty"`
}

// Stats is a snapshot of the manager's budget, for monitoring and for a training
// script that wants to know when to stop.
type Stats struct {
	OpenEnvs    int     `json:"open_envs"`
	MaxEnvs     int     `json:"max_envs"`
	MaxExecs    int     `json:"max_execs"`
	Rollouts    int     `json:"rollouts"`
	MaxRollouts int     `json:"max_rollouts"`
	CostUSD     float64 `json:"cost_usd"`
	MaxCostUSD  float64 `json:"max_cost_usd"`
	ExecEnabled bool    `json:"exec_enabled"`
	TTLSeconds  float64 `json:"ttl_seconds"`
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager owns a dataset's environments.
type Manager struct {
	cfg  engine.Config
	ds   *dataset.Dataset
	opts Options

	byID map[string]dataset.TaskSpec

	mu       sync.Mutex
	envs     map[string]*Env
	rollouts int
	costUSD  float64

	// reap stops the TTL goroutine.
	reap     chan struct{}
	reapDone sync.WaitGroup

	// now is injectable so tests can age an environment without sleeping.
	now func() time.Time
}

// New builds a Manager.
//
// The engine configuration supplies the sandbox backend, the agent spec and the
// grader threshold. Its RunID is overridden per rollout, so a manager can be
// built from the same configuration a batch run uses.
func New(cfg engine.Config, ds *dataset.Dataset, opts Options) (*Manager, error) {
	if ds == nil {
		return nil, errors.New("rlenv: dataset is nil")
	}
	if len(ds.Tasks) == 0 {
		return nil, fmt.Errorf("rlenv: dataset %q has no tasks", ds.Manifest.Name)
	}
	o := opts.withDefaults()

	// The engine is built once, up front: an unknown grader or a typo'd metric
	// name should fail at construction, not on the first rollout of a training
	// job that has already been running for an hour.
	probe := cfg
	probe.RunID = "rlenv-probe"
	probe.Store = nil
	if _, err := engine.New(probe); err != nil {
		return nil, err
	}

	byID := make(map[string]dataset.TaskSpec, len(ds.Tasks))
	for _, t := range ds.Tasks {
		byID[t.ID] = t
	}
	m := &Manager{
		cfg:  cfg,
		ds:   ds,
		opts: o,
		byID: byID,
		envs: map[string]*Env{},
		reap: make(chan struct{}),
		now:  func() time.Time { return time.Now().UTC() },
	}
	m.reapDone.Add(1)
	go m.reapLoop()
	return m, nil
}

// Options returns the resolved options.
func (m *Manager) Options() Options { return m.opts }

// DatasetName is the dataset being served, for banners and logs.
func (m *Manager) DatasetName() string { return m.ds.Manifest.Name }

// Stats snapshots the budget.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		OpenEnvs: m.countOpenLocked(), MaxEnvs: m.opts.MaxEnvs,
		MaxExecs: m.opts.MaxExecs,
		Rollouts: m.rollouts, MaxRollouts: m.opts.MaxRollouts,
		CostUSD: m.costUSD, MaxCostUSD: m.opts.MaxCostUSD,
		ExecEnabled: m.opts.AllowExec,
		TTLSeconds:  m.opts.TTL.Seconds(),
	}
}

// Tasks lists the dataset's tasks as environments.
//
// The order is the dataset's, so a trainer that iterates sequentially sees the
// same curriculum twice running; the prompt is included because a caller almost
// always needs it and a second round trip per rollout is pure latency.
func (m *Manager) Tasks() []TaskInfo {
	out := make([]TaskInfo, 0, len(m.ds.Tasks))
	for _, t := range m.ds.Tasks {
		out = append(out, m.taskInfo(t))
	}
	return out
}

// Task returns one task's projection.
func (m *Manager) Task(id string) (TaskInfo, bool) {
	t, ok := m.byID[id]
	if !ok {
		return TaskInfo{}, false
	}
	return m.taskInfo(t), true
}

func (m *Manager) taskInfo(t dataset.TaskSpec) TaskInfo {
	graders := make([]string, 0, len(t.Graders))
	for _, g := range t.Graders {
		name := g.Name
		if name == "" {
			name = g.Type
		}
		graders = append(graders, name)
	}
	prompt := taskPrompt(t)
	return TaskInfo{
		ID: t.ID, Category: t.Category, Difficulty: t.Difficulty,
		Tags: t.Tags, Prompt: prompt, PromptChars: len(prompt),
		RewardKind:    string(t.Reward.EffectiveType()),
		HasOracle:     t.Oracle != nil,
		WorkspaceKind: string(t.Workspace.Type),
		Graders:       graders,
		Guarded:       len(t.RestorePaths) > 0 || len(t.ForbidChangedPaths) > 0,
	}
}

// ---------------------------------------------------------------------------
// Environments
// ---------------------------------------------------------------------------

// Env is one open environment.
type Env struct {
	id        string
	task      dataset.TaskSpec
	prompt    string
	ws        *engine.Workspace
	createdAt time.Time

	// mu guards everything below. It is held only for bookkeeping: commands run
	// with it released, so one slow command cannot block the whole service.
	mu      sync.Mutex
	state   EnvState
	execs   int
	execLog []ExecLog
	result  *Result
}

// ID returns the environment id.
func (e *Env) ID() string { return e.id }

// Open provisions an environment for a task.
func (m *Manager) Open(ctx context.Context, taskID string) (*Env, error) {
	t, ok := m.byID[taskID]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", ErrNoSuchEnv, taskID)
	}
	m.mu.Lock()
	if m.countOpenLocked() >= m.opts.MaxEnvs {
		open := m.opts.MaxEnvs
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %d environment(s) already open", ErrLimit, open)
	}
	m.mu.Unlock()

	id := "env-" + agent.NewUUID()
	eng, err := m.engineFor(id)
	if err != nil {
		return nil, err
	}
	ws, err := eng.OpenWorkspace(ctx, t, m.ds.Dir, m.workspaceDir(id))
	if err != nil {
		return nil, err
	}
	env := &Env{
		id: id, task: t, prompt: taskPrompt(t), ws: ws,
		createdAt: m.now(), state: EnvOpen,
	}
	m.mu.Lock()
	m.envs[id] = env
	m.mu.Unlock()
	return env, nil
}

// Get returns an environment by id.
func (m *Manager) Get(id string) (*Env, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchEnv, id)
	}
	return e, nil
}

// Info renders an environment's state.
func (m *Manager) Info(e *Env) EnvInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	info := EnvInfo{
		ID: e.id, TaskID: e.task.ID, State: e.state,
		CreatedAt: e.createdAt, AgeS: m.now().Sub(e.createdAt).Seconds(),
		Execs: e.execs, MaxExecs: m.opts.MaxExecs,
		Prompt: e.prompt,
	}
	if m.cfg.Backend.Kind == engine.BackendLocal {
		info.WorkspaceDir = e.ws.Dir
	}
	if e.result != nil {
		r := *e.result
		info.Result = &r
	}
	return info
}

// List renders every environment, oldest first.
func (m *Manager) List() []EnvInfo {
	m.mu.Lock()
	envs := make([]*Env, 0, len(m.envs))
	for _, e := range m.envs {
		envs = append(envs, e)
	}
	m.mu.Unlock()
	sort.Slice(envs, func(i, j int) bool { return envs[i].createdAt.Before(envs[j].createdAt) })
	out := make([]EnvInfo, 0, len(envs))
	for _, e := range envs {
		out = append(out, m.Info(e))
	}
	return out
}

// Exec runs a command inside an environment.
//
// Commands are recorded rather than only executed: a reward is trustworthy only
// if the actions behind it can be read back, and "what did the policy actually
// run" is the first question asked of any surprising rollout.
func (m *Manager) Exec(ctx context.Context, id, command string, timeout time.Duration) (*ExecLog, error) {
	if !m.opts.AllowExec {
		return nil, ErrExecDisabled
	}
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("rlenv: command is empty")
	}
	e, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	switch e.state {
	case EnvOpen:
	case EnvVerified:
		e.mu.Unlock()
		return nil, ErrEnvVerified
	default:
		state := e.state
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: environment is %s", ErrNoSuchEnv, state)
	}
	if e.execs >= m.opts.MaxExecs {
		used := e.execs
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: %d command(s) already run in %s", ErrLimit, used, id)
	}
	e.execs++
	seq := e.execs
	e.mu.Unlock()

	limit := timeout
	if limit <= 0 || limit > m.opts.ExecTimeout {
		limit = m.opts.ExecTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	start := m.now()
	res, runErr := e.ws.Sandbox.Exec(runCtx, command, sandbox.ExecOptions{Timeout: limit})
	log := ExecLog{Seq: seq, Command: command, At: start}
	if res != nil {
		log.ExitCode, log.TimedOut = res.ExitCode, res.TimedOut
		out, truncated := clip(res.CombinedText(), m.opts.MaxOutputBytes)
		log.Output, log.Truncated = out, truncated
	}
	log.DurationMs = float64(m.now().Sub(start).Milliseconds())

	e.mu.Lock()
	e.execLog = append(e.execLog, log)
	e.mu.Unlock()

	if runErr != nil {
		return &log, runErr
	}
	return &log, nil
}

// WriteFile puts a file into an environment's workspace.
//
// It exists alongside Exec because a policy harness with no shell access -- a
// token-level policy, for instance -- still has to be able to submit work.
func (m *Manager) WriteFile(ctx context.Context, id, rel string, data []byte) error {
	if !m.opts.AllowExec {
		return ErrExecDisabled
	}
	if strings.TrimSpace(rel) == "" {
		return errors.New("rlenv: path is empty")
	}
	e, err := m.Get(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	switch state {
	case EnvOpen:
	case EnvVerified:
		return ErrEnvVerified
	default:
		return fmt.Errorf("%w: environment is %s", ErrNoSuchEnv, state)
	}
	return e.ws.Sandbox.WriteFile(rel, data, 0o644)
}

// Verify grades an environment, persists the verdict and destroys its sandbox.
//
// Verification is terminal. Letting a policy act again after seeing the reward
// would make the reward a function of the policy's knowledge of the reward,
// which is how verifiers get gamed; the environment is closed so the only way
// forward is a new one.
func (m *Manager) Verify(ctx context.Context, id string) (*Result, error) {
	e, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.state == EnvVerified {
		res := e.result
		e.mu.Unlock()
		return res, ErrEnvVerified
	}
	if e.state != EnvOpen {
		state := e.state
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: environment is %s", ErrNoSuchEnv, state)
	}
	// Claim the environment before grading: two concurrent verifies would both
	// pass the check above and grade the same tree twice.
	e.state = EnvVerified
	e.mu.Unlock()

	eng, err := m.engineFor(id)
	if err != nil {
		e.mu.Lock()
		e.state = EnvExpired
		e.mu.Unlock()
		_ = e.ws.Close(ctx, true)
		return nil, err
	}
	v := eng.Verify(ctx, e.task, e.ws.Sandbox, e.ws.Baseline, nil, true)
	// The workspace is torn down either way: a verified environment is finished,
	// and a failed one is not something a caller can retry into correctness.
	if cerr := e.ws.Close(ctx, true); cerr != nil {
		v.Warnings = append(v.Warnings, cerr.Error())
	}

	res := resultFromVerdict(e.id, e.task.ID, v)
	m.persist(ctx, res, e, v.Grade)
	res.EnvID = e.id
	e.mu.Lock()
	e.result = res
	e.mu.Unlock()
	return res, nil
}

// Rollout runs the configured agent on a task once and returns the reward.
//
// This is the path for a trainer that owns no harness of its own. It is an
// ordinary engine run over a one-task dataset, which is what makes its result
// land in the database and its reward identical to the benchmark's.
func (m *Manager) Rollout(ctx context.Context, taskID string) (*Result, error) {
	t, ok := m.byID[taskID]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", ErrNoSuchEnv, taskID)
	}
	if err := m.reserveRollout(); err != nil {
		return nil, err
	}

	cfg := m.cfg
	cfg.Mode = engine.RunModeAgent
	cfg.RunID = "rl-" + agent.NewUUID()
	cfg.RunName = "rollout " + taskID
	cfg.Notes = fmt.Sprintf("rlenv rollout of %s", taskID)
	cfg.SamplesPerTask = 1
	cfg.Concurrency = 1
	// Resume and FailFast both defeat the purpose here: a rollout answered from
	// a previous run's evidence is not a rollout, and stopping at the first
	// failure would leave a trainer with nothing to learn from.
	cfg.Resume, cfg.FailFast = false, false

	eng, err := engine.New(cfg)
	if err != nil {
		return nil, err
	}
	sub := *m.ds
	sub.Tasks = []dataset.TaskSpec{t}

	runRes, runErr := eng.Run(ctx, &sub)
	if runErr != nil {
		return nil, runErr
	}
	if runRes == nil {
		return nil, errors.New("rlenv: rollout produced no result")
	}
	if len(runRes.Tasks) == 0 || runRes.Tasks[0] == nil {
		return nil, errors.New("rlenv: rollout produced no cell")
	}
	tr := runRes.Tasks[0]

	m.mu.Lock()
	m.costUSD += tr.CostUSD()
	m.mu.Unlock()

	res := resultFromCell(tr)
	res.RunID = runRes.RunID
	res.TaskRunID = runRes.RunID + "/" + storage.TaskKey(tr.TaskID, tr.SampleIndex)

	if cerr := m.checkCost(); cerr != nil {
		// The rollout is already paid for, so it is returned with the cap noted
		// rather than discarded. The next call is refused instead.
		res.Error = joinMsg(res.Error, cerr.Error())
	}
	return res, nil
}

// Archive renders the full evidence for an environment.
func (m *Manager) Archive(ctx context.Context, id string) (*Archive, error) {
	e, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	arch := &Archive{Env: m.Info(e)}
	e.mu.Lock()
	arch.Execs = append([]ExecLog(nil), e.execLog...)
	arch.Result = e.result
	e.mu.Unlock()

	// The changed set is read from the live baseline where one still exists; a
	// verified environment has already been torn down, and its diff is in the
	// stored result instead.
	if e.ws != nil && e.ws.Baseline != nil && e.ws.Sandbox != nil {
		changed, cerr := e.ws.Baseline.ChangedPaths(ctx, sandbox.DiffOptions{IgnorePaths: grader.HarnessIgnore})
		if cerr == nil {
			arch.Changed = changed
		}
	}
	return arch, nil
}

// Close destroys an environment without grading it.
func (m *Manager) Close(ctx context.Context, id string) error {
	m.mu.Lock()
	e, ok := m.envs[id]
	delete(m.envs, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchEnv, id)
	}
	return m.destroy(ctx, e)
}

// CloseAll destroys every environment and stops the reaper.
func (m *Manager) CloseAll(ctx context.Context) {
	close(m.reap)
	m.reapDone.Wait()

	m.mu.Lock()
	envs := make([]*Env, 0, len(m.envs))
	for _, e := range m.envs {
		envs = append(envs, e)
	}
	m.envs = map[string]*Env{}
	m.mu.Unlock()

	for _, e := range envs {
		_ = m.destroy(ctx, e)
	}
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (m *Manager) countOpenLocked() int {
	n := 0
	for _, e := range m.envs {
		e.mu.Lock()
		if e.state == EnvOpen {
			n++
		}
		e.mu.Unlock()
	}
	return n
}

func (m *Manager) reserveRollout() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.opts.MaxRollouts > 0 && m.rollouts >= m.opts.MaxRollouts {
		return fmt.Errorf("%w: %d rollout(s) already performed", ErrLimit, m.opts.MaxRollouts)
	}
	if m.opts.MaxCostUSD > 0 && m.costUSD >= m.opts.MaxCostUSD {
		return fmt.Errorf("%w: cumulative cost $%.4f reached the $%.4f cap",
			ErrLimit, m.costUSD, m.opts.MaxCostUSD)
	}
	m.rollouts++
	return nil
}

func (m *Manager) checkCost() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.opts.MaxCostUSD > 0 && m.costUSD >= m.opts.MaxCostUSD {
		return fmt.Errorf("%w: cumulative cost $%.4f reached the $%.4f cap",
			ErrLimit, m.costUSD, m.opts.MaxCostUSD)
	}
	return nil
}

func (m *Manager) workspaceDir(id string) string {
	root := m.cfg.WorkRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "agenteval")
	}
	return filepath.Join(root, "rlenv", id)
}

// engineFor builds the engine a rollout or a verification runs with.
//
// A fresh Engine per environment keeps the manager stateless between
// environments; the only shared state is the store, which serialises its own
// writes.
func (m *Manager) engineFor(id string) (*engine.Engine, error) {
	cfg := m.cfg
	cfg.RunID = id
	return engine.New(cfg)
}

func (m *Manager) destroy(ctx context.Context, e *Env) error {
	e.mu.Lock()
	if e.state == EnvOpen {
		e.state = EnvExpired
	}
	e.mu.Unlock()
	if e.ws == nil {
		return nil
	}
	return e.ws.Close(ctx, true)
}

// reapLoop destroys environments that have outlived the TTL.
//
// The interval is a fraction of the TTL because a reaper that ran only once per
// TTL would let an environment live up to twice as long as configured, which is
// how a leaked sandbox becomes a full disk.
func (m *Manager) reapLoop() {
	defer m.reapDone.Done()
	interval := m.opts.TTL / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.reap:
			return
		case <-ticker.C:
			m.reapOnce(context.Background())
		}
	}
}

func (m *Manager) reapOnce(ctx context.Context) {
	cutoff := m.now().Add(-m.opts.TTL)
	m.mu.Lock()
	var expired []*Env
	for id, e := range m.envs {
		if e.createdAt.Before(cutoff) {
			expired = append(expired, e)
			delete(m.envs, id)
		}
	}
	m.mu.Unlock()
	for _, e := range expired {
		_ = m.destroy(ctx, e)
	}
}

// persist stores a verified environment as a run, so its reward is auditable
// with the same tools as any other.
//
// A failure here is deliberately not fatal: the caller already has the verdict,
// and losing a database row must not turn a measured reward into an error the
// trainer would have to discard.
func (m *Manager) persist(ctx context.Context, res *Result, e *Env, grade *grader.Grade) {
	st := m.cfg.Store
	if st == nil {
		return
	}
	runID := "rl-" + agent.NewUUID()
	rec := &storage.RunRecord{
		ID: runID, Name: "rl env " + e.id, Status: storage.StatusCompleted,
		DatasetName: m.ds.Manifest.Name, DatasetVersion: m.ds.Manifest.Version,
		DatasetHash: m.ds.Hash, DatasetPath: m.ds.Dir,
		AgentKind: m.cfg.Agent.Kind, AgentModel: m.cfg.Agent.Model,
		SandboxBackend: string(m.cfg.Backend.Kind), SandboxImage: m.cfg.Backend.Image,
		Mode:           string(RunModeRL),
		SamplesPerTask: 1, Concurrency: 1,
		Notes:       fmt.Sprintf("environment %s driven externally, then verified", e.id),
		ToolVersion: m.cfg.ToolVersion,
	}
	if err := st.CreateRun(ctx, rec); err != nil {
		return
	}
	trRec := &storage.TaskRunRecord{
		ID: runID + "/" + storage.TaskKey(e.task.ID, 0), RunID: runID,
		TaskID: e.task.ID, SampleIndex: 0,
		Category: e.task.Category, Difficulty: e.task.Difficulty, Tags: e.task.Tags,
		Status: storage.TaskStatusCompleted, StopReason: res.StopReason,
		Passed: res.Passed, Score: res.Score, Threshold: m.cfg.GraderThreshold,
		Reward:     res.Reward,
		RewardKind: res.RewardKind,
		Diff:       res.Diff, Answer: res.Answer, Error: res.Error,
		Violations: res.Violations, Restored: res.Restored,
		DurationMs: int64(res.DurationMs),
	}
	if grade != nil {
		// The grader blob is the stored form; a failure to encode it loses
		// detail, not the verdict, so it is not worth refusing the write over.
		if raw, err := json.Marshal(grade.Results); err == nil {
			trRec.GradesJSON = string(raw)
		}
	}
	if err := st.SaveTaskRun(ctx, trRec); err != nil {
		return
	}
	if err := st.FinishRun(ctx, runID, storage.StatusCompleted, "", 0); err != nil {
		return
	}
	_ = st.SetRunTag(ctx, runID, "rl_env", e.id)
	_ = st.SetRunTag(ctx, runID, "rl_task", e.task.ID)
	res.RunID, res.TaskRunID = runID, trRec.ID
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

// resultFromVerdict projects a hand-driven environment's verdict.
func resultFromVerdict(envID, taskID string, v engine.Verdict) *Result {
	r := &Result{
		EnvID: envID, TaskID: taskID,
		Reward: v.Reward, RewardKind: v.RewardKind,
		Score: v.Score(), Passed: v.Passed(),
		Rejected:   len(v.Violations) > 0,
		Violations: v.Violations, Restored: v.Restored,
		// No agent ran, so there is no stop reason and no answer beyond what the
		// harness left in the workspace. Saying "verified" is more honest than
		// borrowing the agent vocabulary for a process the agent was not part of.
		StopReason:   "verified",
		ChangedPaths: v.ChangedPaths,
		Diff:         v.Diff,
		Graders:      gradersOf(v.Grade),
	}
	if len(v.Errors) > 0 {
		r.Error = strings.Join(v.Errors, "; ")
	}
	for _, w := range v.Warnings {
		r.Error = joinMsg(r.Error, w)
	}
	return r
}

// resultFromCell projects an engine cell, which is the rollout path.
func resultFromCell(tr *engine.TaskResult) *Result {
	r := &Result{
		TaskID: tr.TaskID,
		Reward: tr.Reward, RewardKind: tr.RewardKind,
		Score: tr.Score(), Passed: tr.Passed(),
		Rejected:   len(tr.Violations) > 0,
		Violations: tr.Violations, Restored: tr.Restored,
		StopReason: tr.StopReason, Error: tr.Error,
		ChangedPaths: tr.ChangedPaths, Diff: tr.Diff, Answer: tr.Answer,
		DurationMs: float64(tr.Duration().Milliseconds()),
		Graders:    gradersOf(tr.Grade),
	}
	if tr.Attempt != nil {
		r.CostUSD = tr.Attempt.CostUSD()
		// Turns live on the parsed trace, not on the attempt: the attempt is
		// what the CLI returned, the trace is what it means.
		if t := tr.Attempt.Trace; t != nil {
			r.Turns = t.Turns()
		}
		u := tr.Attempt.Usage()
		r.InputTokens, r.OutputTokens = u.InputTokens, u.OutputTokens
	}
	return r
}

func gradersOf(g *grader.Grade) []GraderOutcome {
	if g == nil {
		return nil
	}
	out := make([]GraderOutcome, 0, len(g.Results))
	for _, r := range g.Results {
		out = append(out, GraderOutcome{
			Grader: r.Grader, Type: r.Type, Passed: r.Passed, Score: r.Score,
			Weight: r.Weight, Required: r.Required, Reason: r.Reason, Error: r.Error,
			Duration: float64(r.Duration.Milliseconds()),
		})
	}
	return out
}

func taskPrompt(t dataset.TaskSpec) string {
	if t.ResolvedPrompt != "" {
		return t.ResolvedPrompt
	}
	return t.Prompt
}

// clip bounds captured output, in bytes, and reports whether it did.
func clip(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	return s[:n] + fmt.Sprintf("\n... (%d more bytes)", len(s)-n), true
}

func joinMsg(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + "; " + add
	}
}

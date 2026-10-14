package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/store"
)

// The in-place tests.
//
// This is the one mode that touches a directory the harness does not own, so
// what is tested here is not only that it works but that it cannot destroy
// anything: the repository must still exist afterwards, it must be on the
// branch and commit it started on, and the agent's work must not have been
// committed into it. Each of those is silent when it is wrong -- a run that
// deleted a working tree still writes rows -- so each needs its own assertion.

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false",
		"-c", "user.name=agenteval", "-c", "user.email=eval@agenteval.local"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
	return strings.TrimSpace(string(out))
}

// inPlaceFixture is a repository with two commits and a dataset of one task
// bound to its first commit.
type inPlaceFixture struct {
	repo   string
	first  string
	second string
	ds     *dataset.Dataset
}

func newInPlaceFixture(t *testing.T, tasks ...dataset.TaskSpec) inPlaceFixture {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "--quiet", "--initial-branch=main")
	writeFile(t, filepath.Join(repo, "code.py"), "first\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "--quiet", "-m", "first")
	first := gitRun(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "code.py"), "second\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "--quiet", "-m", "second")
	second := gitRun(t, repo, "rev-parse", "HEAD")

	if len(tasks) == 0 {
		tasks = []dataset.TaskSpec{{ID: "t1", Prompt: "do it"}}
	}
	for i := range tasks {
		tasks[i].Workspace = dataset.Workspace{Type: dataset.WorkspaceGit, Repo: repo, Ref: first}
		if tasks[i].Limits == nil {
			tasks[i].Limits = &dataset.Limits{TimeoutS: 60}
		}
	}
	return inPlaceFixture{
		repo: repo, first: first, second: second,
		ds: &dataset.Dataset{Dir: t.TempDir(), Tasks: tasks},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runInPlace drives one in-place run whose agent runs the given shell command
// in the workspace.
func runInPlace(t *testing.T, f inPlaceFixture, cfg Config) (*Result, error) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg.Store = st
	cfg.Agent = agent.Spec{Kind: agent.KindMock, Bin: cfg.Agent.Bin}
	cfg.InPlace = true
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 1
	}
	if cfg.Samples == 0 {
		cfg.Samples = 1
	}
	return Run(context.Background(), f.ds, cfg)
}

// ---------------------------------------------------------------------------
// Planning and refusals
// ---------------------------------------------------------------------------

func TestInPlacePlansEveryCellInTheTasksOwnRepository(t *testing.T) {
	f := newInPlaceFixture(t,
		dataset.TaskSpec{ID: "a", Prompt: "x"},
		dataset.TaskSpec{ID: "b", Prompt: "y"},
	)
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Samples: 2, InPlace: true})
	cells, err := plan(f.ds, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 4 {
		t.Fatalf("planned %d cells, want 4", len(cells))
	}
	for _, c := range cells {
		if c.dir != f.repo {
			t.Errorf("cell %s got directory %s, want the task's own repository", c.task.ID, c.dir)
		}
	}
}

func TestInPlaceRefusesConcurrencyAboveOne(t *testing.T) {
	// Two cells can name the same directory even when they are different
	// tasks, and nothing in the scheduler keeps them apart.
	err := Config{InPlace: true, Concurrency: 2}.Validate()
	if err == nil {
		t.Fatal("in-place with concurrency 2 must be refused")
	}
	if !strings.Contains(err.Error(), "cannot keep 2 cells in flight") {
		t.Errorf("the refusal must state the invariant, got: %v", err)
	}
}

func TestInPlaceRefusesKeepWorkspaces(t *testing.T) {
	// --keep is about leaving workspaces behind for inspection. This mode
	// creates none, and a reader who thought --keep was what protected their
	// repository would be wrong in the direction that costs them a working tree.
	err := Config{InPlace: true, KeepWorkspaces: true}.Validate()
	if err == nil {
		t.Fatal("in-place with --keep must be refused")
	}
	if !strings.Contains(err.Error(), "creates no workspaces") {
		t.Errorf("the refusal must say why, got: %v", err)
	}
}

func TestPreflightRefusesWhatInPlaceCannotHonour(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		ws   dataset.Workspace
		want string
	}{
		{
			name: "no workspace",
			ws:   dataset.Workspace{Type: dataset.WorkspaceEmpty},
			want: "no repository to work in",
		},
		{
			name: "a remote",
			ws:   dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "https://example.com/r.git", Ref: "abc"},
			want: "a remote rather than a directory",
		},
		{
			name: "a relative repository",
			ws:   dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "vendor/r", Ref: "abc"},
			want: "relative to the dataset",
		},
		{
			name: "a subdirectory",
			ws:   dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "/tmp/x", Ref: "abc", Subdir: "src"},
			want: "subdirectory of its repository",
		},
		{
			name: "setup commands",
			ws: dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "/tmp/x", Ref: "abc",
				Setup: []dataset.Command{{Run: "make"}}},
			want: "runs setup commands",
		},
		{
			name: "history that must stay hidden",
			ws: dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "/tmp/x", Ref: "abc",
				KeepGitHistory: &no},
			want: "history must not be readable",
		},
		{
			name: "a directory that is gone",
			ws: dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "/tmp/definitely-not-here", Ref: "abc",
				KeepGitHistory: &yes},
			want: "not there",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := &dataset.Dataset{Dir: t.TempDir(), Tasks: []dataset.TaskSpec{
				{ID: "t1", Prompt: "x", Workspace: tc.ws},
			}}
			err := Preflight(context.Background(), ds, Config{InPlace: true})
			if err == nil {
				t.Fatal("must be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "t1") {
				t.Errorf("the refusal must name the task, got: %v", err)
			}
		})
	}
}

func TestPreflightRefusesBeforeTheRunIsRecorded(t *testing.T) {
	// A run that cannot be carried out must not leave a failed run behind: the
	// list would fill with rows for mistakes that were knowable up front.
	f := newInPlaceFixture(t)
	writeFile(t, filepath.Join(f.repo, "code.py"), "someone's work\n")

	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = Run(context.Background(), f.ds, Config{
		Store: st, Agent: agent.Spec{Kind: agent.KindMock}, InPlace: true, Concurrency: 1,
	})
	if err == nil {
		t.Fatal("a dirty repository must be refused")
	}
	runs, err := st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a refused run left %d row(s) in the store", len(runs))
	}
}

// ---------------------------------------------------------------------------
// The run itself
// ---------------------------------------------------------------------------

func TestAnInPlaceRunLeavesTheRepositoryAsItFoundIt(t *testing.T) {
	// The end-to-end claim, and the one that matters most: whatever the agent
	// did, the directory is still there, still on its branch, still at its
	// commit, and clean.
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "sh -c 'echo agent > code.py; echo new > added.py'"

	if _, err := runInPlace(t, f, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(f.repo); err != nil {
		t.Fatalf("the repository must still exist: %v", err)
	}
	if got := gitRun(t, f.repo, "rev-parse", "HEAD"); got != f.second {
		t.Errorf("HEAD = %s, want %s", got, f.second)
	}
	if got := gitRun(t, f.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := gitRun(t, f.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the tree is not clean: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(f.repo, "code.py")); err != nil || string(got) != "second\n" {
		t.Errorf("code.py = %q (%v), want the original content", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "added.py")); !os.IsNotExist(err) {
		t.Error("a file the agent created must be gone")
	}
}

func TestAnInPlaceRunDoesNotCommitIntoTheRepository(t *testing.T) {
	// The baseline commit a copy-based run creates would be a commit in the
	// user's history, on their branch, containing their staged state. There
	// must be no such thing here: the commit the task names is the baseline.
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "sh -c 'echo agent > code.py'"

	before := gitRun(t, f.repo, "rev-list", "--count", "HEAD")
	res, err := runInPlace(t, f, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := gitRun(t, f.repo, "rev-list", "--count", "HEAD"); got != before {
		t.Errorf("the repository gained commits: %s -> %s", before, got)
	}
	log := gitRun(t, f.repo, "log", "--format=%s")
	if strings.Contains(log, "agenteval baseline") {
		t.Errorf("the harness committed into the repository:\n%s", log)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(res.Attempts))
	}
	// The diff is still measured against the commit the task names.
	if !strings.Contains(res.Attempts[0].Diff, "agent") {
		t.Errorf("the agent's change is not in the diff: %q", res.Attempts[0].Diff)
	}
}

func TestAnInPlaceCellRecordsItsRestoreNotes(t *testing.T) {
	// The report has to be able to say what happened to a directory the harness
	// does not own, and the next run over the dataset will not see this state.
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "sh -c 'echo agent > code.py'"

	res, err := runInPlace(t, f, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	notes := res.Attempts[0].RestoreNotes
	if notes == "" {
		t.Fatal("a restore must be recorded even when it went cleanly: an empty note reads as " +
			"'not restored'")
	}
	if !strings.Contains(notes, "put back on main at") || !strings.Contains(notes, "nothing was left behind") {
		t.Errorf("restore notes = %q", notes)
	}
	if !res.Run.InPlace {
		t.Error("the run row must say it worked in place")
	}
}

func TestAnInPlaceRunSaysSoOnItsRow(t *testing.T) {
	// Two runs whose rows differ only in this were not the same experiment.
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "true"

	res, err := runInPlace(t, f, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Run.InPlace {
		t.Error("the recorded run must say it worked in the task's own repository")
	}
	if res.Attempts[0].WorkspaceDir != f.repo {
		t.Errorf("workspace_dir = %q, want the repository the agent worked in", res.Attempts[0].WorkspaceDir)
	}
}

func TestAChainRestoresOnlyWhenItsLastCellIsDone(t *testing.T) {
	// A chain's later tasks start from what the earlier ones left, so putting
	// the tree back between them would throw away the very work the next prompt
	// is about.
	f := newInPlaceFixture(t,
		dataset.TaskSpec{ID: "p0", Session: "s1", PromptIndex: intPtr(0), Prompt: "first"},
		dataset.TaskSpec{ID: "p1", Session: "s1", PromptIndex: intPtr(1), Prompt: "second"},
	)
	cfg := Config{ChainSessions: true}
	// The first cell leaves a file; the second asserts it is still there.
	cfg.Agent.Bin = "sh -c 'test -f from-first || echo made > from-first'"

	if _, err := runInPlace(t, f, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// After the chain, the repository is back and the file is gone.
	if _, err := os.Stat(filepath.Join(f.repo, "from-first")); !os.IsNotExist(err) {
		t.Error("the chain's repository must be put back when the chain ends")
	}
	if got := gitRun(t, f.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the tree is not clean: %q", got)
	}
}

func TestAnInPlaceRunRefusesARepositoryHeldByAnotherRun(t *testing.T) {
	// Two runs in one repository would check out over each other, and each
	// would put back a state neither of them started from.
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "true"

	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Hold the lock by hand, the way a concurrent run would.
	lockDir := filepath.Join(f.repo, ".git", "agenteval")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := `{"repo":"` + f.repo + `","head":"` + f.second + `","branch":"main","pid":` +
		"1" + `,"started_at":"2026-01-02T03:04:05Z","tool":"aeval"}`
	if err := os.WriteFile(filepath.Join(lockDir, "inplace.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Run(context.Background(), f.ds, Config{
		Store: st, Agent: agent.Spec{Kind: agent.KindMock, Bin: "true"}, InPlace: true, Concurrency: 1,
	})
	if err == nil {
		t.Fatal("a held repository must be refused")
	}
	if !strings.Contains(err.Error(), "update-ref refs/heads/main") {
		t.Errorf("the refusal must carry the recovery commands, got: %v", err)
	}
}

func TestInPlaceReleasesTheLockWhenTheRunEnds(t *testing.T) {
	f := newInPlaceFixture(t)
	cfg := Config{}
	cfg.Agent.Bin = "true"
	if _, err := runInPlace(t, f, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "agenteval", "inplace.lock")); !os.IsNotExist(err) {
		t.Errorf("the lock must be gone once the run is over: %v", err)
	}
}

// TestAHalfRecordedChainSaysWhatWentWrongInPlace covers the reason a resumed
// chain cannot be completed, which is not the same in the two modes.
//
// In a copy the predecessor's workspace went away with its cell. In place the
// repository was put back where it was found, which is the opposite of leaving
// it behind and just as fatal: the remaining tasks would each start from a
// state their predecessor never left, and the rows would describe that state
// rather than the agent.
func TestAHalfRecordedChainSaysWhatWentWrongInPlace(t *testing.T) {
	planned := planFor(t, true, 1, chained("s1-p0", "s1", 0), chained("s1-p1", "s1", 1))

	copied := chainsIntact(planned, planned[1:], Config{})
	if copied == nil || !strings.Contains(copied.Error(), "went away with the cell") {
		t.Errorf("a copy-based chain must say the workspace went away, got: %v", copied)
	}
	inPlace := chainsIntact(planned, planned[1:], Config{InPlace: true})
	if inPlace == nil {
		t.Fatal("a half-recorded chain must be refused in place too")
	}
	if !strings.Contains(inPlace.Error(), "put back where it was found") {
		t.Errorf("an in-place chain must say the repository was put back, got: %v", inPlace)
	}
	if strings.Contains(inPlace.Error(), "went away with the cell") {
		t.Error("the copy-based reason is not true of a repository that was put back")
	}
}

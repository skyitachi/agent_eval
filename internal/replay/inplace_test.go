package replay

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// The in-place replay tests drive the fixture session against a real
// repository: the transcript's recorded directory is rewritten to point at it.

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false",
		"-c", "user.name=agenteval", "-c", "user.email=eval@agenteval.local"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

// boundRepo is a repository on main with one commit.
func boundRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("def sub(a, b): return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "--quiet", "-m", "first")
	return repo
}

// boundManager serves the fixture session recorded in repo, with the database
// and the replay work root outside it.
func boundManager(t *testing.T, repo string) (*Manager, *store.Store) {
	t.Helper()
	root := t.TempDir()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(string(body), `"cwd":"/work/proj"`, `"cwd":"`+repo+`"`))
	dir := filepath.Join(root, "-bound")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := NewManager(Config{
		Store: st, Catalog: &session.Catalog{ClaudeRoot: root}, Presets: mockPresets(t),
		WorkRoot: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, st
}

func submit(t *testing.T, m *Manager, workspace string) (store.Replay, error) {
	t.Helper()
	return m.Submit(context.Background(), Request{Source: session.CatalogClaude, SessionID: sessionID,
		Choice: Choice{Harness: "scripted", Skill: "none"}, Workspace: workspace})
}

func TestAReplayWorksInPlaceByDefaultAndGivesUncommittedWorkBack(t *testing.T) {
	repo := boundRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := git(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
	m, st := boundManager(t, repo)

	rp, err := submit(t, m, "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitFinished(t, st, rp.ID)
	if done.Status != store.ReplayCompleted {
		t.Fatalf("replay = %s: %s\n%s", done.Status, done.Error, done.Log)
	}
	if done.WorkspaceMode != WorkspaceInPlace {
		t.Errorf("workspace = %q, want in-place\n%s", done.WorkspaceMode, done.Log)
	}
	run, err := st.GetRun(context.Background(), done.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.InPlace {
		t.Error("the run row must say it worked in place")
	}
	for _, want := range []string{"working in place in " + repo, "2 uncommitted path(s) are set aside",
		"re-applied the 2 uncommitted path(s)"} {
		if !strings.Contains(done.Log, want) {
			t.Errorf("the log should say %q:\n%s", want, done.Log)
		}
	}
	if got := git(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Errorf("status = %q, want the user's work back: %q", got, before)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "calc.py")); string(got) != "work in progress\n" {
		t.Errorf("calc.py = %q", got)
	}
}

func TestAutoFallsBackToACopyWhenInPlaceIsImpossible(t *testing.T) {
	// The fixture's recorded directory does not exist on this machine.
	m, st := newManager(t)
	rp, err := submit(t, m, WorkspaceAuto)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitFinished(t, st, rp.ID)
	if done.Status != store.ReplayCompleted {
		t.Fatalf("replay = %s: %s\n%s", done.Status, done.Error, done.Log)
	}
	if done.WorkspaceMode != WorkspaceCopy {
		t.Errorf("workspace = %q, want copy", done.WorkspaceMode)
	}
	if !strings.Contains(done.Log, "working in a copy, because this replay cannot work in place") {
		t.Errorf("the log must say why it fell back:\n%s", done.Log)
	}
}

func TestInPlaceOnlyIsRefusedOnTheFormWhenItCannotBeDone(t *testing.T) {
	m, st := newManager(t)
	_, err := submit(t, m, WorkspaceInPlace)
	if !errors.Is(err, ErrNotInPlace) {
		t.Fatalf("Submit = %v, want ErrNotInPlace", err)
	}
	if list, _ := st.ListReplays(context.Background(), store.ReplayFilter{}); len(list) != 0 {
		t.Errorf("a refused replay must leave no row: %+v", list)
	}
}

func TestInPlaceOnlyIsDecidedAgainWhenTheReplayStarts(t *testing.T) {
	// The form's answer can go stale before the worker picks the replay up, so
	// the run asks again, and an in-place-only replay that can no longer work
	// in place fails rather than falling back to a copy.
	m, st := newManager(t)
	j := &job{m: m, rp: store.Replay{ID: "decide", WorkspaceMode: WorkspaceInPlace}}
	if err := st.CreateReplay(context.Background(), j.rp); err != nil {
		t.Fatal(err)
	}
	if _, err := m.decideInPlace(context.Background(), j, nil, true); !errors.Is(err, ErrNotInPlace) {
		t.Errorf("an in-place-only replay that cannot be bound must fail, got: %v", err)
	}
}

func TestCopyIsHonouredEvenWhenInPlaceWouldWork(t *testing.T) {
	repo := boundRepo(t)
	m, st := boundManager(t, repo)
	rp, err := submit(t, m, WorkspaceCopy)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitFinished(t, st, rp.ID)
	if done.Status != store.ReplayCompleted || done.WorkspaceMode != WorkspaceCopy {
		t.Fatalf("replay = %s/%s: %s", done.Status, done.WorkspaceMode, done.Error)
	}
	run, _ := st.GetRun(context.Background(), done.RunID)
	if run.InPlace {
		t.Error("a copy replay must not work in place")
	}
}

func TestAStrandedDirectoryRefusesEvenACopy(t *testing.T) {
	// Work an earlier run set aside and never re-applied is the user's, on no
	// branch. A copy would run and leave it there with nothing saying so.
	repo := boundRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "calc.py"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "stash", "push", "--quiet")
	git(t, repo, "update-ref", "refs/agenteval/stash", git(t, repo, "rev-parse", "refs/stash"))
	m, _ := boundManager(t, repo)

	if _, err := submit(t, m, WorkspaceAuto); err == nil || !strings.Contains(err.Error(), "did not re-apply") {
		t.Errorf("auto must refuse a stranded directory, got: %v", err)
	}
	if _, err := submit(t, m, WorkspaceCopy); err != nil {
		t.Errorf("an explicit copy is still allowed, got: %v", err)
	}
}

func TestAnUnknownWorkspaceModeIsRefused(t *testing.T) {
	m, _ := newManager(t)
	if _, err := submit(t, m, "elsewhere"); err == nil || !strings.Contains(err.Error(), "no workspace mode") {
		t.Errorf("got: %v", err)
	}
}

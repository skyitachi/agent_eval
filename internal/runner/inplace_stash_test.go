package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/store"
)

// dirtyFixture leaves the fixture's repository with a staged change, a further
// unstaged change and an untracked file, and returns its status.
func dirtyFixture(t *testing.T, f inPlaceFixture) string {
	t.Helper()
	writeFile(t, filepath.Join(f.repo, "code.py"), "staged\n")
	gitRun(t, f.repo, "add", "code.py")
	writeFile(t, filepath.Join(f.repo, "code.py"), "unstaged\n")
	writeFile(t, filepath.Join(f.repo, "mine.txt"), "mine\n")
	return gitRun(t, f.repo, "status", "--porcelain=v1", "--untracked-files=all")
}

func TestADirtyRepositoryIsRefusedWithoutStash(t *testing.T) {
	f := newInPlaceFixture(t)
	dirtyFixture(t, f)
	cfg := Config{}
	cfg.Agent.Bin = "true"
	if _, err := runInPlace(t, f, cfg); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("a dirty repository must still be refused without Stash, got: %v", err)
	}
}

func TestAStashingRunWorksOnTheRecordedCommitAndGivesTheWorkBack(t *testing.T) {
	f := newInPlaceFixture(t)
	before := dirtyFixture(t, f)
	cfg := Config{Stash: true}
	// The agent must see the recorded commit, not the user's uncommitted work.
	cfg.Agent.Bin = "sh -c 'test \"$(cat code.py)\" = first && test ! -e mine.txt && echo agent > code.py'"

	res, err := runInPlace(t, f, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Attempts[0].Status != store.CellOK || !strings.Contains(res.Attempts[0].Diff, "agent") {
		t.Fatalf("the agent did not run on the recorded commit: %+v", res.Attempts[0])
	}
	if got := gitRun(t, f.repo, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Errorf("status = %q, want the uncommitted work back: %q", got, before)
	}
	if got := gitRun(t, f.repo, "rev-parse", "HEAD"); got != f.second {
		t.Errorf("HEAD = %s, want %s", got, f.second)
	}
	if len(res.InPlaceNotes) != 1 || !strings.Contains(res.InPlaceNotes[0], "re-applied the 2") {
		t.Errorf("in-place notes = %q", res.InPlaceNotes)
	}
	if got := gitRun(t, f.repo, "stash", "list"); got != "" {
		t.Errorf("the stash list must be as it was: %q", got)
	}
}

func TestACancelledChainStillGivesTheRepositoryBack(t *testing.T) {
	// Cancelled after the chain's first cell, the chain's own restore never
	// runs. The run must still put the tree back and re-apply the work.
	f := newInPlaceFixture(t,
		dataset.TaskSpec{ID: "p0", Session: "s1", PromptIndex: intPtr(0), Prompt: "first"},
		dataset.TaskSpec{ID: "p1", Session: "s1", PromptIndex: intPtr(1), Prompt: "second"},
	)
	before := dirtyFixture(t, f)
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = Run(ctx, f.ds, Config{
		Store: st, InPlace: true, Stash: true, Concurrency: 1, ChainSessions: true,
		Agent: agent.Spec{Kind: agent.KindMock, Bin: "sh -c 'echo agent > code.py; echo x > agent.txt'"},
		Progress: func(p Progress) {
			if p.Done == 1 {
				cancel()
			}
		},
	})
	if err == nil {
		t.Fatal("a cancelled run must say so")
	}
	runs, _ := st.ListRuns(context.Background(), store.RunFilter{})
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if cells, _ := st.ListAttempts(context.Background(), runs[0].ID); len(cells) != 1 {
		t.Fatalf("cells = %d, want the run stopped after the chain's first", len(cells))
	}
	if got := gitRun(t, f.repo, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Errorf("status = %q, want %q", got, before)
	}
	if got := gitRun(t, f.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "agent.txt")); !os.IsNotExist(err) {
		t.Error("the agent's file must be gone")
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "agenteval", "inplace.lock")); !os.IsNotExist(err) {
		t.Error("the lock must be released")
	}
}

func TestStashWithoutInPlaceIsAConfigurationError(t *testing.T) {
	if err := (Config{Stash: true, Concurrency: 1}).Validate(); err == nil {
		t.Error("Stash without InPlace must be refused")
	}
}

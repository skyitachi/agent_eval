package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stash tests.
//
// A stashing run takes a repository with uncommitted work in it, so the claim
// under test is that the work comes back exactly: staged and unstaged changes
// to the same file, untracked files, and nothing of the agent's mixed in. Each
// way that can fail is silent -- a stash that lost the index still leaves the
// file contents right -- so each has its own assertion.

// dirty gives r a staged change, a further unstaged change to the same file, and
// an untracked file, which is the combination a careless stash loses track of.
func (r inPlaceRepo) dirty(t *testing.T) string {
	t.Helper()
	write(t, r.dir, "code.py", "staged\n")
	gitOK(t, r.dir, "add", "code.py")
	write(t, r.dir, "code.py", "unstaged\n")
	write(t, r.dir, "notes/todo.txt", "mine\n")
	return gitOK(t, r.dir, "status", "--porcelain=v1", "--untracked-files=all")
}

func (r inPlaceRepo) acquireStash(t *testing.T, opts InPlaceOptions) *InPlace {
	t.Helper()
	opts.Stash = true
	ip, err := AcquireInPlaceWith(context.Background(), r.dir, opts)
	if err != nil {
		t.Fatalf("AcquireInPlaceWith: %v", err)
	}
	t.Cleanup(func() { _ = ip.Release() })
	return ip
}

func TestAStashingRunGivesTheUncommittedWorkBackExactly(t *testing.T) {
	r := newInPlaceRepo(t)
	before := r.dirty(t)
	ctx := context.Background()

	ip := r.acquireStash(t, InPlaceOptions{})
	if ip.Stashed() != 2 {
		t.Errorf("stashed %d path(s), want 2", ip.Stashed())
	}
	// Set aside, so the checkout sees a clean tree, and off the stash list,
	// where the agent's own `git stash pop` would find it.
	if got := r.status(t); got != "" {
		t.Fatalf("the tree must be clean once the work is set aside: %q", got)
	}
	if got := gitOK(t, r.dir, "stash", "list"); got != "" {
		t.Errorf("the work must not be on the stash list: %q", got)
	}
	if gitOK(t, r.dir, "rev-parse", "--verify", StashRef) == "" {
		t.Error("the work must be held under " + StashRef)
	}

	// The agent works, and the chain's restore puts the tree back.
	if err := ip.Checkout(ctx, r.first); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	write(t, r.dir, "code.py", "agent\n")
	write(t, r.dir, "agent.txt", "agent\n")
	if _, err := ip.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	note, err := ip.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !strings.Contains(note, "re-applied the 2 uncommitted path(s)") {
		t.Errorf("Finish note = %q", note)
	}
	if got := gitOK(t, r.dir, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Errorf("status after = %q, want what it was before the run: %q", got, before)
	}
	if got := read(t, r.dir, "code.py"); got != "unstaged\n" {
		t.Errorf("worktree code.py = %q, want the unstaged change", got)
	}
	if got := gitOK(t, r.dir, "show", ":code.py"); got != "staged" {
		t.Errorf("index code.py = %q, want the staged change", got)
	}
	if got := read(t, r.dir, "notes/todo.txt"); got != "mine\n" {
		t.Errorf("untracked file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "agent.txt")); !os.IsNotExist(err) {
		t.Error("what the agent created must not come back with the user's work")
	}
	if _, err := runHost(ctx, r.dir, "rev-parse", "--verify", "--quiet", StashRef); err == nil {
		t.Error(StashRef + " must be dropped once the work is back")
	}
	if got := r.branch(t); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
}

func TestFinishPutsBackAChainTheRunLeftMidWay(t *testing.T) {
	// A run cancelled between two cells of a chain never reaches the chain's
	// restore, and the work set aside can only come back onto a tree that has
	// been put back.
	r := newInPlaceRepo(t)
	before := r.dirty(t)
	ctx := context.Background()
	ip := r.acquireStash(t, InPlaceOptions{})
	if err := ip.Checkout(ctx, r.first); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	write(t, r.dir, "code.py", "agent\n")

	note, err := ip.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !strings.Contains(note, "put back on main") || !strings.Contains(note, "re-applied") {
		t.Errorf("Finish note = %q", note)
	}
	if got := r.head(t); got != r.second {
		t.Errorf("HEAD = %s, want %s", got, r.second)
	}
	if got := gitOK(t, r.dir, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
		t.Errorf("status after = %q, want %q", got, before)
	}
	// Once only.
	if again, err := ip.Finish(ctx); again != "" || err != nil {
		t.Errorf("a second Finish = %q, %v; want nothing", again, err)
	}
}

func TestFinishKeepsTheWorkWhenItCannotBeAppliedCleanly(t *testing.T) {
	// A tree that is not clean when the work comes back would mix the two. The
	// work stays in its commit and the error says how to get it.
	r := newInPlaceRepo(t)
	r.dirty(t)
	ctx := context.Background()
	ip := r.acquireStash(t, InPlaceOptions{})
	sha := gitOK(t, r.dir, "rev-parse", StashRef)
	write(t, r.dir, "stray.txt", "someone edited the tree meanwhile\n")

	_, err := ip.Finish(ctx)
	if err == nil {
		t.Fatal("Finish over a dirty tree must fail rather than apply")
	}
	if !strings.Contains(err.Error(), "stash apply --index "+sha) {
		t.Errorf("the error must say how to bring the work back, got: %v", err)
	}
	if got := gitOK(t, r.dir, "rev-parse", StashRef); got != sha {
		t.Errorf("%s = %s, want it kept at %s", StashRef, got, sha)
	}
}

func TestTheLockOfAStashingRunNamesTheStash(t *testing.T) {
	// A process that dies after the stash leaves the user's work on no branch;
	// the lock is the breadcrumb, and the refusal a later run gets must say
	// how to bring it back.
	r := newInPlaceRepo(t)
	r.dirty(t)
	ip := r.acquireStash(t, InPlaceOptions{})
	sha := gitOK(t, r.dir, "rev-parse", StashRef)
	lockPath := filepath.Join(r.dir, ".git", inPlaceLockDir, inPlaceLockName)
	var held inPlaceLock
	if err := json.Unmarshal([]byte(read(t, filepath.Dir(lockPath), inPlaceLockName)), &held); err != nil {
		t.Fatal(err)
	}
	if held.Stash != sha {
		t.Fatalf("lock stash = %q, want %s", held.Stash, sha)
	}

	// Pretend the holder died.
	held.PID = 999999
	if err := writeLock(lockPath, held); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireInPlaceWith(context.Background(), r.dir, InPlaceOptions{Stash: true})
	if err == nil {
		t.Fatal("a repository a dead run left must be refused")
	}
	if !strings.Contains(err.Error(), "stash apply --index "+sha) {
		t.Errorf("the recovery must re-apply the stash, got:\n%v", err)
	}
	if !errors.Is(err, ErrStranded) {
		t.Error("the refusal must be recognisable as a stranded repository")
	}
	_ = ip
}

func TestAStashLeftWithoutALockIsRefused(t *testing.T) {
	r := newInPlaceRepo(t)
	write(t, r.dir, "code.py", "mine\n")
	gitOK(t, r.dir, "stash", "push", "--quiet")
	sha := gitOK(t, r.dir, "rev-parse", "refs/stash")
	gitOK(t, r.dir, "update-ref", StashRef, sha)

	err := CheckInPlaceWith(context.Background(), r.dir, InPlaceOptions{Stash: true})
	if err == nil || !errors.Is(err, ErrStranded) {
		t.Fatalf("a leftover %s must be refused as stranded, got: %v", StashRef, err)
	}
	if !strings.Contains(err.Error(), sha) {
		t.Errorf("the refusal must name the commit holding the work, got: %v", err)
	}
}

func TestAStashingRunRefusesWhatAStashCannotCarry(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, r inPlaceRepo)
		expect string
	}{
		{"a conflicted merge", func(t *testing.T, r inPlaceRepo) {
			gitOK(t, r.dir, "checkout", "--quiet", "-b", "side", r.first)
			write(t, r.dir, "code.py", "side\n")
			gitOK(t, r.dir, "commit", "--quiet", "-am", "side")
			gitOK(t, r.dir, "checkout", "--quiet", "main")
			_, _ = runHost(context.Background(), r.dir, "merge", "side")
		}, "unresolved conflicts"},
		{"an untracked repository", func(t *testing.T, r inPlaceRepo) {
			nested := filepath.Join(r.dir, "vendor")
			gitOK(t, r.dir, "init", "--quiet", nested)
			write(t, nested, "x", "x\n")
		}, "untracked repositories"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newInPlaceRepo(t)
			tc.setup(t, r)
			before := gitOK(t, r.dir, "status", "--porcelain")
			_, err := AcquireInPlaceWith(context.Background(), r.dir, InPlaceOptions{Stash: true})
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("want a refusal about %s, got: %v", tc.expect, err)
			}
			if got := gitOK(t, r.dir, "status", "--porcelain"); got != before {
				t.Errorf("a refusal must leave the repository alone: %q -> %q", before, got)
			}
			if _, err := os.Stat(filepath.Join(r.dir, ".git", inPlaceLockDir, inPlaceLockName)); !os.IsNotExist(err) {
				t.Error("a refusal must not leave a lock behind")
			}
		})
	}
}

func TestProtectedPathsSurviveTheStashAndTheRestore(t *testing.T) {
	// The harness's own database, inside the repository and not ignored, must
	// not be carried off by the stash nor deleted by the restore's clean -- and
	// it must not count as the user's uncommitted work either.
	r := newInPlaceRepo(t)
	write(t, r.dir, "aeval.db", "database\n")
	write(t, r.dir, "replays/r1/dataset/tasks.jsonl", "{}\n")
	write(t, r.dir, "mine.txt", "mine\n")
	protect := []string{filepath.Join(r.dir, "aeval.db"), filepath.Join(r.dir, "replays")}
	ctx := context.Background()

	if paths, err := UncommittedPaths(ctx, r.dir, protect); err != nil || len(paths) != 1 {
		t.Fatalf("uncommitted = %v (%v), want only mine.txt", paths, err)
	}
	ip := r.acquireStash(t, InPlaceOptions{Protect: protect})
	if got := read(t, r.dir, "aeval.db"); got != "database\n" {
		t.Fatalf("the database must not be set aside: %q", got)
	}
	if err := ip.Checkout(ctx, r.first); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	write(t, r.dir, "replays/r1/work.log", "written mid-run\n")
	rep, err := ip.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rep.LeftBehind) != 0 {
		t.Errorf("the harness's own files must not be reported as the agent's: %v", rep.LeftBehind)
	}
	if _, err := ip.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, p := range []string{"aeval.db", "replays/r1/work.log", "mine.txt"} {
		if _, err := os.Stat(filepath.Join(r.dir, p)); err != nil {
			t.Errorf("%s must survive the run: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, ".git", "info", "exclude")); err == nil {
		if strings.Contains(read(t, filepath.Join(r.dir, ".git", "info"), "exclude"), excludeMarker) {
			t.Error(".git/info/exclude must be put back as it was")
		}
	}
}

func TestATrackedProtectedPathIsRefused(t *testing.T) {
	r := newInPlaceRepo(t)
	write(t, r.dir, "aeval.db", "tracked\n")
	gitOK(t, r.dir, "add", "aeval.db")
	gitOK(t, r.dir, "commit", "--quiet", "-m", "db")
	err := CheckInPlaceWith(context.Background(), r.dir,
		InPlaceOptions{Stash: true, Protect: []string{filepath.Join(r.dir, "aeval.db")}})
	if err == nil || !strings.Contains(err.Error(), "is tracked") {
		t.Fatalf("a tracked database must be refused, got: %v", err)
	}
}

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// gitOK runs git in a test repository and fails the test if it does not work.
// The environment is the package's own sanitised one, so a developer's global
// configuration cannot decide whether these tests pass.
func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runHost(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
	return strings.TrimSpace(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// inPlaceRepo is a repository on a branch with two commits, which is the
// smallest thing that can tell "the recorded commit" apart from "the branch
// tip".
type inPlaceRepo struct {
	dir    string
	first  string
	second string
}

func newInPlaceRepo(t *testing.T) inPlaceRepo {
	t.Helper()
	dir := t.TempDir()
	gitOK(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "code.py", "first\n")
	gitOK(t, dir, "add", "-A")
	gitOK(t, dir, "commit", "--quiet", "-m", "first")
	first := gitOK(t, dir, "rev-parse", "HEAD")
	write(t, dir, "code.py", "second\n")
	gitOK(t, dir, "add", "-A")
	gitOK(t, dir, "commit", "--quiet", "-m", "second")
	second := gitOK(t, dir, "rev-parse", "HEAD")
	return inPlaceRepo{dir: dir, first: first, second: second}
}

func (r inPlaceRepo) head(t *testing.T) string {
	t.Helper()
	return gitOK(t, r.dir, "rev-parse", "HEAD")
}

func (r inPlaceRepo) branch(t *testing.T) string {
	t.Helper()
	return gitOK(t, r.dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func (r inPlaceRepo) status(t *testing.T) string {
	t.Helper()
	return gitOK(t, r.dir, "status", "--porcelain")
}

// acquire is the ordinary first move of every test here: take the repository,
// and undo it when the test ends so a failure cannot leave a lock behind for
// the next test to trip over.
func (r inPlaceRepo) acquire(t *testing.T) *InPlace {
	t.Helper()
	ip, err := AcquireInPlace(context.Background(), r.dir)
	if err != nil {
		t.Fatalf("AcquireInPlace: %v", err)
	}
	t.Cleanup(func() { _ = ip.Release() })
	return ip
}

// ---------------------------------------------------------------------------
// Checkout
// ---------------------------------------------------------------------------

func TestInPlaceChecksOutTheRecordedCommit(t *testing.T) {
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if got := r.head(t); got != r.first {
		t.Errorf("HEAD = %s, want the recorded commit %s", got, r.first)
	}
	if got := read(t, r.dir, "code.py"); got != "first\n" {
		t.Errorf("the tree is not the recorded commit's: %q", got)
	}
	// Detached on purpose: an agent that commits must not be able to move the
	// user's branch, and the branch is what the restore puts back.
	if got := r.branch(t); got != "HEAD" {
		t.Errorf("HEAD is on %q, want a detached HEAD", got)
	}
}

func TestInPlaceCheckoutRefusesATreeEditedDuringTheRun(t *testing.T) {
	// Something edited the repository between two chains. --force would throw
	// that away silently, and it is not the harness's to throw away.
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	write(t, r.dir, "code.py", "someone else's work\n")
	err := ip.Checkout(context.Background(), r.first)
	if err == nil {
		t.Fatal("checkout over an edited tree must be refused")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("the refusal must say why, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func TestInPlaceRestoresTheBranchAndTheTree(t *testing.T) {
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatal(err)
	}
	// What an agent does: edits a tracked file, creates a new one, deletes
	// another.
	write(t, r.dir, "code.py", "agent was here\n")
	write(t, r.dir, "added.py", "new\n")
	write(t, r.dir, "doomed.py", "x\n")
	gitOK(t, r.dir, "add", "doomed.py")

	rep, err := ip.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := r.branch(t); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := r.head(t); got != r.second {
		t.Errorf("HEAD = %s, want the commit the run started from (%s)", got, r.second)
	}
	if got := read(t, r.dir, "code.py"); got != "second\n" {
		t.Errorf("code.py = %q, want the original content back", got)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "added.py")); !os.IsNotExist(err) {
		t.Error("a file the agent created must be gone")
	}
	if got := r.status(t); got != "" {
		t.Errorf("status = %q, want a clean tree", got)
	}
	if !rep.Clean {
		t.Error("the report must say the tree is clean")
	}
	if !strings.Contains(rep.Notes, "nothing was left behind") {
		t.Errorf("a clean restore still has to say so, got: %q", rep.Notes)
	}
}

func TestInPlaceRestoresABranchTheAgentMoved(t *testing.T) {
	// An agent that re-attaches to the branch and commits moves the user's
	// branch pointer. Checking the branch out afterwards would land on the
	// agent's commit, so the ref has to be repaired first.
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatal(err)
	}
	gitOK(t, r.dir, "checkout", "main")
	write(t, r.dir, "code.py", "agent commit\n")
	gitOK(t, r.dir, "add", "-A")
	gitOK(t, r.dir, "commit", "--quiet", "-m", "agent")

	if _, err := ip.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := gitOK(t, r.dir, "rev-parse", "refs/heads/main"); got != r.second {
		t.Errorf("main = %s, want %s: the branch the agent moved was not put back", got, r.second)
	}
	if got := r.branch(t); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := read(t, r.dir, "code.py"); got != "second\n" {
		t.Errorf("the agent's commit is still in the tree: %q", got)
	}
}

func TestInPlaceRestoreSurvivesACancelledContext(t *testing.T) {
	// The regression this whole file turns on. A restore runs *after* the work
	// stopped -- Ctrl-C, a cell out of budget, --fail-fast -- so the context it
	// is handed is usually already cancelled. If it inherited that, every git
	// command would fail instantly and the repository would be left detached at
	// the recorded commit with the agent's changes in it.
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatal(err)
	}
	write(t, r.dir, "code.py", "agent was here\n")
	write(t, r.dir, "added.py", "new\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := ip.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore with a cancelled context: %v", err)
	}
	if !rep.Clean {
		t.Errorf("the tree was not put back: %q", rep.Notes)
	}
	if got := r.head(t); got != r.second {
		t.Errorf("HEAD = %s, want %s", got, r.second)
	}
	if got := read(t, r.dir, "code.py"); got != "second\n" {
		t.Errorf("code.py = %q, want the original", got)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "added.py")); !os.IsNotExist(err) {
		t.Error("a file the agent created must be gone")
	}
}

func TestInPlaceRemovesANestedRepository(t *testing.T) {
	// -ff, not -f: git refuses to remove a nested repository without it, and an
	// agent that cloned something inside the workspace would leave it behind.
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(r.dir, "vendor", "dep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOK(t, nested, "init", "--quiet")

	if _, err := ip.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, ".git")); !os.IsNotExist(err) {
		t.Error("a repository the agent cloned inside the workspace must be removed")
	}
}

func TestInPlaceLeavesIgnoredFilesAndReportsThem(t *testing.T) {
	// The residue a restore cannot account for. -x would remove these, and
	// would take the user's own .env or node_modules with them; so they stay,
	// and the report says which ones appeared.
	r := newInPlaceRepo(t)
	write(t, r.dir, ".gitignore", "build/\n.env\n")
	gitOK(t, r.dir, "add", "-A")
	gitOK(t, r.dir, "commit", "--quiet", "-m", "ignore")
	write(t, r.dir, ".env", "SECRET=1\n")
	second := gitOK(t, r.dir, "rev-parse", "HEAD")

	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	write(t, r.dir, "build/out.bin", "artifact\n")

	rep, err := ip.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := read(t, r.dir, ".env"); got != "SECRET=1\n" {
		t.Errorf("an ignored file that was already there must survive: %q", got)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "build", "out.bin")); err != nil {
		t.Error("an ignored file the agent created is reported rather than removed")
	}
	if len(rep.LeftBehind) != 1 || rep.LeftBehind[0] != "build/out.bin" {
		t.Errorf("LeftBehind = %v, want the ignored path that appeared", rep.LeftBehind)
	}
	if !strings.Contains(rep.Notes, "build/out.bin") {
		t.Errorf("the report must name what it left behind, got: %q", rep.Notes)
	}
	if !rep.Clean {
		t.Error("an ignored file does not make the worktree dirty")
	}
}

func TestInPlaceRemovesTheHarnessDirectory(t *testing.T) {
	// .agenteval is where the graders write. It is usually gitignored, so the
	// clean skips it and the removal has to be explicit -- otherwise the
	// harness's own files are left in someone's working tree.
	r := newInPlaceRepo(t)
	write(t, r.dir, ".gitignore", ".agenteval/\n")
	gitOK(t, r.dir, "add", "-A")
	gitOK(t, r.dir, "commit", "--quiet", "-m", "ignore harness")
	head := gitOK(t, r.dir, "rev-parse", "HEAD")

	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), head); err != nil {
		t.Fatal(err)
	}
	write(t, r.dir, HarnessDir+"/results.json", "{}\n")

	if _, err := ip.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, HarnessDir)); !os.IsNotExist(err) {
		t.Error("the harness's own directory must not be left in the repository")
	}
}

func TestInPlaceKeepsAHarnessDirectoryThatWasAlreadyThere(t *testing.T) {
	// The other half of the same rule: "remove .agenteval" is only right when
	// the harness created it. A directory that was there before the run is not
	// the harness's to delete.
	r := newInPlaceRepo(t)
	write(t, r.dir, ".gitignore", ".agenteval/\n")
	write(t, r.dir, HarnessDir+"/mine.txt", "keep me\n")
	gitOK(t, r.dir, "add", "-A")
	gitOK(t, r.dir, "commit", "--quiet", "-m", "ignore harness")
	head := gitOK(t, r.dir, "rev-parse", "HEAD")

	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), head); err != nil {
		t.Fatal(err)
	}
	write(t, r.dir, HarnessDir+"/results.json", "{}\n")

	if _, err := ip.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := read(t, r.dir, HarnessDir+"/mine.txt"); got != "keep me\n" {
		t.Errorf("a pre-existing %s must survive: %q", HarnessDir, got)
	}
}

func TestInPlaceReportsRefsTheAgentMoved(t *testing.T) {
	// Refs the agent creates are reported rather than deleted: the harness
	// cannot know which of them the agent meant to keep, and silently removing
	// someone's branch is worse than telling them about it.
	r := newInPlaceRepo(t)
	ip := r.acquire(t)
	if err := ip.Checkout(context.Background(), r.first); err != nil {
		t.Fatal(err)
	}
	gitOK(t, r.dir, "branch", "agent-branch")
	gitOK(t, r.dir, "tag", "agent-tag")

	rep, err := ip.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	joined := strings.Join(rep.MovedRefs, " ")
	if !strings.Contains(joined, "refs/heads/agent-branch") {
		t.Errorf("MovedRefs = %v, want the branch the agent created", rep.MovedRefs)
	}
	if !strings.Contains(joined, "refs/tags/agent-tag") {
		t.Errorf("MovedRefs = %v, want the tag the agent created", rep.MovedRefs)
	}
	if !strings.Contains(rep.Notes, "refs the agent moved") {
		t.Errorf("the report must say the refs were left as they are, got: %q", rep.Notes)
	}
	if got := gitOK(t, r.dir, "rev-parse", "refs/heads/agent-branch"); got == "" {
		t.Error("the branch must still be there")
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestInPlaceRefusesADirtyRepository(t *testing.T) {
	// Uncommitted work is the one thing a restore cannot reconstruct: the
	// recorded commit comes back, the changes on top of it do not.
	cases := []struct {
		name   string
		change func(t *testing.T, dir string)
	}{
		{"modified", func(t *testing.T, dir string) { write(t, dir, "code.py", "edited\n") }},
		{"untracked", func(t *testing.T, dir string) { write(t, dir, "notes.txt", "x\n") }},
		{"staged", func(t *testing.T, dir string) {
			write(t, dir, "code.py", "staged\n")
			gitOK(t, dir, "add", "-A")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newInPlaceRepo(t)
			tc.change(t, r.dir)
			_, err := AcquireInPlace(context.Background(), r.dir)
			if err == nil {
				t.Fatal("a dirty repository must be refused")
			}
			if !strings.Contains(err.Error(), r.dir) {
				t.Errorf("the refusal must name the repository, got: %v", err)
			}
			if !strings.Contains(err.Error(), "uncommitted changes") {
				t.Errorf("the refusal must say what is wrong, got: %v", err)
			}
		})
	}
}

func TestInPlaceRefusesADirectoryThatIsNotARepository(t *testing.T) {
	dir := t.TempDir()
	_, err := AcquireInPlace(context.Background(), dir)
	if err == nil {
		t.Fatal("a directory that is not a repository must be refused")
	}
	if !strings.Contains(err.Error(), "no .git directory") {
		t.Errorf("the refusal must say what is missing, got: %v", err)
	}
}

func TestInPlaceRefusesALinkedWorktree(t *testing.T) {
	// A linked worktree keeps a gitfile where the lock would go, and checking
	// it out would fight the tree it is linked to.
	r := newInPlaceRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	// A branch of its own: git refuses to check the same branch out in two
	// worktrees at once, which is itself the reason a linked worktree is not a
	// directory this run can work in.
	gitOK(t, r.dir, "worktree", "add", "--quiet", "-b", "linked-branch", linked)
	if _, err := AcquireInPlace(context.Background(), linked); err == nil {
		t.Fatal("a linked worktree must be refused")
	}
}

func TestInPlaceRefusesASubmoduleRepository(t *testing.T) {
	// A change inside a submodule is invisible to a diff at the root and is not
	// restored by a checkout, so the run would measure something it cannot see
	// and leave behind something it cannot put back.
	r := newInPlaceRepo(t)
	write(t, r.dir, ".gitmodules", "[submodule \"dep\"]\n\tpath = dep\n\turl = ./dep\n")
	_, err := AcquireInPlace(context.Background(), r.dir)
	if err == nil {
		t.Fatal("a repository with submodules must be refused")
	}
	if !strings.Contains(err.Error(), "submodules") {
		t.Errorf("the refusal must say what is wrong, got: %v", err)
	}
}

func TestInPlaceRefusesABareRepository(t *testing.T) {
	// A bare repository has no worktree, so there is nothing for an agent to
	// work in and nothing to put back.
	dir := t.TempDir()
	gitOK(t, dir, "init", "--quiet", "--bare")
	_, err := AcquireInPlace(context.Background(), dir)
	if err == nil {
		t.Fatal("a bare repository must be refused")
	}
}

// ---------------------------------------------------------------------------
// The lock
// ---------------------------------------------------------------------------

func TestInPlaceRefusesASecondHolderOfTheSameRepository(t *testing.T) {
	r := newInPlaceRepo(t)
	ip := r.acquire(t)

	_, err := AcquireInPlace(context.Background(), r.dir)
	if err == nil {
		t.Fatal("a second run in one repository must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "another run is working in") {
		t.Errorf("the refusal must say the repository is held, got: %v", err)
	}
	if !strings.Contains(msg, r.dir) {
		t.Errorf("the refusal must name the repository, got: %v", err)
	}
	// The recovery instruction is the point of the lock: a reader who loses the
	// process still has to be able to put the directory back.
	for _, want := range []string{"update-ref refs/heads/main", "checkout --force main", "clean -ffd"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must carry the recovery command %q, got:\n%s", want, msg)
		}
	}

	// Releasing is what lets the next run in.
	if err := ip.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := AcquireInPlace(context.Background(), r.dir)
	if err != nil {
		t.Fatalf("a released repository must be usable again: %v", err)
	}
	_ = again.Release()
}

func TestInPlaceRefusesAStaleLockAndPrintsTheRecovery(t *testing.T) {
	// A run that was killed leaves the lock behind. It must not be stolen
	// silently -- the repository may still be checked out at an old commit --
	// but it must be recoverable, and the pid is how the two cases are told
	// apart.
	r := newInPlaceRepo(t)
	lockDir := filepath.Join(r.dir, ".git", inPlaceLockDir)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{"repo":"` + r.dir + `","head":"` + r.second + `","branch":"main",` +
		`"pid":999999,"started_at":"2026-01-02T03:04:05Z","tool":"aeval"}`
	if err := os.WriteFile(filepath.Join(lockDir, inPlaceLockName), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireInPlace(context.Background(), r.dir)
	if err == nil {
		t.Fatal("a stale lock must be refused rather than stolen")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not finish") {
		t.Errorf("the refusal must say the run was interrupted, got: %v", err)
	}
	if !strings.Contains(msg, r.second) {
		t.Errorf("the recovery must name the commit to return to, got:\n%s", msg)
	}
}

func TestInPlaceTakesTheLockOnlyOnceItMeansIt(t *testing.T) {
	// A refusal before the lock is taken must not leave one behind: a lock held
	// by a run that never started is a repository that refuses every later run
	// for a reason that is not true.
	r := newInPlaceRepo(t)
	write(t, r.dir, "code.py", "edited\n")
	if _, err := AcquireInPlace(context.Background(), r.dir); err == nil {
		t.Fatal("a dirty repository must be refused")
	}
	lock := filepath.Join(r.dir, ".git", inPlaceLockDir, inPlaceLockName)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("a refused acquire must not leave a lock behind: %v", err)
	}

	// And the other half: the lock is there for exactly as long as the run
	// holds the repository.
	gitOK(t, r.dir, "checkout", "--", "code.py")
	ip := r.acquire(t)
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the lock must exist while a run holds the repository: %v", err)
	}
	if err := ip.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("Release must remove the lock: %v", err)
	}
}

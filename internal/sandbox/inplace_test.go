package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/startstate"
)

// The inplace-replay tests.
//
// An inplace workspace is the one kind that works in a directory the harness did
// not create, which is what makes it worth its own file: the failure modes here
// are all "something quiet happened to somebody's working tree". A replay that
// starts from a directory that has moved measures the wrong thing and reports it
// as the agent's; a rollback that does not run leaves the agent's files behind;
// a rollback that runs when it should not takes the user's. None of those are
// visible in a score, so each needs an assertion.

func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(startstate.EnvHome, home)
	return home
}

func mustRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runHost(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAt(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// inplaceRepo builds a repository with one commit and a recorded start state for
// it, the way a hook and a collect would between them.
func inplaceRepo(t *testing.T, sessionID string) (dir, ref string, ws dataset.Workspace) {
	t.Helper()
	tempHome(t)
	dir = t.TempDir()
	mustRun(t, dir, "init", "--quiet")
	mustRun(t, dir, "config", "user.email", "test@example.com")
	mustRun(t, dir, "config", "user.name", "test")
	writeAt(t, filepath.Join(dir, "README.md"), "the original line\n")
	writeAt(t, filepath.Join(dir, "notes.md"), "notes from before\n")
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir, "commit", "--quiet", "-m", "the commit the session started from")
	ref = strings.TrimSpace(mustRun(t, dir, "rev-parse", "HEAD"))

	ws = dataset.Workspace{Type: dataset.WorkspaceInplace, Dir: dir, Ref: ref, Session: sessionID}
	return dir, ref, ws
}

// inplaceSandbox roots a sandbox at the directory the workspace names: for this
// workspace type the two have to be the same directory, so an inplace test that
// did not would only be testing the refusal.
func inplaceSandbox(t *testing.T, root string) Sandbox {
	t.Helper()
	sb, err := NewLocal(LocalConfig{Root: root, KeepWorkspace: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close(context.Background()) })
	return sb
}

// recordState captures and saves the start state for a directory, so that a
// non-git replay has a snapshot to verify against.
func recordState(t *testing.T, sessionID, dir string) *startstate.Record {
	t.Helper()
	rec, err := startstate.Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.SessionID = sessionID
	if err := startstate.Save(rec, startstate.Options{}); err != nil {
		t.Fatal(err)
	}
	return rec
}

// ---------------------------------------------------------------------------
// Materialize
// ---------------------------------------------------------------------------

func TestInplaceMaterializeRefusesADirectoryThatHasMoved(t *testing.T) {
	dir, ref, ws := inplaceRepo(t, "sess-moved")

	// The user carried on working after the session ended. Whatever this replay
	// would measure, it would not be the recording's starting point.
	writeAt(t, filepath.Join(dir, "README.md"), "edited after the session\n")
	writeAt(t, filepath.Join(dir, "scratch.txt"), "and something new\n")

	sb := inplaceSandbox(t, dir)
	err := Materialize(context.Background(), sb, ws, ".")
	if err == nil {
		t.Fatal("Materialize ran an inplace replay from a directory that had moved")
	}
	var drift *InplaceDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("error = %v (%T), want an *InplaceDriftError naming what changed", err, err)
	}
	if drift.Dir != dir {
		t.Errorf("the error names %s, want %s", drift.Dir, dir)
	}
	for _, want := range []string{"README.md", "scratch.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s:\n%s", want, err)
		}
	}
	// And it says what to do about it, because "it changed" sends a reader
	// looking while "commit those changes" does not.
	if !strings.Contains(err.Error(), "commit or stash") {
		t.Errorf("the error does not say what to do next:\n%s", err)
	}
	// Nothing was touched on the way to refusing.
	if got := readAt(t, filepath.Join(dir, "README.md")); got != "edited after the session\n" {
		t.Errorf("the refusal changed README.md: %q", got)
	}
	if strings.TrimSpace(mustRun(t, dir, "rev-parse", "HEAD")) != ref {
		t.Error("the refusal moved HEAD")
	}
}

func TestInplaceMaterializeRefusesASandboxRootedSomewhereElse(t *testing.T) {
	dir, _, ws := inplaceRepo(t, "sess-root")

	// A sandbox rooted elsewhere would run the agent outside the recorded
	// directory while the task said otherwise, and every later reading of the
	// result would be wrong.
	sb := inplaceSandbox(t, t.TempDir())
	err := Materialize(context.Background(), sb, ws, ".")
	if err == nil {
		t.Fatal("Materialize accepted a sandbox rooted somewhere other than the workspace")
	}
	if !strings.Contains(err.Error(), "same directory") {
		t.Errorf("error = %v, want it to say the two have to be the same directory", err)
	}
	_ = dir
}

func TestInplaceMaterializeRefusesADirectoryThatIsNotThere(t *testing.T) {
	tempHome(t)
	dir := filepath.Join(t.TempDir(), "not-created")
	ws := dataset.Workspace{Type: dataset.WorkspaceInplace, Dir: dir, Ref: "deadbeef"}

	sb := inplaceSandbox(t, dir)
	if err := Materialize(context.Background(), sb, ws, "."); err == nil {
		t.Fatal("Materialize accepted an inplace workspace that does not exist")
	}
}

func TestInplaceMaterializeAcceptsADirectoryThatStillMatches(t *testing.T) {
	dir, _, ws := inplaceRepo(t, "sess-clean")
	sb := inplaceSandbox(t, dir)

	if err := Materialize(context.Background(), sb, ws, "."); err != nil {
		t.Fatalf("Materialize refused a directory that has not moved: %v", err)
	}
	// Nothing was built: an inplace git replay uses the repository that is
	// already there.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("the repository is gone: %v", err)
	}
}

func TestInplaceMaterializeRefusesAWorkspaceThatNamesNoStartingPoint(t *testing.T) {
	tempHome(t)
	dir := t.TempDir()
	ws := dataset.Workspace{Type: dataset.WorkspaceInplace, Dir: dir}

	sb := inplaceSandbox(t, dir)
	err := Materialize(context.Background(), sb, ws, ".")
	if err == nil {
		t.Fatal("Materialize accepted an inplace workspace with neither ref nor snapshot")
	}
	if !strings.Contains(err.Error(), "neither a ref nor a snapshot") {
		t.Errorf("error = %v, want it to name what is missing", err)
	}
}

// ---------------------------------------------------------------------------
// Baselines
// ---------------------------------------------------------------------------

func TestInplaceBaselinePinsTheRecordedCommitRatherThanHead(t *testing.T) {
	dir, ref, ws := inplaceRepo(t, "sess-pin")
	head := strings.TrimSpace(mustRun(t, dir, "log", "-1", "--format=%H"))
	if head != ref {
		t.Fatalf("the fixture is wrong: HEAD %s, recorded %s", head, ref)
	}

	// Move the repository on. The baseline is still the recorded commit: the
	// point of pinning one is that the reading does not depend on what the
	// directory has been doing since.
	writeAt(t, filepath.Join(dir, "README.md"), "later work\n")
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir, "commit", "--quiet", "-m", "the user carried on")

	base, err := InplaceBaseline(context.Background(), ws)
	if err != nil {
		t.Fatalf("InplaceBaseline: %v", err)
	}
	if base.SHA != ref {
		t.Errorf("baseline = %s, want the recorded commit %s", base.SHA, ref)
	}
	if base.GitDir != "" {
		t.Errorf("GitDir = %q, want it empty: this directory has its own repository", base.GitDir)
	}
	if base.Root != dir {
		t.Errorf("Root = %q, want %q", base.Root, dir)
	}
}

func TestADirectoryWithoutARepositoryGetsABaselineThatIsNotInIt(t *testing.T) {
	home := tempHome(t)
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "notes.md"), "the original notes\n")
	writeAt(t, filepath.Join(dir, "sub/inner.txt"), "nested\n")
	rec := recordState(t, "sess-nogit", dir)

	ws := dataset.Workspace{
		Type:     dataset.WorkspaceInplace,
		Dir:      dir,
		Snapshot: rec.Snapshot,
		Session:  "sess-nogit",
	}
	sb := inplaceSandbox(t, dir)
	if err := Materialize(context.Background(), sb, ws, "."); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// The one thing this workspace type exists to avoid.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("a .git directory was written into the replayed directory")
	}
	gitDir := filepath.Join(home, "baselines", "sess-nogit")
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		t.Fatalf("no baseline repository under the store: %v", err)
	}

	base, err := InplaceBaseline(context.Background(), ws)
	if err != nil {
		t.Fatalf("InplaceBaseline: %v", err)
	}
	if base.GitDir != gitDir {
		t.Errorf("GitDir = %q, want %q", base.GitDir, gitDir)
	}
	if base.SHA == "" {
		t.Error("the baseline names no commit")
	}

	// And the diff it produces is a diff of the directory against its snapshot,
	// read through the repository that lives somewhere else.
	writeAt(t, filepath.Join(dir, "notes.md"), "changed by the attempt\n")
	writeAt(t, filepath.Join(dir, "created.txt"), "new\n")
	changed, err := base.ChangedPaths(context.Background(), DiffOptions{})
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	joined := strings.Join(changed, "\n")
	for _, want := range []string{"notes.md", "created.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the diff does not mention %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "inner.txt") {
		t.Errorf("the diff mentions a file the attempt did not touch:\n%s", joined)
	}
}

func TestADirectoryBaselineIsKeptWhenItStillMatches(t *testing.T) {
	tempHome(t)
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "notes.md"), "the original notes\n")
	rec := recordState(t, "sess-keep", dir)
	ws := dataset.Workspace{
		Type: dataset.WorkspaceInplace, Dir: dir, Snapshot: rec.Snapshot, Session: "sess-keep",
	}

	sb := inplaceSandbox(t, dir)
	ctx := context.Background()
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatal(err)
	}
	first, err := InplaceBaseline(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatal(err)
	}
	again, err := InplaceBaseline(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA != again.SHA {
		t.Errorf("the baseline moved between runs: %s then %s; an attempt measured against "+
			"the first would be reported against the second", first.SHA, again.SHA)
	}
}

func TestADirectoryBaselineFollowsASnapshotThatWasRecordedAgain(t *testing.T) {
	// A session id can be recorded twice -- the hook runs at the start of every
	// session with that id -- and the second recording replaces the snapshot. A
	// baseline left over from the first would have the replay diffing against a
	// starting state it never verified against.
	tempHome(t)
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "notes.md"), "the first recording\n")
	rec := recordState(t, "sess-again", dir)
	ws := dataset.Workspace{
		Type: dataset.WorkspaceInplace, Dir: dir, Snapshot: rec.Snapshot, Session: "sess-again",
	}

	sb := inplaceSandbox(t, dir)
	ctx := context.Background()
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatal(err)
	}
	first, err := InplaceBaseline(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}

	// The directory is recorded again, from a different state.
	writeAt(t, filepath.Join(dir, "notes.md"), "the second recording\n")
	second := recordState(t, "sess-again", dir)
	if second.Snapshot != rec.Snapshot {
		t.Fatalf("the fixture is wrong: %s then %s", rec.Snapshot, second.Snapshot)
	}

	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatal(err)
	}
	third, err := InplaceBaseline(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA == third.SHA {
		t.Error("the baseline still commits the first snapshot, so the replay would measure " +
			"the difference between two recordings as the agent's work")
	}
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestRollbackPutsAGitDirectoryBackWithoutLeavingAnythingInIt(t *testing.T) {
	dir, ref, ws := inplaceRepo(t, "sess-rollback")
	ctx := context.Background()

	// What an attempt might have left: a modified tracked file, a deleted one,
	// an untracked one, and one inside a directory it made.
	writeAt(t, filepath.Join(dir, "README.md"), "changed by the attempt\n")
	if err := os.Remove(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(dir, "created.txt"), "the attempt made this\n")
	writeAt(t, filepath.Join(dir, "nested/deeper/file.txt"), "and this\n")

	if err := RollbackInplace(ctx, ws); err != nil {
		t.Fatalf("RollbackInplace: %v", err)
	}

	if got := strings.TrimSpace(mustRun(t, dir, "rev-parse", "HEAD")); got != ref {
		t.Errorf("HEAD = %s, want the recorded commit %s", got, ref)
	}
	if out := strings.TrimSpace(mustRun(t, dir, "status", "--porcelain")); out != "" {
		t.Errorf("the directory is still dirty after a rollback:\n%s", out)
	}
	if got := readAt(t, filepath.Join(dir, "README.md")); got != "the original line\n" {
		t.Errorf("README.md = %q, want it reverted", got)
	}
	if got := readAt(t, filepath.Join(dir, "notes.md")); got != "notes from before\n" {
		t.Errorf("notes.md = %q, want it restored", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "created.txt")); err == nil {
		t.Error("created.txt is still in the directory")
	}

	// Moved, not deleted: it is the thing the run was measuring.
	for rel, want := range map[string]string{
		"created.txt":            "the attempt made this\n",
		"nested/deeper/file.txt": "and this\n",
	} {
		path, err := startstate.QuarantinePath("sess-rollback", rel)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAt(t, path); got != want {
			t.Errorf("the quarantined %s = %q, want %q", rel, got, want)
		}
	}

	// And the repository itself is left as it was found rather than holding a
	// copy of the attempt: no stash entry, nothing staged.
	if out := strings.TrimSpace(mustRun(t, dir, "stash", "list")); out != "" {
		t.Errorf("the rollback left something in the stash:\n%s", out)
	}
}

func TestRollbackHandlesAnAttemptThatCommittedItsWork(t *testing.T) {
	dir, ref, ws := inplaceRepo(t, "sess-committed")

	// An agent is allowed to commit. Those files are tracked now, so a reset
	// alone would drop them -- which is why they are found before it runs.
	writeAt(t, filepath.Join(dir, "README.md"), "changed by the attempt\n")
	writeAt(t, filepath.Join(dir, "committed.txt"), "the attempt committed this\n")
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir, "commit", "--quiet", "-m", "what the attempt did")

	if err := RollbackInplace(context.Background(), ws); err != nil {
		t.Fatalf("RollbackInplace: %v", err)
	}

	if got := strings.TrimSpace(mustRun(t, dir, "rev-parse", "HEAD")); got != ref {
		t.Errorf("HEAD = %s, want the recorded commit %s", got, ref)
	}
	if out := strings.TrimSpace(mustRun(t, dir, "status", "--porcelain")); out != "" {
		t.Errorf("the directory is still dirty after a rollback:\n%s", out)
	}
	path, err := startstate.QuarantinePath("sess-committed", "committed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, path); got != "the attempt committed this\n" {
		t.Errorf("the file the attempt committed = %q, want it kept aside", got)
	}
}

func TestRollbackLeavesIgnoredFilesAlone(t *testing.T) {
	// A file git is told to ignore is invisible to the cleanliness check that
	// admitted the directory, so there is no way to tell one the attempt created
	// from one that was already there. Moving somebody's build output aside on a
	// guess is worse than leaving it.
	dir, _, ws := inplaceRepo(t, "sess-ignored")
	writeAt(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir, "commit", "--quiet", "-m", "ignore logs")
	ref := strings.TrimSpace(mustRun(t, dir, "rev-parse", "HEAD"))
	ws.Ref = ref

	writeAt(t, filepath.Join(dir, "build.log"), "ignored output\n")
	writeAt(t, filepath.Join(dir, "created.txt"), "not ignored\n")

	if err := RollbackInplace(context.Background(), ws); err != nil {
		t.Fatalf("RollbackInplace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build.log")); err != nil {
		t.Errorf("an ignored file was taken away: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "created.txt")); err == nil {
		t.Error("a file the attempt created was left behind")
	}
}

func TestRollbackOnADirectoryWithNoRepositoryUsesTheSnapshot(t *testing.T) {
	tempHome(t)
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "notes.md"), "the original notes\n")
	rec := recordState(t, "sess-nogit-rollback", dir)

	writeAt(t, filepath.Join(dir, "notes.md"), "changed by the attempt\n")
	writeAt(t, filepath.Join(dir, "created.txt"), "the attempt made this\n")

	ws := dataset.Workspace{
		Type: dataset.WorkspaceInplace, Dir: dir, Snapshot: rec.Snapshot,
		Session: "sess-nogit-rollback",
	}
	if err := RollbackInplace(context.Background(), ws); err != nil {
		t.Fatalf("RollbackInplace: %v", err)
	}

	if got := readAt(t, filepath.Join(dir, "notes.md")); got != "the original notes\n" {
		t.Errorf("notes.md = %q, want it restored from the snapshot", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "created.txt")); err == nil {
		t.Error("created.txt is still in the directory")
	}
	path, err := startstate.QuarantinePath("sess-nogit-rollback", "created.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, path); got != "the attempt made this\n" {
		t.Errorf("the quarantined file = %q, want it kept aside", got)
	}
	// Still not a repository: the rollback must not have made one.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("the rollback turned the directory into a repository")
	}
}

func TestRollbackRefusesAWorkspaceThatNamesNoStartingPoint(t *testing.T) {
	tempHome(t)
	ws := dataset.Workspace{Type: dataset.WorkspaceInplace, Dir: t.TempDir()}
	if err := RollbackInplace(context.Background(), ws); err == nil {
		t.Error("RollbackInplace accepted a workspace with nothing to roll back to")
	}
}

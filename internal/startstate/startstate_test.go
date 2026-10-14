package startstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The start-state tests.
//
// Everything here guards the same property from a different side: a replay must
// be able to tell "this directory is where the recording started" from "this
// directory has moved since". Both answers are quiet -- one runs and produces a
// number, the other should not run at all -- so each case needs an assertion
// rather than a reader's attention.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func useTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	return home
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// gitRepo returns a repository with one commit, and that commit's SHA.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	write(t, filepath.Join(dir, "README.md"), "the original line\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "--quiet", "-m", "the commit the session started from")
	return dir, head(t, dir)
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// captureAndSave runs the two halves of recording a session the way the hook
// does.
func captureAndSave(t *testing.T, sessionID, dir string) *Record {
	t.Helper()
	useTempHome(t)
	rec, err := Capture(dir)
	if err != nil {
		t.Fatalf("Capture(%s): %v", dir, err)
	}
	rec.SessionID = sessionID
	if err := Save(rec, Options{ToolVersion: "test"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return rec
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

func TestCaptureDescribesTheRepositoryItIsGiven(t *testing.T) {
	dir, sha := gitRepo(t)

	rec, err := Capture(dir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if rec.Kind != KindGit {
		t.Errorf("kind = %q, want %q", rec.Kind, KindGit)
	}
	if rec.Commit != sha {
		t.Errorf("commit = %q, want %q", rec.Commit, sha)
	}
	if rec.Branch == "" || rec.Branch == "HEAD" {
		t.Errorf("branch = %q, want the checked-out branch", rec.Branch)
	}
	if len(rec.Dirty) != 0 {
		t.Errorf("dirty = %v, want nothing: the repository was just committed", rec.Dirty)
	}
	if rec.Cwd != dir {
		t.Errorf("cwd = %q, want %q", rec.Cwd, dir)
	}
}

func TestCaptureKeepsTheStatusCodesInItsListOfDirtyFiles(t *testing.T) {
	// The codes are the point. " M README.md" and "?? scratch.txt" are different
	// facts about the starting state -- a file that was edited versus one that
	// was never tracked -- and a bare path list would lose the difference.
	dir, _ := gitRepo(t)
	write(t, filepath.Join(dir, "README.md"), "edited before the session\n")
	write(t, filepath.Join(dir, "scratch.txt"), "never added\n")

	rec, err := Capture(dir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	joined := strings.Join(rec.Dirty, "\n")
	if !strings.Contains(joined, "?? scratch.txt") {
		t.Errorf("dirty = %v, want the untracked file with its status code", rec.Dirty)
	}
	if !strings.Contains(joined, "M README.md") {
		t.Errorf("dirty = %v, want the modified file with its status code", rec.Dirty)
	}
}

func TestCaptureKnowsAPlainDirectoryIsNotARepository(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.md"), "no repository here\n")

	rec, err := Capture(dir)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if rec.Kind != KindDir {
		t.Errorf("kind = %q, want %q: there is no repository above %s", rec.Kind, KindDir, dir)
	}
	if rec.Commit != "" || rec.Branch != "" {
		t.Errorf("commit/branch = %q/%q, want them empty for a directory with no repository",
			rec.Commit, rec.Branch)
	}
}

func TestCaptureRefusesSomethingThatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	write(t, file, "x")
	if _, err := Capture(file); err == nil {
		t.Error("Capture accepted a file")
	}
	if _, err := Capture(filepath.Join(t.TempDir(), "nothing-here")); err == nil {
		t.Error("Capture accepted a path that does not exist")
	}
	if _, err := Capture(""); err == nil {
		t.Error("Capture accepted an empty path")
	}
}

// ---------------------------------------------------------------------------
// Save and Read
// ---------------------------------------------------------------------------

func TestSaveCopiesAPlainDirectoryAndReadFindsItAgain(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.md"), "the original notes\n")
	write(t, filepath.Join(dir, "sub/inner.txt"), "nested\n")

	useTempHome(t)
	rec, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.SessionID = "sess-1"
	saved := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if err := Save(rec, Options{Now: func() time.Time { return saved }, ToolVersion: "v1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if rec.Files != 2 || rec.Bytes != int64(len("the original notes\n")+len("nested\n")) {
		t.Errorf("snapshot counted %d file(s) / %d byte(s), want 2 / %d",
			rec.Files, rec.Bytes, len("the original notes\n")+len("nested\n"))
	}
	if got := read(t, filepath.Join(rec.Snapshot, "sub/inner.txt")); got != "nested\n" {
		t.Errorf("snapshot content = %q", got)
	}

	back, ok, err := Read("sess-1")
	if err != nil || !ok {
		t.Fatalf("Read = (%v, %v, %v), want the record back", back, ok, err)
	}
	if back.Kind != KindDir || back.SessionID != "sess-1" || back.ToolVersion != "v1" {
		t.Errorf("Read returned %+v", back)
	}
	if !back.RecordedAt.Equal(saved) {
		t.Errorf("recorded_at = %s, want %s", back.RecordedAt, saved)
	}
}

func TestReadReportsASessionWithNoRecordAsAnAbsentOne(t *testing.T) {
	// Recording is opt-in, and most sessions collected from an existing
	// transcript never had a hook. That is a normal state, not an error.
	useTempHome(t)
	rec, ok, err := Read("never-recorded")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if ok || rec != nil {
		t.Errorf("Read = (%v, %v), want (nil, false): nobody recorded that session", rec, ok)
	}
	if _, ok, err := Read(""); err != nil || ok {
		t.Errorf("Read(\"\") = (%v, %v), want (nil, false)", ok, err)
	}
}

func TestSaveRefusesARecordItCannotAttribute(t *testing.T) {
	useTempHome(t)
	dir := t.TempDir()
	rec, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(rec, Options{}); err == nil {
		t.Error("Save accepted a record with no session id, so nothing could ever find it")
	}
	if err := Save(nil, Options{}); err == nil {
		t.Error("Save accepted a nil record")
	}
}

func TestASnapshotLeavesOutWhatItCannotHonestlyCopy(t *testing.T) {
	// Three things are deliberately absent, each for its own reason: a .git
	// directory belongs to a repository this record says is not one, a symlink
	// can walk the snapshot out of the tree it describes, and a truncation would
	// read as "the directory had fewer files at the start".
	dir := t.TempDir()
	write(t, filepath.Join(dir, "real.txt"), "contents\n")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, ".git/config"), "not a snapshot's business\n")
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := captureAndSave(t, "sess-snapshot", dir)
	for _, unwanted := range []string{"link.txt", filepath.Join(".git", "config")} {
		if _, err := os.Lstat(filepath.Join(rec.Snapshot, unwanted)); err == nil {
			t.Errorf("the snapshot contains %s", unwanted)
		}
	}
	if got := read(t, filepath.Join(rec.Snapshot, "real.txt")); got != "contents\n" {
		t.Errorf("the snapshot lost real.txt: %q", got)
	}
}

func TestASnapshotStopsAtTheLimitInsteadOfTruncating(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 4096))

	useTempHome(t)
	rec, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.SessionID = "sess-big"
	err = Save(rec, Options{MaxSnapshotBytes: 1024})
	if err == nil {
		t.Fatal("Save accepted a directory past the limit")
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error = %v, want it to name the limit it passed", err)
	}

	// And nothing was left behind that a later Read would take for a record.
	if _, ok, err := Read("sess-big"); err != nil || ok {
		t.Errorf("Read = (%v, %v), want no record: a refused save must not be half-visible", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

func TestVerifyPassesADirectoryThatHasNotMoved(t *testing.T) {
	dir, _ := gitRepo(t)
	rec := captureAndSave(t, "sess-clean", dir)

	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !drift.Clean() {
		t.Errorf("drift = %v (%s), want clean", drift, drift.Error())
	}
	if lines := drift.Lines(); len(lines) != 0 {
		t.Errorf("Lines = %v, want nothing to say about a directory that has not moved", lines)
	}
}

func TestVerifyNamesTheCommitWhenTheDirectoryHasMovedOn(t *testing.T) {
	dir, first := gitRepo(t)
	rec := captureAndSave(t, "sess-moved", dir)

	write(t, filepath.Join(dir, "README.md"), "changed\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "--quiet", "-m", "a later commit")

	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if drift.Clean() {
		t.Fatal("a directory on a later commit was reported as unchanged")
	}
	if drift.Want != first || drift.Head == first {
		t.Errorf("drift wants %s and found %s, want the recorded %s and the new HEAD",
			drift.Want, drift.Head, first)
	}
	lines := strings.Join(drift.Lines(), "\n")
	if !strings.Contains(lines, shortSHA(first)) {
		t.Errorf("the report does not name the recorded commit:\n%s", lines)
	}
	if !strings.Contains(lines, shortSHA(drift.Head)) {
		t.Errorf("the report does not name the commit found:\n%s", lines)
	}
}

func TestVerifyReportsUncommittedChangesByName(t *testing.T) {
	dir, _ := gitRepo(t)
	rec := captureAndSave(t, "sess-dirty", dir)

	write(t, filepath.Join(dir, "README.md"), "edited after the session\n")
	write(t, filepath.Join(dir, "scratch.txt"), "and this is new\n")

	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if drift.Clean() {
		t.Fatal("a directory with uncommitted changes was reported as unchanged")
	}
	lines := strings.Join(drift.Lines(), "\n")
	for _, want := range []string{"README.md", "scratch.txt"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the report does not name %s:\n%s", want, lines)
		}
	}
}

func TestVerifyRefusesAGitRecordThatNamesNoCommit(t *testing.T) {
	// Capture leaves the commit empty when it could not read HEAD. That is "not
	// known", which is a different answer from "the commit you recorded", and it
	// must not be reported as drift: the reader would go looking for a change
	// that never happened.
	dir, _ := gitRepo(t)
	rec := &Record{SessionID: "s", Cwd: dir, Kind: KindGit}

	if _, err := Verify(rec, context.Background()); err == nil {
		t.Fatal("Verify compared against a commit that was never recorded")
	} else if !strings.Contains(err.Error(), "does not name a commit") {
		t.Errorf("error = %v, want it to say the commit is unknown", err)
	}
}

func TestVerifyComparesAPlainDirectoryAgainstItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep.md"), "kept\n")
	write(t, filepath.Join(dir, "gone.md"), "here at the start\n")
	rec := captureAndSave(t, "sess-files", dir)

	// Change one, remove another, add a third.
	write(t, filepath.Join(dir, "keep.md"), "edited\n")
	if err := os.Remove(filepath.Join(dir, "gone.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "added.md"), "new\n")

	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if drift.Clean() {
		t.Fatal("a directory that changed was reported as unchanged")
	}
	if len(drift.Changed) != 1 || drift.Changed[0] != "keep.md" {
		t.Errorf("changed = %v, want [keep.md]", drift.Changed)
	}
	if len(drift.Missing) != 1 || drift.Missing[0] != "gone.md" {
		t.Errorf("missing = %v, want [gone.md]", drift.Missing)
	}
	if len(drift.Added) != 1 || drift.Added[0] != "added.md" {
		t.Errorf("added = %v, want [added.md]", drift.Added)
	}
}

func TestVerifyComparesContentsAndNotTimestamps(t *testing.T) {
	// A rewrite with identical bytes is not a change: an attempt that touched a
	// file and put the same text back would otherwise be reported as drift and
	// the replay would be refused for no reason.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "same.txt"), "identical\n")
	rec := captureAndSave(t, "sess-mtime", dir)

	write(t, filepath.Join(dir, "same.txt"), "identical\n")

	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !drift.Clean() {
		t.Errorf("drift = %v, want clean: the contents did not change", drift)
	}
}

func TestVerifyNamesADirectoryOrSnapshotThatIsGone(t *testing.T) {
	dir := t.TempDir()
	rec := captureAndSave(t, "sess-gone", dir)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(rec, context.Background()); err == nil {
		t.Error("Verify accepted a directory that is not there")
	}

	// And the other half: the directory is there but its snapshot is not.
	dir2 := t.TempDir()
	rec2 := captureAndSave(t, "sess-nosnap", dir2)
	if err := os.RemoveAll(rec2.Snapshot); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(rec2, context.Background())
	if err == nil {
		t.Fatal("Verify accepted a record whose snapshot is gone")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Errorf("error = %v, want it to name the snapshot", err)
	}
}

func TestVerifyRejectsARecordItCannotRead(t *testing.T) {
	if _, err := Verify(nil, context.Background()); err == nil {
		t.Error("Verify accepted a nil record")
	}
	rec := &Record{SessionID: "s", Cwd: t.TempDir(), Kind: Kind("nonsense")}
	if _, err := Verify(rec, context.Background()); err == nil {
		t.Error("Verify accepted a record with an unknown kind")
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func TestRestorePutsAPlainDirectoryBackAndQuarantinesWhatWasAdded(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep.md"), "the original\n")
	write(t, filepath.Join(dir, "deleted.md"), "was here\n")
	rec := captureAndSave(t, "sess-restore", dir)

	write(t, filepath.Join(dir, "keep.md"), "edited by the attempt\n")
	if err := os.Remove(filepath.Join(dir, "deleted.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "created.md"), "the attempt made this\n")

	moved, err := Restore(rec)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(moved) == 0 {
		t.Error("Restore reported nothing, but three things had changed")
	}

	if got := read(t, filepath.Join(dir, "keep.md")); got != "the original\n" {
		t.Errorf("keep.md = %q, want the recorded contents back", got)
	}
	if got := read(t, filepath.Join(dir, "deleted.md")); got != "was here\n" {
		t.Errorf("deleted.md = %q, want it restored", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "created.md")); err == nil {
		t.Error("created.md is still in the directory")
	}

	// Moved, not deleted: it is the thing the run was measuring.
	quarantined, err := QuarantinePath(rec.SessionID, "created.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, quarantined); got != "the attempt made this\n" {
		t.Errorf("the quarantined file = %q, want it kept as it was", got)
	}

	// And the directory now agrees with its record again.
	drift, err := Verify(rec, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !drift.Clean() {
		t.Errorf("after Restore the directory still differs: %s", drift.Error())
	}
}

func TestRestoreIgnoresAGitRecord(t *testing.T) {
	// A git directory is put back through git, not by copying files over it.
	moved, err := Restore(&Record{Kind: KindGit, SessionID: "s"})
	if err != nil || moved != nil {
		t.Errorf("Restore(git record) = (%v, %v), want (nil, nil)", moved, err)
	}
	if moved, err := Restore(nil); err != nil || moved != nil {
		t.Errorf("Restore(nil) = (%v, %v), want (nil, nil)", moved, err)
	}
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

func TestSessionIDsCannotReachOutsideTheStore(t *testing.T) {
	// Session ids come out of a transcript, which is a file a tool wrote and not
	// necessarily anything trustworthy, and they end up as path components.
	home := useTempHome(t)

	for _, id := range []string{
		"../../etc/passwd",
		"..",
		".",
		"a/b/c",
		"/absolute/path",
		".",
		"",
		"   ",
		"one two three",
	} {
		path, err := recordPath(id)
		if err != nil {
			t.Fatalf("recordPath(%q): %v", id, err)
		}
		rel, err := filepath.Rel(home, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("recordPath(%q) = %s, which is outside %s", id, path, home)
		}

		snap, err := SnapshotPath(id)
		if err != nil {
			t.Fatal(err)
		}
		if rel, err := filepath.Rel(home, snap); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("SnapshotPath(%q) = %s, which is outside %s", id, snap, home)
		}

		disc, err := DiscardDir(id)
		if err != nil {
			t.Fatal(err)
		}
		if rel, err := filepath.Rel(home, disc); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("DiscardDir(%q) = %s, which is outside %s", id, disc, home)
		}
	}
}

func TestRootPrefersTheEnvironmentOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	got, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Errorf("Root() = %q, want %q", got, home)
	}
}

func TestSaveCreatesTheStoreWhenItIsNotThereYet(t *testing.T) {
	// The hook runs inside somebody's session, in a directory that has never had
	// an aeval run in it. Making the store is part of the first save.
	rec := captureAndSave(t, "first-run", t.TempDir())

	home, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "starts", "first-run.json")); err != nil {
		t.Errorf("the record was not written under the store: %v", err)
	}
	if _, err := os.Stat(rec.Snapshot); err != nil {
		t.Errorf("the snapshot was not written under the store: %v", err)
	}
}

func TestSaveIsReplacedAtomicallyRatherThanAppendedTo(t *testing.T) {
	// A hook that is killed partway through must not leave a half-written record
	// that reads as a valid one, so the file is written beside its final name
	// and renamed into place.
	useTempHome(t)
	captureAndSave(t, "sess-rename", t.TempDir())

	path, err := recordPath("sess-rename")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a temporary file was left behind next to the record")
	}

	// Saving again replaces rather than merges.
	rec2, ok, err := Read("sess-rename")
	if err != nil || !ok {
		t.Fatalf("Read = (%v, %v, %v)", rec2, ok, err)
	}
	rec2.ToolVersion = "second"
	if err := Save(rec2, Options{ToolVersion: "second"}); err != nil {
		t.Fatal(err)
	}
	back, _, err := Read("sess-rename")
	if err != nil {
		t.Fatal(err)
	}
	if back.ToolVersion != "second" {
		t.Errorf("tool_version = %q, want the second save to have replaced the first",
			back.ToolVersion)
	}
}

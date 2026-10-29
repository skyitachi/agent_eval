// This file is the only place in this package that operates on a directory the
// harness did not create. Everything else materialises a workspace under the
// work root and owns its lifetime; here the directory belongs to the person
// running the evaluation, and the contract is to put it back.
//
// Nothing in this file may call RemoveDir. The directory is the user's, and a
// cleanup path that reaches it would delete a working tree rather than a
// workspace -- which is why the runner's removal is guarded structurally rather
// than by a flag.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// HarnessDir is the dotted directory the harness owns inside a workspace.
//
// It is declared here rather than beside the graders that write into it because
// the two things that have to agree about it are both about the workspace's
// layout: the graders, which put files there, and a run working in a directory
// it did not create, which has to take them away again.
const HarnessDir = ".agenteval"

const (
	// inPlaceLockDir and inPlaceLockName place the lock inside .git, where it
	// travels with the repository rather than with the tree: a lock beside the
	// files would be removed by the very clean the restore runs.
	inPlaceLockDir  = "agenteval"
	inPlaceLockName = "inplace.lock"
	// restoreBudget bounds the restore. It is generous because every step is a
	// local git command, and it exists only so that a wedged git cannot hang a
	// run forever at the moment it is trying to give the directory back.
	restoreBudget = 2 * time.Minute
	// StashRef holds the uncommitted work a run set aside. It is a ref of the
	// harness's own rather than an entry in the stash list, because the agent
	// runs git in this repository with permissions bypassed: a `git stash pop`
	// in a replayed session would apply the user's work into the agent's tree,
	// and the restore would then throw it away with the agent's.
	StashRef     = "refs/agenteval/stash"
	stashMessage = "aeval: uncommitted work set aside by an in-place run, re-applied when it ends"
)

// InPlace is a repository a run works in directly, together with what it needs
// to put that repository back.
//
// The state captured at AcquireInPlace is the state Restore returns to, so the
// two calls are a pair and neither is meaningful alone. AcquireInPlace is the
// only constructor: a zero InPlace has no lock, no recorded HEAD and nothing to
// restore to.
type InPlace struct {
	// Repo is the absolute path of the repository being worked in.
	Repo string
	// Head is the commit HEAD pointed at when the run started.
	Head string
	// Branch is the branch HEAD pointed at, and is empty when it was detached.
	// Restore puts the ref back before it checks anything out, because an agent
	// that re-attached and committed would otherwise have moved it.
	Branch string
	// Ref is the commit most recently checked out for a task.
	Ref string

	lockPath      string
	ignored       map[string]bool
	refs          map[string]string
	hadHarnessDir bool

	// checkedOut is set from the first checkout until the next restore, so
	// that Finish can tell a repository a chain left mid-way -- a run cancelled
	// between two of its cells -- from one already put back.
	checkedOut bool
	// stash is the commit holding the work set aside when the repository was
	// taken, empty when there was none or once it is back. stashed counts its
	// paths and statusBefore is `git status` as it was found, which is what the
	// re-applied tree is compared against.
	stash        string
	stashed      int
	statusBefore string
	// protect is Options.Protect relative to Repo, slash-separated, for the
	// paths inside it. finished is set by the first Finish.
	protect  []string
	finished bool
	// excluded is set while .git/info/exclude carries the protected paths;
	// excludeHad and excludeOrig are what it held before.
	excluded    bool
	excludeHad  bool
	excludeOrig []byte
}

// InPlaceOptions shapes how a repository is taken.
type InPlaceOptions struct {
	// Stash takes a repository with uncommitted changes: they are set aside
	// when it is taken and re-applied by Finish. Without it such a repository
	// is refused, because nothing else here could put the changes back.
	Stash bool
	// Protect names paths the harness itself writes while the run is going --
	// its database, the replay's own directory. One inside the repository has
	// to be ignored by git, or setting the changes aside would carry it off and
	// the restore's clean would delete it. They are also left out of the
	// restore's list of ignored files the agent created.
	Protect []string
}

// RestoreReport is what happened to the repository when it was put back.
//
// It exists because "restored" is not the same claim as "as you left it": the
// two lists below are things a restore deliberately does not touch, and a
// reader who is not told about them would take a clean status for a repository
// that is exactly as it was.
type RestoreReport struct {
	// LeftBehind are ignored paths that were not there before the run. They are
	// reported rather than removed: -x on the clean would take the user's own
	// ignored files with them, and those are not the harness's to delete.
	LeftBehind []string
	// MovedRefs are refs other than HEAD that the agent created, moved or
	// deleted. Reported rather than repaired, because the harness cannot know
	// which of them the agent meant to keep.
	MovedRefs []string
	// Clean is whether the worktree is back to the recorded commit with no
	// changes at all.
	Clean bool
	// Notes is the sentence a report shows. It is written even when everything
	// went cleanly: an empty string reads as "not restored", and that is the
	// one reading this must never invite.
	Notes string
}

// inPlaceLock is the breadcrumb a killed run leaves behind.
//
// It carries the state the restore would have used, so that a repository left
// checked out at an old commit is recoverable by hand. It is written before the
// first checkout, because the process can die at any point after that.
type inPlaceLock struct {
	Repo      string `json:"repo"`
	Head      string `json:"head"`
	Branch    string `json:"branch"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	Tool      string `json:"tool"`
	// Stash is the commit holding the uncommitted work the run set aside. It
	// is the one thing in a killed run's repository that is not on any branch.
	Stash string `json:"stash,omitempty"`
}

// CheckInPlace reports whether a repository can be worked in directly, without
// taking the lock.
//
// It is separate from AcquireInPlace so that a caller can refuse a dataset
// before any of it runs -- a run that dies on the fourth task because the
// fourth task's repository is dirty has already worked in three others. Every
// question here is asked again under the lock, because the answers can change
// between the two.
func CheckInPlace(ctx context.Context, repo string) error {
	info, err := os.Stat(repo)
	if err != nil {
		return fmt.Errorf("the directory %s is not there, and this run works in the recorded "+
			"directory itself rather than in a copy of it: there is nothing to check out", repo)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", repo)
	}
	// A linked worktree and a submodule both keep a gitfile here instead of a
	// directory. The lock has nowhere to live in that case, and a worktree's
	// checkout would fight the tree it is linked to.
	gitDir := filepath.Join(repo, ".git")
	if gi, err := os.Stat(gitDir); err != nil || !gi.IsDir() {
		return fmt.Errorf("%s is not a repository this run can work in: it has no .git directory "+
			"of its own (a linked worktree and a submodule keep theirs elsewhere, and this run has "+
			"to be able to put back what it changes)", repo)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gitmodules")); err == nil {
		return fmt.Errorf("%s has submodules, and this run cannot measure or put back what happens "+
			"inside one: a change to a submodule's contents is invisible to a diff at the repository "+
			"root, and a checkout does not restore it. Run this task against a copy instead", repo)
	}
	out, err := runHost(ctx, repo, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("%s is not a git repository: %s", repo, firstLine(out))
	}
	if strings.TrimSpace(out) == "true" {
		return fmt.Errorf("%s is a bare repository, so it has no worktree to run an agent in", repo)
	}
	return requireCleanTree(ctx, repo, "work in")
}

// CheckInPlaceWith is CheckInPlace for a run taking the repository under opts.
//
// With Stash, uncommitted changes are not a refusal: they are what the run sets
// aside. What is refused instead is everything a stash cannot carry and put
// back exactly -- see checkStashable.
func CheckInPlaceWith(ctx context.Context, repo string, opts InPlaceOptions) error {
	dirty := CheckInPlace(ctx, repo)
	if dirty != nil && !errors.Is(dirty, errDirty) {
		return dirty
	}
	// Before the dirty refusal: work an earlier run left set aside explains a
	// directory better than the list of paths it left behind does.
	if err := checkLeftover(ctx, repo); err != nil {
		return err
	}
	rels := protectedRel(repo, opts.Protect)
	if err := checkProtected(ctx, repo, "HEAD", rels); err != nil {
		return err
	}
	if dirty != nil && len(rels) > 0 {
		// The harness's own files are excluded while the run holds the
		// repository, so they are not what makes it dirty.
		if paths, err := UncommittedPaths(ctx, repo, opts.Protect); err == nil && len(paths) == 0 {
			dirty = nil
		}
	}
	if !opts.Stash {
		return dirty
	}
	return checkStashable(ctx, repo)
}

// UncommittedPaths lists the worktree's uncommitted paths, untracked files
// included and the protected paths left out: the ones a stashing run would set
// aside.
func UncommittedPaths(ctx context.Context, repo string, protect []string) ([]string, error) {
	out, err := runHost(ctx, repo, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("read the state of %s: %s", repo, firstLine(out))
	}
	s := &InPlace{protect: protectedRel(repo, protect)}
	var paths []string
	for _, p := range porcelainPaths(out) {
		if raw, err := strconv.Unquote(p); err == nil && s.isProtected(raw) {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// ErrStranded is matched by a refusal that is about a repository an earlier
// run left in the wrong state, as opposed to one this run merely cannot work
// in. A caller that would otherwise fall back to a copy must not: falling back
// would leave the user's work where the earlier run left it, unannounced.
var ErrStranded = errors.New("sandbox: the repository was left mid-run by an earlier run")

type strandedError struct{ msg string }

func (e *strandedError) Error() string        { return e.msg }
func (e *strandedError) Is(target error) bool { return target == ErrStranded }

// errDirty is matched by requireCleanTree's refusal, so that a stashing run can
// tell "dirty" apart from the refusals it still has to honour.
var errDirty = errors.New("sandbox: the worktree has uncommitted changes")

type dirtyError struct{ msg string }

func (e *dirtyError) Error() string        { return e.msg }
func (e *dirtyError) Is(target error) bool { return target == errDirty }

// checkLeftover refuses a repository holding work an earlier run set aside and
// never re-applied. That work is the user's, it is on no branch, and a second
// stash would bury it under another.
func checkLeftover(ctx context.Context, repo string) error {
	out, err := runHost(ctx, repo, "rev-parse", "--verify", "--quiet", StashRef+"^{commit}")
	if err != nil {
		return nil
	}
	sha := strings.TrimSpace(out)
	lockPath := filepath.Join(repo, ".git", inPlaceLockDir, inPlaceLockName)
	if _, err := os.Stat(lockPath); err == nil {
		var held inPlaceLock
		if raw, err := os.ReadFile(lockPath); err == nil && json.Unmarshal(raw, &held) == nil &&
			held.Stash == sha && processAlive(held.PID) {
			// A run in progress set it aside and will bring it back. Taking the
			// lock is what decides whether this run may start, and it waits for
			// nothing, so it will say so if that run is still there.
			return nil
		}
		// The lock names the commit and branch to go back to as well, so its
		// instructions are the complete ones.
		return heldError(lockPath, repo)
	}
	return &strandedError{msg: fmt.Sprintf("an earlier in-place run in %s set uncommitted work aside in commit %s "+
		"(%s) and did not re-apply it, so this run will not set aside more on top of it. The work is still "+
		"there. Once the directory is as you want it, bring it back and drop the ref with:\n\n"+
		"    git -C %s stash apply --index %s\n    git -C %s update-ref -d %s",
		repo, shortSHA(sha), StashRef, repo, sha, repo, StashRef)}
}

// checkProtected refuses a repository that tracks, at rev, a path the harness
// itself is writing. Untracked ones are excluded for the run's duration (see
// excludeProtected); a tracked one would be rewritten by a checkout, stashed
// with the user's changes, and measured as the agent's work.
func checkProtected(ctx context.Context, repo, rev string, rels []string) error {
	for _, rel := range rels {
		out, err := runHost(ctx, repo, "ls-tree", "-r", "--name-only", rev, "--", rel)
		if err != nil {
			return fmt.Errorf("read %s at %s in %s: %s", rel, shortSHA(rev), repo, firstLine(out))
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("%s is tracked by %s at %s, and this run writes there while it works in the "+
				"repository: a checkout would overwrite it, and putting the repository back would undo what "+
				"the run wrote. Move it outside the repository, or run this task against a copy",
				rel, repo, shortSHA(rev))
		}
	}
	return nil
}

// excludeMarker opens the block a run adds to .git/info/exclude.
const excludeMarker = "# aeval in-place run: paths the harness writes while it works here"

// excludeProtected adds the protected paths to .git/info/exclude for as long
// as the run holds the repository.
//
// The repository's own .gitignore is not enough: the recorded commit may
// predate the line that ignores them, and once it is checked out the restore's
// clean would delete them. info/exclude is read whatever is checked out.
func (s *InPlace) excludeProtected() error {
	if len(s.protect) == 0 {
		return nil
	}
	path := filepath.Join(s.Repo, ".git", "info", "exclude")
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A block a killed run left behind is not the user's, so it is not
		// part of what gets put back. The block is always the file's tail.
		if i := strings.Index(string(raw), excludeMarker); i >= 0 {
			raw = raw[:i]
		}
		s.excludeHad, s.excludeOrig = true, raw
	case !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", path, err)
	}
	var b strings.Builder
	b.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(excludeMarker + "\n")
	for _, rel := range s.protect {
		b.WriteString("/" + rel + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	s.excluded = true
	return nil
}

// unexclude puts .git/info/exclude back as it was found.
func (s *InPlace) unexclude() error {
	if !s.excluded {
		return nil
	}
	path := filepath.Join(s.Repo, ".git", "info", "exclude")
	var err error
	if s.excludeHad {
		err = os.WriteFile(path, s.excludeOrig, 0o644)
	} else {
		err = os.Remove(path)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("put %s back: %w", path, err)
	}
	s.excluded = false
	return nil
}

// resolvePath follows symlinks as far as the path exists, so that /var and
// /private/var on macOS compare as the same directory.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(r, filepath.Base(abs))
	}
	return abs
}

// checkStashable refuses the uncommitted states a stash cannot carry and put
// back exactly.
//
//   - Unmerged paths: a stash cannot record a conflicted index at all.
//   - A merge, rebase, cherry-pick, revert or bisect in progress: its state
//     lives in .git beside the index, which a stash does not carry and a
//     checkout of another commit would leave describing the wrong tree.
//   - An untracked directory that is a repository of its own: a stash skips it
//     and the restore's clean would delete it.
func checkStashable(ctx context.Context, repo string) error {
	out, err := runHost(ctx, repo, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return fmt.Errorf("read the state of %s: %s", repo, firstLine(out))
	}
	var unmerged, nested []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		xy, p := line[:2], strings.TrimSpace(line[3:])
		switch xy {
		case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
			unmerged = append(unmerged, strconv.Quote(p))
		case "??":
			if strings.HasSuffix(p, "/") {
				if _, err := os.Stat(filepath.Join(repo, p, ".git")); err == nil {
					nested = append(nested, strconv.Quote(p))
				}
			}
		}
	}
	if len(unmerged) > 0 {
		return fmt.Errorf("%s has unresolved conflicts (%s), and a conflicted index cannot be set aside and "+
			"put back. Resolve or abort them first, or run this task against a copy", repo, strings.Join(unmerged, ", "))
	}
	for _, marker := range []struct{ path, what string }{
		{"MERGE_HEAD", "a merge"}, {"CHERRY_PICK_HEAD", "a cherry-pick"}, {"REVERT_HEAD", "a revert"},
		{"rebase-merge", "a rebase"}, {"rebase-apply", "a rebase or am"}, {"BISECT_LOG", "a bisect"},
	} {
		if _, err := os.Stat(filepath.Join(repo, ".git", marker.path)); err == nil {
			return fmt.Errorf("%s is in the middle of %s, and that state cannot be set aside and put back: "+
				"checking out the recorded commit would leave it describing the wrong tree. Finish or abort "+
				"it first, or run this task against a copy", repo, marker.what)
		}
	}
	if len(nested) > 0 {
		return fmt.Errorf("%s holds untracked repositories of its own (%s), which git cannot set aside, and "+
			"putting the directory back would delete them. Move or ignore them first, or run this task "+
			"against a copy", repo, strings.Join(nested, ", "))
	}
	return nil
}

// requireCleanTree refuses a repository whose worktree has changes.
//
// Uncommitted work is the one thing a restore cannot reconstruct: the recorded
// commit is recoverable, the changes on top of it are not, so a run that
// started on top of them would have to discard them to check anything out and
// could never put them back.
func requireCleanTree(ctx context.Context, repo, what string) error {
	out, err := runHost(ctx, repo, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read the state of %s: %s", repo, firstLine(out))
	}
	dirty := porcelainPaths(out)
	if len(dirty) == 0 {
		return nil
	}
	shown := dirty
	more := ""
	if len(shown) > 5 {
		more = fmt.Sprintf(" (and %d more)", len(shown)-5)
		shown = shown[:5]
	}
	return &dirtyError{msg: fmt.Sprintf("%s has uncommitted changes, so this run cannot %s it: %s%s. "+
		"A run checks the repository out at the commit the task names and puts it back afterwards, "+
		"and uncommitted changes are the one thing that cannot be put back. Commit or stash them "+
		"first, or run this task against a copy",
		repo, what, strings.Join(shown, ", "), more)}
}

// AcquireInPlace takes the repository for one run and records what it needs to
// put it back.
//
// The lock is not a courtesy: two runs in one repository would check out over
// each other and each would put back a state neither of them started from. It
// is taken exclusively and never waited for, so a second run says so instead of
// hanging.
func AcquireInPlace(ctx context.Context, repo string) (*InPlace, error) {
	return AcquireInPlaceWith(ctx, repo, InPlaceOptions{})
}

// AcquireInPlaceWith is AcquireInPlace under opts. With Stash, a repository
// with uncommitted changes is taken by setting them aside under the lock; the
// lock records the commit holding them, and Finish is what brings them back.
func AcquireInPlaceWith(ctx context.Context, repo string, opts InPlaceOptions) (*InPlace, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	if err := CheckInPlaceWith(ctx, abs, opts); err != nil {
		return nil, err
	}

	s := &InPlace{Repo: abs, protect: protectedRel(abs, opts.Protect)}
	s.lockPath = filepath.Join(abs, ".git", inPlaceLockDir, inPlaceLockName)
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("prepare the lock in %s: %w", abs, err)
	}
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, heldError(s.lockPath, abs)
		}
		return nil, fmt.Errorf("take the lock on %s: %w", abs, err)
	}

	// From here on a failure has to give the lock back, or the repository is
	// left refusing runs for a reason that is no longer true.
	release := func() {
		_ = f.Close()
		_ = os.Remove(s.lockPath)
	}
	if s.Head, err = s.revParse(ctx, "HEAD"); err != nil {
		release()
		return nil, err
	}
	if s.Branch, err = s.branch(ctx); err != nil {
		release()
		return nil, err
	}
	lock := inPlaceLock{
		Repo: abs, Head: s.Head, Branch: s.Branch,
		PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339), Tool: "aeval",
	}
	blob, _ := json.Marshal(lock)
	if _, err := f.Write(append(blob, '\n')); err != nil {
		release()
		return nil, fmt.Errorf("write the lock in %s: %w", abs, err)
	}
	// The breadcrumb is worthless if it is still in a buffer when the process
	// dies, which is the only moment it is ever read.
	if err := f.Sync(); err != nil {
		release()
		return nil, fmt.Errorf("write the lock in %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(s.lockPath)
		return nil, fmt.Errorf("write the lock in %s: %w", abs, err)
	}

	// The harness's own files are hidden from git first, so that neither the
	// stash nor the restore's clean can take them.
	giveUp := func() {
		_ = s.unexclude()
		_ = os.Remove(s.lockPath)
	}
	if err := s.excludeProtected(); err != nil {
		giveUp()
		return nil, err
	}
	// Asked again under the lock: the answer that matters is the one the
	// checkout will see, not the one the pre-flight saw.
	if !opts.Stash {
		if err := requireCleanTree(ctx, abs, "work in"); err != nil {
			giveUp()
			return nil, err
		}
	} else {
		if err := checkStashable(ctx, abs); err != nil {
			giveUp()
			return nil, err
		}
		if err := s.setAside(ctx); err != nil {
			giveUp()
			return nil, err
		}
		if s.stash != "" {
			// The lock is rewritten to name the commit, because from here on a
			// process that dies leaves the user's work on no branch, and the
			// lock is what tells them where it is.
			lock.Stash = s.stash
			if err := writeLock(s.lockPath, lock); err != nil {
				_, _ = s.unstash(ctx)
				giveUp()
				return nil, err
			}
		}
	}
	// After the stash, so that what the restore compares against is the tree
	// the run starts from rather than the one it set aside.
	s.ignored = s.ignoredPaths(ctx)
	s.refs = s.refSnapshot(ctx)
	if _, err := os.Stat(filepath.Join(abs, HarnessDir)); err == nil {
		s.hadHarnessDir = true
	}
	return s, nil
}

// writeLock replaces the lock's contents. Through a rename, so that a process
// that dies part-way leaves the old breadcrumb rather than half of the new one.
func writeLock(path string, lock inPlaceLock) error {
	blob, _ := json.Marshal(lock)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(blob, '\n'), 0o644); err != nil {
		return fmt.Errorf("write the lock %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write the lock %s: %w", path, err)
	}
	return nil
}

// setAside stashes the worktree's uncommitted changes, untracked files
// included, and moves the stash from the stash list to StashRef.
//
// Ignored files are not touched: they are not part of what a checkout changes,
// and the restore leaves them alone too.
func (s *InPlace) setAside(ctx context.Context) error {
	out, err := runHost(ctx, s.Repo, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("read the state of %s: %s", s.Repo, firstLine(out))
	}
	paths := porcelainPaths(out)
	if len(paths) == 0 {
		return nil
	}
	before := s.stashTop(ctx)
	if out, err := runHost(ctx, s.Repo, "stash", "push", "--include-untracked", "--quiet", "-m", stashMessage); err != nil {
		// A stash that failed part-way may still have recorded one and cleared
		// the tree, so it is put back before the refusal.
		if top := s.stashTop(ctx); top != "" && top != before {
			_, _ = runHost(ctx, s.Repo, "stash", "pop", "--index", "--quiet")
		}
		return fmt.Errorf("set aside the uncommitted changes in %s: %s", s.Repo, firstLine(out))
	}
	sha := s.stashTop(ctx)
	if sha == "" || sha == before {
		return fmt.Errorf("set aside the uncommitted changes in %s: git recorded no stash, and the "+
			"changes cannot be put back unless one is recorded", s.Repo)
	}
	s.stash, s.stashed, s.statusBefore = sha, len(paths), out
	if out, err := runHost(ctx, s.Repo, "update-ref", StashRef, sha); err != nil {
		_, _ = runHost(ctx, s.Repo, "stash", "pop", "--index", "--quiet")
		s.stash = ""
		return fmt.Errorf("record the uncommitted changes of %s: %s", s.Repo, firstLine(out))
	}
	// Off the stash list, where the agent's own `git stash pop` would find it.
	if out, err := runHost(ctx, s.Repo, "stash", "drop", "--quiet"); err != nil {
		_, _ = s.unstash(ctx)
		return fmt.Errorf("move the uncommitted changes of %s off the stash list: %s", s.Repo, firstLine(out))
	}
	if err := requireCleanTree(ctx, s.Repo, "set aside the changes in"); err != nil {
		// Something the stash did not take. Running on top of it would charge
		// it to the agent and the restore's clean would delete it.
		_, _ = s.unstash(ctx)
		return fmt.Errorf("not everything in %s could be set aside: %w", s.Repo, err)
	}
	return nil
}

func (s *InPlace) stashTop(ctx context.Context) string {
	out, err := runHost(ctx, s.Repo, "rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// unstash re-applies the work setAside took, on a tree that must be clean, and
// drops StashRef once the worktree reads exactly as it was found. Short of
// that the ref is kept: it is the only copy of the user's work.
func (s *InPlace) unstash(ctx context.Context) (string, error) {
	if s.stash == "" {
		return "", nil
	}
	keep := fmt.Sprintf("The work is safe in commit %s (%s); once the directory is as you want it, bring it "+
		"back with:\n\n    git -C %s stash apply --index %s\n    git -C %s update-ref -d %s",
		shortSHA(s.stash), StashRef, s.Repo, s.stash, s.Repo, StashRef)
	if err := requireCleanTree(ctx, s.Repo, "re-apply the uncommitted work in"); err != nil {
		return "", &strandedError{msg: "the uncommitted work set aside in " + s.Repo +
			" was NOT re-applied, because the directory was not put back clean. " + keep}
	}
	if out, err := runHost(ctx, s.Repo, "stash", "apply", "--index", "--quiet", s.stash); err != nil {
		return "", &strandedError{msg: "re-applying the uncommitted work set aside in " + s.Repo +
			" failed (" + firstLine(out) + "). " + keep}
	}
	after, err := runHost(ctx, s.Repo, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || after != s.statusBefore {
		return "", &strandedError{msg: "the uncommitted work set aside in " + s.Repo + " was re-applied, but " +
			"`git status` does not read as it did before the run, so the commit holding it is kept. Compare " +
			"with `git -C " + s.Repo + " stash show --include-untracked -p " + s.stash + "`, then drop it with " +
			"`git -C " + s.Repo + " update-ref -d " + StashRef + "`"}
	}
	_, _ = runHost(ctx, s.Repo, "update-ref", "-d", StashRef, s.stash)
	n := s.stashed
	s.stash = ""
	return fmt.Sprintf("re-applied the %d uncommitted path(s) set aside in %s when the run started", n, s.Repo), nil
}

// Stashed reports how many uncommitted paths the run set aside, zero when the
// repository was clean.
func (s *InPlace) Stashed() int { return s.stashed }

// Finish gives the repository back at the end of a run: it puts back a chain
// the run left mid-way, then re-applies the uncommitted work the run set aside.
//
// A chain is left mid-way when a run is cancelled between two of its cells --
// the restore belongs to the chain's last cell, and that cell never ran. The
// work is re-applied only on a tree that was put back clean, since applying it
// over the agent's changes would mix the two. Finish runs once; later calls
// return nothing.
func (s *InPlace) Finish(ctx context.Context) (string, error) {
	if s.finished {
		return "", nil
	}
	s.finished = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreBudget)
	defer cancel()
	var notes []string
	if s.checkedOut {
		rep, err := s.Restore(ctx)
		notes = append(notes, rep.Notes)
		if err != nil {
			if s.stash != "" {
				return strings.Join(notes, "; "), &strandedError{msg: rep.Notes + ". The uncommitted work set aside " +
					"when the run started was not re-applied on top of that; it is in commit " + shortSHA(s.stash) +
					" (" + StashRef + ")"}
			}
			return strings.Join(notes, "; "), err
		}
	}
	note, err := s.unstash(ctx)
	if note != "" {
		notes = append(notes, note)
	}
	if xerr := s.unexclude(); xerr != nil && err == nil {
		err = xerr
	}
	return strings.Join(notes, "; "), err
}

// protectedRel turns the protected paths inside repo into slash-separated
// paths relative to it.
func protectedRel(repo string, paths []string) []string {
	root := resolvePath(repo)
	var out []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		rel, err := filepath.Rel(root, resolvePath(p))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func (s *InPlace) isProtected(p string) bool {
	for _, rel := range s.protect {
		if p == rel || strings.HasPrefix(p, rel+"/") {
			return true
		}
	}
	return false
}

// Checkout detaches the repository at ref.
//
// Detached rather than on a branch, deliberately: an agent that commits would
// otherwise move the user's branch pointer, and the restore would then be
// putting back a branch the agent had rewritten. Detached, its commits are
// unreachable and the diff against the baseline still shows what it changed.
func (s *InPlace) Checkout(ctx context.Context, ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("no commit to check out in %s", s.Repo)
	}
	if err := checkProtected(ctx, s.Repo, ref, s.protect); err != nil {
		return err
	}
	// A person may have edited the tree while the previous chain ran, and
	// --force would throw that away without saying so.
	if err := requireCleanTree(ctx, s.Repo, "check out "+shortSHA(ref)+" in"); err != nil {
		return err
	}
	s.checkedOut = true
	if out, err := runHost(ctx, s.Repo, "checkout", "--detach", "--force", ref); err != nil {
		return fmt.Errorf("check out %s in %s: %s", ref, s.Repo, firstLine(out))
	}
	s.Ref = ref
	return nil
}

// Restore puts the repository back where it was found.
//
// The context it is given is deliberately ignored. Every caller reaches this
// after something went wrong or ran out of time -- a Ctrl-C, a cell that hit
// its budget, --fail-fast -- and a restore that inherited that cancellation
// would fail every git command instantly, leaving the repository detached at
// the recorded commit with the agent's changes in it. That is the state this
// file exists to prevent, so the restore drops the cancellation and bounds
// itself instead. Values carried by the context survive; its deadline and its
// cancellation do not.
//
// Every step runs even when an earlier one failed, because they repair
// different things and a failure in one says nothing about the next.
func (s *InPlace) Restore(ctx context.Context) (RestoreReport, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreBudget)
	defer cancel()

	var failures []string
	step := func(what string, args ...string) {
		if out, err := runHost(ctx, s.Repo, args...); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", what, firstLine(out)))
		}
	}

	// The ref first. An agent that re-attached to the branch and committed
	// would have moved it, and checking the branch out would then land on the
	// agent's commit rather than on the one the run started from.
	if s.Branch != "" {
		step("put branch "+s.Branch+" back", "update-ref", "refs/heads/"+s.Branch, s.Head)
	}
	target := s.Head
	if s.Branch != "" {
		target = s.Branch
	}
	step("check the worktree out", "checkout", "--force", target)
	// -ff so a repository the agent cloned inside this one is removed too.
	// Never -x: that would delete the user's own ignored files, which are not
	// the harness's to delete.
	step("remove what the agent left", "clean", "-ffd")
	if !s.hadHarnessDir {
		if err := os.RemoveAll(filepath.Join(s.Repo, HarnessDir)); err != nil {
			failures = append(failures, "remove "+HarnessDir+": "+err.Error())
		}
	}

	s.checkedOut = false
	rep := RestoreReport{
		LeftBehind: s.newIgnoredPaths(ctx),
		MovedRefs:  s.movedRefs(ctx),
	}
	if out, err := runHost(ctx, s.Repo, "status", "--porcelain"); err == nil {
		rep.Clean = len(porcelainPaths(out)) == 0
	}
	rep.Notes = restoreNotes(s.Repo, s.Head, s.Branch, rep, failures)
	if len(failures) > 0 {
		return rep, errors.New(strings.Join(failures, "; "))
	}
	return rep, nil
}

// Release drops the lock. The run's last act, not the restore's: a lock held
// only across the restore would let a second run start while the first was
// still reading its own results.
func (s *InPlace) Release() error {
	err := os.Remove(s.lockPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Tidy the directory the lock lived in, so a repository that has been run
	// in does not keep an empty .git/agenteval behind.
	_ = os.Remove(filepath.Dir(s.lockPath))
	return nil
}

// ---------------------------------------------------------------------------
// Repository state
// ---------------------------------------------------------------------------

func (s *InPlace) revParse(ctx context.Context, rev string) (string, error) {
	out, err := runHost(ctx, s.Repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read %s in %s: %s", rev, s.Repo, firstLine(out))
	}
	return strings.TrimSpace(out), nil
}

func (s *InPlace) branch(ctx context.Context) (string, error) {
	out, err := runHost(ctx, s.Repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		// Detached HEAD is a legitimate starting state, and git reports it as a
		// failure rather than as an empty answer.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// ignoredPaths lists what the repository already hides from git.
func (s *InPlace) ignoredPaths(ctx context.Context) map[string]bool {
	out, err := runHost(ctx, s.Repo, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set
}

// newIgnoredPaths are the ignored paths that appeared during the run. They are
// the residue a restore cannot account for: invisible to the diff, untouched by
// the clean, and impossible to tell apart from a file the user had there.
func (s *InPlace) newIgnoredPaths(ctx context.Context) []string {
	var out []string
	for p := range s.ignoredPaths(ctx) {
		if !s.ignored[p] && !s.isProtected(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (s *InPlace) refSnapshot(ctx context.Context) map[string]string {
	out, err := runHost(ctx, s.Repo, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return map[string]string{}
	}
	return parseRefs(out)
}

// movedRefs are the refs the agent created, moved or deleted, apart from HEAD's
// own branch, which the restore has already put back.
func (s *InPlace) movedRefs(ctx context.Context) []string {
	now := s.refSnapshot(ctx)
	var out []string
	for name, sha := range now {
		if name == StashRef {
			// The harness's own, and accounted for by Finish.
			continue
		}
		if s.refs[name] != sha {
			out = append(out, name+" -> "+shortSHA(sha))
		}
	}
	for name := range s.refs {
		if _, ok := now[name]; !ok {
			out = append(out, name+" (deleted)")
		}
	}
	sort.Strings(out)
	return out
}

func parseRefs(out string) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && name != "" {
			refs[name] = sha
		}
	}
	return refs
}

// porcelainPaths reads the paths out of `git status --porcelain` output. The
// status letters are dropped: they are not what the message is about, and a
// reader who wants them can run git themselves.
func porcelainPaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// "XY path", and a rename is "XY old -> new".
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if _, to, ok := strings.Cut(p, " -> "); ok {
			p = to
		}
		paths = append(paths, strconv.Quote(p))
	}
	sort.Strings(paths)
	return paths
}

// ---------------------------------------------------------------------------
// What the user is told
// ---------------------------------------------------------------------------

// restoreNotes is the sentence the report shows.
//
// It names the commit as well as the branch, because the branch is where a
// person looks and the commit is what the restore actually guarantees: a branch
// that had moved on between the checkout and the restore is put back to the
// commit the run found, not to wherever it had got to.
func restoreNotes(repo, head, branch string, rep RestoreReport, failures []string) string {
	if len(failures) > 0 {
		return "the repository at " + repo + " was NOT fully put back: " +
			strings.Join(failures, "; ") + ". It is checked out at the wrong commit or has the " +
			"agent's changes in it, and it has to be repaired by hand"
	}
	where := "detached at " + shortSHA(head)
	if branch != "" {
		where = "on " + branch + " at " + shortSHA(head)
	}
	note := "put back " + where + " in " + repo
	if !rep.Clean {
		note += ", but it is not clean"
	}
	switch {
	case len(rep.LeftBehind) > 0:
		note += "; " + strconv.Itoa(len(rep.LeftBehind)) + " ignored path(s) the agent created were " +
			"left in place: " + strings.Join(rep.LeftBehind, ", ")
	default:
		note += "; nothing was left behind"
	}
	if len(rep.MovedRefs) > 0 {
		note += "; refs the agent moved were left as they are: " + strings.Join(rep.MovedRefs, ", ")
	}
	return note
}

// heldError is the refusal a second run gets, and it is also the recovery
// instruction for a run that was killed: the lock is the only record of the
// state the restore would have used.
func heldError(lockPath, repo string) error {
	var held inPlaceLock
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("%s is held by another run, and its lock could not be read (%v): "+
			"wait for that run to finish, or remove %s once you are sure it is gone",
			repo, err, lockPath)
	}
	if err := json.Unmarshal(raw, &held); err != nil {
		return fmt.Errorf("%s is held by another run, and its lock could not be read (%v): "+
			"wait for that run to finish, or remove %s once you are sure it is gone",
			repo, err, lockPath)
	}
	if !processAlive(held.PID) {
		return &strandedError{msg: fmt.Sprintf("a run was working in %s and did not finish (started %s, process %d, "+
			"which is no longer running), so it was interrupted before it could put the directory "+
			"back.\n\nPut it back with:\n\n%s",
			repo, held.StartedAt, held.PID, recoveryCommands(repo, held))}
	}
	return fmt.Errorf("another run is working in %s right now (started %s, process %d). "+
		"Two runs in one repository would check out over each other, and neither could put back "+
		"the state it found. Wait for it to finish.\n\nIf it was killed and never finished, put "+
		"the directory back with:\n\n%s",
		repo, held.StartedAt, held.PID, recoveryCommands(repo, held))
}

// recoveryCommands is the by-hand version of Restore, printed for a run that
// died before it could run it.
func recoveryCommands(repo string, held inPlaceLock) string {
	var b strings.Builder
	if held.Branch != "" && held.Head != "" {
		fmt.Fprintf(&b, "    git -C %s update-ref refs/heads/%s %s\n", repo, held.Branch, held.Head)
		fmt.Fprintf(&b, "    git -C %s checkout --force %s\n", repo, held.Branch)
	} else if held.Head != "" {
		fmt.Fprintf(&b, "    git -C %s checkout --force %s\n", repo, held.Head)
	} else {
		return "    (the lock does not name a commit to return to; inspect " + repo + " by hand)"
	}
	fmt.Fprintf(&b, "    git -C %s clean -ffd\n", repo)
	if held.Stash != "" {
		fmt.Fprintf(&b, "    git -C %s stash apply --index %s\n", repo, held.Stash)
		fmt.Fprintf(&b, "    git -C %s update-ref -d %s\n", repo, StashRef)
		fmt.Fprintf(&b, "    rm %s\n", filepath.Join(repo, ".git", inPlaceLockDir, inPlaceLockName))
		b.WriteString("\nThe clean discards what the interrupted agent left in the directory. Your own " +
			"uncommitted work is not lost by it: the run set it aside in commit " + shortSHA(held.Stash) +
			" before it started, and the apply after the clean brings it back.")
		return b.String()
	}
	b.WriteString("\nThe last command discards everything in the directory that was never committed, " +
		"so run it only once you are sure there is nothing there you want.")
	return b.String()
}

// processAlive reports whether a pid is still running.
//
// It is a hint and not a fact. A pid can be reused by an unrelated process, and
// on a platform that cannot be asked the answer is always no. Both readings lead
// to the same instruction -- the lock is never stolen automatically -- so a
// wrong answer here changes the wording and nothing else.
func processAlive(pid int) bool {
	if pid <= 0 || runtime.GOOS == "windows" {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	// EPERM means the process is there and belongs to someone else, which is
	// still a process holding the lock.
	return err == nil || errors.Is(err, syscall.EPERM)
}

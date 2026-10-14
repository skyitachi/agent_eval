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
	return fmt.Errorf("%s has uncommitted changes, so this run cannot %s it: %s%s. "+
		"A run checks the repository out at the commit the task names and puts it back afterwards, "+
		"and uncommitted changes are the one thing that cannot be put back. Commit or stash them "+
		"first, or run this task against a copy",
		repo, what, strings.Join(shown, ", "), more)
}

// AcquireInPlace takes the repository for one run and records what it needs to
// put it back.
//
// The lock is not a courtesy: two runs in one repository would check out over
// each other and each would put back a state neither of them started from. It
// is taken exclusively and never waited for, so a second run says so instead of
// hanging.
func AcquireInPlace(ctx context.Context, repo string) (*InPlace, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	if err := CheckInPlace(ctx, abs); err != nil {
		return nil, err
	}

	s := &InPlace{Repo: abs}
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
	s.ignored = s.ignoredPaths(ctx)
	s.refs = s.refSnapshot(ctx)
	if _, err := os.Stat(filepath.Join(abs, HarnessDir)); err == nil {
		s.hadHarnessDir = true
	}

	blob, _ := json.Marshal(inPlaceLock{
		Repo: abs, Head: s.Head, Branch: s.Branch,
		PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339), Tool: "aeval",
	})
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

	// Asked again under the lock: the answer that matters is the one the
	// checkout will see, not the one the pre-flight saw.
	if err := requireCleanTree(ctx, abs, "work in"); err != nil {
		_ = os.Remove(s.lockPath)
		return nil, err
	}
	return s, nil
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
	// A person may have edited the tree while the previous chain ran, and
	// --force would throw that away without saying so.
	if err := requireCleanTree(ctx, s.Repo, "check out "+shortSHA(ref)+" in"); err != nil {
		return err
	}
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
		if !s.ignored[p] {
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
		return fmt.Errorf("a run was working in %s and did not finish (started %s, process %d, "+
			"which is no longer running), so it was interrupted before it could put the directory "+
			"back.\n\nPut it back with:\n\n%s",
			repo, held.StartedAt, held.PID, recoveryCommands(repo, held))
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

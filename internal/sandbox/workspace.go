package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/startstate"
)

// Materialize builds the initial workspace described by a dataset spec.
//
// Order matters and is fixed:
//
//  1. base state      (empty | inline files + assets | git clone)
//  2. baseline commit (so the agent's changes are diffable)
//  3. setup commands  (may need a working tree to build in)
//
// For git workspaces the upstream history is dropped by default and replaced by
// a single baseline commit. That is deliberate: if the clone retained its
// history, an agent could locate the upstream fix commit and read the answer.
func Materialize(ctx context.Context, sb Sandbox, ws dataset.Workspace, datasetDir string) error {
	switch ws.Type {
	case dataset.WorkspaceInplace:
		// An inplace replay builds nothing, so it gets no baseline commit
		// either. The directory is somebody's own working tree: committing to
		// it would change the thing being measured, and stripping its history
		// would destroy work that has nothing to do with this measurement. What
		// defines its starting state is what the workspace names -- a commit, or
		// a snapshot of the contents -- and checkInplace is what holds the
		// directory to it.
		return materializeInplace(ctx, sb, ws)
	case dataset.WorkspaceEmpty:
		// nothing to do
	case dataset.WorkspaceFiles:
		if err := materializeFiles(sb, ws, datasetDir); err != nil {
			return err
		}
	case dataset.WorkspaceGit:
		if err := materializeGit(ctx, sb, ws, datasetDir); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown workspace type %q", ws.Type)
	}

	if _, err := InitBaseline(ctx, sb); err != nil {
		return fmt.Errorf("create baseline commit: %w", err)
	}

	if len(ws.Setup) > 0 {
		if err := runSetup(ctx, sb, ws); err != nil {
			return err
		}
		// Re-snapshot after setup so build artifacts produced by setup
		// (dependency installs, generated code) are not attributed to the agent.
		if _, err := Rebaseline(ctx, sb); err != nil {
			return fmt.Errorf("re-snapshot after setup: %w", err)
		}
	}
	return nil
}

func materializeFiles(sb Sandbox, ws dataset.Workspace, datasetDir string) error {
	if ws.Assets != "" {
		src := ws.Assets
		if !filepath.IsAbs(src) {
			src = filepath.Join(datasetDir, src)
		}
		dst := sb.Root()
		if ws.AssetsTarget != "" {
			dst = joinClean(sb.Root(), ws.AssetsTarget)
		}
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("copy assets %s: %w", ws.Assets, err)
		}
	}
	for rel, content := range ws.Files {
		if err := sb.WriteFile(rel, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write inline file %s: %w", rel, err)
		}
	}
	return nil
}

func materializeGit(ctx context.Context, sb Sandbox, ws dataset.Workspace, datasetDir string) error {
	repo := ws.Repo
	if !looksLikeURL(repo) && !filepath.IsAbs(repo) {
		repo = filepath.Join(datasetDir, repo)
	}
	// Clone elsewhere first so a Subdir can be promoted to the sandbox root and
	// the same on-disk layout works for both backends.
	stage, err := os.MkdirTemp("", "agenteval-clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	cloneInto := filepath.Join(stage, "repo")
	args := []string{"clone", "--quiet", "--no-tags"}
	if ws.Ref != "" {
		// A branch/tag ref can be fetched shallowly; a raw SHA may not be, so
		// fall back to a full clone in that case.
		if isHexSHA(ws.Ref) {
			args = append(args, repo, cloneInto)
		} else {
			args = append(args, "--depth", "1", "--branch", ws.Ref, repo, cloneInto)
		}
	} else {
		args = append(args, "--depth", "1", repo, cloneInto)
	}
	if out, err := runHost(ctx, "", args...); err != nil {
		return fmt.Errorf("git clone %s: %s", repo, firstLine(out))
	}
	if ws.Ref != "" && isHexSHA(ws.Ref) {
		if out, err := runHost(ctx, cloneInto, "checkout", "--quiet", ws.Ref); err != nil {
			return fmt.Errorf("git checkout %s: %s", ws.Ref, firstLine(out))
		}
	}
	// A clone of a repository with no commits succeeds and leaves nothing
	// behind, so the failure mode is not an error here but a silently empty
	// workspace three layers away -- where it reads as an agent that had
	// nothing to work on. A ref that does not exist fails differently and
	// loudly, so this is about the repository, not the checkout.
	if out, err := runHost(ctx, cloneInto, "rev-parse", "--verify", "HEAD"); err != nil {
		where := "its default branch"
		if ws.Ref != "" {
			where = ws.Ref
		}
		return fmt.Errorf("workspace repo %s has no commit at %s, so the workspace would be empty "+
			"(cloning a repository with no commits succeeds and carries nothing): %s",
			repo, where, firstLine(out))
	}

	src := cloneInto
	if ws.Subdir != "" {
		src = joinClean(cloneInto, ws.Subdir)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("workspace.subdir %q not found in %s", ws.Subdir, repo)
		}
	}

	keepHistory := ws.KeepGitHistory != nil && *ws.KeepGitHistory
	if keepHistory {
		if err := copyDir(src, sb.Root()); err != nil {
			return err
		}
		if src != cloneInto {
			// A promoted subdirectory needs the repository root for git to work.
			if err := copyDir(filepath.Join(cloneInto, ".git"), filepath.Join(sb.Root(), ".git")); err != nil {
				return fmt.Errorf("carry .git into workspace: %w", err)
			}
		}
		return nil
	}

	if err := copyDir(src, sb.Root()); err != nil {
		return err
	}
	// Drop upstream history so the fix cannot be read out of the repository.
	if err := os.RemoveAll(filepath.Join(sb.Root(), ".git")); err != nil {
		return fmt.Errorf("strip upstream .git: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inplace workspaces
// ---------------------------------------------------------------------------

// InplaceDriftError reports a directory that has moved away from the state a
// replay is supposed to start from.
//
// The list of differences is the useful part, which is why this is a type rather
// than a formatted string: "the directory changed" sends a reader looking, while
// "these files changed and HEAD is not the commit you recorded" tells them what
// to do about it. Nothing runs when this is returned -- the design's constraint
// is that a replay whose starting point has moved is not run at all, because the
// difference would be invisible in the result.
type InplaceDriftError struct {
	// Dir is the directory that no longer matches.
	Dir string
	// Drift is what changed.
	Drift *startstate.Drift
	// Kind is the record's strategy, which decides what the remedy is.
	Kind string
}

func (e *InplaceDriftError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s no longer matches the state this replay starts from:", e.Dir)
	for _, line := range e.Drift.Lines() {
		b.WriteString("\n  " + line)
	}
	if e.Kind == string(startstate.KindGit) {
		b.WriteString("\ncommit or stash those changes, then run again")
	} else {
		b.WriteString("\nput the directory back the way its snapshot has it, then run again")
	}
	return b.String()
}

// materializeInplace checks that the directory is where the recording started,
// and gives a non-git one something to be diffed against.
//
// Nothing is copied and nothing is written into the directory: the runner has
// already pointed the sandbox at it, so the agent will work where the session
// worked, which is the whole reason this type exists. The check is therefore not
// a formality. Without it a replay of a directory that has moved measures the
// difference between two starting points and reports it as the agent's.
func materializeInplace(ctx context.Context, sb Sandbox, ws dataset.Workspace) error {
	if info, err := os.Stat(ws.Dir); err != nil {
		return fmt.Errorf("inplace workspace %s is not there: %w", ws.Dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("inplace workspace %s is not a directory", ws.Dir)
	}
	// The sandbox must be rooted exactly at the directory the task names. If it
	// is not, the agent would run somewhere else while the task said otherwise,
	// and every later reading of the result would be wrong.
	root, err := filepath.Abs(sb.Root())
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(ws.Dir)
	if err != nil {
		return err
	}
	if root != dir {
		return fmt.Errorf("inplace workspace is %s but the sandbox is rooted at %s; an inplace "+
			"replay runs in the directory itself, so those have to be the same directory",
			dir, root)
	}

	rec, err := recordFor(ws)
	if err != nil {
		return err
	}
	drift, err := startstate.Verify(rec, ctx)
	if err != nil {
		return err
	}
	if !drift.Clean() {
		return &InplaceDriftError{Dir: dir, Drift: drift, Kind: string(rec.Kind)}
	}

	if rec.Kind == startstate.KindDir {
		return buildDirBaseline(ctx, ws)
	}
	return nil
}

// recordFor turns a workspace back into the start state it claims to start from.
//
// The dataset carries the record's fields rather than a path to the record file,
// so a task says what it needs without depending on a home directory having
// survived since it was collected. That is the same trade assets already make:
// not portable, and honest about it.
func recordFor(ws dataset.Workspace) (*startstate.Record, error) {
	switch {
	case ws.Ref != "":
		return &startstate.Record{
			SessionID: ws.Session,
			Cwd:       ws.Dir,
			Kind:      startstate.KindGit,
			Commit:    ws.Ref,
		}, nil
	case ws.Snapshot != "":
		return &startstate.Record{
			SessionID: ws.Session,
			Cwd:       ws.Dir,
			Kind:      startstate.KindDir,
			Snapshot:  ws.Snapshot,
		}, nil
	default:
		return nil, fmt.Errorf("inplace workspace %s names neither a ref nor a snapshot, so "+
			"nothing says where the replay starts", ws.Dir)
	}
}

// buildDirBaseline gives a non-git inplace replay something to be diffed against.
//
// The directory cannot hold the repository -- writing a .git into somebody's
// working tree is exactly what this workspace type exists to avoid -- so the
// repository is built elsewhere and git is told to treat the directory as its
// work tree. What it commits is the snapshot, which is what makes the later diff
// mean "what the agent did" rather than "how the directory differs from a
// repository it was never in".
func buildDirBaseline(ctx context.Context, ws dataset.Workspace) error {
	gitDir, err := startstate.BaselineDir(ws.Session)
	if err != nil {
		return err
	}
	if baselineCommitsTheSnapshot(ctx, gitDir, ws.Snapshot) {
		// Built once. Rebuilding a baseline that already commits the snapshot
		// would be wasted work, and -- worse -- it would move the SHA that an
		// earlier attempt was measured against out from under it.
		return nil
	}
	if err := os.RemoveAll(gitDir); err != nil {
		return fmt.Errorf("clear stale baseline in %s: %w", gitDir, err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		return fmt.Errorf("create baseline in %s: %w", gitDir, err)
	}
	// While the commit is being made, the snapshot is the work tree.
	env := []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + ws.Snapshot}
	if out, err := runHostEnv(ctx, "", env, "init", "--quiet"); err != nil {
		return fmt.Errorf("create baseline repository: %s", firstLine(out))
	}
	if out, err := runHostEnv(ctx, "", env, "add", "-A"); err != nil {
		return fmt.Errorf("baseline add: %s", firstLine(out))
	}
	msg := "agenteval baseline: the contents of " + ws.Dir + " before the recorded session"
	if out, err := runHostEnv(ctx, "", env, "commit", "--quiet", "--allow-empty", "-m", msg); err != nil {
		return fmt.Errorf("baseline commit: %s", firstLine(out))
	}
	return nil
}

// baselineCommitsTheSnapshot reports whether the baseline repository at gitDir
// still holds exactly the snapshot's contents.
//
// A session can be recorded twice -- the hook runs at the start of every session
// with that id -- and the second recording replaces the snapshot. Keeping the
// baseline built from the first one would leave the replay diffing against a
// starting state that is not the one it verified against, which is precisely the
// silent wrong answer this whole workspace type exists to avoid. Asking git
// whether the work tree is clean, with the snapshot as the work tree, is a cheap
// way to ask whether the two still agree.
func baselineCommitsTheSnapshot(ctx context.Context, gitDir, snapshot string) bool {
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return false
	}
	if _, err := os.Stat(snapshot); err != nil {
		return false
	}
	out, err := runHostEnv(ctx, "", []string{
		"GIT_DIR=" + gitDir,
		"GIT_WORK_TREE=" + snapshot,
	}, "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == ""
}

// InplaceBaseline returns the baseline an inplace workspace is measured against.
//
// For a git workspace that is the recorded commit, taken from the workspace
// rather than from HEAD: the point of pinning a commit is that the answer does
// not depend on what the directory has been doing since. For a non-git one it is
// the commit buildDirBaseline made out of the snapshot, together with the
// repository holding it.
func InplaceBaseline(ctx context.Context, ws dataset.Workspace) (*Baseline, error) {
	switch {
	case ws.Ref != "":
		return &Baseline{SHA: ws.Ref, Root: ws.Dir}, nil
	case ws.Snapshot != "":
		gitDir, err := startstate.BaselineDir(ws.Session)
		if err != nil {
			return nil, err
		}
		sha, err := baselineSHA(ctx, gitDir)
		if err != nil {
			return nil, err
		}
		return &Baseline{SHA: sha, Root: ws.Dir, GitDir: gitDir}, nil
	default:
		return nil, fmt.Errorf("inplace workspace %s names neither a ref nor a snapshot", ws.Dir)
	}
}

// RollbackInplace puts an inplace workspace back to the state a replay started
// from.
//
// The design calls for the directory to go back and for the patch to be kept,
// and the order matters: the caller diffs first, then calls this. Both kinds of
// workspace behave the same way, and the reason is that both are somebody's
// directory rather than the harness's: files the attempt created are moved into
// the session's discard directory instead of deleted, and everything else is put
// back. Nothing is left behind in the repository -- no stash entry, no staged
// addition -- because the directory is the user's, and a replay that has
// finished has no business holding onto it.
//
// A file the attempt modified is reverted rather than copied aside: its content
// is in the patch, which the caller took before calling this.
//
// The check that runs before an inplace cell makes this safe: the directory was
// verified clean, so every change present now is one this attempt made, and
// putting the directory back cannot be confused with destroying something that
// was already there.
func RollbackInplace(ctx context.Context, ws dataset.Workspace) error {
	switch {
	case ws.Ref != "":
		return rollbackGit(ctx, ws)
	case ws.Snapshot != "":
		return rollbackDir(ws)
	default:
		return fmt.Errorf("inplace workspace %s names neither a ref nor a snapshot, so there is "+
			"nothing to roll it back to", ws.Dir)
	}
}

func rollbackGit(ctx context.Context, ws dataset.Workspace) error {
	// Drop the intent-to-add markers the diff left in the index first.
	//
	// ChangedPaths runs `git add -A -N` so that untracked files appear in the
	// diff, which leaves the index holding an entry for every file the attempt
	// created. Until those are cleared the file counts as a tracked empty one:
	// `git ls-files --others` will not report it, so it would survive the
	// rollback as a staged addition, and `git stash push` refuses the tree
	// outright ("Entry 'handoff.txt' not uptodate. Cannot merge."). Clearing
	// them turns the entries back into the untracked files they are.
	if out, err := runHost(ctx, ws.Dir, "reset", "--quiet"); err != nil {
		return fmt.Errorf("clear the index in %s before rolling back: %s", ws.Dir, firstLine(out))
	}

	// Move aside what the attempt created, before the reset below can take it
	// away -- and before it turns a file the attempt committed into one the
	// reset silently drops.
	created, err := createdPaths(ctx, ws)
	if err != nil {
		return err
	}
	if err := quarantineCreated(ws, created); err != nil {
		return err
	}

	// An agent is allowed to commit, and a commit is part of what it left
	// behind, so HEAD goes back too. The commit stays reachable through the
	// reflog until git decides otherwise.
	if out, err := runHost(ctx, ws.Dir, "reset", "--hard", "--quiet", ws.Ref); err != nil {
		return fmt.Errorf("reset %s to %s: %s", ws.Dir, ws.Ref, firstLine(out))
	}
	return nil
}

// createdPaths lists files an attempt left in an inplace git directory that the
// recorded commit did not have.
//
// Two questions are being asked rather than one. An attempt that wrote a file
// and left it untracked is the common case. An attempt that committed its work
// is the other: those files are tracked now, so they are not "untracked", and a
// reset to the recorded commit is what would remove them -- which is exactly why
// they have to be found before it runs.
//
// Files git is told to ignore are deliberately not listed. They are invisible to
// the cleanliness check that admitted this directory, so there is no way to tell
// one the attempt created from one that was already sitting there when the
// replay arrived, and moving somebody's build output aside on a guess is worse
// than leaving it. The same rule already decides what the diff can see.
func createdPaths(ctx context.Context, ws dataset.Workspace) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	addAll := func(text string) {
		for _, line := range strings.Split(text, "\n") {
			rel := strings.TrimSpace(line)
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}

	out1, err := runHost(ctx, ws.Dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("list what the attempt created in %s: %s", ws.Dir, firstLine(out1))
	}
	addAll(out1)

	// Compared against the recorded commit rather than against HEAD: an attempt
	// that committed is the case this second question exists for.
	out2, err := runHost(ctx, ws.Dir, "diff", "--name-only", "--no-renames", "--diff-filter=A", ws.Ref)
	if err != nil {
		return nil, fmt.Errorf("list what the attempt added in %s: %s", ws.Dir, firstLine(out2))
	}
	addAll(out2)

	sort.Strings(out)
	return out, nil
}

// quarantineCreated moves created files into the session's discard directory.
//
// Directories the attempt made are left behind, empty when everything in them
// moved: git does not track directories, so one the attempt created cannot be
// told from one that was already there and empty, and a rollback does not get to
// walk a tree removing things wholesale.
func quarantineCreated(ws dataset.Workspace, created []string) error {
	for _, rel := range created {
		src := filepath.Join(ws.Dir, filepath.FromSlash(rel))
		if _, err := os.Lstat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Already gone: nothing to preserve, and not an error.
				continue
			}
			return err
		}
		dst, err := startstate.QuarantinePath(ws.Session, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := moveAside(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// moveAside relocates one created path into the discard directory.
//
// A plain rename would do when both sides are on the same filesystem, and often
// they are not: the discard directory is under the user's home, while the
// directory a recording ran in can be anywhere -- another volume, a mounted
// share. Falling back to a copy is what keeps the rollback from stopping
// halfway through with some of the attempt's files already moved.
func moveAside(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	case info.Mode().IsRegular():
		if err := copyFile(src, dst); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot move %s aside: it is neither a file nor a symlink", src)
	}
	return os.Remove(src)
}

func rollbackDir(ws dataset.Workspace) error {
	_, err := startstate.Restore(&startstate.Record{
		SessionID: ws.Session,
		Cwd:       ws.Dir,
		Kind:      startstate.KindDir,
		Snapshot:  ws.Snapshot,
	})
	if err != nil {
		return fmt.Errorf("restore %s from its snapshot: %w", ws.Dir, err)
	}
	return nil
}

// baselineSHA reads the commit buildDirBaseline wrote, and doubles as the way to
// ask whether one was written at all.
func baselineSHA(ctx context.Context, gitDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return "", fmt.Errorf("no baseline repository in %s: %w", gitDir, err)
	}
	out, err := runHostEnv(ctx, "", []string{"GIT_DIR=" + gitDir}, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read baseline commit in %s: %s", gitDir, firstLine(out))
	}
	return strings.TrimSpace(out), nil
}

func runSetup(ctx context.Context, sb Sandbox, ws dataset.Workspace) error {
	for i, c := range ws.Setup {
		opts := ExecOptions{
			Cwd: c.Cwd,
			Env: envMap(ws.Env),
		}
		if c.TimeoutS > 0 {
			opts.Timeout = time.Duration(c.TimeoutS) * time.Second
		}
		res, err := sb.Exec(ctx, c.Run, opts)
		if err != nil {
			return fmt.Errorf("setup[%d] %q: %w", i, c.Run, err)
		}
		if !res.Success() && !c.Optional {
			return fmt.Errorf("setup[%d] %q failed (exit %d):\n%s",
				i, c.Run, res.ExitCode, truncate(firstLines(res.CombinedText(), 20), 2000))
		}
	}
	return nil
}

func envMap(vars []dataset.EnvVar) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

// ---------------------------------------------------------------------------
// Git baseline and diffing
// ---------------------------------------------------------------------------

// Baseline records the commit the agent's work is measured against.
type Baseline struct {
	// SHA is the baseline commit.
	SHA string
	// Root is the host workspace path.
	Root string
	// GitDir is the repository to read instead of Root/.git, when there is one.
	//
	// An inplace replay of a directory that is not a repository needs this: git
	// can diff a work tree it does not own through GIT_DIR and GIT_WORK_TREE,
	// which is how a snapshot becomes a baseline without a .git being written
	// into the directory being measured.
	GitDir string
}

// git runs a git command against this baseline.
//
// Everything that reads or diffs the workspace goes through here rather than
// through runHost with b.Root, because for an inplace non-git workspace the
// repository is somewhere else entirely and the directory is only a work tree.
func (b *Baseline) git(ctx context.Context, args ...string) (string, error) {
	if b.GitDir == "" {
		return runHost(ctx, b.Root, args...)
	}
	return runHostEnv(ctx, b.Root, []string{
		"GIT_DIR=" + b.GitDir,
		"GIT_WORK_TREE=" + b.Root,
	}, args...)
}

// InitBaseline initializes (or reuses) a git repository and commits the current
// state. It is safe to call when a repository already exists.
func InitBaseline(ctx context.Context, sb Sandbox) (*Baseline, error) {
	root := sb.Root()
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git is required to diff agent changes")
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		if out, err := runHost(ctx, root, "init", "--quiet"); err != nil {
			return nil, fmt.Errorf("git init: %s", firstLine(out))
		}
	}
	if out, err := runHost(ctx, root, "add", "-A"); err != nil {
		return nil, fmt.Errorf("git add: %s", firstLine(out))
	}
	if out, err := runHost(ctx, root, "commit", "--quiet", "--allow-empty", "-m", "agenteval baseline"); err != nil {
		// An empty commit can still fail on exotic configurations; surfacing the
		// real message beats a generic error.
		return nil, fmt.Errorf("git commit baseline: %s", firstLine(out))
	}
	sha, err := revParse(ctx, root)
	if err != nil {
		return nil, err
	}
	return &Baseline{SHA: sha, Root: root}, nil
}

// Rebaseline commits the current state again, producing a fresh baseline. Used
// after setup commands so their side effects are not charged to the agent.
func Rebaseline(ctx context.Context, sb Sandbox) (*Baseline, error) {
	return InitBaseline(ctx, sb)
}

// CurrentBaseline returns the baseline commit without creating one.
//
// Materialize already establishes the baseline, including a re-snapshot after
// setup commands, so a caller that wants to diff must read it rather than call
// InitBaseline again: that would append an empty commit and move the SHA away
// from the tree the agent actually started from.
func CurrentBaseline(ctx context.Context, sb Sandbox) (*Baseline, error) {
	root := sb.Root()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf("no git repository in %s: call Materialize first", root)
	}
	sha, err := revParse(ctx, root)
	if err != nil {
		return nil, err
	}
	return &Baseline{SHA: sha, Root: root}, nil
}

func revParse(ctx context.Context, root string) (string, error) {
	out, err := runHost(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %s", firstLine(out))
	}
	return strings.TrimSpace(out), nil
}

// DiffOptions filters the produced patch.
type DiffOptions struct {
	// Paths limits the diff to matching globs. Empty means all paths.
	Paths []string
	// IgnorePaths drops matching paths from the diff.
	IgnorePaths []string
	// ContextLines sets unified diff context. Zero uses git's default (3).
	ContextLines int
}

// ChangedPaths lists files that differ from the baseline, including untracked
// ones, after applying the filters in opts.
func (b *Baseline) ChangedPaths(ctx context.Context, opts DiffOptions) ([]string, error) {
	if out, err := b.git(ctx, "add", "-A", "-N"); err != nil {
		return nil, fmt.Errorf("git add -N: %s", firstLine(out))
	}
	out, err := b.git(ctx,
		"diff", "--name-only", "--no-renames", b.SHA, "--", ".")
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %s", firstLine(out))
	}
	var all []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			all = append(all, filepath.ToSlash(s))
		}
	}
	return FilterPaths(all, opts.Paths, opts.IgnorePaths), nil
}

// Diff returns a unified diff of the workspace against the baseline.
func (b *Baseline) Diff(ctx context.Context, opts DiffOptions) (string, error) {
	paths, err := b.ChangedPaths(ctx, opts)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	args := []string{"diff", "--no-color", "--no-renames"}
	if opts.ContextLines > 0 {
		args = append(args, fmt.Sprintf("--unified=%d", opts.ContextLines))
	}
	args = append(args, b.SHA, "--")
	args = append(args, paths...)
	out, err := b.git(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git diff: %s", firstLine(out))
	}
	return out, nil
}

// DiffStat returns "path | +adds -dels" lines for the changed files.
func (b *Baseline) DiffStat(ctx context.Context, opts DiffOptions) (string, error) {
	paths, err := b.ChangedPaths(ctx, opts)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	args := append([]string{"diff", "--numstat", b.SHA, "--"}, paths...)
	out, err := b.git(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git diff --numstat: %s", firstLine(out))
	}
	return out, nil
}

// RestorePaths puts every path matching one of the globs back into its baseline
// state and reports what it touched.
//
// Restoring means three things, and forgetting the third is the usual bug:
//
//  1. a file the agent edited gets the baseline content back;
//  2. a file the agent deleted comes back;
//  3. a file the agent created, which did not exist at the baseline, is removed
//     -- "restore" has to mean "the path is in its baseline state", and a file
//     that was not there is not in its baseline state while it exists.
//
// Everything goes through git against the host path, which is the same tree both
// backends operate on (docker bind-mounts it), so the result does not depend on
// which backend produced the workspace.
//
// The intended use is the graded test suite: an attempt measured after its own
// edits to the tests were undone is a measurement of capability rather than of
// compliance, and under RL it is also the difference between a reward that
// rewards solving the task and a reward that rewards weakening the assertion.
func (b *Baseline) RestorePaths(ctx context.Context, globs []string) (restored, removed []string, err error) {
	if len(globs) == 0 {
		return nil, nil, nil
	}
	tracked, err := b.baselineFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	changed, err := b.ChangedPaths(ctx, DiffOptions{})
	if err != nil {
		return nil, nil, err
	}
	for _, p := range FilterPaths(changed, globs, nil) {
		if _, ok := tracked[p]; ok {
			if out, cerr := b.git(ctx, "checkout", b.SHA, "--", p); cerr != nil {
				return restored, removed, fmt.Errorf("restore %s: %s", p, firstLine(out))
			}
			restored = append(restored, p)
			continue
		}
		// Created after the baseline: remove it and drop the index entry the
		// changed-path probe created, so the path stops being visible to any
		// later diff.
		abs := filepath.Join(b.Root, filepath.FromSlash(p))
		if rerr := os.Remove(abs); rerr != nil && !os.IsNotExist(rerr) {
			return restored, removed, fmt.Errorf("remove %s: %w", p, rerr)
		}
		if out, rerr := b.git(ctx, "rm", "-f", "--cached", "--ignore-unmatch", "--", p); rerr != nil {
			return restored, removed, fmt.Errorf("unstage %s: %s", p, firstLine(out))
		}
		removed = append(removed, p)
	}
	return restored, removed, nil
}

// baselineFiles lists the paths that exist in the baseline commit.
func (b *Baseline) baselineFiles(ctx context.Context) (map[string]struct{}, error) {
	out, err := b.git(ctx, "ls-tree", "-r", "--name-only", b.SHA)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %s", firstLine(out))
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			set[filepath.ToSlash(s)] = struct{}{}
		}
	}
	return set, nil
}

func (b *Baseline) String() string { return "baseline@" + shortSHA(b.SHA) }

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// runHost executes git on the host, independent of the sandbox backend.
//
// The -c flags pin behaviour that would otherwise depend on the machine's git
// configuration, which would make scores non-reproducible:
//
//	safe.directory=*      the container may have written files as another uid
//	core.quotepath=false  keep non-ASCII (e.g. Chinese) filenames literal
//	commit.gpgsign=false  a signing hook must not fail the baseline commit
//	core.autocrlf=false   no line-ending rewriting between backends
//	gc.auto=0             no background repack during a run
//	maintenance.auto=false and core.fsmonitor=false likewise keep git from
//	                    spawning helpers that mutate .git while the workspace is
//	                    being torn down or diffed
//
// The environment is sanitised as well, which matters more than the flags: a
// developer's global config is an uncontrolled input to the evaluation. It can
// install hooks that rewrite the baseline commit, override the diff driver, or
// -- as one real tool does -- attach `trace2.eventtarget` to a socket so a
// background daemon writes into .git/ai/ after every git command and races with
// workspace cleanup. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are therefore
// pointed at the null device, so a run depends only on the flags above and on
// the repository's own config.
func runHost(ctx context.Context, dir string, args ...string) (string, error) {
	return runHostEnv(ctx, dir, nil, args...)
}

// runHostEnv runs git with extra environment entries.
//
// It exists for the inplace case, where the repository git must read is not the
// directory it is standing in. GIT_DIR and GIT_WORK_TREE are how git is told
// about a work tree it does not own, which is what lets a directory be diffed
// without a .git being written into it -- the one thing an inplace replay must
// not do to somebody's working directory.
func runHostEnv(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	base := []string{
		"-c", "safe.directory=*",
		"-c", "core.quotepath=false",
		"-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false",
		"-c", "core.fsmonitor=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "user.name=agenteval",
		"-c", "user.email=eval@agenteval.local",
	}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		// A clone must fail fast rather than block on a credential prompt.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

// CopyDirInto copies the *contents* of a host directory into a subdirectory of a
// sandbox workspace, creating the destination when needed.
//
// This is how graders inject hidden test suites after the agent has finished, so
// the agent can neither read nor modify them during the attempt.
func CopyDirInto(srcDir, workspaceRoot, target string) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}
	dst := workspaceRoot
	if strings.TrimSpace(target) != "" {
		dst = joinClean(workspaceRoot, target)
	}
	return copyDir(srcDir, dst)
}

// copyDir recursively copies src into dst, preserving executable bits.
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}
		if !d.Type().IsRegular() {
			return nil // skip sockets, devices, fifos
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func looksLikeURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@")
}

func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... (%d more lines)", len(lines)-n)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

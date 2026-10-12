package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
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
	if out, err := runHost(ctx, b.Root, "add", "-A", "-N"); err != nil {
		return nil, fmt.Errorf("git add -N: %s", firstLine(out))
	}
	out, err := runHost(ctx, b.Root,
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
	out, err := runHost(ctx, b.Root, args...)
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
	out, err := runHost(ctx, b.Root, args...)
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
			if out, cerr := runHost(ctx, b.Root, "checkout", b.SHA, "--", p); cerr != nil {
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
		if out, rerr := runHost(ctx, b.Root, "rm", "-f", "--cached", "--ignore-unmatch", "--", p); rerr != nil {
			return restored, removed, fmt.Errorf("unstage %s: %s", p, firstLine(out))
		}
		removed = append(removed, p)
	}
	return restored, removed, nil
}

// baselineFiles lists the paths that exist in the baseline commit.
func (b *Baseline) baselineFiles(ctx context.Context) (map[string]struct{}, error) {
	out, err := runHost(ctx, b.Root, "ls-tree", "-r", "--name-only", b.SHA)
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

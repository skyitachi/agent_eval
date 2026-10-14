// Package startstate records what a directory looked like before an agent
// session started, so that a replay can begin from the same place.
//
// A transcript is not enough to reconstruct that place. It names the directory a
// session ran in and the branch it was on, but not the commit; and a session
// that ran outside a repository leaves nothing behind but a path, while the
// directory itself has by then been changed by the very session being replayed.
// Both facts are needed for the replay to be honest about what it measures, and
// neither survives until collect runs -- so they are captured before the session
// starts, by a hook, and looked up afterwards by session id.
//
// The two cases this package distinguishes are the two documents/isolation.md
// names:
//
//   - a git directory is reproduced by pinning a commit. The commit is what the
//     tree is; a working tree that was already dirty when the session started is
//     recorded in the notes rather than reproduced, because nobody captured
//     those changes and a note saying so beats a clean tree pretending they
//     never existed.
//   - a directory that is not a repository is reproduced by its contents, so the
//     contents are copied out while they are still the ones the session started
//     from.
//
// Nothing here decides whether a replay is allowed to proceed. Verify reports
// how far the directory has drifted from its record; the caller decides what to
// do about it.
package startstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EnvHome overrides where records and snapshots are kept. Tests set it to a
// temporary directory; a normal run leaves it alone and gets ~/.aeval.
const EnvHome = "AEVAL_HOME"

// Kind says which of the two reproduction strategies a record describes.
type Kind string

const (
	// KindGit means the directory was inside a git work tree, and the commit is
	// what defines its starting state.
	KindGit Kind = "git"
	// KindDir means the directory was not a repository, so its contents are the
	// only description of its starting state.
	KindDir Kind = "dir"
)

// DefaultMaxSnapshotBytes caps how much of a non-git directory is copied out.
// A snapshot is a copy of somebody's working directory, and those are not
// always small: failing loudly at a bound beats silently filling a disk.
const DefaultMaxSnapshotBytes int64 = 512 << 20

// Record is the start state captured for one session.
type Record struct {
	// SessionID is the session the state belongs to. It is the join key: a
	// collected task carries the same id, which is how a replay finds the
	// directory state its recording started from.
	SessionID string `json:"session_id"`
	// Cwd is the absolute path the session ran in.
	Cwd string `json:"cwd"`
	// Kind is "git" or "dir".
	Kind Kind `json:"kind"`

	// Commit is HEAD at capture time. For a repository this is what "the same
	// environment" means.
	Commit string `json:"commit,omitempty"`
	// Branch is the checked-out branch, kept because a detached HEAD is worth
	// being able to see after the fact.
	Branch string `json:"branch,omitempty"`
	// Dirty lists working-tree changes that already existed when the session
	// started, as `git status --porcelain` lines -- status codes included, so a
	// reader can tell a modified file from an untracked one.
	Dirty []string `json:"dirty,omitempty"`

	// Snapshot is the absolute path of the copied-out contents (non-git only).
	Snapshot string `json:"snapshot,omitempty"`
	// Files counts what the snapshot holds (non-git only).
	Files int `json:"files,omitempty"`
	// Bytes is the snapshot size (non-git only).
	Bytes int64 `json:"bytes,omitempty"`

	// RecordedAt is when the state was captured.
	RecordedAt time.Time `json:"recorded_at"`
	// ToolVersion records what wrote this.
	ToolVersion string `json:"tool_version,omitempty"`
}

// Options tunes how a record is written.
type Options struct {
	// MaxSnapshotBytes caps a directory snapshot. Zero means
	// DefaultMaxSnapshotBytes.
	MaxSnapshotBytes int64
	// ToolVersion is recorded for provenance; empty leaves the field out.
	ToolVersion string
	// Now overrides the clock. Tests set it; callers leave it nil.
	Now func() time.Time
}

// Root returns the directory records and snapshots live under.
func Root() (string, error) {
	if v := os.Getenv(EnvHome); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory for %s: %w", EnvHome, err)
	}
	return filepath.Join(home, ".aeval"), nil
}

// Capture describes dir as it is right now.
//
// It does not write anything: what is captured and where it is stored are
// separate so that a caller which discovers it cannot store the state has not
// already paid for copying it.
func Capture(dir string) (*Record, error) {
	if dir == "" {
		return nil, errors.New("startstate: no directory to capture")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("capture %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("capture %s: not a directory", abs)
	}

	rec := &Record{Cwd: abs, RecordedAt: time.Now().UTC()}
	if !insideWorkTree(abs) {
		rec.Kind = KindDir
		return rec, nil
	}

	rec.Kind = KindGit
	// A failure here is not fatal to the capture: the record is still what tells
	// a replay where the session ran, and an empty commit reads as "not known"
	// rather than as a wrong answer.
	if out, err := gitOut(context.Background(), abs, "rev-parse", "HEAD"); err == nil {
		rec.Commit = strings.TrimSpace(out)
	}
	if out, err := gitOut(context.Background(), abs, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		rec.Branch = strings.TrimSpace(out)
	}
	if dirty, err := dirtyPaths(context.Background(), abs); err == nil {
		rec.Dirty = dirty
	}
	return rec, nil
}

// Save writes rec, copying a directory snapshot when the record is not a git
// one. The session id must already be set.
func Save(rec *Record, opts Options) error {
	if rec == nil {
		return errors.New("startstate: nothing to save")
	}
	if rec.SessionID == "" {
		return errors.New("startstate: a record needs a session id")
	}
	if opts.Now == nil {
		rec.RecordedAt = time.Now().UTC()
	} else {
		rec.RecordedAt = opts.Now().UTC()
	}
	rec.ToolVersion = opts.ToolVersion

	if rec.Kind == KindDir {
		max := opts.MaxSnapshotBytes
		if max == 0 {
			max = DefaultMaxSnapshotBytes
		}
		dest, err := SnapshotPath(rec.SessionID)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("clear stale snapshot %s: %w", dest, err)
		}
		tree := filepath.Join(dest, "tree")
		files, bytes, err := snapshotTree(rec.Cwd, tree, max)
		if err != nil {
			return err
		}
		rec.Snapshot = tree
		rec.Files = files
		rec.Bytes = bytes
	}

	path, err := recordPath(rec.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create record directory: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	b = append(b, '\n')
	// Written to a temporary file and renamed, so a hook that is killed partway
	// through cannot leave a half-written record that reads as a valid one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install record: %w", err)
	}
	return nil
}

// Read returns the record for a session. A session with no record is reported as
// (nil, false, nil) rather than as an error: recording is opt-in, and most
// sessions collected from an existing transcript will never have had a hook.
func Read(sessionID string) (*Record, bool, error) {
	if sessionID == "" {
		return nil, false, nil
	}
	path, err := recordPath(sessionID)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read record %s: %w", path, err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, false, fmt.Errorf("parse record %s: %w", path, err)
	}
	return &rec, true, nil
}

// Drift describes how a directory has moved away from its record.
type Drift struct {
	// Kind is the strategy the record describes.
	Kind Kind
	// Cwd is the directory the record names.
	Cwd string
	// Head is the commit the directory is on now (git only).
	Head string
	// Want is the commit that was recorded (git only).
	Want string
	// Dirty lists what is uncommitted now (git only).
	Dirty []string
	// Changed, Missing and Added describe a directory against its snapshot.
	Changed []string
	Missing []string
	Added   []string
}

// Clean reports whether the directory still matches its record.
func (d *Drift) Clean() bool {
	if d == nil {
		return true
	}
	return d.Head == d.Want &&
		len(d.Dirty) == 0 && len(d.Changed) == 0 &&
		len(d.Missing) == 0 && len(d.Added) == 0
}

// Lines renders the drift as the lines a caller can print. It is empty when the
// directory still matches, so a caller can decide to say nothing.
func (d *Drift) Lines() []string {
	if d == nil || d.Clean() {
		return nil
	}
	var out []string
	if d.Kind == KindGit {
		if len(d.Want) > 0 && d.Head != d.Want {
			out = append(out, fmt.Sprintf("HEAD is %s, the record says %s", shortSHA(d.Head), shortSHA(d.Want)))
		}
		if len(d.Dirty) > 0 {
			out = append(out, fmt.Sprintf("%d uncommitted change(s):", len(d.Dirty)))
			for _, line := range d.Dirty {
				out = append(out, "  "+line)
			}
		}
		return out
	}
	for _, group := range []struct {
		label string
		paths []string
	}{
		{"changed since the record", d.Changed},
		{"there at the start and gone now", d.Missing},
		{"not there at the start", d.Added},
	} {
		if len(group.paths) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%d file(s) %s:", len(group.paths), group.label))
		for _, p := range group.paths {
			out = append(out, "  "+p)
		}
	}
	return out
}

// Error renders the drift as one line, for embedding in another message.
func (d *Drift) Error() string {
	lines := d.Lines()
	if len(lines) == 0 {
		return "the directory matches its record"
	}
	return strings.Join(lines, "; ")
}

// Verify reports how far the directory has drifted from its record.
//
// This is the check the design document's run constraint names: a replay that
// starts from a directory which has moved is not starting from the place the
// recording started from, and the difference is invisible in the result unless
// somebody looks for it.
func Verify(rec *Record, ctx context.Context) (*Drift, error) {
	if rec == nil {
		return nil, errors.New("startstate: no record to verify against")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d := &Drift{Kind: rec.Kind, Cwd: rec.Cwd}
	if info, err := os.Stat(rec.Cwd); err != nil {
		return nil, fmt.Errorf("the recorded directory %s is not there: %w", rec.Cwd, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("the recorded path %s is not a directory", rec.Cwd)
	}

	switch rec.Kind {
	case KindGit:
		// A record with no commit is one Capture could not read HEAD for, and it
		// says so by leaving the field empty rather than by inventing a value.
		// There is nothing to compare against, so this is refused here instead
		// of being reported as drift: "the commit is not known" and "the commit
		// is not the one you recorded" call for different things.
		if rec.Commit == "" {
			return nil, fmt.Errorf("the record for session %s does not name a commit, so there is "+
				"nothing to compare %s against", rec.SessionID, rec.Cwd)
		}
		d.Want = rec.Commit
		if out, err := gitOut(ctx, rec.Cwd, "rev-parse", "HEAD"); err != nil {
			return nil, fmt.Errorf("read HEAD of %s: %w", rec.Cwd, err)
		} else {
			d.Head = strings.TrimSpace(out)
		}
		dirty, err := dirtyPaths(ctx, rec.Cwd)
		if err != nil {
			return nil, fmt.Errorf("read status of %s: %w", rec.Cwd, err)
		}
		d.Dirty = dirty
	case KindDir:
		if rec.Snapshot == "" {
			return nil, fmt.Errorf("record for session %s has no snapshot, so there is nothing "+
				"to compare %s against", rec.SessionID, rec.Cwd)
		}
		if _, err := os.Stat(rec.Snapshot); err != nil {
			return nil, fmt.Errorf("the snapshot for session %s (%s) is gone: %w",
				rec.SessionID, rec.Snapshot, err)
		}
		diff, err := compareTrees(rec.Snapshot, rec.Cwd)
		if err != nil {
			return nil, err
		}
		d.Changed, d.Missing, d.Added = diff.Changed, diff.Missing, diff.Added
	default:
		return nil, fmt.Errorf("record for session %s has unknown kind %q", rec.SessionID, rec.Kind)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

func recordPath(sessionID string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "starts", safeName(sessionID)+".json"), nil
}

// SnapshotPath returns where the directory snapshot for a session lives.
func SnapshotPath(sessionID string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "snapshots", safeName(sessionID)), nil
}

// BaselineDir returns where a non-git inplace replay keeps the repository that
// holds its recorded starting state.
//
// The replayed directory is not a repository and must not become one, so git is
// pointed at a repository somewhere else and told to treat that directory as its
// work tree. This is where that repository lives -- next to the snapshot whose
// contents it commits, so the two cannot drift apart unnoticed.
func BaselineDir(sessionID string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "baselines", safeName(sessionID)), nil
}

// DiscardDir returns where a replay puts files an attempt created, once it puts
// the directory back the way it found it.
//
// They are moved rather than deleted for the same reason the snapshot exists:
// whatever an attempt produced is the thing the run was measuring, and measuring
// it should not be what destroys it. Both kinds of inplace replay use this, so a
// git directory and a plain one behave the same way when they are rolled back.
func DiscardDir(sessionID string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "discarded", safeName(sessionID)), nil
}

// QuarantinePath is where one created path is moved to. rel is slash-separated
// and relative to the replayed directory.
func QuarantinePath(sessionID, rel string) (string, error) {
	base, err := DiscardDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.FromSlash(rel)), nil
}

// safeName keeps a session id from reaching outside the store.
//
// Session ids come from a transcript, which is a file a tool wrote and not
// necessarily anything trustworthy, and they end up as a path component. The
// whitelist is the same trade the runner makes for chain names: anything outside
// it becomes a dash, and an id that reduces to nothing becomes "session".
func safeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "session"
	}
	return out
}

// ---------------------------------------------------------------------------
// Git plumbing
// ---------------------------------------------------------------------------

// gitOut runs git with the ambient configuration neutralised.
//
// The environment is sanitised for the same reason the materializer sanitises
// it: a developer's global config is an uncontrolled input, and an alias or a
// trace hook here would change what this reads about somebody's repository.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	base := []string{
		"-c", "safe.directory=*",
		"-c", "core.quotepath=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %s", strings.Join(args, " "), firstLine(string(out)))
	}
	return string(out), nil
}

// dirtyPaths lists working-tree changes as `git status --porcelain` lines.
//
// The status codes are kept rather than stripped to bare paths: a reader who
// sees "?? scratch.txt" learns that the file was never tracked, which is a
// different fact from " M README.md".
func dirtyPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOut(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		paths = append(paths, line)
	}
	sort.Strings(paths)
	return paths, nil
}

// insideWorkTree reports whether dir sits inside a git work tree.
//
// Asking git rather than looking for a .git directory is deliberate: the session
// may have run in a subdirectory of a repository, or in a linked worktree where
// .git is a file. Both are repositories, and both can be pinned to a commit.
func insideWorkTree(dir string) bool {
	out, err := gitOut(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

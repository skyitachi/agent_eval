package startstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// TreeDiff is how a directory differs from a snapshot.
type TreeDiff struct {
	// Changed are files present on both sides with different contents.
	Changed []string
	// Missing are files the snapshot has and the directory does not.
	Missing []string
	// Added are files the directory has and the snapshot does not.
	Added []string
}

// Empty reports whether the two trees agree.
func (d *TreeDiff) Empty() bool {
	return d == nil || (len(d.Changed) == 0 && len(d.Missing) == 0 && len(d.Added) == 0)
}

// snapshotTree copies src into dst and reports what it copied.
//
// Two things are deliberately left out. A .git directory never travels: a record
// whose kind is "dir" describes a directory that was not a repository, and the
// only .git it could hold belongs to a nested repository that the snapshot's
// purposes do not reach. Symbolic links are skipped rather than recreated:
// following one can walk a snapshot out of the tree it is meant to describe, and
// recreating one can fail across filesystems -- a snapshot that silently means
// something different is worse than one that says what it left out.
//
// maxBytes is a hard stop, not a truncation. A half-copied directory would read
// as a directory that had fewer files at the start, which is a wrong answer that
// looks like a right one.
func snapshotTree(src, dst string, maxBytes int64) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			// Sockets, devices and fifos: not files, and not copyable.
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		if maxBytes > 0 && bytes > maxBytes {
			return fmt.Errorf("the snapshot passed %d bytes at %s; raise the limit or record "+
				"this session without --workspace-cwd", maxBytes, rel)
		}
		if err := copyFile(path, filepath.Join(dst, rel), info.Mode().Perm()); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return files, bytes, err
	}
	return files, bytes, nil
}

// compareTrees reports how dir differs from snapshot.
//
// Contents are compared by hash rather than by size and timestamp: a replay's
// whole job is to be trusted about what changed, and mtime is not a fact about
// contents.
func compareTrees(snapshot, dir string) (*TreeDiff, error) {
	diff := &TreeDiff{}
	inSnapshot := map[string]bool{}

	err := filepath.WalkDir(snapshot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(snapshot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		inSnapshot[rel] = true

		other := filepath.Join(dir, filepath.FromSlash(rel))
		otherInfo, err := os.Stat(other)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				diff.Missing = append(diff.Missing, rel)
				return nil
			}
			return err
		}
		if !otherInfo.Mode().IsRegular() {
			diff.Changed = append(diff.Changed, rel)
			return nil
		}
		same, err := sameContents(path, other)
		if err != nil {
			return err
		}
		if !same {
			diff.Changed = append(diff.Changed, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("compare against snapshot: %w", err)
	}

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !inSnapshot[rel] {
			diff.Added = append(diff.Added, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("compare directory against snapshot: %w", err)
	}

	sort.Strings(diff.Changed)
	sort.Strings(diff.Missing)
	sort.Strings(diff.Added)
	return diff, nil
}

// Restore puts a non-git directory back to the state its snapshot describes.
//
// Files that were not there at the start are moved into quarantine rather than
// deleted when a quarantine directory is given. The design asks for the
// directory to go back to its starting state, and back means reversible: an
// agent that created something worth keeping should not have it destroyed by the
// act of measuring what it created.
//
// Only the paths the comparison names are touched. Nothing here walks a
// directory tree removing things wholesale.
func Restore(rec *Record) ([]string, error) {
	if rec == nil || rec.Kind != KindDir {
		return nil, nil
	}
	if rec.Snapshot == "" {
		return nil, fmt.Errorf("record for session %s has no snapshot to restore from", rec.SessionID)
	}
	diff, err := compareTrees(rec.Snapshot, rec.Cwd)
	if err != nil {
		return nil, err
	}
	var moved []string

	for _, rel := range diff.Changed {
		if err := copyFile(filepath.Join(rec.Snapshot, filepath.FromSlash(rel)),
			filepath.Join(rec.Cwd, filepath.FromSlash(rel)), 0o644); err != nil {
			return moved, err
		}
		moved = append(moved, "restored "+rel)
	}
	for _, rel := range diff.Missing {
		dst := filepath.Join(rec.Cwd, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return moved, err
		}
		if err := copyFile(filepath.Join(rec.Snapshot, filepath.FromSlash(rel)), dst, 0o644); err != nil {
			return moved, err
		}
		moved = append(moved, "restored "+rel)
	}
	for _, rel := range diff.Added {
		victim := filepath.Join(rec.Cwd, filepath.FromSlash(rel))
		dest, err := quarantine(rec.SessionID, rel)
		if err != nil {
			return moved, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return moved, err
		}
		if err := os.Rename(victim, dest); err != nil {
			return moved, err
		}
		moved = append(moved, "moved aside "+rel)
	}
	return moved, nil
}

// quarantine moves a path an agent created out of the way.
//
// It lands under the session's discard directory, which keeps it out of the
// working directory the replay is trying to leave as it found it while staying
// retrievable -- and in one place, whichever kind of replay put it there.
func quarantine(sessionID, rel string) (string, error) {
	return QuarantinePath(sessionID, rel)
}

// ---------------------------------------------------------------------------
// File plumbing
// ---------------------------------------------------------------------------

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sameContents(a, b string) (bool, error) {
	ha, err := hashFile(a)
	if err != nil {
		return false, err
	}
	hb, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

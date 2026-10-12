package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultRoot is where Claude Code keeps session transcripts.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ProjectDir maps a working directory to the transcript directory Claude Code
// uses for it.
//
// The encoding is Claude Code's, not this tool's: every path separator becomes a
// dash, so /Users/x/lab becomes -Users-x-lab. It is lossy -- a directory whose
// name contains a dash is indistinguishable from a nested path -- which is why
// it is only ever used to *find* candidate transcripts. Every fact the collector
// reports (cwd, branch) is read back out of the transcript itself rather than
// inferred from the directory name.
func ProjectDir(root, cwd string) string {
	if root == "" || cwd == "" {
		return ""
	}
	return filepath.Join(root, strings.ReplaceAll(cwd, string(filepath.Separator), "-"))
}

// Source is a discoverable transcript.
type Source struct {
	// Path is the transcript file.
	Path string
	// SessionID is the file name without its extension.
	SessionID string
	// ProjectDir is the directory it was found under.
	ProjectDir string
	// ModTime orders transcripts by recency.
	ModTime time.Time
	Size    int64
}

// Sources lists transcripts under root, newest first.
//
// A missing root is not an error: a machine that has never run Claude Code has
// no transcripts, and that is a state the caller reports, not a failure it
// recovers from.
func Sources(root string) ([]Source, error) {
	if root == "" {
		return nil, nil
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var out []Source
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, Source{
				Path:       filepath.Join(dir, e.Name()),
				SessionID:  strings.TrimSuffix(e.Name(), ".jsonl"),
				ProjectDir: dir,
				ModTime:    info.ModTime(),
				Size:       info.Size(),
			})
		}
	}
	sortSources(out)
	return out, nil
}

// SourcesForCwd lists the transcripts of one working directory, newest first.
func SourcesForCwd(root, cwd string) ([]Source, error) {
	dir := ProjectDir(root, cwd)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no sessions recorded for %s (looked in %s)", cwd, dir)
		}
		return nil, err
	}
	var out []Source
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Source{
			Path:       filepath.Join(dir, e.Name()),
			SessionID:  strings.TrimSuffix(e.Name(), ".jsonl"),
			ProjectDir: dir,
			ModTime:    info.ModTime(),
			Size:       info.Size(),
		})
	}
	sortSources(out)
	return out, nil
}

// Resolve turns a user-supplied selector into a single transcript.
//
// Accepted forms, in the order they are tried:
//
//	<path ending in .jsonl>  used as-is
//	<a path>                 the newest transcript of that working directory
//	<session uuid or prefix> looked up under root
//
// The discrimination is on the separator, not on os.Stat. A recorded working
// directory is a fact about the machine the session ran on, so it usually does
// not exist here -- it is still a perfectly good way to name the session, and
// requiring the directory to be present would make the selector useless exactly
// when it is most needed. Session uuids never contain a separator, so the rule
// is unambiguous.
func Resolve(root, selector string) (Source, error) {
	if selector == "" {
		return Source{}, fmt.Errorf("no session given")
	}
	if strings.HasSuffix(selector, ".jsonl") {
		info, err := os.Stat(selector)
		if err != nil {
			return Source{}, err
		}
		return Source{
			Path:      selector,
			SessionID: strings.TrimSuffix(filepath.Base(selector), ".jsonl"),
			ModTime:   info.ModTime(),
			Size:      info.Size(),
		}, nil
	}

	if strings.ContainsRune(selector, filepath.Separator) {
		srcs, err := SourcesForCwd(root, selector)
		if err != nil {
			return Source{}, err
		}
		if len(srcs) == 0 {
			return Source{}, fmt.Errorf("no sessions recorded for %s", selector)
		}
		return srcs[0], nil
	}

	all, err := Sources(root)
	if err != nil {
		return Source{}, err
	}
	var matches []Source
	for _, s := range all {
		if s.SessionID == selector || strings.HasPrefix(s.SessionID, selector) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return Source{}, fmt.Errorf("no session matching %q under %s", selector, root)
	case 1:
		return matches[0], nil
	default:
		return Source{}, fmt.Errorf("%q matches %d sessions; give a longer prefix or a path", selector, len(matches))
	}
}

// Latest returns the most recent transcript under root.
func Latest(root string) (Source, error) {
	srcs, err := Sources(root)
	if err != nil {
		return Source{}, err
	}
	if len(srcs) == 0 {
		return Source{}, fmt.Errorf("no sessions under %s", root)
	}
	return srcs[0], nil
}

func sortSources(s []Source) {
	sort.Slice(s, func(i, j int) bool {
		if !s[i].ModTime.Equal(s[j].ModTime) {
			return s[i].ModTime.After(s[j].ModTime)
		}
		return s[i].Path < s[j].Path
	})
}

package sandbox

import (
	"path/filepath"
	"strings"
)

// joinClean joins a workspace-relative path onto root while guaranteeing the
// result stays inside root. Dataset files are authored by users, so a path like
// "../../etc/passwd" must not be able to escape the sandbox.
func joinClean(root, rel string) string {
	// Clean("/" + rel) collapses any ".." segments that would climb out.
	safe := filepath.Clean("/" + strings.ReplaceAll(rel, `\`, "/"))
	return filepath.Join(root, safe)
}

func dirOf(p string) string { return filepath.Dir(p) }

// relPaths normalizes a list of paths to workspace-relative slash form.
func relPaths(root string, abs []string) []string {
	out := make([]string, 0, len(abs))
	for _, a := range abs {
		r, err := filepath.Rel(root, a)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

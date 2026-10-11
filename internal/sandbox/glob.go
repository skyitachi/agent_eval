package sandbox

import (
	"regexp"
	"strings"
	"sync"
)

// Glob matching for dataset path filters.
//
// Go's path.Match does not support "**", but dataset authors expect it
// (".pytest_cache/**", "src/**/*.py"). These helpers add doublestar semantics on
// top of slash-separated paths.

var (
	globCacheMu sync.Mutex
	globCache   = map[string]*regexp.Regexp{}
)

// MatchGlob reports whether a slash-separated path matches a glob pattern.
//
// Supported syntax:
//
//   - any run of characters except "/"
//     ?      exactly one character except "/"
//     **/    zero or more leading path segments
//     **     any run of characters including "/"
//     [abc]  character class
func MatchGlob(pattern, path string) bool {
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// MatchAny reports whether path matches any pattern. An empty pattern list
// matches nothing, which lets callers use "len(patterns) == 0" as "no filter".
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if MatchGlob(p, path) {
			return true
		}
	}
	return false
}

// FilterPaths applies include patterns followed by ignore patterns. An empty
// include list means "everything not ignored".
func FilterPaths(paths []string, include, ignore []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if len(include) > 0 && !MatchAny(include, p) {
			continue
		}
		if len(ignore) > 0 && MatchAny(ignore, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if re, ok := cachedGlob(pattern); ok {
		return re, nil
	}
	var b strings.Builder
	b.WriteString(`^`)
	// A bare pattern without a slash also matches at any depth, mirroring
	// .gitignore-ish ergonomics: "*.pyc" should catch "src/a.pyc".
	if !strings.Contains(pattern, "/") {
		b.WriteString(`(?:.*/)?`)
	}
	b.WriteString(globToRegex(pattern))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	globCacheMu.Lock()
	globCache[pattern] = re
	globCacheMu.Unlock()
	return re, nil
}

func cachedGlob(pattern string) (*regexp.Regexp, bool) {
	globCacheMu.Lock()
	defer globCacheMu.Unlock()
	re, ok := globCache[pattern]
	return re, ok
}

func globToRegex(pattern string) string {
	var b strings.Builder
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				// "**/" also swallows the directory separator so that
				// "**/x" matches "x".
				if i+2 < len(runes) && runes[i+2] == '/' {
					b.WriteString(`(?:.*/)?`)
					i += 2
					continue
				}
				b.WriteString(`.*`)
				i++
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			// Copy the class through to the next unescaped ']'.
			j := i + 1
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j < len(runes) {
				inner := string(runes[i : j+1])
				if strings.HasPrefix(inner, "[!") {
					inner = "[^" + inner[2:]
				}
				b.WriteString(inner)
				i = j
			} else {
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

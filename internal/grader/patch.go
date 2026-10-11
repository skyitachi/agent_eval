package grader

import (
	"context"
	"fmt"
	"strings"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

func init() { Register(&Patch{}) }

// Patch grades the shape of the workspace change rather than its correctness.
//
// It answers questions no test suite can: did the agent change anything at all,
// did it touch files it was told not to, did it rewrite half the repository.
// Those are the failure modes that produce a green test run and an unusable
// submission -- deleting the tests, stubbing out the caller, or blanket-reverting
// the repository.
type Patch struct{}

func (p *Patch) Type() string { return "patch" }

type patchConfig struct {
	// RequireChangedPaths lists globs; each one must match at least one changed
	// path. This is how "the fix must actually be in slugify.py" is expressed.
	RequireChangedPaths []string `json:"require_changed_paths,omitempty"`
	// ForbidChangedPaths lists globs no changed path may match. Typically used to
	// protect the visible test suite from being edited into passing.
	ForbidChangedPaths []string `json:"forbid_changed_paths,omitempty"`
	// ExpectChanged requires at least one change. Defaults to true.
	ExpectChanged *bool `json:"expect_changed,omitempty"`
	// MinChangedFiles / MaxChangedFiles bound the size of the change set.
	MinChangedFiles int `json:"min_changed_files,omitempty"`
	MaxChangedFiles int `json:"max_changed_files,omitempty"`
	// MinDiffLines / MaxDiffLines bound the patch size, catching both a no-op and
	// a repository-wide rewrite.
	MinDiffLines int `json:"min_diff_lines,omitempty"`
	MaxDiffLines int `json:"max_diff_lines,omitempty"`
	// Paths / IgnorePaths narrow the change set for this grader only, on top of
	// the task's output filters.
	Paths       []string `json:"paths,omitempty"`
	IgnorePaths []string `json:"ignore_paths,omitempty"`
}

func (p *Patch) Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error) {
	var cfg patchConfig
	if err := DecodeConfig(spec, &cfg); err != nil {
		return nil, err
	}
	if cfg.ExpectChanged == nil {
		yes := true
		cfg.ExpectChanged = &yes
	}

	changed, err := gc.ChangedPaths()
	if err != nil {
		return nil, fmt.Errorf("compute changed paths: %w", err)
	}
	ignore := append(append([]string{}, HarnessIgnore...), cfg.IgnorePaths...)
	changed = sandbox.FilterPaths(changed, cfg.Paths, ignore)

	diff, err := gc.Diff()
	if err != nil {
		return nil, fmt.Errorf("compute diff: %w", err)
	}
	diffLines := countDiffLines(diff)

	res := &Result{
		Details: map[string]any{
			"changed_paths": changed,
			"changed_files": len(changed),
			"diff_lines":    diffLines,
		},
	}

	var problems []string

	if *cfg.ExpectChanged && len(changed) == 0 {
		problems = append(problems, "no changes were made to the workspace")
	}
	if !*cfg.ExpectChanged && len(changed) > 0 {
		problems = append(problems, fmt.Sprintf("expected no changes but %d file(s) were modified", len(changed)))
	}

	// Every required glob must be satisfied by at least one changed path.
	for _, pattern := range cfg.RequireChangedPaths {
		matched := false
		for _, c := range changed {
			if sandbox.MatchGlob(pattern, c) {
				matched = true
				break
			}
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("no changed file matches required pattern %q", pattern))
		}
	}

	// No changed path may match a forbidden glob.
	for _, pattern := range cfg.ForbidChangedPaths {
		for _, c := range changed {
			if sandbox.MatchGlob(pattern, c) {
				problems = append(problems, fmt.Sprintf("protected path %q was modified (matches %q)", c, pattern))
			}
		}
	}

	if cfg.MinChangedFiles > 0 && len(changed) < cfg.MinChangedFiles {
		problems = append(problems, fmt.Sprintf("only %d file(s) changed, need at least %d", len(changed), cfg.MinChangedFiles))
	}
	if cfg.MaxChangedFiles > 0 && len(changed) > cfg.MaxChangedFiles {
		problems = append(problems, fmt.Sprintf("%d files changed, limit is %d", len(changed), cfg.MaxChangedFiles))
	}
	if cfg.MinDiffLines > 0 && diffLines < cfg.MinDiffLines {
		problems = append(problems, fmt.Sprintf("diff is %d lines, need at least %d", diffLines, cfg.MinDiffLines))
	}
	if cfg.MaxDiffLines > 0 && diffLines > cfg.MaxDiffLines {
		problems = append(problems, fmt.Sprintf("diff is %d lines, limit is %d", diffLines, cfg.MaxDiffLines))
	}

	if len(problems) > 0 {
		res.Passed = false
		res.Score = 0
		res.Reason = strings.Join(problems, "; ")
		return res, nil
	}

	res.Passed = true
	res.Score = 1
	if len(changed) == 0 {
		res.Reason = "no changes required, none made"
	} else {
		res.Reason = fmt.Sprintf("%d file(s) changed as expected: %s", len(changed), strings.Join(truncateList(changed, 5), ", "))
	}
	return res, nil
}

// countDiffLines counts added and removed content lines, ignoring file headers.
func countDiffLines(diff string) int {
	n := 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"),
			strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "@@"):
			continue
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
			n++
		}
	}
	return n
}

func truncateList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := append([]string{}, items[:max]...)
	return append(out, fmt.Sprintf("(+%d more)", len(items)-max))
}

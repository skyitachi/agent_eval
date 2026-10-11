package grader

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/skyitachi/agenteval/internal/dataset"
)

func init() { Register(&ExactMatch{}) }

// ExactMatch grades a textual answer against an expected value.
//
// This is the workhorse for question-answering and multiple-choice style tasks.
// The pipeline is deliberately ordered and explicit:
//
//	source text -> normalize operations (in order) -> regex extraction -> compare
//
// Regex extraction is what makes the grader robust against an agent that wraps
// its answer in prose ("The answer is 31.") or a code fence, without resorting to
// a fuzzy comparison that would accept a wrong answer.
type ExactMatch struct{}

func (e *ExactMatch) Type() string { return "exact_match" }

type exactMatchConfig struct {
	// Expected is the single accepted value.
	Expected string `json:"expected,omitempty"`
	// ExpectedAny is a set of accepted values; the answer must equal one of them.
	ExpectedAny []string `json:"expected_any,omitempty"`
	// MatchMode is "exact" (default), "contains", or "regex".
	MatchMode string `json:"match_mode,omitempty"`
	// Regex extracts the candidate answer before comparison. When it contains a
	// capture group, group 1 is used; otherwise the whole match is used.
	Regex string `json:"regex,omitempty"`
	// RegexMatch selects which match to use when Regex matches more than once:
	// "first" (default) or "last".
	//
	// This matters because agents routinely echo their input. Asked for the value
	// of compute(5), a model may answer "compute(5) returns 31", so a first-match
	// extraction of (\d+) yields the wrong number while "last" yields the answer.
	RegexMatch string `json:"regex_match,omitempty"`
	// Normalize lists transformations applied in order:
	//   trim, lower, upper, collapse_ws, last_line, first_line,
	//   strip_punct, strip_quotes, strip_code_fence
	Normalize []string `json:"normalize,omitempty"`
	// CaseSensitive disables case folding. Default false.
	CaseSensitive bool `json:"case_sensitive,omitempty"`
	// Source selects the text to grade: "final_answer" (default), "diff".
	Source string `json:"source,omitempty"`
}

func (e *ExactMatch) Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error) {
	var cfg exactMatchConfig
	if err := DecodeConfig(spec, &cfg); err != nil {
		return nil, err
	}
	if cfg.Expected == "" && len(cfg.ExpectedAny) == 0 {
		return nil, fmt.Errorf("exact_match requires config.expected or config.expected_any")
	}

	var raw string
	switch cfg.Source {
	case "", "final_answer", "stdout", "answer":
		raw = gc.FinalAnswer()
	case "diff":
		d, err := gc.Diff()
		if err != nil {
			return nil, fmt.Errorf("compute diff: %w", err)
		}
		raw = d
	default:
		return nil, fmt.Errorf("unknown source %q (want final_answer or diff)", cfg.Source)
	}

	got, err := extractAnswer(raw, cfg)
	if err != nil {
		return &Result{
			Passed:  false,
			Score:   0,
			Reason:  err.Error(),
			Details: map[string]any{"raw_answer": clip(raw, 2000), "regex": cfg.Regex},
		}, nil
	}

	want := cfg.ExpectedAny
	if cfg.Expected != "" {
		want = append([]string{cfg.Expected}, want...)
	}

	match, candidates := compare(got, want, cfg)
	details := map[string]any{
		"extracted":  got,
		"expected":   want,
		"match_mode": orDefaultStr(cfg.MatchMode, "exact"),
		"raw_answer": clip(raw, 2000),
	}
	if len(candidates) > 0 {
		details["normalized_candidates"] = candidates
	}

	if match {
		return &Result{Passed: true, Score: 1, Reason: fmt.Sprintf("answer %q matched", clip(got, 120)), Details: details}, nil
	}
	return &Result{
		Passed:  false,
		Score:   0,
		Reason:  fmt.Sprintf("answer %q did not match any of %v", clip(got, 120), want),
		Details: details,
	}, nil
}

// extractAnswer applies normalization then regex extraction.
func extractAnswer(raw string, cfg exactMatchConfig) (string, error) {
	s := raw
	for _, op := range cfg.Normalize {
		var err error
		s, err = applyNormalize(s, op, cfg.CaseSensitive)
		if err != nil {
			return "", err
		}
	}
	if cfg.Regex != "" {
		re, err := compileCache(cfg.Regex)
		if err != nil {
			return "", fmt.Errorf("invalid regex %q: %w", cfg.Regex, err)
		}
		matches := re.FindAllStringSubmatch(s, -1)
		if len(matches) == 0 {
			return "", fmt.Errorf("answer did not match extraction pattern %q", cfg.Regex)
		}
		pick := matches[0]
		if cfg.RegexMatch == "last" {
			pick = matches[len(matches)-1]
		}
		if len(pick) > 1 {
			return strings.TrimSpace(pick[1]), nil
		}
		return strings.TrimSpace(pick[0]), nil
	}
	return strings.TrimSpace(s), nil
}

func compare(got string, want []string, cfg exactMatchConfig) (bool, []string) {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if !cfg.CaseSensitive {
			s = strings.ToLower(s)
		}
		return s
	}
	g := norm(got)
	mode := orDefaultStr(cfg.MatchMode, "exact")

	var normalized []string
	for _, w := range want {
		normalized = append(normalized, norm(w))
	}

	switch mode {
	case "exact", "":
		for _, w := range normalized {
			if g == w {
				return true, normalized
			}
		}
	case "contains":
		for _, w := range normalized {
			if w != "" && strings.Contains(g, w) {
				return true, normalized
			}
		}
	case "regex":
		// In regex mode the "expected" values are themselves patterns.
		for _, w := range want {
			re, err := compileCache(w)
			if err != nil {
				continue
			}
			if re.MatchString(got) {
				return true, want
			}
		}
	}
	return false, normalized
}

func applyNormalize(s, op string, caseSensitive bool) (string, error) {
	switch op {
	case "trim":
		return strings.TrimSpace(s), nil
	case "lower":
		return strings.ToLower(s), nil
	case "upper":
		return strings.ToUpper(s), nil
	case "collapse_ws":
		return collapseWhitespace(s), nil
	case "last_line":
		return lastNonEmptyLine(s), nil
	case "first_line":
		return firstNonEmptyLine(s), nil
	case "strip_punct":
		return strings.Trim(s, " \t\r\n.,;:!?\"'`()[]{}"), nil
	case "strip_quotes":
		return strings.Trim(s, " \t\r\n\"'`“”‘’"), nil
	case "strip_code_fence":
		return stripCodeFence(s), nil
	case "sort_lines":
		return strings.Join(sortedLines(s), "\n"), nil
	case "":
		return s, nil
	default:
		return "", fmt.Errorf("unknown normalize operation %q", op)
	}
}

var fenceRe = regexp.MustCompile("(?s)^\\s*```[a-zA-Z0-9_+-]*\\s*\\n(.*?)\\n?\\s*```\\s*$")

// stripCodeFence unwraps a single fenced block, so "```\n31\n```" becomes "31".
func stripCodeFence(s string) string {
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func sortedLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	// Insertion sort keeps this dependency-free and the inputs are tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

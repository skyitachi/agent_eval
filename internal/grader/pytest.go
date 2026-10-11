package grader

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

func init() { Register(&Pytest{}) }

// Pytest runs a Python test suite inside the sandbox and scores the outcome.
//
// It supports two materially different setups, and the distinction matters for
// evaluation validity:
//
//   - Tests already present in the workspace. The agent can read them, and can
//     also edit them, so pair this with a `patch` grader that forbids changes to
//     the test files.
//   - A hidden suite injected from `config.suite` at grade time. The agent never
//     sees it, which is the setup used by benchmarks that need to resist
//     test-detection and test-editing.
type Pytest struct{}

func (p *Pytest) Type() string { return "pytest" }

type pytestConfig struct {
	// Paths are test targets passed to pytest, e.g. ["tests/test_x.py"]. Empty
	// runs whole-project collection.
	Paths []string `json:"paths,omitempty"`
	// Suite is a dataset-relative directory or file copied into the sandbox
	// immediately before pytest runs.
	Suite string `json:"suite,omitempty"`
	// SuiteTarget is where Suite lands. Empty means the workspace root.
	SuiteTarget string `json:"suite_target,omitempty"`
	// MinPassed is the minimum number of passing tests. 0 means "any non-zero
	// count", which still rejects a suite that collected nothing.
	MinPassed int `json:"min_passed,omitempty"`
	// MaxFailed allows a small number of failures. Default 0: any failure fails.
	MaxFailed int `json:"max_failed,omitempty"`
	// Command overrides the pytest invocation entirely. When set, Paths,
	// PythonExtraArgs and the default flags are ignored.
	Command string `json:"command,omitempty"`
	// Python pins the interpreter used for `-m pytest`. When set it is used
	// as-is, and a missing pytest is reported as an error rather than silently
	// falling back to another interpreter.
	Python string `json:"python,omitempty"`
	// PythonCandidates are additional interpreters probed (in order) when Python
	// is unset. The environment override EnvPython is probed first, then these,
	// then "python3" and "python".
	PythonCandidates []string `json:"python_candidates,omitempty"`
	// ExtraArgs are appended to the default pytest invocation.
	ExtraArgs []string `json:"extra_args,omitempty"`
	// Env is passed to the pytest process.
	Env map[string]string `json:"env,omitempty"`
	// Setup commands run just before pytest (e.g. installing dev dependencies).
	Setup []string `json:"setup,omitempty"`
	// TimeoutS bounds the test run.
	TimeoutS int `json:"timeout_s,omitempty"`
}

// PytestOutcome is the parsed result of a pytest run.
type PytestOutcome struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errors  int `json:"errors"`
	Skipped int `json:"skipped"`
	XFailed int `json:"xfailed"`
	XPassed int `json:"xpassed"`
	// Collected is the total number of tests that produced a verdict.
	Collected int `json:"collected"`
	// ExitCode is pytest's process exit status.
	ExitCode int `json:"exit_code"`
	// FailingTests lists "path::name" entries from the short summary.
	FailingTests []string `json:"failing_tests,omitempty"`
	// Raw is the command output, kept for the report.
	Raw string `json:"-"`
	// ParseOK is false when no summary line was recognised, which usually means
	// collection failed outright.
	ParseOK bool `json:"parse_ok"`
}

// Success applies the pass/fail policy to a parsed outcome.
func (o PytestOutcome) Success(minPassed, maxFailed int) (bool, string) {
	switch {
	case !o.ParseOK:
		return false, "could not parse a pytest summary line (collection probably failed)"
	case o.Collected == 0:
		return false, "no tests were collected"
	case o.Errors > 0:
		return false, fmt.Sprintf("%d test error(s) (collection or fixture failure)", o.Errors)
	case o.Failed > maxFailed:
		return false, fmt.Sprintf("%d test(s) failed (allowed: %d)", o.Failed, maxFailed)
	case o.Passed == 0:
		return false, "no tests passed"
	case minPassed > 0 && o.Passed < minPassed:
		return false, fmt.Sprintf("only %d test(s) passed, need at least %d", o.Passed, minPassed)
	}
	return true, fmt.Sprintf("%d test(s) passed", o.Passed)
}

// Score returns the fraction of tests that produced a passing verdict.
func (o PytestOutcome) Score() float64 {
	if o.Collected <= 0 {
		return 0
	}
	return float64(o.Passed) / float64(o.Collected)
}

var pytestCounterRe = regexp.MustCompile(`(\d+)\s+(passed|failed|error|errors|skipped|xfailed|xpassed|deselected|warning|warnings)`)

var failingTestRe = regexp.MustCompile(`(?m)^(?:FAILED|ERROR)\s+(\S+)`)

// ParsePytestOutput extracts the outcome from pytest's output.
//
// pytest prints per-file progress and then a final totals line; the totals line
// is the one worth trusting. Scanning from the bottom for the first line that
// carries any counter avoids summing the same tests twice.
func ParsePytestOutput(out string, exitCode int) PytestOutcome {
	res := PytestOutcome{ExitCode: exitCode, Raw: out}

	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, "passed") && !strings.Contains(line, "failed") &&
			!strings.Contains(line, "error") && !strings.Contains(line, "no tests ran") {
			continue
		}
		if strings.Contains(line, "no tests ran") {
			res.ParseOK = true
			break
		}
		matches := pytestCounterRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}
		for _, m := range matches {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			switch m[2] {
			case "passed":
				res.Passed = n
			case "failed":
				res.Failed = n
			case "error", "errors":
				res.Errors = n
			case "skipped":
				res.Skipped = n
			case "xfailed":
				res.XFailed = n
			case "xpassed":
				res.XPassed = n
			}
		}
		res.ParseOK = true
		break
	}

	res.Collected = res.Passed + res.Failed + res.Errors

	for _, m := range failingTestRe.FindAllStringSubmatch(out, -1) {
		res.FailingTests = append(res.FailingTests, m[1])
	}
	if len(res.FailingTests) > 50 {
		res.FailingTests = res.FailingTests[:50]
	}
	return res
}

func (p *Pytest) Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error) {
	var cfg pytestConfig
	if err := DecodeConfig(spec, &cfg); err != nil {
		return nil, err
	}

	// Materialise the standard artifact bundle so a failing run can be
	// investigated from inside the workspace.
	if _, err := WriteArtifacts(gc); err != nil {
		return nil, err
	}

	// Inject the hidden suite at grade time. This must happen after the agent has
	// stopped, otherwise the tests would be visible during the attempt.
	if cfg.Suite != "" {
		if err := CopyInto(gc, cfg.Suite, cfg.SuiteTarget); err != nil {
			return nil, err
		}
	}

	env := mergeEnv(gc.Env(), cfg.Env)
	for _, cmd := range cfg.Setup {
		res, err := gc.Sandbox.Exec(ctx, cmd, sandbox.ExecOptions{
			Env:     env,
			Timeout: durationOr(cfg.TimeoutS, 300*time.Second),
		})
		if err != nil {
			return nil, fmt.Errorf("pytest setup %q: %w", cmd, err)
		}
		if !res.Success() {
			return &Result{
				Passed: false,
				Score:  0,
				Reason: fmt.Sprintf("test setup command failed (exit %d): %s", res.ExitCode, clip(res.CombinedText(), 500)),
				Details: map[string]any{
					"setup_command": cmd,
					"setup_exit":    res.ExitCode,
					"setup_output":  clip(res.CombinedText(), 4000),
				},
			}, nil
		}
	}

	command := cfg.Command
	if command == "" {
		python, err := p.resolveInterpreter(ctx, gc, cfg)
		if err != nil {
			return nil, err
		}
		// -p no:cacheprovider keeps .pytest_cache out of the workspace, so it
		// never shows up in the agent's diff. --color=no keeps output parseable.
		args := []string{shelljoin(python, "-m", "pytest", "-q", "--tb=short", "--color=no", "-p", "no:cacheprovider")}
		args = append(args, cfg.ExtraArgs...)
		args = append(args, cfg.Paths...)
		command = strings.Join(args, " ")
	}

	res, err := gc.Sandbox.Exec(ctx, command, sandbox.ExecOptions{
		Env:     env,
		Timeout: durationOr(cfg.TimeoutS, 600*time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("run pytest: %w", err)
	}

	outcome := ParsePytestOutput(res.CombinedText(), res.ExitCode)
	passed, reason := outcome.Success(cfg.MinPassed, cfg.MaxFailed)

	// pytest exit code 5 means "no tests collected"; that is a configuration
	// problem rather than an agent failure, and is worth flagging loudly.
	if res.ExitCode == 5 {
		reason = "pytest collected no tests (exit 5): check config.paths"
	}
	if outcome.Errors > 0 && len(outcome.FailingTests) == 0 {
		reason += " (see output for the collection error)"
	}

	details := map[string]any{
		"command":     command,
		"exit_code":   res.ExitCode,
		"passed":      outcome.Passed,
		"failed":      outcome.Failed,
		"errors":      outcome.Errors,
		"skipped":     outcome.Skipped,
		"collected":   outcome.Collected,
		"parse_ok":    outcome.ParseOK,
		"timed_out":   res.TimedOut,
		"duration_ms": res.Duration.Milliseconds(),
		"output_tail": clip(tail(res.CombinedText(), 40), 8000),
		"min_passed":  cfg.MinPassed,
		"suite":       cfg.Suite,
	}
	if len(outcome.FailingTests) > 0 {
		details["failing_tests"] = outcome.FailingTests
	}
	if res.TimedOut {
		reason = fmt.Sprintf("test run timed out after %ds", cfg.TimeoutS)
	}

	return &Result{
		Passed:  passed && !res.TimedOut,
		Score:   outcome.Score(),
		Reason:  reason,
		Details: details,
	}, nil
}

// ---------------------------------------------------------------------------

// EnvPython overrides the interpreter used by the pytest grader, for machines
// where the interpreter that has pytest installed is not on PATH (a virtualenv,
// conda env or interpreter inside a task image).
const EnvPython = "AGENTEVAL_PYTHON"

// resolveInterpreter finds an interpreter inside the sandbox that has pytest.
//
// Probing matters because "no module named pytest" is an environment problem,
// not an agent failure. Reporting it as a failed test run would silently turn a
// misconfigured harness into a zero score, which is the single most misleading
// outcome an evaluation framework can produce.
func (p *Pytest) resolveInterpreter(ctx context.Context, gc *Context, cfg pytestConfig) (string, error) {
	if cfg.Python != "" {
		ok, detail := probePytest(ctx, gc, cfg.Python)
		if !ok {
			return "", fmt.Errorf("config.python=%q has no usable pytest: %s", cfg.Python, detail)
		}
		return cfg.Python, nil
	}

	v, err := gc.Memo("pytest.python:"+strings.Join(cfg.PythonCandidates, ","), func() (any, error) {
		var candidates []string
		if env := strings.TrimSpace(os.Getenv(EnvPython)); env != "" {
			candidates = append(candidates, env)
		}
		candidates = append(candidates, cfg.PythonCandidates...)
		candidates = append(candidates, "python3", "python")

		var tried []string
		for _, c := range dedupe(candidates) {
			ok, detail := probePytest(ctx, gc, c)
			if ok {
				return c, nil
			}
			tried = append(tried, fmt.Sprintf("%s (%s)", c, detail))
		}
		return "", fmt.Errorf(
			"no Python interpreter with pytest found inside the sandbox; tried: %s. "+
				"Fix by setting config.python, adding config.python_candidates, "+
				"exporting %s, or installing pytest via workspace.setup / the sandbox image",
			strings.Join(tried, "; "), EnvPython)
	})
	if err != nil {
		return "", err
	}
	name, _ := v.(string)
	return name, nil
}

// probePytest reports whether an interpreter can import pytest.
func probePytest(ctx context.Context, gc *Context, python string) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := gc.Sandbox.Exec(probeCtx, shelljoin(python, "-m", "pytest", "--version"),
		sandbox.ExecOptions{Env: gc.Env(), Timeout: 30 * time.Second})
	if err != nil {
		return false, err.Error()
	}
	if res.Success() {
		return true, strings.TrimSpace(res.Stdout)
	}
	return false, firstNonEmptyLine(res.CombinedText())
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func durationOr(seconds int, def time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return def
}

func mergeEnv(base, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	// PYTHONDONTWRITEBYTECODE stops __pycache__ from polluting the diff.
	if _, set := out["PYTHONDONTWRITEBYTECODE"]; !set {
		out["PYTHONDONTWRITEBYTECODE"] = "1"
	}
	return out
}

// shelljoin quotes each argument so a path with spaces survives.
func shelljoin(args ...string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, shellQuoteWord(a))
	}
	return strings.Join(parts, " ")
}

func shellQuoteWord(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("._-/:@=+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func tail(s string, lines int) string {
	parts := strings.Split(s, "\n")
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}

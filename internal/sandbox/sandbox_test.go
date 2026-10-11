package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
)

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.pyc", "a.pyc", true},
		{"*.pyc", "src/deep/a.pyc", true}, // bare patterns match at any depth
		{"*.pyc", "a.py", false},
		{"__pycache__/**", "__pycache__/x.pyc", true},
		{"__pycache__/**", "src/__pycache__/x.pyc", false}, // anchored by the leading segment
		{"**/__pycache__/**", "src/__pycache__/x.pyc", true},
		{"src/**/*.py", "src/a/b/c.py", true},
		{"src/**/*.py", "src/c.py", true},
		{"src/**/*.py", "other/c.py", false},
		{"slugify.py", "slugify.py", true},
		{"a?c.txt", "abc.txt", true},
		{"a?c.txt", "ac.txt", false},
		{"[ab]x", "ax", true},
		{"[ab]x", "cx", false},
		{"[!ab]x", "cx", true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestFilterPathsIgnoresTakePrecedence(t *testing.T) {
	paths := []string{"src/a.py", "src/b.pyc", "docs/c.md", "src/__pycache__/d.pyc"}
	got := FilterPaths(paths, []string{"src/**"}, []string{"*.pyc", "**/__pycache__/**"})
	want := []string{"src/a.py"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("FilterPaths = %v, want %v", got, want)
	}
}

func TestJoinCleanRefusesToEscapeRoot(t *testing.T) {
	root := "/tmp/ws"
	for _, rel := range []string{"../../etc/passwd", "/etc/passwd", "a/../../b"} {
		got := joinClean(root, rel)
		if !strings.HasPrefix(got, root) {
			t.Errorf("joinClean(%q) = %q escaped root", rel, got)
		}
	}
}

// ---------------------------------------------------------------------------
// local backend
// ---------------------------------------------------------------------------

func newTestLocal(t *testing.T) *Local {
	t.Helper()
	sb, err := NewLocal(LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close(context.Background()) })
	return sb
}

func TestLocalExecCapturesOutputAndExitCode(t *testing.T) {
	sb := newTestLocal(t)
	ctx := context.Background()

	res, err := sb.Exec(ctx, "printf 'hello'; printf 'oops' >&2", ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success() {
		t.Errorf("expected success, got %+v", res)
	}
	if res.Stdout != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "oops" {
		t.Errorf("stderr = %q", res.Stderr)
	}

	// A non-zero exit is a result, not an infrastructure error.
	res, err = sb.Exec(ctx, "exit 3", ExecOptions{})
	if err != nil {
		t.Fatalf("non-zero exit should not return an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Err != nil {
		t.Errorf("Err should be nil for a clean non-zero exit, got %v", res.Err)
	}
	if res.Success() {
		t.Error("Success should be false for exit 3")
	}
}

func TestLocalExecTimeoutKillsProcessGroup(t *testing.T) {
	sb := newTestLocal(t)
	// The shell spawns a child that outlives the shell; a process-group kill
	// must take both down and return promptly.
	start := time.Now()
	res, err := sb.Exec(context.Background(), "sleep 30 & sleep 30", ExecOptions{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Errorf("TimedOut = false, res = %+v", res)
	}
	if elapsed > 15*time.Second {
		t.Errorf("timeout took %v; the process group was probably not killed", elapsed)
	}
}

func TestLocalExecCwdAndEnv(t *testing.T) {
	sb := newTestLocal(t)
	ctx := context.Background()
	if err := sb.WriteFile("sub/inner.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(ctx, "pwd", ExecOptions{Cwd: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Stdout), "/sub") {
		t.Errorf("cwd not applied: %q", res.Stdout)
	}

	res, err = sb.Exec(ctx, "printf '%s' \"$MY_VAR\"", ExecOptions{Env: map[string]string{"MY_VAR": "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "abc" {
		t.Errorf("env not applied: %q", res.Stdout)
	}
}

func TestLocalExecStreamForwardsWhileBuffering(t *testing.T) {
	sb := newTestLocal(t)
	var seen strings.Builder
	res, err := sb.ExecStream(context.Background(), "echo one; echo two", ExecOptions{},
		Streams{Stdout: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(seen.String()) != "one\ntwo" {
		t.Errorf("streamed = %q", seen.String())
	}
	if strings.TrimSpace(res.Stdout) != "one\ntwo" {
		t.Errorf("buffered = %q", res.Stdout)
	}
}

func TestLocalReadWriteFileRejectsEscape(t *testing.T) {
	sb := newTestLocal(t)
	if err := sb.WriteFile("../escaped.txt", []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(sb.Root()), "escaped.txt")); err == nil {
		t.Fatal("WriteFile escaped the workspace root")
	}
}

// ---------------------------------------------------------------------------
// materialize + baseline
// ---------------------------------------------------------------------------

func TestMaterializeFilesAndBaselineDiff(t *testing.T) {
	sb := newTestLocal(t)
	ctx := context.Background()

	ws := dataset.Workspace{
		Type:   dataset.WorkspaceFiles,
		Files:  map[string]string{"src/main.py": "print('v1')\n", "README.md": "hi\n"},
		Assets: "testdata/assets",
	}
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	if got, _ := sb.ReadFile("src/main.py"); string(got) != "print('v1')\n" {
		t.Errorf("inline file = %q", got)
	}
	// Assets are copied by *content* into the target, so the directory's own
	// name is not reproduced in the workspace.
	if !sb.Exists("from_assets.txt") {
		t.Error("assets directory contents were not copied to the workspace root")
	}

	// A pristine workspace has an empty diff.
	base, err := InitBaseline(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := base.Diff(ctx, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("expected empty diff on a pristine workspace, got:\n%s", diff)
	}

	// The baseline must have been committed, so modification shows up as a diff
	// and a brand new file is detected as untracked-but-changed.
	if err := sb.WriteFile("src/main.py", []byte("print('v2')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("src/new.py", []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := base.ChangedPaths(ctx, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(changed, ",")
	if !strings.Contains(joined, "src/main.py") || !strings.Contains(joined, "src/new.py") {
		t.Fatalf("changed paths = %v", changed)
	}

	diff, err = base.Diff(ctx, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-print('v1')", "+print('v2')", "+new"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q\n%s", want, diff)
		}
	}

	// Ignore filters must drop noise such as __pycache__.
	_ = sb.WriteFile("__pycache__/main.cpython-313.pyc", []byte("junk"), 0o644)
	filtered, err := base.ChangedPaths(ctx, DiffOptions{IgnorePaths: []string{"**/__pycache__/**"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range filtered {
		if strings.Contains(p, "__pycache__") {
			t.Errorf("ignore filter leaked %q", p)
		}
	}

	// Path scoping must be able to restrict the patch to a single file.
	scoped, err := base.ChangedPaths(ctx, DiffOptions{Paths: []string{"src/new.py"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0] != "src/new.py" {
		t.Errorf("scoped paths = %v", scoped)
	}
}

func TestMaterializeRunsSetupAndRebaselines(t *testing.T) {
	sb := newTestLocal(t)
	ctx := context.Background()

	ws := dataset.Workspace{
		Type:  dataset.WorkspaceFiles,
		Files: map[string]string{"a.txt": "1\n"},
		Setup: []dataset.Command{{Run: "echo generated > artifact.txt"}},
	}
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Setup output must exist...
	if !sb.Exists("artifact.txt") {
		t.Fatal("setup command did not run")
	}
	// ...but must not be counted as an agent change, because the baseline is
	// re-taken after setup.
	base, err := InitBaseline(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := base.Diff(ctx, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("setup artifacts leaked into the agent diff:\n%s", diff)
	}
}

func TestMaterializeSetupFailureIsFatalUnlessOptional(t *testing.T) {
	sb := newTestLocal(t)
	ctx := context.Background()

	ws := dataset.Workspace{
		Type:  dataset.WorkspaceEmpty,
		Setup: []dataset.Command{{Run: "exit 7"}},
	}
	if err := Materialize(ctx, sb, ws, "."); err == nil {
		t.Fatal("a failing setup command should abort materialization")
	}

	sb2 := newTestLocal(t)
	ws.Setup = []dataset.Command{{Run: "exit 7", Optional: true}}
	if err := Materialize(ctx, sb2, ws, "."); err != nil {
		t.Fatalf("optional setup failure should be tolerated: %v", err)
	}
}

func TestMaterializeStripsUpstreamGitHistory(t *testing.T) {
	// Build a source repo that contains a "fix" commit which must not leak to
	// the agent.
	src := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if out, err := runHost(ctx, src, args...); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(src, "code.py"), []byte("buggy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "introduce bug")
	if err := os.WriteFile(filepath.Join(src, "code.py"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "the fix commit message")

	sb := newTestLocal(t)
	ws := dataset.Workspace{Type: dataset.WorkspaceGit, Repo: src}
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// The working tree must be at the latest upstream state...
	got, err := sb.ReadFile("code.py")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "fixed" {
		t.Errorf("workspace content = %q, want the checked-out revision", got)
	}

	// ...but the history must be gone, so the fix is not readable via git log.
	out, _ := runHost(ctx, sb.Root(), "log", "--oneline", "--all")
	if strings.Contains(out, "the fix commit message") {
		t.Errorf("upstream history leaked into the workspace:\n%s", out)
	}
	if strings.Contains(out, "introduce bug") {
		t.Errorf("upstream history leaked into the workspace:\n%s", out)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n"); n > 1 {
		t.Errorf("expected a single baseline commit, got:\n%s", out)
	}
}

func TestMaterializeKeepsHistoryWhenAsked(t *testing.T) {
	src := t.TempDir()
	ctx := context.Background()
	keep := true
	run := func(args ...string) {
		t.Helper()
		if out, err := runHost(ctx, src, args...); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "original upstream commit")

	sb := newTestLocal(t)
	ws := dataset.Workspace{Type: dataset.WorkspaceGit, Repo: src, KeepGitHistory: &keep}
	if err := Materialize(ctx, sb, ws, "."); err != nil {
		t.Fatal(err)
	}
	out, _ := runHost(ctx, sb.Root(), "log", "--oneline", "--all")
	if !strings.Contains(out, "original upstream commit") {
		t.Errorf("history should be preserved when keep_git_history=true:\n%s", out)
	}
}

// TestGlobalGitConfigCannotInfluenceTheWorkspace pins down an invariant that is
// easy to lose: the evaluation must depend only on the repository's own content,
// never on the machine's git configuration.
//
// The test installs a post-commit hook through GIT_CONFIG_GLOBAL. If the harness
// honoured that config the hook would run inside the workspace and leave its
// marker behind -- and a hook that rewrites a commit or a diff would silently
// change every score.
func TestGlobalGitConfigCannotInfluenceTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "hook-ran")
	hook := "#!/bin/sh\ntouch " + shellQuoteForTest(marker) + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfgFile, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgFile)

	sb, err := NewLocal(LocalConfig{Root: filepath.Join(dir, "ws")})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(context.Background())

	if err := sb.WriteFile("a.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitBaseline(context.Background(), sb); err != nil {
		t.Fatalf("InitBaseline: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a git hook from the machine's global config ran inside the eval workspace; " +
			"the baseline commit is not reproducible")
	}
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// The report's client is the one piece of the pipeline with no test of its own.
// Everything else is checked by driving the real binary over a real dataset and
// reading the artifact it wrote; the client is JavaScript inside that artifact,
// so reading the artifact only ever proves the source was shipped, never that
// anything was drawn.
//
// That blind spot was not hypothetical. The drawer interpolated its tab bodies
// into a template, and a template stringifies what it is given, so every tab
// rendered the literal text "[object HTMLDivElement]" where its content should
// have been. The file, the payload and the exit code were all correct, and the
// regression suite was green.
//
// This file closes it the only way it can be closed: load a generated report in
// a browser, open a cell, click every tab, and read the DOM back.
package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// browser is the browser the render tests drive, or "" when none was found.
var browser = findBrowser()

// findBrowser looks for something that can run --headless --dump-dom.
//
// Not finding one skips these tests rather than failing them: a machine without
// a browser has not lost the rest of the pipeline, and the same reasoning
// already applies to the pytest-dependent tests. AGENTEVAL_CHROME overrides the
// search, the way AGENTEVAL_PYTHON overrides the interpreter for the pytest
// grader.
func findBrowser() string {
	if env := strings.TrimSpace(os.Getenv("AGENTEVAL_CHROME")); env != "" {
		return env
	}
	for _, path := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// renderProbe is appended to a generated report. It opens the first cell, clicks
// every tab, and hands back what the client drew.
//
// Three details are load-bearing rather than decorative:
//
//   - the needles are assembled at run time, because --dump-dom prints this
//     script as well, and a literal "[object " in it would look exactly like the
//     bug being looked for;
//   - the result travels base64-encoded, because re-serialising the DOM rewrites
//     every "<" the payload carries;
//   - the counting is done on markup, not on text. A regex for `class="dl `
//     matches an element and also matches the same string once it has been
//     escaped into visible text, which is the difference being tested, so the
//     diff count looks for the opening tag.
//
// Tab clicks re-query the button by name each time: a click re-renders the
// drawer, so a button captured beforehand is detached afterwards.
const renderProbe = `<script>
setTimeout(function () {
  function done(payload) {
    var pre = document.createElement('pre');
    pre.textContent = 'PROBE64=' + btoa(unescape(encodeURIComponent(JSON.stringify(payload))));
    document.documentElement.appendChild(pre);
  }
  var names = ['overview', 'trajectory', 'diff', 'graders', 'tools', 'raw'];
  var out = { tabs: [] };
  var rows = document.querySelectorAll('#main tbody tr');
  out.cells = rows.length;
  var body = null;
  if (rows.length) {
    rows[0].click();
    body = document.querySelector('.drawer-body');
  }
  out.drawerOpen = !!body;
  out.strips = body ? body.querySelectorAll('.tabs').length : 0;
  names.forEach(function (name) {
    var tab = { name: name, present: false };
    var button = body ? document.querySelector('.drawer-body [data-tab="' + name + '"]') : null;
    if (button) {
      tab.present = true;
      button.click();
      var wrap = document.querySelector('.drawer-body');
      var html = wrap.innerHTML;
      var text = wrap.textContent;
      tab.bytes = html.length;
      tab.text = text.slice(0, 4000);
      tab.kvBlocks = (html.match(/<dl class="kv">/g) || []).length;
      tab.diffLines = (html.match(/<span class="dl /g) || []).length;
      tab.deltas = (html.match(/class="delta /g) || []).length;
      tab.markers = (html.match(/class="bad"/g) || []).length;
      tab.objectString = text.indexOf('[' + 'object ') >= 0;
      tab.baseline = text.indexOf('Against the recorded baseline') >= 0;
      tab.answer = text.indexOf('Final answer') >= 0;
      tab.recorded = text.indexOf('recorded (baseline)') >= 0;
    }
    out.tabs.push(tab);
  });
  done(out);
}, 700);
</script>
</body>`

type renderTab struct {
	Name         string `json:"name"`
	Present      bool   `json:"present"`
	Bytes        int    `json:"bytes"`
	Text         string `json:"text"`
	KVBlocks     int    `json:"kvBlocks"`
	DiffLines    int    `json:"diffLines"`
	Deltas       int    `json:"deltas"`
	Markers      int    `json:"markers"`
	ObjectString bool   `json:"objectString"`
	Baseline     bool   `json:"baseline"`
	Answer       bool   `json:"answer"`
	Recorded     bool   `json:"recorded"`
}

type renderResult struct {
	Cells      int         `json:"cells"`
	DrawerOpen bool        `json:"drawerOpen"`
	Strips     int         `json:"strips"`
	Tabs       []renderTab `json:"tabs"`
}

// browserRender loads a report in a browser and returns what the client drew.
func browserRender(t *testing.T, reportPath string) renderResult {
	t.Helper()
	doc, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if n := strings.Count(string(doc), "</body>"); n != 1 {
		t.Fatalf("report has %d </body> tags; the probe expects exactly one", n)
	}
	page := filepath.Join(t.TempDir(), "probe.html")
	probed := strings.Replace(string(doc), "</body>", renderProbe, 1)
	if err := os.WriteFile(page, []byte(probed), 0o644); err != nil {
		t.Fatalf("write probe page: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, browser,
		"--headless", "--disable-gpu", "--hide-scrollbars",
		"--window-size=1440,1000", "--virtual-time-budget=5000",
		"--dump-dom", "file://"+page)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run %s: %v\nstderr:\n%s", browser, err, tail(stderr.String(), 400))
	}

	dump := stdout.String()
	// The app's own source mentions the marker prefix, so take the last one: the
	// probe appends its <pre> after everything the report shipped.
	i := strings.LastIndex(dump, "PROBE64=")
	if i < 0 {
		t.Fatalf("the browser never reported back, so the client never finished rendering\n"+
			"stderr:\n%s", tail(stderr.String(), 400))
	}
	blob := dump[i+len("PROBE64="):]
	if j := strings.Index(blob, "</pre>"); j >= 0 {
		blob = blob[:j]
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		t.Fatalf("decode probe payload: %v", err)
	}
	var res renderResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("parse probe payload: %v", err)
	}
	return res
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// requireBrowser skips a test when this machine has nothing to render with.
func requireBrowser(t *testing.T) {
	t.Helper()
	if browser == "" {
		t.Skip("no browser found; set AGENTEVAL_CHROME to run this test")
	}
}

// TestReportDrawerRendersEveryTab walks the drawer of a real report.
//
// The good agent is used because it produces a patch, so the diff tab has
// markup to draw: a tab that renders an empty state proves much less than one
// that renders a diff.
func TestReportDrawerRendersEveryTab(t *testing.T) {
	requireBrowser(t)
	work := t.TempDir()
	db := filepath.Join(work, "render.db")
	// runAgent skips the test itself when no interpreter with pytest is around.
	runAgent(t, work, db, "good", "render-model")

	res := browserRender(t, filepath.Join(work, "good.html"))
	if res.Cells == 0 {
		t.Fatal("the report drew no cells, so there is nothing to open")
	}
	if !res.DrawerOpen || res.Strips == 0 {
		t.Fatalf("clicking a cell did not open the drawer (open=%v tab strips=%d): the page renders, the interaction does not",
			res.DrawerOpen, res.Strips)
	}
	if len(res.Tabs) != 6 {
		t.Fatalf("probe reported %d tabs, want 6", len(res.Tabs))
	}
	for _, tab := range res.Tabs {
		if !tab.Present {
			t.Errorf("%s tab was not found in the drawer", tab.Name)
			continue
		}
		// The tab strip is a few hundred bytes on its own, so a body that did
		// not render leaves a tab that is nothing but its own name.
		if tab.Bytes < 700 {
			t.Errorf("%s rendered %d bytes, about a tab strip and nothing else: its body is missing",
				tab.Name, tab.Bytes)
		}
		if tab.Markers > 0 {
			t.Errorf("%s shows %d unrendered-value marker(s): a node was interpolated where a string belonged",
				tab.Name, tab.Markers)
		}
		if tab.ObjectString {
			t.Errorf("%s shows a stringified object instead of content", tab.Name)
		}
	}
	if res.Tabs[0].KVBlocks == 0 {
		t.Error("the overview tab drew no key/value card")
	}
	if !res.Tabs[0].Answer {
		t.Error("the overview tab does not carry the final answer")
	}
	if res.Tabs[2].DiffLines == 0 {
		t.Error("the diff tab drew no diff lines")
	}
}

// TestReportDrawerShowsTheRecordedBaseline covers the card a reader compares
// against, which the checked-in demo dataset cannot reach: it has no baseline.
func TestReportDrawerShowsTheRecordedBaseline(t *testing.T) {
	requireBrowser(t)
	work := t.TempDir()
	ds := writeBaselineDataset(t, filepath.Join(work, "ds"))
	report := filepath.Join(work, "recorded.html")

	r := runCLI(t, "run",
		"--dataset", ds,
		"--agent", "mock",
		"--bin", filepath.Join(agents, "good.sh"),
		"--name", "recorded",
		"--db", filepath.Join(work, "recorded.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", report,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	res := browserRender(t, report)
	if !res.DrawerOpen {
		t.Fatal("the drawer did not open")
	}
	overview := res.Tabs[0]
	if !overview.Baseline {
		t.Error("the overview tab does not show the recorded baseline the cell was compared against")
	}
	// A card with labels and no values is the failure this test exists for:
	// both deltas are built by interpolating a fragment into the row, and both
	// printed "[object Object]" when that interpolation was a string addition.
	if overview.Deltas < 2 {
		t.Errorf("the baseline card drew %d delta(s), want the token delta and the busy-time delta", overview.Deltas)
	}
	if overview.Markers > 0 || overview.ObjectString {
		t.Errorf("the baseline card does not render its values\n%s", overview.Text)
	}
	// 100 in + 20 out + 40 cached, from the fixture below.
	if !strings.Contains(overview.Text, "160") {
		t.Errorf("the baseline's recorded total is not on the page\n%s", overview.Text)
	}
	// What the recording concluded, beside what this attempt concluded. The
	// numbers say what each cost; only the two answers say whether they agree,
	// and a reader comparing attempts is asking the second question.
	if !overview.Recorded {
		t.Error("the overview tab does not show the recording's own final answer")
	}
	if !strings.Contains(overview.Text, "the recorded answer") {
		t.Errorf("the recording's final answer does not reach the page\n%s", overview.Text)
	}
	if !strings.Contains(overview.Text, "31") {
		t.Errorf("this attempt's own final answer is missing from the comparison\n%s", overview.Text)
	}
}

// writeBaselineDataset writes a one-task dataset whose recording carries an
// answer and a spend, so the report has something to compare against.
//
// The workspace holds logic.py because the shared good agent answers that file
// with "31": without it the mock agent would print nothing, and a comparison
// with one side empty would not exercise the side-by-side it is here to check.
func writeBaselineDataset(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "baselines"), 0o755); err != nil {
		t.Fatalf("create dataset dir: %v", err)
	}
	manifest := `name: render-baseline
description: a one-task dataset with a recorded baseline, for render tests
tasks:
  - id: recorded
    prompt: "state the answer"
    workspace:
      type: files
      files:
        logic.py: "def answer():\n    return 0\n"
    output: {type: stdout}
    baseline:
      file: baselines/recorded.json
      notes: a stand-in recording
`
	baseline := `{
  "id": "rec-1",
  "source": "render-test",
  "agent_model": "recorded-model",
  "final_output": "the recorded answer",
  "turns": [
    {"role": "user", "text": "state the answer"},
    {"role": "assistant", "text": "the recorded answer"}
  ],
  "usage": {"input_tokens": 100, "output_tokens": 20, "cache_read_tokens": 40},
  "cost_usd": 0.25,
  "duration_ms": 9000,
  "busy_ms": 5000
}`
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("dataset.yaml", manifest)
	write(filepath.Join("baselines", "recorded.json"), baseline)
	return dir
}

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
// a browser, read the run header, open a cell, click every tab, and read the DOM
// back.
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
//   - the table's markup count is the exception: it reads the table's own text
//     rather than the document's, because the document carries this script and
//     its source is made of the very tags being counted. A table cell holds
//     ids, counts, statuses and durations, and has no business containing one.
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
  var out = { tabs: [], toolsCell: '', toolsBadges: 0, tableText: '', tableMarkup: 0, sortedHead: '', sortedMarkup: 0 };
  var rows = document.querySelectorAll('#main tbody tr');
  out.cells = rows.length;
  var body = null;
  if (rows.length) {
    rows[0].click();
    body = document.querySelector('.drawer-body');
  }
  out.drawerOpen = !!body;
  out.strips = body ? body.querySelectorAll('.tabs').length : 0;
  var head = document.querySelector('.card.runhead');
  out.header = head ? head.textContent : '';
  var table = document.querySelector('#main table');
  if (table) {
    var heads = Array.prototype.map.call(table.querySelectorAll('thead th'), function (th) {
      return th.textContent.trim().toLowerCase();
    });
    var col = -1;
    heads.forEach(function (h, i) { if (h.indexOf('tools') === 0) col = i; });
    var first = table.querySelector('tbody tr');
    var cell = (col >= 0 && first) ? first.children[col] : null;
    out.toolsCell = cell ? cell.textContent : '';
    out.toolsBadges = cell ? cell.querySelectorAll('.v.fail').length : 0;
    out.tableText = table.textContent;
    out.tableMarkup = (out.tableText.match(/<[a-z/]/g) || []).length;
  }
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
      tab.recordedBy = text.indexOf('recorded by') >= 0;
      tab.crossAgent = text.indexOf('The recording was made by') >= 0;
      tab.genericNote = text.indexOf('the recorded time is the session') >= 0;
    }
    out.tabs.push(tab);
  });
  // Sorting is the only thing that makes a column header draw its arrow, so the
  // header has to be read twice: as it loads, and after a click. Done last,
  // because a click re-renders the whole page and would detach the drawer the
  // tabs above were read from.
  var sortTh = document.querySelector('#main thead th[data-key]:not([data-key=""])');
  if (sortTh) {
    sortTh.click();
    var thead = document.querySelector('#main thead');
    out.sortedHead = thead ? thead.textContent : '';
    out.sortedMarkup = (out.sortedHead.match(/<[a-z/]/g) || []).length;
  }
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
	RecordedBy   bool   `json:"recordedBy"`
	CrossAgent   bool   `json:"crossAgent"`
	GenericNote  bool   `json:"genericNote"`
}

type renderResult struct {
	Cells        int         `json:"cells"`
	DrawerOpen   bool        `json:"drawerOpen"`
	Strips       int         `json:"strips"`
	Header       string      `json:"header"`
	ToolsCell    string      `json:"toolsCell"`
	ToolsBadges  int         `json:"toolsBadges"`
	TableText    string      `json:"tableText"`
	TableMarkup  int         `json:"tableMarkup"`
	SortedHead   string      `json:"sortedHead"`
	SortedMarkup int         `json:"sortedMarkup"`
	Tabs         []renderTab `json:"tabs"`
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

// TestReportTaskTableShowsValuesNotMarkup covers the two places in the task
// table that compose markup instead of interpolating a value.
//
// Both had the same shape and neither was visible in the artifact. The Tools
// column read `6<span class="v fail" title="2 tool error(s)">!</span>`, and a
// sorted column header read `Task <span class="arrow">▲</span>` -- because a
// tpl() result is a plain string, so interpolating one into another template
// escapes it a second time and the page prints the markup it was building.
// This file's header warns about the opposite mistake: a value that should have
// been wrapped, printed as "[object HTMLDivElement]". This is what happens when
// the wrapping is missing instead, and it is invisible for the same reason --
// the file was correct, the payload was correct (the cell carried 2 tool calls
// and 1 tool error), the exit code was 0, and "[object " never appears. A badge
// and the text of a badge are the same bytes in the artifact and different
// things only after a browser has parsed them; the header arrow is worse still,
// because it does not exist at all until a reader clicks a column to sort.
//
// The fixture is the only one in the suite that calls a tool: a cell with no
// failed call leaves this column reading "-", so the cell that broke would
// never have been exercised.
func TestReportTaskTableShowsValuesNotMarkup(t *testing.T) {
	requireBrowser(t)
	work := t.TempDir()
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds"))
	report := filepath.Join(work, "tools.html")

	r := runCLI(t, "run",
		"--dataset", ds,
		"--bin", filepath.Join(agents, "tool-errors.sh"),
		"--name", "tools",
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", report,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	res := browserRender(t, report)
	if res.Cells == 0 {
		t.Fatal("the run drew no cells, so there is no table to read")
	}
	// The count, then the badge's own glyph: two calls in the fixture's stream,
	// one of which failed.
	if res.ToolsCell != "2!" {
		t.Errorf("the tools cell reads %q, want the call count followed by the error badge",
			res.ToolsCell)
	}
	// And the badge is an element, not a copied-out tag.
	if res.ToolsBadges != 1 {
		t.Errorf("the cell drew %d tool-error badge(s), want 1\n%s", res.ToolsBadges, res.TableText)
	}
	// The general claim the one cell is an instance of. A cell holds ids,
	// counts, statuses and durations, so a tag-shaped "<" among them can only
	// be a value that was escaped twice.
	if res.TableMarkup > 0 {
		t.Errorf("the table prints its own markup as text (%d tag-shaped sequences)\n%s",
			res.TableMarkup, res.TableText)
	}
	// The same claim about the header, read in the state where it draws an
	// arrow at all.
	if res.SortedMarkup > 0 {
		t.Errorf("the sorted header prints its own markup as text (%d tag-shaped sequences)\n%s",
			res.SortedMarkup, res.SortedHead)
	}
	// The positive half, without which the count above would also pass on a
	// header that drew nothing: the arrow is there, as a glyph, on the clicked
	// column.
	if !strings.Contains(res.SortedHead, "▲") && !strings.Contains(res.SortedHead, "▼") {
		t.Errorf("the sorted header drew no sort arrow\n%s", res.SortedHead)
	}
}

// TestReportHeaderNamesTheModelTheStreamNamed covers the chip a reader looks
// for and used not to find.
//
// The model was rendered only when the label column was set, so on every run
// that did not pass --model the header carried no model at all -- and "no model
// was requested", "the CLI's default was used" and "nobody recorded it" all
// looked like the same empty space. The chip is drawn by the client, so finding
// the text in the file would only prove the source shipped; this drives a run
// whose fixture's stream names a model and reads the page back.
func TestReportHeaderNamesTheModelTheStreamNamed(t *testing.T) {
	requireBrowser(t)
	work := t.TempDir()
	ds := writeOneAnswerDataset(t, filepath.Join(work, "ds"))
	report := filepath.Join(work, "model.html")

	r := runCLI(t, "run",
		"--dataset", ds,
		"--bin", filepath.Join(agents, "chain.sh"),
		"--name", "model",
		"--db", filepath.Join(work, "runs.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", report,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	res := browserRender(t, report)
	if res.Cells == 0 {
		t.Fatal("the run drew no cells, so there is no page to read")
	}
	// No --model was passed, so the label is empty and the stream is the only
	// source -- which is exactly the case that used to render as nothing.
	if !strings.Contains(res.Header, "default (the CLI chose it): stand-in") {
		t.Errorf("the header does not say which model served the run\n%s", res.Header)
	}
	if strings.Contains(res.Header, "[object ") {
		t.Errorf("the header shows a stringified object instead of its chips\n%s", res.Header)
	}
}

// TestReportHeaderSaysTheCellsWereChained covers the one claim the run header
// has to make about a chained run.
//
// The chip is drawn by the client from run.chain_sessions, so finding the string
// in the payload proves nothing about the page: that is the same blind spot that
// let every drawer tab render "[object HTMLDivElement]" while the file, the
// payload and the exit code all stayed correct. The chip is built with raw(),
// which is exactly where an interpolated node would print itself instead of its
// content.
//
// It matters because a chained run still reports k samples per task, and a
// reader who is not told will read a continuation as a repeat of one
// measurement.
func TestReportHeaderSaysTheCellsWereChained(t *testing.T) {
	requireBrowser(t)
	work := t.TempDir()
	ds := writeChainedTurnDataset(t, filepath.Join(work, "ds"))
	report := filepath.Join(work, "chained.html")

	r := runCLI(t, "run",
		"--dataset", ds,
		"--agent", "mock",
		"--bin", filepath.Join(agents, "chain.sh"),
		"--name", "chained",
		"--chain-sessions",
		"--db", filepath.Join(work, "chained.db"),
		"--work-root", filepath.Join(work, "work"),
		"--report", report,
	)
	if r.exitCode != 0 {
		t.Fatalf("run exited %d\nstdout:\n%s\nstderr:\n%s", r.exitCode, r.stdout, r.stderr)
	}

	res := browserRender(t, report)
	if res.Cells == 0 {
		t.Fatal("the chained run drew no cells, so there is no page to read")
	}
	if !strings.Contains(res.Header, "chained in order") {
		t.Errorf("the header does not say the cells were chained, so a continuation reads as a repeat\n%s", res.Header)
	}
	if strings.Contains(res.Header, "[object ") {
		t.Errorf("the header shows a stringified object instead of its chips\n%s", res.Header)
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
	// Who made the recording, and the one thing about that the reader has to be
	// told outright. The replay ran `mock` against a recording made by `cursor`,
	// so the deltas above are the difference between two agents -- and both the
	// name and the notice are built by interpolating a fragment into a row,
	// which is exactly where "[object Object]" would hide.
	if !overview.RecordedBy {
		t.Errorf("the card does not name who made the recording\n%s", overview.Text)
	}
	if !strings.Contains(overview.Text, "cursor") {
		t.Errorf("the recording's producer is not on the page\n%s", overview.Text)
	}
	if !overview.CrossAgent {
		t.Errorf("a recording from another agent must be flagged, not shown as like-for-like\n%s", overview.Text)
	}
	// The recording's own caveats replace the framework's generic sentence:
	// two caveat rows in one card is how both stop being read.
	if !strings.Contains(overview.Text, "records no cost") {
		t.Errorf("the recording's own caveats did not reach the card\n%s", overview.Text)
	}
	if overview.GenericNote {
		t.Errorf("the generic busy-time sentence should give way to the recording's own\n%s", overview.Text)
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
  "agent_name": "cursor",
  "agent_model": "recorded-model",
  "notes": "records no cost and no per-request timing",
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

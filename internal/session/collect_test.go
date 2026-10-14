package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/machine"
)

func TestCollectSessionSplitEmitsOneTaskPerSession(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.Task.ID != "aaaabbbb" {
		t.Errorf("id = %q, want the short session id", it.Task.ID)
	}
	if it.Task.Prompt != "修复 calc.py 里的减法错误" {
		t.Errorf("prompt = %q, want the opening request", it.Task.Prompt)
	}
	if it.Task.Baseline == nil || it.Task.Baseline.File != "baselines/aaaabbbb.json" {
		t.Errorf("baseline ref = %+v", it.Task.Baseline)
	}
	if it.BaselineFile != "baselines/aaaabbbb.json" {
		t.Errorf("baseline file = %q", it.BaselineFile)
	}
	if it.Baseline.Source != SourceName || it.Baseline.ID != s.ID {
		t.Errorf("baseline provenance = %+v", it.Baseline)
	}
}

func TestCollectedTaskWithoutAGraderIsMeasurableNotScorable(t *testing.T) {
	// A transcript has no ground truth, so collect emits no grader by default.
	// Inventing one would fabricate a correctness criterion the data cannot
	// support.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{})
	if len(items[0].Task.Graders) != 0 {
		t.Errorf("graders = %+v, want none", items[0].Task.Graders)
	}
}

func TestGraderIsAttachedOnlyWhenAsked(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{
		Grader:       "exact_match",
		GraderConfig: map[string]any{"expect": "已修复，减号写反了。"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := items[0].Task.Graders
	if len(g) != 1 || g[0].Type != "exact_match" || g[0].Name != "exact_match" {
		t.Fatalf("graders = %+v", g)
	}
	if g[0].Config["expect"] != "已修复，减号写反了。" {
		t.Errorf("grader config did not survive: %+v", g[0].Config)
	}
}

func TestWorkspaceBindingChangesTheOutputContract(t *testing.T) {
	s := parseFixture(t)

	plain, _ := Collect([]*Session{s}, Options{})
	if plain[0].Task.Workspace.Type != dataset.WorkspaceEmpty {
		t.Errorf("unbound workspace = %q, want empty", plain[0].Task.Workspace.Type)
	}
	if plain[0].Task.Output.Type != dataset.OutputStdout {
		t.Errorf("unbound output = %q, want stdout (the answer is conversational)",
			plain[0].Task.Output.Type)
	}

	bound, _ := Collect([]*Session{s}, Options{WorkspaceRepo: "https://example.com/r.git", WorkspaceRef: "v1"})
	if bound[0].Task.Workspace.Type != dataset.WorkspaceGit {
		t.Errorf("bound workspace = %q, want git", bound[0].Task.Workspace.Type)
	}
	if bound[0].Task.Workspace.Ref != "v1" {
		t.Errorf("ref = %q", bound[0].Task.Workspace.Ref)
	}
	if bound[0].Task.Output.Type != dataset.OutputPatch {
		t.Errorf("bound output = %q, want patch", bound[0].Task.Output.Type)
	}
}

func TestLimitsAreAnchoredToTheRecordedAttempt(t *testing.T) {
	// A task whose baseline took nine minutes, run under a default 30s timeout,
	// reports a timeout and looks like the agent failed. The budget follows the
	// baseline, with a floor so a cheap task still has room to think.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{})
	if got := items[0].Task.Limits.TimeoutS; got != 300 {
		t.Errorf("timeout = %d, want the 300s floor (baseline busy was 6s)", got)
	}
	if got := items[0].Task.Limits.MaxTurns; got != 20 {
		t.Errorf("max turns = %d, want the 20 floor", got)
	}

	// A slow baseline raises it. The clock sits on the last assistant entry of
	// the turn the opening prompt started, which is what the slice reads.
	s.Turns[2].DurationMs = 600_000 // 10 minutes
	slow, _ := Collect([]*Session{s}, Options{})
	if got := slow[0].Task.Limits.TimeoutS; got != 1800 {
		t.Errorf("timeout = %d, want 3x the baseline's 600s", got)
	}
}

func TestOnePromptSessionSizesItsBudgetFromTheSessionTotal(t *testing.T) {
	// With a single prompt the slice is the whole session, so the session's own
	// busy time applies -- including the case where that time had to be
	// estimated because the capture recorded none.
	s := parseFixture(t)
	s.Prompts = s.Prompts[:1]
	s.Turns = s.Turns[:3]
	s.BusyMs = 600_000
	items, _ := Collect([]*Session{s}, Options{})
	if got := items[0].Task.Limits.TimeoutS; got != 1800 {
		t.Errorf("timeout = %d, want 3x the session's 600s", got)
	}
	if got := items[0].Baseline.BusyMs; got != 600_000 {
		t.Errorf("baseline busy = %d, want the session's 600000", got)
	}
}

func TestExplicitLimitsWin(t *testing.T) {
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{TimeoutS: 42, MaxTurns: 7})
	if items[0].Task.Limits.TimeoutS != 42 || items[0].Task.Limits.MaxTurns != 7 {
		t.Errorf("limits = %+v", items[0].Task.Limits)
	}
}

func TestSplitTurnEmitsOneTaskPerPrompt(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{Split: SplitTurn})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Task.ID != "aaaabbbb-p0" || items[1].Task.ID != "aaaabbbb-p1" {
		t.Errorf("ids = %q, %q", items[0].Task.ID, items[1].Task.ID)
	}
	if items[1].Task.Prompt != "好的" {
		t.Errorf("second prompt = %q", items[1].Task.Prompt)
	}
}

func TestSplitTurnSlicesTheTrajectoryRatherThanRepeatingIt(t *testing.T) {
	// Each task's baseline must stop where its own prompt's answer stops.
	// Letting it run to the end of the session charges the first task for every
	// turn that came after it, and the fixture's two prompts make that visible:
	// the first task covers three turns, not all five.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{Split: SplitTurn})

	first, second := items[0].Baseline, items[1].Baseline
	if len(first.Turns) != 3 {
		t.Errorf("first baseline turns = %d, want only the turns answering it", len(first.Turns))
	}
	if len(second.Turns) != 2 {
		t.Errorf("second baseline turns = %d, want the slice from turn 3", len(second.Turns))
	}
	if second.Turns[0].Role != RoleUser || second.Turns[0].Text != "好的" {
		t.Errorf("second baseline starts at %+v, want its own prompt", second.Turns[0])
	}
	if got, want := first.Usage.Total(), 280; got != want {
		t.Errorf("first baseline tokens = %d, want %d (its own two messages)", got, want)
	}
	if got, want := second.Usage.Total(), 12; got != want {
		t.Errorf("second baseline tokens = %d, want %d (only the last message)", got, want)
	}
}

// TestMetadataAgreesWithTheBaselineItAccompanies pins a contradiction that a
// real collection exposed. Under SplitTurn the metadata block kept reporting the
// whole session's tokens while the baseline file beside it held the slice's, so
// one task's record said 528,296 and its own baseline said 108,935. The numbers
// now come from the same place; the free-form provenance fields still come from
// the session, which is what they are for.
func TestMetadataAgreesWithTheBaselineItAccompanies(t *testing.T) {
	s := parseFixture(t)

	for _, split := range []Split{SplitSession, SplitTurn} {
		items, err := Collect([]*Session{s}, Options{Split: split})
		if err != nil {
			t.Fatalf("split %s: %v", split, err)
		}
		for _, it := range items {
			want := it.Baseline.Usage.Total()
			if got := it.Task.Metadata["baseline_tokens"]; got != want {
				t.Errorf("split %s task %s: metadata baseline_tokens = %v, but its baseline file totals %d",
					split, it.Task.ID, got, want)
			}
			if got := it.Task.Metadata["baseline_ms"]; got != it.Baseline.BusyMs {
				t.Errorf("split %s task %s: metadata baseline_ms = %v, baseline file says %d",
					split, it.Task.ID, got, it.Baseline.BusyMs)
			}
		}
	}
}

// Every per-turn task covers exactly one prompt and the turns answering it, so
// the fields that describe the trajectory have to stay sliced, and the totals
// above them have to follow the slice rather than the session.
func TestLaterSplitTurnsStillCarryOnlyTheirOwnTrajectory(t *testing.T) {
	items, _ := Collect([]*Session{parseFixture(t)}, Options{Split: SplitTurn})
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if got := items[1].Task.Metadata["baseline_tokens"]; got != 12 {
		t.Errorf("second task baseline_tokens = %v, want 12 (the follow-up's own spend)", got)
	}
	if got := items[0].Task.Metadata["baseline_tokens"]; got != 280 {
		t.Errorf("first task baseline_tokens = %v, want 280 (only the turns it covers)", got)
	}
}

func TestPerTurnBaselineRefusesToInventACost(t *testing.T) {
	// The transcript reports a running session total, never a per-turn one.
	// Apportioning it would produce a number that looks measured and is not.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{Split: SplitTurn})
	if items[1].Baseline.CostUSD != 0 {
		t.Errorf("per-turn cost = %v, want unset", items[1].Baseline.CostUSD)
	}
	if !strings.Contains(items[1].Baseline.Notes, "cost") {
		t.Errorf("the baseline must say what it does not cover, got %q", items[1].Baseline.Notes)
	}
}

func TestMinPromptRunesDropsConversationalNoise(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{Split: SplitTurn, MinPromptRunes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (\"好的\" is a turn, not a task)", len(items))
	}
	// The dropped prompt still ends the slice. Its answer belongs to it, so
	// letting the kept task run on would charge it for a turn it did not cause.
	if got := len(items[0].Baseline.Turns); got != 3 {
		t.Errorf("kept task covers %d turns, want 3: the follow-up's own turns are not its", got)
	}
	if _, err := Collect([]*Session{s}, Options{Split: SplitTurn, MinPromptRunes: 100}); err == nil {
		t.Error("filtering everything away must be an error, not an empty dataset")
	}
}

func TestCollectRefusesASessionWithNoPrompt(t *testing.T) {
	s := &Session{ID: "machine-driven"}
	if _, err := Collect([]*Session{s}, Options{}); err == nil {
		t.Fatal("a session with no human prompt must be reported, not silently skipped")
	}
}

func TestCollectDeduplicatesIDs(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s, s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Task.ID == items[1].Task.ID {
		t.Errorf("both tasks got id %q; a dataset must not contain duplicates", items[0].Task.ID)
	}
	if !strings.HasSuffix(items[1].Task.ID, "-2") {
		t.Errorf("second id = %q, want a numeric suffix", items[1].Task.ID)
	}
}

func TestIDPrefixIsAppliedAndSanitised(t *testing.T) {
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{IDPrefix: "zed/gopls "})
	if got := items[0].Task.ID; got != "zed-gopls-aaaabbbb" {
		t.Errorf("id = %q, want the prefix sanitised into the dataset id pattern", got)
	}
}

func TestMetadataCarriesProvenance(t *testing.T) {
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{Category: "real-session", Tags: []string{"go"}})
	m := items[0].Task.Metadata
	for _, k := range []string{"source", "session_id", "cwd", "git_branch", "claude_version", "models", "transcript"} {
		if _, ok := m[k]; !ok {
			t.Errorf("metadata is missing %q: %+v", k, m)
		}
	}
	if items[0].Task.Category != "real-session" {
		t.Errorf("category = %q", items[0].Task.Category)
	}
}

func TestWriteDatasetRoundTripsThroughTheLoader(t *testing.T) {
	// The whole point of collect is that its output is an ordinary dataset.
	// If the loader cannot read it, the feature is a dead end.
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{Split: SplitTurn, Grader: "exact_match",
		GraderConfig: map[string]any{"expect": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "collected")
	if err := WriteDataset(dir, "collected-sessions", "from a fixture", items); err != nil {
		t.Fatalf("WriteDataset: %v", err)
	}

	ds, err := dataset.Load(dir)
	if err != nil {
		t.Fatalf("the collected dataset does not load back: %v", err)
	}
	if len(ds.Tasks) != 2 {
		t.Fatalf("loaded %d tasks, want 2", len(ds.Tasks))
	}
	if ds.Hash == "" {
		t.Error("a loaded dataset must carry a hash")
	}
	for _, task := range ds.Tasks {
		if task.ResolvedBaseline == nil {
			t.Errorf("task %s: the baseline did not resolve", task.ID)
		}
		if task.ResolvedPrompt == "" {
			t.Errorf("task %s: the prompt is empty", task.ID)
		}
	}
	first := ds.Tasks[0]
	// 280 is the first turn's own two messages, and it happens to equal the
	// cost-state session total here. What matters is that it is the slice's
	// sum: the session total would have been 292 with the follow-up folded in,
	// and the cost-state figure cannot describe one turn of a longer session.
	if first.ResolvedBaseline.Usage.Total() != 280 {
		t.Errorf("per-turn baseline tokens = %d, want 280 (the slice's own messages)", first.ResolvedBaseline.Usage.Total())
	}
	if first.ResolvedBaseline.FinalOutput != "已修复，减号写反了。" {
		t.Errorf("baseline final output = %q, want the answer to the first prompt", first.ResolvedBaseline.FinalOutput)
	}
}

func TestSessionSplitMeasuresTheOpeningRequest(t *testing.T) {
	// The session split is one task per session, and the task is the opening
	// request -- so the baseline has to be the opening request's own turns. On a
	// session where the human kept asking, the whole trajectory answers
	// questions this task never asks, and a replay measured against it would be
	// reported as cheaper than a recording it does not correspond to.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{})
	dir := filepath.Join(t.TempDir(), "collected")
	if err := WriteDataset(dir, "n", "", items); err != nil {
		t.Fatal(err)
	}
	ds, err := dataset.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := ds.Tasks[0].ResolvedBaseline
	if got := len(base.Turns); got != 3 {
		t.Errorf("session baseline turns = %d, want the 3 answering the opening request", got)
	}
	if got := base.Usage.Total(); got != 280 {
		t.Errorf("session baseline tokens = %d, want 280 (the opening request's own messages)", got)
	}
	if base.FinalOutput != "已修复，减号写反了。" {
		t.Errorf("session baseline final output = %q, want the answer to the opening request", base.FinalOutput)
	}
	// Cost is a session total, and this baseline is not the session.
	if base.CostUSD != 0 {
		t.Errorf("session baseline cost = %v, want unset", base.CostUSD)
	}
	if !strings.Contains(base.Notes, "not attributable") {
		t.Errorf("the baseline must say what it does not cover, got %q", base.Notes)
	}
}

func TestTheSessionSplitSaysWhichPromptsItLeftOut(t *testing.T) {
	// TestSessionSplitMeasuresTheOpeningRequest pins what the session split
	// measures. This pins what it declares: the prompts past the first are not
	// in the dataset at all, and nothing else in a one-task collection says how
	// many prompts the recording held. A reader who does not know to look for
	// that takes the opening request's slice for the whole session -- which, on
	// a session the human kept asking in, is a fraction of it.
	s := parseFixture(t)
	if len(s.Prompts) < 2 {
		t.Fatalf("the fixture holds %d prompt(s); this test needs one the human kept asking in", len(s.Prompts))
	}
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("the session split emitted %d task(s), want 1", len(items))
	}
	notes := items[0].Baseline.Notes
	for _, want := range []string{
		fmt.Sprintf("holds %d prompts", len(s.Prompts)),
		"collected the first one only",
		fmt.Sprintf("the other %d are not in it", len(s.Prompts)-1),
		"--split turn",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the baseline must say %q, got:\n%s", want, notes)
		}
	}

	// A one-prompt session is the case the split is for and there is nothing to
	// declare: the slice is the session.
	one := parseFixture(t)
	one.Prompts = one.Prompts[:1]
	one.Turns = one.Turns[:3]
	only, err := Collect([]*Session{one}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := only[0].Baseline.Notes; strings.Contains(got, "collected the first one only") {
		t.Errorf("a one-prompt session is the whole session, so it must not be declared partial:\n%s", got)
	}

	// And the turn split collects them, so the claim belongs to the session
	// split alone rather than to every multi-prompt session.
	all, err := Collect([]*Session{parseFixture(t)}, Options{Split: SplitTurn})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("the turn split emitted %d task(s), want one per prompt", len(all))
	}
	for _, it := range all {
		if strings.Contains(it.Baseline.Notes, "not in it") {
			t.Errorf("the turn split collected every prompt, so nothing is missing:\n%s", it.Baseline.Notes)
		}
	}
}

// TestCollectedTurnsCarryTheSessionAsFieldsAndMetadata covers the duplication
// that makes a turn-split dataset replayable as a chain.
//
// The two copies exist for different readers: the metadata entries are the
// human-facing provenance block, which the engine is not allowed to interpret,
// and the fields are the copy the runner may act on when it is asked to put a
// session's turns back in one workspace. Two copies of one fact drift unless
// something pins them, and the drift here is quiet -- a chain that groups the
// wrong tasks still runs and still writes rows.
func TestCollectedTurnsCarryTheSessionAsFieldsAndMetadata(t *testing.T) {
	s := parseFixture(t)
	items, err := Collect([]*Session{s}, Options{Split: SplitTurn})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 2 {
		t.Fatalf("the fixture gave %d turn task(s); this test needs more than one", len(items))
	}
	for i, it := range items {
		task := it.Task
		if task.Session != s.ID {
			t.Errorf("%s: session field = %q, want the recording's %q", task.ID, task.Session, s.ID)
		}
		if task.PromptIndex == nil {
			t.Fatalf("%s: no prompt index, so the chain has no order to replay in", task.ID)
		}
		if *task.PromptIndex != i {
			t.Errorf("%s: prompt index = %d, want %d", task.ID, *task.PromptIndex, i)
		}
		// And the metadata the reader sees says the same thing.
		if got, _ := task.Metadata["session_id"].(string); got != task.Session {
			t.Errorf("%s: metadata session_id = %q but the field says %q", task.ID, got, task.Session)
		}
		if got, ok := task.Metadata["prompt_index"].(int); !ok || got != *task.PromptIndex {
			t.Errorf("%s: metadata prompt_index = %v but the field says %d", task.ID, task.Metadata["prompt_index"], *task.PromptIndex)
		}
	}

	// A task with no recording behind it must not claim a session, or the flag
	// would chain hand-written tasks together by accident.
	hand := dataset.TaskSpec{ID: "hand"}
	if hand.Session != "" || hand.PromptIndex != nil {
		t.Error("a task written by hand starts with no session and no index")
	}
}

func TestCollectedDatasetIsStableAcrossRewrites(t *testing.T) {
	// Two collections of the same session must hash the same. Otherwise a
	// rerun would look like a different dataset and no comparison would hold.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{})
	base := t.TempDir()
	one, two := filepath.Join(base, "one"), filepath.Join(base, "two")
	if err := WriteDataset(one, "n", "", items); err != nil {
		t.Fatal(err)
	}
	if err := WriteDataset(two, "n", "", items); err != nil {
		t.Fatal(err)
	}
	d1, err := dataset.Load(one)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := dataset.Load(two)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash != d2.Hash {
		t.Errorf("hashes differ: %s vs %s", d1.Hash, d2.Hash)
	}
}

func TestBaselineChangeChangesTheDatasetHash(t *testing.T) {
	// The comparison a run makes is part of what the run means, so a changed
	// baseline must not be able to hide behind an unchanged hash.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{})
	base := t.TempDir()
	one := filepath.Join(base, "one")
	if err := WriteDataset(one, "n", "", items); err != nil {
		t.Fatal(err)
	}
	before, err := dataset.Load(one)
	if err != nil {
		t.Fatal(err)
	}

	items[0].Baseline.Usage.InputTokens = 999
	two := filepath.Join(base, "two")
	if err := WriteDataset(two, "n", "", items); err != nil {
		t.Fatal(err)
	}
	after, err := dataset.Load(two)
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash == after.Hash {
		t.Error("editing the baseline left the dataset hash unchanged")
	}
}

func TestWriteDatasetRefusesNothing(t *testing.T) {
	if err := WriteDataset(t.TempDir(), "n", "", nil); err == nil {
		t.Error("writing an empty dataset must fail loudly")
	}
}

func TestAppendAddsOnlyWhatIsNotAlreadyCollected(t *testing.T) {
	// Collecting the same directory again after more sessions is what append is
	// for. The first prompt is already in the dataset; only the follow-up may be
	// added, or the same recording would be counted twice in every aggregate.
	s := parseFixture(t)
	dir := filepath.Join(t.TempDir(), "ds")
	first, _ := Collect([]*Session{s}, Options{})
	if err := WriteDataset(dir, "n", "hand-edited", first); err != nil {
		t.Fatal(err)
	}
	manifestBefore, _ := os.ReadFile(filepath.Join(dir, "dataset.yaml"))

	turns, _ := Collect([]*Session{s}, Options{Split: SplitTurn})
	res, err := AppendDataset(dir, "other", "ignored", turns)
	if err != nil {
		t.Fatalf("AppendDataset: %v", err)
	}
	if res.Fresh {
		t.Fatal("an existing dataset was treated as a new one")
	}
	if len(res.Added) != 1 || res.Added[0].Task.ID != "aaaabbbb-p1" {
		t.Errorf("added = %v, want only the follow-up", itemIDs(res.Added))
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "aaaabbbb-p0" {
		t.Errorf("skipped = %v, want the first prompt, which is already collected", res.Skipped)
	}

	ds, err := dataset.Load(dir)
	if err != nil {
		t.Fatalf("the appended dataset does not load: %v", err)
	}
	var ids []string
	for _, task := range ds.Tasks {
		ids = append(ids, task.ID)
		if task.ResolvedBaseline == nil {
			t.Errorf("task %s lost its baseline", task.ID)
		}
	}
	if strings.Join(ids, ",") != "aaaabbbb,aaaabbbb-p1" {
		t.Errorf("tasks = %v, want the original followed by the follow-up", ids)
	}
	// The manifest may have been edited by hand since it was generated.
	if after, _ := os.ReadFile(filepath.Join(dir, "dataset.yaml")); string(after) != string(manifestBefore) {
		t.Errorf("appending rewrote the manifest:\n%s", after)
	}

	// A second identical append is a no-op, not an error.
	again, err := AppendDataset(dir, "n", "", turns)
	if err != nil {
		t.Fatalf("second AppendDataset: %v", err)
	}
	if len(again.Added) != 0 || len(again.Skipped) != 2 {
		t.Errorf("re-appending added %v and skipped %v, want nothing added", itemIDs(again.Added), again.Skipped)
	}
}

func TestAppendRenamesAnIdTakenByADifferentRecording(t *testing.T) {
	// Ids are an 8-character session prefix, so two sessions can collide. The
	// second one is a different recording and must still be collectable.
	dir := filepath.Join(t.TempDir(), "ds")
	first, _ := Collect([]*Session{parseFixture(t)}, Options{})
	if err := WriteDataset(dir, "n", "", first); err != nil {
		t.Fatal(err)
	}
	other := parseFixture(t)
	other.ID = "aaaabbbb-9999-different-session"
	items, _ := Collect([]*Session{other}, Options{})

	res, err := AppendDataset(dir, "n", "", items)
	if err != nil {
		t.Fatalf("AppendDataset: %v", err)
	}
	if got := res.Renamed["aaaabbbb"]; got != "aaaabbbb-2" {
		t.Errorf("renamed = %v, want aaaabbbb -> aaaabbbb-2", res.Renamed)
	}
	ds, err := dataset.Load(dir)
	if err != nil {
		t.Fatalf("the appended dataset does not load: %v", err)
	}
	if len(ds.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(ds.Tasks))
	}
	// The renamed task must point at its own baseline, not at the original's.
	renamedTask := ds.Tasks[1]
	if renamedTask.Baseline.File != "baselines/aaaabbbb-2.json" || renamedTask.ResolvedBaseline.ID != other.ID {
		t.Errorf("renamed task's baseline = %s (session %s), want its own", renamedTask.Baseline.File, renamedTask.ResolvedBaseline.ID)
	}
	if ds.Tasks[0].ResolvedBaseline.ID == other.ID {
		t.Error("the append overwrote the original task's baseline")
	}
}

func TestAppendToAnEmptyDirectoryWritesANewDataset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	items, _ := Collect([]*Session{parseFixture(t)}, Options{})
	res, err := AppendDataset(dir, "n", "", items)
	if err != nil {
		t.Fatalf("AppendDataset: %v", err)
	}
	if !res.Fresh {
		t.Error("a directory with no manifest holds no dataset to append to")
	}
	if _, err := dataset.Load(dir); err != nil {
		t.Errorf("the new dataset does not load: %v", err)
	}
}

func TestAppendRefusesAnInlineDataset(t *testing.T) {
	// Appending to inline tasks would mean rewriting the manifest, and with it
	// whatever was written there by hand.
	dir := t.TempDir()
	manifest := "name: inline\ntasks:\n  - id: t1\n    prompt: hi\n    graders: [{type: exit_code}]\n"
	if err := os.WriteFile(filepath.Join(dir, "dataset.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _ := Collect([]*Session{parseFixture(t)}, Options{})
	if _, err := AppendDataset(dir, "n", "", items); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Errorf("err = %v, want a refusal that names the way out", err)
	}
}

func itemIDs(items []Item) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.Task.ID)
	}
	return ids
}

func TestUnknownSplitIsRejected(t *testing.T) {
	s := parseFixture(t)
	if _, err := Collect([]*Session{s}, Options{Split: "half"}); err == nil {
		t.Error("an unknown split must be rejected rather than guessed at")
	}
}

// ---------------------------------------------------------------------------
// Binding a task to the directory its recording ran in
// ---------------------------------------------------------------------------

// gitIn runs git in dir with the developer's own configuration kept out of it:
// a global trace2 hook or a diff driver is an uncontrolled input, and a test
// that depends on one is a test that fails on someone else's machine.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false",
		"-c", "user.name=agenteval", "-c", "user.email=eval@agenteval.local"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

// TestAnEmptyReplaySaysWhereTheRecordingRan is the defect this closes.
//
// A collected task starts from an empty workspace -- the transcript holds the
// branch and not the commit, so the tree cannot be rebuilt from it -- while the
// session it was collected from ran somewhere with files in it. Both halves are
// true in the ordinary case and the dataset used to state neither, which left a
// report comparing two attempts that were not the same task.
func TestAnEmptyReplaySaysWhereTheRecordingRan(t *testing.T) {
	s := parseFixture(t)
	s.Cwd = "/Users/someone/tmp/1015oncall/未命名"
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0].Task.Workspace.Type; got != dataset.WorkspaceEmpty {
		t.Fatalf("workspace type = %q, want the unbound default", got)
	}
	notes := items[0].Baseline.Notes
	if !strings.Contains(notes, s.Cwd) || !strings.Contains(notes, "replays in an empty workspace") {
		t.Errorf("the baseline must say the replay has no workspace and where the recording ran, got: %q", notes)
	}
	// The claim is kept to what is known: that the session needed those files is
	// not knowable from a transcript.
	if strings.Contains(notes, "the files it worked on") {
		t.Errorf("the note must not claim the session needed files it may never have opened: %q", notes)
	}
}

// TestABoundWorkspaceIsNotAlsoCalledAnEmptyReplay: the note describes the
// workspace, so a task that has one must not carry the sentence about not
// having one.
func TestABoundWorkspaceIsNotAlsoCalledAnEmptyReplay(t *testing.T) {
	s := parseFixture(t)
	s.Cwd = "/Users/someone/tmp/whatever"
	items, err := Collect([]*Session{s}, Options{WorkspaceRepo: "/repos/proj", WorkspaceRef: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0].Task.Workspace; got.Type != dataset.WorkspaceGit || got.Repo != "/repos/proj" {
		t.Fatalf("workspace = %+v, want the bound repository", got)
	}
	if strings.Contains(items[0].Baseline.Notes, "empty workspace") {
		t.Errorf("a bound task must not be described as an empty replay: %q", items[0].Baseline.Notes)
	}
}

// TestWorkspaceCwdClonesARepositoryWithACommit: a repository can be reproduced
// faithfully enough to be worth preferring, so it is bound as a repository.
// This capture carries no session-context block, so the branch is all there is
// to bind to -- see TestBindCwdPrefersTheRecordedCommitOverTheBranch for the
// case where the capture names a commit.
func TestWorkspaceCwdClonesARepositoryWithACommit(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("buggy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "c")

	s := parseFixture(t)
	s.Cwd, s.GitBranch = dir, "main"
	items, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	got := items[0].Task.Workspace
	if got.Type != dataset.WorkspaceGit || got.Repo != dir || got.Ref != "main" {
		t.Fatalf("workspace = %+v, want the repository at its recorded branch", got)
	}
	// The note is what stops the comparison being read as like-for-like: the
	// branch is not the commit.
	if !strings.Contains(items[0].Baseline.Notes, "records the branch and not the commit") {
		t.Errorf("the binding must say what it is, got: %q", items[0].Baseline.Notes)
	}
}

// TestWorkspaceCwdRefusesWhatCannotBeCloned covers both halves of one rule: a
// directory that is not a repository, and one that is but has no commits. The
// second is the case that looks bindable and clones to nothing.
//
// Neither is copied. A copy would be the directory as it stands now, which is
// after the recorded session finished -- so if that session fixed something in
// there, the replay starts from the answer and the baseline comparison measures
// the copy instead of the agent. Refusing says that plainly; copying hides it in
// a task that looks bound.
func TestWorkspaceCwdRefusesWhatCannotBeCloned(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare makes the directory and returns it.
		prepare func(t *testing.T) string
	}{
		{
			name: "not a repository at all",
			prepare: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name: "a repository with no commits",
			prepare: func(t *testing.T) string {
				dir := t.TempDir()
				gitIn(t, dir, "init", "--quiet")
				if err := os.WriteFile(filepath.Join(dir, "checked-out.txt"), []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.prepare(t)
			s := parseFixture(t)
			s.Cwd, s.GitBranch = dir, "main"
			_, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
			if err == nil {
				t.Fatal("a directory that cannot be cloned must be refused, not copied")
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "--workspace-repo") {
				t.Errorf("the error must name the directory and the way out, got: %v", err)
			}
		})
	}
}

// TestWorkspaceCwdRefusesADirectoryThatIsGone: the flag means "use the
// directory the recording ran in", and falling back to an empty workspace would
// produce a task that looks bound and measures nothing.
func TestWorkspaceCwdRefusesADirectoryThatIsGone(t *testing.T) {
	s := parseFixture(t)
	s.Cwd = filepath.Join(t.TempDir(), "deleted-after-the-session")
	_, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
	if err == nil {
		t.Fatal("binding a directory that is gone must fail rather than fall back")
	}
	if !strings.Contains(err.Error(), s.Cwd) {
		t.Errorf("the error must name the directory, got: %v", err)
	}
}

// TestBindCwdPrefersTheRecordedCommitOverTheBranch is the difference between a
// replay that starts where the session started and one that starts wherever the
// session's branch ended up. The capture names the commit; the branch is the
// weaker claim and is only used when the commit is gone.
func TestBindCwdPrefersTheRecordedCommitOverTheBranch(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "first")
	first := revParse(t, dir)
	// A second commit on the same branch, so the branch tip is NOT the commit
	// the session started from. That gap is the whole point of the binding.
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "second")

	s := parseFixture(t)
	s.Cwd, s.GitBranch, s.GitHead = dir, "main", first[:10]
	s.GitStart = GitStart{Known: true}
	items, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	got := items[0].Task.Workspace
	if got.Ref != first {
		t.Errorf("ref = %q, want the full sha of the commit the session started from (%s), "+
			"not the branch tip", got.Ref, first)
	}
	if len(got.Ref) != 40 {
		t.Errorf("ref = %q (%d chars), want a full object name a checkout can use", got.Ref, len(got.Ref))
	}
	if !strings.Contains(items[0].Baseline.Notes, "the commit the session started from") {
		t.Errorf("the binding must say it is pinned to a commit, got: %q", items[0].Baseline.Notes)
	}
	if items[0].Baseline.GitHead != first[:10] {
		t.Errorf("baseline git_head = %q, want the commit the capture recorded", items[0].Baseline.GitHead)
	}
}

// TestBindCwdFallsBackWhenTheRecordedCommitIsGone: a commit that has been
// rebased or collected away cannot be checked out, and the honest answer is the
// branch plus a note that says which of the two happened.
func TestBindCwdFallsBackWhenTheRecordedCommitIsGone(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "c")

	s := parseFixture(t)
	s.Cwd, s.GitBranch, s.GitHead = dir, "main", "deadbee"
	s.GitStart = GitStart{Known: true}
	items, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0].Task.Workspace.Ref; got != "main" {
		t.Errorf("ref = %q, want the branch when the recorded commit is not in the repository", got)
	}
	notes := items[0].Baseline.Notes
	if !strings.Contains(notes, "no longer in this repository") {
		t.Errorf("the note must say the commit is gone rather than that none was recorded, got: %q", notes)
	}
}

// TestADirtyRecordingSaysTheReplayCannotStartWhereItDid: a commit is
// reproducible and uncommitted changes are not, so a clean checkout of the
// recorded commit is not a replay of where the session started.
func TestADirtyRecordingSaysTheReplayCannotStartWhereItDid(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "c")
	head := revParse(t, dir)

	s := parseFixture(t)
	s.Cwd, s.GitBranch, s.GitHead = dir, "main", head[:10]
	s.GitStart = GitStart{Known: true, Dirty: true, DirtyPaths: 4}
	items, err := Collect([]*Session{s}, Options{WorkspaceCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	if !items[0].Baseline.RecordedDirty {
		t.Error("the baseline must record that the recording started dirty")
	}
	notes := items[0].Baseline.Notes
	if !strings.Contains(notes, "4 uncommitted path(s)") {
		t.Errorf("the note must say how much the commit cannot reproduce, got: %q", notes)
	}
	if !strings.Contains(notes, "not from where the recording started") {
		t.Errorf("the note must not let a clean checkout pass for the starting state, got: %q", notes)
	}
}

// TestMetadataCarriesTheCommitAndTheDirtyFlag: the task's own metadata is what a
// person reads when they open the dataset, and "which commit" is the first
// question a replay raises.
func TestMetadataCarriesTheCommitAndTheDirtyFlag(t *testing.T) {
	s := parseFixture(t)
	s.GitBranch, s.GitHead = "main", "2d347614e9"
	s.GitStart = GitStart{Known: true, Dirty: true, DirtyPaths: 1}
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := items[0].Task.Metadata
	if m["git_head"] != "2d347614e9" {
		t.Errorf("metadata git_head = %v, want the recorded commit", m["git_head"])
	}
	if m["git_dirty"] != true {
		t.Errorf("metadata git_dirty = %v, want true", m["git_dirty"])
	}
}

// revParse is the full object name of HEAD, which is what a binding has to
// produce for a checkout to be possible.
func revParse(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(out)
}

// TestADirtyRecordingIsNotDescribedAsAReplayOfACommitWhenItIsNot: the note is
// about what the replay starts from, so it belongs only on a task that starts
// from that commit. An unbound task reproduces neither the commit nor the
// changes on top of it, and it already carries a note saying so.
func TestADirtyRecordingIsNotDescribedAsAReplayOfACommitWhenItIsNot(t *testing.T) {
	s := parseFixture(t)
	s.Cwd, s.GitHead = "/Users/someone/tmp/whatever", "2d347614e9"
	s.GitStart = GitStart{Known: true, Dirty: true, DirtyPaths: 3}
	items, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	notes := items[0].Baseline.Notes
	if strings.Contains(notes, "clean checkout of the recorded commit") {
		t.Errorf("an empty-workspace replay must not be described as a checkout: %q", notes)
	}
	if !strings.Contains(notes, "replays in an empty workspace") {
		t.Errorf("it still has to say what it does replay from, got: %q", notes)
	}
	// The fact is still recorded on the baseline: it is true of the recording
	// whether or not this task uses it.
	if !items[0].Baseline.RecordedDirty {
		t.Error("the recording started dirty and the baseline must say so")
	}
}

func TestCollectStampsTheMachineOnEveryBaseline(t *testing.T) {
	s := parseFixture(t)
	m := &machine.Identity{ID: "abc", Hostname: "box"}
	items, err := Collect([]*Session{s}, Options{Split: SplitTurn, Machine: m})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 2 {
		t.Fatalf("items = %d, want the fixture's two prompts", len(items))
	}
	for _, it := range items {
		if it.Baseline.Machine == nil || !it.Baseline.Machine.Same(*m) {
			t.Errorf("%s: machine = %+v, want %+v", it.Task.ID, it.Baseline.Machine, m)
		}
	}
	// Each baseline owns its identity; editing one must not edit the others.
	items[0].Baseline.Machine.ID = "edited"
	if items[1].Baseline.Machine.ID != "abc" || m.ID != "abc" {
		t.Error("the baselines share one identity")
	}

	none, err := Collect([]*Session{s}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if none[0].Baseline.Machine != nil {
		t.Errorf("no machine was given, got %+v", none[0].Baseline.Machine)
	}
}

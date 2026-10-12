package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
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

	// A slow baseline raises it.
	s.BusyMs = 600_000 // 10 minutes
	slow, _ := Collect([]*Session{s}, Options{})
	if got := slow[0].Task.Limits.TimeoutS; got != 1800 {
		t.Errorf("timeout = %d, want 3x the baseline's 600s", got)
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
	// The follow-up's baseline must start at the follow-up. Repeating the whole
	// session would charge the first prompt's cost to the second one.
	s := parseFixture(t)
	items, _ := Collect([]*Session{s}, Options{Split: SplitTurn})

	first, second := items[0].Baseline, items[1].Baseline
	if len(first.Turns) != 5 {
		t.Errorf("first baseline turns = %d, want the whole session", len(first.Turns))
	}
	if len(second.Turns) != 2 {
		t.Errorf("second baseline turns = %d, want the slice from turn 3", len(second.Turns))
	}
	if second.Turns[0].Role != RoleUser || second.Turns[0].Text != "好的" {
		t.Errorf("second baseline starts at %+v, want its own prompt", second.Turns[0])
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

// The per-turn split is nested by design -- the first task keeps the whole
// session, each later one starts at its own prompt -- so the fields that
// describe the trajectory must stay sliced even as the totals above follow them.
func TestLaterSplitTurnsStillCarryOnlyTheirOwnTrajectory(t *testing.T) {
	items, _ := Collect([]*Session{parseFixture(t)}, Options{Split: SplitTurn})
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if got := items[1].Task.Metadata["baseline_tokens"]; got != 12 {
		t.Errorf("second task baseline_tokens = %v, want 12 (the follow-up's own spend)", got)
	}
	if got := items[0].Task.Metadata["baseline_tokens"]; got != 292 {
		t.Errorf("first task baseline_tokens = %v, want 292 (every turn it covers)", got)
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
	// 292 is the summed per-message figure, not the cost-state total of 280:
	// this is a per-turn baseline, and the session-scoped total cannot describe
	// one turn of it. The fixture makes the two disagree on purpose.
	if first.ResolvedBaseline.Usage.Total() != 292 {
		t.Errorf("per-turn baseline tokens = %d, want 292 (the summed messages)", first.ResolvedBaseline.Usage.Total())
	}
	if first.ResolvedBaseline.FinalOutput != "好的。" {
		t.Errorf("baseline final output = %q", first.ResolvedBaseline.FinalOutput)
	}
}

func TestSessionSplitUsesTheRecordedSessionTotal(t *testing.T) {
	// The session-scoped baseline is the one place the cost-state record
	// applies, and it is authoritative there.
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
	if got := ds.Tasks[0].ResolvedBaseline.Usage.Total(); got != 280 {
		t.Errorf("session baseline tokens = %d, want the recorded 280", got)
	}
	if got := ds.Tasks[0].ResolvedBaseline.CostUSD; got != 0.25 {
		t.Errorf("session baseline cost = %v, want 0.25", got)
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

func TestUnknownSplitIsRejected(t *testing.T) {
	s := parseFixture(t)
	if _, err := Collect([]*Session{s}, Options{Split: "half"}); err == nil {
		t.Error("an unknown split must be rejected rather than guessed at")
	}
}

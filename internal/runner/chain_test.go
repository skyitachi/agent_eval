package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
)

// The chained-replay tests.
//
// Orchestration in this package is covered end to end, because that is the only
// level at which "the agent saw the file" can be observed. What is here is the
// part a browser run cannot see: which cells share a directory, in what order
// they are handed over, and which groups of them are refused. Every one of
// those is silent when it is wrong -- a chain in the wrong order still runs and
// still writes rows -- so each needs an assertion of its own.

func chained(id, session string, promptIndex int) dataset.TaskSpec {
	return dataset.TaskSpec{ID: id, Session: session, PromptIndex: intPtr(promptIndex)}
}

func intPtr(v int) *int { return &v }

func planFor(t *testing.T, chaining bool, samples int, tasks ...dataset.TaskSpec) []cell {
	t.Helper()
	cfg := withDefaults(Config{
		RunID: "run1", WorkRoot: t.TempDir(), Samples: samples, ChainSessions: chaining,
	})
	cells, err := plan(&dataset.Dataset{Tasks: tasks}, cfg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return cells
}

func ids(cells []cell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.task.ID)
	}
	return out
}

// TestChainingGivesASessionsTasksOneWorkspace is the whole point of the flag:
// a follow-up prompt asks about what the earlier one produced, so replaying it
// in a fresh workspace asks a question about files that were never there.
func TestChainingGivesASessionsTasksOneWorkspace(t *testing.T) {
	cells := planFor(t, true, 1,
		chained("s1-p0", "s1", 0),
		chained("s1-p1", "s1", 1),
		chained("s1-p2", "s1", 2),
	)
	if len(cells) != 3 {
		t.Fatalf("planned %d cells, want 3", len(cells))
	}
	for _, c := range cells[1:] {
		if c.dir != cells[0].dir {
			t.Errorf("%s got workspace %s, want the chain's %s", c.task.ID, c.dir, cells[0].dir)
		}
	}
	// A task that names no session is its own chain, which is what keeps the
	// flag from changing an ordinary dataset.
	lone := planFor(t, true, 1, dataset.TaskSpec{ID: "hand-written"})
	if len(lone) != 1 || !strings.HasSuffix(lone[0].dir, filepath.Join("hand-written", "s0")) {
		t.Errorf("a task with no session got %v, want its own directory", ids(lone))
	}

	// With the flag off, the same dataset keeps its own cells apart -- the
	// sessions are readable but nothing joins them up.
	off := planFor(t, false, 1, chained("s1-p0", "s1", 0), chained("s1-p1", "s1", 1))
	if off[0].dir == off[1].dir {
		t.Error("without the flag two tasks shared a workspace, which is not what it asked for")
	}
}

// TestAChainIsReplayedInTheOrderThePromptsWereAsked covers the ordering half.
// The recorded turn index decides it, not the order the tasks file happens to
// list them in: the file is editable by hand and a reordered one would replay
// the follow-up before the request it follows.
func TestAChainIsReplayedInTheOrderThePromptsWereAsked(t *testing.T) {
	cells := planFor(t, true, 1,
		chained("s1-p2", "s1", 2),
		chained("s1-p0", "s1", 0),
		chained("s1-p1", "s1", 1),
	)
	if got := ids(cells); got[0] != "s1-p0" || got[1] != "s1-p1" || got[2] != "s1-p2" {
		t.Errorf("order = %v, want the prompts in recorded order", got)
	}
	if !cells[0].first || cells[0].last || !cells[2].last || cells[2].first {
		t.Errorf("the chain's ends are wrong: %+v", cells)
	}
	for _, c := range cells {
		if c.chain != "s1" {
			t.Errorf("%s is in chain %q, want s1", c.task.ID, c.chain)
		}
	}
}

// TestAChainWithNoRecordedOrderFallsBackToTheDataset covers the arm that must
// not guess. Half a key is not a key: sorting on it would scatter the tasks
// that lack one wherever a zero landed, and a chain in an arbitrary order
// measures the harness's sorting rather than the agent.
func TestAChainWithNoRecordedOrderFallsBackToTheDataset(t *testing.T) {
	unordered := dataset.TaskSpec{ID: "s1-a", Session: "s1"}
	ordered := chained("s1-b", "s1", 0)
	cells := planFor(t, true, 1, unordered, ordered)
	if got := ids(cells); got[0] != "s1-a" || got[1] != "s1-b" {
		t.Errorf("order = %v, want the dataset's own when an index is missing", got)
	}
}

// TestASessionIsChainedOncePerSample: two samples are two independent runs of
// the same chain, so they must not share a workspace -- that would make the
// second sample start from the first one's leftovers and stop being a repeat.
func TestASessionIsChainedOncePerSample(t *testing.T) {
	cells := planFor(t, true, 2, chained("s1-p0", "s1", 0), chained("s1-p1", "s1", 1))
	if len(cells) != 4 {
		t.Fatalf("planned %d cells, want 4", len(cells))
	}
	bySample := map[int][]string{}
	for _, c := range cells {
		bySample[c.sample] = append(bySample[c.sample], c.dir)
	}
	for s, dirs := range bySample {
		if len(dirs) != 2 || dirs[0] != dirs[1] {
			t.Errorf("sample %d got workspaces %v, want one shared by the chain", s, dirs)
		}
	}
	if bySample[0][0] == bySample[1][0] {
		t.Error("the two samples shared a workspace, so the second is not a repeat of the first")
	}
	// Each sample is its own first-and-last pair.
	firsts := 0
	for _, c := range cells {
		if c.first {
			firsts++
		}
	}
	if firsts != 2 {
		t.Errorf("%d cells are marked first, want one per chain-and-sample", firsts)
	}
}

// TestChainingRequiresOneCellInFlight guards the invariant the shared directory
// rests on. Two cells of one chain in flight at once would write over each
// other, and no column on the row would record that it happened.
func TestChainingRequiresOneCellInFlight(t *testing.T) {
	if err := (Config{ChainSessions: true, Concurrency: 1}).Validate(); err != nil {
		t.Errorf("chaining one cell at a time must be allowed: %v", err)
	}
	err := Config{ChainSessions: true, Concurrency: 4}.Validate()
	if err == nil {
		t.Fatal("chaining with four cells in flight must be refused, not silently serialised")
	}
	if !strings.Contains(err.Error(), "4") {
		t.Errorf("the refusal should name the concurrency it cannot honour, got: %v", err)
	}
	// The same concurrency without chaining is the ordinary parallel run.
	if err := (Config{Concurrency: 4}).Validate(); err != nil {
		t.Errorf("a parallel run that does not chain is fine: %v", err)
	}
}

// TestAHalfRecordedChainIsRefused covers --resume.
//
// A chained cell starts from the workspace its predecessor left, and that
// workspace went away with the cell. Resuming a chain in the middle therefore
// starts its remaining tasks from nothing, and the rows they write would be
// about the missing files rather than about the agent.
func TestAHalfRecordedChainIsRefused(t *testing.T) {
	planned := planFor(t, true, 1,
		chained("s1-p0", "s1", 0), chained("s1-p1", "s1", 1), chained("s1-p2", "s1", 2),
		chained("s2-p0", "s2", 0), chained("s2-p1", "s2", 1),
	)
	err := chainsIntact(planned, planned[1:], Config{})
	if err == nil {
		t.Fatal("a chain whose first task is already recorded must not be resumed in the middle")
	}
	if !strings.Contains(err.Error(), "s1#0") || !strings.Contains(err.Error(), "2 of its 3") {
		t.Errorf("the refusal must name the chain and what is left of it, got: %v", err)
	}

	// Nothing recorded yet: every chain runs whole.
	if err := chainsIntact(planned, planned, Config{}); err != nil {
		t.Errorf("an untouched chain must be runnable: %v", err)
	}
	// Everything recorded: every chain is skipped whole.
	if err := chainsIntact(planned, nil, Config{}); err != nil {
		t.Errorf("a finished chain must be skipped, not refused: %v", err)
	}
	// One chain whole and another half done is still a refusal -- and it names
	// the one that is broken rather than the one that is fine.
	half := append([]cell{}, planned[:3]...)
	half = append(half, planned[4]) // s2-p1 without s2-p0
	err = chainsIntact(planned, half, Config{})
	if err == nil || !strings.Contains(err.Error(), "s2#0") || strings.Contains(err.Error(), "s1#0") {
		t.Errorf("the refusal must point at the half-done chain, got: %v", err)
	}
}

// TestAWorkspaceNameCannotEscapeTheWorkRoot is the guard on a value that comes
// out of a recording. Session ids are UUIDs in practice, but a task file is
// meant to be edited by hand and a name carrying a separator would put the
// workspace outside the run's own directory, where the next run would find it.
func TestAWorkspaceNameCannotEscapeTheWorkRoot(t *testing.T) {
	for _, in := range []string{"../../etc", "a/b", "..", "."} {
		got := pathName(in)
		if strings.ContainsAny(got, `/\`) || got == ".." || got == "." {
			t.Errorf("pathName(%q) = %q, which is not one safe path component", in, got)
		}
	}

	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Samples: 1, ChainSessions: true})
	cells, err := plan(&dataset.Dataset{Tasks: []dataset.TaskSpec{
		{ID: "t", Session: "../../escape", PromptIndex: intPtr(0)},
	}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cfg.WorkRoot, "run1")
	if rel, err := filepath.Rel(root, cells[0].dir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("workspace %s is outside the run's directory %s", cells[0].dir, root)
	}
}

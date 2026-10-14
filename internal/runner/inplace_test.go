package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
)

// The inplace-orchestration tests.
//
// Two things about this workspace type are decided here rather than in the
// sandbox: where a cell's directory comes from, and what a run refuses to do
// with it. Both fail quietly when they are wrong -- a cell pointed at the wrong
// directory still runs, and a cell that shares a directory still writes a row --
// so each is asserted rather than assumed.

func inplaceTask(id, dir string) dataset.TaskSpec {
	return dataset.TaskSpec{
		ID:        id,
		Workspace: dataset.Workspace{Type: dataset.WorkspaceInplace, Dir: dir, Ref: "abc123"},
	}
}

// TestAnInplaceCellWorksInTheDirectoryTheTaskNames is the difference between
// this type and every other one: the cell does not get a directory of its own
// under the run's workspace root, because the whole point is to work where the
// recording worked.
func TestAnInplaceCellWorksInTheDirectoryTheTaskNames(t *testing.T) {
	dir := t.TempDir()
	cells := planFor(t, false, 1, inplaceTask("t1", dir))
	if len(cells) != 1 {
		t.Fatalf("planned %d cells, want 1", len(cells))
	}
	if cells[0].dir != dir {
		t.Errorf("cell runs in %s, want the directory the task names (%s)", cells[0].dir, dir)
	}
}

func TestAnOrdinaryCellStillGetsItsOwnDirectory(t *testing.T) {
	// The exception has to stay an exception: a task of any other kind keeps
	// being planned into a per-run, per-cell directory.
	cells := planFor(t, false, 1, dataset.TaskSpec{ID: "t1"})
	if len(cells) != 1 {
		t.Fatalf("planned %d cells, want 1", len(cells))
	}
	if !strings.Contains(cells[0].dir, "t1") {
		t.Errorf("cell runs in %s, want a per-cell directory under the run root", cells[0].dir)
	}
}

func TestAnInplaceRunRefusesToKeepTwoCellsInFlight(t *testing.T) {
	// Two attempts in one directory would write over each other somewhere
	// nobody could undo, and the directory is not the harness's to copy first.
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Concurrency: 2, Samples: 1})
	err := cfg.ValidateDataset(&dataset.Dataset{Tasks: []dataset.TaskSpec{inplaceTask("t1", t.TempDir())}})
	if err == nil {
		t.Fatal("a run accepted an inplace task with concurrency 2")
	}
	if !strings.Contains(err.Error(), "--concurrency 1") {
		t.Errorf("error = %v, want it to name the value that would work", err)
	}
	// And the task the refusal is about is named, because a dataset is rarely
	// one task long.
	if !strings.Contains(err.Error(), "t1") {
		t.Errorf("error = %v, want it to name the task", err)
	}
}

func TestAnInplaceRunRefusesMoreThanOneSample(t *testing.T) {
	// A second sample of one task would measure what the first one left behind
	// rather than the recorded starting state.
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Concurrency: 1, Samples: 2})
	err := cfg.ValidateDataset(&dataset.Dataset{Tasks: []dataset.TaskSpec{inplaceTask("t1", t.TempDir())}})
	if err == nil {
		t.Fatal("a run accepted an inplace task with 2 samples")
	}
	if !strings.Contains(err.Error(), "sample") {
		t.Errorf("error = %v, want it to say what the problem is", err)
	}
}

func TestAnInplaceRunAcceptsOneCellAtATime(t *testing.T) {
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Concurrency: 1, Samples: 1})
	if err := cfg.ValidateDataset(&dataset.Dataset{
		Tasks: []dataset.TaskSpec{inplaceTask("t1", t.TempDir())},
	}); err != nil {
		t.Errorf("a run with one cell at a time was refused: %v", err)
	}
}

func TestTheInplaceConstraintsDoNotTouchOtherDatasets(t *testing.T) {
	// Concurrency and samples are ordinary things to want. Only a task that
	// works in somebody else's directory makes them unsafe, and the check has
	// to stay quiet about every other dataset or it would be turned off.
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Concurrency: 8, Samples: 3})
	if err := cfg.ValidateDataset(&dataset.Dataset{
		Tasks: []dataset.TaskSpec{{ID: "t1"}, {ID: "t2"}},
	}); err != nil {
		t.Errorf("an ordinary dataset was refused: %v", err)
	}
	if err := cfg.ValidateDataset(&dataset.Dataset{
		Tasks: []dataset.TaskSpec{{
			ID:        "t1",
			Workspace: dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "https://example.com/r.git"},
		}},
	}); err != nil {
		t.Errorf("a git dataset was refused: %v", err)
	}
	// A nil dataset is not an error either: the check is about the tasks in it.
	if err := cfg.ValidateDataset(nil); err != nil {
		t.Errorf("ValidateDataset(nil) = %v, want nil", err)
	}
}

func TestTheValidateDatasetCheckIsSeparateFromTheChainCheck(t *testing.T) {
	// They are two different configuration mistakes and a caller has to be able
	// to tell them apart: one is about a flag that only makes sense for chained
	// runs, the other about a flag that does not make sense for inplace ones.
	// Folding them together would let a message name the wrong flag.
	cfg := withDefaults(Config{RunID: "run1", WorkRoot: t.TempDir(), Concurrency: 2, ChainSessions: true})
	if err := cfg.Validate(); err == nil {
		t.Fatal("the chain check stopped refusing a concurrent chained run")
	}
	if err := cfg.ValidateDataset(&dataset.Dataset{
		Tasks: []dataset.TaskSpec{chained("s1-p0", "s1", 0)},
	}); err != nil {
		t.Errorf("the dataset check refused a chained, non-inplace dataset: %v", err)
	}
}

// TestTheRunCountsTheTasksItWillReplayInPlace covers the number the report
// leans on.
//
// Nothing on an attempt row says its workspace was the user's own directory --
// the patch reads like any other and the directory is put back afterwards --
// so this count is the only place a reader can learn it. Two ways of getting it
// wrong are both silent: counting cells instead of tasks (the same run at k=3
// would claim three of its tasks were in place) and turning it into a flag (a
// half-bound dataset would claim all of it was).
func TestTheRunCountsTheTasksItWillReplayInPlace(t *testing.T) {
	mixed := &dataset.Dataset{Tasks: []dataset.TaskSpec{
		{ID: "t1"},
		inplaceTask("t2", t.TempDir()),
		{ID: "t3", Workspace: dataset.Workspace{Type: dataset.WorkspaceGit, Repo: "/repo"}},
		inplaceTask("t4", t.TempDir()),
	}}
	if got := inplaceTasks(mixed); got != 2 {
		t.Errorf("inplaceTasks = %d for a dataset with two bound tasks, want 2: it is a count "+
			"and not a flag, because --workspace-cwd binds a task only when its "+
			"start-of-session record survived", got)
	}

	// Nothing bound, and no dataset at all. Both are ordinary -- every dataset
	// written before this feature existed is the first one -- and both have to
	// read as "none", not as an error.
	plain := &dataset.Dataset{Tasks: []dataset.TaskSpec{{ID: "t1"}, {ID: "t2"}}}
	if got := inplaceTasks(plain); got != 0 {
		t.Errorf("inplaceTasks = %d for an unbound dataset, want 0", got)
	}
	if got := inplaceTasks(nil); got != 0 {
		t.Errorf("inplaceTasks(nil) = %d, want 0", got)
	}
}

func TestAnInplaceCellIsNotRemovedWhenTheCellIsDone(t *testing.T) {
	// The run did not create the directory, so the run does not get to take it
	// away. This is asserted through the plan because the removal is decided
	// there: a cell whose chain ends takes its workspace with it.
	dir := t.TempDir()
	cells := planFor(t, true, 1, inplaceTask("t1", dir), inplaceTask("t2", dir))
	if len(cells) != 2 {
		t.Fatalf("planned %d cells, want 2", len(cells))
	}
	for _, c := range cells {
		if c.dir != dir {
			t.Errorf("cell %s runs in %s, want %s", c.task.ID, c.dir, dir)
		}
	}
	if !strings.HasPrefix(cells[0].dir, filepath.Clean(dir)) {
		t.Errorf("the workspace was rewritten on the way through planning: %s", cells[0].dir)
	}
}

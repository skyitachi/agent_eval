package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/machine"
	"github.com/skyitachi/agenteval/internal/store"
)

// onMachine makes the replay believe it runs on id, and counts how often it
// asked.
func onMachine(t *testing.T, id machine.Identity, err error) *int {
	t.Helper()
	calls := 0
	old := currentMachine
	currentMachine = func() (machine.Identity, error) { calls++; return id, err }
	t.Cleanup(func() { currentMachine = old })
	return &calls
}

func recordedOn(id string, m *machine.Identity) dataset.TaskSpec {
	return dataset.TaskSpec{
		ID: id, Prompt: "x", Workspace: dataset.Workspace{Type: dataset.WorkspaceEmpty},
		ResolvedBaseline: &dataset.Baseline{ID: "s-" + id, Machine: m},
	}
}

var (
	here  = machine.Identity{ID: "aaaa", Hostname: "here", OS: "darwin", Arch: "arm64"}
	there = machine.Identity{ID: "bbbb", Hostname: "there", OS: "linux", Arch: "amd64"}
)

func TestCheckMachineLetsTheCollectingMachineReplay(t *testing.T) {
	onMachine(t, here, nil)
	same := here
	same.Hostname = "renamed" // a host name is not what makes it the same machine
	ds := &dataset.Dataset{Tasks: []dataset.TaskSpec{recordedOn("t1", &same)}}
	if err := CheckMachine(ds); err != nil {
		t.Fatalf("the machine that collected the task was refused: %v", err)
	}
}

func TestCheckMachineRefusesAnotherMachine(t *testing.T) {
	onMachine(t, here, nil)
	ds := &dataset.Dataset{Tasks: []dataset.TaskSpec{
		recordedOn("ok", &here), recordedOn("t1", &there), recordedOn("t2", &there),
	}}
	err := CheckMachine(ds)
	if err == nil {
		t.Fatal("a replay on another machine must be refused")
	}
	msg := err.Error()
	// The refusal names what to act on: which tasks, which two machines, and
	// what to do instead.
	for _, want := range []string{"2 task(s)", "t1, t2", "there (linux/amd64", "here (darwin/arm64",
		"re-collect"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should say %q, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "ok,") || strings.Contains(msg, ": ok") {
		t.Errorf("the task from this machine is not one of the refused ones:\n%s", msg)
	}
}

func TestCheckMachineLeavesUnstampedTasksAlone(t *testing.T) {
	// Hand-written tasks and ones collected before the identity existed have
	// always run anywhere. Nor is the machine probed for them: a platform that
	// cannot be identified must still run every dataset it could run before.
	calls := onMachine(t, machine.Identity{}, errors.New("no identifier"))
	ds := &dataset.Dataset{Tasks: []dataset.TaskSpec{
		{ID: "hand", Prompt: "x"},
		recordedOn("old", nil),
	}}
	if err := CheckMachine(ds); err != nil {
		t.Fatalf("an unstamped dataset was refused: %v", err)
	}
	if *calls != 0 {
		t.Errorf("the machine was probed %d time(s) for a dataset that names none", *calls)
	}
}

func TestCheckMachineRefusesWhenThisMachineCannotBeIdentified(t *testing.T) {
	// Running anyway would skip the one check the dataset asked for.
	onMachine(t, machine.Identity{}, errors.New("no identifier"))
	err := CheckMachine(&dataset.Dataset{Tasks: []dataset.TaskSpec{recordedOn("t1", &there)}})
	if err == nil || !strings.Contains(err.Error(), "cannot be identified") {
		t.Fatalf("err = %v, want a refusal saying this machine cannot be identified", err)
	}
}

func TestAReplayOnAnotherMachineLeavesNoRunBehind(t *testing.T) {
	onMachine(t, here, nil)
	st, err := store.Open(t.TempDir() + "/runs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ds := &dataset.Dataset{Dir: t.TempDir(), Tasks: []dataset.TaskSpec{recordedOn("t1", &there)}}
	_, err = Run(context.Background(), ds, Config{Store: st, Agent: agent.Spec{Kind: agent.KindMock}})
	if err == nil || !strings.Contains(err.Error(), "different machine") {
		t.Fatalf("err = %v, want the replay refused", err)
	}
	runs, err := st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a refused replay left %d run row(s) behind", len(runs))
	}
}

package session

import (
	"path/filepath"
	"strings"
	"testing"
)

const workbuddyFixture = "testdata/trace_workbuddy_sample.jsonl"

func TestWorkBuddyTracesLists(t *testing.T) {
	traces, err := WorkBuddyTraces("testdata")
	if err != nil {
		t.Fatalf("WorkBuddyTraces: %v", err)
	}
	var got *WorkBuddyTrace
	for i := range traces {
		if traces[i].ID == "0549439b-649e-4c4e-b84e-59f1755a2a60" {
			got = &traces[i]
		}
	}
	if got == nil {
		t.Fatalf("WorkBuddyTraces did not list the fixture; got %d trace(s)", len(traces))
	}
	if !strings.Contains(got.Title, "List the files") {
		t.Errorf("Title = %q, want it to contain the opening request", got.Title)
	}
	if got.Model != "space-bunny" {
		t.Errorf("Model = %q, want space-bunny", got.Model)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if got.Path == "" {
		t.Error("Path should not be empty")
	}
}

func TestParseWorkBuddySession(t *testing.T) {
	s, err := ParseWorkBuddy(workbuddyFixture)
	if err != nil {
		t.Fatalf("ParseWorkBuddy: %v", err)
	}

	if s.ID != "0549439b-649e-4c4e-b84e-59f1755a2a60" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.Source != WorkBuddySourceName {
		t.Errorf("Source = %q, want %q", s.Source, WorkBuddySourceName)
	}
	if len(s.Prompts) != 1 {
		t.Fatalf("Prompts = %d, want 1", len(s.Prompts))
	}
	if !strings.Contains(s.Prompts[0].Text, "List the files") {
		t.Errorf("Prompt[0] = %q", s.Prompts[0].Text)
	}

	// user turn + two assistant turns (the Bash call attaches to gen1's turn).
	if len(s.Turns) != 3 {
		t.Fatalf("Turns = %d, want 3 (user, assistant, assistant); roles: %v", len(s.Turns), turnRoles(s.Turns))
	}
	if s.Turns[0].Role != RoleUser {
		t.Errorf("Turns[0].Role = %q, want user", s.Turns[0].Role)
	}
	if s.Turns[1].Role != RoleAssistant || s.Turns[2].Role != RoleAssistant {
		t.Errorf("Turns[1..2] roles = %v, want [assistant assistant]", turnRoles(s.Turns)[1:])
	}
	if !strings.Contains(s.Turns[1].Text, "list the files") {
		t.Errorf("Turns[1].Text = %q", s.Turns[1].Text)
	}
	// The Bash tool call attaches to the generation it ran under.
	if len(s.Turns[1].Tools) != 1 || s.Turns[1].Tools[0].Name != "Bash" {
		t.Errorf("Turns[1].Tools = %+v, want one Bash call", s.Turns[1].Tools)
	}

	if len(s.ToolCalls) != 1 || s.ToolCalls[0].Name != "Bash" {
		t.Errorf("ToolCalls = %+v, want one Bash", s.ToolCalls)
	}
	if s.ToolResults != 1 {
		t.Errorf("ToolResults = %d, want 1", s.ToolResults)
	}

	if len(s.Models) != 1 || s.Models[0] != "space-bunny" {
		t.Errorf("Models = %v, want [space-bunny]", s.Models)
	}
	if s.Usage.InputTokens != 22466562 || s.Usage.OutputTokens != 24842 || s.Usage.CacheReadTokens != 22414606 {
		t.Errorf("Usage = %+v, want input 22466562 / output 24842 / cache 22414606", s.Usage)
	}

	if len(s.BaselineNotes) == 0 {
		t.Error("BaselineNotes should explain the trace format's limits")
	}
}

func TestResolveWorkBuddyByID(t *testing.T) {
	tr, err := ResolveWorkBuddy("testdata", "0549439b-649e-4c4e-b84e-59f1755a2a60")
	if err != nil {
		t.Fatalf("ResolveWorkBuddy: %v", err)
	}
	if !strings.HasSuffix(tr.Path, filepath.Base(workbuddyFixture)) {
		t.Errorf("Path = %q", tr.Path)
	}
}

func turnRoles(turns []Turn) []string {
	out := make([]string, len(turns))
	for i, t := range turns {
		out[i] = t.Role
	}
	return out
}

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/session"
)

// TestHintArgKeepsOrdinaryPathsBare covers the common case and the reason the
// helper exists: a hint is meant to be pasted, so a path that does not need
// quoting should not acquire any.
func TestHintArgKeepsOrdinaryPathsBare(t *testing.T) {
	for _, in := range []string{"reports/model-swap.db", "aeval.db", "./x/y.db", "/tmp/a.db"} {
		if got := hintArg(in); got != in {
			t.Errorf("hintArg(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestHintArgQuotesWhatTheShellWouldSplit is the half that matters: a database
// under a directory with a space is exactly where a naive hint stops being
// pasteable, and quoting it is cheaper than explaining that.
func TestHintArgQuotesWhatTheShellWouldSplit(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my runs/a.db", `'my runs/a.db'`},
		{"a b", `'a b'`},
		{"tab\there", "'tab\there'"},
		{"$HOME/x.db", `'$HOME/x.db'`},
		{"it's.db", `'it'\''s.db'`},
	}
	for _, c := range cases {
		if got := hintArg(c.in); got != c.want {
			t.Errorf("hintArg(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestWarnUnresolvableCommandsSpeaksOnlyWhenItCanBeActedOn covers the pre-flight
// warning.
//
// It exists to stop an attempt being paid for, which it can only do before the
// run -- so it has to say which command, and which flags would load it. The
// other half is that it stays quiet the rest of the time: a warning that fires
// on working runs is a warning nobody reads.
// TestWarnDroppedPromptsSpeaksOnlyWhenPromptsWereLost covers the collect
// warning.
//
// The default split collects the opening request, so a session the human kept
// asking in loses the rest of it silently: nothing in the tasks says how many
// prompts the recording held. The warning is what keeps that from reading as a
// complete collection -- and it has to stay quiet on the sessions where nothing
// was dropped, or it becomes a warning nobody reads.
func TestWarnDroppedPromptsSpeaksOnlyWhenPromptsWereLost(t *testing.T) {
	item := func(sessionID string, prompts int) session.Item {
		return session.Item{Task: dataset.TaskSpec{Metadata: map[string]any{
			"session_id":      sessionID,
			"session_prompts": prompts,
		}}}
	}

	// One task from a three-prompt session: two prompts are not in the dataset.
	var buf bytes.Buffer
	warnDroppedPrompts(&buf, []session.Item{item("1f2114cd-b37c-41fe-9127-60a6417c6794", 3)}, session.SplitSession)
	got := buf.String()
	for _, want := range []string{"1 session(s)", "1f2114cd", "(1 of 3)", "--split turn"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning should mention %q, got:\n%s", want, got)
		}
	}

	// The turn split collected all three, so there is nothing to say.
	buf.Reset()
	warnDroppedPrompts(&buf, []session.Item{
		item("s1", 3), item("s1", 3), item("s1", 3),
	}, session.SplitTurn)
	if buf.Len() != 0 {
		t.Errorf("a collection that kept every prompt warned anyway:\n%s", buf.String())
	}

	// Under the turn split the drops are --min-prompt-runes' doing, so the
	// advice has to name that flag rather than the split already in use.
	buf.Reset()
	warnDroppedPrompts(&buf, []session.Item{item("s2", 5), item("s2", 5)}, session.SplitTurn)
	if got := buf.String(); !strings.Contains(got, "--min-prompt-runes") ||
		strings.Contains(got, "Re-run with --split turn") {
		t.Errorf("the turn split must not be advised to use itself, got:\n%s", got)
	}

	// A task with no recording behind it -- written by hand -- has no session
	// to compare against and is not a drop.
	buf.Reset()
	warnDroppedPrompts(&buf, []session.Item{{Task: dataset.TaskSpec{ID: "hand-written"}}}, session.SplitSession)
	if buf.Len() != 0 {
		t.Errorf("a hand-written task has no prompt count to fall short of:\n%s", buf.String())
	}
}

func TestWarnUnresolvableCommandsSpeaksOnlyWhenItCanBeActedOn(t *testing.T) {
	ds := &dataset.Dataset{Tasks: []dataset.TaskSpec{
		{ID: "a", ResolvedPrompt: "/bff-oncall  测试环境 新服务"},
		{ID: "b", ResolvedPrompt: "fix the failing test"},
		{ID: "c", ResolvedPrompt: "/bff-oncall 再来一次"},
	}}
	cleanRoom := agent.Spec{Isolation: &agent.Isolation{
		Bare: true, StrictMCPConfig: true, DisableSlashCommands: true,
	}}

	var buf bytes.Buffer
	warnUnresolvableCommands(&buf, ds, cleanRoom)
	got := buf.String()
	// Two tasks, one distinct command: the count is about exposure, the names
	// are about what to do next.
	for _, want := range []string{"2 task(s)", "/bff-oncall", "bare=true", "disable-slash-commands=true", "--bare=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning should mention %q, got:\n%s", want, got)
		}
	}

	// Skills on: the command has somewhere to resolve, so there is nothing to
	// warn about even though the prompt looks the same.
	buf.Reset()
	warnUnresolvableCommands(&buf, ds, agent.Spec{Isolation: &agent.Isolation{StrictMCPConfig: true}})
	if buf.Len() != 0 {
		t.Errorf("a run that loads the user's skills warned anyway:\n%s", buf.String())
	}

	// A clean room over ordinary prompts is the common case, and it is silent:
	// the flags alone are not the trigger, the prompts are.
	buf.Reset()
	ordinary := &dataset.Dataset{Tasks: []dataset.TaskSpec{{ID: "b", ResolvedPrompt: "fix the failing test"}}}
	warnUnresolvableCommands(&buf, ordinary, cleanRoom)
	if buf.Len() != 0 {
		t.Errorf("prompts that invoke nothing warned:\n%s", buf.String())
	}
}

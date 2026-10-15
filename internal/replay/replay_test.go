package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

const fixture = "../../tests/testdata/session/basic.jsonl"

// mockPresets drives the mock agent with the one-turn fixture, so a replay
// runs end to end without a model.
func mockPresets(t *testing.T) *Presets {
	t.Helper()
	bin, err := filepath.Abs("../../tests/testdata/agents/one-turn.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	skill := filepath.Join(dir, "skills", "demo")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `models: [sonnet, opus]
default_model: sonnet
harnesses:
  - name: scripted
    notes: the one-turn stand-in
    agent:
      kind: mock
      bin: ` + bin + `
  - name: scripted-open
    agent:
      kind: mock
      bin: ` + bin + `
      isolation: {bare: false, strict_mcp_config: true, disable_slash_commands: false}
skills:
  - name: none
  - name: demo
    plugin_dirs: [skills/demo]
`
	path := filepath.Join(dir, "presets.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPresets(path)
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	return p
}

// claudeRoot lays the fixture transcript out the way ~/.claude/projects is.
func claudeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "-work-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	title := `{"type":"ai-title","sessionId":"aaaabbbb-1111-2222-3333-444455556666","aiTitle":"Fix the subtraction"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "aaaabbbb-1111-2222-3333-444455556666.jsonl"),
		append(body, title...), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := NewManager(Config{
		Store:    st,
		Catalog:  &session.Catalog{ClaudeRoot: claudeRoot(t)},
		Presets:  mockPresets(t),
		WorkRoot: t.TempDir(),
		Timeout:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, st
}

func waitFinished(t *testing.T, st *store.Store, id string) store.Replay {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rp, err := st.GetReplay(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rp.Finished() {
			return rp
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replay %s did not finish", id)
	return store.Replay{}
}

const sessionID = "aaaabbbb-1111-2222-3333-444455556666"

func TestAReplayRunsTheSessionAndRegistersItsReport(t *testing.T) {
	m, st := newManager(t)
	ctx := context.Background()
	rp, err := m.Submit(ctx, Request{Source: session.CatalogClaude, SessionID: sessionID,
		Choice: Choice{Harness: "scripted", Skill: "none", Model: "opus"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rp.SessionTitle != "Fix the subtraction" {
		t.Errorf("title = %q, want the transcript's ai-title", rp.SessionTitle)
	}
	done := waitFinished(t, st, rp.ID)
	if done.Status != store.ReplayCompleted {
		t.Fatalf("replay = %s: %s\nlog:\n%s", done.Status, done.Error, done.Log)
	}
	if done.RunID == "" || done.ReportID == "" {
		t.Fatalf("a completed replay must name its run and report: %+v", done)
	}

	run, err := st.GetRun(ctx, done.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentModel != "opus" || run.AgentKind != agent.KindMock {
		t.Errorf("the run drove %s/%s, want the picked mock/opus", run.AgentKind, run.AgentModel)
	}
	// The fixture holds two prompts. A replay of the session replays both,
	// chained, because the second asks about what the first produced.
	cells, _ := st.ListAttempts(ctx, done.RunID)
	if len(cells) != 2 {
		t.Errorf("cells = %d, want one per prompt", len(cells))
	}
	if !run.ChainSessions {
		t.Error("a multi-prompt replay must chain its turns in one workspace")
	}
	reg, err := st.GetReport(ctx, done.ReportID)
	if err != nil {
		t.Fatalf("the registered report is not in the registry: %v", err)
	}
	if reg.Kind != store.ReportKindRun || !strings.Contains(reg.ParamsJSON, done.RunID) {
		t.Errorf("report %+v is not the run report of %s", reg, done.RunID)
	}
	for _, want := range []string{"collected 2 task(s)", "[2/2]", "report " + done.ReportID} {
		if !strings.Contains(done.Log, want) {
			t.Errorf("the log should say %q:\n%s", want, done.Log)
		}
	}
}

func TestSubmitRefusesWhatIsNotOnOffer(t *testing.T) {
	m, st := newManager(t)
	ctx := context.Background()
	cases := map[string]struct {
		req  Request
		want string
	}{
		"unknown session": {Request{Source: "claude", SessionID: "nope", Choice: Choice{}}, "no claude session"},
		"unknown source":  {Request{Source: "vim", SessionID: sessionID}, "no session source"},
		"unknown harness": {Request{Source: "claude", SessionID: sessionID, Choice: Choice{Harness: "rm -rf"}}, "no harness"},
		"unknown skill":   {Request{Source: "claude", SessionID: sessionID, Choice: Choice{Skill: "/etc"}}, "no skill set"},
		"model with a newline": {Request{Source: "claude", SessionID: sessionID,
			Choice: Choice{Model: "sonnet\n--dangerously"}}, "does not look like a model id"},
		"model with a space": {Request{Source: "claude", SessionID: sessionID,
			Choice: Choice{Model: "sonnet --bare"}}, "does not look like a model id"},
		// The clean-room flag removes every skill, so a replay with skills
		// under it would report that they did not matter.
		"skills that cannot load": {Request{Source: "claude", SessionID: sessionID,
			Choice: Choice{Harness: "scripted", Skill: "demo"}}, "cannot load"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := m.Submit(ctx, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if rows, _ := st.ListReplays(ctx, store.ReplayFilter{}); len(rows) != 0 {
		t.Errorf("refused requests left %d replay row(s)", len(rows))
	}
}

func TestSkillsLoadUnderAHarnessThatKeepsThem(t *testing.T) {
	p := mockPresets(t)
	spec, _, err := p.Resolve(Choice{Harness: "scripted-open", Skill: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.PluginDirs) != 1 || !spec.SkillsActive() {
		t.Errorf("spec = %+v, want the demo skill loaded", spec)
	}
	if spec.Model != "sonnet" {
		t.Errorf("model = %q, want the presets' default", spec.Model)
	}
}

func TestPresetsRefuseAHarnessThatSetsAnotherChoice(t *testing.T) {
	dir := t.TempDir()
	for name, doc := range map[string]string{
		"model":        "harnesses:\n  - name: h\n    agent: {model: opus}\n",
		"plugin_dirs":  "harnesses:\n  - name: h\n    agent: {plugin_dirs: [x]}\n",
		"unknown key":  "harnesses:\n  - name: h\n    agent: {binn: claude}\n",
		"missing dir":  "harnesses:\n  - name: h\nskills:\n  - name: s\n    plugin_dirs: [nope]\n",
		"bad name":     "harnesses:\n  - name: 'a b'\n",
		"no harnesses": "skills: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPresets(path); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

func TestCancellingAQueuedReplayKeepsItFromRunning(t *testing.T) {
	m, st := newManager(t)
	ctx := context.Background()
	// Hold the worker so the second replay stays queued.
	first, err := m.Submit(ctx, Request{Source: "claude", SessionID: sessionID, Choice: Choice{Harness: "scripted"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Submit(ctx, Request{Source: "claude", SessionID: sessionID, Choice: Choice{Harness: "scripted"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(ctx, second.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFinished(t, st, first.ID)
	got := waitFinished(t, st, second.ID)
	if got.Status != store.ReplayFailed || got.RunID != "" {
		t.Errorf("a cancelled replay = %+v, want failed with no run", got)
	}
	if err := m.Cancel(ctx, second.ID); err == nil {
		t.Error("cancelling a finished replay must say it has finished")
	}
}

func TestANewManagerClosesWhatAnEarlierProcessLeftRunning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateReplay(ctx, store.Replay{ID: "stale", Status: store.ReplayRunning}); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{Store: st, Catalog: &session.Catalog{}, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	rp, _ := st.GetReplay(ctx, "stale")
	if rp.Status != store.ReplayFailed || rp.Error != AbandonedReason {
		t.Errorf("a replay left running = %+v, want it closed as abandoned", rp)
	}
}

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplayWorkspacesAreRecognised(t *testing.T) {
	cases := map[string]bool{
		// The directory a replay of a session runs in.
		"/Users/x/lab/agent_eval/replays/dd415a7a0cde9c20/work/4d92d2be-53a9-4ac5-afd0-ab107f512304/" +
			"3c11e6dc-848b-49f5-a399-5ca8d6775755/s0": true,
		// The work root is arbitrary; the replay id, "work" and the sample
		// directory are not.
		"/tmp/somewhere/0123456789abcdef/work/run/key/s12": true,
		// A recording.
		"/work/proj": false,
		"/Users/x/lab/agent_eval/replays/dd415a7a0cde9c20/work/proj/s0":       false,
		"/Users/x/lab/agent_eval/notareplays/dd415a7a0cde9c2/work/run/key/s0": false,
		"/Users/x/lab/agent_eval/replays/dd415a7a0cde9c2/work/run/key/s0":     false,
		"/Users/x/lab/agent_eval/replays/DD415A7A0CDE9C20/work/run/key/s0":    false,
		"/Users/x/lab/agent_eval/replays/dd415a7a0cde9c20/works/run/key/s0":   false,
		"/Users/x/lab/agent_eval/replays/dd415a7a0cde9c20/work/run/key/sx":    false,
		"": false,
	}
	for cwd, want := range cases {
		if got := IsReplayWorkspace(cwd); got != want {
			t.Errorf("IsReplayWorkspace(%q) = %v, want %v", cwd, got, want)
		}
	}
}

// TestACatalogDoesNotOfferTheOutputOfAReplay: a replay writes a transcript of
// its own, under its own workspace. That transcript has a human prompt in it
// (the replayed one), so the machine-driven test alone would let it through and
// the page would offer to replay the replay.
func TestACatalogDoesNotOfferTheOutputOfAReplay(t *testing.T) {
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	replayCwd := "/tmp/aeval/replays/0123456789abcdef/work/run1/aaaabbbb-1111-2222-3333-444455556666/s0"
	replayBody := strings.ReplaceAll(string(body), `"cwd":"/work/proj"`, `"cwd":"`+replayCwd+`"`)
	if replayBody == string(body) {
		t.Fatal("the fixture no longer records /work/proj; update the cwd this test replaces")
	}

	root := t.TempDir()
	write := func(dir, id, contents string) {
		t.Helper()
		if err := writeFile(filepath.Join(root, dir, id+".jsonl"), contents); err != nil {
			t.Fatal(err)
		}
	}
	write("-work-proj", "aaaabbbb-1111-2222-3333-444455556666", string(body))
	write("-tmp-aeval-replays-0123456789abcdef-work-run1-aaaabbbb-1111-2222-3333-444455556666-s0",
		"ccccdddd-1111-2222-3333-444455556666", replayBody)

	c := &Catalog{ClaudeRoot: root}
	entries, errs := c.List()
	if err := errs[CatalogClaude]; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "aaaabbbb-1111-2222-3333-444455556666" {
		t.Fatalf("the listing = %+v, want only the recorded session", entries)
	}
	if _, err := c.Find(CatalogClaude, "ccccdddd-1111-2222-3333-444455556666"); err == nil {
		t.Error("the replay's own transcript is still offered as a session to replay")
	}
}

func TestAReplayTranscriptIsNotARecording(t *testing.T) {
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	produced, err := IsReplayTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if produced {
		t.Error("a recording was reported as a replay's output")
	}

	replay := strings.ReplaceAll(string(body), `"cwd":"/work/proj"`,
		`"cwd":"/tmp/replays/0123456789abcdef/work/run/key/s0"`)
	tmp := filepath.Join(dir, "y.jsonl")
	if err := os.WriteFile(tmp, []byte(replay), 0o644); err != nil {
		t.Fatal(err)
	}
	produced, err = IsReplayTranscript(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !produced {
		t.Error("a replay's transcript was reported as a recording")
	}
}

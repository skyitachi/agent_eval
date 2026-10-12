package trace

import (
	"strings"
	"testing"
)

// Which model served an attempt is reported by the stream in two places that
// answer two different questions, and a report that shows only one of them can
// state the wrong model with a straight face:
//
//   - the init event announces what the CLI is about to use, before any request
//     has been made;
//   - the modelUsage block is keyed by the models that were actually billed.
//
// A run can name the same model in both and be unremarkable, or be handed a
// different one by a fallback or a gateway -- which appears as a second key and
// nowhere else.

func TestModelsNamesWhatWasBilledNotOnlyWhatWasAnnounced(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","model":"announced-model"}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","model":"announced-model","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","num_turns":1,"result":"hi","modelUsage":{"billed-b":{"inputTokens":10},"billed-a":{"inputTokens":5}}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := tr.Models()
	if len(got) != 2 || got[0] != "billed-a" || got[1] != "billed-b" {
		t.Errorf("Models() = %v, want the two models that billed tokens, sorted", got)
	}
	// The announcement is still readable, because it is the other half of the
	// claim: it says what was asked for, not what answered.
	if tr.Model != "announced-model" {
		t.Errorf("the announced model should survive the per-model totals, got %q", tr.Model)
	}
}

func TestModelsFallsBackToTheAnnouncedNameWhenNothingWasBilled(t *testing.T) {
	// An older CLI emits no per-model breakdown, and for those the init name
	// is the only answer there is. Returning nothing would report a run whose
	// model is known as one whose model is unknown.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","model":"only-announced"}`,
		`{"type":"result","subtype":"success","num_turns":1,"result":"hi","usage":{"input_tokens":10}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := tr.Models(); len(got) != 1 || got[0] != "only-announced" {
		t.Errorf("Models() = %v, want the announced name", got)
	}
}

func TestModelsOfAStreamThatNamedNoneIsEmpty(t *testing.T) {
	// Not a model called "": the endpoint declined to say, and nil is how that
	// reaches the report as "not recorded" rather than as a blank name.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"result","subtype":"success","num_turns":1,"result":"hi","usage":{"input_tokens":10}}`,
	}, "\n")
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := tr.Models(); len(got) != 0 {
		t.Errorf("Models() = %v, want nothing", got)
	}
}

// A blank key in the per-model block is the endpoint declining to name a model,
// which must not reach the page as a model named "".
func TestABlankModelKeyIsNotAModel(t *testing.T) {
	stream := `{"type":"result","subtype":"success","num_turns":1,"result":"hi","modelUsage":{"":{"inputTokens":10},"real":{"inputTokens":5}}}`
	tr, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := tr.Models(); len(got) != 1 || got[0] != "real" {
		t.Errorf("Models() = %v, want only the named model", got)
	}
}

// TestModelsReadsARealStream pins the two sources against a stream recorded by
// the real CLI, because the fixtures above are shapes I wrote and this one is
// not.
func TestModelsReadsARealStream(t *testing.T) {
	tr, err := ParseFile("testdata/read_tool_stream.jsonl")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := tr.Models()
	if len(got) != 1 || got[0] != "deepseek-v4-flash" {
		t.Errorf("Models() = %v, want the model the stream was billed to", got)
	}
	if !SameModel(got[0], tr.Model) {
		t.Errorf("the billed model %q and the announced one %q should agree", got[0], tr.Model)
	}
}

// TestSameModelAcceptsAnAliasAndRejectsAnotherModel is the rule that decides
// whether a run is reported as having been served something else.
//
// String equality is the wrong test and would fire on ordinary runs:
// `--model sonnet` is answered by a stream naming
// `claude-sonnet-4-5-20250929`, and third-party gateways spell it their own way
// on top of that. A warning that fires on every run is one nobody reads, so
// containment in either direction is the most that strings can honestly carry.
func TestSameModelAcceptsAnAliasAndRejectsAnotherModel(t *testing.T) {
	same := [][2]string{
		{"sonnet", "claude-sonnet-4-5-20250929"},
		{"claude-sonnet-4-5-20250929", "sonnet"},
		{"Sonnet", "SONNET"},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
	}
	for _, c := range same {
		if !SameModel(c[0], c[1]) {
			t.Errorf("SameModel(%q, %q) = false, want true: the alias resolves to that model", c[0], c[1])
		}
	}
	differ := [][2]string{
		{"sonnet", "haiku-4"},
		{"claude-opus-4-7", "claude-sonnet-4-5"},
		// Nothing to compare is not agreement. Both directions of the claim
		// would be invented, so neither is made.
		{"", "sonnet"},
		{"sonnet", ""},
		{"", ""},
	}
	for _, c := range differ {
		if SameModel(c[0], c[1]) {
			t.Errorf("SameModel(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

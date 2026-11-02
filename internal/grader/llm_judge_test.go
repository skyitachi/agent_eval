package grader

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skyitachi/agenteval/internal/dataset"
)

// judgeStub answers every call with one canned reply and hands the request it
// received to the test.
func judgeStub(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("the judge's request is not JSON: %v", err)
		}
		seen = req
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// A reasoning model can spend the whole output budget thinking and never emit
// a text block. That reply has no JSON in it, and reporting it as "no JSON
// object found; raw reply: " sends the reader hunting for a parsing bug: the
// knob is max_tokens, and the message has to say so. This failure was real --
// the shipped judge config asked for 2048 and got an empty answer.
func TestAJudgeThatReasonsItselfOutOfABudgetSaysSo(t *testing.T) {
	srv, _ := judgeStub(t, `{"id":"m","type":"message","role":"assistant","model":"deepseek-flash",
	  "content":[{"type":"thinking","thinking":"Let me consider the diff at length...","signature":"s"}],
	  "stop_reason":"max_tokens","usage":{"input_tokens":982,"output_tokens":2048}}`)
	t.Setenv("AEVAL_TEST_JUDGE_TOKEN", "not-a-real-key")

	f := newFixture(t)
	f.task = dataset.TaskSpec{ID: "t1", Prompt: "fix the query"}
	f.answer("The query is fixed.")
	g, err := Get("llm_judge")
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Grade(context.Background(), f.ctx(), f.spec("llm_judge", map[string]any{
		"base_url":       srv.URL,
		"auth_token_env": "AEVAL_TEST_JUDGE_TOKEN",
		"model":          "deepseek-flash",
		"input":          "final_answer",
		"rubric":         "The fix is correct.",
	}))
	if err == nil {
		t.Fatal("a reply with no text produced a verdict; it should refuse")
	}
	msg := err.Error()
	for _, want := range []string{"max_tokens", "2048", "deepseek-flash"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should name %q: the reader needs to know which knob to turn", msg, want)
		}
	}
	if strings.Contains(msg, "no JSON object found") {
		t.Errorf("error %q blames the reply's format for what is a budget problem", msg)
	}
}

// The budget is a configuration key, so it has to reach the wire -- a config
// that is read and then ignored would look configured and behave as if it were
// not.
func TestTheJudgeBudgetReachesTheRequest(t *testing.T) {
	srv, seen := judgeStub(t, `{"id":"m","type":"message","role":"assistant","model":"deepseek-flash",
	  "content":[{"type":"text","text":"{\"score\": 0.9, \"passed\": true, \"reason\": \"done\"}"}],
	  "stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}`)
	t.Setenv("AEVAL_TEST_JUDGE_TOKEN", "not-a-real-key")

	f := newFixture(t)
	f.task = dataset.TaskSpec{ID: "t1", Prompt: "fix the query"}
	f.answer("The query is fixed.")
	g, err := Get("llm_judge")
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.Grade(context.Background(), f.ctx(), f.spec("llm_judge", map[string]any{
		"base_url":       srv.URL,
		"auth_token_env": "AEVAL_TEST_JUDGE_TOKEN",
		"model":          "deepseek-flash",
		"input":          "final_answer",
		"rubric":         "The fix is correct.",
		"max_tokens":     float64(16384),
	}))
	if err != nil {
		t.Fatalf("a well-formed verdict was refused: %v", err)
	}
	if !res.Passed || res.Score != 0.9 {
		t.Errorf("verdict read as %v/%v, want a pass at 0.9", res.Passed, res.Score)
	}
	if got := (*seen)["max_tokens"]; got != float64(16384) {
		t.Errorf("request carried max_tokens %v, want the configured 16384", got)
	}
	if got := (*seen)["model"]; got != "deepseek-flash" {
		t.Errorf("request asked for model %v, want deepseek-flash", got)
	}
}

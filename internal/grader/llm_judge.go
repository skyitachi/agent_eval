package grader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/llm"
)

func init() { Register(&LLMJudge{}) }

// LLMJudge scores an open-ended answer against a rubric using a language model.
//
// # Design choices that matter for a usable judge
//
//   - The judge is asked for a strict JSON object, but the response is parsed
//     leniently (see llm.ExtractJSON). A single stray sentence of prose should
//     not turn into a zero.
//   - The judge's own model is configurable and independent of the model under
//     test, so a run can cross-check a cheap model's work with a stronger one.
//   - The prompt pins the output contract and forbids grading anything other than
//     the criteria, which is the main failure mode of rubric judges: rewarding
//     verbosity or style instead of correctness.
//   - A parse failure is reported as a grader error rather than a silent zero, so
//     a broken judge never masquerades as a failing agent.
type LLMJudge struct{}

func (j *LLMJudge) Type() string { return "llm_judge" }

type llmJudgeConfig struct {
	// Rubric is the natural-language description of what good looks like.
	Rubric string `json:"rubric,omitempty"`
	// Criteria are discrete, individually weighted checks. When present they are
	// listed in the prompt and each is scored separately, which makes the verdict
	// auditable instead of a single opaque number.
	Criteria []judgeCriterion `json:"criteria,omitempty"`

	// Model is the judge model. Defaults to the LLM client's resolved model.
	Model string `json:"model,omitempty"`
	// BaseURL overrides the API root, e.g. "https://api.deepseek.com/anthropic".
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv / AuthTokenEnv name the credential environment variables.
	APIKeyEnv    string `json:"api_key_env,omitempty"`
	AuthTokenEnv string `json:"auth_token_env,omitempty"`

	// Input selects the graded material: "final_answer" (default), "diff", or
	// "both".
	Input string `json:"input,omitempty"`
	// Reference is an optional ideal answer shown to the judge.
	Reference string `json:"reference,omitempty"`
	// ReferenceFile loads Reference from a dataset-relative file.
	ReferenceFile string `json:"reference_file,omitempty"`
	// TaskContext, when set, adds the task prompt to the judge's context.
	TaskContext *bool `json:"task_context,omitempty"`

	// Threshold is the score at which the grader passes. Default 0.67.
	Threshold float64 `json:"threshold,omitempty"`
	// MaxTokens bounds the judge's reply. Default 2048.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature defaults to 0 for the most repeatable verdicts available.
	Temperature float64 `json:"temperature,omitempty"`
	// TimeoutS bounds one judge call. Default 180.
	TimeoutS int `json:"timeout_s,omitempty"`
	// MaxInputChars truncates oversized inputs. Default 60000.
	MaxInputChars int `json:"max_input_chars,omitempty"`
}

type judgeCriterion struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Weight      float64 `json:"weight,omitempty"`
}

// judgeVerdict is the JSON contract the model must satisfy.
type judgeVerdict struct {
	Score    *float64 `json:"score"`
	Passed   *bool    `json:"passed"`
	Reason   string   `json:"reason"`
	Criteria []struct {
		Name   string  `json:"name"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	} `json:"criteria"`
}

func (j *LLMJudge) Grade(ctx context.Context, gc *Context, spec dataset.GraderSpec) (*Result, error) {
	var cfg llmJudgeConfig
	if err := DecodeConfig(spec, &cfg); err != nil {
		return nil, err
	}

	subject, err := j.collectInput(gc, cfg)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(subject) == "" {
		return &Result{
			Passed:  false,
			Score:   0,
			Reason:  "nothing to judge: the agent produced an empty answer",
			Details: map[string]any{"input": cfg.Input},
		}, nil
	}

	reference := cfg.Reference
	if cfg.ReferenceFile != "" {
		data, err := gc.Sandbox.ReadFile(cfg.ReferenceFile)
		if err != nil {
			// Fall back to a dataset-relative read so a reference can live beside
			// the dataset rather than inside the workspace.
			if err := CopyInto(gc, cfg.ReferenceFile, HarnessDir); err != nil {
				return nil, fmt.Errorf("reference_file %s: %w", cfg.ReferenceFile, err)
			}
			data, err = gc.Sandbox.ReadFile(HarnessDir + "/" + baseName(cfg.ReferenceFile))
			if err != nil {
				return nil, fmt.Errorf("reference_file %s: %w", cfg.ReferenceFile, err)
			}
		}
		reference = string(data)
	}

	client := llm.New(llm.Config{
		BaseURL:      cfg.BaseURL,
		APIKeyEnv:    cfg.APIKeyEnv,
		AuthTokenEnv: cfg.AuthTokenEnv,
		Model:        cfg.Model,
		Timeout:      durationOr(cfg.TimeoutS, 180*time.Second),
	})
	if !client.Available() {
		return nil, fmt.Errorf("llm_judge: %w", client.MissingCredentialsError())
	}

	includeTask := true
	if cfg.TaskContext != nil {
		includeTask = *cfg.TaskContext
	}
	limit := cfg.MaxInputChars
	if limit <= 0 {
		limit = 60000
	}

	prompt := buildJudgePrompt(judgePromptInput{
		Rubric:        cfg.Rubric,
		Criteria:      cfg.Criteria,
		TaskPrompt:    gc.Task.ResolvedPrompt,
		IncludeTask:   includeTask,
		Reference:     reference,
		Subject:       clip(subject, limit),
		SubjectLabel:  subjectLabel(cfg.Input),
		TruncatedNote: len(subject) > limit,
	})

	start := time.Now()
	resp, err := client.Complete(ctx, llm.Request{
		Model:       cfg.Model,
		System:      judgeSystemPrompt,
		Messages:    []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:   orDefaultInt(cfg.MaxTokens, 2048),
		Temperature: cfg.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("llm_judge call: %w", err)
	}

	verdict, err := parseVerdict(resp.Text)
	if err != nil {
		return nil, fmt.Errorf("llm_judge returned an unusable verdict: %w; raw reply: %s",
			err, clip(resp.Text, 800))
	}

	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = 0.67
	}
	score := 0.0
	if verdict.Score != nil {
		score = clamp01(*verdict.Score)
	} else if len(verdict.Criteria) > 0 {
		// Derive the score from per-criterion results when the model omitted the
		// aggregate, weighting by the configured criteria.
		score = weightedCriteriaScore(verdict, cfg.Criteria)
	}
	passed := score >= threshold
	if verdict.Passed != nil {
		passed = *verdict.Passed && score >= threshold*0.5
	}

	details := map[string]any{
		"judge_model":      resp.Model,
		"judge_base_url":   client.BaseURL(),
		"score":            score,
		"threshold":        threshold,
		"subject_chars":    len(subject),
		"input":            subjectLabel(cfg.Input),
		"judge_latency_ms": resp.Latency.Milliseconds(),
		"judge_usage": map[string]any{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
		},
		"truncated":   len(subject) > limit,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	if len(verdict.Criteria) > 0 {
		criteria := make([]map[string]any, 0, len(verdict.Criteria))
		for _, c := range verdict.Criteria {
			criteria = append(criteria, map[string]any{
				"name":   c.Name,
				"score":  c.Score,
				"reason": c.Reason,
			})
		}
		details["criteria"] = criteria
	}

	reason := verdict.Reason
	if reason == "" {
		reason = fmt.Sprintf("judge scored %0.2f against a threshold of %0.2f", score, threshold)
	}

	return &Result{
		Passed:  passed,
		Score:   score,
		Reason:  reason,
		Details: details,
	}, nil
}

// collectInput assembles the material shown to the judge.
func (j *LLMJudge) collectInput(gc *Context, cfg llmJudgeConfig) (string, error) {
	switch orDefaultStr(cfg.Input, "final_answer") {
	case "final_answer", "answer":
		return gc.FinalAnswer(), nil
	case "diff":
		return gc.Diff()
	case "both":
		diff, err := gc.Diff()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("### Agent final answer\n\n")
		b.WriteString(gc.FinalAnswer())
		b.WriteString("\n\n### Workspace diff\n\n```diff\n")
		b.WriteString(diff)
		b.WriteString("\n```\n")
		return b.String(), nil
	default:
		return "", fmt.Errorf("unknown input %q (want final_answer, diff or both)", cfg.Input)
	}
}

func subjectLabel(input string) string {
	switch orDefaultStr(input, "final_answer") {
	case "answer", "final_answer":
		return "final_answer"
	default:
		return input
	}
}

func parseVerdict(text string) (*judgeVerdict, error) {
	raw, ok := llm.ExtractJSON(text)
	if !ok {
		return nil, fmt.Errorf("no JSON object found")
	}
	var v judgeVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v.Score == nil && v.Passed == nil && len(v.Criteria) == 0 {
		return nil, fmt.Errorf("JSON contained none of score, passed or criteria")
	}
	return &v, nil
}

// weightedCriteriaScore aggregates per-criterion scores using the configured
// weights, falling back to an unweighted mean.
func weightedCriteriaScore(v *judgeVerdict, configured []judgeCriterion) float64 {
	weights := map[string]float64{}
	for _, c := range configured {
		weights[strings.ToLower(strings.TrimSpace(c.Name))] = c.Weight
	}
	num, den := 0.0, 0.0
	usedWeights := false
	for _, c := range v.Criteria {
		w := weights[strings.ToLower(strings.TrimSpace(c.Name))]
		if w > 0 {
			usedWeights = true
		} else {
			w = 1
		}
		num += w * clamp01(c.Score)
		den += w
	}
	if den == 0 {
		return 0
	}
	if !usedWeights && len(configured) > 0 {
		// The model's criterion names did not line up with the configured ones.
		// An unweighted mean is still a defensible reading of its verdict.
		return num / float64(len(v.Criteria))
	}
	return num / den
}

const judgeSystemPrompt = `You are a strict, impartial evaluator of AI agent output.

Rules:
1. Judge only against the supplied rubric and criteria. Do not reward length,
   confidence, formatting or style.
2. Base every judgement on the supplied material alone. Never assume work that is
   not shown was done.
3. If the material is empty, off-topic, or does not attempt the task, score 0.
4. Be calibrated: 1.0 means fully correct and complete, 0.5 means partially
   correct, 0 means wrong or missing.
5. Reply with a single JSON object and nothing else. No prose before or after, no
   markdown fence.

Required JSON shape:
{
  "score": <number between 0 and 1>,
  "passed": <true if the work meets the rubric>,
  "reason": "<one or two sentences>",
  "criteria": [{"name": "<criterion name>", "score": <0..1>, "reason": "<short>"}]
}`

type judgePromptInput struct {
	Rubric        string
	Criteria      []judgeCriterion
	TaskPrompt    string
	IncludeTask   bool
	Reference     string
	Subject       string
	SubjectLabel  string
	TruncatedNote bool
}

func buildJudgePrompt(in judgePromptInput) string {
	var b strings.Builder

	b.WriteString("## Your task\n\nEvaluate the work under review and reply with the required JSON.\n\n")

	if in.IncludeTask && strings.TrimSpace(in.TaskPrompt) != "" {
		b.WriteString("## The task the agent was given\n\n")
		b.WriteString(strings.TrimSpace(in.TaskPrompt))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(in.Rubric) != "" {
		b.WriteString("## Rubric\n\n")
		b.WriteString(strings.TrimSpace(in.Rubric))
		b.WriteString("\n\n")
	}

	if len(in.Criteria) > 0 {
		b.WriteString("## Criteria to score individually\n\n")
		for _, c := range in.Criteria {
			fmt.Fprintf(&b, "- **%s**", c.Name)
			if c.Weight > 0 {
				fmt.Fprintf(&b, " (weight %.2f)", c.Weight)
			}
			if c.Description != "" {
				fmt.Fprintf(&b, ": %s", c.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("\nScore each criterion from 0 to 1 and list them in `criteria`.\n\n")
	}

	if strings.TrimSpace(in.Reference) != "" {
		b.WriteString("## Reference solution\n\n")
		b.WriteString(strings.TrimSpace(in.Reference))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "## Work under review (%s)\n\n", in.SubjectLabel)
	b.WriteString(in.Subject)
	b.WriteString("\n")
	if in.TruncatedNote {
		b.WriteString("\n(Note: the material above was truncated for length.)\n")
	}
	return b.String()
}

func orDefaultInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Package llm is a minimal client for the Anthropic Messages API and for
// Anthropic-compatible gateways.
//
// Only the subset needed for LLM-as-judge grading is implemented: one request,
// one response, text extraction and token accounting. It deliberately avoids an
// SDK dependency so the framework stays a single static binary and works against
// any endpoint that speaks the same wire protocol -- including third-party
// gateways such as DeepSeek's, which is what the MESSAGES endpoint in this
// repository is configured against.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Defaults for environment-provided credentials and endpoint.
const (
	EnvAPIKey    = "ANTHROPIC_API_KEY"
	EnvAuthToken = "ANTHROPIC_AUTH_TOKEN"
	EnvBaseURL   = "ANTHROPIC_BASE_URL"
	EnvModel     = "ANTHROPIC_MODEL"

	DefaultBaseURL = "https://api.anthropic.com"
	DefaultVersion = "2023-06-01"
	DefaultModel   = "claude-sonnet-4-5"
)

// Config describes how to reach the API.
type Config struct {
	// BaseURL is the API root, e.g. "https://api.anthropic.com" or
	// "https://api.deepseek.com/anthropic".
	BaseURL string
	// APIKeyEnv / AuthTokenEnv name the environment variables to read
	// credentials from. Defaults: ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN.
	APIKeyEnv    string
	AuthTokenEnv string
	// APIKey / AuthToken override the environment when set.
	APIKey    string
	AuthToken string
	// Model defaults to EnvModel, then DefaultModel.
	Model string
	// Version is the anthropic-version header.
	Version string
	// Timeout for a single request. Default 120s.
	Timeout time.Duration
	// Retries for transient failures (429/5xx/network). Default 2.
	Retries int
	// HTTPClient overrides the default client (used in tests).
	HTTPClient *http.Client
}

// Resolve fills in defaults from the environment.
//
// Credential handling is explicit rather than implicit so that a grading run can
// never silently pick up an unrelated key: a gateway token is sent as a bearer
// token, a native API key as x-api-key.
func (c Config) Resolve() Config {
	out := c
	if out.BaseURL == "" {
		out.BaseURL = envOr(EnvBaseURL, DefaultBaseURL)
	}
	if out.APIKeyEnv == "" {
		out.APIKeyEnv = EnvAPIKey
	}
	if out.AuthTokenEnv == "" {
		out.AuthTokenEnv = EnvAuthToken
	}
	if out.APIKey == "" {
		out.APIKey = os.Getenv(out.APIKeyEnv)
	}
	if out.AuthToken == "" {
		out.AuthToken = os.Getenv(out.AuthTokenEnv)
	}
	if out.Model == "" {
		out.Model = envOr(EnvModel, DefaultModel)
	}
	if out.Version == "" {
		out.Version = DefaultVersion
	}
	if out.Timeout == 0 {
		out.Timeout = 120 * time.Second
	}
	if out.Retries == 0 {
		out.Retries = 2
	}
	return out
}

// Client is a resolved LLM configuration ready to make requests.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client, resolving defaults from the environment.
func New(cfg Config) *Client {
	resolved := cfg.Resolve()
	hc := resolved.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: resolved.Timeout}
	}
	return &Client{cfg: resolved, http: hc}
}

// Model returns the model this client will call.
func (c *Client) Model() string { return c.cfg.Model }

// BaseURL returns the resolved endpoint root.
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// Available reports whether credentials were found. Callers use it to fail fast
// with an actionable message instead of getting a 401 mid-run.
func (c *Client) Available() bool {
	return c.cfg.APIKey != "" || c.cfg.AuthToken != ""
}

// MissingCredentialsError explains exactly which variables were consulted.
func (c *Client) MissingCredentialsError() error {
	return fmt.Errorf("no API credentials found: set %s (native key) or %s (gateway bearer token)",
		c.cfg.APIKeyEnv, c.cfg.AuthTokenEnv)
}

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

// Message is one turn in the conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a single Messages API call.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	// StopSequences are optional.
	StopSequences []string
}

type wireRequest struct {
	Model         string    `json:"model"`
	System        string    `json:"system,omitempty"`
	Messages      []Message `json:"messages"`
	MaxTokens     int       `json:"max_tokens"`
	Temperature   *float64  `json:"temperature,omitempty"`
	StopSequences []string  `json:"stop_sequences,omitempty"`
}

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Usage reports token consumption for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total returns the sum of input and output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Response is a completed call.
type Response struct {
	Text       string
	Model      string
	StopReason string
	Usage      Usage
	// Latency is the wall-clock time for the successful attempt.
	Latency time.Duration
}

// ---------------------------------------------------------------------------
// Completion
// ---------------------------------------------------------------------------

// Complete performs one request, retrying transient failures.
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	if !c.Available() {
		return nil, c.MissingCredentialsError()
	}
	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 4096
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("llm: request has no messages")
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff, bounded so a grading run cannot hang for long.
			delay := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, retryable, err := c.do(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("llm: giving up after %d attempts: %w", c.cfg.Retries+1, lastErr)
}

func (c *Client) do(ctx context.Context, req Request) (*Response, bool, error) {
	body := wireRequest{
		Model:         req.Model,
		System:        req.System,
		Messages:      req.Messages,
		MaxTokens:     req.MaxTokens,
		StopSequences: req.StopSequences,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("llm: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL(), bytes.NewReader(raw))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", c.cfg.Version)
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("x-api-key", c.cfg.APIKey)
	}
	if c.cfg.AuthToken != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.cfg.AuthToken)
	}

	start := time.Now()
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// Network errors are worth retrying.
		return nil, true, fmt.Errorf("llm: request failed: %w", err)
	}
	defer httpResp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(httpResp.Body, 32<<20))
	if err != nil {
		return nil, true, fmt.Errorf("llm: read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		retryable := httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500
		return nil, retryable, fmt.Errorf("llm: HTTP %d: %s",
			httpResp.StatusCode, strings.TrimSpace(firstLine(string(payload))))
	}

	var parsed wireResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, false, fmt.Errorf("llm: decode response: %w; body: %s",
			err, strings.TrimSpace(firstLine(string(payload))))
	}
	if parsed.Error != nil {
		return nil, false, fmt.Errorf("llm: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}

	var text strings.Builder
	for _, block := range parsed.Content {
		// Gateways emit reasoning/thinking blocks alongside text; only text
		// blocks are the model's answer.
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return &Response{
		Text:       text.String(),
		Model:      parsed.Model,
		StopReason: parsed.StopReason,
		Usage:      Usage{InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens},
		Latency:    time.Since(start),
	}, false, nil
}

// messagesURL builds the endpoint, tolerating a base URL that already carries
// the /v1 segment.
func (c *Client) messagesURL() string {
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	switch {
	case strings.HasSuffix(base, "/v1"):
		return base + "/messages"
	case strings.HasSuffix(base, "/messages"):
		return base
	default:
		return base + "/v1/messages"
	}
}

// ---------------------------------------------------------------------------
// JSON extraction
// ---------------------------------------------------------------------------

// ExtractJSON pulls the first complete JSON object out of text that may contain
// prose or a markdown code fence.
//
// Judge models are instructed to answer with bare JSON, but instruction-following
// is imperfect; recovering the object instead of failing the grader keeps a
// borderline model from poisoning a whole evaluation run.
func ExtractJSON(text string) ([]byte, bool) {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil, false
	}
	if obj, ok := balancedObject(s); ok {
		return obj, true
	}
	// Fall back to the content of a fenced block.
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			rest = rest[:k]
		}
		if obj, ok := balancedObject(strings.TrimSpace(rest)); ok {
			return obj, true
		}
	}
	return nil, false
}

// balancedObject finds the first balanced {...} span, ignoring braces inside
// string literals.
func balancedObject(s string) ([]byte, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// braces inside strings are data, not structure
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return []byte(s[start : i+1]), true
			}
		}
	}
	return nil, false
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

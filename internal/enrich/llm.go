// Package enrich asks a language model for a short brief on each open task
// (summary, deliverable, steps, requirements, time estimate) and caches the
// answer by input hash, so a task costs one call until its teacher-side text
// changes.
//
// llm.go is a minimal client for the OpenAI-compatible chat completions
// route (OpenRouter, DeepSeek, Zhipu GLM, a local llama.cpp server, ...).
package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Completer is one system+user → text round trip. Tests fake it.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// ErrStop means further calls this run are pointless: rate limited, out of
// credit, or the key was rejected. The caller keeps what it has and moves on.
var ErrStop = errors.New("llm: stop calling")

// Client talks to POST {baseURL}/chat/completions.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client

	noReasoning bool

	attempts int
	backoff  time.Duration
}

// NoReasoning asks the provider not to generate chain-of-thought tokens.
// It is for extraction work — reading a calendar into rows — where thinking
// is pure cost: one real calendar chunk answered in 18,202 completion tokens
// of which 13,934 were reasoning, six minutes and most of the money for an
// answer that is a transcription. Providers that do not support the field
// ignore it. Briefs keep their reasoning; judgement is what they are for.
func (c *Client) NoReasoning() *Client {
	c.noReasoning = true
	return c
}

// NewClient returns a client. A nil httpClient gets a 3-minute timeout,
// long enough for slow free-tier providers.
func NewClient(baseURL, apiKey, model string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 3 * time.Minute}
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		model:    model,
		http:     httpClient,
		attempts: 3,
		backoff:  2 * time.Second,
	}
}

// Model is the model id sent with every request.
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
	Reasoning      *reasoning        `json:"reasoning,omitempty"`
}

// reasoning is OpenRouter's switch for chain-of-thought tokens; other
// OpenAI-compatible servers ignore the field.
type reasoning struct {
	Enabled bool `json:"enabled"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one chat request in JSON mode. 5xx, connection failures and
// empty answers are retried; 401/402/403/429 return an error wrapping ErrStop.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	req := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: map[string]string{"type": "json_object"},
		Temperature:    0.2,
	}
	if c.noReasoning {
		req.Reasoning = &reasoning{Enabled: false}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			t := time.NewTimer(c.backoff << (attempt - 2))
			select {
			case <-ctx.Done():
				t.Stop()
				return "", ctx.Err()
			case <-t.C:
			}
		}
		text, retry, err := c.once(ctx, payload)
		if err == nil {
			return text, nil
		}
		if !retry || ctx.Err() != nil {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

// once makes one HTTP call and reports whether a failure is worth retrying.
func (c *Client) once(ctx context.Context, payload []byte) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// OpenRouter attribution; other providers ignore it.
	req.Header.Set("X-Title", "schoolwork-check")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", true, fmt.Errorf("llm: reading response: %w", err)
	}

	var parsed chatResponse
	_ = json.Unmarshal(body, &parsed)
	msg := ""
	if parsed.Error != nil {
		msg = parsed.Error.Message
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusPaymentRequired,
		resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		return "", false, fmt.Errorf("%w: HTTP %d %s", ErrStop, resp.StatusCode, msg)
	case resp.StatusCode >= 500:
		return "", true, fmt.Errorf("llm: HTTP %d %s", resp.StatusCode, msg)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", false, fmt.Errorf("llm: HTTP %d %s", resp.StatusCode, msg)
	}

	// OpenRouter can answer 200 with an error object when the upstream failed.
	if parsed.Error != nil {
		return "", true, fmt.Errorf("llm: %s", msg)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", true, errors.New("llm: empty answer")
	}
	return parsed.Choices[0].Message.Content, false, nil
}

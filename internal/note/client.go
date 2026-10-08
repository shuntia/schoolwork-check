// client.go speaks note's HTTP API as it exists today (2026-09-21): tasks
// are keyed by external_id through PUT /api/tasks/by-external/{id}, and
// PATCH refreshes the rest. See
// docs/note-sink.md for the mapping and ~/Projects/note's external-tasks
// spec for the API this layer is waiting on.
package note

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnauthorized is returned for any 401. note answers a bad token with an
// empty body, so there is nothing else to report.
var ErrUnauthorized = errors.New("note: token rejected (401) — mint a new token in note Settings and set NOTE_TOKEN")

// APIError is a non-2xx answer other than 401. Message comes from the
// {"error": "..."} body note sends with 422 and 409; 400, 404 and 500 have
// empty bodies and leave it "".
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("note: HTTP %d", e.Status)
	}
	return fmt.Sprintf("note: HTTP %d: %s", e.Status, e.Message)
}

// Task is one row of note's tasks table as the API returns it.
type Task struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	State          string `json:"state"` // open | in_progress | done | dropped
	Source         string `json:"source"`
	Notes          string `json:"notes"`
	DurationMin    *int   `json:"duration_min"`
	DurationSource string `json:"duration_source"`
	ParentID       *int64 `json:"parent_id"`
	IsNow          bool   `json:"is_now"`
	UpdatedAt      string `json:"updated_at"`
	// DueAt is RFC 3339 in UTC, or nil when undated. URL is "" when there
	// is none. Both are the importer's to refresh.
	DueAt *string `json:"due_at"`
	URL   string  `json:"url"`
	// ExternalID is our id for the task ("canvas:assignment:12345"), or ""
	// for a task made in note or by a run older than 2026-09-21.
	ExternalID string `json:"external_id"`
}

// ErrDeclined is a by-external PUT answered 410: the user deleted this task
// in note, and note will not make it again.
var ErrDeclined = errors.New("note: the user deleted this task")

// UpsertFields is the body of PUT /api/tasks/by-external/{id}: the task
// without its external_id, which the path carries. note rejects unknown
// fields, so this mirrors its NewTask. Absent fields are left alone on a
// task that already exists.
type UpsertFields struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Notes       string  `json:"notes"`
	State       string  `json:"state,omitempty"`
	DueAt       *string `json:"due_at,omitempty"`
	URL         string  `json:"url"`
}

// Upsert makes or refreshes the task note knows by this external id. It
// reports whether the call created it (201) rather than refreshed it (200).
// On a refresh note takes the title, notes, due date and link, leaves the
// description and steps to whoever briefed the task, and moves the state
// only forward to done. A 410 is ErrDeclined.
func (c *Client) Upsert(ctx context.Context, externalID string, f UpsertFields) (Task, bool, error) {
	var out Task
	status, err := c.doStatus(ctx, http.MethodPut, "/api/tasks/by-external/"+url.PathEscape(externalID), f, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusGone {
		return Task{}, false, ErrDeclined
	}
	if err != nil {
		return Task{}, false, err
	}
	return out, status == http.StatusCreated, nil
}

// TaskNode is a top-level task with its steps. note flattens the task's own
// fields next to "children", which encoding/json reproduces by promoting the
// embedded struct's fields (TestTaskNodeDecoding pins that down).
type TaskNode struct {
	Task
	Children []Task `json:"children"`
}

// Client is a thin wrapper over note's task routes. The zero value is not
// usable; call NewClient.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// briefHTTP is http without its overall timeout; Brief bounds each
	// agent call with briefTimeout on the context instead.
	briefHTTP *http.Client
	log       *slog.Logger

	// attempts and backoff govern retries of 5xx and connection failures.
	attempts int
	backoff  time.Duration
}

// NewClient returns a client for the note server at baseURL authenticating
// with a bearer token. A nil httpClient gets a 30s-timeout default; a nil
// logger discards.
func NewClient(baseURL, token string, httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	briefHTTP := *httpClient
	briefHTTP.Timeout = 0
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		http:      httpClient,
		briefHTTP: &briefHTTP,
		log:       logger,
		attempts:  3,
		backoff:   300 * time.Millisecond,
	}
}

// List returns every non-dropped top-level task with its non-dropped steps.
// The route takes no parameters and is not paginated.
func (c *Client) List(ctx context.Context) ([]TaskNode, error) {
	var out []TaskNode
	if err := c.do(ctx, http.MethodGet, "/api/tasks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Patch applies a subset of {title, description, state, notes, duration_min,
// parent_id, is_now, due_at, url, external_id}. Unknown keys are a 422 (the server uses
// deny_unknown_fields). The response carries the task's fields at the top
// level plus optional "parent" and "demoted_from_now", which we ignore.
func (c *Client) Patch(ctx context.Context, id int64, fields map[string]any) (Task, error) {
	var out Task
	if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/tasks/%d", id), fields, &out); err != nil {
		return Task{}, err
	}
	return out, nil
}

// BriefResult is note's answer to POST /api/tasks/{id}/agent.
type BriefResult struct {
	TaskID  int64       `json:"task_id"`
	Outcome string      `json:"outcome"` // briefed | dropped | unchanged
	Steps   []BriefStep `json:"steps"`
	Task    TaskNode    `json:"task"`
}

// BriefStep is one tool call the agent made.
type BriefStep struct {
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args"`
	Result  json.RawMessage `json:"result"`
	IsError bool            `json:"is_error"`
}

// briefTimeout bounds one agent session: 2-4 model rounds, observed 5-30 s.
const briefTimeout = 5 * time.Minute

// Brief hands one top-level task to note's server-side agent with the LMS
// text as context. The agent owns description, duration and steps, and may
// drop an open task that is not homework. It is one attempt, never retried
// here: a 502 means the provider failed and the task is untouched, so the
// next run is the retry and provider spend stays predictable.
func (c *Client) Brief(ctx context.Context, id int64, taskContext string) (BriefResult, error) {
	var out BriefResult
	if err := c.agentPost(ctx, fmt.Sprintf("/api/tasks/%d/agent", id), map[string]string{"context": taskContext}, &out); err != nil {
		return BriefResult{}, err
	}
	return out, nil
}

// InboxResult is note's answer to POST /api/agent/inbox.
type InboxResult struct {
	SourceID  string            `json:"source_id"`
	Outcome   string            `json:"outcome"` // remembered | nothing | task
	Reason    string            `json:"reason"`
	MemoryIDs []json.RawMessage `json:"memory_ids"`
	Steps     []BriefStep       `json:"steps"`
}

// Inbox hands an informational LMS item (a material or an announcement) to
// note's agent, which stores durable facts in memory, ignores it, or answers
// "task" when it is really work. Like Brief, one attempt with a long timeout;
// note archives earlier facts for the same source id on every call.
func (c *Client) Inbox(ctx context.Context, sourceID, kind, taskContext string) (InboxResult, error) {
	var out InboxResult
	err := c.agentPost(ctx, "/api/agent/inbox", map[string]string{
		"source_id": sourceID, "kind": kind, "context": taskContext,
	}, &out)
	return out, err
}

// agentPost is a single POST with briefTimeout, for agent routes.
func (c *Client) agentPost(ctx context.Context, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, briefTimeout)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("note: POST %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.briefHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("note: POST %s: %w", path, err)
	}
	return finish(resp, out)
}

// do performs one request with retries. body is JSON-encoded when non-nil;
// out is JSON-decoded from a 2xx response when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.doStatus(ctx, method, path, body, out)
	return err
}

// doStatus is do, also reporting the final response's status code, for the
// routes that say something with 200 versus 201.
func (c *Client) doStatus(ctx context.Context, method, path string, body, out any) (int, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("note: encode %s %s: %w", method, path, err)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			wait := c.backoff << (attempt - 2)
			c.log.Debug("note: retrying", "method", method, "path", path, "attempt", attempt, "wait", wait)
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return 0, ctx.Err()
			case <-t.C:
			}
		}

		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
		if err != nil {
			return 0, fmt.Errorf("note: %s %s: %w", method, path, err)
		}
		// Scheme match is case-sensitive on the server: send exactly "Bearer ".
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			lastErr = fmt.Errorf("note: %s %s: %w", method, path, err)
			continue
		}

		// 5xx is worth another go; every 4xx is final.
		if resp.StatusCode >= 500 {
			drain(resp)
			lastErr = &APIError{Status: resp.StatusCode}
			continue
		}
		return resp.StatusCode, finish(resp, out)
	}
	return 0, lastErr
}

// finish turns one final (non-retryable) response into a result or an error.
func finish(resp *http.Response, out any) error {
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(resp.Body)}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("note: decode %s: %w", resp.Request.URL.Path, err)
	}
	return nil
}

// errorMessage reads {"error": "..."}; anything else (empty body, HTML, a
// truncated stream) counts as no message.
func errorMessage(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 1<<16))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return ""
	}
	return payload.Error
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

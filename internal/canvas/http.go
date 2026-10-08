package canvas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnauthorized is returned when Canvas rejects the access token (HTTP 401).
var ErrUnauthorized = errors.New("canvas: token rejected (401) — student tokens expire within 120 days, generate a new one")

// maxRetries is the number of retries (in addition to the first attempt) for
// rate limiting and 5xx responses.
const maxRetries = 3

// apiError is a non-retryable HTTP failure carrying a snippet of the body.
type apiError struct {
	Status int
	URL    string
	Body   string
}

func (e *apiError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 200 {
		b = b[:200] + "…"
	}
	if b == "" {
		return fmt.Sprintf("canvas: %s: HTTP %d", e.URL, e.Status)
	}
	return fmt.Sprintf("canvas: %s: HTTP %d: %s", e.URL, e.Status, b)
}

// url builds an absolute API URL from a path like "/api/v1/courses" plus an
// optional already-encoded query string.
func (c *Client) url(path, query string) string {
	u := c.baseURL + path
	if query != "" {
		u += "?" + query
	}
	return u
}

// doRequest performs a GET with the bearer token, retrying rate limits and 5xx.
// The caller owns the returned body.
func (c *Client) doRequest(ctx context.Context, rawURL string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.retryBase<<(attempt-1)); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("canvas: bad request url %q: %w", rawURL, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("canvas: GET %s: %w", rawURL, err)
			continue
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close()
			return nil, ErrUnauthorized
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp, nil
		case resp.StatusCode == http.StatusForbidden:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if isRateLimited(resp, body) {
				c.log.Debug("canvas rate limited, retrying", "url", rawURL, "attempt", attempt+1,
					"remaining", resp.Header.Get("X-Rate-Limit-Remaining"))
				lastErr = &apiError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
				continue
			}
			return nil, &apiError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
		case resp.StatusCode >= 500:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			lastErr = &apiError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
			continue
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			return nil, &apiError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("canvas: GET %s: exhausted retries", rawURL)
	}
	return nil, lastErr
}

// isRateLimited reports whether a 403 is Canvas's throttling response rather
// than a real permission error.
func isRateLimited(resp *http.Response, body []byte) bool {
	if strings.Contains(strings.ToLower(string(body)), "rate limit exceeded") {
		return true
	}
	// Canvas sends X-Rate-Limit-Remaining on every response; a 403 with the
	// bucket at or below zero is throttling.
	if v := resp.Header.Get("X-Rate-Limit-Remaining"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil && f <= 0 {
			return true
		}
	}
	return false
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// getJSON fetches rawURL and decodes the body into v.
func (c *Client) getJSON(ctx context.Context, rawURL string, v any) error {
	resp, err := c.doRequest(ctx, rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("canvas: decode %s: %w", rawURL, err)
	}
	return nil
}

// getPaged fetches a list endpoint and follows RFC 5988 `rel="next"` links.
func getPaged[T any](ctx context.Context, c *Client, rawURL string) ([]T, error) {
	var out []T
	seen := map[string]bool{}
	for rawURL != "" {
		if seen[rawURL] {
			break // defensive: a server that points next at itself
		}
		seen[rawURL] = true
		if err := ctx.Err(); err != nil {
			return out, err
		}
		resp, err := c.doRequest(ctx, rawURL)
		if err != nil {
			return out, err
		}
		var page []T
		dec := json.NewDecoder(resp.Body)
		dec.UseNumber()
		decErr := dec.Decode(&page)
		next := parseLinkHeader(resp.Header.Get("Link"))["next"]
		resp.Body.Close()
		if decErr != nil {
			return out, fmt.Errorf("canvas: decode %s: %w", rawURL, decErr)
		}
		out = append(out, page...)
		rawURL = next
	}
	return out, nil
}

// parseLinkHeader parses an RFC 5988 Link header into rel -> URL. URLs may
// themselves contain commas and semicolons, so the value is scanned rather
// than split.
func parseLinkHeader(v string) map[string]string {
	out := map[string]string{}
	for {
		i := strings.IndexByte(v, '<')
		if i < 0 {
			return out
		}
		v = v[i+1:]
		j := strings.IndexByte(v, '>')
		if j < 0 {
			return out
		}
		target := strings.TrimSpace(v[:j])
		v = v[j+1:]

		// Parameters run until the comma that precedes the next "<...>".
		var params string
		if k := strings.IndexByte(v, '<'); k < 0 {
			params, v = v, ""
		} else {
			seg := v[:k]
			if cut := strings.LastIndexByte(seg, ','); cut >= 0 {
				params, v = seg[:cut], v[cut+1:]
			} else {
				params, v = seg, v[k:]
			}
		}
		for _, p := range strings.Split(params, ";") {
			name, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
				continue
			}
			for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(val), `"'`)) {
				if _, dup := out[rel]; !dup && target != "" {
					out[rel] = target
				}
			}
		}
	}
}

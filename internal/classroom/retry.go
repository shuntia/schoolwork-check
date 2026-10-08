package classroom

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// retryAttempts is how many times a transient Google API failure is retried.
const retryAttempts = 3

// errTokenExpired is returned whenever Google rejects the cached credentials.
// The 7-day note matters: an OAuth client left in "Testing" publishing status
// issues refresh tokens that Google expires after a week.
var errTokenExpired = errors.New(
	"classroom: token expired or revoked — run `schoolwork-check google-login` " +
		"(if your Google Cloud OAuth consent screen is still in \"Testing\" mode, " +
		"refresh tokens expire after 7 days and you must re-consent)")

// retry runs fn, retrying rate-limit and server errors with exponential
// backoff. Authentication failures short-circuit to errTokenExpired.
func (c *Client) retry(ctx context.Context, what string, fn func() error) error {
	var err error
	for attempt := range retryAttempts {
		if attempt > 0 {
			delay := c.retryDelay << (attempt - 1)
			if delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					t.Stop()
					return ctx.Err()
				case <-t.C:
				}
			}
		}
		err = fn()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if ae := authError(err); ae != nil {
			return ae
		}
		if !retryable(err) {
			return err
		}
		c.log.Debug("classroom: retrying google api call", "op", what, "attempt", attempt+1, "err", err)
	}
	return fmt.Errorf("%s: giving up after %d attempts: %w", what, retryAttempts, err)
}

// authError recognises "your credentials are no longer good" and translates it
// into an actionable message. It returns nil for every other error.
func authError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errTokenExpired) {
		return errTokenExpired
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return errTokenExpired
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusUnauthorized {
		return errTokenExpired
	}
	if s := err.Error(); strings.Contains(s, "invalid_grant") || strings.Contains(s, "token expired or revoked") {
		return errTokenExpired
	}
	return nil
}

// retryable reports whether err is worth another attempt: 429, 5xx, or a
// transport-level failure.
func retryable(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code == http.StatusTooManyRequests || ge.Code >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return false
}

// isForbidden reports whether Google refused the call outright — typically a
// domain that restricts an API for students. Not retryable, not fatal.
func isForbidden(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && (ge.Code == http.StatusForbidden || ge.Code == http.StatusNotFound)
}

package engine

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ErrIncomplete means the connection ended before all bytes arrived.
//
// GO NOTE: errors are ordinary values. Declaring one as a variable lets
// callers check for it with errors.Is(err, engine.ErrIncomplete).
var ErrIncomplete = errors.New("connection ended before the file was complete")

// ErrStalled means no data arrived for too long (see Download.StallTimeout).
var ErrStalled = errors.New("connection stalled: no data received")

// HTTPError is a reply from the server that isn't the file, e.g. 404 or 503.
//
// GO NOTE: any type with an "Error() string" method counts as an error.
// That's an INTERFACE: Go's error type is defined as "anything that has
// Error() string". You never declare "implements" - having the method is enough.
type HTTPError struct {
	StatusCode int
	Status     string
	// RetryAfter is how long the server asked us to wait (its Retry-After
	// header, sent with 429 and 503 replies), or 0 if it didn't say.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return "server replied " + e.Status }

// newHTTPError builds an HTTPError from a response, including Retry-After.
func newHTTPError(resp *http.Response) *HTTPError {
	e := &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		// Retry-After is either a number of seconds or a date.
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			e.RetryAfter = time.Until(t)
		}
		e.RetryAfter = min(max(e.RetryAfter, 0), 5*time.Minute) // ignore absurd values
	}
	return e
}

// isConnectionLimit reports whether the server refused a connection because
// we have too many open: 429 Too Many Requests or 503 Service Unavailable.
func isConnectionLimit(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) &&
		(he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusServiceUnavailable)
}

// retryAfter returns the wait the server asked for, if any.
func retryAfter(err error) time.Duration {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.RetryAfter
	}
	return 0
}

// diskError wraps a problem with OUR side - can't create or write the file
// (disk full, no permission). Retrying won't fix it, so we stop immediately.
type diskError struct{ err error }

func (e *diskError) Error() string { return fmt.Sprintf("disk error: %v", e.err) }
func (e *diskError) Unwrap() error { return e.err } // lets errors.Is see inside

// retryable decides whether trying again could help.
//
//   - Network errors, drops and stalls: yes, the connection may recover.
//   - 5xx (server error), 408 (timeout), 429 (too many requests): yes, these
//     are temporary by definition.
//   - Other 4xx (404 Not Found, 403 Forbidden...): no, the answer won't change.
//   - Disk errors: no.
func retryable(err error) bool {
	// GO NOTE: errors.As checks whether err is (or wraps) a given type,
	// and if so, fills in the variable. It's like isinstance() in Python.
	var de *diskError
	if errors.As(err, &de) || errors.Is(err, ErrFileChanged) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 500 ||
			he.StatusCode == http.StatusRequestTimeout ||
			he.StatusCode == http.StatusTooManyRequests
	}
	return true
}

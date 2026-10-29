package xdive

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for use with errors.Is.
var (
	// ErrRateLimited matches errors returned after the retry budget was spent
	// on 429, 503 SERVER_BUSY or 409 IDEMPOTENCY_KEY_IN_PROGRESS responses.
	ErrRateLimited = errors.New("xdive: rate limited")
	// ErrQuotaExceeded matches HTTP 402 responses (plan quota exhausted).
	ErrQuotaExceeded = errors.New("xdive: quota exceeded")
	// ErrCrawlTimeout matches the error returned by WaitForCrawl when a crawl
	// does not finish in time.
	ErrCrawlTimeout = errors.New("xdive: crawl did not finish in time")
	// ErrNoAPIKey is returned by New when no API key is configured.
	ErrNoAPIKey = errors.New("xdive: an API key is required (WithAPIKey or XDIVE_API_KEY)")
)

// Error is an error response from the API.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Code is the machine-readable error code ("" for older error bodies).
	Code string
	// Message is the human-readable message.
	Message string
	// Retryable is the server's "retryable" flag: the same request may succeed later.
	Retryable bool
	// RequestID is the X-Request-ID response header, useful for support.
	RequestID string
	// DocsURL links to the documentation for this error, when provided.
	DocsURL string
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("xdive: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("xdive: %d: %s", e.Status, e.Message)
}

// Is reports whether target is ErrQuotaExceeded (HTTP 402) or, for status 429,
// ErrRateLimited.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrQuotaExceeded:
		return e.Status == 402
	case ErrRateLimited:
		return e.Status == 429
	}
	return false
}

// RateLimitedError is returned when the API kept asking the client to slow
// down until the retry budget ran out. It wraps the final *Error.
type RateLimitedError struct {
	// Err is the final API error (Status, Code, RequestID, ...).
	Err *Error
	// RetryAfter is the wait the server asked for in its last response.
	RetryAfter time.Duration
}

// Unwrap exposes the underlying *Error to errors.As.
func (e *RateLimitedError) Unwrap() error { return e.Err }

// Error implements the error interface.
func (e *RateLimitedError) Error() string { return e.Err.Error() }

// Is matches ErrRateLimited.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }

// CrawlTimeoutError is returned by WaitForCrawl when the crawl is not
// finished within the timeout. The crawl keeps running on the server.
type CrawlTimeoutError struct {
	// Err describes the timeout (Status 408).
	Err *Error
	// CrawlID is the id of the crawl that was being waited for.
	CrawlID string
}

// Unwrap exposes the underlying *Error to errors.As.
func (e *CrawlTimeoutError) Unwrap() error { return e.Err }

// Error implements the error interface.
func (e *CrawlTimeoutError) Error() string { return e.Err.Error() }

// Is matches ErrCrawlTimeout.
func (e *CrawlTimeoutError) Is(target error) bool { return target == ErrCrawlTimeout }

func newCrawlTimeout(id string, timeout time.Duration) *CrawlTimeoutError {
	return &CrawlTimeoutError{
		Err: &Error{
			Status:  408,
			Message: fmt.Sprintf("crawl %s did not finish within %ds", id, int(timeout.Round(time.Second)/time.Second)),
		},
		CrawlID: id,
	}
}

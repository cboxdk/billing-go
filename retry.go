package cboxbilling

// Hand-written (not generated): automatic idempotency keys and retries with backoff.

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy says how NewClient retries a 429, a 5xx or a network error: for reads, and for
// writes that carry an Idempotency-Key only, so a retried write cannot apply twice.
type RetryPolicy struct {
	// MaxAttempts is the tries in all, the first included. 1 turns retries off.
	MaxAttempts int
	// BaseDelay is the backoff before the first retry, doubled for each one after.
	BaseDelay time.Duration
	// MaxDelay is the longest wait between tries, Retry-After included.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is three tries, backing off from 500 ms, waiting at most 20 s.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 20 * time.Second}

// NoRetries turns retries off.
var NoRetries = RetryPolicy{MaxAttempts: 1}

// Delay is the wait before retry number retry (1 for the first): the server's Retry-After when it
// sent one, otherwise exponential backoff with full jitter. Both are capped at MaxDelay.
func (p RetryPolicy) Delay(retry int, retryAfter string) time.Duration {
	if wait, ok := parseRetryAfter(retryAfter); ok {
		return min(max(wait, 0), p.MaxDelay)
	}
	ceiling := float64(p.BaseDelay) * math.Pow(2, float64(retry-1))
	if ceiling > float64(p.MaxDelay) {
		ceiling = float64(p.MaxDelay)
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// IsRetryableStatus reports whether a status is worth another try: 429 and every 5xx.
func IsRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return time.Until(at), true
	}
	return 0, false
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func isSafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// retryTransport adds an Idempotency-Key to writes that lack one and retries as RetryPolicy says.
// Every try of one request sends the same key.
type retryTransport struct {
	next    http.RoundTripper
	policy  RetryPolicy
	autoKey bool
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	header := req.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	if t.autoKey && isWrite(req.Method) && header.Get("Idempotency-Key") == "" {
		header.Set("Idempotency-Key", NewIdempotencyKey())
	}
	retryable := isSafe(req.Method) || header.Get("Idempotency-Key") != ""
	// A body that cannot be read again cannot be sent again.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		retryable = false
	}

	for attempt := 1; ; attempt++ {
		try := req.Clone(req.Context())
		try.Header = header.Clone()
		if attempt > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			try.Body = body
		}

		last := !retryable || attempt >= t.policy.MaxAttempts
		resp, err := next.RoundTrip(try)
		if err != nil {
			if last || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if waitErr := wait(req.Context(), t.policy.Delay(attempt, "")); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if last || !IsRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		retryAfter := resp.Header.Get("Retry-After")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if waitErr := wait(req.Context(), t.policy.Delay(attempt, retryAfter)); waitErr != nil {
			return nil, waitErr
		}
	}
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

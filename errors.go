package cboxbilling

// Hand-written (not generated): typed errors from the API's {error, message} body.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Sentinels for errors.Is, one per status family.
var (
	ErrBadRequest       = errors.New("cboxbilling: bad request")
	ErrUnauthenticated  = errors.New("cboxbilling: unauthenticated")
	ErrPermissionDenied = errors.New("cboxbilling: permission denied")
	ErrNotFound         = errors.New("cboxbilling: not found")
	ErrConflict         = errors.New("cboxbilling: conflict")
	ErrValidation       = errors.New("cboxbilling: validation failed")
	ErrRateLimited      = errors.New("cboxbilling: rate limited")
	ErrServer           = errors.New("cboxbilling: server error")
)

// Error is a non-2xx response. Use errors.As to get it, and errors.Is with the sentinels above
// to branch on the kind:
//
//	var apiErr *cboxbilling.Error
//	if errors.As(err, &apiErr) && errors.Is(err, cboxbilling.ErrNotFound) { … }
type Error struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Kind is the body's `error` field verbatim: a sentence, or a code such as
	// `insufficient_scope` or `idempotency_conflict`.
	Kind string
	// Message is the body's `message`, or Kind when there is none.
	Message string
	// Code is the body's `code`, on refusals that name one.
	Code string
	// RequiredScope is set with `insufficient_scope`: the scope the operation needs.
	RequiredScope string
	// Errors maps each field to its messages on a 422.
	Errors map[string][]string
	// Body is the raw response body.
	Body []byte
	// Header is the response headers.
	Header http.Header
}

func (e *Error) Error() string {
	return fmt.Sprintf("cboxbilling: %d: %s", e.StatusCode, e.Message)
}

// Is matches the sentinel for this error's status.
func (e *Error) Is(target error) bool {
	return target == e.sentinel()
}

// RetryAfter is the server's Retry-After on a 429, in seconds, or 0.
func (e *Error) RetryAfter() int {
	seconds, err := strconv.Atoi(e.Header.Get("Retry-After"))
	if err != nil {
		return 0
	}
	return seconds
}

func (e *Error) sentinel() error {
	switch {
	case e.StatusCode == http.StatusBadRequest:
		return ErrBadRequest
	case e.StatusCode == http.StatusUnauthorized:
		return ErrUnauthenticated
	case e.StatusCode == http.StatusForbidden:
		return ErrPermissionDenied
	case e.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case e.StatusCode == http.StatusConflict:
		return ErrConflict
	case e.StatusCode == http.StatusUnprocessableEntity:
		return ErrValidation
	case e.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case e.StatusCode >= 500:
		return ErrServer
	}
	return nil
}

// newError builds an *Error from a status, headers and raw body.
func newError(status int, header http.Header, body []byte) *Error {
	apiErr := &Error{StatusCode: status, Body: body, Header: header}

	var parsed struct {
		Error         string              `json:"error"`
		Message       string              `json:"message"`
		Code          string              `json:"code"`
		RequiredScope string              `json:"required_scope"`
		Errors        map[string][]string `json:"errors"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		apiErr.Kind = parsed.Error
		apiErr.Message = parsed.Message
		apiErr.Code = parsed.Code
		apiErr.RequiredScope = parsed.RequiredScope
		apiErr.Errors = parsed.Errors
	}
	if apiErr.Message == "" {
		apiErr.Message = apiErr.Kind
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}

// typedErrorTransport turns every non-2xx response into an *Error. The http.Client wraps it in
// a *url.Error, which errors.As sees through.
type typedErrorTransport struct {
	next http.RoundTripper
}

func (t *typedErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	resp, err := next.RoundTrip(req)
	if err != nil || resp.StatusCode < 300 {
		return resp, err
	}

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	return nil, newError(resp.StatusCode, resp.Header, body)
}

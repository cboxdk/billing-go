package cboxbilling

// Hand-written (not generated): the client constructor. See README.md.

import (
	"errors"
	"net/http"
	"strings"
)

// Version is this SDK's version, sent in the User-Agent.
const Version = "0.2.1"

type settings struct {
	cfg     *Configuration
	retry   RetryPolicy
	autoKey bool
}

// Option configures NewClient.
type Option func(*settings)

// WithHTTPClient sends requests through your own *http.Client. Its Transport is wrapped so that
// writes get idempotency keys, retries happen and error responses become *Error values.
func WithHTTPClient(client *http.Client) Option {
	return func(s *settings) {
		s.cfg.HTTPClient = client
	}
}

// WithHeader sends a header on every request.
func WithHeader(name, value string) Option {
	return func(s *settings) {
		s.cfg.AddDefaultHeader(name, value)
	}
}

// WithRetryPolicy replaces DefaultRetryPolicy; NoRetries turns retries off.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(s *settings) {
		s.retry = policy
	}
}

// WithoutAutoIdempotencyKey stops NewClient from giving writes a key of their own. A write then
// carries a key only when you set one, and is retried only then.
func WithoutAutoIdempotencyKey() Option {
	return func(s *settings) {
		s.autoKey = false
	}
}

// NewClient returns the generated *APIClient set up for Cbox Billing:
//
//   - bearer auth with token, against baseURL (your deployment's API root, e.g. https://api.cboxbilling.com/v1);
//   - every write (POST, PUT, PATCH, DELETE) without an Idempotency-Key gets a fresh UUIDv4 one;
//   - a 429, a 5xx or a network error is retried per DefaultRetryPolicy, honouring Retry-After,
//     for reads and for writes that carry an Idempotency-Key (every try sends the same key);
//   - every non-2xx response comes back as an error that errors.As turns into *Error.
//
//	client, err := cboxbilling.NewClient(os.Getenv("CBOX_BILLING_TOKEN"), "https://api.cboxbilling.com/v1")
//	plans, _, err := client.PlansAPI.ListPlans(ctx).Execute()
func NewClient(token, baseURL string, opts ...Option) (*APIClient, error) {
	if token == "" {
		return nil, errors.New("cboxbilling: an API token is required")
	}
	if baseURL == "" {
		return nil, errors.New("cboxbilling: a base URL is required")
	}

	cfg := NewConfiguration()
	cfg.Servers = ServerConfigurations{{URL: strings.TrimRight(baseURL, "/")}}
	cfg.OperationServers = map[string]ServerConfigurations{}
	cfg.UserAgent = "billing-go/" + Version
	cfg.AddDefaultHeader("Authorization", "Bearer "+token)

	s := &settings{cfg: cfg, retry: DefaultRetryPolicy, autoKey: true}
	for _, opt := range opts {
		opt(s)
	}
	if s.retry.MaxAttempts < 1 {
		s.retry.MaxAttempts = 1
	}

	base := http.DefaultClient
	if cfg.HTTPClient != nil {
		base = cfg.HTTPClient
	}
	wrapped := *base
	wrapped.Transport = &typedErrorTransport{
		next: &retryTransport{next: base.Transport, policy: s.retry, autoKey: s.autoKey},
	}
	cfg.HTTPClient = &wrapped

	return NewAPIClient(cfg), nil
}

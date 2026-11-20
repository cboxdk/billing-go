package enforcement

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

// transport is the client's own HTTP layer. It never retries: a lease that fails goes to the
// failure policy, and a report that fails is sent again by the next report. Answers are read
// strictly; a body that is not a JSON object is a BillingUnavailableError.
type transport struct {
	baseURL string
	token   string
	http    *http.Client
	headers map[string]string
	timeout time.Duration
}

func (t *transport) get(ctx context.Context, path string) (map[string]any, error) {
	return t.send(ctx, http.MethodGet, path, nil, t.timeout)
}

func (t *transport) post(ctx context.Context, path string, body any, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = t.timeout
	}
	return t.send(ctx, http.MethodPost, path, body, timeout)
}

func (t *transport) send(ctx context.Context, method, path string, body any, timeout time.Duration) (map[string]any, error) {
	endpoint := strings.SplitN(path, "?", 2)[0]
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range t.headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, &BillingUnavailableError{Endpoint: endpoint, Message: fmt.Sprintf("Billing [%s] is unreachable: %v", endpoint, err), Err: err}
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &BillingUnavailableError{Endpoint: endpoint, StatusCode: resp.StatusCode, Message: fmt.Sprintf("Billing [%s] answer could not be read: %v", endpoint, err), Err: err}
	}

	parsed, parseErr := decode(text)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
			var payload any = string(text)
			if parseErr == nil {
				payload = parsed
			}
			return nil, &BillingRefusedError{Endpoint: endpoint, StatusCode: resp.StatusCode, Body: payload}
		}
		return nil, &BillingUnavailableError{Endpoint: endpoint, StatusCode: resp.StatusCode, Message: fmt.Sprintf("Billing [%s] answered HTTP %d", endpoint, resp.StatusCode)}
	}
	object, ok := parsed.(map[string]any)
	if parseErr != nil || !ok {
		return nil, malformed(endpoint, "expected a JSON object")
	}
	return object, nil
}

// decode parses JSON keeping numbers as json.Number, so an integer can be told from a float.
func decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func malformed(endpoint, detail string) error {
	return &BillingUnavailableError{Endpoint: endpoint, Message: fmt.Sprintf("Billing [%s] gave a malformed answer: %s", endpoint, detail)}
}

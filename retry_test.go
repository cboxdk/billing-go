package cboxbilling_test

// Retries and automatic idempotency keys, through the fault proxy in front of Prism
// (sdks/testing/fault-proxy.mjs): it answers the first tries with an injected status and records
// every attempt's Idempotency-Key.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	cboxbilling "github.com/cboxdk/billing-go"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type attempt struct {
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	IdempotencyKey *string `json:"idempotency_key"`
}

func faulty(t *testing.T, times, status int, retryAfter string, opts ...cboxbilling.Option) (string, *cboxbilling.APIClient) {
	t.Helper()
	id := cboxbilling.NewIdempotencyKey()
	all := []cboxbilling.Option{
		cboxbilling.WithHeader("X-Fault-Id", id),
		cboxbilling.WithHeader("X-Fault-Times", strconv.Itoa(times)),
		cboxbilling.WithHeader("X-Fault-Status", strconv.Itoa(status)),
	}
	if retryAfter != "" {
		all = append(all, cboxbilling.WithHeader("X-Fault-Retry-After", retryAfter))
	}
	return id, newClient(t, append(all, opts...)...)
}

func attempts(t *testing.T, id string) []attempt {
	t.Helper()
	resp, err := http.Get(mockURL() + "/__faults/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []attempt
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	return list
}

func change(client *cboxbilling.APIClient, key string) error {
	req := client.SubscriptionsAPI.ChangeSubscriptionQuantity(context.Background(), "org_acme").
		QuantityRequest(*cboxbilling.NewQuantityRequest(10))
	if key != "" {
		req = req.IdempotencyKey(key)
	}
	_, _, err := req.Execute()
	return err
}

func TestA503ThenA200IsRetriedHonouringRetryAfterWithTheSameAutomaticKey(t *testing.T) {
	id, client := faulty(t, 1, 503, "1")
	started := time.Now()

	if err := change(client, ""); err != nil {
		t.Fatal(err)
	}

	if time.Since(started) < 900*time.Millisecond {
		t.Fatalf("expected to wait for Retry-After, took %v", time.Since(started))
	}
	tries := attempts(t, id)
	if len(tries) != 2 || tries[0].IdempotencyKey == nil || tries[1].IdempotencyKey == nil {
		t.Fatalf("expected two tries with keys, got %+v", tries)
	}
	if !uuidV4.MatchString(*tries[0].IdempotencyKey) || *tries[1].IdempotencyKey != *tries[0].IdempotencyKey {
		t.Fatalf("expected one UUIDv4 key on both tries, got %q and %q", *tries[0].IdempotencyKey, *tries[1].IdempotencyKey)
	}
}

func TestA400IsNotRetried(t *testing.T) {
	id, client := faulty(t, 1, 400, "")

	if err := change(client, ""); !errors.Is(err, cboxbilling.ErrBadRequest) {
		t.Fatalf("expected a bad request, got %v", err)
	}
	if n := len(attempts(t, id)); n != 1 {
		t.Fatalf("expected one try, got %d", n)
	}
}

func TestTheCallersOwnKeyIsKeptAndAWriteWithoutAKeyIsNotRetried(t *testing.T) {
	id, client := faulty(t, 1, 503, "0")
	if err := change(client, "my-key-1"); err != nil {
		t.Fatal(err)
	}
	tries := attempts(t, id)
	if len(tries) != 2 || *tries[0].IdempotencyKey != "my-key-1" || *tries[1].IdempotencyKey != "my-key-1" {
		t.Fatalf("expected the caller's key on both tries, got %+v", tries)
	}

	id, client = faulty(t, 1, 503, "0", cboxbilling.WithoutAutoIdempotencyKey())
	if err := change(client, ""); !errors.Is(err, cboxbilling.ErrServer) {
		t.Fatalf("expected the 503, got %v", err)
	}
	tries = attempts(t, id)
	if len(tries) != 1 || tries[0].IdempotencyKey != nil {
		t.Fatalf("expected one keyless try, got %+v", tries)
	}
}

func TestRetriesStopAfterTheConfiguredTriesOrNeverStartWhenTurnedOff(t *testing.T) {
	id, client := faulty(t, 5, 503, "0", cboxbilling.WithRetryPolicy(cboxbilling.RetryPolicy{MaxAttempts: 2, MaxDelay: time.Second}))
	if err := change(client, ""); !errors.Is(err, cboxbilling.ErrServer) {
		t.Fatalf("expected the 503, got %v", err)
	}
	if n := len(attempts(t, id)); n != 2 {
		t.Fatalf("expected two tries, got %d", n)
	}

	id, client = faulty(t, 1, 503, "0", cboxbilling.WithRetryPolicy(cboxbilling.NoRetries))
	if err := change(client, ""); !errors.Is(err, cboxbilling.ErrServer) {
		t.Fatalf("expected the 503, got %v", err)
	}
	if n := len(attempts(t, id)); n != 1 {
		t.Fatalf("expected one try, got %d", n)
	}
}

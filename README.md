# Cbox Billing for Go

`github.com/cboxdk/billing-go` is the Go client for the Cbox Billing API, generated from its
OpenAPI contract. Go 1.23+.

## Install

The module is not published yet: the module path names its own repository, which does not exist
yet. Until it does, use this directory with a `replace` directive:

```bash
go mod edit -require=github.com/cboxdk/billing-go@v0.0.0 \
  -replace=github.com/cboxdk/billing-go=/path/to/cbox-billing/sdks/go
```

Once published:

```bash
go get github.com/cboxdk/billing-go
```

## Quickstart

```go
import (
	"context"
	"os"

	cboxbilling "github.com/cboxdk/billing-go"
)

client, err := cboxbilling.NewClient(
	os.Getenv("CBOX_BILLING_TOKEN"), // cbl_… (live) or cbt_… (test)
	"https://api.cboxbilling.com/v1",
)
if err != nil {
	return err
}

plans, _, err := client.PlansAPI.ListPlans(context.Background()).Execute()
```

`client` is the generated `*APIClient`; every API is a field on it (`PlansAPI`,
`SubscriptionsAPI`, `InvoicesAPI`, `OrganizationsAPI`, …). Requests are builders: set parameters,
then call `Execute()`.

## Writes and idempotency keys

A write that honours `Idempotency-Key` has an `IdempotencyKey(…)` setter. Make one key per business
action and keep it with that action: a retry with the same key replays the first response
instead of doing the work twice.

```go
key := cboxbilling.NewIdempotencyKey() // store it with the order you are invoicing

line := cboxbilling.NewInvoiceLineRequest("Consulting", "2.000", 50000)
invoice, _, err := client.InvoicesAPI.CreateInvoice(ctx, "org_acme").
	IdempotencyKey(key).
	InvoiceRequest(*cboxbilling.NewInvoiceRequest([]cboxbilling.InvoiceLineRequest{*line})).
	Execute()
```

## Retries and automatic keys

Every write (POST, PUT, PATCH, DELETE) without an `Idempotency-Key` gets a fresh UUIDv4 one, so
it is safe to retry. A 429, a 5xx or a network error is then tried again, up to 3 tries in all,
with exponential backoff and jitter, waiting as long as `Retry-After` says when the server sends
it (at most 20 seconds). Reads are retried too. A write without a key, because you turned the
automatic keys off, is never retried. Every try of one call sends the same key.

The automatic key protects a single call. To make a retry of a whole business action safe (after a crash, say), set your own key with `IdempotencyKey(…)` and keep it with that action.

```go
client, err := cboxbilling.NewClient(token, baseURL,
	cboxbilling.WithRetryPolicy(cboxbilling.RetryPolicy{MaxAttempts: 5, BaseDelay: 250 * time.Millisecond, MaxDelay: 10 * time.Second}),
	// or cboxbilling.WithRetryPolicy(cboxbilling.NoRetries), and cboxbilling.WithoutAutoIdempotencyKey()
)
```

## Pagination

List endpoints return one page with `has_more` and `next_cursor`. `Paginate` returns an iterator
over every item:

```go
pages := cboxbilling.Paginate[cboxbilling.Invoice](func(cursor string) (*cboxbilling.InvoiceList, error) {
	req := client.InvoicesAPI.ListInvoices(ctx, "org_acme")
	if cursor != "" {
		req = req.Cursor(cursor)
	}
	page, _, err := req.Execute()
	return page, err
})
for invoice, err := range pages {
	if err != nil {
		return err
	}
	fmt.Println(invoice.GetNumber())
}
```

## Errors

With `NewClient`, every non-2xx response returns an error that `errors.As` turns into a
`*cboxbilling.Error`, carrying `StatusCode`, `Kind` (the body's `error`), `Message`, `Code`,
`RequiredScope` and, on a 422, `Errors` per field. `errors.Is` matches the kind:
`ErrBadRequest`, `ErrUnauthenticated`, `ErrPermissionDenied`, `ErrNotFound`, `ErrConflict`,
`ErrValidation`, `ErrRateLimited` (see `RetryAfter()`), `ErrServer`.

```go
_, _, err := client.SubscriptionsAPI.GetSubscription(ctx, "org_acme").Execute()
switch {
case errors.Is(err, cboxbilling.ErrNotFound):
	// no active subscription
case err != nil:
	var apiErr *cboxbilling.Error
	if errors.As(err, &apiErr) && apiErr.Kind == "insufficient_scope" {
		log.Printf("the token needs %s", apiErr.RequiredScope)
	}
	return err
}
```

## Webhooks

Verify every delivery against the **raw** request body, before you parse it:

```go
func handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	event, err := cboxbilling.ConstructWebhookEvent(body, r.Header.Get(cboxbilling.SignatureHeader), os.Getenv("CBOX_BILLING_WEBHOOK_SECRET"))
	if errors.Is(err, cboxbilling.ErrWebhookSignature) {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	// event.Type, event.Data; dedupe on event.ID
	w.WriteHeader(http.StatusOK)
}
```

A delivery signed more than five minutes from now is refused as a replay. Use a
`cboxbilling.WebhookVerifier` to set another `Tolerance`, or a negative one to turn the check off.

## Enforcement: metering on the hot path

The `enforcement` package meters usage against Billing without a network call per request. Reservations
take units from a slice of the organization's allowance leased into a local store; Billing is
asked only when a slice runs low, and committed usage is reported as running totals. It is the
same design as `cboxdk/laravel-billing-client` and behaves the same in all five SDKs: the shared
[conformance scenarios](../conformance) pin it down, and [SPEC.md](../conformance/SPEC.md) is the
full reference. Use a token with the `enforce` scope.

```go
import "github.com/cboxdk/billing-go/enforcement"

billing, err := enforcement.New(os.Getenv("CBOX_BILLING_TOKEN"), "https://api.cboxbilling.com/v1")

func handle(ctx context.Context, org string) error {
	r, err := billing.Reserve(ctx, org, "api.requests", 10) // hold an estimate
	if err != nil {
		return err // see Errors below
	}
	defer billing.Release(ctx, r) // gives the hold back on the error path; a no-op after Commit

	used := doTheWork()
	return billing.Commit(ctx, r, enforcement.Units(used)) // the rest goes back to the slice
}
```

Several meters at once, all or nothing: `ReserveMany(ctx, org, map[string]int64{"api.requests": 1,
"tokens": 2000})`, committed with `Commit(ctx, set, enforcement.PerMeter{"api.requests": 1,
"tokens": 1450})`. `Can(ctx, org, meter, n)` asks without holding anything.

**Features and entitlements**, cached for 60 seconds and deny by default:

```go
if ok, _ := billing.HasFeature(ctx, org, "sso"); ok { … }
limit, _ := billing.FeatureValue(ctx, org, "max_projects") // any: int64, string or bool; nil when off
ents, _ := billing.Entitlements(ctx, org)                   // ents.Meters["api.requests"].Allowance
```

**Schedule two calls**: `billing.Report(ctx, "")` every few seconds (sends the running totals; a
failed report is simply sent again, larger, next time) and `billing.Sweep(ctx)` every minute
(reclaims reservations whose request died before it committed).

**Keep the cache fresh from webhooks.** After verifying a delivery, pass it on:

```go
event, err := cboxbilling.ConstructWebhookEvent(body, r.Header.Get(cboxbilling.SignatureHeader), secret)
var data map[string]any
_ = json.Unmarshal(event.Data, &data)
_, _ = billing.HandleWebhook(ctx, event.Type, data) // entitlements.changed, subscription.*, …
```

**Share the state between processes** with Redis. The store takes any client through one function
that runs a raw command; with `github.com/redis/go-redis/v9`:

```go
store := enforcement.NewRedisStore(func(ctx context.Context, args ...string) (any, error) {
	a := make([]any, len(args))
	for i, v := range args {
		a[i] = v
	}
	reply, err := rdb.Do(ctx, a...).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return reply, err
})
billing, err := enforcement.New(token, baseURL, enforcement.WithStore(store))
```

Processes that report the same meters must share one store: Billing accepts one writer per
(organization, meter) usage stream.

**When Billing is down.** A refill that cannot reach Billing opens a breaker for 30 seconds,
and reservations go to the failure policy: `last_known` (the default) admits up to the allowance
it last knew, then a small outage grace (100 units per organization and meter per hour); `allow`
admits everything; `deny` refuses. Everything admitted is still committed and reported, and
Billing settles it once it is back. Entitlements and features are served from the cache, marked
`stale`, while it is unreachable.

| Error | When | Your answer |
| --- | --- | --- |
| `*enforcement.QuotaExceededError` | Billing granted nothing: the allowance is spent, the meter is off, a credit or overage cap is reached, or an agreement is not accepted (`reason`) | the customer's limit: a 429 or an upgrade prompt |
| `*enforcement.BillingRefusedError` | Billing refused the call (a 4xx such as a revoked token; `status`) | a bug or a config error: alert |
| `*enforcement.BillingUnavailableError` | Billing cannot be reached and the failure policy refuses | your choice: retry later, or 503 |

Match them with `errors.As`. An invalid argument (a negative estimate, a second commit) is
`enforcement.ErrInvalidArgument`.

Options: `WithLeaseSize`, `WithRefillThreshold`, `WithFailurePolicy`, `WithReservationTTL`,
`WithGraceUnits`, `WithGraceWindow`, `WithGraceMeter`, `WithBreakerCooldown`, `WithEntitlementsTTL`,
`WithTimeout`, `WithLeaseTimeout`, `WithPrefix`, `WithHTTPClient`, `WithBackgroundRefill` and
more; defaults are in SPEC.md. A runnable example: [`examples/enforcement`](examples/enforcement).

## Development

Files named `api_*.go`, `model_*.go`, `client.go`, `configuration.go`, `response.go`, `utils.go`,
`go.mod` and `go.sum` are generated by [`sdks/generate.sh`](../generate.sh). `cbox.go`,
`errors.go`, `idempotency.go`, `pagination.go`, `webhook.go`, the `enforcement/` package,
`examples/` and the tests are written by hand. Tests: [`sdks/test.sh go`](../test.sh), or
[`sdks/live.sh go`](../live.sh) to add the live tests against the app.

## Licence

MIT

package enforcement_test

// Runs the shared enforcement scenarios in sdks/conformance/scenarios against this package's
// Client, with a scripted Billing and a clock that moves only when a step says so.
// sdks/conformance/README.md describes the format; every SDK runs the same files.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cboxdk/billing-go/enforcement"
)

const start = int64(1_800_000_000_000)
const base = "http://billing.test/v1"

func scenarioDir() string {
	if dir := os.Getenv("CBOX_CONFORMANCE_DIR"); dir != "" {
		return dir
	}
	// sdks/conformance in this repository; conformance/ beside the module in the release mirror.
	for _, dir := range []string{filepath.Join("..", "..", "conformance", "scenarios"), filepath.Join("..", "conformance", "scenarios")} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return filepath.Join("..", "..", "conformance", "scenarios")
}

type scriptedBilling struct {
	queues   map[string][]map[string]any
	always   map[string]map[string]any
	received []received
}

type received struct {
	route string
	body  any
}

func (b *scriptedBilling) RoundTrip(req *http.Request) (*http.Response, error) {
	route := req.Method + " " + strings.TrimPrefix(req.URL.EscapedPath(), "/v1")
	var body any
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}
	b.received = append(b.received, received{route, body})
	var answer map[string]any
	if q := b.queues[route]; len(q) > 0 {
		answer, b.queues[route] = q[0], q[1:]
	} else if a, ok := b.always[route]; ok {
		answer = a
	} else {
		return nil, fmt.Errorf("billing has no answer left for %s", route)
	}
	if answer["network_error"] == true {
		return nil, errors.New("connection refused")
	}
	status := 200
	if s, ok := answer["status"].(float64); ok {
		status = int(s)
	}
	var text []byte
	if s, ok := answer["body"].(string); ok {
		text = []byte(s)
	} else {
		text, _ = json.Marshal(answer["body"])
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(text)), Header: http.Header{}, Request: req}, nil
}

func errorName(err error) string {
	var q *enforcement.QuotaExceededError
	var r *enforcement.BillingRefusedError
	var u *enforcement.BillingUnavailableError
	switch {
	case errors.As(err, &q):
		return "QuotaExceeded"
	case errors.As(err, &r):
		return "BillingRefused"
	case errors.As(err, &u):
		return "BillingUnavailable"
	case errors.Is(err, enforcement.ErrInvalidArgument):
		return "InvalidArgument"
	}
	return "unexpected " + err.Error()
}

func holdView(h enforcement.Hold) map[string]any {
	return map[string]any{"backed_by_lease": h.BackedByLease(), "grace": h.Grace()}
}

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

func str(v any) string { s, _ := v.(string); return s }

func durationOpt(v any) time.Duration {
	f, _ := v.(float64)
	return time.Duration(f * float64(time.Second))
}

func options(t *testing.T, config map[string]any) []enforcement.Option {
	var opts []enforcement.Option
	for name, v := range config {
		switch name {
		case "lease_size":
			opts = append(opts, enforcement.WithLeaseSize(num(v)))
		case "refill_threshold":
			opts = append(opts, enforcement.WithRefillThreshold(num(v)))
		case "failure_policy":
			opts = append(opts, enforcement.WithFailurePolicy(enforcement.FailurePolicy(str(v))))
		case "reservation_ttl":
			opts = append(opts, enforcement.WithReservationTTL(durationOpt(v)))
		case "grace_units":
			opts = append(opts, enforcement.WithGraceUnits(num(v)))
		case "grace_window":
			opts = append(opts, enforcement.WithGraceWindow(durationOpt(v)))
		case "grace_meters":
			for meter, n := range v.(map[string]any) {
				opts = append(opts, enforcement.WithGraceMeter(meter, num(n)))
			}
		case "breaker_cooldown":
			opts = append(opts, enforcement.WithBreakerCooldown(durationOpt(v)))
		case "snapshot_refresh":
			opts = append(opts, enforcement.WithSnapshotRefresh(durationOpt(v)))
		case "entitlements_ttl":
			opts = append(opts, enforcement.WithEntitlementsTTL(durationOpt(v)))
		case "lock_ttl":
			opts = append(opts, enforcement.WithLockTTL(durationOpt(v)))
		case "lock_wait":
			opts = append(opts, enforcement.WithLockWait(durationOpt(v)))
		default:
			t.Fatalf("unknown config %s", name)
		}
	}
	return opts
}

// normalize turns a result into the same shape json.Unmarshal gives the expectation.
func normalize(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestConformance(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(scenarioDir(), "*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no scenarios in %s", scenarioDir())
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) { runScenario(t, file) })
	}
}

func runScenario(t *testing.T, file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var scenario map[string]any
	if err := json.Unmarshal(raw, &scenario); err != nil {
		t.Fatal(err)
	}
	now := start
	clock := func() time.Time { return time.UnixMilli(now) }
	billing := &scriptedBilling{queues: map[string][]map[string]any{}, always: map[string]map[string]any{}}
	script, _ := scenario["billing"].(map[string]any)
	for route, answer := range script {
		if list, ok := answer.([]any); ok {
			for _, a := range list {
				billing.queues[route] = append(billing.queues[route], a.(map[string]any))
			}
		} else {
			billing.always[route] = answer.(map[string]any)["always"].(map[string]any)
		}
	}
	config, _ := scenario["config"].(map[string]any)
	opts := append([]enforcement.Option{
		enforcement.WithHTTPClient(&http.Client{Transport: billing}),
		enforcement.WithClock(clock),
		enforcement.WithStore(enforcement.NewMemoryStore(clock)),
		enforcement.WithBackgroundRefill(false),
	}, options(t, config)...)
	client, err := enforcement.New("cbt_conformance", base, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	saved := map[string]enforcement.Held{}

	for i, s := range scenario["steps"].([]any) {
		step := s.(map[string]any)
		where := fmt.Sprintf("step %d (%s)", i+1, step["op"])
		expect, _ := step["expect"].(map[string]any)
		org := str(step["org"])
		var result any
		var err error
		switch step["op"] {
		case "reserve":
			var r *enforcement.Reservation
			r, err = client.Reserve(ctx, org, str(step["meter"]), num(step["estimate"]))
			if err == nil {
				if name := str(step["save"]); name != "" {
					saved[name] = r
				}
				result = holdView(r.Hold)
			}
		case "reserve_many":
			meters := map[string]int64{}
			for m, n := range step["meters"].(map[string]any) {
				meters[m] = num(n)
			}
			var r *enforcement.ReservationSet
			r, err = client.ReserveMany(ctx, org, meters)
			if err == nil {
				if name := str(step["save"]); name != "" {
					saved[name] = r
				}
				buckets := map[string]any{}
				for _, h := range r.Holds {
					buckets[h.Meter] = holdView(h)
				}
				result = map[string]any{"buckets": buckets}
			}
		case "commit":
			if m, ok := step["actual"].(map[string]any); ok {
				actuals := enforcement.PerMeter{}
				for meter, n := range m {
					actuals[meter] = num(n)
				}
				err = client.Commit(ctx, saved[str(step["ref"])], actuals)
			} else {
				err = client.Commit(ctx, saved[str(step["ref"])], enforcement.Units(num(step["actual"])))
			}
		case "release":
			err = client.Release(ctx, saved[str(step["ref"])])
		case "can":
			result, err = client.Can(ctx, org, str(step["meter"]), num(step["n"]))
		case "balance":
			result, err = client.Balance(ctx, org, str(step["meter"]))
		case "report":
			result, err = client.Report(ctx, org)
		case "sweep":
			result, err = client.Sweep(ctx)
		case "entitlements":
			var e *enforcement.Entitlements
			e, err = client.Entitlements(ctx, org)
			if err == nil {
				result = map[string]any{"stale": e.Stale, "meters": e.Meters}
			}
		case "features":
			var f *enforcement.Features
			f, err = client.Features(ctx, org)
			if err == nil {
				result = map[string]any{"stale": f.Stale, "suspended": f.Suspended, "features": f.Features}
			}
		case "has_feature":
			result, err = client.HasFeature(ctx, org, str(step["key"]))
		case "feature_value":
			result, err = client.FeatureValue(ctx, org, str(step["key"]))
		case "cost":
			result, err = client.Cost(ctx, org, str(step["meter"]), num(step["units"]))
		case "invalidate":
			err = client.Invalidate(ctx, org)
		case "handle_webhook":
			event := step["event"].(map[string]any)
			data, _ := event["data"].(map[string]any)
			result, err = client.HandleWebhook(ctx, str(event["type"]), data)
		case "advance":
			now += int64(step["seconds"].(float64) * 1000)
		case "respond":
			route := str(step["route"])
			for _, a := range step["with"].([]any) {
				billing.queues[route] = append(billing.queues[route], a.(map[string]any))
			}
		case "expect_requests":
			got := billing.received
			billing.received = nil
			want := step["requests"].([]any)
			var gotRoutes, wantRoutes []string
			for _, g := range got {
				gotRoutes = append(gotRoutes, g.route)
			}
			for _, w := range want {
				wantRoutes = append(wantRoutes, str(w.(map[string]any)["route"]))
			}
			if !reflect.DeepEqual(gotRoutes, wantRoutes) {
				t.Fatalf("%s: requests Billing received\n got: %v\nwant: %v", where, gotRoutes, wantRoutes)
			}
			for j, w := range want {
				if body, ok := w.(map[string]any)["body"]; ok && !reflect.DeepEqual(got[j].body, body) {
					t.Fatalf("%s: body of request %d\n got: %v\nwant: %v", where, j+1, got[j].body, body)
				}
			}
		default:
			t.Fatalf("unknown op %v", step["op"])
		}

		if err != nil {
			if expect["error"] == nil {
				t.Fatalf("%s returned %v", where, err)
			}
			if name := errorName(err); name != expect["error"] {
				t.Fatalf("%s: error %s, want %v (%v)", where, name, expect["error"], err)
			}
			if reason, ok := expect["reason"]; ok {
				var q *enforcement.QuotaExceededError
				if !errors.As(err, &q) || q.Reason != reason {
					t.Fatalf("%s: reason %v, want %v", where, err, reason)
				}
			}
			if status, ok := expect["status"]; ok {
				var r *enforcement.BillingRefusedError
				if !errors.As(err, &r) || float64(r.StatusCode) != status {
					t.Fatalf("%s: status %v, want %v", where, err, status)
				}
			}
			continue
		}
		if expect["error"] != nil {
			t.Fatalf("%s should have returned %v", where, expect["error"])
		}
		if want, ok := expect["result"]; ok {
			if got := normalize(result); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: result\n got: %#v\nwant: %#v", where, got, want)
			}
		}
	}

	for route, q := range billing.queues {
		if len(q) > 0 {
			t.Fatalf("scripted answers left unused for %s", route)
		}
	}
}

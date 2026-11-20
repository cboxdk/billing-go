package enforcement_test

// The enforcement client against a real Cbox Billing app in test mode.
//
// Runs when CBOX_BILLING_LIVE_URL (the API root, such as http://127.0.0.1:8000/api/v1) and
// CBOX_BILLING_LIVE_TOKEN (a cbt_ token from sdks/testing/seed-live.php) are set; CI sets both.
// The organization org_sdk_live_go is on the Team plan: api.requests is billed past 1,000,000,
// storage.gb is capped at 100, sso is on, compliance is off and max_projects is 10.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cboxdk/billing-go/enforcement"
)

const liveOrg = "org_sdk_live_go"

func liveClient(t *testing.T, token string) *enforcement.Client {
	t.Helper()
	url, envToken := os.Getenv("CBOX_BILLING_LIVE_URL"), os.Getenv("CBOX_BILLING_LIVE_TOKEN")
	if url == "" || envToken == "" {
		t.Skip("CBOX_BILLING_LIVE_URL and CBOX_BILLING_LIVE_TOKEN are not set")
	}
	if token == "" {
		token = envToken
	}
	c, err := enforcement.New(token, url, enforcement.WithBackgroundRefill(false))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLiveEntitlementsAndFeatures(t *testing.T) {
	ctx := context.Background()
	billing := liveClient(t, "")
	ents, err := billing.Entitlements(ctx, liveOrg)
	if err != nil {
		t.Fatal(err)
	}
	storage := ents.Meters["storage.gb"]
	if !ents.Enabled("api.requests") || storage.Allowance == nil || *storage.Allowance != 100 || storage.Overage != "block" || ents.Stale {
		t.Fatalf("unexpected entitlements: %+v", ents)
	}

	if on, err := billing.HasFeature(ctx, liveOrg, "sso"); err != nil || !on {
		t.Fatalf("sso = %v, %v; want on", on, err)
	}
	if on, err := billing.HasFeature(ctx, liveOrg, "compliance"); err != nil || on {
		t.Fatalf("compliance = %v, %v; want off", on, err)
	}
	if v, err := billing.FeatureValue(ctx, liveOrg, "max_projects"); err != nil || v != int64(10) {
		t.Fatalf("max_projects = %#v, %v; want 10", v, err)
	}
}

func TestLiveReserveCommitAndReport(t *testing.T) {
	ctx := context.Background()
	billing := liveClient(t, "")
	r, err := billing.Reserve(ctx, liveOrg, "api.requests", 5)
	if err != nil || !r.Hold.BackedByLease() {
		t.Fatalf("Reserve = %+v, %v", r, err)
	}
	if left, _ := billing.Balance(ctx, liveOrg, "api.requests"); left != 95 {
		t.Fatalf("balance after reserving 5 of 100 = %d", left)
	}
	if err := billing.Commit(ctx, r, enforcement.Units(3)); err != nil {
		t.Fatal(err)
	}
	if left, _ := billing.Balance(ctx, liveOrg, "api.requests"); left != 97 {
		t.Fatalf("balance after committing 3 = %d", left)
	}
	if n, err := billing.Report(ctx, liveOrg); err != nil || n != 1 {
		t.Fatalf("Report = %d, %v; Billing should accept the running total", n, err)
	}
	if n, err := billing.Report(ctx, liveOrg); err != nil || n != 0 {
		t.Fatalf("second Report = %d, %v; an unchanged stream is not sent again", n, err)
	}
}

func TestLiveCappedMeterRefusesPastItsAllowance(t *testing.T) {
	_, err := liveClient(t, "").Reserve(context.Background(), liveOrg, "storage.gb", 150)
	var q *enforcement.QuotaExceededError
	if !errors.As(err, &q) || q.Reason != "quota_exhausted" {
		t.Fatalf("Reserve past the cap = %v; want QuotaExceeded (quota_exhausted)", err)
	}
}

func TestLiveBadTokenIsRefusedNotAnOutage(t *testing.T) {
	_, err := liveClient(t, "cbt_not_a_token").Reserve(context.Background(), liveOrg, "api.requests", 1)
	var r *enforcement.BillingRefusedError
	if !errors.As(err, &r) || r.StatusCode != 401 {
		t.Fatalf("Reserve with a bad token = %v; want BillingRefused 401", err)
	}
}

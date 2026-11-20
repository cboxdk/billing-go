// Command enforcement meters work against Cbox Billing with the enforcement client: a feature
// gate, a reserve → work → commit cycle per request, a refusal handled as the customer's limit,
// and the report and sweep you would schedule.
//
//	CBOX_BILLING_URL=https://api.cboxbilling.com/v1 CBOX_BILLING_TOKEN=cbt_… CBOX_BILLING_ORG=org_acme \
//	  go run ./examples/enforcement
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cboxdk/billing-go/enforcement"
)

func main() {
	ctx := context.Background()
	org := os.Getenv("CBOX_BILLING_ORG")

	// One client per process; share it between goroutines. For several processes or hosts, give
	// them one enforcement.NewRedisStore(…) through enforcement.WithStore.
	billing, err := enforcement.New(os.Getenv("CBOX_BILLING_TOKEN"), os.Getenv("CBOX_BILLING_URL"))
	if err != nil {
		log.Fatal(err)
	}

	// A boolean feature, and a config value. Both are cached and deny by default.
	sso, err := billing.HasFeature(ctx, org, "sso")
	if err != nil {
		log.Fatal(err)
	}
	projects, err := billing.FeatureValue(ctx, org, "max_projects")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("sso: %v, max_projects: %v\n", sso, projects)

	// Each request: hold an estimate, do the work, commit what it really used.
	for i := 0; i < 3; i++ {
		used, err := handle(ctx, billing, org)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("request %d used %d api.requests\n", i+1, used)
	}

	// A meter capped at 100 refuses a larger reservation: that is the customer's limit, not an error
	// in your app.
	if _, err := billing.Reserve(ctx, org, "storage.gb", 150); err != nil {
		var quota *enforcement.QuotaExceededError
		if !errors.As(err, &quota) {
			log.Fatal(err)
		}
		fmt.Printf("storage.gb refused: %s\n", quota.Reason)
	}

	// Schedule these: Report every few seconds, Sweep every minute.
	reported, err := billing.Report(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	swept, err := billing.Sweep(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("reported %d organization(s), swept %d reservation(s)\n", reported, swept)
}

// handle is one request: reserve, work, commit. Release on the error path gives the hold back;
// after a commit it does nothing, so it is safe to defer.
func handle(ctx context.Context, billing *enforcement.Client, org string) (int64, error) {
	r, err := billing.Reserve(ctx, org, "api.requests", 10)
	if err != nil {
		return 0, err // *QuotaExceededError: answer 429; *BillingRefusedError: check the token
	}
	defer billing.Release(ctx, r) //nolint:errcheck // a no-op once committed

	time.Sleep(10 * time.Millisecond) // the work
	used := int64(4)

	return used, billing.Commit(ctx, r, enforcement.Units(used))
}

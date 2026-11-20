package cboxbilling_test

// Smoke tests against a Prism mock server started from the OpenAPI contract (sdks/test.sh starts
// it). Prism checks every request against the spec and answers with the spec's examples.

import (
	"context"
	"errors"
	"os"
	"testing"

	cboxbilling "github.com/cboxdk/billing-go"
)

func mockURL() string {
	if url := os.Getenv("CBOX_BILLING_MOCK_URL"); url != "" {
		return url
	}
	return "http://127.0.0.1:4010"
}

func newClient(t *testing.T, opts ...cboxbilling.Option) *cboxbilling.APIClient {
	t.Helper()
	client, err := cboxbilling.NewClient("cbt_test_token", mockURL(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestListsPlansWithABearerTokenAndParsesTheModels(t *testing.T) {
	plans, _, err := newClient(t).PlansAPI.ListPlans(context.Background()).Execute()
	if err != nil {
		t.Fatal(err)
	}
	if plans.GetCurrency() != "EUR" || len(plans.GetData()) == 0 || plans.GetData()[0].GetKey() != "pro" || plans.GetHasMore() {
		t.Fatalf("unexpected plans: %+v", plans)
	}
}

func TestARequestWithoutATokenIsUnauthenticated(t *testing.T) {
	// The generated client on its own, with no Authorization header.
	cfg := cboxbilling.NewConfiguration()
	cfg.Servers = cboxbilling.ServerConfigurations{{URL: mockURL()}}
	_, resp, err := cboxbilling.NewAPIClient(cfg).PlansAPI.ListPlans(context.Background()).Execute()
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("expected a 401, got %v", err)
	}

	// And through NewClient with a token the server refuses, as a typed error.
	client := newClient(t, cboxbilling.WithHeader("Prefer", "code=401"))
	_, _, err = client.PlansAPI.ListPlans(context.Background()).Execute()
	var apiErr *cboxbilling.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || !errors.Is(err, cboxbilling.ErrUnauthenticated) {
		t.Fatalf("expected a typed 401, got %v", err)
	}
}

func TestAnErrorBodyBecomesATypedErrorCarryingErrorAndMessage(t *testing.T) {
	client := newClient(t, cboxbilling.WithHeader("Prefer", "code=404"))
	_, _, err := client.SubscriptionsAPI.GetSubscription(context.Background(), "org_acme").Execute()

	var apiErr *cboxbilling.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *cboxbilling.Error, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 404 || !errors.Is(err, cboxbilling.ErrNotFound) || errors.Is(err, cboxbilling.ErrConflict) {
		t.Fatalf("expected a not-found error, got %v", apiErr)
	}
	if apiErr.Kind == "" || apiErr.Message == "" {
		t.Fatalf("expected error and message from the body, got %+v", apiErr)
	}
}

func TestCreatesAnInvoiceWithAnIdempotencyKey(t *testing.T) {
	line := cboxbilling.NewInvoiceLineRequest("Consulting", "2.000", 50000)
	invoice, _, err := newClient(t).InvoicesAPI.CreateInvoice(context.Background(), "org_acme").
		IdempotencyKey(cboxbilling.NewIdempotencyKey()).
		InvoiceRequest(*cboxbilling.NewInvoiceRequest([]cboxbilling.InvoiceLineRequest{*line})).
		Execute()
	if err != nil {
		t.Fatal(err)
	}
	if invoice.GetNumber() == "" {
		t.Fatalf("expected an invoice number, got %+v", invoice)
	}
}

func TestPaginateFollowsNextCursorAcrossPages(t *testing.T) {
	client := newClient(t)
	var cursors []string
	pages := cboxbilling.Paginate[cboxbilling.Invoice](func(cursor string) (*cboxbilling.InvoiceList, error) {
		cursors = append(cursors, cursor)
		req := client.InvoicesAPI.ListInvoices(context.Background(), "org_acme")
		if cursor != "" {
			req = req.Cursor(cursor)
		}
		page, _, err := req.Execute()
		return page, err
	})

	// The mock answers every page with `has_more: true`, so stop after three items.
	seen := 0
	for _, err := range pages {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if seen == 3 {
			break
		}
	}

	if seen != 3 || len(cursors) != 3 || cursors[0] != "" || cursors[1] != "eyJrIjo0Mn0" || cursors[2] != "eyJrIjo0Mn0" {
		t.Fatalf("unexpected pagination: seen=%d cursors=%v", seen, cursors)
	}
}

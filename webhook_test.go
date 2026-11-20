package cboxbilling_test

// The fixture is a delivery signed by the app's own WebhookDeliverer
// (sdks/fixtures/generate-webhook-fixture.php), so this proves the helper accepts what the server
// really sends, and refuses anything else.

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	cboxbilling "github.com/cboxdk/billing-go"
)

type fixture struct {
	Secret    string            `json:"secret"`
	Timestamp int64             `json:"timestamp"`
	Body      string            `json:"body"`
	Headers   map[string]string `json:"headers"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("../fixtures/webhook.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func verifierAt(f fixture, secret string, offset time.Duration, tolerance time.Duration) cboxbilling.WebhookVerifier {
	return cboxbilling.WebhookVerifier{
		Secret:    secret,
		Tolerance: tolerance,
		Now:       func() time.Time { return time.Unix(f.Timestamp, 0).Add(offset) },
	}
}

func TestVerifiesADeliverySignedByTheApp(t *testing.T) {
	f := loadFixture(t)
	signature := f.Headers[cboxbilling.SignatureHeader]

	event, err := verifierAt(f, f.Secret, 0, 0).ConstructEvent([]byte(f.Body), signature)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != f.Headers[cboxbilling.EventTypeHeader] || event.DeliveryID != f.Headers[cboxbilling.DeliveryIDHeader] {
		t.Fatalf("unexpected event: %+v", event)
	}
}

func TestRefusesATamperedBodyAReEncodedBodyAndAWrongSecret(t *testing.T) {
	f := loadFixture(t)
	signature := f.Headers[cboxbilling.SignatureHeader]

	var decoded any
	if err := json.Unmarshal([]byte(f.Body), &decoded); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		payload string
		secret  string
	}{
		"tampered":   {f.Body + " ", f.Secret},
		"re-encoded": {string(reencoded), f.Secret},
		"wrong key":  {f.Body, "whsec_wrong"},
	}
	for name, c := range cases {
		if c.payload == f.Body && c.secret == f.Secret {
			t.Fatalf("%s: the case must differ from the fixture", name)
		}
		err := verifierAt(f, c.secret, 0, 0).Verify([]byte(c.payload), signature)
		if !errors.Is(err, cboxbilling.ErrWebhookSignature) {
			t.Fatalf("%s: expected ErrWebhookSignature, got %v", name, err)
		}
	}
}

func TestRefusesADeliveryOutsideTheToleranceWindowUnlessTheCheckIsOff(t *testing.T) {
	f := loadFixture(t)
	signature := f.Headers[cboxbilling.SignatureHeader]

	if err := verifierAt(f, f.Secret, 301*time.Second, 0).Verify([]byte(f.Body), signature); !errors.Is(err, cboxbilling.ErrWebhookSignature) {
		t.Fatalf("expected a stale delivery to fail, got %v", err)
	}
	if err := verifierAt(f, f.Secret, 301*time.Second, -1).Verify([]byte(f.Body), signature); err != nil {
		t.Fatalf("expected the check to be off, got %v", err)
	}
	// The package function uses the real clock, so the old fixture is refused as a replay.
	if err := cboxbilling.VerifyWebhook([]byte(f.Body), signature, f.Secret); !errors.Is(err, cboxbilling.ErrWebhookSignature) {
		t.Fatalf("expected the fixture to be stale by now, got %v", err)
	}
}

func TestRefusesAMissingOrMalformedHeader(t *testing.T) {
	f := loadFixture(t)
	for _, header := range []string{"", "v1=abc", "t=1793723794", "t=soon,v1=" + strings.Repeat("0", 64)} {
		if err := verifierAt(f, f.Secret, 0, 0).Verify([]byte(f.Body), header); !errors.Is(err, cboxbilling.ErrWebhookSignature) {
			t.Fatalf("header %q: expected ErrWebhookSignature, got %v", header, err)
		}
	}
}

type fakePage struct {
	more bool
	next string
	data []int
}

func (p *fakePage) GetHasMore() bool      { return p.more }
func (p *fakePage) GetNextCursor() string { return p.next }
func (p *fakePage) GetData() []int        { return p.data }

func TestPaginateStopsOnTheLastPageAndSurfacesErrors(t *testing.T) {
	pages := []*fakePage{{true, "c1", []int{1, 2}}, {false, "", []int{3}}}
	var cursors []string
	var items []int
	for item, err := range cboxbilling.Paginate[int](func(cursor string) (*fakePage, error) {
		cursors = append(cursors, cursor)
		return pages[len(cursors)-1], nil
	}) {
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if len(items) != 3 || items[2] != 3 || len(cursors) != 2 || cursors[1] != "c1" {
		t.Fatalf("unexpected pagination: items=%v cursors=%v", items, cursors)
	}

	boom := errors.New("boom")
	for _, err := range cboxbilling.Paginate[int](func(string) (*fakePage, error) { return nil, boom }) {
		if !errors.Is(err, boom) {
			t.Fatalf("expected the fetch error, got %v", err)
		}
	}
}

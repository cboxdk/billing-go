package cboxbilling

// Hand-written (not generated): webhook signature verification.
//
// Every delivery carries `X-Cbox-Signature: t={unix timestamp},v1={hex HMAC-SHA256}`. The MAC is
// computed with the endpoint's signing secret over "{t}.{raw body}". Verify against the raw
// request body, byte for byte: parsing and re-encoding the JSON changes it.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Webhook header names.
const (
	SignatureHeader  = "X-Cbox-Signature"
	TimestampHeader  = "X-Cbox-Timestamp"
	EventTypeHeader  = "X-Cbox-Event-Type"
	DeliveryIDHeader = "X-Cbox-Delivery-Id"
)

// DefaultWebhookTolerance is how far a delivery's timestamp may be from now before it is refused
// as a replay.
const DefaultWebhookTolerance = 5 * time.Minute

// ErrWebhookSignature is returned (wrapped) for every delivery that does not verify.
var ErrWebhookSignature = errors.New("cboxbilling: webhook signature verification failed")

// WebhookEvent is the envelope every delivery's body holds.
type WebhookEvent struct {
	// ID is the source event's id. The same event delivered twice has the same ID.
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Data       json.RawMessage `json:"data"`
	DeliveryID string          `json:"delivery_id"`
	CreatedAt  string          `json:"created_at"`
}

// WebhookVerifier verifies deliveries for one endpoint secret.
type WebhookVerifier struct {
	Secret string
	// Tolerance is the allowed clock difference. Zero means DefaultWebhookTolerance; a negative
	// value turns the replay check off.
	Tolerance time.Duration
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// VerifyWebhook verifies a delivery with the default tolerance.
func VerifyWebhook(payload []byte, signatureHeader, secret string) error {
	return WebhookVerifier{Secret: secret}.Verify(payload, signatureHeader)
}

// ConstructWebhookEvent verifies a delivery with the default tolerance and parses its envelope.
func ConstructWebhookEvent(payload []byte, signatureHeader, secret string) (*WebhookEvent, error) {
	return WebhookVerifier{Secret: secret}.ConstructEvent(payload, signatureHeader)
}

// Verify returns nil when the delivery is signed with v.Secret and within the tolerance.
func (v WebhookVerifier) Verify(payload []byte, signatureHeader string) error {
	if signatureHeader == "" {
		return wrapSignature("missing " + SignatureHeader + " header")
	}

	timestamp, signatures, ok := parseSignatureHeader(signatureHeader)
	if !ok {
		return wrapSignature("malformed " + SignatureHeader + " header")
	}

	tolerance := v.Tolerance
	if tolerance == 0 {
		tolerance = DefaultWebhookTolerance
	}
	if tolerance > 0 {
		now := time.Now()
		if v.Now != nil {
			now = v.Now()
		}
		diff := now.Unix() - timestamp
		if diff < 0 {
			diff = -diff
		}
		if diff > int64(tolerance/time.Second) {
			return wrapSignature("timestamp outside the tolerance window")
		}
	}

	mac := hmac.New(sha256.New, []byte(v.Secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(payload)
	expected := mac.Sum(nil)

	for _, candidate := range signatures {
		provided, err := hex.DecodeString(candidate)
		if err == nil && hmac.Equal(expected, provided) {
			return nil
		}
	}
	return wrapSignature("no signature matches the payload")
}

// ConstructEvent verifies a delivery and parses its envelope.
func (v WebhookVerifier) ConstructEvent(payload []byte, signatureHeader string) (*WebhookEvent, error) {
	if err := v.Verify(payload, signatureHeader); err != nil {
		return nil, err
	}
	var event WebhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func parseSignatureHeader(header string) (int64, []string, bool) {
	var timestamp int64 = -1
	var signatures []string

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			if value == "" || strings.Trim(value, "0123456789") != "" {
				continue
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err == nil {
				timestamp = parsed
			}
		case "v1":
			if value != "" {
				signatures = append(signatures, value)
			}
		}
	}

	return timestamp, signatures, timestamp >= 0 && len(signatures) > 0
}

func wrapSignature(reason string) error {
	return errors.Join(ErrWebhookSignature, errors.New(reason))
}

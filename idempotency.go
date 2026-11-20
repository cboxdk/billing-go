package cboxbilling

// Hand-written (not generated).

import (
	"crypto/rand"
	"fmt"
)

// NewIdempotencyKey returns a fresh Idempotency-Key (a random UUID v4). Generate it once per
// business action and keep it with that action, so a retry after a timeout sends the same key
// and the server replays the first response instead of doing the work twice.
func NewIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("cboxbilling: no randomness for an idempotency key: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

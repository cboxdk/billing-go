package enforcement

import (
	"errors"
	"fmt"
)

// ErrInvalidArgument is wrapped by every error for a bad argument: an estimate that is not
// positive, an actual out of range or missing, or a reservation already settled.
var ErrInvalidArgument = errors.New("enforcement: invalid argument")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// QuotaExceededError: Billing granted nothing (or, in an outage, the last known allowance is
// spent). Always refused, whatever the failure policy.
type QuotaExceededError struct {
	Org       string
	Meter     string
	Requested int64
	// Reason is quota_exhausted, the lease's reason (credit_limit_reached, overage_cap_reached,
	// seat_required), agreement_required, or in an outage outage_limit or meter_disabled.
	Reason string
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("enforcement: allowance exhausted for meter [%s] on org [%s]: cannot reserve %d unit(s) (%s)", e.Meter, e.Org, e.Requested, e.Reason)
}

// BillingRefusedError: Billing answered with a refusal (a 4xx other than 408 and 429). Always refused.
type BillingRefusedError struct {
	Endpoint   string
	StatusCode int
	// Body is the response body, parsed when it was JSON.
	Body any
}

func (e *BillingRefusedError) Error() string {
	return fmt.Sprintf("enforcement: Billing refused [%s] with HTTP %d", e.Endpoint, e.StatusCode)
}

// BillingUnavailableError: Billing could not be reached or gave no usable answer. Resolved by the
// failure policy.
type BillingUnavailableError struct {
	Endpoint string
	// StatusCode is 0 when no response arrived.
	StatusCode int
	Message    string
	Err        error
}

func (e *BillingUnavailableError) Error() string { return "enforcement: " + e.Message }

func (e *BillingUnavailableError) Unwrap() error { return e.Err }

func isUnavailable(err error) bool {
	var u *BillingUnavailableError
	return errors.As(err, &u)
}

// isBilling is true for an error that came from Billing (a refusal or an outage), as opposed to
// a store failure.
func isBilling(err error) bool {
	var r *BillingRefusedError
	return isUnavailable(err) || errors.As(err, &r)
}

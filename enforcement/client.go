// Package enforcement meters usage against Cbox Billing without a request to Billing per call.
//
// Reservations take units from a slice of the organization's allowance leased into a Store,
// Billing is asked only to refill, and committed usage is reported as running totals. The same
// client, with the same behaviour, exists in every Cbox Billing SDK; sdks/conformance/SPEC.md in
// the cbox-billing repository describes it.
//
//	client, err := enforcement.New(os.Getenv("CBOX_BILLING_TOKEN"), "https://api.cboxbilling.com/v1")
//	r, err := client.Reserve(ctx, "org_acme", "api.requests", 1)
//	if err != nil { … } // *QuotaExceededError, *BillingRefusedError, *BillingUnavailableError
//	defer client.Release(ctx, r) // does nothing once committed
//	… do the work …
//	err = client.Commit(ctx, r, enforcement.Units(1))
package enforcement

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FailurePolicy decides what happens when Billing cannot be reached and the slice is too small.
type FailurePolicy string

const (
	// LastKnown admits up to the allowance last known, then a small outage grace. The default.
	LastKnown FailurePolicy = "last_known"
	// Allow admits everything.
	Allow FailurePolicy = "allow"
	// Deny refuses with a *BillingUnavailableError.
	Deny FailurePolicy = "deny"
)

// HoldKind is where a hold's units came from.
type HoldKind string

const (
	HoldLease    HoldKind = "lease"    // the local slice
	HoldAdmitted HoldKind = "admitted" // in an outage: the last known allowance
	HoldGrace    HoldKind = "grace"    // in an outage: the outage grace
	HoldAllowed  HoldKind = "allowed"  // in an outage: the allow policy
)

// Hold is the units held on one meter.
type Hold struct {
	Meter  string   `json:"meter"`
	Amount int64    `json:"amount"`
	Kind   HoldKind `json:"kind"`
	// GraceWindow is the window a grace hold was taken from.
	GraceWindow *int64 `json:"grace_window"`
}

// BackedByLease is true when the units were taken from a leased slice.
func (h Hold) BackedByLease() bool { return h.Kind == HoldLease }

// Grace is true when the units came from the outage grace.
func (h Hold) Grace() bool { return h.Kind == HoldGrace }

// Held is a Reservation or a ReservationSet.
type Held interface {
	reservation() (id, org string, holds []Hold)
}

// Reservation is units held on one meter. Settle it with Commit or Release.
type Reservation struct {
	ID   string
	Org  string
	Hold Hold
}

func (r *Reservation) reservation() (string, string, []Hold) { return r.ID, r.Org, []Hold{r.Hold} }

// ReservationSet is units held on several meters at once. Settle it with Commit (PerMeter) or Release.
type ReservationSet struct {
	ID    string
	Org   string
	Holds []Hold
}

func (r *ReservationSet) reservation() (string, string, []Hold) { return r.ID, r.Org, r.Holds }

// Buckets is the hold per meter.
func (r *ReservationSet) Buckets() map[string]Hold {
	out := make(map[string]Hold, len(r.Holds))
	for _, h := range r.Holds {
		out[h.Meter] = h
	}
	return out
}

// MeterEntitlement is one meter's entitlement, as GET /entitlements/{org} resolves it.
type MeterEntitlement struct {
	Enabled bool `json:"enabled"`
	// Allowance is the included units per period; nil when unlimited or disabled.
	Allowance *int64 `json:"allowance"`
	// Weight is the billable cost per unit; nil when disabled.
	Weight *float64 `json:"weight"`
	// Overage is block (a hard limit), bill (paid overage), or another behaviour Billing names.
	Overage string `json:"overage"`
}

func (m MeterEntitlement) capped() bool {
	return m.Enabled && m.Allowance != nil && m.Overage != "bill"
}

// Entitlements is an organization's per-meter entitlements. Deny by default.
type Entitlements struct {
	Org    string
	Meters map[string]MeterEntitlement
	// Stale is true when Billing could not be reached and this is the last answer it gave.
	Stale bool
}

// Enabled reports whether the meter is known and enabled.
func (e *Entitlements) Enabled(meter string) bool {
	m, ok := e.Meters[meter]
	return ok && m.Enabled
}

// Cost is units × weight; 0 for an unknown meter, a meter without a weight or no units.
func (e *Entitlements) Cost(meter string, units int64) float64 {
	m, ok := e.Meters[meter]
	if !ok || m.Weight == nil || units <= 0 {
		return 0
	}
	return float64(units) * *m.Weight
}

// FeatureDecision is one boolean or config feature.
type FeatureDecision struct {
	// Type is "boolean", "config", or nil.
	Type    *string `json:"type"`
	Enabled bool    `json:"enabled"`
	// Value is an int64, a string, or nil.
	Value any `json:"value"`
	// Source is plan, override or default.
	Source string `json:"source"`
}

// Features is an organization's boolean and config features. Deny by default; a suspended
// account has none.
type Features struct {
	Org       string
	Features  map[string]FeatureDecision
	Suspended bool
	Stale     bool
}

// Enabled is true when the feature is granted and the account is not suspended.
func (f *Features) Enabled(key string) bool {
	d, ok := f.Features[key]
	return !f.Suspended && ok && d.Enabled
}

// Value is a config feature's value (int64 or string), true for a boolean feature, nil when not granted.
func (f *Features) Value(key string) any {
	if !f.Enabled(key) {
		return nil
	}
	if d := f.Features[key]; d.Type != nil && *d.Type == "config" {
		return d.Value
	}
	return true
}

// Option configures New. Defaults are in sdks/conformance/SPEC.md.
type Option func(*Client)

func WithStore(s Store) Option                  { return func(c *Client) { c.store = s } }
func WithLeaseSize(n int64) Option              { return func(c *Client) { c.leaseSize = n } }
func WithRefillThreshold(n int64) Option        { return func(c *Client) { c.refillThreshold = n } }
func WithFailurePolicy(p FailurePolicy) Option  { return func(c *Client) { c.policy = p } }
func WithReservationTTL(d time.Duration) Option { return func(c *Client) { c.reservationTTL = d } }
func WithGraceUnits(n int64) Option             { return func(c *Client) { c.graceUnits = n } }
func WithGraceWindow(d time.Duration) Option    { return func(c *Client) { c.graceWindow = d } }
func WithGraceMeter(meter string, n int64) Option {
	return func(c *Client) { c.graceMeters[meter] = n }
}
func WithBreakerCooldown(d time.Duration) Option { return func(c *Client) { c.breakerCooldown = d } }
func WithSnapshotRefresh(d time.Duration) Option { return func(c *Client) { c.snapshotRefresh = d } }
func WithEntitlementsTTL(d time.Duration) Option { return func(c *Client) { c.entitlementsTTL = d } }
func WithLockTTL(d time.Duration) Option         { return func(c *Client) { c.lockTTL = d } }
func WithLockWait(d time.Duration) Option        { return func(c *Client) { c.lockWait = d } }
func WithTimeout(d time.Duration) Option         { return func(c *Client) { c.transport.timeout = d } }
func WithLeaseTimeout(d time.Duration) Option    { return func(c *Client) { c.leaseTimeout = d } }
func WithPrefix(p string) Option                 { return func(c *Client) { c.prefix = p } }
func WithHTTPClient(h *http.Client) Option       { return func(c *Client) { c.transport.http = h } }
func WithHeader(name, value string) Option {
	return func(c *Client) { c.transport.headers[name] = value }
}

// WithBackgroundRefill turns the background top-up on (the default) or off. Off, a reservation
// that leaves the slice low tops it up before it returns.
func WithBackgroundRefill(on bool) Option { return func(c *Client) { c.backgroundRefill = on } }

// WithBackgroundErrorHandler is called with an error a background top-up hit.
func WithBackgroundErrorHandler(f func(error)) Option {
	return func(c *Client) { c.onBackgroundError = f }
}

// WithClock replaces time.Now; for tests.
func WithClock(f func() time.Time) Option { return func(c *Client) { c.clock = f } }

// Client is the hot-path enforcement client. Safe for concurrent use.
type Client struct {
	store             Store
	transport         *transport
	clock             func() time.Time
	leaseSize         int64
	refillThreshold   int64
	policy            FailurePolicy
	reservationTTL    time.Duration
	graceUnits        int64
	graceWindow       time.Duration
	graceMeters       map[string]int64
	breakerCooldown   time.Duration
	snapshotRefresh   time.Duration
	entitlementsTTL   time.Duration
	lockTTL           time.Duration
	lockWait          time.Duration
	leaseTimeout      time.Duration
	prefix            string
	backgroundRefill  bool
	onBackgroundError func(error)
}

// New returns a client for token (an API token with the enforce scope: cbl_… live, cbt_… test)
// against baseURL, your deployment's API root.
func New(token, baseURL string, opts ...Option) (*Client, error) {
	if token == "" {
		return nil, errors.New("enforcement: an API token is required")
	}
	if baseURL == "" {
		return nil, errors.New("enforcement: a base URL is required")
	}
	c := &Client{
		transport:         &transport{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: http.DefaultClient, headers: map[string]string{}, timeout: 5 * time.Second},
		clock:             time.Now,
		leaseSize:         100,
		refillThreshold:   20,
		policy:            LastKnown,
		reservationTTL:    300 * time.Second,
		graceUnits:        100,
		graceWindow:       time.Hour,
		graceMeters:       map[string]int64{},
		breakerCooldown:   30 * time.Second,
		snapshotRefresh:   300 * time.Second,
		entitlementsTTL:   60 * time.Second,
		lockTTL:           10 * time.Second,
		lockWait:          5 * time.Second,
		leaseTimeout:      2 * time.Second,
		prefix:            "cbox-billing:",
		backgroundRefill:  true,
		onBackgroundError: func(error) {},
	}
	for _, opt := range opts {
		opt(c)
	}
	switch {
	case c.policy != LastKnown && c.policy != Allow && c.policy != Deny:
		return nil, fmt.Errorf("enforcement: unknown failure policy %q: use last_known, allow or deny", c.policy)
	case c.leaseSize <= 0, c.reservationTTL <= 0, c.graceWindow <= 0, c.snapshotRefresh <= 0,
		c.entitlementsTTL <= 0, c.lockTTL <= 0, c.leaseTimeout <= 0, c.transport.timeout <= 0:
		return nil, errors.New("enforcement: sizes, TTLs and timeouts must be positive")
	case c.refillThreshold < 0, c.graceUnits < 0, c.breakerCooldown < 0, c.lockWait < 0:
		return nil, errors.New("enforcement: thresholds, grace and cooldowns must be 0 or more")
	}
	if c.store == nil {
		c.store = NewMemoryStore(c.clock)
	}
	return c, nil
}

// ---- the hot path ----

// Reserve holds estimate units of meter: locally when the slice covers it, otherwise after one lease.
//
// It returns a *QuotaExceededError when Billing granted nothing (or, in an outage, the last known
// allowance is spent), a *BillingRefusedError when Billing refused the lease, a
// *BillingUnavailableError when Billing is unreachable and the policy refuses, and an error
// wrapping ErrInvalidArgument when estimate is not positive.
func (c *Client) Reserve(ctx context.Context, org, meter string, estimate int64) (*Reservation, error) {
	if estimate <= 0 {
		return nil, invalid("the estimate must be a positive integer")
	}
	hold, err := c.hold(ctx, org, meter, estimate)
	if err != nil {
		return nil, err
	}
	id := newID()
	if err := c.open(ctx, id, org, []Hold{hold}); err != nil {
		return nil, err
	}
	if hold.Kind == HoldLease {
		if err := c.refillIfLow(ctx, org, meter); err != nil {
			return nil, err
		}
	}
	return &Reservation{ID: id, Org: org, Hold: hold}, nil
}

// ReserveMany holds several meters at once, in key order, all or nothing: when one cannot be
// held, the others go back and it returns the failing meter's error.
func (c *Client) ReserveMany(ctx context.Context, org string, meters map[string]int64) (*ReservationSet, error) {
	if len(meters) == 0 {
		return nil, invalid("a reservation set needs at least one meter")
	}
	names := sortedKeys(meters)
	for _, name := range names {
		if meters[name] <= 0 {
			return nil, invalid("the estimate for [%s] must be a positive integer", name)
		}
	}
	held := make([]Hold, 0, len(names))
	for _, name := range names {
		hold, err := c.hold(ctx, org, name, meters[name])
		if err != nil {
			for _, h := range held {
				if gerr := c.giveBack(ctx, org, h, h.Amount); gerr != nil {
					return nil, errors.Join(err, gerr)
				}
			}
			return nil, err
		}
		held = append(held, hold)
	}
	id := newID()
	if err := c.open(ctx, id, org, held); err != nil {
		return nil, err
	}
	for _, h := range held {
		if h.Kind == HoldLease {
			if err := c.refillIfLow(ctx, org, h.Meter); err != nil {
				return nil, err
			}
		}
	}
	return &ReservationSet{ID: id, Org: org, Holds: held}, nil
}

// Actual is what a reservation used: Units for a single-meter Reservation, PerMeter (one entry
// for every meter) for a ReservationSet.
type Actual interface {
	actuals(holds []Hold, set bool) ([]int64, error)
}

// Units is the units a single-meter Reservation used.
type Units int64

// PerMeter is the units a ReservationSet used, one entry for every meter in it.
type PerMeter map[string]int64

func (u Units) actuals(holds []Hold, set bool) ([]int64, error) {
	if set {
		return nil, invalid("a reservation set is committed with PerMeter")
	}
	if int64(u) < 0 || int64(u) > holds[0].Amount {
		return nil, invalid("the actual for meter [%s] must be from 0 to %d", holds[0].Meter, holds[0].Amount)
	}
	return []int64{int64(u)}, nil
}

func (p PerMeter) actuals(holds []Hold, set bool) ([]int64, error) {
	if !set {
		return nil, invalid("a single-meter reservation is committed with Units")
	}
	in := map[string]bool{}
	for _, h := range holds {
		in[h.Meter] = true
	}
	for meter := range p {
		if !in[meter] {
			return nil, invalid("meter [%s] is not in this reservation", meter)
		}
	}
	used := make([]int64, len(holds))
	for i, h := range holds {
		a, ok := p[h.Meter]
		if !ok {
			return nil, invalid("missing the actual for meter [%s]", h.Meter)
		}
		if a < 0 || a > h.Amount {
			return nil, invalid("the actual for meter [%s] must be from 0 to %d", h.Meter, h.Amount)
		}
		used[i] = a
	}
	return used, nil
}

// Commit settles a reservation to what was used, each actual from 0 to what was held:
//
//	client.Commit(ctx, r, enforcement.Units(1))
//	client.Commit(ctx, set, enforcement.PerMeter{"api.requests": 1, "tokens": 812})
//
// The rest goes back; the usage is recorded for reporting. An actual out of range or missing, the
// wrong kind of Actual, or a reservation already settled is an error wrapping ErrInvalidArgument,
// and nothing moves.
func (c *Client) Commit(ctx context.Context, r Held, actual Actual) error {
	if r == nil || actual == nil {
		return invalid("commit needs a reservation and an actual")
	}
	id, org, holds := r.reservation()
	_, set := r.(*ReservationSet)
	used, err := actual.actuals(holds, set)
	if err != nil {
		return err
	}
	return c.settle(ctx, id, org, holds, used)
}

func (c *Client) settle(ctx context.Context, id, org string, holds []Hold, actuals []int64) error {
	closed, err := c.store.Delete(ctx, c.key("res", id))
	if err != nil {
		return err
	}
	if closed {
		for i, h := range holds {
			if err := c.giveBack(ctx, org, h, h.Amount-actuals[i]); err != nil {
				return err
			}
		}
	} else {
		swept, err := c.store.Delete(ctx, c.key("swept", id))
		if err != nil {
			return err
		}
		if !swept {
			return invalid("reservation [%s] is already settled", id)
		}
		// The sweeper gave every unit back; take out again what was used, as far as the slice allows.
		for i, h := range holds {
			if h.Kind == HoldLease && actuals[i] > 0 {
				if _, err := c.take(ctx, org, h.Meter, actuals[i]); err != nil {
					return err
				}
			}
		}
	}
	for i, h := range holds {
		if actuals[i] > 0 {
			if err := c.record(ctx, org, h.Meter, actuals[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Release gives a reservation's units back without recording usage. Nothing once settled, so it
// is safe to defer.
func (c *Client) Release(ctx context.Context, r Held) error {
	id, org, holds := r.reservation()
	closed, err := c.store.Delete(ctx, c.key("res", id))
	if err != nil {
		return err
	}
	if !closed {
		_, err := c.store.Delete(ctx, c.key("swept", id))
		return err
	}
	for _, h := range holds {
		if err := c.giveBack(ctx, org, h, h.Amount); err != nil {
			return err
		}
	}
	return nil
}

// Can reports whether n units could be reserved now. It refills once if short and takes nothing.
// A Billing failure is answered by the failure policy; only a store failure is an error.
func (c *Client) Can(ctx context.Context, org, meter string, n int64) (bool, error) {
	if n <= 0 {
		return false, nil
	}
	if b, err := c.Balance(ctx, org, meter); err != nil || b >= n {
		return err == nil, err
	}
	if _, err := c.refill(ctx, org, meter, n, false); err != nil {
		if isUnavailable(err) {
			return c.outageAdmits(ctx, org, meter, n)
		}
		if isBilling(err) {
			return false, nil
		}
		return false, err
	}
	b, err := c.Balance(ctx, org, meter)
	return b >= n, err
}

// Balance is the units in the local slice.
func (c *Client) Balance(ctx context.Context, org, meter string) (int64, error) {
	return c.counter(ctx, c.key("lease", org, meter))
}

// ---- reporting and upkeep ----

type usageEntry struct {
	Meter      string `json:"meter"`
	Cumulative int64  `json:"cumulative"`
	Seq        int64  `json:"seq"`
	Epoch      int64  `json:"epoch"`
}

// Report sends the buffered running totals: one request per organization (all, or just org when
// it is not empty), its meters in key order, leaving out streams unchanged since their last
// accepted report. It returns how many organizations were reported; one that fails is sent again
// next time.
func (c *Client) Report(ctx context.Context, org string) (int, error) {
	base := c.key("usage") + ":"
	prefix := base
	if org != "" {
		prefix += escapePart(org) + ":"
	}
	keys, err := c.store.Keys(ctx, prefix)
	if err != nil {
		return 0, err
	}
	streams := map[string][]string{}
	for _, key := range keys {
		parts := strings.Split(key[len(base):], ":")
		if len(parts) != 2 {
			continue
		}
		owner := unescapePart(parts[0])
		streams[owner] = append(streams[owner], unescapePart(parts[1]))
	}

	reported := 0
	for _, owner := range sortedKeys(streams) {
		meters := streams[owner]
		sort.Strings(meters)
		var entries []usageEntry
		for _, meter := range meters {
			// The sequence first: a commit raises the total before the sequence, so this total is
			// at least the one this sequence names, and a later commit changes it again.
			seq, err := c.counter(ctx, c.key("useq", owner, meter))
			if err != nil {
				return reported, err
			}
			cumulative, err := c.counter(ctx, c.key("usage", owner, meter))
			if err != nil {
				return reported, err
			}
			sent, ok, err := c.store.Get(ctx, c.key("usent", owner, meter))
			if err != nil {
				return reported, err
			}
			if ok && sent == strconv.FormatInt(seq, 10) {
				continue
			}
			epoch, err := c.counter(ctx, c.key("uepoch", owner, meter))
			if err != nil {
				return reported, err
			}
			entries = append(entries, usageEntry{Meter: meter, Cumulative: cumulative, Seq: seq, Epoch: epoch})
		}
		if len(entries) == 0 {
			continue
		}
		if _, err := c.transport.post(ctx, "/usage", map[string]any{"org": owner, "entries": entries}, 0); err != nil {
			if isBilling(err) {
				continue
			}
			return reported, err
		}
		for _, e := range entries {
			if err := c.store.Put(ctx, c.key("usent", owner, e.Meter), strconv.FormatInt(e.Seq, 10), 0); err != nil {
				return reported, err
			}
		}
		reported++
	}
	return reported, nil
}

type storedReservation struct {
	Org       string `json:"org"`
	ExpiresAt int64  `json:"expires_at"`
	Holds     []Hold `json:"holds"`
}

const sweptTTL = 24 * time.Hour

// Sweep closes every reservation older than the reservation TTL and gives its units back. It
// returns how many. Run it on a schedule, about as often as the TTL.
func (c *Client) Sweep(ctx context.Context) (int, error) {
	base := c.key("res") + ":"
	now := c.now()
	keys, err := c.store.Keys(ctx, base)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, key := range keys {
		raw, ok, err := c.store.Get(ctx, key)
		if err != nil {
			return swept, err
		}
		var stored storedReservation
		if !ok || json.Unmarshal([]byte(raw), &stored) != nil || stored.ExpiresAt > now {
			continue
		}
		closed, err := c.store.Delete(ctx, key)
		if err != nil || !closed {
			if err != nil {
				return swept, err
			}
			continue
		}
		if err := c.store.Put(ctx, c.key("swept", unescapePart(key[len(base):])), "1", sweptTTL); err != nil {
			return swept, err
		}
		for _, h := range stored.Holds {
			if err := c.giveBack(ctx, stored.Org, h, h.Amount); err != nil {
				return swept, err
			}
		}
		swept++
	}
	return swept, nil
}

// ---- entitlements and features ----

// Entitlements is the organization's per-meter entitlements, cached for the entitlements TTL.
// When Billing cannot be reached, the last answer, marked Stale.
func (c *Client) Entitlements(ctx context.Context, org string) (*Entitlements, error) {
	cached, err := c.readMeters(ctx, org)
	if err != nil {
		return nil, err
	}
	if cached != nil {
		if _, fresh, err := c.store.Get(ctx, c.key("entfresh", org)); err != nil || fresh {
			return &Entitlements{Org: org, Meters: cached}, err
		}
	}
	e, err := c.fetchEntitlements(ctx, org)
	if err != nil && isUnavailable(err) && cached != nil {
		return &Entitlements{Org: org, Meters: cached, Stale: true}, nil
	}
	return e, err
}

// Features is the organization's boolean and config features, cached like Entitlements.
func (c *Client) Features(ctx context.Context, org string) (*Features, error) {
	raw, haveCached, err := c.store.Get(ctx, c.key("feat", org))
	if err != nil {
		return nil, err
	}
	var cached *Features
	if haveCached {
		if body, derr := decode([]byte(raw)); derr == nil {
			if obj, ok := body.(map[string]any); ok {
				cached, _ = parseFeatures(org, obj, "")
			}
		}
	}
	if cached != nil {
		if _, fresh, err := c.store.Get(ctx, c.key("featfresh", org)); err != nil || fresh {
			return cached, err
		}
	}
	endpoint := "/entitlements/" + url.PathEscape(org) + "/features"
	body, err := c.transport.get(ctx, endpoint)
	var parsed *Features
	if err == nil {
		parsed, err = parseFeatures(org, body, endpoint)
	}
	if err != nil {
		if isUnavailable(err) && cached != nil {
			cached.Stale = true
			return cached, nil
		}
		return nil, err
	}
	store := map[string]any{"features": parsed.Features, "account": map[string]any{"suspended": parsed.Suspended}}
	encoded, _ := json.Marshal(store)
	if err := c.store.Put(ctx, c.key("feat", org), string(encoded), 0); err != nil {
		return nil, err
	}
	if err := c.store.Put(ctx, c.key("featfresh", org), "1", c.entitlementsTTL); err != nil {
		return nil, err
	}
	return parsed, nil
}

// HasFeature is true when the feature is granted and the account is not suspended.
func (c *Client) HasFeature(ctx context.Context, org, key string) (bool, error) {
	f, err := c.Features(ctx, org)
	if err != nil {
		return false, err
	}
	return f.Enabled(key), nil
}

// FeatureValue is a config feature's value, true for a granted boolean feature, nil otherwise.
func (c *Client) FeatureValue(ctx context.Context, org, key string) (any, error) {
	f, err := c.Features(ctx, org)
	if err != nil {
		return nil, err
	}
	return f.Value(key), nil
}

// Cost is units × weight for the meter, from the cached entitlements.
func (c *Client) Cost(ctx context.Context, org, meter string, units int64) (float64, error) {
	e, err := c.Entitlements(ctx, org)
	if err != nil {
		return 0, err
	}
	return e.Cost(meter, units), nil
}

// Invalidate makes the next read for org (everyone, when org is empty) ask Billing, and the next
// refill read the entitlements again. The old answer stays, as the fallback for an outage.
func (c *Client) Invalidate(ctx context.Context, org string) error {
	for _, kind := range []string{"entfresh", "featfresh", "claim"} {
		if org != "" {
			if _, err := c.store.Delete(ctx, c.key(kind, org)); err != nil {
				return err
			}
			continue
		}
		keys, err := c.store.Keys(ctx, c.key(kind)+":")
		if err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := c.store.Delete(ctx, k); err != nil {
				return err
			}
		}
	}
	return nil
}

var invalidating = map[string]bool{
	"entitlements.changed": true, "customer.suspended": true, "customer.reactivated": true,
	"customer.deleted": true, "customer.merged": true, "bundle.benefit_started": true,
	"bundle.benefit_ended": true, "agreement.accepted": true, "agreement.withdrawn": true,
}

var organizationFields = []string{"organization_ref", "organization_id", "cbox_org_id", "organization"}

// HandleWebhook invalidates from a verified webhook event (see cboxbilling.ConstructWebhookEvent), given
// its type and data. It reports whether the event type changes what a customer holds.
func (c *Client) HandleWebhook(ctx context.Context, eventType string, data map[string]any) (bool, error) {
	if eventType == "plan.changed" {
		return true, c.Invalidate(ctx, "")
	}
	if !invalidating[eventType] && !strings.HasPrefix(eventType, "subscription.") {
		return false, nil
	}
	seen := map[string]bool{}
	for _, field := range organizationFields {
		if org, ok := data[field].(string); ok && org != "" && !seen[org] {
			seen[org] = true
			if err := c.Invalidate(ctx, org); err != nil {
				return true, err
			}
		}
	}
	if len(seen) == 0 {
		return true, c.Invalidate(ctx, "")
	}
	return true, nil
}

// ---- holding units ----

func (c *Client) hold(ctx context.Context, org, meter string, estimate int64) (Hold, error) {
	if ok, err := c.take(ctx, org, meter, estimate); err != nil || ok {
		return Hold{Meter: meter, Amount: estimate, Kind: HoldLease}, err
	}
	answer, err := c.refill(ctx, org, meter, estimate, false)
	if err != nil {
		if isUnavailable(err) {
			return c.outageHold(ctx, org, meter, estimate, err)
		}
		return Hold{}, err
	}
	ok, err := c.take(ctx, org, meter, estimate)
	if err != nil {
		return Hold{}, err
	}
	if ok {
		return Hold{Meter: meter, Amount: estimate, Kind: HoldLease}, nil
	}
	reason := "quota_exhausted"
	if answer != nil && answer.reason != "" {
		reason = answer.reason
	}
	return Hold{}, &QuotaExceededError{Org: org, Meter: meter, Requested: estimate, Reason: reason}
}

func (c *Client) outageHold(ctx context.Context, org, meter string, estimate int64, outage error) (Hold, error) {
	switch c.policy {
	case Allow:
		return Hold{Meter: meter, Amount: estimate, Kind: HoldAllowed}, nil
	case Deny:
		return Hold{}, outage
	}
	known, err := c.lastKnown(ctx, org, meter)
	if err != nil {
		return Hold{}, err
	}
	if known != nil {
		ok, err := c.admit(ctx, org, meter, estimate, *known)
		if err != nil {
			return Hold{}, err
		}
		if ok {
			return Hold{Meter: meter, Amount: estimate, Kind: HoldAdmitted}, nil
		}
		if !known.Enabled {
			return Hold{}, &QuotaExceededError{Org: org, Meter: meter, Requested: estimate, Reason: "meter_disabled"}
		}
	}
	window, err := c.takeGrace(ctx, org, meter, estimate)
	if err != nil {
		return Hold{}, err
	}
	if window != nil {
		return Hold{Meter: meter, Amount: estimate, Kind: HoldGrace, GraceWindow: window}, nil
	}
	if known == nil {
		return Hold{}, outage
	}
	return Hold{}, &QuotaExceededError{Org: org, Meter: meter, Requested: estimate, Reason: "outage_limit"}
}

func (c *Client) outageAdmits(ctx context.Context, org, meter string, n int64) (bool, error) {
	if c.policy != LastKnown {
		return c.policy == Allow, nil
	}
	known, err := c.lastKnown(ctx, org, meter)
	if err != nil {
		return false, err
	}
	if known != nil {
		if !known.Enabled {
			return false, nil
		}
		if !known.capped() {
			return true, nil
		}
		room, err := c.headroom(ctx, org, meter, *known)
		if err != nil || room >= n {
			return err == nil, err
		}
	}
	left, err := c.graceRemaining(ctx, org, meter)
	return left >= n, err
}

// ---- refilling ----

type leaseAnswer struct {
	granted int64
	reason  string
}

// refill leases at least want units into the slice, single flight per (organization, meter). It
// returns Billing's answer, or nil when another refill already filled the slice.
func (c *Client) refill(ctx context.Context, org, meter string, want int64, probe bool) (*leaseAnswer, error) {
	if err := c.guard(ctx, probe); err != nil {
		return nil, err
	}
	lock := c.key("lock", org, meter)
	token := newID()
	deadline := time.Now().Add(c.lockWait)
	acquired, err := c.store.Add(ctx, lock, token, c.lockTTL)
	for err == nil && !acquired && !probe && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		acquired, err = c.store.Add(ctx, lock, token, c.lockTTL)
	}
	if err != nil {
		return nil, err
	}
	if !acquired {
		if probe {
			return nil, nil
		}
		// Could not join the single flight in time: refill directly rather than starve.
		if err := c.guard(ctx, probe); err != nil {
			return nil, err
		}
		return c.lease(ctx, org, meter, want)
	}
	defer func() {
		if v, ok, _ := c.store.Get(ctx, lock); ok && v == token {
			_, _ = c.store.Delete(ctx, lock)
		}
	}()
	if b, err := c.Balance(ctx, org, meter); err != nil || b >= want {
		return nil, err
	}
	if err := c.guard(ctx, probe); err != nil {
		return nil, err
	}
	return c.lease(ctx, org, meter, want)
}

func (c *Client) lease(ctx context.Context, org, meter string, want int64) (*leaseAnswer, error) {
	size := max(c.leaseSize, want)
	body, err := c.transport.post(ctx, "/leases", map[string]any{"org": org, "meter": meter, "size": size}, c.leaseTimeout)
	var answer *leaseAnswer
	if err == nil {
		answer, err = parseLease(body)
	}
	if err != nil {
		if isUnavailable(err) && c.breakerCooldown > 0 {
			if perr := c.store.Put(ctx, c.key("breaker"), "1", c.breakerCooldown); perr != nil {
				return nil, errors.Join(err, perr)
			}
		}
		return nil, err
	}
	if _, err := c.store.Delete(ctx, c.key("breaker")); err != nil {
		return nil, err
	}
	if answer.granted > 0 {
		if _, err := c.store.Increment(ctx, c.key("lease", org, meter), answer.granted); err != nil {
			return nil, err
		}
	}
	claimed, err := c.store.Add(ctx, c.key("claim", org), "1", c.snapshotRefresh)
	if err != nil {
		return nil, err
	}
	if claimed {
		// Keep the previous snapshot when this read fails; the next interval reads again.
		if _, err := c.fetchEntitlements(ctx, org); err != nil && !isBilling(err) {
			return nil, err
		}
	}
	return answer, c.recordLease(ctx, org, meter, size, answer.granted)
}

func (c *Client) refillIfLow(ctx context.Context, org, meter string) error {
	b, err := c.Balance(ctx, org, meter)
	if err != nil || b > c.refillThreshold {
		return err
	}
	if c.backgroundRefill {
		bg := context.WithoutCancel(ctx)
		go func() {
			if _, err := c.refill(bg, org, meter, c.refillThreshold+1, true); err != nil {
				c.onBackgroundError(err)
			}
		}()
		return nil
	}
	if _, err := c.refill(ctx, org, meter, c.refillThreshold+1, false); err != nil && !isBilling(err) {
		return err
	}
	return nil
}

func (c *Client) guard(ctx context.Context, probe bool) error {
	if probe || c.breakerCooldown <= 0 {
		return nil
	}
	_, open, err := c.store.Get(ctx, c.key("breaker"))
	if err != nil {
		return err
	}
	if open {
		return &BillingUnavailableError{Endpoint: "/leases", Message: "Billing was unreachable moments ago; the breaker is open"}
	}
	return nil
}

// ---- the last known allowance ----

func (c *Client) fetchEntitlements(ctx context.Context, org string) (*Entitlements, error) {
	endpoint := "/entitlements/" + url.PathEscape(org)
	body, err := c.transport.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	meters, err := parseMeters(body, endpoint)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(map[string]any{"meters": meters})
	if err := c.store.Put(ctx, c.key("ent", org), string(encoded), 0); err != nil {
		return nil, err
	}
	if err := c.store.Put(ctx, c.key("entfresh", org), "1", c.entitlementsTTL); err != nil {
		return nil, err
	}
	return &Entitlements{Org: org, Meters: meters}, nil
}

func (c *Client) readMeters(ctx context.Context, org string) (map[string]MeterEntitlement, error) {
	raw, ok, err := c.store.Get(ctx, c.key("ent", org))
	if err != nil || !ok {
		return nil, err
	}
	body, err := decode([]byte(raw))
	if err != nil {
		return nil, nil
	}
	obj, _ := body.(map[string]any)
	meters, err := parseMeters(obj, "")
	if err != nil {
		return nil, nil
	}
	return meters, nil
}

func (c *Client) lastKnown(ctx context.Context, org, meter string) (*MeterEntitlement, error) {
	meters, err := c.readMeters(ctx, org)
	if err != nil || meters == nil {
		return nil, err
	}
	if m, ok := meters[meter]; ok {
		return &m, nil
	}
	return nil, nil
}

// recordLease counts a lease Billing answered. A short grant on a capped meter means Billing has
// nothing left; a full grant past the allowance means a new period began, so only it counts.
func (c *Client) recordLease(ctx context.Context, org, meter string, requested, granted int64) error {
	known, err := c.lastKnown(ctx, org, meter)
	if err != nil {
		return err
	}
	drawn := c.key("drawn", org, meter)
	if known == nil || !known.capped() {
		if granted > 0 {
			_, err = c.store.Increment(ctx, drawn, granted)
		}
		return err
	}
	allowance := *known.Allowance
	if granted < requested {
		return c.store.Put(ctx, drawn, strconv.FormatInt(allowance, 10), 0)
	}
	after, err := c.store.Increment(ctx, drawn, granted)
	if err != nil || after <= allowance {
		return err
	}
	if err := c.store.Put(ctx, drawn, strconv.FormatInt(granted, 10), 0); err != nil {
		return err
	}
	return c.store.Put(ctx, c.key("admitted", org, meter), "0", 0)
}

func (c *Client) admit(ctx context.Context, org, meter string, units int64, known MeterEntitlement) (bool, error) {
	if !known.Enabled {
		return false, nil
	}
	key := c.key("admitted", org, meter)
	after, err := c.store.Increment(ctx, key, units)
	if err != nil || !known.capped() {
		return err == nil, err
	}
	drawn, err := c.counter(ctx, c.key("drawn", org, meter))
	if err != nil {
		return false, err
	}
	if *known.Allowance-drawn-after < 0 {
		_, err := c.store.Increment(ctx, key, -units)
		return false, err
	}
	return true, nil
}

func (c *Client) headroom(ctx context.Context, org, meter string, known MeterEntitlement) (int64, error) {
	if !known.capped() {
		return 0, nil
	}
	drawn, err := c.counter(ctx, c.key("drawn", org, meter))
	if err != nil {
		return 0, err
	}
	admitted, err := c.counter(ctx, c.key("admitted", org, meter))
	return max(0, *known.Allowance-drawn-admitted), err
}

// ---- outage grace ----

func (c *Client) graceLimit(meter string) int64 {
	limit := c.graceUnits
	if n, ok := c.graceMeters[meter]; ok {
		limit = n
	}
	return max(0, limit)
}

func (c *Client) currentWindow() int64 {
	return c.now() / c.graceWindow.Milliseconds()
}

func (c *Client) takeGrace(ctx context.Context, org, meter string, units int64) (*int64, error) {
	limit := c.graceLimit(meter)
	if units <= 0 || units > limit {
		return nil, nil
	}
	window := c.currentWindow()
	key := c.key("grace", org, meter, strconv.FormatInt(window, 10))
	if _, err := c.store.Add(ctx, key, "0", c.graceWindow); err != nil {
		return nil, err
	}
	used, err := c.store.Increment(ctx, key, units)
	if err != nil {
		return nil, err
	}
	if used > limit {
		_, err := c.store.Increment(ctx, key, -units)
		return nil, err
	}
	return &window, nil
}

func (c *Client) graceRemaining(ctx context.Context, org, meter string) (int64, error) {
	used, err := c.counter(ctx, c.key("grace", org, meter, strconv.FormatInt(c.currentWindow(), 10)))
	return max(0, c.graceLimit(meter)-used), err
}

// ---- store helpers ----

// giveBack returns units to where a hold took them from.
func (c *Client) giveBack(ctx context.Context, org string, h Hold, units int64) error {
	if units <= 0 {
		return nil
	}
	switch h.Kind {
	case HoldLease:
		_, err := c.store.Increment(ctx, c.key("lease", org, h.Meter), units)
		return err
	case HoldAdmitted:
		_, err := c.store.Increment(ctx, c.key("admitted", org, h.Meter), -units)
		return err
	case HoldGrace:
		// Only to the window it came from, and only while it exists.
		if h.GraceWindow == nil {
			return nil
		}
		key := c.key("grace", org, h.Meter, strconv.FormatInt(*h.GraceWindow, 10))
		if _, ok, err := c.store.Get(ctx, key); err != nil || !ok {
			return err
		}
		_, err := c.store.Increment(ctx, key, -units)
		return err
	}
	return nil
}

// take takes amount from the slice if it holds that much. Under concurrency it can only over-refuse.
func (c *Client) take(ctx context.Context, org, meter string, amount int64) (bool, error) {
	key := c.key("lease", org, meter)
	after, err := c.store.Increment(ctx, key, -amount)
	if err != nil {
		return false, err
	}
	if after < 0 {
		_, err := c.store.Increment(ctx, key, amount)
		return false, err
	}
	return true, nil
}

func (c *Client) record(ctx context.Context, org, meter string, units int64) error {
	if _, err := c.store.Add(ctx, c.key("uepoch", org, meter), strconv.FormatInt(c.now(), 10), 0); err != nil {
		return err
	}
	if _, err := c.store.Increment(ctx, c.key("usage", org, meter), units); err != nil {
		return err
	}
	_, err := c.store.Increment(ctx, c.key("useq", org, meter), 1)
	return err
}

func (c *Client) open(ctx context.Context, id, org string, holds []Hold) error {
	raw, _ := json.Marshal(storedReservation{Org: org, ExpiresAt: c.now() + c.reservationTTL.Milliseconds(), Holds: holds})
	return c.store.Put(ctx, c.key("res", id), string(raw), 0)
}

func (c *Client) counter(ctx context.Context, key string) (int64, error) {
	v, ok, err := c.store.Get(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, nil
	}
	return n, nil
}

func (c *Client) now() int64 { return c.clock().UnixMilli() }

func (c *Client) key(kind string, parts ...string) string {
	var b strings.Builder
	b.WriteString(c.prefix)
	b.WriteString(kind)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(escapePart(p))
	}
	return b.String()
}

// ---- parsing ----

func asInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

func parseLease(body map[string]any) (*leaseAnswer, error) {
	granted, ok := asInt(body["granted"])
	if !ok || granted < 0 {
		return nil, malformed("/leases", "missing an integer granted of at least 0")
	}
	if blocked, ok := body["blocked"].(map[string]any); ok {
		reason, ok := blocked["reason"].(string)
		if !ok {
			reason = "agreement_required"
		}
		return &leaseAnswer{granted: granted, reason: reason}, nil
	}
	reason, _ := body["reason"].(string)
	return &leaseAnswer{granted: granted, reason: reason}, nil
}

// emptyArrayAsObject reads PHP's `[]` for an empty map as an empty object.
func emptyArrayAsObject(v any) any {
	if list, ok := v.([]any); ok && len(list) == 0 {
		return map[string]any{}
	}
	return v
}

func parseMeters(body map[string]any, endpoint string) (map[string]MeterEntitlement, error) {
	raw, ok := emptyArrayAsObject(body["meters"]).(map[string]any)
	if !ok {
		return nil, malformed(endpoint, "meters must be an object")
	}
	meters := make(map[string]MeterEntitlement, len(raw))
	for meter, value := range raw {
		entry, ok := value.(map[string]any)
		enabled, isBool := entry["enabled"].(bool)
		if !ok || !isBool {
			return nil, malformed(endpoint, fmt.Sprintf("meter [%s] needs a boolean enabled", meter))
		}
		m := MeterEntitlement{Enabled: enabled, Overage: "block"}
		if a := entry["allowance"]; a != nil {
			n, ok := asInt(a)
			if !ok {
				return nil, malformed(endpoint, fmt.Sprintf("meter [%s] has a non-integer allowance", meter))
			}
			m.Allowance = &n
		}
		if w := entry["weight"]; w != nil {
			num, ok := w.(json.Number)
			f, err := num.Float64()
			if !ok || err != nil {
				return nil, malformed(endpoint, fmt.Sprintf("meter [%s] has a non-numeric weight", meter))
			}
			m.Weight = &f
		}
		if o, present := entry["overage"]; present && o != nil {
			s, ok := o.(string)
			if !ok {
				return nil, malformed(endpoint, fmt.Sprintf("meter [%s] has a non-string overage", meter))
			}
			m.Overage = s
		}
		meters[meter] = m
	}
	return meters, nil
}

func parseFeatures(org string, body map[string]any, endpoint string) (*Features, error) {
	raw, ok := emptyArrayAsObject(body["features"]).(map[string]any)
	account, aok := body["account"].(map[string]any)
	suspended, sok := account["suspended"].(bool)
	if !ok || !aok || !sok {
		return nil, malformed(endpoint, "expected features and account.suspended")
	}
	features := make(map[string]FeatureDecision, len(raw))
	for key, value := range raw {
		bad := malformed(endpoint, fmt.Sprintf("feature [%s] is not a feature decision", key))
		entry, ok := value.(map[string]any)
		enabled, isBool := entry["enabled"].(bool)
		if !ok || !isBool {
			return nil, bad
		}
		d := FeatureDecision{Enabled: enabled, Source: "default"}
		switch t := entry["type"].(type) {
		case nil:
		case string:
			if t != "boolean" && t != "config" {
				return nil, bad
			}
			d.Type = &t
		default:
			return nil, bad
		}
		switch v := entry["value"].(type) {
		case nil:
		case string:
			d.Value = v
		case json.Number:
			n, ok := asInt(v)
			if !ok {
				return nil, bad
			}
			d.Value = n
		default:
			return nil, bad
		}
		if s, present := entry["source"]; present && s != nil {
			str, ok := s.(string)
			if !ok {
				return nil, bad
			}
			d.Source = str
		}
		features[key] = d
	}
	return &Features{Org: org, Features: features, Suspended: suspended}, nil
}

// ---- small helpers ----

// escapePart escapes a key part's own % and :, so every key splits back into its parts.
func escapePart(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, "%", "%25"), ":", "%3A")
}

func unescapePart(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, "%3A", ":"), "%25", "%")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "rsv_" + hex.EncodeToString(b[:])
}

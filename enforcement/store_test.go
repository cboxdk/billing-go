package enforcement_test

// The store contract, run against the memory store and, when REDIS_URL is set (such as
// redis://127.0.0.1:6379/15), against a real Redis. Then many goroutines drain one leased slice
// through a shared store: exactly the leased units are admitted and committed, never more.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cboxdk/billing-go/enforcement"
)

// resp is a minimal Redis client for the tests, so the SDK needs no Redis dependency.
type resp struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

func dialRedis(t *testing.T) *resp {
	t.Helper()
	raw := os.Getenv("REDIS_URL")
	if raw == "" {
		t.Skip("REDIS_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	c := &resp{conn: conn, r: bufio.NewReader(conn)}
	t.Cleanup(func() { conn.Close() })
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		if _, err := c.do(context.Background(), "SELECT", db); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *resp) do(_ context.Context, args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, err
	}
	return c.read()
}

func (c *resp) read() (any, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, errors.New(line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		list := make([]any, n)
		for i := range list {
			if list[i], err = c.read(); err != nil {
				return nil, err
			}
		}
		return list, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

// testPrefix isolates one test run's keys in a shared Redis.
func testPrefix(t *testing.T) string {
	return fmt.Sprintf("cbox-billing-test:%s:%d:", t.Name(), time.Now().UnixNano())
}

func stores(t *testing.T) map[string]func(t *testing.T) (enforcement.Store, *atomic.Int64) {
	return map[string]func(t *testing.T) (enforcement.Store, *atomic.Int64){
		"memory": func(t *testing.T) (enforcement.Store, *atomic.Int64) {
			now := &atomic.Int64{}
			now.Store(start)
			return enforcement.NewMemoryStore(func() time.Time { return time.UnixMilli(now.Load()) }), now
		},
		"redis": func(t *testing.T) (enforcement.Store, *atomic.Int64) {
			return enforcement.NewRedisStore(dialRedis(t).do), nil
		},
	}
}

func TestStoreContract(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, clock := open(t)
			p := testPrefix(t)

			if _, ok, err := s.Get(ctx, p+"missing"); err != nil || ok {
				t.Fatalf("Get of a missing key = %v, %v", ok, err)
			}
			if n, err := s.Increment(ctx, p+"n", 5); err != nil || n != 5 {
				t.Fatalf("Increment from missing = %d, %v; want 5", n, err)
			}
			if n, _ := s.Increment(ctx, p+"n", -7); n != -2 {
				t.Fatalf("Increment = %d; want -2", n)
			}
			if ok, _ := s.Add(ctx, p+"a", "first", 0); !ok {
				t.Fatal("Add of a missing key refused")
			}
			if ok, _ := s.Add(ctx, p+"a", "second", 0); ok {
				t.Fatal("Add of an existing key accepted")
			}
			if v, _, _ := s.Get(ctx, p+"a"); v != "first" {
				t.Fatalf("Add overwrote: %q", v)
			}
			if err := s.Put(ctx, p+"a", "third", 0); err != nil {
				t.Fatal(err)
			}
			if v, _, _ := s.Get(ctx, p+"a"); v != "third" {
				t.Fatalf("Put did not overwrite: %q", v)
			}
			if ok, _ := s.Delete(ctx, p+"a"); !ok {
				t.Fatal("Delete of an existing key said it was missing")
			}
			if ok, _ := s.Delete(ctx, p+"a"); ok {
				t.Fatal("a second Delete claimed the key again")
			}

			// Keys matches the prefix literally, glob characters included.
			for _, k := range []string{"k:a", "k:b", "k*:c", "kx"} {
				_ = s.Put(ctx, p+k, "1", 0)
			}
			keys, err := s.Keys(ctx, p+"k:")
			if err != nil || strings.Join(keys, ",") != p+"k:a,"+p+"k:b" {
				t.Fatalf("Keys = %v, %v", keys, err)
			}
			if keys, _ := s.Keys(ctx, p+"k*"); len(keys) != 1 || keys[0] != p+"k*:c" {
				t.Fatalf("Keys with a glob character in the prefix = %v", keys)
			}

			// Expiry, and Increment keeps it.
			_ = s.Put(ctx, p+"ttl", "4", 200*time.Millisecond)
			if n, _ := s.Increment(ctx, p+"ttl", 1); n != 5 {
				t.Fatalf("Increment of a key with a ttl = %d", n)
			}
			if clock != nil {
				clock.Add(201)
			} else {
				time.Sleep(260 * time.Millisecond)
			}
			if _, ok, _ := s.Get(ctx, p+"ttl"); ok {
				t.Fatal("the key outlived its ttl; Increment dropped the expiry")
			}
			if ok, _ := s.Add(ctx, p+"ttl", "again", 0); !ok {
				t.Fatal("Add refused a key whose ttl had run out")
			}
		})
	}
}

// grantOnce answers the first lease with granted units and every later one with 0.
type grantOnce struct {
	mu      sync.Mutex
	granted int64
	leases  int
}

func (g *grantOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	body := `{"meters":{"api":{"enabled":true,"allowance":null,"overage":"block"}}}`
	if strings.HasSuffix(req.URL.Path, "/leases") {
		n := int64(0)
		if g.leases == 0 {
			n = g.granted
		}
		g.leases++
		body = fmt.Sprintf(`{"lease_id":"lease_%d","granted":%d,"expires_at":null}`, g.leases, n)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}, nil
}

func TestConcurrentWorkersNeverOverdrawTheSlice(t *testing.T) {
	const granted = 500
	const workers = 16
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, _ := open(t)
			billing := &grantOnce{granted: granted}
			prefix := testPrefix(t)
			client, err := enforcement.New("cbt_test", base,
				enforcement.WithStore(store),
				enforcement.WithPrefix(prefix),
				enforcement.WithHTTPClient(&http.Client{Transport: billing}),
				enforcement.WithLeaseSize(granted),
				enforcement.WithBackgroundRefill(false),
			)
			if err != nil {
				t.Fatal(err)
			}

			var admitted atomic.Int64
			var wg sync.WaitGroup
			errs := make(chan error, workers)
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; ; i++ {
						// Every third request holds 3 and gives the whole hold back; the rest use 1.
						estimate := int64(1)
						if (w+i)%3 == 0 {
							estimate = 3
						}
						r, err := client.Reserve(ctx, "org_a", "api", estimate)
						var q *enforcement.QuotaExceededError
						if errors.As(err, &q) {
							// A concurrent take can refuse for a moment; stop only once the slice is empty.
							if left, _ := client.Balance(ctx, "org_a", "api"); left > 0 {
								continue
							}
							return
						}
						if err != nil {
							errs <- err
							return
						}
						if estimate == 3 {
							err = client.Release(ctx, r)
						} else {
							admitted.Add(1)
							err = client.Commit(ctx, r, enforcement.Units(1))
						}
						if err != nil {
							errs <- err
							return
						}
					}
				}(w)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			if got := admitted.Load(); got != granted {
				t.Fatalf("committed %d units from a slice of %d", got, granted)
			}
			if left, _ := client.Balance(ctx, "org_a", "api"); left != 0 {
				t.Fatalf("the slice ended at %d; want 0", left)
			}
			if open, _ := store.Keys(ctx, prefix+"res:"); len(open) != 0 {
				t.Fatalf("%d reservations left open; every one was settled", len(open))
			}
			if usage, _, _ := store.Get(ctx, prefix+"usage:org_a:api"); usage != strconv.Itoa(granted) {
				t.Fatalf("the usage buffer holds %q; want %d", usage, granted)
			}
		})
	}
}

// grantUpTo grants every lease in full until limit units are granted in all (0: no limit), then
// what is left of it.
type grantUpTo struct {
	mu      sync.Mutex
	limit   int64
	granted int64
	leases  int
}

func (g *grantUpTo) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{"meters":{"api":{"enabled":true,"allowance":null,"overage":"block"}}}`
	if strings.HasSuffix(req.URL.Path, "/leases") {
		var lease struct{ Size int64 }
		if err := json.NewDecoder(req.Body).Decode(&lease); err != nil {
			return nil, err
		}
		g.mu.Lock()
		n := lease.Size
		if g.limit > 0 {
			n = min(n, g.limit-g.granted)
		}
		g.granted += n
		g.leases++
		body = fmt.Sprintf(`{"lease_id":"lease_%d","granted":%d,"expires_at":null}`, g.leases, n)
		g.mu.Unlock()
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}, nil
}

// Two clients on one store, many workers reserving from one small slice: a reservation is refused
// only when Billing has granted everything it had, never because another worker took the units a
// refill had just leased (#427).
func TestConcurrentReservationsAreRefusedOnlyWhenBillingIsSpent(t *testing.T) {
	const workers, reservations, estimate = 32, 1600, 10
	for name, open := range stores(t) {
		for _, limit := range []int64{0, 5000} {
			t.Run(fmt.Sprintf("%s/limit=%d", name, limit), func(t *testing.T) {
				ctx := context.Background()
				store, _ := open(t)
				billing := &grantUpTo{limit: limit}
				prefix := testPrefix(t)
				clients := make([]*enforcement.Client, 2)
				for i := range clients {
					c, err := enforcement.New("cbt_test", base,
						enforcement.WithStore(store),
						enforcement.WithPrefix(prefix),
						enforcement.WithHTTPClient(&http.Client{Transport: billing}),
						enforcement.WithLeaseSize(20),
						enforcement.WithBackgroundRefill(false),
					)
					if err != nil {
						t.Fatal(err)
					}
					clients[i] = c
				}

				var next, committed, refused atomic.Int64
				var wg sync.WaitGroup
				errs := make(chan error, workers)
				for w := 0; w < workers; w++ {
					wg.Add(1)
					go func(client *enforcement.Client) {
						defer wg.Done()
						for next.Add(1) <= reservations {
							r, err := client.Reserve(ctx, "org_a", "api", estimate)
							var q *enforcement.QuotaExceededError
							if errors.As(err, &q) {
								refused.Add(1)
								continue
							}
							if err == nil {
								err = client.Commit(ctx, r, enforcement.Units(estimate))
							}
							if err != nil {
								errs <- err
								return
							}
							committed.Add(estimate)
						}
					}(clients[w%2])
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					t.Fatal(err)
				}

				want := int64(reservations * estimate)
				if limit > 0 {
					want = limit
				}
				if got := committed.Load(); got != want {
					t.Fatalf("committed %d units with %d reservations refused; want %d", got, refused.Load(), want)
				}
				if got, wantRefused := refused.Load(), int64(reservations)-want/estimate; got != wantRefused {
					t.Fatalf("%d reservations refused; want %d", got, wantRefused)
				}
				if left, _ := clients[0].Balance(ctx, "org_a", "api"); left+want != billing.granted {
					t.Fatalf("Billing granted %d; %d committed and %d left in the slice", billing.granted, want, left)
				}
			})
		}
	}
}

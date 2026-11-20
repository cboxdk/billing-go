package enforcement

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store is where the client keeps its state: leased slices, open reservations, the usage buffer,
// the last known allowance, grace counters, the breaker, locks and the entitlement cache.
//
// Increment is atomic, starts a missing key at 0 and keeps the key's expiry. Add sets a key only
// when it is absent. Delete reports whether the key existed, so closing a reservation is an
// atomic claim. A ttl of 0 means no expiry.
type Store interface {
	Get(ctx context.Context, key string) (value string, ok bool, err error)
	Increment(ctx context.Context, key string, by int64) (int64, error)
	Add(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Put(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) (bool, error)
	Keys(ctx context.Context, prefix string) ([]string, error)
}

type memoryEntry struct {
	value     string
	expiresAt int64 // ms; 0 = never
}

// MemoryStore keeps state in this process's memory, safe for concurrent use. Right for one
// process; each process then has its own slices and its own usage stream per (organization,
// meter). Use a RedisStore to share them between processes.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
	clock   func() time.Time
}

// NewMemoryStore returns an empty store. clock may be nil (time.Now).
func NewMemoryStore(clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryStore{entries: map[string]memoryEntry{}, clock: clock}
}

func (s *MemoryStore) live(key string) (memoryEntry, bool) {
	e, ok := s.entries[key]
	if !ok {
		return e, false
	}
	if e.expiresAt != 0 && e.expiresAt <= s.clock().UnixMilli() {
		delete(s.entries, key)
		return e, false
	}
	return e, true
}

func (s *MemoryStore) expiry(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return s.clock().UnixMilli() + ttl.Milliseconds()
}

func (s *MemoryStore) Get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	return e.value, ok, nil
}

func (s *MemoryStore) Increment(_ context.Context, key string, by int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	var current int64
	if ok {
		n, err := strconv.ParseInt(e.value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("enforcement: the value at [%s] is not an integer", key)
		}
		current = n
	}
	next := current + by
	s.entries[key] = memoryEntry{value: strconv.FormatInt(next, 10), expiresAt: e.expiresAt}
	return next, nil
}

func (s *MemoryStore) Add(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live(key); ok {
		return false, nil
	}
	s.entries[key] = memoryEntry{value: value, expiresAt: s.expiry(ttl)}
	return true, nil
}

func (s *MemoryStore) Put(_ context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = memoryEntry{value: value, expiresAt: s.expiry(ttl)}
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.live(key)
	delete(s.entries, key)
	return ok, nil
}

func (s *MemoryStore) Keys(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.entries {
		if strings.HasPrefix(key, prefix) {
			if _, ok := s.live(key); ok {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// RedisCommand runs one raw Redis command. It must return (nil, nil) for a nil reply.
//
// With github.com/redis/go-redis:
//
//	func(ctx context.Context, args ...string) (any, error) {
//		a := make([]any, len(args))
//		for i, v := range args { a[i] = v }
//		reply, err := rdb.Do(ctx, a...).Result()
//		if errors.Is(err, redis.Nil) { return nil, nil }
//		return reply, err
//	}
type RedisCommand func(ctx context.Context, args ...string) (any, error)

// RedisStore keeps state in Redis, shared by every process and host using the same Redis and
// prefix. Billing accepts one writer per (organization, meter) usage stream, so processes that
// report the same meters must share one store.
type RedisStore struct{ command RedisCommand }

// NewRedisStore returns a store that runs its commands through command.
func NewRedisStore(command RedisCommand) *RedisStore { return &RedisStore{command: command} }

func redisExpiry(ttl time.Duration) []string {
	if ttl <= 0 {
		return nil
	}
	ms := ttl.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return []string{"PX", strconv.FormatInt(ms, 10)}
}

func redisText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case []byte:
		return string(t), true
	default:
		return fmt.Sprint(t), true
	}
}

func redisInt(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	default:
		s, _ := redisText(v)
		return strconv.ParseInt(s, 10, 64)
	}
}

func (s *RedisStore) Get(ctx context.Context, key string) (string, bool, error) {
	reply, err := s.command(ctx, "GET", key)
	if err != nil {
		return "", false, err
	}
	v, ok := redisText(reply)
	return v, ok, nil
}

func (s *RedisStore) Increment(ctx context.Context, key string, by int64) (int64, error) {
	reply, err := s.command(ctx, "INCRBY", key, strconv.FormatInt(by, 10))
	if err != nil {
		return 0, err
	}
	return redisInt(reply)
}

func (s *RedisStore) Add(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	reply, err := s.command(ctx, append([]string{"SET", key, value, "NX"}, redisExpiry(ttl)...)...)
	return reply != nil, err
}

func (s *RedisStore) Put(ctx context.Context, key, value string, ttl time.Duration) error {
	_, err := s.command(ctx, append([]string{"SET", key, value}, redisExpiry(ttl)...)...)
	return err
}

func (s *RedisStore) Delete(ctx context.Context, key string) (bool, error) {
	reply, err := s.command(ctx, "DEL", key)
	if err != nil {
		return false, err
	}
	n, err := redisInt(reply)
	return n > 0, err
}

func (s *RedisStore) Keys(ctx context.Context, prefix string) ([]string, error) {
	var b strings.Builder
	for _, r := range prefix {
		if strings.ContainsRune(`\*?[]`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	pattern := b.String() + "*"
	found := map[string]bool{}
	cursor := "0"
	for {
		reply, err := s.command(ctx, "SCAN", cursor, "MATCH", pattern, "COUNT", "500")
		if err != nil {
			return nil, err
		}
		parts, ok := reply.([]any)
		if !ok || len(parts) != 2 {
			return nil, fmt.Errorf("enforcement: unexpected SCAN reply %T", reply)
		}
		cursor, _ = redisText(parts[0])
		list, _ := parts[1].([]any)
		for _, k := range list {
			if key, ok := redisText(k); ok {
				found[key] = true
			}
		}
		if cursor == "0" || cursor == "" {
			break
		}
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

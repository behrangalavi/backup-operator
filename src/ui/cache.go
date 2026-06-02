package ui

import (
	"sync"
	"time"
)

// cache is a tiny TTL key→value store with stale-while-revalidate semantics.
//
// The UI may serve many hits per second while a `mc ls` over SFTP costs
// hundreds of ms — and an unreachable destination can pin every dashboard
// click on its 8 s probe timeout. We don't need invalidation — backup runs
// are minute-grained, stale-by-30s is fine. A single sync.Mutex is plenty
// for the request rates we expect from a cluster-internal dashboard.
type cache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	m       map[string]cacheEntry[V]
	loading map[string]bool // dedup concurrent background refreshes per key
	// sem bounds the total number of in-flight background refreshes across all
	// keys. Per-key dedup (loading) stops two goroutines probing the same
	// destination, but on a fleet with many distinct destinations an SSE tick
	// can still fan out one refresh per key simultaneously — each a fresh
	// SSH/TLS handshake. That connection storm is exactly what got the operator
	// IP-blocked by QNAP's bruteforce protection (see §18 ADR). nil = unbounded.
	sem chan struct{}
}

type cacheEntry[V any] struct {
	v   V
	exp time.Time
}

func newCache[V any](ttl time.Duration) *cache[V] {
	return &cache[V]{
		ttl:     ttl,
		m:       make(map[string]cacheEntry[V]),
		loading: make(map[string]bool),
	}
}

// getOrLoad returns a cached value or, on miss/expiry, calls load and stores
// the result. load is invoked outside the lock. Blocks the caller for the
// full load duration — use getOrRefreshAsync where blocking the request is
// not acceptable (UI render paths).
func (c *cache[V]) getOrLoad(key string, load func() (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()

	v, err := load()
	if err != nil {
		var zero V
		return zero, err
	}

	c.mu.Lock()
	c.m[key] = cacheEntry[V]{v: v, exp: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return v, nil
}

// getOrRefreshAsync returns the cached value immediately (zero-value if
// nothing is cached yet) and spawns a background refresh when the entry
// is missing or expired. Returns (value, hasFresh):
//
//   - hasFresh=true  → value is non-stale (within TTL), no refresh kicked
//   - hasFresh=false → value is stale or zero, refresh running in background
//
// Concurrent calls for the same key share a single refresh goroutine.
// onRefresh is invoked after a successful refresh completes — typically
// used to broadcast an SSE event so clients know fresh data is ready.
func (c *cache[V]) getOrRefreshAsync(key string, load func() (V, error), onRefresh func()) (V, bool) {
	c.mu.Lock()
	e, ok := c.m[key]
	fresh := ok && time.Now().Before(e.exp)
	if fresh || c.loading[key] {
		c.mu.Unlock()
		return e.v, fresh
	}
	c.loading[key] = true
	c.mu.Unlock()

	go func() {
		if c.sem != nil {
			c.sem <- struct{}{}
			defer func() { <-c.sem }()
		}
		v, err := load()
		c.mu.Lock()
		delete(c.loading, key)
		if err == nil {
			c.m[key] = cacheEntry[V]{v: v, exp: time.Now().Add(c.ttl)}
		}
		c.mu.Unlock()
		if err == nil && onRefresh != nil {
			onRefresh()
		}
	}()
	return e.v, false
}

// invalidate drops a key. Currently unused — kept for explicit refresh wiring later.
func (c *cache[V]) invalidate(key string) { //nolint:unused // reserved for future cache-invalidation wiring
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

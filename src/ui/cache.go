package ui

import (
	"sync"
	"time"

	"github.com/go-logr/logr"

	"backup-operator/internal/safe"
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

	// log + name identify this cache in panic-recovery logs. The background
	// refresh goroutine runs a caller-supplied load() closure that does
	// storage I/O and meta.json parsing — a panic there must not crash the
	// operator pod, so the goroutine recovers via safe.Goroutine. A zero
	// logr.Logger is a no-op sink (tests construct caches without one), so
	// recovery still happens even when log is unset.
	log  logr.Logger
	name string
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
	// Claim a refresh slot WITHOUT blocking, BEFORE spawning. Acquiring the
	// semaphore inside the goroutine (as this used to) meant every missed key
	// spawned a goroutine that then parked on the full channel — on a cold
	// cache over a large fleet (thousands of keys) that is a thousands-strong
	// goroutine + memory spike. Non-blocking acquire bounds the number of
	// spawned goroutines to the semaphore capacity; an over-cap tick returns
	// stale and retries on the next SSE tick (the stale-while-revalidate
	// contract already tolerates that). It also prevents a flood on one cache
	// from starving the other: a refused refresh doesn't queue, it just waits
	// for the next tick when a slot is likely free.
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			// slot acquired; released in the goroutine below
		default:
			c.mu.Unlock()
			return e.v, false // fresh is false here; skip-and-retry next tick
		}
	}
	c.loading[key] = true
	c.mu.Unlock()

	go func() {
		// Registered first so it runs last (LIFO): it recovers any panic
		// from load() after the loading flag and semaphore slot have been
		// released by the defers below. Without it a panic in the load()
		// closure crashes the whole operator pod.
		defer safe.Goroutine(c.log, "ui-cache-refresh", c.name+":"+key)
		// Always clear the loading flag, even on panic — otherwise a
		// recovered panic would pin this key as "loading" forever and it
		// would never refresh again (silent stale-forever).
		defer func() {
			c.mu.Lock()
			delete(c.loading, key)
			c.mu.Unlock()
		}()
		if c.sem != nil {
			defer func() { <-c.sem }()
		}
		v, err := load()
		if err != nil {
			return
		}
		c.mu.Lock()
		c.m[key] = cacheEntry[V]{v: v, exp: time.Now().Add(c.ttl)}
		c.mu.Unlock()
		if onRefresh != nil {
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

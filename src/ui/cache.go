package ui

import (
	"sync"
	"time"
)

// cache is a tiny TTL key→value store.
//
// The UI may serve many hits per second while a `mc ls` over SFTP costs
// hundreds of ms. We don't need invalidation — backup runs are minute-grained,
// stale-by-30s is fine. A single sync.Mutex is plenty for the request rates
// we expect from a cluster-internal dashboard.
type cache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]cacheEntry[V]
}

type cacheEntry[V any] struct {
	v   V
	exp time.Time
}

func newCache[V any](ttl time.Duration) *cache[V] {
	return &cache[V]{ttl: ttl, m: make(map[string]cacheEntry[V])}
}

// getOrLoad returns a cached value or, on miss/expiry, calls load and stores
// the result. load is invoked outside the lock.
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

// invalidate drops a key. Currently unused — kept for explicit refresh wiring later.
func (c *cache[V]) invalidate(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

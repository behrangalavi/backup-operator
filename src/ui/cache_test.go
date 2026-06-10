package ui

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// getOrRefreshAsync must never block the caller on the loader, even when
// the loader is slow. This is the property /api/jobs (estimateDuration)
// and the dashboard hot paths rely on: a stalled storage probe — e.g. an
// unreachable QNAP FTPS destination on its 30 s dial timeout — must not
// freeze the request and starve the browser's connection pool.
func TestGetOrRefreshAsync_DoesNotBlockOnSlowLoader(t *testing.T) {
	c := newCache[int](time.Minute)

	release := make(chan struct{})
	loaded := make(chan struct{})
	load := func() (int, error) {
		<-release // simulate a hung backend
		close(loaded)
		return 42, nil
	}

	start := time.Now()
	v, fresh := c.getOrRefreshAsync("k", load, nil)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("getOrRefreshAsync blocked for %v; must return immediately", elapsed)
	}
	if fresh {
		t.Fatalf("expected fresh=false on cold miss, got true")
	}
	if v != 0 {
		t.Fatalf("expected zero value on cold miss, got %d", v)
	}

	// Let the background refresh finish and confirm the value lands.
	close(release)
	select {
	case <-loaded:
	case <-time.After(2 * time.Second):
		t.Fatal("background loader never ran")
	}
	// Poll until the cached value is visible (the goroutine writes it
	// after load returns; there's a tiny window after `loaded` closes).
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := c.getOrRefreshAsync("k", load, nil)
		if ok && got == 42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cached value never became fresh: got=%d fresh=%v", got, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Concurrent callers for the same key must share a single background
// refresh — the loading guard prevents a probe storm that would otherwise
// open one stalled connection per render tick.
func TestGetOrRefreshAsync_DedupsConcurrentRefreshes(t *testing.T) {
	c := newCache[int](time.Minute)

	var calls int
	var mu sync.Mutex
	release := make(chan struct{})
	load := func() (int, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return 1, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.getOrRefreshAsync("k", load, nil)
		}()
	}
	wg.Wait()
	close(release)

	// Give the single in-flight goroutine time to finish.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 loader invocation, got %d", calls)
	}
}

// A panic in the load() closure must be recovered inside the background
// goroutine, not crash the operator pod — load() does storage I/O and
// meta.json parsing that can panic on malformed input. The key must also
// be freed from the loading set so subsequent refreshes still run.
func TestGetOrRefreshAsync_RecoversLoaderPanic(t *testing.T) {
	c := newCache[int](time.Minute)

	panicked := make(chan struct{})
	c.getOrRefreshAsync("k", func() (int, error) {
		defer close(panicked)
		panic("boom in load()")
	}, nil)

	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("loader never ran")
	}

	// The loading flag must be cleared even after a panic, so a later
	// refresh for the same key is not pinned forever. Poll until a fresh
	// (non-panicking) load lands its value — proves both that the process
	// survived and that the key is refreshable again.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := c.getOrRefreshAsync("k", func() (int, error) { return 5, nil }, nil)
		if ok && got == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key stuck after panic; never refreshed: got=%d fresh=%v", got, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// onRefresh fires only after a successful background load, so the SSE
// repaint that surfaces the freshly-loaded estimate isn't sent on error.
func TestGetOrRefreshAsync_OnRefreshOnlyOnSuccess(t *testing.T) {
	c := newCache[int](time.Minute)

	var refreshed int
	var mu sync.Mutex
	onRefresh := func() {
		mu.Lock()
		refreshed++
		mu.Unlock()
	}

	// First: a failing load must not invoke onRefresh and must not cache.
	c.getOrRefreshAsync("k", func() (int, error) {
		return 0, errors.New("boom")
	}, onRefresh)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if refreshed != 0 {
		mu.Unlock()
		t.Fatalf("onRefresh fired on failed load")
	}
	mu.Unlock()

	// Then: a successful load fires onRefresh exactly once.
	c.getOrRefreshAsync("k", func() (int, error) {
		return 7, nil
	}, onRefresh)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := refreshed
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("onRefresh did not fire once on success: got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

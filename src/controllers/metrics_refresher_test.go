package controllers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDestSlot_CapsConcurrentHolders proves the per-destination slot
// limits in-flight callers to defaultPerDestConcurrency, summed across
// every goroutine that hits the same destination name. This is the
// invariant the §18 ADR commits to: a destination shared by N sources
// in the global worker pool must still see at most that cap of
// concurrent calls. Without this, the only thing protecting a backend
// would be the global worker count, which is sized for CPU, not for
// the storage box's session limit.
func TestDestSlot_CapsConcurrentHolders(t *testing.T) {
	r := &MetricsRefresher{}

	const callers = 20
	var (
		inFlight int32
		peak     int32
		wg       sync.WaitGroup
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot := r.destSlot("hetzner-sb")
			slot <- struct{}{}
			now := atomic.AddInt32(&inFlight, 1)
			// Track the high-water mark of concurrent holders.
			for {
				old := atomic.LoadInt32(&peak)
				if now <= old || atomic.CompareAndSwapInt32(&peak, old, now) {
					break
				}
			}
			// Hold the slot long enough that other goroutines pile up
			// against the channel — without a hold, the test could pass
			// trivially if every caller completes before the next starts.
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			<-slot
		}()
	}
	wg.Wait()

	if peak > defaultPerDestConcurrency {
		t.Errorf("per-destination cap violated: peak in-flight = %d, cap = %d",
			peak, defaultPerDestConcurrency)
	}
	// And we should actually have hit the cap — otherwise the test was
	// not exercising the limit (e.g. timing flake) and would silently
	// pass through future regressions.
	if peak < defaultPerDestConcurrency {
		t.Errorf("test did not exercise the cap: peak = %d, want %d",
			peak, defaultPerDestConcurrency)
	}
}

// TestDestSlot_DistinctDestinationsDoNotShareCap covers the dual case:
// two different destinations must each get their own cap, otherwise
// we'd have re-introduced a global single-backend bottleneck.
func TestDestSlot_DistinctDestinationsDoNotShareCap(t *testing.T) {
	r := &MetricsRefresher{}

	slotA := r.destSlot("dest-a")
	slotB := r.destSlot("dest-b")

	if slotA == slotB {
		t.Fatal("distinct destination names returned the same channel — cap would be shared")
	}

	// Fill A to its cap; B should still admit.
	for i := 0; i < defaultPerDestConcurrency; i++ {
		slotA <- struct{}{}
	}
	select {
	case slotB <- struct{}{}:
		<-slotB
	case <-time.After(50 * time.Millisecond):
		t.Error("dest-b blocked while dest-a was full — caps must be independent")
	}
	for i := 0; i < defaultPerDestConcurrency; i++ {
		<-slotA
	}
}

// TestDestSlot_SameNameReturnsSameChannel proves the cap aggregates
// correctly across goroutines. Two callers asking for the same
// destination name must end up queueing against one channel — not get
// fresh channels and bypass the cap.
func TestDestSlot_SameNameReturnsSameChannel(t *testing.T) {
	r := &MetricsRefresher{}
	first := r.destSlot("hetzner-sb")
	second := r.destSlot("hetzner-sb")
	if first != second {
		t.Error("destSlot returned different channels for the same name — cap would not aggregate")
	}
}

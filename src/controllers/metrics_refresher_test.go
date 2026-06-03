package controllers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backup-operator/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

// hasSeries reports whether a series with the given name and label subset
// currently exists in reg.
func hasSeries(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) bool {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// TestReconcileDestinations_DropsVanishedSeries proves a destination removed
// from a source's allow-list has its per-(target,destination) series deleted
// on the next tick, while a surviving destination is left intact.
func TestReconcileDestinations_DropsVanishedSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.Register(reg)
	r := &MetricsRefresher{}

	// Tick 1: both destinations present and reporting.
	metrics.SetRetentionLastStatus("recon-t", "keep", true)
	metrics.SetRetentionLastStatus("recon-t", "drop", false)
	r.reconcileDestinations("recon-t", map[string]struct{}{"keep": {}, "drop": {}})

	// Tick 2: "drop" left the allow-list.
	r.reconcileDestinations("recon-t", map[string]struct{}{"keep": {}})

	if hasSeries(t, reg, "backup_operator_retention_last_status",
		map[string]string{"target": "recon-t", "destination": "drop"}) {
		t.Error("retention_last_status{drop} should be deleted after destination left allow-list")
	}
	if !hasSeries(t, reg, "backup_operator_retention_last_status",
		map[string]string{"target": "recon-t", "destination": "keep"}) {
		t.Error("retention_last_status{keep} should survive")
	}
}

// TestReconcileTables_DropsVanishedSeries proves a table dropped from the
// schema has its row-count series deleted on the next tick with stats.
func TestReconcileTables_DropsVanishedSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.Register(reg)
	r := &MetricsRefresher{}

	metrics.SetTableRowCount("recon-tbl", "keep", 10)
	metrics.SetTableRowCount("recon-tbl", "drop", 20)
	r.reconcileTables("recon-tbl", map[string]struct{}{"keep": {}, "drop": {}})

	r.reconcileTables("recon-tbl", map[string]struct{}{"keep": {}})

	if hasSeries(t, reg, "backup_operator_table_row_count",
		map[string]string{"target": "recon-tbl", "table": "drop"}) {
		t.Error("table_row_count{drop} should be deleted after table dropped from schema")
	}
	if !hasSeries(t, reg, "backup_operator_table_row_count",
		map[string]string{"target": "recon-tbl", "table": "keep"}) {
		t.Error("table_row_count{keep} should survive")
	}
}

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

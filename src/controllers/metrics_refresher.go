package controllers

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultGlobalConcurrency caps the number of sources processed
	// simultaneously per refresh tick. This is the operator pod's
	// CPU/goroutine budget, not a backend-protection knob.
	defaultGlobalConcurrency = 8

	// defaultPerDestConcurrency caps in-flight calls against a SINGLE
	// destination, summed across every source that fans out to it.
	// This is the actual backend-protection knob — a Hetzner Storage
	// Box accepts ~10 concurrent SSH sessions; staying at 4 leaves
	// headroom for worker pods running uploads in parallel.
	defaultPerDestConcurrency = 4

	// fullRefreshInterval forces a complete storage walk even when the
	// Secret list ResourceVersion hasn't changed. Needed to pick up new
	// meta.json files produced by CronJob runs (which don't touch the
	// Secrets). Between forced refreshes, unchanged RVs let us skip the
	// expensive storage calls entirely.
	fullRefreshInterval = 5 * time.Minute
)

// MetricsRefresher periodically rebuilds the operator's Prometheus gauges from
// the latest meta.json sidecar found at each destination. Worker pods are
// short-lived so Prometheus cannot scrape them in time; this aggregator is the
// long-lived process that turns "what storage knows" back into live metrics.
type MetricsRefresher struct {
	Client    client.Client
	Logger    logr.Logger
	Namespace string
	Interval  time.Duration

	// Pool is the per-destination storage cache. Optional — left nil the
	// refresher lazy-builds its own pool on first refresh, but production
	// wiring shares one pool with the StorageScrubber via main.go to keep
	// a single client lifecycle per backend on the leader pod.
	Pool     *StoragePool
	poolOnce sync.Once

	// Broadcast is called when a per-target latest-meta timestamp
	// changes between two refresh ticks — i.e. a new backup run has
	// landed on a destination. Wired from main.go to ui.Server.Broadcast
	// so the UI dashboard repaints in near-real-time without polling.
	// Optional: nil disables the SSE side effect (the refresher still
	// updates Prometheus gauges).
	Broadcast func(eventType, data string)

	// lastSeenTimestamp caches the newest meta.json timestamp seen per
	// target on the previous tick. The refresher fires a broadcast when
	// the new tick finds a strictly newer timestamp, so we never
	// re-emit for unchanged data (which would flood the SSE channel).
	tsMu            sync.Mutex
	lastSeenTimestamp map[string]string

	// trackedTargets remembers which targets we exposed last refresh, so we
	// can drop their series when a Source Secret disappears. Without this,
	// a deleted source would leave stale metrics around indefinitely.
	//
	// trackedSeries remembers, per target, the destination and table label
	// sets we exposed on the previous tick. When a destination leaves a
	// source's allow-list or a table is dropped from the schema, we delete
	// the now-orphaned series rather than leaving it stuck at its last value.
	// Both maps share mu.
	mu             sync.Mutex
	trackedTargets map[string]bool
	trackedSeries  map[string]*targetSeries

	// secretToTarget remembers the target name a source Secret last parsed to,
	// keyed by namespace/name. When a still-present Secret fails to parse this
	// tick (a transient edit — host cleared, port typo), we keep its last-known
	// target in `current` so its series stay sticky rather than being swept as
	// "source removed". A genuinely removed source drops out of the Secret list
	// entirely, so its entry is pruned and its series correctly disappear.
	secretToTarget map[string]string

	// lastSrcRV / lastDestRV track the ResourceVersion of the most recent
	// Secret list responses. When neither has changed since the previous
	// tick AND the full refresh interval hasn't elapsed, the refresher
	// skips the expensive storage walk.
	lastSrcRV       string
	lastDestRV      string
	lastFullRefresh time.Time

	// perDestSlots is the per-destination semaphore map shared across
	// every source's goroutines. A destination shared by 50 sources sees
	// at most defaultPerDestConcurrency calls at once, regardless of
	// how the global worker pool schedules the sources. Lazy-initialised
	// in destSlot to keep zero-value MetricsRefresher usable in tests.
	destMu       sync.Mutex
	perDestSlots map[string]chan struct{}
}

// targetSeries records the per-label series a single target currently
// exposes, so the refresher can diff against the previous tick and delete
// series for destinations/tables that have since vanished.
type targetSeries struct {
	destinations map[string]struct{}
	tables       map[string]struct{}
}

// ensureSeries returns the tracked series for target, creating it if absent.
// Caller must hold r.mu.
func (r *MetricsRefresher) ensureSeries(target string) *targetSeries {
	if r.trackedSeries == nil {
		r.trackedSeries = map[string]*targetSeries{}
	}
	ts := r.trackedSeries[target]
	if ts == nil {
		ts = &targetSeries{destinations: map[string]struct{}{}, tables: map[string]struct{}{}}
		r.trackedSeries[target] = ts
	}
	return ts
}

// reconcileDestinations deletes the per-(target,destination) series for any
// destination tracked last tick but absent from cur (the source's current
// allow-list), then records cur as the new tracked set.
func (r *MetricsRefresher) reconcileDestinations(target string, cur map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.ensureSeries(target)
	for d := range ts.destinations {
		if _, ok := cur[d]; !ok {
			metrics.DeleteDestinationMetrics(target, d)
		}
	}
	ts.destinations = cur
}

// reconcileTables deletes the per-(target,table) row-count series for any
// table tracked last tick but absent from cur (the current schema), then
// records cur as the new tracked set. Only called when authoritative stats
// exist this tick — a failed run leaves the prior set sticky.
func (r *MetricsRefresher) reconcileTables(target string, cur map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.ensureSeries(target)
	for t := range ts.tables {
		if _, ok := cur[t]; !ok {
			metrics.DeleteTableMetric(target, t)
		}
	}
	ts.tables = cur
}

// destSlot returns the (lazy-initialised) per-destination semaphore
// for name. Same chan is handed out to every caller for the same
// destination so the cap is global, not per-source.
func (r *MetricsRefresher) destSlot(name string) chan struct{} {
	r.destMu.Lock()
	defer r.destMu.Unlock()
	if r.perDestSlots == nil {
		r.perDestSlots = map[string]chan struct{}{}
	}
	slot, ok := r.perDestSlots[name]
	if !ok {
		slot = make(chan struct{}, defaultPerDestConcurrency)
		r.perDestSlots[name] = slot
	}
	return slot
}

// retainDestSlots prunes per-destination semaphore entries for destinations
// that no longer exist, mirroring Pool.Retain. Without it the map grows
// unbounded under destination churn — every distinct name ever seen leaks one
// channel + map entry for the operator's lifetime. Safe to delete mid-tick:
// the refresh only fans out to destinations present in `dests`, so a name
// absent here has no in-flight goroutine holding its slot, and deleting the
// map entry never closes the channel a live goroutine may still own.
func (r *MetricsRefresher) retainDestSlots(dests []*secrets.Destination) {
	keep := make(map[string]struct{}, len(dests))
	for _, d := range dests {
		keep[d.Name] = struct{}{}
	}
	r.destMu.Lock()
	defer r.destMu.Unlock()
	for name := range r.perDestSlots {
		if _, ok := keep[name]; !ok {
			delete(r.perDestSlots, name)
		}
	}
}

// Start runs the refresh loop until ctx is cancelled. It satisfies
// manager.Runnable so the controller-runtime Manager owns its lifecycle.
func (r *MetricsRefresher) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 30 * time.Second
	}
	r.Logger.Info("starting metrics refresher", "interval", r.Interval, "namespace", r.Namespace)
	r.refresh(ctx)

	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.refresh(ctx)
		}
	}
}

// NeedLeaderElection ensures only the lead operator pulls from storage, so
// replicas don't multiply the read load against destinations.
func (r *MetricsRefresher) NeedLeaderElection() bool { return true }

// ensurePool lazy-builds the pool exactly once, race-free. A pool wired
// externally (main.go) is left untouched.
func (r *MetricsRefresher) ensurePool() {
	r.poolOnce.Do(func() {
		if r.Pool == nil {
			r.Pool = NewStoragePool(r.Logger)
		}
	})
}

func (r *MetricsRefresher) refresh(ctx context.Context) {
	r.ensurePool()
	res, err := listBackupSecrets(ctx, r.Client, r.Namespace, r.Logger)
	if err != nil {
		r.Logger.Error(err, "list secrets")
		return
	}

	// Fast path: when neither the source nor destination Secret list has
	// changed since the last tick (same ResourceVersion) AND the full
	// refresh interval hasn't elapsed, skip the expensive storage walk.
	// This reduces ~150 storage API calls to two cheap K8s list calls on
	// idle clusters while still picking up new CronJob runs every 5 minutes.
	now := time.Now()
	srcRV, destRV := res.SrcRV, res.DestRV
	rvUnchanged := srcRV != "" && srcRV == r.lastSrcRV && destRV == r.lastDestRV
	forceRefresh := r.lastFullRefresh.IsZero() || now.Sub(r.lastFullRefresh) >= fullRefreshInterval
	if rvUnchanged && !forceRefresh {
		r.Logger.V(2).Info("refresh skipped: no secret changes", "srcRV", srcRV, "destRV", destRV)
		return
	}
	r.lastSrcRV = srcRV
	r.lastDestRV = destRV
	r.lastFullRefresh = now

	sources, dests := res.Sources, res.Dests
	r.Pool.Retain(dests)
	r.retainDestSlots(dests)
	r.Logger.V(1).Info("refresh tick", "sources", len(sources), "destinations", len(dests), "pooled_clients", r.Pool.Size())

	// Sources flow through a fixed-size global worker pool. The
	// per-destination semaphore (see refreshSource) caps backend load
	// independently — these two limits do different jobs and must not
	// be conflated.
	var (
		currentMu sync.Mutex
		current   = make(map[string]bool, len(sources))
		wg        sync.WaitGroup
		workers   = make(chan struct{}, defaultGlobalConcurrency)
	)
	for i := range sources {
		s := sources[i] // capture by value — defensive even on Go 1.22+ semantics
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Goroutine(r.Logger, "metrics-refresh-source", s.Name)
			// Acquire a worker slot but bail promptly on shutdown instead of
			// blocking until a peer frees one (matches the per-dest slot
			// acquire below). Only release if we actually acquired.
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}

			src, err := secrets.ParseSource(&s, "")
			if err != nil {
				metrics.IncSourceParseError(s.Name)
				// Keep this target's series sticky if we've seen it parse before:
				// a transient parse error must not make its monitoring go dark
				// (BackupOverdue can never fire on an absent series).
				r.mu.Lock()
				prevTarget, known := r.secretToTarget[secretKey(&s)]
				r.mu.Unlock()
				if known {
					currentMu.Lock()
					current[prevTarget] = true
					currentMu.Unlock()
				}
				r.Logger.V(1).Info("skipping invalid source; keeping last-known metrics", "secret", s.Name, "err", err.Error())
				return
			}
			currentMu.Lock()
			current[src.TargetName] = true
			currentMu.Unlock()
			r.mu.Lock()
			if r.secretToTarget == nil {
				r.secretToTarget = map[string]string{}
			}
			r.secretToTarget[secretKey(&s)] = src.TargetName
			r.mu.Unlock()
			r.refreshSource(ctx, src, dests)
		}()
	}
	wg.Wait()

	// Secret keys present this tick — used to prune secretToTarget for sources
	// that genuinely disappeared (vs. merely failed to parse, which keeps them).
	presentSecrets := make(map[string]bool, len(sources))
	for i := range sources {
		presentSecrets[secretKey(&sources[i])] = true
	}

	r.mu.Lock()
	for prev := range r.trackedTargets {
		if !current[prev] {
			metrics.DeleteTargetMetrics(prev)
			delete(r.trackedSeries, prev)
		}
	}
	for sk := range r.secretToTarget {
		if !presentSecrets[sk] {
			delete(r.secretToTarget, sk)
		}
	}
	r.trackedTargets = current
	r.mu.Unlock()

	// Prune the broadcast-dedup cache for targets that no longer exist, so it
	// doesn't leak one entry per ever-seen target over the process lifetime.
	r.tsMu.Lock()
	for tgt := range r.lastSeenTimestamp {
		if !current[tgt] {
			delete(r.lastSeenTimestamp, tgt)
		}
	}
	r.tsMu.Unlock()
}

// secretKey identifies a Secret across ticks for the secretToTarget map.
func secretKey(s *corev1.Secret) string {
	return s.Namespace + "/" + s.Name
}

func (r *MetricsRefresher) refreshSource(ctx context.Context, src *secrets.Source, all []*secrets.Destination) {
	allowed := secrets.FilterDestinations(src, all)

	// Reconcile the destination label-set up front (independent of whether
	// any run data exists yet): a destination dropped from the allow-list
	// should have its series deleted even if the remaining destinations are
	// currently empty.
	curDests := make(map[string]struct{}, len(allowed))
	for _, d := range allowed {
		curDests[d.Name] = struct{}{}
	}
	r.reconcileDestinations(src.TargetName, curDests)

	// We track two independent "best" metas across destinations:
	//   - newest:    dictates last_run_status / last_run_anomalies / size_change_ratio
	//                even if it represents a failed run
	//   - success:   dictates dump_size, table_count, last_success_timestamp —
	//                fields that only make sense when a real artifact exists
	var (
		newest, success        *meta.MetaFile
		newestTS, successTS    time.Time
		resultMu               sync.Mutex
		wg                     sync.WaitGroup
	)
	for _, d := range allowed {
		// Acquire the per-destination slot BEFORE spawning the goroutine.
		// The slot is shared across every source fanning out to this
		// backend, so the backend sees at most defaultPerDestConcurrency
		// in-flight calls. Acquiring before `go` means a slow/down backend
		// blocks the (already globally-bounded) source goroutine here
		// instead of accumulating one blocked goroutine per source×dest —
		// the unbounded-queue growth the previous in-goroutine acquire had.
		slot := r.destSlot(d.Name)
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			// Shutdown mid-fan-out: wait for the destination goroutines already
			// spawned to finish rather than detaching them (they hold slots and
			// write metrics). Skip the aggregation below — the tick is aborting.
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(r.Logger, "metrics-refresh-destination", d.Name)
			defer func() { <-slot }()

			st, err := r.Pool.Get(d)
			if err != nil {
				r.Logger.V(1).Info("storage init failed; treating destination as failing",
					"target", src.TargetName, "destination", d.Name, "err", err.Error())
				metrics.SetDestinationFailed(src.TargetName, d.Name, true)
				return
			}
			m, ts, outcome := loadLatestMeta(ctx, st, src.TargetName)
			switch outcome {
			case metaError:
				// Storage unreachable / unreadable (offline host, rotated
				// credentials, corrupt payload). Raise the gauge so
				// BackupDestinationFailing can fire — this is the failure mode
				// the alert is documented to catch. Pool.Get succeeding only
				// builds the client; the connect/auth happens here.
				metrics.SetDestinationFailed(src.TargetName, d.Name, true)
				return
			case metaEmpty:
				// Reachable but nothing uploaded yet (legitimate first run) —
				// explicitly not failed, and no run data to fold in.
				metrics.SetDestinationFailed(src.TargetName, d.Name, false)
				return
			}
			metrics.SetDestinationFailed(src.TargetName, d.Name, false)
			if !m.IsFailure() {
				metrics.SetLastSuccess(src.TargetName, d.Name, ts)
			}

			resultMu.Lock()
			if newest == nil || ts.After(newestTS) {
				newest = m
				newestTS = ts
			}
			if !m.IsFailure() && (success == nil || ts.After(successTS)) {
				success = m
				successTS = ts
			}
			resultMu.Unlock()
		}(d)
	}
	wg.Wait()

	if newest == nil {
		// No data anywhere yet — leave gauges absent. lastRunStatus only
		// becomes meaningful once at least one run has uploaded a meta.
		return
	}

	// Detect "new data" — newest.Timestamp differs from what we recorded
	// last tick — and push an SSE event so the live UI repaints. This is
	// what turns the dashboard into a live stream: a backup that lands
	// on a destination shows up within one refresh tick (default 30 s)
	// instead of waiting for the user to click refresh.
	r.tsMu.Lock()
	if r.lastSeenTimestamp == nil {
		r.lastSeenTimestamp = make(map[string]string)
	}
	prevTS, hadPrev := r.lastSeenTimestamp[src.TargetName]
	r.lastSeenTimestamp[src.TargetName] = newest.Timestamp
	r.tsMu.Unlock()
	if r.Broadcast != nil && hadPrev && prevTS != newest.Timestamp {
		r.Broadcast("meta_changed", src.TargetName)
	}

	metrics.SetLastRunStatus(src.TargetName, !newest.IsFailure())
	if newest.Report != nil {
		if newest.Report.SizeChangeRatio > 0 {
			metrics.SetDumpSizeChangeRatio(src.TargetName, newest.Report.SizeChangeRatio)
		}
		metrics.SetSchemaChanged(src.TargetName, newest.Report.SchemaChanged)
		metrics.SetCharsetChanged(src.TargetName, newest.Report.CharsetChanged)
		metrics.SetLastRunAnomalies(src.TargetName, len(newest.Report.Anomalies))
	}
	// A failed run won't have a report. Leave last_run_anomalies (and
	// schema/charset/size, above) sticky on their last known values rather than
	// zeroing them — a transient failure must not resolve BackupAnomaliesAppearing
	// exactly when something else is also breaking. (Previously this branch
	// zeroed anomalies, contradicting its own "keep sticky" comment.)

	if success != nil {
		metrics.SetDumpSize(src.TargetName, success.EncryptedSizeBytes)
		if !success.SchemaChangedAt.IsZero() {
			metrics.SetSchemaLastChange(src.TargetName, success.SchemaChangedAt)
		}
		if success.Stats != nil {
			metrics.SetTableCount(src.TargetName, len(success.Stats.Tables))
			curTables := make(map[string]struct{}, len(success.Stats.Tables))
			for _, t := range success.Stats.Tables {
				metrics.SetTableRowCount(src.TargetName, t.Name, t.RowCount)
				curTables[t.Name] = struct{}{}
			}
			r.reconcileTables(src.TargetName, curTables)
		}
		// Failed runs typically fail fast and would systematically
		// underestimate the gauge, so it tracks the latest *successful* run
		// only — same rationale as DumpSize and the UI's MedianDuration.
		if success.DurationSeconds > 0 {
			metrics.SetLastRunDuration(src.TargetName, success.DBType,
				time.Duration(success.DurationSeconds*float64(time.Second)))
		}
		if success.DumpDurationSeconds > 0 {
			metrics.SetLastDumpDuration(src.TargetName, success.DBType,
				time.Duration(success.DumpDurationSeconds*float64(time.Second)))
		}
		// Retention status per destination, captured during the pre-upload
		// sweep. Every destination's latest meta carries the same Retention
		// block (the sweep runs once across all destinations), so reading
		// from the success pivot is sufficient. Skip when the run had
		// retention disabled (Days=0) or no Retention block existed yet
		// (legacy meta) — leaving the gauge absent is meaningful.
		for _, rr := range success.Retention {
			metrics.SetRetentionLastStatus(src.TargetName, rr.Name, rr.Status == meta.StatusSuccess)
			metrics.SetRetentionLastDeleted(src.TargetName, rr.Name, rr.DeletedDumps)
		}
	}

	// Restore-verification: surfaced from the latest meta that carries a
	// result (success or newest, doesn't matter — verification ran at
	// some point and we want operators to see it). Only set when present
	// to avoid creating empty mode-labelled series for sources that have
	// verification disabled.
	rvSource := newest
	if success != nil && success.RestoreVerification != nil {
		// Prefer the success path so a transient failed run doesn't drop
		// the verification gauge.
		rvSource = success
	}
	if rvSource != nil && rvSource.RestoreVerification != nil {
		rv := rvSource.RestoreVerification
		passed := rv.Verdict == meta.VerificationMatch
		metrics.SetRestoreVerificationPassed(src.TargetName, rv.Mode, passed)
		if !rv.CompletedAt.IsZero() {
			metrics.SetRestoreVerificationLastTimestamp(src.TargetName, rv.Mode, rv.CompletedAt)
		}
	}
}

// metaOutcome distinguishes the three states a meta lookup can end in, which
// the boolean "found" used to conflate. The distinction is load-bearing for
// destination_failed: a storage I/O error (unreachable host, rotated
// credentials, unreadable/corrupt payload) must raise the gauge so
// BackupDestinationFailing can fire, whereas a reachable-but-empty destination
// (no run uploaded yet) must NOT — that is the legitimate first-run case.
type metaOutcome int

const (
	metaOK    metaOutcome = iota // meta found, read, and parsed
	metaEmpty                    // storage reachable, but no parseable meta present
	metaError                    // storage could not be reached / read
)

func (o metaOutcome) String() string {
	switch o {
	case metaOK:
		return "ok"
	case metaEmpty:
		return "empty"
	case metaError:
		return "error"
	}
	return "unknown"
}

// loadLatestMeta fetches and parses the most recent *.meta.json under the
// given target prefix. The metaOutcome tells callers whether a nil result
// means "nothing there" (metaEmpty) or "storage is broken" (metaError).
func loadLatestMeta(ctx context.Context, st storage.Storage, target string) (*meta.MetaFile, time.Time, metaOutcome) {
	// Reuse one connection for the List + Get when the backend supports it
	// (SFTP/FTPS). This runs once per (source × destination) every tick on the
	// refresher and scrubber hot path; without the shared session each call
	// dials a fresh SSH/TLS connection, so List+Get is two handshakes per pair
	// per tick. S3 has no session concept and falls through to per-call dialing.
	active := st
	if bs, ok := st.(storage.BatchStorage); ok {
		sess, closer, err := bs.WithSession(ctx)
		if err != nil {
			return nil, time.Time{}, metaError
		}
		defer func() { _ = closer() }()
		active = sess
	}

	objs, err := active.List(ctx, target+"/")
	if err != nil {
		return nil, time.Time{}, metaError
	}
	if len(objs) == 0 {
		return nil, time.Time{}, metaEmpty
	}
	latest := mostRecentMeta(objs)
	if latest.Path == "" {
		// Objects exist (dumps) but no meta.json — storage is reachable, there
		// is just nothing parseable to read. Not a destination failure.
		return nil, time.Time{}, metaEmpty
	}
	rc, err := active.Get(ctx, latest.Path)
	if err != nil {
		return nil, time.Time{}, metaError
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, time.Time{}, metaError
	}
	var m meta.MetaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		// A meta object exists but is unreadable — the destination is not
		// serving good data, so it counts as failed rather than empty.
		return nil, time.Time{}, metaError
	}
	m.Path = latest.Path
	ts := latest.LastModified
	if parsed := m.ParsedTimestamp(); !parsed.IsZero() {
		// Prefer the timestamp baked into the meta payload over the storage
		// LastModified, since some backends update mtime on listing or
		// replicate with skewed clocks.
		ts = parsed
	}
	return &m, ts, metaOK
}

func mostRecentMeta(objs []storage.Object) storage.Object {
	var latest storage.Object
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, ".meta.json") {
			continue
		}
		// Select by the path-encoded ISO timestamp, not LastModified: mtime is
		// unreliable (backends bump it on listing, clock-skewed replication),
		// which could pin an older run as "latest" and make last_run_status /
		// the scrubber check the wrong run. ISO timestamps in the path sort
		// lexically = chronologically.
		if latest.Path == "" || o.Path > latest.Path {
			latest = o
		}
	}
	return latest
}

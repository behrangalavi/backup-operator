package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/scheduler"
	"backup-operator/internal/secrets"
	"backup-operator/storage"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dataSource is the read-side surface the handlers depend on. Kept narrow so
// it's trivial to swap with a fake in tests.
type dataSource interface {
	listTargets(ctx context.Context) ([]targetSummary, error)
	target(ctx context.Context, name string) (*targetDetail, error)
	estimateDuration(ctx context.Context, name string, n int) (time.Duration, int, error)
	fleetHeatmap(ctx context.Context, days int) (*dashboardSummary, error)
	// listDestinations resolves every BackupDestination with its credentials;
	// getDestination resolves one by name (NotFound error when absent).
	listDestinations(ctx context.Context) ([]*secrets.Destination, error)
	getDestination(ctx context.Context, name string) (*secrets.Destination, error)
}

// nextRunAfter returns the next time the schedule will fire after `now`,
// or nil if the schedule is invalid or the source is suspended. UTC.
func nextRunAfter(schedule string, suspended bool, now time.Time) *time.Time {
	if suspended {
		return nil
	}
	next, ok := scheduler.NextRun(schedule, now)
	if !ok {
		return nil
	}
	return &next
}

// k8sData implements dataSource using a controller-runtime client to
// enumerate BackupSources / BackupDestinations in the watched namespace and
// the storage abstraction to fetch meta.json files from destinations.
type k8sData struct {
	client    client.Client
	namespace string
	log       logr.Logger
	// defaultSchedule mirrors the operator's DEFAULT_SCHEDULE so a source
	// without spec.schedule shows the schedule its CronJob actually runs on.
	defaultSchedule string

	latestCache *cache[map[string]*meta.MetaFile] // per-destination → target→meta
	runsCache   *cache[[]*meta.MetaFile]          // per (target,destination)

	// pool is the shared storage client cache. When non-nil, every
	// storage-touching method goes through it; when nil, the data
	// layer builds fresh clients per call. Server.New wires this
	// from ui.Config.Pool. See comments on Server.storageFor.
	pool StoragePool

	// onRefresh is called from background storage probes once fresh data
	// is in the cache. Server wires this to its SSE broker so the
	// frontend repaints when slow destinations finish refreshing,
	// without ever blocking the original request that triggered the
	// refresh. Optional — nil disables the broadcast.
	onRefresh func()

	// listMu guards the two RV-keyed list caches below. Every UI request
	// used to list and re-flatten every BackupSource / BackupDestination —
	// and for destinations that means one uncached Secret GET each. The
	// CR lists come from the informer cache and carry a ResourceVersion, so
	// when the RV is unchanged the flattened result is reused. Destinations
	// additionally expire after listCacheMaxAge: a rotated credentials
	// Secret does not bump the CR list's RV, and the storage pool keys on
	// the flattened Data, so the resolve must be redone periodically.
	listMu    sync.Mutex
	srcCache  listCache[sourceListing]
	destCache listCache[[]*secrets.Destination]
}

// listCache is one RV-keyed memo of a flattened object list.
type listCache[V any] struct {
	rv    string
	at    time.Time
	value V
	ok    bool
}

// sourceListing pairs the flattened sources with their creation timestamps.
type sourceListing struct {
	sources   []*secrets.Source
	createdAt map[string]time.Time
}

// listCacheMaxAge bounds how long an RV-keyed list memo is trusted even when
// the RV has not moved — the window within which a rotated destination
// Secret is still served with the old credentials by the UI.
const listCacheMaxAge = 15 * time.Second

// storageFor resolves a destination's client via the data layer's pool,
// sharing storageForPool with Server.storageFor. The data layer is wired
// separately from Server (tests build k8sData with a fake client and no
// pool), so it keeps its own thin wrapper over the shared helper.
func (d *k8sData) storageFor(dest *secrets.Destination) (storage.Storage, error) {
	return storageForPool(d.pool, dest, d.log.WithName("storage"))
}

// maxBackgroundRefreshes caps how many storage-probe goroutines the UI's
// stale-while-revalidate caches may run concurrently across all keys and both
// caches combined. Each probe is a fresh SSH/TLS handshake; left unbounded, a
// single SSE tick on a large fleet fans out one per destination at once and
// trips backend bruteforce protection (the QNAP IP-block incident, §18 ADR).
const maxBackgroundRefreshes = 6

func newK8sData(c client.Client, namespace, defaultSchedule string, log logr.Logger) *k8sData {
	// One semaphore shared by both caches so the bound is on total in-flight
	// probes, not per-cache.
	sem := make(chan struct{}, maxBackgroundRefreshes)
	cacheLog := log.WithName("ui-cache")
	latestCache := newCache[map[string]*meta.MetaFile](30 * time.Second)
	latestCache.sem = sem
	latestCache.log = cacheLog
	latestCache.name = "latest"
	runsCache := newCache[[]*meta.MetaFile](30 * time.Second)
	runsCache.sem = sem
	runsCache.log = cacheLog
	runsCache.name = "runs"
	return &k8sData{
		client:          c,
		namespace:       namespace,
		log:             log,
		defaultSchedule: defaultSchedule,
		latestCache:     latestCache,
		runsCache:       runsCache,
	}
}

func (d *k8sData) listTargets(ctx context.Context) ([]targetSummary, error) {
	sources, createdAt, err := d.listSourcesWithMeta(ctx)
	if err != nil {
		return nil, err
	}
	dests, err := d.listDestinations(ctx)
	if err != nil {
		return nil, err
	}

	// Pull "latest meta per target" once per destination, then merge by
	// target. The first destination that has a recorded run for a target
	// wins; that's good enough for an overview row.
	//
	// IMPORTANT: this is the hot path for /api/targets, which the dashboard
	// hits on every navigation. We MUST NOT block on the storage probe —
	// an unreachable destination would otherwise pin every dashboard click
	// on its 8 s timeout. getOrRefreshAsync returns whatever's cached
	// (zero-value on first ever miss) and spawns the refresh in the
	// background; onRefresh fires an SSE event so the frontend repaints
	// when fresh data lands.
	latestByTarget := map[string]*meta.MetaFile{}
	for _, dest := range dests {
		dest := dest // capture for the goroutine closure
		perDest, _ := d.latestCache.getOrRefreshAsync(dest.Name, func() (map[string]*meta.MetaFile, error) {
			// Background refresh runs in its own goroutine — use a fresh
			// context with a sane upper bound so a hung destination can't
			// hold a goroutine forever. The original request's ctx is
			// the wrong one here (it has likely returned to the client
			// by the time we run).
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			st, err := d.storageFor(dest)
			if err != nil {
				return nil, err
			}
			return meta.LatestPerTarget(rctx, st)
		}, d.onRefresh)
		if perDest == nil {
			continue
		}
		for tgt, m := range perDest {
			existing, exists := latestByTarget[tgt]
			if !exists {
				latestByTarget[tgt] = m
			} else if existing.IsFailure() && !m.IsFailure() && m.Timestamp >= existing.Timestamp {
				latestByTarget[tgt] = m
			} else if m.Timestamp > existing.Timestamp {
				latestByTarget[tgt] = m
			}
		}
	}

	out := make([]targetSummary, 0, len(sources))
	now := time.Now().UTC()
	for _, src := range sources {
		materialisedSchedule := scheduler.ApplyJitter(src.Schedule, src.JitterMinutes, src.ObjectName)
		summary := targetSummary{
			Name:       src.TargetName,
			ObjectName: src.ObjectName,
			DBType:     src.DBType,
			// The materialised schedule is what the CronJob actually
			// runs after per-source minute jitter. Operators reading
			// the dashboard need to see the effective time, not the
			// pre-jitter annotation — otherwise "0 2 * * *" on the
			// card and "37 2 * * *" in the cluster would silently
			// disagree.
			Schedule:     materialisedSchedule,
			NextRun:      nextRunAfter(materialisedSchedule, src.Suspended, now),
			Suspended:    src.Suspended,
			Destinations: destinationsAllowedFor(src, dests),
			CreatedAt:    createdAt[src.ObjectName],
			Latest:       latestByTarget[src.TargetName],
			Analysis: analysisFlags{
				AnalyzerEnabled:   src.AnalyzerEnabled,
				EmptyDumpCheck:    src.EmptyDumpCheck,
				RowDropThreshold:  src.RowDropThreshold,
				SizeDropThreshold: src.SizeDropThreshold,
			},
			Verification: verificationFlags{
				Mode:       src.RestoreVerificationMode,
				Interval:   formatDurationOptional(src.RestoreVerificationInterval),
				Image:      src.VerificationImage,
				VolumeSize: src.VerificationVolumeSize,
			},
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// estimateDuration returns a median run duration for target across all
// allowed destinations and the sample size that produced it. Returns
// (0, 0, nil) when no successful, duration-bearing meta is available
// (e.g. brand-new target, all destinations unreachable, only legacy
// metas without DurationSeconds). Reuses runsCache so calling this
// per-running-job from /api/jobs does not amplify storage I/O.
//
// CRITICAL: the storage probe is non-blocking (getOrRefreshAsync). The
// estimate is only a progress-bar nicety, so it must NEVER pin /api/jobs
// on a slow/unreachable destination. The previous blocking getOrLoad
// inherited the request context (no deadline) and probed destinations
// sequentially — a hung QNAP FTPS destination would fall back to its 30 s
// dial timeout and freeze /api/jobs on every poll while a job ran, which
// in turn starved the browser's per-host connection pool and left the
// whole dashboard showing only the nav. Now we return whatever's cached
// immediately and let the background refresh + onRefresh SSE event fill
// in the estimate on a later render.
func (d *k8sData) estimateDuration(ctx context.Context, name string, n int) (time.Duration, int, error) {
	sources, err := d.listSources(ctx)
	if err != nil {
		return 0, 0, err
	}
	var src *secrets.Source
	for _, s := range sources {
		if s.TargetName == name {
			src = s
			break
		}
	}
	if src == nil {
		return 0, 0, nil
	}
	allDests, err := d.listDestinations(ctx)
	if err != nil {
		return 0, 0, err
	}
	dests := secrets.FilterDestinations(src, allDests)
	if len(dests) == 0 {
		return 0, 0, nil
	}
	// Merge metas across destinations like target() does — a single
	// destination may be unreachable or lag behind. Dedup by timestamp.
	byTimestamp := map[string]*meta.MetaFile{}
	for _, dest := range dests {
		dest := dest // capture for the goroutine closure
		// Bounded listing: the median needs n successful runs, so fetching
		// the newest 4n covers a run of failures without pulling the whole
		// history. Own cache key — target() keeps the unbounded history under
		// the plain name@dest key for the detail page.
		key := name + "@" + dest.Name + "#latest" + strconv.Itoa(n*4)
		got, _ := d.runsCache.getOrRefreshAsync(key, func() ([]*meta.MetaFile, error) {
			// Background refresh runs in its own goroutine with a bounded
			// context — the request's ctx has likely returned to the client
			// by the time this runs, and an unbounded probe would let a hung
			// destination pin a goroutine forever.
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			st, err := d.storageFor(dest)
			if err != nil {
				return nil, err
			}
			return meta.ListWithOptions(rctx, st, name, meta.ListOptions{Limit: n * 4})
		}, d.onRefresh)
		for _, m := range got {
			if _, ok := byTimestamp[m.Timestamp]; !ok {
				byTimestamp[m.Timestamp] = m
			}
		}
	}
	merged := make([]*meta.MetaFile, 0, len(byTimestamp))
	for _, m := range byTimestamp {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Timestamp > merged[j].Timestamp })
	dur, count := meta.MedianDurationFromList(merged, n)
	return dur, count, nil
}

func (d *k8sData) target(ctx context.Context, name string) (*targetDetail, error) {
	sources, err := d.listSources(ctx)
	if err != nil {
		return nil, err
	}
	var src *secrets.Source
	for _, s := range sources {
		if s.TargetName == name {
			src = s
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("target %q not found", name)
	}

	allDests, err := d.listDestinations(ctx)
	if err != nil {
		return nil, err
	}
	dests := secrets.FilterDestinations(src, allDests)
	if len(dests) == 0 {
		return &targetDetail{Source: src, Destinations: nil, Runs: nil}, nil
	}

	// Merge run history from ALL destinations. Each destination may have
	// runs that others don't (e.g. partial upload failures). Deduplicate
	// by timestamp, preferring the meta from the destination it was fetched
	// from so SourceDestination is set for download routing.
	//
	// Probe destinations in parallel, each under a bounded per-destination
	// timeout. The detail page genuinely needs the run history (unlike the
	// dashboard, which can show stale), so this stays blocking — but it must
	// NOT inherit the request context unbounded and probe sequentially: a
	// slow/unreachable QNAP FTPS would otherwise pin /api/targets/<t>/runs
	// until the browser's client-side timeout aborted it, leaving the detail
	// page perpetually "loading". With a 12 s cap per destination and
	// fan-out, the endpoint returns within ~12 s worst case and yields the
	// runs from whichever destinations are reachable.
	byTimestamp := map[string]*meta.MetaFile{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, dest := range dests {
		dest := dest
		key := name + "@" + dest.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Goroutine(d.log, "target-runs", dest.Name)
			sem <- struct{}{}
			defer func() { <-sem }()

			got, err := d.runsCache.getOrLoad(key, func() ([]*meta.MetaFile, error) {
				rctx, cancel := context.WithTimeout(ctx, 12*time.Second)
				defer cancel()
				st, err := d.storageFor(dest)
				if err != nil {
					return nil, err
				}
				return meta.List(rctx, st, name)
			})
			if err != nil {
				d.log.V(1).Info("destination unreadable for run history", "destination", dest.Name, "err", err.Error())
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range got {
				existing, ok := byTimestamp[m.Timestamp]
				if !ok {
					byTimestamp[m.Timestamp] = m
				} else if existing.IsFailure() && !m.IsFailure() {
					byTimestamp[m.Timestamp] = m
				}
			}
		}()
	}
	wg.Wait()
	runs := make([]*meta.MetaFile, 0, len(byTimestamp))
	for _, m := range byTimestamp {
		runs = append(runs, m)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Timestamp > runs[j].Timestamp })
	return &targetDetail{Source: src, Destinations: dests, Runs: runs}, nil
}

func (d *k8sData) listSources(ctx context.Context) ([]*secrets.Source, error) {
	out, _, err := d.listSourcesWithMeta(ctx)
	return out, err
}

// listSourcesWithMeta returns flattened sources alongside an objectName →
// creationTimestamp map so UI callers can sort by creation time without
// bloating secrets.Source itself. Sources are flattened spec-only (no
// credentials Secret read): the UI never connects to a database.
func (d *k8sData) listSourcesWithMeta(ctx context.Context) ([]*secrets.Source, map[string]time.Time, error) {
	var list v1alpha1.BackupSourceList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace)); err != nil {
		return nil, nil, err
	}
	d.listMu.Lock()
	if c := d.srcCache; c.ok && c.rv == list.ResourceVersion && list.ResourceVersion != "" && time.Since(c.at) < listCacheMaxAge {
		d.listMu.Unlock()
		return c.value.sources, c.value.createdAt, nil
	}
	d.listMu.Unlock()
	out := make([]*secrets.Source, 0, len(list.Items))
	createdAt := make(map[string]time.Time, len(list.Items))
	for i := range list.Items {
		src, err := secrets.FromBackupSource(&list.Items[i], nil, d.defaultSchedule)
		if err != nil {
			d.log.V(1).Info("skipping invalid source", "source", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, src)
		createdAt[list.Items[i].Name] = list.Items[i].CreationTimestamp.Time
	}
	d.listMu.Lock()
	d.srcCache = listCache[sourceListing]{rv: list.ResourceVersion, at: time.Now(), value: sourceListing{sources: out, createdAt: createdAt}, ok: true}
	d.listMu.Unlock()
	return out, createdAt, nil
}

// listDestinations resolves every BackupDestination with its credentials
// Secret. A destination whose Secret is missing or whose spec does not
// flatten is logged and skipped so one broken destination doesn't blank the
// dashboard. Secrets are read uncached (the manager disables the Secret
// informer), so this costs one API read per destination.
func (d *k8sData) listDestinations(ctx context.Context) ([]*secrets.Destination, error) {
	var list v1alpha1.BackupDestinationList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace)); err != nil {
		return nil, err
	}
	d.listMu.Lock()
	if c := d.destCache; c.ok && c.rv == list.ResourceVersion && list.ResourceVersion != "" && time.Since(c.at) < listCacheMaxAge {
		d.listMu.Unlock()
		return c.value, nil
	}
	d.listMu.Unlock()
	out := make([]*secrets.Destination, 0, len(list.Items))
	for i := range list.Items {
		dest, err := d.resolveDestination(ctx, &list.Items[i])
		if err != nil {
			d.log.V(1).Info("skipping destination", "destination", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, dest)
	}
	d.listMu.Lock()
	d.destCache = listCache[[]*secrets.Destination]{rv: list.ResourceVersion, at: time.Now(), value: out, ok: true}
	d.listMu.Unlock()
	return out, nil
}

// getDestination resolves one BackupDestination by name. A missing object
// surfaces as the API server's NotFound error so handlers can map it to 404.
func (d *k8sData) getDestination(ctx context.Context, name string) (*secrets.Destination, error) {
	var bd v1alpha1.BackupDestination
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: name}, &bd); err != nil {
		return nil, err
	}
	return d.resolveDestination(ctx, &bd)
}

func (d *k8sData) resolveDestination(ctx context.Context, bd *v1alpha1.BackupDestination) (*secrets.Destination, error) {
	var creds corev1.Secret
	key := client.ObjectKey{Namespace: bd.Namespace, Name: bd.Spec.CredentialsSecretRef.Name}
	if err := d.client.Get(ctx, key, &creds); err != nil {
		return nil, fmt.Errorf("credentials secret %q: %w", key.Name, err)
	}
	return secrets.FromBackupDestination(bd, &creds)
}

// findRun resolves a (target, timestamp) pair to the MetaFile that the
// detail page already loaded. Returns nil if the timestamp doesn't match
// a known run.
// formatDurationOptional renders a Go duration as the same string format
// the user typed in the annotation, with the special case that 0 (the
// "annotation absent" sentinel) becomes "" so JSON marshalling drops it
// via omitempty rather than rendering "0s".
func formatDurationOptional(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func findRun(runs []*meta.MetaFile, timestamp string) *meta.MetaFile {
	for _, r := range runs {
		if r.Timestamp == timestamp {
			return r
		}
	}
	return nil
}

// destinationsAllowedFor returns the sorted names of destinations the source's
// allow-list permits, used purely for display. Delegates to secrets.FilterDestinations
// for the actual filtering logic.
func destinationsAllowedFor(src *secrets.Source, all []*secrets.Destination) []string {
	filtered := secrets.FilterDestinations(src, all)
	names := make([]string, len(filtered))
	for i, d := range filtered {
		names[i] = d.Name
	}
	sort.Strings(names)
	return names
}

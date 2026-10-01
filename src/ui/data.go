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

// dashboardSummary bundles every "across the fleet" dataset the
// dashboard needs in one round-trip. Each projection is derived from
// the same per-target run history scan, so paying for the meta-file
// reads once instead of five separate endpoints.
type dashboardSummary struct {
	Heatmap           []heatmapRow           `json:"heatmap"`
	Storage           []storageDayPoint      `json:"storage"`
	Anomalies         []anomalyEntry         `json:"anomalies"`
	Durations         []durationStat         `json:"durations"`
	VerificationDaily []verificationDayPoint `json:"verificationDaily"`
}

// durationStat is one bar in the per-target duration distribution
// chart. Only counts successful runs over the window — failures fail
// fast and would skew the median toward zero.
type durationStat struct {
	Target string  `json:"target"`
	DBType string  `json:"dbType"`
	Count  int     `json:"count"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// verificationDayPoint feeds the restore-verification pass-rate
// trend. Passed counts runs whose restoreVerification.verdict =
// "match"; Failed counts everything else (mismatch, skipped, error).
// Runs without a verification block (mode=off) aren't counted at
// all — only verification-armed runs contribute to the pass rate.
type verificationDayPoint struct {
	Day    string `json:"day"`
	Passed int    `json:"passed"`
	Failed int    `json:"failed"`
}

// heatmapRow is one lane in the dashboard's fleet heatmap. Days is
// always exactly the requested length (today as the rightmost cell);
// missing entries are zero-valued so the frontend doesn't have to
// reason about gaps.
type heatmapRow struct {
	Target string        `json:"target"`
	DBType string        `json:"dbType"`
	Days   []heatmapCell `json:"days"`
}

type heatmapCell struct {
	Day    string `json:"day"`    // YYYY-MM-DD UTC
	Status string `json:"status"` // "ok" | "failed" | "mixed" | "none"
	Runs   int    `json:"runs"`
}

// storageDayPoint is one column of the stacked area chart: total
// encrypted-dump bytes uploaded that day, broken down per source
// (target name). Per-source is more actionable than per-DB-type —
// operators want to know which specific source is eating storage,
// not which technology. The frontend hashes target names to stable
// colours so the same source gets the same colour across reloads.
// Failed runs are excluded — they have no real payload.
type storageDayPoint struct {
	Day       string           `json:"day"`       // YYYY-MM-DD UTC
	PerTarget map[string]int64 `json:"perTarget"` // target → bytes
}

// anomalyEntry is one event for the stream visualization. Kind and
// Subject come straight from analyzer.Anomaly; severity is derived
// from kind so the frontend can colour without keeping its own map.
type anomalyEntry struct {
	Target   string `json:"target"`
	DBType   string `json:"dbType"`
	Time     string `json:"time"` // RFC3339 UTC of the run that produced this
	Day      string `json:"day"`  // YYYY-MM-DD (denormalized for trivial bucketing in JS)
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Detail   string `json:"detail"`
	Severity string `json:"severity"` // "critical" | "warning" | "info"
}

// anomalySeverity classifies a single Kind into one of three buckets
// used for colour coding. The mapping is conservative — only the kinds
// that signal "data is missing or shrunk" are critical; schema/charset
// changes are usually intentional and rate info.
func anomalySeverity(kind string) string {
	switch kind {
	case "size-collapse", "table-disappeared", "dump-empty-content":
		return "critical"
	case "row-count-drop", "row-drop":
		return "warning"
	default:
		return "info"
	}
}

// targetSummary is what the index page needs per source.
type targetSummary struct {
	Name       string
	ObjectName string
	DBType     string
	Schedule   string
	// NextRun is the next time the CronJob will fire after now, computed
	// from the materialised (post-jitter) Schedule. nil if the schedule
	// could not be parsed (invalid cron expression) or the source is
	// suspended. Format: RFC3339 UTC. The UI converts to local time and
	// shows both absolute and relative ("in 4h 32m").
	NextRun      *time.Time `json:"nextRun,omitempty"`
	Suspended    bool
	Destinations []string
	CreatedAt    time.Time      // BackupSource CreationTimestamp; read off the raw object at access time
	Latest       *meta.MetaFile // nil if no runs yet

	// Analysis surfaces the per-source analyzer toggles. The UI uses this to
	// render the "Analysis Coverage" card so an operator can see at a glance
	// which validations are armed for this source — and which are off-by-
	// design for the source's DB type (charset for mongo/redis, row counter
	// for mongo/redis, etc.).
	Analysis analysisFlags

	// Verification surfaces the source's restore-verification config so
	// the source list can render "mode | last-verified" without an
	// extra round-trip. The actual verdict for the most recent run
	// lives in Latest.RestoreVerification.
	Verification verificationFlags `json:"verification"`
}

type analysisFlags struct {
	AnalyzerEnabled   bool    `json:"analyzerEnabled"`
	EmptyDumpCheck    bool    `json:"emptyDumpCheck"`
	RowDropThreshold  float64 `json:"rowDropThreshold"`  // -1 = default
	SizeDropThreshold float64 `json:"sizeDropThreshold"` // -1 = default
}

// verificationFlags mirrors the per-source verification configuration
// so the source list can render configuration-state badges without
// re-querying the Source secret.
type verificationFlags struct {
	Mode       string `json:"mode"`               // "off" / stream-validate / schema-only / sample / full
	Interval   string `json:"interval,omitempty"` // raw annotation value (Go duration)
	Image      string `json:"image,omitempty"`
	VolumeSize string `json:"volumeSize,omitempty"`
}

// targetDetail backs the per-source detail page.
type targetDetail struct {
	Source       *secrets.Source
	Destinations []*secrets.Destination
	Runs         []*meta.MetaFile
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

// fleetHeatmap aggregates per-target, per-day status, per-day storage
// bytes, and analyzer anomalies for the last `days` days. All three
// derive from the same per-target run scan — paying for the meta-file
// reads once instead of three times. Reuses runsCache so a dashboard
// hit doesn't double-dial destinations the per-target detail page
// already cached. Background-refresh via the same getOrRefreshAsync
// pattern as latestCache — a dashboard render never blocks on
// storage probes.
//
// Status classification per day:
//   - "ok"     : at least one run, no failures
//   - "failed" : at least one failure, no successes
//   - "mixed"  : both success and failure that day (manual re-run after fail)
//   - "none"   : no run recorded for that day
//
// Cap `days` at 90 to bound the per-target work; defaults to 30.
func (d *k8sData) fleetHeatmap(ctx context.Context, days int) (*dashboardSummary, error) {
	if days <= 0 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	sources, _, err := d.listSourcesWithMeta(ctx)
	if err != nil {
		return nil, err
	}
	allDests, err := d.listDestinations(ctx)
	if err != nil {
		return nil, err
	}

	// Build the day axis once. UTC throughout so day boundaries match
	// the dump timestamp (also UTC); converting to the operator's local
	// time would shift cells +/- 1 day in some zones.
	now := time.Now().UTC()
	dayAxis := make([]string, days)
	for i := 0; i < days; i++ {
		t := now.AddDate(0, 0, -(days - 1 - i))
		dayAxis[i] = t.Format("2006-01-02")
	}
	earliestDay := dayAxis[0]
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))

	rows := make([]heatmapRow, 0, len(sources))
	// Fleet-wide daily byte totals, keyed by day then target name. The
	// frontend uses these to render a per-source stacked area; with
	// many sources the stacked area can get visually busy, but
	// per-source is what operators ask ("which source is growing
	// fastest?") rather than per-DB-type ("are postgres dumps
	// dominant?").
	bytesByDayTarget := make(map[string]map[string]int64, days)
	for _, d := range dayAxis {
		bytesByDayTarget[d] = make(map[string]int64)
	}
	// Anomalies are streamed as a flat list — the frontend buckets per
	// day at render time. Bounded while collecting (newest maxAnomalies
	// kept) so a busy fleet's history never materialises an unbounded slice
	// just to be truncated afterwards.
	anomalies := newBoundedNewest(maxAnomalies)

	// Verification: per-day pass/fail counters fleet-wide. Only runs
	// with a RestoreVerification block contribute; runs with mode=off
	// are excluded from both numerator and denominator.
	verifPassed := make(map[string]int, days)
	verifFailed := make(map[string]int, days)
	// Duration distribution: per-target raw duration list, summarised
	// post-loop into median/p95/min/max.
	durationsByTarget := make(map[string][]float64, len(sources))

	for _, src := range sources {
		row := heatmapRow{
			Target: src.TargetName,
			DBType: src.DBType,
			Days:   make([]heatmapCell, days),
		}
		for i, d := range dayAxis {
			row.Days[i] = heatmapCell{Day: d, Status: "none"}
		}

		// Per-day counters; written through to row.Days at the end so
		// the loop body stays branchless.
		ok := make(map[string]int, days)
		failed := make(map[string]int, days)

		// Prefer the first allowed destination that already has cached
		// runs — most dashboard users keep the per-target detail page
		// open occasionally, populating the cache. New targets fall
		// back to a dial via getOrRefreshAsync (non-blocking).
		dests := secrets.FilterDestinations(src, allDests)
		for _, dest := range dests {
			// Own cache key per day window: the heatmap only needs runs
			// inside its window, so the listing is bounded with Since and
			// the cached metas are slimmed to the fields the charts read.
			// target() keeps the full history under the plain name@dest key.
			key := src.TargetName + "@" + dest.Name + "#since" + earliestDay
			dest := dest
			got, _ := d.runsCache.getOrRefreshAsync(key, func() ([]*meta.MetaFile, error) {
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				st, err := d.storageFor(dest)
				if err != nil {
					return nil, err
				}
				runs, err := meta.ListWithOptions(rctx, st, src.TargetName, meta.ListOptions{Since: windowStart})
				if err != nil {
					return nil, err
				}
				for _, m := range runs {
					slimForHeatmap(m)
				}
				return runs, nil
			}, d.onRefresh)
			if got == nil {
				continue
			}
			for _, m := range got {
				ts := m.ParsedTimestamp()
				if ts.IsZero() {
					continue
				}
				day := ts.UTC().Format("2006-01-02")
				if day < earliestDay {
					continue
				}
				if m.IsFailure() {
					failed[day]++
				} else {
					ok[day]++
					// Bytes only count for successful runs. Failures
					// have no real payload — including their size
					// (typically 0) wouldn't break the chart but
					// counting them as 0 in the area chart is the
					// honest representation.
					bytesByDayTarget[day][src.TargetName] += m.EncryptedSizeBytes
					// Duration distribution counts successful runs
					// only; failures fail-fast (often in seconds)
					// and would systematically skew the median toward
					// zero. DurationSeconds == 0 = legacy meta from
					// before that field existed.
					if m.DurationSeconds > 0 {
						durationsByTarget[src.TargetName] = append(durationsByTarget[src.TargetName], m.DurationSeconds)
					}
				}
				// Restore-verification: count whenever the run has a
				// RestoreVerification block, regardless of success/
				// failure. A failed dump can still have produced (and
				// failed) a verification attempt; both are signal.
				if m.RestoreVerification != nil {
					if m.RestoreVerification.Verdict == meta.VerificationMatch {
						verifPassed[day]++
					} else {
						verifFailed[day]++
					}
				}
				// Analyzer anomalies are attached to the run's Report.
				// Capture each one as a stream entry; the frontend
				// renders them as dots on a timeline.
				if m.Report != nil {
					for _, a := range m.Report.Anomalies {
						anomalies.add(anomalyEntry{
							Target:   src.TargetName,
							DBType:   src.DBType,
							Time:     ts.UTC().Format(time.RFC3339),
							Day:      day,
							Kind:     a.Kind,
							Subject:  a.Subject,
							Detail:   a.Detail,
							Severity: anomalySeverity(a.Kind),
						})
					}
				}
			}
			// One destination's run history is enough — every dest
			// should hold the same set of runs modulo inconsistencies
			// (already surfaced by the consistency panel). Keeps the
			// loop O(targets), not O(targets × destinations).
			break
		}

		for i, d := range dayAxis {
			o, f := ok[d], failed[d]
			row.Days[i].Runs = o + f
			switch {
			case o > 0 && f > 0:
				row.Days[i].Status = "mixed"
			case f > 0:
				row.Days[i].Status = "failed"
			case o > 0:
				row.Days[i].Status = "ok"
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Target < rows[j].Target })

	// Materialise storage points in day order so the frontend can plot
	// them without sorting client-side.
	storage := make([]storageDayPoint, days)
	for i, d := range dayAxis {
		storage[i] = storageDayPoint{Day: d, PerTarget: bytesByDayTarget[d]}
	}

	// Anomalies: newest first. The collector already kept only the newest
	// maxAnomalies so the JSON stays under a few hundred KiB even on busy
	// fleets; older entries are still on the per-target detail page.
	anomalyList := anomalies.sorted()

	// Duration stats per target — median + p95 + min + max. Sort the
	// per-target slice once, compute percentiles inline (Go has no
	// stdlib percentile so this is just an index lookup on the
	// sorted slice — bounded by Count which is at most ~days for a
	// daily backup).
	durations := make([]durationStat, 0, len(durationsByTarget))
	for _, src := range sources {
		vals := durationsByTarget[src.TargetName]
		if len(vals) == 0 {
			continue
		}
		sort.Float64s(vals)
		median := vals[len(vals)/2]
		// p95 index: round up so a single sample reports itself as p95.
		p95idx := int(float64(len(vals)-1) * 0.95)
		durations = append(durations, durationStat{
			Target: src.TargetName,
			DBType: src.DBType,
			Count:  len(vals),
			Median: median,
			P95:    vals[p95idx],
			Min:    vals[0],
			Max:    vals[len(vals)-1],
		})
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i].Median > durations[j].Median })

	// Verification pass-rate: emit one point per day in axis order so
	// the line chart has stable X-coords. Zero/zero days contribute a
	// (0,0) sample — the frontend treats them as gaps.
	verification := make([]verificationDayPoint, days)
	for i, d := range dayAxis {
		verification[i] = verificationDayPoint{Day: d, Passed: verifPassed[d], Failed: verifFailed[d]}
	}

	return &dashboardSummary{
		Heatmap:           rows,
		Storage:           storage,
		Anomalies:         anomalyList,
		Durations:         durations,
		VerificationDaily: verification,
	}, nil
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

// maxAnomalies caps the anomaly stream in the dashboard payload.
const maxAnomalies = 200

// boundedNewest keeps the newest `capacity` anomaly entries by Time while
// they are collected, replacing the oldest kept entry when full. Capacity is
// small (200), so a linear re-scan for the new oldest on each replacement is
// cheaper than a heap and keeps the code obvious.
type boundedNewest struct {
	capacity int
	items    []anomalyEntry
	oldest   int // index of the oldest item while full; -1 when unknown
}

func newBoundedNewest(capacity int) *boundedNewest {
	return &boundedNewest{capacity: capacity, items: make([]anomalyEntry, 0, capacity), oldest: -1}
}

func (b *boundedNewest) add(e anomalyEntry) {
	if len(b.items) < b.capacity {
		b.items = append(b.items, e)
		return
	}
	if b.oldest < 0 {
		b.oldest = b.findOldest()
	}
	if e.Time <= b.items[b.oldest].Time {
		return // older than everything kept
	}
	b.items[b.oldest] = e
	b.oldest = b.findOldest()
}

func (b *boundedNewest) findOldest() int {
	idx := 0
	for i := range b.items {
		if b.items[i].Time < b.items[idx].Time {
			idx = i
		}
	}
	return idx
}

// sorted returns the kept entries newest-first. Never nil, so the JSON
// renders [] rather than null.
func (b *boundedNewest) sorted() []anomalyEntry {
	out := make([]anomalyEntry, len(b.items))
	copy(out, b.items)
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// slimForHeatmap drops the per-table payload the dashboard never reads. The
// heatmap cache holds `days` runs per (target, destination); a wide schema
// would otherwise keep thousands of TableStats per source resident in the
// operator for the cache TTL. Report.Anomalies, size, duration, status and
// the verification block are kept — those are what the charts consume.
func slimForHeatmap(m *meta.MetaFile) {
	m.Stats = nil
	if m.Report != nil {
		m.Report.Current = nil
		m.Report.Previous = nil
		m.Report.TableDiffs = nil
	}
}

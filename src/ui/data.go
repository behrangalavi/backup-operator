package ui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/scheduler"
	"backup-operator/internal/secrets"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
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
}

// dashboardSummary bundles the three "across the fleet" datasets the
// dashboard needs in one round-trip. All three are derived from the
// same per-target run history scan, so paying for it once is much
// cheaper than three endpoints reading the same meta.json files.
type dashboardSummary struct {
	Heatmap   []heatmapRow      `json:"heatmap"`
	Storage   []storageDayPoint `json:"storage"`
	Anomalies []anomalyEntry    `json:"anomalies"`
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
// encrypted-dump bytes uploaded that day, broken down by db type so
// the operator sees "Postgres backups dominate" at a glance. Failed
// runs are excluded — they have no real payload.
type storageDayPoint struct {
	Day     string           `json:"day"`     // YYYY-MM-DD UTC
	PerType map[string]int64 `json:"perType"` // dbType → bytes
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
	Name         string
	SecretName   string
	DBType       string
	Schedule     string
	// NextRun is the next time the CronJob will fire after now, computed
	// from the materialised (post-jitter) Schedule. nil if the schedule
	// could not be parsed (invalid cron expression) or the source is
	// suspended. Format: RFC3339 UTC. The UI converts to local time and
	// shows both absolute and relative ("in 4h 32m").
	NextRun      *time.Time `json:"nextRun,omitempty"`
	Suspended    bool
	Destinations []string
	CreatedAt    time.Time      // Secret CreationTimestamp; read off raw corev1 at access time
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
	AnalyzerEnabled    bool    `json:"analyzerEnabled"`
	EmptyDumpCheck     bool    `json:"emptyDumpCheck"`
	RowDropThreshold   float64 `json:"rowDropThreshold"`  // -1 = default
	SizeDropThreshold  float64 `json:"sizeDropThreshold"` // -1 = default
}

// verificationFlags mirrors the per-source verification configuration
// so the source list can render configuration-state badges without
// re-querying the Source secret.
type verificationFlags struct {
	Mode     string `json:"mode"`               // "off" / stream-validate / schema-only / sample / full
	Interval string `json:"interval,omitempty"` // raw annotation value (Go duration)
	Image    string `json:"image,omitempty"`
	VolumeSize string `json:"volumeSize,omitempty"`
}

// targetDetail backs the per-source detail page.
type targetDetail struct {
	Source       *secrets.Source
	Destinations []*secrets.Destination
	Runs         []*meta.MetaFile
}

// k8sData implements dataSource using a controller-runtime client to enumerate
// labelled Secrets in the watched namespace and the storage abstraction to
// fetch meta.json files from destinations.
// cronParser parses standard 5-field cron expressions ("M H DoM Mo DoW"),
// matching what Kubernetes CronJobs accept. The default robfig parser
// adds an extra leading-seconds field which would reject every valid
// CronJob schedule — we use the explicit Standard parser to match.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// nextRunAfter returns the next time the schedule will fire after `now`,
// or nil if the schedule is invalid or the source is suspended. UTC.
func nextRunAfter(schedule string, suspended bool, now time.Time) *time.Time {
	if suspended || schedule == "" {
		return nil
	}
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil
	}
	next := sched.Next(now)
	if next.IsZero() {
		return nil
	}
	return &next
}

type k8sData struct {
	client    client.Client
	namespace string
	log       logr.Logger

	latestCache *cache[map[string]*meta.MetaFile] // per-destination → target→meta
	runsCache   *cache[[]*meta.MetaFile]          // per (target,destination)

	// onRefresh is called from background storage probes once fresh data
	// is in the cache. Server wires this to its SSE broker so the
	// frontend repaints when slow destinations finish refreshing,
	// without ever blocking the original request that triggered the
	// refresh. Optional — nil disables the broadcast.
	onRefresh func()
}

func newK8sData(c client.Client, namespace string, log logr.Logger) *k8sData {
	return &k8sData{
		client:      c,
		namespace:   namespace,
		log:         log,
		latestCache: newCache[map[string]*meta.MetaFile](30 * time.Second),
		runsCache:   newCache[[]*meta.MetaFile](30 * time.Second),
	}
}

func (d *k8sData) listTargets(ctx context.Context) ([]targetSummary, error) {
	sources, createdAt, err := d.listSourceSecretsWithMeta(ctx)
	if err != nil {
		return nil, err
	}
	dests, err := d.listDestinationSecrets(ctx)
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
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
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
		materialisedSchedule := scheduler.ApplyJitter(src.Schedule, src.JitterMinutes, src.SecretName)
		summary := targetSummary{
			Name:         src.TargetName,
			SecretName:   src.SecretName,
			DBType:       src.DBType,
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
			CreatedAt:    createdAt[src.SecretName],
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
func (d *k8sData) estimateDuration(ctx context.Context, name string, n int) (time.Duration, int, error) {
	sources, err := d.listSourceSecrets(ctx)
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
	allDests, err := d.listDestinationSecrets(ctx)
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
		key := name + "@" + dest.Name
		got, err := d.runsCache.getOrLoad(key, func() ([]*meta.MetaFile, error) {
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
			if err != nil {
				return nil, err
			}
			return meta.List(ctx, st, name)
		})
		if err != nil {
			continue
		}
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
	sources, err := d.listSourceSecrets(ctx)
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

	allDests, err := d.listDestinationSecrets(ctx)
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
	byTimestamp := map[string]*meta.MetaFile{}
	for _, dest := range dests {
		key := name + "@" + dest.Name
		got, err := d.runsCache.getOrLoad(key, func() ([]*meta.MetaFile, error) {
			st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
			if err != nil {
				return nil, err
			}
			return meta.List(ctx, st, name)
		})
		if err != nil {
			d.log.V(1).Info("destination unreadable for run history", "destination", dest.Name, "err", err.Error())
			continue
		}
		for _, m := range got {
			existing, ok := byTimestamp[m.Timestamp]
			if !ok {
				byTimestamp[m.Timestamp] = m
			} else if existing.IsFailure() && !m.IsFailure() {
				byTimestamp[m.Timestamp] = m
			}
		}
	}
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
	sources, _, err := d.listSourceSecretsWithMeta(ctx)
	if err != nil {
		return nil, err
	}
	allDests, err := d.listDestinationSecrets(ctx)
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

	rows := make([]heatmapRow, 0, len(sources))
	// Fleet-wide daily byte totals, keyed by day then dbType so the
	// frontend can render either a single-series area or a stacked area.
	bytesByDayType := make(map[string]map[string]int64, days)
	for _, d := range dayAxis {
		bytesByDayType[d] = make(map[string]int64)
	}
	// Anomalies are streamed as a flat list — the frontend buckets per
	// day at render time. Capped after the loop to keep payload sane.
	var anomalies []anomalyEntry

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
			key := src.TargetName + "@" + dest.Name
			dest := dest
			got, _ := d.runsCache.getOrRefreshAsync(key, func() ([]*meta.MetaFile, error) {
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, d.log.WithName("storage"))
				if err != nil {
					return nil, err
				}
				return meta.List(rctx, st, src.TargetName)
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
					bytesByDayType[day][src.DBType] += m.EncryptedSizeBytes
				}
				// Analyzer anomalies are attached to the run's Report.
				// Capture each one as a stream entry; the frontend
				// renders them as dots on a timeline.
				if m.Report != nil {
					for _, a := range m.Report.Anomalies {
						anomalies = append(anomalies, anomalyEntry{
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
		storage[i] = storageDayPoint{Day: d, PerType: bytesByDayType[d]}
	}

	// Anomalies: newest first, cap at 200 so the JSON stays under a
	// few hundred KiB even on busy fleets. A 30-day timeline visualises
	// fine with that many; older entries are still on the per-target
	// detail page.
	sort.Slice(anomalies, func(i, j int) bool { return anomalies[i].Time > anomalies[j].Time })
	if len(anomalies) > 200 {
		anomalies = anomalies[:200]
	}

	return &dashboardSummary{
		Heatmap:   rows,
		Storage:   storage,
		Anomalies: anomalies,
	}, nil
}

func (d *k8sData) listSourceSecrets(ctx context.Context) ([]*secrets.Source, error) {
	return d.listParsedSecrets(ctx, labels.RoleSource)
}

// listSourceSecretsWithMeta returns parsed sources alongside a secretName→creationTimestamp
// map so UI callers can sort by creation time without bloating secrets.Source itself.
func (d *k8sData) listSourceSecretsWithMeta(ctx context.Context) ([]*secrets.Source, map[string]time.Time, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleSource,
	}); err != nil {
		return nil, nil, err
	}
	out := make([]*secrets.Source, 0, len(list.Items))
	createdAt := make(map[string]time.Time, len(list.Items))
	for i := range list.Items {
		src, err := secrets.ParseSource(&list.Items[i], "")
		if err != nil {
			d.log.V(1).Info("skipping invalid source", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, src)
		createdAt[list.Items[i].Name] = list.Items[i].CreationTimestamp.Time
	}
	return out, createdAt, nil
}

func (d *k8sData) listDestinationSecrets(ctx context.Context) ([]*secrets.Destination, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: labels.RoleDestination,
	}); err != nil {
		return nil, err
	}
	out := make([]*secrets.Destination, 0, len(list.Items))
	for i := range list.Items {
		dest, err := secrets.ParseDestination(&list.Items[i])
		if err != nil {
			d.log.V(1).Info("skipping invalid destination", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, dest)
	}
	return out, nil
}

func (d *k8sData) listParsedSecrets(ctx context.Context, role string) ([]*secrets.Source, error) {
	var list corev1.SecretList
	if err := d.client.List(ctx, &list, client.InNamespace(d.namespace), client.MatchingLabels{
		labels.LabelRole: role,
	}); err != nil {
		return nil, err
	}
	out := make([]*secrets.Source, 0, len(list.Items))
	for i := range list.Items {
		// Default schedule is unimportant for UI display; we only consume
		// the parsed source's own metadata, never trigger runs from it.
		src, err := secrets.ParseSource(&list.Items[i], "")
		if err != nil {
			d.log.V(1).Info("skipping invalid source", "secret", list.Items[i].Name, "err", err.Error())
			continue
		}
		out = append(out, src)
	}
	return out, nil
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


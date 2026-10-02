package ui

import (
	"context"
	"sort"
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
)

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

	axis := newDayAxis(time.Now().UTC(), days)
	acc := newHeatmapAcc(axis, len(sources))
	for _, src := range sources {
		runs := d.heatmapRuns(src, secrets.FilterDestinations(src, allDests), axis)
		acc.addSource(src, runs)
	}
	return acc.summary(sources), nil
}

// dayAxis is the heatmap's day window. UTC throughout so day boundaries
// match the dump timestamp (also UTC); converting to the operator's local
// time would shift cells +/- 1 day in some zones.
type dayAxis struct {
	days        []string // YYYY-MM-DD, oldest first, today last
	earliest    string
	windowStart time.Time
}

func newDayAxis(now time.Time, days int) dayAxis {
	ax := dayAxis{days: make([]string, days)}
	for i := 0; i < days; i++ {
		ax.days[i] = now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
	}
	ax.earliest = ax.days[0]
	ax.windowStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	return ax
}

// heatmapRuns returns the run history of the first allowed destination that
// yields one. One destination is enough — every destination should hold the
// same runs modulo inconsistencies (surfaced by the consistency panel), which
// keeps the dashboard O(targets), not O(targets × destinations). Own cache
// key per day window: the heatmap only needs runs inside its window, so the
// listing is bounded with Since and the cached metas are slimmed to the
// fields the charts read; target() keeps the full history under name@dest.
func (d *k8sData) heatmapRuns(src *secrets.Source, dests []*secrets.Destination, axis dayAxis) []*meta.MetaFile {
	for _, dest := range dests {
		dest := dest
		key := src.TargetName + "@" + dest.Name + "#since" + axis.earliest
		got, _ := d.runsCache.getOrRefreshAsync(key, func() ([]*meta.MetaFile, error) {
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			st, err := d.storageFor(dest)
			if err != nil {
				return nil, err
			}
			runs, err := meta.ListWithOptions(rctx, st, src.TargetName, meta.ListOptions{Since: axis.windowStart})
			if err != nil {
				return nil, err
			}
			for _, m := range runs {
				slimForHeatmap(m)
			}
			return runs, nil
		}, d.onRefresh)
		if got != nil {
			return got
		}
	}
	return nil
}

// heatmapAcc accumulates every fleet-wide projection in one pass over each
// target's run history.
type heatmapAcc struct {
	axis              dayAxis
	rows              []heatmapRow
	bytesByDayTarget  map[string]map[string]int64 // day → target → bytes (successful runs only)
	anomalies         *boundedNewest
	verifPassed       map[string]int
	verifFailed       map[string]int
	durationsByTarget map[string][]float64
}

func newHeatmapAcc(axis dayAxis, sources int) *heatmapAcc {
	days := len(axis.days)
	acc := &heatmapAcc{
		axis:              axis,
		rows:              make([]heatmapRow, 0, sources),
		bytesByDayTarget:  make(map[string]map[string]int64, days),
		anomalies:         newBoundedNewest(maxAnomalies),
		verifPassed:       make(map[string]int, days),
		verifFailed:       make(map[string]int, days),
		durationsByTarget: make(map[string][]float64, sources),
	}
	for _, day := range axis.days {
		acc.bytesByDayTarget[day] = make(map[string]int64)
	}
	return acc
}

// addSource folds one target's runs into the heatmap row and the fleet-wide
// counters. Bytes and durations count successful runs only — failures have no
// real payload and fail fast, which would skew the median toward zero.
// Verification counts every run that carries a RestoreVerification block.
func (a *heatmapAcc) addSource(src *secrets.Source, runs []*meta.MetaFile) {
	days := len(a.axis.days)
	row := heatmapRow{Target: src.TargetName, DBType: src.DBType, Days: make([]heatmapCell, days)}
	for i, day := range a.axis.days {
		row.Days[i] = heatmapCell{Day: day, Status: "none"}
	}
	ok := make(map[string]int, days)
	failed := make(map[string]int, days)
	for _, m := range runs {
		ts := m.ParsedTimestamp()
		if ts.IsZero() {
			continue
		}
		day := ts.UTC().Format("2006-01-02")
		if day < a.axis.earliest {
			continue
		}
		if m.IsFailure() {
			failed[day]++
		} else {
			ok[day]++
			a.bytesByDayTarget[day][src.TargetName] += m.EncryptedSizeBytes
			if m.DurationSeconds > 0 { // 0 = legacy meta from before the field existed
				a.durationsByTarget[src.TargetName] = append(a.durationsByTarget[src.TargetName], m.DurationSeconds)
			}
		}
		if m.RestoreVerification != nil {
			if m.RestoreVerification.Verdict == meta.VerificationMatch {
				a.verifPassed[day]++
			} else {
				a.verifFailed[day]++
			}
		}
		a.addAnomalies(src, m, ts, day)
	}
	for i, day := range a.axis.days {
		o, f := ok[day], failed[day]
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
	a.rows = append(a.rows, row)
}

func (a *heatmapAcc) addAnomalies(src *secrets.Source, m *meta.MetaFile, ts time.Time, day string) {
	if m.Report == nil {
		return
	}
	for _, an := range m.Report.Anomalies {
		a.anomalies.add(anomalyEntry{
			Target:   src.TargetName,
			DBType:   src.DBType,
			Time:     ts.UTC().Format(time.RFC3339),
			Day:      day,
			Kind:     an.Kind,
			Subject:  an.Subject,
			Detail:   an.Detail,
			Severity: anomalySeverity(an.Kind),
		})
	}
}

// summary materialises the accumulated projections in the order the
// frontend plots them (day order, newest-first anomalies, slowest targets
// first).
func (a *heatmapAcc) summary(sources []*secrets.Source) *dashboardSummary {
	sort.Slice(a.rows, func(i, j int) bool { return a.rows[i].Target < a.rows[j].Target })

	storage := make([]storageDayPoint, len(a.axis.days))
	verification := make([]verificationDayPoint, len(a.axis.days))
	for i, day := range a.axis.days {
		storage[i] = storageDayPoint{Day: day, PerTarget: a.bytesByDayTarget[day]}
		verification[i] = verificationDayPoint{Day: day, Passed: a.verifPassed[day], Failed: a.verifFailed[day]}
	}

	// Duration stats per target — median + p95 + min + max from the sorted
	// per-target slice (bounded by Count, at most ~days for a daily backup).
	durations := make([]durationStat, 0, len(a.durationsByTarget))
	for _, src := range sources {
		vals := a.durationsByTarget[src.TargetName]
		if len(vals) == 0 {
			continue
		}
		sort.Float64s(vals)
		durations = append(durations, durationStat{
			Target: src.TargetName,
			DBType: src.DBType,
			Count:  len(vals),
			Median: vals[len(vals)/2],
			P95:    vals[int(float64(len(vals)-1)*0.95)], // rounds down so a single sample reports itself
			Min:    vals[0],
			Max:    vals[len(vals)-1],
		})
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i].Median > durations[j].Median })

	return &dashboardSummary{
		Heatmap:           a.rows,
		Storage:           storage,
		Anomalies:         a.anomalies.sorted(),
		Durations:         durations,
		VerificationDaily: verification,
	}
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

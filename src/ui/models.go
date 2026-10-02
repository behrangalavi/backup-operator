package ui

import (
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
)

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

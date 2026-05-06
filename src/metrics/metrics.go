package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// All metrics are scoped to backup target (db) and where useful, destination.
// These are the signals Alertmanager rules will fire on — keep them stable.
var (
	dumpDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backup_operator_dump_duration_seconds",
			Help:    "Time spent dumping a database (excludes encryption and upload)",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"target", "db_type"},
	)

	uploadDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backup_operator_upload_duration_seconds",
			Help:    "Time spent uploading a single dump to a single destination",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"target", "destination", "storage_type"},
	)

	runDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backup_operator_run_duration_seconds",
			Help:    "Total end-to-end backup run time including dump, upload, and retention",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200},
		},
		[]string{"target", "db_type"},
	)

	dumpSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_dump_size_bytes",
			Help: "Encrypted dump size of the most recent successful run",
		},
		[]string{"target"},
	)

	dumpSizeChangeRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_dump_size_change_ratio",
			Help: "current/previous dump size; <0.5 = suspicious shrinkage",
		},
		[]string{"target"},
	)

	tableCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_table_count",
			Help: "Number of tables/collections found in the current dump",
		},
		[]string{"target"},
	)

	tableRowCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_table_row_count",
			Help: "Row count per table at the most recent run",
		},
		[]string{"target", "table"},
	)

	schemaChanged = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_schema_changed",
			Help: "1 if the schema hash differs from the previous run, 0 otherwise",
		},
		[]string{"target"},
	)

	charsetChanged = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_charset_changed",
			Help: "1 if database character_set or collation differs from the previous run, 0 otherwise",
		},
		[]string{"target"},
	)

	schemaLastChange = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_schema_last_change_timestamp_seconds",
			Help: "Unix timestamp of the most recent run where the schema fingerprint changed (carried forward across unchanged runs)",
		},
		[]string{"target"},
	)

	// Gauges (not counters): the operator-side aggregator reconstructs these
	// from the latest meta.json found in each destination, so they reflect
	// the most recent known state rather than a monotonic count. Counters
	// would require an always-on producer; worker pods are too short-lived
	// for Prometheus to scrape, so the run that wrote them is gone before
	// the scrape arrives.
	lastRunAnomalies = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_last_run_anomalies",
			Help: "Number of analyzer anomalies recorded in the most recent run",
		},
		[]string{"target"},
	)

	lastRunStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_last_run_status",
			Help: "Outcome of the most recent run: 1 = success, 0 = failure",
		},
		[]string{"target"},
	)

	// Reconstructed by the operator-side aggregator from the latest
	// successful meta.json's DurationSeconds. The corresponding histogram
	// (run_duration_seconds) is observed by short-lived worker pods that
	// Prometheus cannot scrape, so the gauge is the only run-timing signal
	// that actually reaches Prometheus today.
	lastRunDurationSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_last_run_duration_seconds",
			Help: "Wall-clock duration of the most recent successful run, reconstructed from meta.json",
		},
		[]string{"target", "db_type"},
	)

	lastSuccessTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful run for a target/destination",
		},
		[]string{"target", "destination"},
	)

	destinationFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_destination_failed",
			Help: "1 if the most recent upload to this destination failed, 0 otherwise",
		},
		[]string{"target", "destination"},
	)

	retentionDeletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "backup_operator_retention_deleted_total",
			Help: "Objects removed by the retention policy",
		},
		[]string{"target", "destination", "kind"}, // kind = dump | meta | other
	)

	retentionFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "backup_operator_retention_failed_total",
			Help: "Retention operations that failed against a destination",
		},
		[]string{"target", "destination"},
	)

	// Storage scrub metrics — produced by the operator-side scrubber that
	// re-hashes stored dumps to detect silent corruption. The operator pod is
	// long-lived and Prometheus-scraped, so a Counter is well-defined here
	// (unlike the worker-side counters which need Gauge reconstruction).
	storageScrubPassed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_storage_scrub_passed",
			Help: "1 if the most recent storage scrub of the pair (target, destination) matched the recorded SHA256, 0 if it failed",
		},
		[]string{"target", "destination"},
	)

	storageScrubLastCheck = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_storage_scrub_last_check_timestamp_seconds",
			Help: "Unix timestamp of the most recent scrub attempt for the pair (target, destination)",
		},
		[]string{"target", "destination"},
	)

	storageScrubFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "backup_operator_storage_scrub_failed_total",
			Help: "Cumulative storage scrub failures (checksum mismatch or unreachable dump) per pair",
		},
		[]string{"target", "destination"},
	)

	// Restore-verification metrics — written by the worker (within the
	// running pod) into the meta.json sidecar, then reconstructed by the
	// operator-side MetricsRefresher into these gauges. Same pattern as
	// the analyzer fields. Mode is included as a label so dashboards can
	// distinguish stream-validate runs from later schema-only/full runs
	// without losing history when a source switches modes.
	restoreVerificationPassed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_restore_verification_passed",
			Help: "1 if the most recent restore-verification of this target succeeded, 0 if it found a mismatch, absent if never verified",
		},
		[]string{"target", "mode"},
	)

	restoreVerificationLastTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_restore_verification_last_timestamp_seconds",
			Help: "Unix timestamp of the most recent restore-verification attempt for this target",
		},
		[]string{"target", "mode"},
	)

	// Worker-only histogram (not visible to Prometheus, see CLAUDE.md §12).
	// Kept for symmetry with dump_duration / upload_duration so future
	// observability surface can read it from the worker process if needed.
	restoreVerificationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backup_operator_restore_verification_duration_seconds",
			Help:    "Time spent in the restore-verification phase",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"target", "mode"},
	)
)

// gatherer holds the same registry we registered to, so the alerts package
// can re-evaluate rule conditions in-process without each consumer needing to
// know which registry the operator uses (controller-runtime's, in main.go).
var gatherer prometheus.Gatherer

func Register(registry prometheus.Registerer) {
	registry.MustRegister(
		runDurationSeconds,
		dumpDurationSeconds,
		uploadDurationSeconds,
		dumpSizeBytes,
		dumpSizeChangeRatio,
		tableCount,
		tableRowCount,
		schemaChanged,
		charsetChanged,
		schemaLastChange,
		lastRunAnomalies,
		lastRunStatus,
		lastRunDurationSeconds,
		lastSuccessTimestamp,
		destinationFailed,
		retentionDeletedTotal,
		retentionFailedTotal,
		storageScrubPassed,
		storageScrubLastCheck,
		storageScrubFailedTotal,
		restoreVerificationPassed,
		restoreVerificationLastTimestamp,
		restoreVerificationDuration,
	)
	if g, ok := registry.(prometheus.Gatherer); ok {
		gatherer = g
	}
}

// Gatherer returns the registry we registered to, or nil if Register has
// not been called (tests that bypass main wiring).
func Gatherer() prometheus.Gatherer { return gatherer }

func IncRetentionDeleted(target, destination, kind string) {
	retentionDeletedTotal.WithLabelValues(target, destination, kind).Inc()
}

func IncRetentionFailure(target, destination string) {
	retentionFailedTotal.WithLabelValues(target, destination).Inc()
}

func ObserveRunDuration(target, dbType string, d time.Duration) {
	runDurationSeconds.WithLabelValues(target, dbType).Observe(d.Seconds())
}

func ObserveDumpDuration(target, dbType string, d time.Duration) {
	dumpDurationSeconds.WithLabelValues(target, dbType).Observe(d.Seconds())
}

func ObserveUploadDuration(target, destination, storageType string, d time.Duration) {
	uploadDurationSeconds.WithLabelValues(target, destination, storageType).Observe(d.Seconds())
}

func SetDumpSize(target string, bytes int64) {
	dumpSizeBytes.WithLabelValues(target).Set(float64(bytes))
}

func SetDumpSizeChangeRatio(target string, ratio float64) {
	dumpSizeChangeRatio.WithLabelValues(target).Set(ratio)
}

func SetTableCount(target string, count int) {
	tableCount.WithLabelValues(target).Set(float64(count))
}

func SetTableRowCount(target, table string, rows int64) {
	tableRowCount.WithLabelValues(target, table).Set(float64(rows))
}

func SetSchemaChanged(target string, changed bool) {
	v := 0.0
	if changed {
		v = 1.0
	}
	schemaChanged.WithLabelValues(target).Set(v)
}

func SetCharsetChanged(target string, changed bool) {
	v := 0.0
	if changed {
		v = 1.0
	}
	charsetChanged.WithLabelValues(target).Set(v)
}

func SetSchemaLastChange(target string, t time.Time) {
	if t.IsZero() {
		return
	}
	schemaLastChange.WithLabelValues(target).Set(float64(t.Unix()))
}

func SetLastRunAnomalies(target string, count int) {
	lastRunAnomalies.WithLabelValues(target).Set(float64(count))
}

func SetLastRunStatus(target string, success bool) {
	v := 0.0
	if success {
		v = 1.0
	}
	lastRunStatus.WithLabelValues(target).Set(v)
}

func SetLastRunDuration(target, dbType string, d time.Duration) {
	if d <= 0 {
		return
	}
	lastRunDurationSeconds.WithLabelValues(target, dbType).Set(d.Seconds())
}

func SetLastSuccess(target, destination string, t time.Time) {
	lastSuccessTimestamp.WithLabelValues(target, destination).Set(float64(t.Unix()))
}

func SetDestinationFailed(target, destination string, failed bool) {
	v := 0.0
	if failed {
		v = 1.0
	}
	destinationFailed.WithLabelValues(target, destination).Set(v)
}

func SetStorageScrubPassed(target, destination string, passed bool) {
	v := 0.0
	if passed {
		v = 1.0
	}
	storageScrubPassed.WithLabelValues(target, destination).Set(v)
}

func SetStorageScrubLastCheck(target, destination string, t time.Time) {
	storageScrubLastCheck.WithLabelValues(target, destination).Set(float64(t.Unix()))
}

func IncStorageScrubFailed(target, destination string) {
	storageScrubFailedTotal.WithLabelValues(target, destination).Inc()
}

func SetRestoreVerificationPassed(target, mode string, passed bool) {
	v := 0.0
	if passed {
		v = 1.0
	}
	restoreVerificationPassed.WithLabelValues(target, mode).Set(v)
}

func SetRestoreVerificationLastTimestamp(target, mode string, t time.Time) {
	if t.IsZero() {
		return
	}
	restoreVerificationLastTimestamp.WithLabelValues(target, mode).Set(float64(t.Unix()))
}

func ObserveRestoreVerificationDuration(target, mode string, d time.Duration) {
	restoreVerificationDuration.WithLabelValues(target, mode).Observe(d.Seconds())
}

func DeleteTargetMetrics(target string) {
	dumpSizeBytes.DeleteLabelValues(target)
	dumpSizeChangeRatio.DeleteLabelValues(target)
	tableCount.DeleteLabelValues(target)
	schemaChanged.DeleteLabelValues(target)
	charsetChanged.DeleteLabelValues(target)
	schemaLastChange.DeleteLabelValues(target)
	tableRowCount.DeletePartialMatch(prometheus.Labels{"target": target})
	lastSuccessTimestamp.DeletePartialMatch(prometheus.Labels{"target": target})
	destinationFailed.DeletePartialMatch(prometheus.Labels{"target": target})
	lastRunStatus.DeleteLabelValues(target)
	lastRunAnomalies.DeleteLabelValues(target)
	lastRunDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	runDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	dumpDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	uploadDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubPassed.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubLastCheck.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubFailedTotal.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationPassed.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationLastTimestamp.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationDuration.DeletePartialMatch(prometheus.Labels{"target": target})
}

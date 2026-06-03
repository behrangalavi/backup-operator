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

	// Reconstructed from meta.json's DumpDurationSeconds. The
	// corresponding histogram (dump_duration_seconds) lives in the
	// short-lived worker process and is never scraped by Prometheus.
	lastDumpDurationSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_last_dump_duration_seconds",
			Help: "Duration of the dump phase in the most recent successful run, reconstructed from meta.json",
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

	// Set to 1 when the worker tried to load a previous-run meta as the
	// analyzer baseline but every destination errored before any could be
	// read. Distinguishes "first run, nothing to compare" (gauge stays 0)
	// from "all destinations broken, analyzer is running blind" (gauge=1).
	// Without this signal, a fleet-wide storage outage degrades semantic
	// alerting silently — the run still succeeds, dumps still upload, but
	// schema/size drift detection is dark.
	analyzerBaselineUnavailable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_analyzer_baseline_unavailable",
			Help: "1 if every destination failed when loading the analyzer baseline; 0 otherwise (including first run)",
		},
		[]string{"target"},
	)

	destinationFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_destination_failed",
			Help: "1 if the most recent upload to this destination failed, 0 otherwise",
		},
		[]string{"target", "destination"},
	)

	// Retention is reconstructed from the latest meta.json's pre-upload
	// sweep results, same operator-side aggregation pattern as the rest of
	// the run-level signals. The previous Counter pair was worker-only
	// dead code: short-lived workers can't be scraped, so retention
	// failures were invisible until storage filled up.
	retentionLastStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_retention_last_status",
			Help: "1 if the most recent pre-upload retention sweep for this pair succeeded, 0 otherwise",
		},
		[]string{"target", "destination"},
	)

	retentionLastDeleted = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_operator_retention_last_deleted_count",
			Help: "Number of dumps deleted by the most recent retention sweep (excludes meta sidecars)",
		},
		[]string{"target", "destination"},
	)

	sourceParseErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "backup_operator_source_parse_errors_total",
			Help: "Source Secrets that failed annotation/data parsing during reconciliation",
		},
		[]string{"secret"},
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

	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backup_operator_http_request_duration_seconds",
			Help:    "Latency of HTTP requests served by the UI/API server",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path", "status"},
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
		lastDumpDurationSeconds,
		lastSuccessTimestamp,
		analyzerBaselineUnavailable,
		destinationFailed,
		retentionLastStatus,
		retentionLastDeleted,
		storageScrubPassed,
		storageScrubLastCheck,
		storageScrubFailedTotal,
		sourceParseErrors,
		restoreVerificationPassed,
		restoreVerificationLastTimestamp,
		restoreVerificationDuration,
		HTTPRequestDuration,
	)
	if g, ok := registry.(prometheus.Gatherer); ok {
		gatherer = g
	}
}

// Gatherer returns the registry we registered to, or nil if Register has
// not been called (tests that bypass main wiring).
func Gatherer() prometheus.Gatherer { return gatherer }

func SetRetentionLastStatus(target, destination string, ok bool) {
	v := 0.0
	if ok {
		v = 1.0
	}
	retentionLastStatus.WithLabelValues(target, destination).Set(v)
}

func SetRetentionLastDeleted(target, destination string, count int) {
	retentionLastDeleted.WithLabelValues(target, destination).Set(float64(count))
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

func SetLastDumpDuration(target, dbType string, d time.Duration) {
	if d <= 0 {
		return
	}
	lastDumpDurationSeconds.WithLabelValues(target, dbType).Set(d.Seconds())
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

func SetAnalyzerBaselineUnavailable(target string, unavailable bool) {
	v := 0.0
	if unavailable {
		v = 1.0
	}
	analyzerBaselineUnavailable.WithLabelValues(target).Set(v)
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

func IncSourceParseError(secret string) {
	sourceParseErrors.WithLabelValues(secret).Inc()
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

// DeleteDestinationMetrics removes every series keyed by the (target,
// destination) pair. Called when a destination drops out of a source's
// allow-list so its gauges go absent instead of sticking at the last value
// the destination ever reported.
func DeleteDestinationMetrics(target, destination string) {
	lastSuccessTimestamp.DeleteLabelValues(target, destination)
	destinationFailed.DeleteLabelValues(target, destination)
	retentionLastStatus.DeleteLabelValues(target, destination)
	retentionLastDeleted.DeleteLabelValues(target, destination)
	storageScrubPassed.DeleteLabelValues(target, destination)
	storageScrubLastCheck.DeleteLabelValues(target, destination)
	storageScrubFailedTotal.DeleteLabelValues(target, destination)
}

// DeleteTableMetric removes the per-table row-count series for a table that no
// longer exists in the source schema, so a dropped table stops reporting its
// last-known row count forever.
func DeleteTableMetric(target, table string) {
	tableRowCount.DeleteLabelValues(target, table)
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
	analyzerBaselineUnavailable.DeleteLabelValues(target)
	lastRunDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	lastDumpDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	runDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	dumpDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	uploadDurationSeconds.DeletePartialMatch(prometheus.Labels{"target": target})
	retentionLastStatus.DeletePartialMatch(prometheus.Labels{"target": target})
	retentionLastDeleted.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubPassed.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubLastCheck.DeletePartialMatch(prometheus.Labels{"target": target})
	storageScrubFailedTotal.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationPassed.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationLastTimestamp.DeletePartialMatch(prometheus.Labels{"target": target})
	restoreVerificationDuration.DeletePartialMatch(prometheus.Labels{"target": target})
}

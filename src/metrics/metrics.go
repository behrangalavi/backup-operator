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
)

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
		lastRunAnomalies,
		lastRunStatus,
		lastSuccessTimestamp,
		destinationFailed,
		retentionDeletedTotal,
		retentionFailedTotal,
	)
}

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

func DeleteTargetMetrics(target string) {
	dumpSizeBytes.DeleteLabelValues(target)
	dumpSizeChangeRatio.DeleteLabelValues(target)
	tableCount.DeleteLabelValues(target)
	schemaChanged.DeleteLabelValues(target)
	tableRowCount.DeletePartialMatch(prometheus.Labels{"target": target})
	lastSuccessTimestamp.DeletePartialMatch(prometheus.Labels{"target": target})
	destinationFailed.DeletePartialMatch(prometheus.Labels{"target": target})
	lastRunStatus.DeleteLabelValues(target)
	lastRunAnomalies.DeleteLabelValues(target)
}

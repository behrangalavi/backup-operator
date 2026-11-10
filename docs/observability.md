# Metrics and Alerts

## 12. Metrics Catalog

Exposed by the operator pod on `:8080/metrics`. **Worker pods are short-lived** — Prometheus cannot scrape them in time, so the run-level metrics are reconstructed by the operator's `MetricsRefresher` (`controllers/metrics_refresher.go`). It runs on a tick (default 30s, see `METRICS_REFRESH_INTERVAL_SECONDS`), lists the `BackupSource`s in the watch namespace, lists each destination root once per tick (`dest_listing.go`) to find the newest `*.meta.json` per target, and writes the result into the operator's local Prometheus registry. That is why everything below is a Gauge — counters would require an always-on producer the worker cannot provide.

The histograms (`dump_duration_seconds`, `upload_duration_seconds`, `run_duration_seconds`) are kept in the worker for code-coupling reasons but their samples never reach Prometheus. For run-level timing, the operator-side aggregator now reconstructs `last_run_duration_seconds` as a Gauge from each successful run's meta.json — that is the metric to query for trend alerts. The dump and per-destination upload phase histograms remain a known gap; if you need per-phase distribution, see the [§18](adr/README.md) ADR on OTel as a future second-track export.

Metric names are exported constants (`metrics.Name*`); the local alert evaluator and the UI's scrub-status reader match on those constants, never on string literals, so a rename cannot silently break a consumer.

`metrics.Register(reg)` also stashes the registry as a `prometheus.Gatherer` (`metrics.Gatherer()`), which the alerts package's `LocalProvider` reads to re-evaluate the chart's PrometheusRule conditions inline — this is the no-Prometheus-configured fallback path for the UI's `/api/alerts` endpoint (see [§13](#13-default-alert-rules) and the [§18](adr/README.md) ADR on alerts providers).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backup_operator_dump_duration_seconds` | Histogram | `target`, `db_type` | Worker-only; not visible to Prometheus (see note above) |
| `backup_operator_upload_duration_seconds` | Histogram | `target`, `destination`, `storage_type` | Worker-only; not visible to Prometheus (see note above) |
| `backup_operator_run_duration_seconds` | Histogram | `target`, `db_type` | Total end-to-end backup run time including dump, upload, and retention. Observed via deferred call so both success and failure paths are captured. Worker-only; not visible to Prometheus (see note above) |
| `backup_operator_dump_size_bytes` | Gauge | `target` | Encrypted size of the most recent meta.json's dump |
| `backup_operator_dump_size_change_ratio` | Gauge | `target` | current / previous size from the latest meta.json's report; <0.5 = suspicious shrinkage |
| `backup_operator_table_count` | Gauge | `target` | Tables/collections in the most recent run's stats |
| `backup_operator_table_row_count` | Gauge | `target`, `table` | Per-table row count (estimate) from the most recent run |
| `backup_operator_schema_changed` | Gauge | `target` | 1 if schema hash differs from the previous run |
| `backup_operator_charset_changed` | Gauge | `target` | 1 if the database's character_set or collation differs from the previous run |
| `backup_operator_schema_last_change_timestamp_seconds` | Gauge | `target` | Unix ts of the most recent run where the schema fingerprint actually changed. Carried forward across unchanged runs. Lets queries flag "schema older than N days" |
| `backup_operator_last_run_anomalies` | Gauge | `target` | Analyzer anomaly count in the most recent run |
| `backup_operator_last_run_status` | Gauge | `target` | 1 = most recent run produced a meta.json, 0 otherwise |
| `backup_operator_last_run_duration_seconds` | Gauge | `target`, `db_type` | Wall-clock duration of the most recent **successful** run, reconstructed from `meta.json`'s `durationSeconds`. Failed runs are excluded (they fail fast and would systematically underestimate). The corresponding histogram (`run_duration_seconds`) is observed by the worker but never reaches Prometheus, so this gauge is the only run-timing signal scrape can see. Trade-off: no distribution (P95/P99); use `avg_over_time` / `quantile_over_time` over the gauge for trend analysis. |
| `backup_operator_last_dump_duration_seconds` | Gauge | `target`, `db_type` | Duration of the **dump phase alone** of the most recent successful run, reconstructed from `meta.json`'s `dumpDurationSeconds`. Same operator-side reconstruction as `last_run_duration_seconds`; lets you separate dump time from upload/retention time. The worker-side `dump_duration_seconds` histogram is never scraped. |
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts parsed from the most recent meta.json found at that destination |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if the destination's storage cannot be initialised **or cannot be reached/read at refresh time** (offline host, rotated credentials, unreadable meta), 0 once the destination is reachable (a meta.json was read, or the target prefix listed cleanly with nothing uploaded yet) |
| `backup_operator_analyzer_baseline_unavailable` | Gauge | `target` | 1 when the worker tried to read the analyzer baseline but every destination errored before any could be read; 0 otherwise (including the legitimate first-run case where no baseline exists yet). Distinguishes "first run, nothing to compare" from "fleet-wide storage outage, analyzer running blind". |
| `backup_operator_retention_last_status` | Gauge | `target`, `destination` | 1 if the most recent pre-upload retention sweep for this pair succeeded, 0 otherwise. Reconstructed by the operator from the `retention` block in `meta.json`. Absent until at least one sweep has run. Drives the `BackupRetentionFailing` alert. |
| `backup_operator_retention_last_deleted_count` | Gauge | `target`, `destination` | Number of dump artifacts deleted by the most recent sweep (excludes meta sidecars). Lets dashboards show "we are actively trimming" vs. "retention runs but nothing to do". |
| `backup_operator_storage_scrub_passed` | Gauge | `target`, `destination` | 1 if the most recent scrub of this pair matched the recorded SHA256, 0 otherwise. Only present when `STORAGE_SCRUB_ENABLED=true` and at least one scrub has run. |
| `backup_operator_storage_scrub_last_check_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts of the most recent scrub attempt for this pair |
| `backup_operator_storage_scrub_failed_total` | Counter | `target`, `destination` | Cumulative scrub failures (mismatch or unreachable). Operator-side, scraped normally. |
| `backup_operator_restore_verification_passed` | Gauge | `target`, `mode` | 1 if the most recent restore-verifier run for this target+mode produced verdict `match`, 0 if `mismatch`/`skipped`. Absent until a verifier has run at least once — present-with-zero is meaningful, absent is "not configured / not yet due". |
| `backup_operator_restore_verification_last_timestamp_seconds` | Gauge | `target`, `mode` | Unix ts of the most recent restore-verifier completion for this pair. Drives the `BackupRestoreVerificationStale` alert. |
| `backup_operator_restore_verification_duration_seconds` | Histogram | `target`, `mode` | Worker-only; not visible to Prometheus (same as the dump/upload histograms). |
| `backup_operator_source_parse_errors_total` | Counter | `source` | BackupSources whose spec could not be flattened into a runnable source (unsafe `targetName`, missing host, unresolvable username). Incremented by both the CronJob reconciler and the MetricsRefresher. Operator-side, scraped normally. Most schema violations are already rejected at admission, so this fires rarely; the refresher keeps a target's other series sticky across a transient flatten error, so this counter (and the `Ready=False` condition) is the signal that an object is misconfigured even while its gauges look healthy. |
| `backup_operator_http_request_duration_seconds` | Histogram | `method`, `path`, `status` | UI HTTP request latency. `path` is normalised to a closed label set (`normalizeMetricPath`) — parametric routes collapse to their prefix, unknown paths to `"other"` — so an unauthenticated path-scanner can't blow up series cardinality. |

---

## 13. Default Alert Rules

Shipped in the Helm chart's `values.yaml` under `prometheusRule.rules`. The chart only renders the `PrometheusRule` if `monitoring.coreos.com/v1` is present (i.e. Prometheus Operator installed). Override at install time to fit your environment.

| Alert | Expr (simplified) | Severity |
|---|---|---|
| `BackupOverdue` | last success >36h | warning |
| `BackupDestinationFailing` | `destination_failed == 1` for 15m | warning |
| `BackupDumpSizeCollapsed` | `dump_size_change_ratio < 0.5` for 5m | **critical** |
| `BackupSchemaChanged` | `schema_changed == 1` | info |
| `BackupCharsetChanged` | `charset_changed == 1` | warning |
| `BackupStorageCorrupted` | `storage_scrub_passed == 0` for 15m | **critical** |
| `BackupRetentionFailing` | `retention_last_status == 0` for 24h | warning |
| `BackupAnomaliesAppearing` | `last_run_anomalies > 0` for 5m | warning |
| `BackupLastRunFailed` | `last_run_status == 0` for 5m | warning |
| `BackupSucceeded` | `time() - last_success_timestamp_seconds < 120` | info |
| `BackupRestoreVerificationFailed` | `restore_verification_passed == 0` for 5m | **critical** |
| `BackupRestoreVerificationStale` | `time() - restore_verification_last_timestamp > 14d` | warning |

`BackupSucceeded` is a heartbeat-style positive signal (firing + resolved per run) — useful when you want a notification on every successful backup, but expect one firing + one resolved mail per completed run per target. With a frequent cron (e.g. every 5 min in the test stack), this is intentionally noisy.

The semantic alerts (`DumpSizeCollapsed`, `SchemaChanged`, `AnomaliesAppearing`) are the project's main differentiator vs K8up — they alert on *content*, not just job exit code.

These same conditions also surface in the operator UI under `/api/alerts` and `#/alerts` — Prometheus-backed when `PROMETHEUS_URL` is set, otherwise re-evaluated locally over the registry returned by `metrics.Gatherer()`. The local path does NOT honour `for:` debounce; treat the in-UI list as advisory and Alertmanager as the audit-grade source. See the [§18](adr/README.md) ADR on alert providers.

---

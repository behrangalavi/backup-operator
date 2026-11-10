# Architectural Decision Records

One file per decision, numbered in the order they were recorded. Each one
keeps the *reason* — what was considered, what was rejected and why — so a
later change can tell whether the original constraint still holds.

Adding one: copy the shape of an existing file (`# NNNN. Title`, a
`**Status:**` line, then the reasoning), take the next free number and add a
row below. When a decision is replaced, do not delete it: set its status to
`Superseded` and link the replacement.

## Pre-CRD names

Decisions recorded before [0001](0001-crds-after-all-backupsource-backupdestination-backuprecipien.md)
talk about labelled Secrets and `backup.mogenius.io/<key>` annotations. The
reasoning still applies; the names moved:

| Legacy (labelled Secret) | Now |
|---|---|
| Secret labelled `backup.mogenius.io/role=source` | `BackupSource` + credentials Secret (`spec.database.credentialsSecretRef`) |
| Secret labelled `role=destination` | `BackupDestination` + credentials Secret (`spec.credentialsSecretRef`) |
| Secret labelled `role=age-recipient` | `BackupRecipient` (`spec.publicKey`) |
| `backup.mogenius.io/name` | `spec.targetName` (default `metadata.name`) |
| `schedule` | `spec.schedule` |
| `suspended` | `spec.suspended` |
| `jitter-minutes` | `spec.jitterMinutes` |
| `destinations` | `spec.destinations` |
| `retention-days` / `min-keep` | `spec.retention.days` / `spec.retention.minKeep` |
| `compression` | `spec.compression` |
| `analyzer-enabled`, `anonymize-tables`, `empty-dump-check` | `spec.analyzer.enabled`, `.anonymizeTables`, `.emptyDumpCheck` |
| `row-drop-threshold` / `size-drop-threshold` (ratio) | `spec.analyzer.rowDropThresholdPercent` / `.sizeDropThresholdPercent` (percent) |
| `restore-verification-mode`, `-interval`, `verification-image`, `verification-volume-size` | `spec.restoreVerification.mode`, `.interval`, `.image`, `.volumeSize` |
| `extra-sslmode` / `extra-tls` | `spec.database.tls.mode` |
| `extra-sslrootcert` / `extra-ssl-ca` (file path) | `spec.database.tls.caSecretRef` (Secret, materialised by the worker) |
| other `extra-<key>` | `spec.database.extra.<key>` |
| destination `path-prefix` | `BackupDestination.spec.pathPrefix` |
| destination `known-hosts`, `insecure-skip-host-verify` | `spec.sftp.knownHosts`, `spec.sftp.insecureSkipHostVerify` |
| "the source Secret" as Event target | the `BackupSource` (`kubectl describe backupsource <name>`) |

## Index

| # | Decision | Status |
|---|---|---|
| [0001](0001-crds-after-all-backupsource-backupdestination-backuprecipien.md) | CRDs after all: `BackupSource` / `BackupDestination` / `BackupRecipient`, credentials stay in Secrets | Accepted |
| [0002](0002-chart-ships-crds-as-templates-with-resource-policy-keep-and.md) | Chart ships CRDs as templates with `resource-policy: keep`, and ships no CRs — recipients are seeded by the operator | Accepted (pre-CRD wording) |
| [0003](0003-backupsource-status-is-written-by-the-operator-only-change-o.md) | `BackupSource.status` is written by the operator only, change-only | Accepted |
| [0004](0004-recipientreconciler-watches-backuprecipient-only-merged-secr.md) | RecipientReconciler watches `BackupRecipient` only; merged-Secret drift is repaired by requeue, not by a Secret watch | Accepted |
| [0005](0005-operator-does-not-run-backups.md) | Operator does not run backups | Accepted |
| [0006](0006-ownerreference-for-cascade-delete.md) | OwnerReference for cascade delete | Accepted |
| [0007](0007-label-transition-predicate.md) | Label-transition predicate | Superseded |
| [0008](0008-cronjob-deletion-is-ownership-checked-not-name-only.md) | CronJob deletion is ownership-checked, not name-only | Superseded |
| [0009](0009-stats-from-live-db-not-parsed-dump.md) | Stats from live DB, not parsed dump | Accepted |
| [0010](0010-sidecar-meta-json-is-unencrypted.md) | Sidecar `meta.json` is unencrypted | Accepted |
| [0011](0011-single-dump-fan-out-via-temp-file.md) | Single dump → fan-out via temp file | Accepted |
| [0012](0012-storage-list-returns-logical-paths.md) | `Storage.List()` returns logical paths | Accepted |
| [0013](0013-superseded-by-backuprecipient-kept-for-the-reasoning-per-rec.md) | (Superseded by `BackupRecipient`, kept for the reasoning.) Per-recipient Secrets via `role=age-recipient`, materialised by an operator reconciler | Superseded |
| [0014](0014-agepublickeys-accepts-both-string-and-array-in-helm-values-n.md) | `agePublicKeys` accepts both string and array in Helm values, normalised to newline-separated env-var | Accepted |
| [0015](0015-age-over-gpg.md) | `age` over GPG | Accepted |
| [0016](0016-no-notifier-built-in.md) | No notifier built-in | Accepted |
| [0017](0017-three-binaries-one-image.md) | Three binaries, one image | Accepted |
| [0018](0018-restore-is-a-separate-binary.md) | Restore is a separate binary | Accepted |
| [0019](0019-canonical-metafile-in-internal-meta-not-per-consumer-copies.md) | Canonical `MetaFile` in `internal/meta`, not per-consumer copies | Accepted |
| [0020](0020-metrics-package-not-metricstore.md) | `metrics` package, not `metricStore` | Accepted |
| [0021](0021-batchstorage-interface-for-connection-reuse.md) | `BatchStorage` interface for connection reuse | Accepted |
| [0022](0022-structured-error-types-in-the-pipeline.md) | Structured error types in the pipeline | Accepted |
| [0023](0023-post-upload-size-verification.md) | Post-upload size verification | Accepted |
| [0024](0024-psa-restricted-securitycontext-on-all-pods.md) | PSA-restricted SecurityContext on all pods | Accepted |
| [0025](0025-exponential-retry-on-transient-upload-failures.md) | Exponential retry on transient upload failures | Accepted |
| [0026](0026-ssh-handshake-timeout.md) | SSH handshake timeout | Accepted |
| [0027](0027-ftps-as-a-separate-driver-not-sftp-with-a-tls-toggle.md) | FTPS as a separate driver, not "SFTP with a TLS toggle" | Accepted |
| [0028](0028-worker-resource-limits-via-env-vars.md) | Worker resource limits via env vars | Accepted |
| [0029](0029-k8s-native-job-retries-via-worker-backoff-limit-default-2.md) | K8s-native Job retries via `WORKER_BACKOFF_LIMIT` (default 2) | Accepted |
| [0030](0030-kubernetes-events-as-audit-trail.md) | Kubernetes Events as audit trail | Accepted (pre-CRD wording) |
| [0031](0031-health-probes-on-the-operator.md) | Health probes on the operator | Accepted |
| [0032](0032-operator-side-metric-aggregation-not-pushgateway.md) | Operator-side metric aggregation, not Pushgateway | Accepted |
| [0033](0033-retention-status-as-gauge-from-meta-json-not-the-worker-side.md) | Retention status as Gauge from meta.json, not the worker-side Counter pair | Accepted (pre-CRD wording) |
| [0034](0034-per-destination-and-per-table-series-go-absent-on-removal-no.md) | Per-destination and per-table series go absent on removal, not sticky | Accepted (pre-CRD wording) |
| [0035](0035-run-duration-as-a-gauge-from-meta-json-not-as-a-histogram-vi.md) | Run duration as a Gauge from meta.json, not as a histogram via Pushgateway | Accepted |
| [0036](0036-separate-serviceaccounts-for-operator-and-worker.md) | Separate ServiceAccounts for operator and worker | Accepted (pre-CRD wording) |
| [0037](0037-writable-tmp-via-emptydir-not-relaxing-readonlyrootfilesyste.md) | Writable `/tmp` via emptyDir, not relaxing `readOnlyRootFilesystem` | Accepted |
| [0038](0038-eventbroadcaster-shutdown-before-exit.md) | EventBroadcaster shutdown before exit | Accepted |
| [0039](0039-signal-aware-context-for-graceful-shutdown.md) | Signal-aware context for graceful shutdown | Accepted |
| [0040](0040-upload-concurrency-semaphore.md) | Upload concurrency semaphore | Accepted |
| [0041](0041-poddisruptionbudget-for-the-operator.md) | PodDisruptionBudget for the operator | Accepted |
| [0042](0042-ui-error-sanitization.md) | UI error sanitization | Accepted |
| [0043](0043-two-tier-error-sanitization-fully-generic-vs-credential-scru.md) | Two-tier error sanitization: fully-generic vs credential-scrubbed | Accepted |
| [0044](0044-sha256-checksum-in-meta-json.md) | SHA256 checksum in meta.json | Accepted |
| [0045](0045-metricsrefresher-long-lived-storage-pool-two-tier-concurrenc.md) | MetricsRefresher: long-lived storage pool + two-tier concurrency | Accepted |
| [0046](0046-pool-lazy-init-guarded-by-sync-once-sftp-keepalive-goroutine.md) | Pool lazy-init guarded by `sync.Once`; SFTP keepalive goroutine bound to connection lifetime | Accepted |
| [0047](0047-metricsrefresher-scrubber-hot-path-reuses-one-sftp-ftps-sess.md) | MetricsRefresher/scrubber hot path reuses one SFTP/FTPS session for List+Get via `WithSession` | Accepted |
| [0048](0048-no-f-sync-on-the-dump-temp-file.md) | No `f.Sync()` on the dump temp file | Accepted |
| [0049](0049-bounded-ui-background-refresh-fan-out-via-a-shared-semaphore.md) | Bounded UI background-refresh fan-out via a shared semaphore | Accepted |
| [0050](0050-post-dump-collectstats-only-for-engines-without-a-row-counte.md) | Post-dump `CollectStats` only for engines without a row counter (mongo/redis) | Accepted |
| [0051](0051-schedule-jitter-at-cronjob-templating-not-as-window-start-wi.md) | Schedule jitter at CronJob templating, not as `window-start`/`window-end` annotations | Accepted (pre-CRD wording) |
| [0052](0052-table-name-anonymization.md) | Table-name anonymization | Accepted (pre-CRD wording) |
| [0053](0053-purge-cli-for-dsgvo-art-17.md) | Purge CLI for DSGVO Art. 17 | Accepted |
| [0054](0054-image-digest-pinning.md) | Image digest pinning | Accepted |
| [0055](0055-optional-networkpolicy-for-operator-pod.md) | Optional NetworkPolicy for operator pod | Accepted |
| [0056](0056-emptydir-mount-at-configured-temp-dir.md) | EmptyDir mount at configured TEMP_DIR | Accepted |
| [0057](0057-vanilla-js-spa-over-react-vue.md) | Vanilla JS SPA over React/Vue | Accepted |
| [0058](0058-sse-over-websocket-for-live-updates.md) | SSE over WebSocket for live updates | Accepted |
| [0059](0059-ui-crud-via-the-kubernetes-api-not-a-database.md) | UI CRUD via the Kubernetes API, not a database | Accepted |
| [0060](0060-sensitive-field-masking-in-api-get-responses.md) | Sensitive field masking in API GET responses | Accepted |
| [0061](0061-csrf-defence-on-mutating-ui-endpoints-via-csrfmiddleware.md) | CSRF defence on mutating UI endpoints via `csrfMiddleware` | Accepted |
| [0062](0062-manual-trigger-creates-job-from-cronjob-spec.md) | Manual trigger creates Job from CronJob spec | Accepted |
| [0063](0063-sse-periodic-refresh-broadcast.md) | SSE periodic refresh broadcast | Accepted |
| [0064](0064-legacy-template-routes-preserved.md) | Legacy template routes preserved | Accepted |
| [0065](0065-settings-page-is-read-only-fleet-defaults-have-one-source-of.md) | Settings page is read-only; fleet defaults have one source of truth (the chart) | Accepted |
| [0066](0066-crud-endpoints-address-typed-kinds-so-the-is-this-really-a-b.md) | CRUD endpoints address typed kinds, so the "is this really a backup Secret" check is gone | Accepted (pre-CRD wording) |
| [0067](0067-two-phase-fan-out-with-per-destination-result-tracking.md) | Two-phase fan-out with per-destination result tracking | Accepted |
| [0068](0068-run-history-merged-from-all-destinations.md) | Run history merged from ALL destinations | Accepted |
| [0069](0069-multi-storage-enterprise-api-endpoints.md) | Multi-storage enterprise API endpoints | Accepted |
| [0070](0070-db-credentials-never-appear-on-dumper-command-lines.md) | DB credentials never appear on dumper command lines | Accepted |
| [0071](0071-centralised-error-sanitisation-in-dumper-sanitizestderr-wrap.md) | Centralised error sanitisation in `dumper.SanitizeStderr` / `WrapExecError` | Accepted |
| [0072](0072-ui-body-size-cap-and-sse-client-cap.md) | UI body-size cap and SSE client cap | Accepted |
| [0073](0073-bounded-path-label-on-the-request-duration-histogram-normali.md) | Bounded `path` label on the request-duration histogram (`normalizeMetricPath`) | Accepted |
| [0074](0074-alerts-surfaced-via-two-independent-providers.md) | Alerts surfaced via two independent providers | Accepted (pre-CRD wording) |
| [0075](0075-helm-chart-sets-release-kube-prometheus-stack-by-default.md) | Helm chart sets `release: kube-prometheus-stack` by default | Accepted |
| [0076](0076-pre-upload-retention-sweep.md) | Pre-upload retention sweep | Accepted |
| [0077](0077-upload-error-classification-via-classifyuploaderror.md) | Upload error classification via `classifyUploadError` | Accepted |
| [0078](0078-cronjob-hash-suffix-naming.md) | CronJob hash-suffix naming | Accepted (pre-CRD wording) |
| [0079](0079-sftp-partial-file-cleanup-on-upload-failure.md) | SFTP partial file cleanup on upload failure | Accepted |
| [0080](0080-retention-cleans-empty-ancestor-directories.md) | Retention cleans empty ancestor directories | Accepted |
| [0081](0081-operator-now-calls-alertmanager-directly-for-status-checks-a.md) | Operator now calls Alertmanager directly for status checks and test alerts | Accepted |
| [0082](0082-empty-dump-hard-fail-driven-by-row-counter-not-byte-count.md) | Empty-dump hard-fail driven by row counter, not byte count | Accepted (pre-CRD wording) |
| [0083](0083-row-counter-scan-errors-fall-back-to-stats-never-hard-fail-t.md) | Row-counter scan errors fall back to stats, never hard-fail the dump | Accepted |
| [0084](0084-dual-path-empty-dump-detection-row-counter-for-sql-size-heur.md) | Dual-path empty-dump detection: row counter for SQL, size heuristic for mongo/redis | Accepted (pre-CRD wording) |
| [0085](0085-mysqldump-flag-hardening-events-default-character-set-utf8mb.md) | mysqldump flag hardening: `--events --default-character-set=utf8mb4` always, `--column-statistics=0` only when the binary is Oracle's mysqldump | Accepted |
| [0086](0086-one-mysql-protocol-dump-binary-per-arch-oracle-s-mysqldump-o.md) | One mysql-protocol dump binary per arch: Oracle's `mysqldump` on amd64, `mariadb-dump` on arm64 | Accepted |
| [0087](0087-pg-dump-from-pgdg-not-debian-bookworm.md) | pg_dump from PGDG, not debian-bookworm | Accepted |
| [0088](0088-charset-collation-captured-into-dumper-stats-and-analyzed-fo.md) | Charset/collation captured into `dumper.Stats` and analyzed for drift | Accepted |
| [0089](0089-storage-scrubber-re-hashes-stored-dumps-periodically.md) | Storage scrubber re-hashes stored dumps periodically | Accepted |
| [0090](0090-schema-change-timestamp-carried-forward-across-runs.md) | Schema-change timestamp carried forward across runs | Accepted |
| [0091](0091-restore-verification-phase-2-spawns-the-ephemeral-db-pod-fro.md) | Restore-verification Phase 2 spawns the ephemeral DB pod from the worker, owned by the worker pod's UID | Accepted |
| [0092](0092-read-only-docs-portal-on-its-own-port-8083.md) | Read-only docs portal on its own port (`:8083`) | Accepted |
| [0093](0093-job-duration-estimate-from-past-meta-json-not-prometheus.md) | Job duration estimate from past meta.json, not Prometheus | Accepted |
| [0094](0094-restore-verification-uses-an-ephemeral-keypair-generated-ins.md) | Restore-verification uses an ephemeral keypair generated inside the worker pod | Accepted |
| [0095](0095-stream-validate-uses-engine-specific-parsing-not-byte-equali.md) | stream-validate uses engine-specific parsing, not byte equality | Accepted |
| [0096](0096-phase-2-schema-only-is-stream-filtering-for-sql-a-label-for.md) | Phase-2 schema-only is stream-filtering for SQL, a label for mongo/redis | Accepted |
| [0097](0097-phase-2-readiness-is-engine-specific-not-pod-ready.md) | Phase-2 readiness is engine-specific, not Pod `Ready` | Accepted |
| [0098](0098-redis-phase-2-verification-is-auth-roundtrip-not-rdb-load.md) | Redis Phase-2 verification is auth + roundtrip, not RDB load | Accepted |
| [0099](0099-suspended-state-lives-on-the-source-object-spec-suspended-to.md) | Suspended state lives on the source object (`spec.suspended` today, an annotation before the CRD move), not on the CronJob | Accepted (pre-CRD wording) |
| [0100](0100-internal-safe-goroutine-for-every-long-lived-operator-gorout.md) | `internal/safe.Goroutine` for every long-lived operator goroutine | Accepted |
| [0101](0101-etag-gzip-short-cache-control-on-the-sse-driven-read-endpoin.md) | ETag + gzip + short Cache-Control on the SSE-driven read endpoints | Accepted |
| [0102](0102-analyzer-baseline-unavailable-as-a-side-effect-setter-inside.md) | `analyzer_baseline_unavailable` as a side-effect setter inside `loadPreviousMeta`, not a typed return | Accepted |
| [0103](0103-post-upload-retention-visibility-via-logs-events-not-via-met.md) | Post-upload retention visibility via logs + Events, not via metric or schema change | Accepted (pre-CRD wording) |
| [0104](0104-typed-api-error-codes-apiresponse-code-as-the-frontend-backe.md) | Typed API error codes (`apiResponse.Code`) as the frontend-backend contract | Accepted |
| [0105](0105-sse-event-routing-render-debounce-not-full-dom-diff-incremen.md) | SSE event routing + render debounce, not full DOM-diff incremental updates | Accepted |
| [0106](0106-sftp-password-keyboard-interactive-with-key-auth-still-prefe.md) | SFTP password + keyboard-interactive, with key auth still preferred | Accepted |
| [0107](0107-single-combined-api-dashboard-heatmap-endpoint-for-all-fleet.md) | Single combined `/api/dashboard/heatmap` endpoint for all fleet-wide projections | Accepted |
| [0108](0108-stale-while-revalidate-cache-for-slow-probes-getorrefreshasy.md) | Stale-while-revalidate cache for slow probes (`getOrRefreshAsync`) | Accepted |
| [0109](0109-ui-handlers-share-the-controllers-storagepool.md) | UI handlers share the controllers.StoragePool | Accepted |
| [0110](0110-test-destination-button-does-a-real-upload-readback-delete-p.md) | Test-destination button does a real Upload+Readback+Delete probe | Accepted |
| [0111](0111-ssrf-egress-guard-on-ui-initiated-storage-dials-checkdestina.md) | SSRF egress guard on UI-initiated storage dials (`checkDestinationEgress`) | Accepted |
| [0112](0112-ftps-mkdirall-restores-the-working-directory-after-its-exist.md) | FTPS `mkdirAll` restores the working directory after its existence probe | Accepted |
| [0113](0113-ftps-data-channel-tls-session-reuse-via-tls-newlruclientsess.md) | FTPS data channel TLS session reuse via `tls.NewLRUClientSessionCache` | Accepted |
| [0114](0114-events-k8s-io-v1-required-fields-on-ui-emitted-events.md) | events.k8s.io/v1 required fields on UI-emitted Events | Accepted |
| [0115](0115-empty-form-field-clearing-via-explicit-removekeys-on-put-not.md) | Empty form-field clearing via explicit `removeKeys` on PUT, not via empty-value semantics | Accepted |
| [0116](0116-six-dashboard-charts-derived-from-the-same-fleet-summary-end.md) | Six dashboard charts derived from the same fleet-summary endpoint | Accepted |
| [0117](0117-cronjob-schedule-s-next-fire-time-pre-computed-server-side-a.md) | CronJob schedule's next fire time pre-computed server-side and surfaced everywhere | Accepted (pre-CRD wording) |
| [0118](0118-ui-cache-background-refresh-goroutine-recovers-panics-safe-g.md) | UI cache background-refresh goroutine recovers panics (`safe.Goroutine`) | Accepted |
| [0119](0119-target-detail-page-uses-stale-while-revalidate-for-run-histo.md) | Target detail page uses stale-while-revalidate for run history, mirroring the dashboard | Accepted |
| [0120](0120-meta-upload-on-all-destinations-failure-fails-the-run-not-si.md) | Meta-upload-on-all-destinations failure fails the run, not silently succeeds | Accepted |
| [0121](0121-schema-only-restore-verifier-propagates-scanner-errors-inste.md) | schema-only restore-verifier propagates scanner errors instead of masking them as EOF | Accepted |
| [0122](0122-degraded-empty-dump-detection-is-surfaced-as-a-warning-not-e.md) | Degraded empty-dump detection is surfaced as a warning, NOT enforced via an absolute byte floor | Accepted |
| [0123](0123-meta-list-latestpertarget-share-one-connection-across-the-li.md) | `meta.List` / `LatestPerTarget` share one connection across the List + per-file Gets | Accepted |
| [0124](0124-storage-scrubber-walks-sources-concurrently-behind-a-shared.md) | Storage scrubber walks sources concurrently behind a shared egress throttle | Accepted |
| [0125](0125-operator-ui-hardening-bounded-slot-map-race-free-reconciler.md) | Operator/UI hardening: bounded slot map, race-free reconciler client, single SSE reconnect | Accepted |
| [0126](0126-target-detail-run-history-is-paginated-client-side-and-the-s.md) | Target detail run history is paginated client-side and the size-trend chart is capped | Accepted |
| [0127](0127-string-prefix-storage-backends-s3-azure-gcs-join-paths-by-co.md) | String-prefix storage backends (S3/Azure/GCS) join paths by concatenation, not `path.Join` | Accepted |
| [0128](0128-structural-identifiers-source-target-name-destination-name-p.md) | Structural identifiers (source target name, destination name, path-prefix) are validated at parse time | Accepted (pre-CRD wording) |
| [0129](0129-retention-minkeep-counts-real-dumps-not-timestamps.md) | Retention MinKeep counts real dumps, not timestamps | Accepted |
| [0130](0130-loadlatestmeta-returns-a-typed-outcome-so-destination-failed.md) | `loadLatestMeta` returns a typed outcome so `destination_failed` can catch runtime storage failures | Accepted |
| [0131](0131-ui-stored-xss-in-the-run-history-download-view-controls-fixe.md) | UI stored-XSS in the run-history download/view controls, fixed by correct-context escaping | Accepted (pre-CRD wording) |
| [0132](0132-ui-s3-destination-form-used-the-wrong-data-key-names-and-mas.md) | UI S3 destination form used the wrong data-key names, and masked credentials must be preserved-on-empty across all backends | Accepted |
| [0133](0133-phase-2-verifier-db-pod-uses-a-per-run-random-password-not-a.md) | Phase-2 verifier DB pod uses a per-run random password, not a compile-time constant | Accepted |
| [0134](0134-db-connection-tls-knobs-apply-to-the-dump-tool-not-just-the.md) | DB-connection TLS knobs apply to the dump tool, not just the stats connection | Accepted (pre-CRD wording) |
| [0135](0135-storage-backends-are-measured-against-one-conformance-suite.md) | Storage backends are measured against one conformance suite; `ErrNotFound` and `Close` joined the interface | Accepted |
| [0136](0136-verifier-budget-timeout-advances-the-interval-clock.md) | Verifier budget timeout advances the interval clock | Accepted |
| [0137](0137-redis-empty-dump-floor-scales-with-the-key-count.md) | Redis empty-dump floor scales with the key count | Accepted |
| [0138](0138-restore-cli-operates-on-all-given-destinations.md) | Restore CLI operates on all given destinations | Accepted |
| [0139](0139-strict-csp-by-removing-inline-handlers-not-by-unsafe-inline.md) | Strict CSP by removing inline handlers, not by `'unsafe-inline'` | Accepted |
| [0140](0140-metricsrefresher-iteration-4-one-root-listing-per-destinatio.md) | MetricsRefresher iteration 4: one root listing per destination per tick | Accepted |
| [0141](0141-config-is-strict-at-startup.md) | Config is strict at startup | Accepted |
| [0142](0142-chart-templated-with-tests-ingress-egress-networkpolicy-rele.md) | Chart: templated with tests, `[Ingress, Egress]` NetworkPolicy, release reuses the PR gate | Accepted (pre-CRD wording) |
| [0143](0143-spa-page-modules-reach-the-classic-helpers-through-the-globa.md) | SPA page modules reach the classic helpers through the global scope; shared state lives in one module | Accepted |
| [0144](0144-ui-handler-files-are-split-by-concern-lint-got-stricter-inst.md) | UI handler files are split by concern; lint got stricter instead of adding `nolint` | Accepted |
| [0145](0145-github-actions-are-sha-pinned.md) | GitHub Actions are SHA-pinned | Accepted |

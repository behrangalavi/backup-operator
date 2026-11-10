# Architecture

## 1. Project Overview

### Why this exists

K8up, Stash, and Velero all solve adjacent problems but none of them satisfy all three of:

1. **Typed, validated configuration with status** — a `BackupSource` is rejected at admission when it is wrong and tells you via `kubectl get` when it last succeeded; credentials stay in plain Secrets you can source from the database's own chart.
2. **Semantic dump analysis** — alerting on dump *content* (table disappeared, row-count collapsed, schema fingerprint changed), not just job status.
3. **Multi-destination fan-out as first-class** — one dump → N storage backends in parallel, mixed protocols.

This service is what you get when those three are non-negotiable.

### What it does, end to end

1. The operator watches `BackupSource`, `BackupDestination` and `BackupRecipient` objects in its namespace.
2. For every `BackupSource` it produces a `batch/v1.CronJob` mirroring `spec.schedule` and writes the result back into `status`.
3. At cron-tick, Kubernetes spawns a `Job` whose pod runs the **worker** binary.
4. The worker reads its `BackupSource` plus the referenced credentials Secret, lists `BackupDestinations` (each with its credentials Secret), dumps the database, encrypts with `age` (public key only), uploads to all destinations in parallel.
5. Before the dump it captures table-level statistics; after upload it compares with the previous run's stats and exposes the result as Prometheus metrics.
6. Retention prunes old artifacts, with a safety floor that prevents accidentally deleting the most recent N.
7. For recovery, an operator runs the **restore** CLI on their own machine with the offline `age` private key.

---

## 2. Architecture Overview

```
                                ┌──────────────────────────────────────────────┐
                                │ Kubernetes API                               │
                                │                                              │
   user applies BackupSource    │   BackupSource   ←┐                          │
            │                   │   (+ creds Secret)│ OwnerReference (GC)      │
            ▼                   │                   ▼                          │
   ┌──────────────────┐  watch  │   batch/v1.CronJob ──tick──▶ batch/v1.Job    │
   │ Operator pod     ├─────────▶                                  │           │
   │ (backup-operator) │ reconcile │                                ▼           │
   └──────────────────┘         │                          ┌──────────────┐    │
   - reconciles Secrets         │                          │ Worker pod   │    │
   - templates CronJob          │                          │ (backup-     │    │
   - leader election only       │                          │  worker)     │    │
                                │                          └──────┬───────┘    │
                                └─────────────────────────────────┼────────────┘
                                                                  │
                                          stats + dump + encrypt  │  list destinations
                                                                  ▼
                                                          ┌─────────────────────┐
                                                          │ BackupDestinations  │
                                                          │ (+ creds Secrets)   │
                                                          └─────────────────────┘
                                                                  │
                                                       fan-out, parallel
                                                                  ▼
                                                          ┌──────────────────┐
                                                          │ Hetzner SB / S3 │
                                                          │ MinIO / R2 / B2  │
                                                          └──────────────────┘

                                ┌──────────────────────────────────────────────┐
                                │ Operator's machine (offline)                 │
                                │                                              │
                                │   age private key  ──▶  backup-restore CLI  │
                                │                              │               │
                                │                              ▼               │
                                │                         decrypted dump       │
                                │                         to stdout/file       │
                                └──────────────────────────────────────────────┘
```

### Three binaries, one container image

| Binary | Where it runs | Purpose | Why separate |
|---|---|---|---|
| `backup-operator` | Operator Deployment | Reconciles BackupSource → managed CronJob + status | Stays small, can be replicated, cannot be crowded out by a big dump |
| `backup-worker` | CronJob-spawned Job pod | One-shot: dump → encrypt → fan-out → retention | One pod per backup run, isolated resources, native K8s observability |
| `backup-restore` | Operator's laptop | List + decrypt + extract a chosen artifact; `migrate` subcommand converts legacy Secrets | Only place the age private key ever lives |

The container image ships the two in-cluster binaries — `backup-operator` and `backup-worker` — with the entrypoint differing per pod. `backup-restore` is deliberately NOT in the image: it is built separately and runs on the operator's machine, the only place the age private key ever lives (see [§5.3](#53-restore-cmdrestoremaingo--backup-restore) and the "Restore is a separate binary" ADR).

---

## 4. Directory Structure

```
src/
├── analyzer/            # Stats comparison: schema diffing, row-count drops, size collapse
├── api/v1alpha1/        # backup.mogenius.io CRD types (BackupSource, BackupDestination, BackupRecipient) + generated DeepCopy; CRD YAML is generated into charts/backup-operator/files/crds (just generate)
├── assert/              # Fail-fast assertion utilities (panic on critical errors)
├── cmd/
│   ├── main.go          # Operator binary — pure reconciler
│   ├── worker/          # Worker binary — one-shot backup runner
│   └── restore/         # Restore CLI — out-of-cluster; also `backup-restore migrate` (legacy Secrets → CRs)
├── config/              # Singleton env-var config with schema validation; validate.go holds the shared validators + default constants used by operator and worker
├── controllers/
│   ├── cronjob_controller.go  # BackupSource → batch/v1.CronJob + Ready condition / materialised schedule in status
│   ├── list.go                # Lists BackupSources + BackupDestinations and resolves destination credentials Secrets
│   ├── dest_listing.go        # One root listing per destination per refresher tick → newest meta per target (replaces per-source List+Get)
│   ├── metrics_refresher.go   # Reconstructs run-level Gauges from each destination's latest meta.json
│   ├── status.go / status_writer.go # Change-only status writers: run-level BackupSource status, Reachable on BackupDestination
│   ├── recipient_reconciler.go # BackupRecipient objects → merged Secret AGE_PUBLIC_KEYS; Bootstrap() seeds chart-provided keys
│   └── storage_scrub.go       # Periodic SHA256 verification of stored dumps (silent corruption detector)
├── crypto/              # age public-key encryption + private-key decryption
├── dumper/              # DB dump abstraction
│   ├── factory/         # Creates the right Dumper from spec.database.type
│   ├── postgres/        # pg_dump exec + stats via pgx
│   ├── mysql/           # mysqldump exec + stats via go-sql-driver/mysql
│   ├── mongo/           # mongodump exec + stats via mongo-driver/v2
│   └── redis/           # redis-cli --rdb exec + stats via INFO/DBSIZE
├── internal/
│   ├── agekeys/         # Hash + object-name helpers for BackupRecipient (shared by operator bootstrap, UI, migrate)
│   ├── backup/          # Pipeline (worker only): stats → dump → encrypt → fan-out → retention
│   ├── labels/          # Storage-layout constants: restore-verification modes, compression, dump suffixes, merged-recipients field
│   ├── meta/            # MetaFile type — deserialized sidecar JSON (shared across pipeline, refresher, UI)
│   ├── migrate/         # Legacy labelled-Secret → CR conversion (the only place the old label/annotation keys survive)
│   ├── redact/          # Credential scrubbing for error strings (status fields, Events, UI diagnostics)
│   ├── safe/            # safe.Goroutine — panic recovery + stack trace; used by every long-lived goroutine in the operator
│   ├── scheduler/       # Cron jitter + next-run computation shared by reconciler, status writer and UI
│   └── secrets/         # Flattens BackupSource/BackupDestination (+ credentials Secret) into Source/Destination; FilterDestinations
├── metrics/             # Prometheus metrics — semantic signals for Alertmanager
├── storage/             # Upload destination abstraction (Storage + optional BatchStorage / DirectoryRemover, ErrNotFound, Close)
│   ├── storagetest/     # In-memory fake + conformance suite every backend's semantics are checked against
│   ├── factory/         # Creates the right Storage from BackupDestination spec.type
│   ├── sftp/            # Hetzner Storage Box and generic SFTP
│   ├── ftps/            # FTP over TLS (QNAP, Synology, FreeNAS-style NAS firmware)
│   ├── s3/              # AWS S3, MinIO, Hetzner Object Storage, R2, B2, ...
│   ├── azure/           # Azure Blob Storage (shared-key auth)
│   └── gcs/             # Google Cloud Storage (service-account JSON auth)
├── verifier/            # Restore-verification: prove the uploaded artifact is decryptable+parseable
│   ├── verifier.go      # Verifier interface, ShouldVerify schedule logic, FailureResult helper
│   ├── factory/         # Mode → Verifier (stream-validate, schema-only, sample, full)
│   ├── stream/          # ModeStreamValidate: in-process decrypt → gunzip → parser, no DB-pod-spawn
│   ├── ephemeral/       # K8s Spawner — creates short-lived DB pods with emptyDir; ownerRef-cascade-cleaned
│   └── restore/         # ModeSchemaOnly / Sample / Full: spawn DB → restore → smoke queries (Phase 2)
├── ui/                  # Built-in web dashboard and management API
│   ├── cache.go         # TTL key→value cache with both getOrLoad (blocking) and getOrRefreshAsync (stale-while-revalidate) — used to keep slow storage probes off the dashboard hot path
│   ├── data.go          # Read-side aggregation: dataSource, k8sData (RV-memoised source/destination lists), listTargets, target detail, estimateDuration. Holds the shared StoragePool reference.
│   ├── models.go        # View-model types (targetSummary, dashboardSummary, heatmap/anomaly/duration points)
│   ├── heatmap.go       # fleetHeatmap: one bounded pass over the day window → heatmap + storage + anomalies (200-entry buffer) + durations + verification
│   ├── errors_api.go    # Typed API error catalogue (codeValidation, codeNotFound, …) + writeError helper
│   ├── handlers.go      # Legacy HTML template handlers + /download/<target>/<ts>/{dump|meta} (uses StoragePool)
│   ├── api_types.go     # Shared apiResponse type
│   ├── handlers_trigger.go   # Manual trigger (Job from the CronJob template) + /api/jobs with duration estimate
│   ├── handlers_dest_diag.go # destination-stats / destination-health (+ scrub status from the metric registry) / consistency-check
│   ├── handlers_dashboard.go # /api/dashboard/heatmap
│   ├── events.go        # emitUIEvent / emitObjectEvent — Kubernetes Events for UI mutations, with request id + auth-proxy identity
│   ├── handlers_sources.go      # BackupSource CRUD (+ password-only credentials Secret owned by the CR)
│   ├── handlers_destinations.go # BackupDestination CRUD (+ credentials Secret); test-connection does a real upload+readback+delete probe
│   ├── handlers_age_keys.go  # Age recipient CRUD (`/api/age-keys`), backed by BackupRecipient objects
│   ├── handlers_settings.go  # Settings API: GET /api/settings (effective operator defaults, read-only), values.yaml export
│   ├── middleware_cache.go   # cachedJSON: ETag + gzip + short Cache-Control on high-volume read endpoints
│   ├── middleware_security.go # CSP + security headers, X-Request-Id, auth-proxy identity extraction, V(1) access log
│   ├── server.go        # HTTP server, routing, SPA handler, SSE broker, StoragePool interface + storageFor helper
│   ├── static/          # SPA frontend (vanilla JS, no build step)
│   │   ├── index.html   # SPA shell with sidebar (Dashboard / Sources / Destinations / Jobs / Alerts / Audit / Age Keys / Settings), modal, toast containers
│   │   ├── style.css    # Token-only theming (dark default, light via prefers-color-scheme or data-theme), responsive layout, chart/gantt/heatmap styles
│   │   ├── js/          # core.js/charts.js are classic scripts; pages.js/app.js are ES modules. js/tests/*.test.mjs run with `node --test` (escaping, helpers, data-action registry, module layout, i18n/theme invariants)
│   │   │   ├── core.js  # DOM helpers ($, $$), i18n (tr escapes, trText), API + fetchWithTimeout, toast, modal (focus trap), escape/format, data-action registry, theme
│   │   │   ├── charts.js # SVG chart renderers reading colours from CSS tokens
│   │   │   ├── pages.js # Thin entry: imports every page module, re-exports renderers/router/state
│   │   │   ├── pages/   # state.js (single shared mutable state + render-gen guard), router.js (page registry, hash routing), one module per page: dashboard, sources, destinations, alerts, jobs, audit, target, settings, agekeys
│   │   │   └── app.js   # Entry point: SSE wiring, hash-router hookup, init
│   │   └── i18n/        # EN/DE/FR translation dictionaries (loaded by lang picker)
│   └── templates/       # Legacy Go HTML templates (kept for backward compat)
└── docs/                # Read-only docs portal: serves CLAUDE.md + README.md + generated tech stack
    ├── server.go        # HTTP server on DOCS_ADDR (default :8083); separate process from UI
    ├── render.go        # goldmark Markdown renderer; in-page search dropdown injected into shell
    ├── shell.go         # HTML shell + sidebar nav (no K8s client, no Secret access)
    ├── tech_stack.go    # Generates "tech stack" page from go.mod direct deps
    └── static/          # CSS/JS for the docs portal (vanilla, embedded)
charts/backup-operator/   # Helm chart (CRDs via templates/crds.yaml ← files/crds, Deployment, RBAC, Service, ServiceMonitor, PrometheusRule)
test/local/              # Manifests for the Docker Desktop test stack (gitignored, see section 16)
test/e2e/                # kind-based backup → restore round trip run by the CI `e2e` job
Dockerfile               # Builds operator + worker into one alpine image with DB clients
Justfile                 # Build/test/lint targets + local test setup
.env.example             # Operator envs template for `just run` (copy to .env)
```

---

## 5. The Three Binaries

### 5.1 Operator (`cmd/main.go` → `backup-operator`)

A pure Kubernetes reconciler. Does **not**:

- run cron in-process
- hold encryption keys
- shell out to `pg_dump` / `mysqldump` (MySQL+MariaDB) / `mongodump` / `redis-cli --rdb`
- maintain destination caches

Does:

- Watch `BackupSource` objects in `WATCH_NAMESPACE` (generation changes only — status writes don't wake it)
- For each `BackupSource`, ensure a managed `batch/v1.CronJob` exists with the right spec, then write `Ready`, `cronJobName`, `materializedSchedule` and `nextScheduleTime` into its status
- Owns the CronJob via `OwnerReference` so deleting the `BackupSource` cascades to the CronJob
- Read Secrets uncached and by name only (destination credentials for probes, the merged age Secret) — the operator never holds an informer over the namespace's Secrets
- Leader-elects so concurrent operator replicas don't race on `CreateOrUpdate`

The CronJob's pod-spec is templated from `WORKER_*` env values that the operator carries. Helm sets these to mirror its own image, so worker pods always run the same release as the operator that created them.

### 5.2 Worker (`cmd/worker/main.go` → `backup-worker`)

Launched by Kubernetes when the CronJob fires. Runs once and exits.

```
main()
  ├── flags: --source, --namespace
  ├── config.Initialize  # AGE_PUBLIC_KEYS, RUN_TIMEOUT_SECONDS, TEMP_DIR, ...
  ├── load BackupSource by name + its credentialsSecretRef Secret
  ├── flatten to secrets.Source (spec resolved against defaults, password from the Secret)
  ├── materialise spec.database.tls.caSecretRef to TEMP_DIR (if set)
  ├── list BackupDestinations, resolve each credentialsSecretRef
  ├── filter by source.AllowsDestination()  # honors spec.destinations
  ├── construct crypto.Encryptor from AGE_PUBLIC_KEYS
  ├── construct Pipeline with staticDestProvider
  └── pipeline.Run(ctx, src)  ── exit 0 / exit 1
```

The pipeline performs (in order):

1. `CollectStats(ctx)` — only if `analyzer-enabled` is true. Failure here is non-fatal; the analyzer simply skips comparison this run.
2. **Dump → gzip → age → temp file.** The age recipient public key is the only key the worker has access to. The pipeline is a tee of `io.Reader`s; the temp file is the only on-disk materialisation.
3. Load previous run's `meta.json` from any destination → analyzer comparison → metrics.
4. **Two-phase fan-out:**
   - **Phase 1 (`fanOutDumps`):** `sync.WaitGroup` over destinations, each goroutine opens the temp file independently and uploads the dump. Per-destination errors are logged + metrified, never aborting peers. Returns `[]DestinationResult` with per-destination outcome (name, storageType, status, error).
   - **Phase 2 (`uploadMeta`):** Builds `meta.json` including the `destinations` results array, then uploads to all destinations that had a successful dump upload. Retries up to 3 times with exponential backoff to avoid "phantom backups" (dump exists but meta.json missing).
5. **Retention** (best-effort, never fails the run): list dumps, sort by timestamp, protect `MinKeep` newest, delete those older than `RetentionDays`.

### 5.3 Restore (`cmd/restore/main.go` → `backup-restore`)

Local CLI. Uses your `~/.kube/config` to read the `BackupDestination`(s) and their credentials Secrets, downloads the chosen artifact, decrypts with the offline private key, streams to stdout (or a file). The ciphertext is hashed while streaming and compared with the `sha256` in `meta.json` (mismatch → non-zero exit, `-o` file removed; no sidecar → "integrity not verified" log). A failed download removes the partial output file so a truncated dump can never pass for a good one. Compression is resolved `--compression` flag > dump filename suffix > `meta.json` > gzip. Decrypt failures carry a hint (`NoIdentityMatch` → "wrong private key? see kubectl get backuprecipients").

```bash
# Show what's available
backup-restore --destination hetzner -n backup --target prod-users --list

# Fetch the latest, gunzip, pipe to psql
backup-restore --destination hetzner -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost prod_clone

# Specific timestamp to file
backup-restore --destination hetzner -n backup --target prod-users \
  --age-key ~/age.key --timestamp 20260428T020000Z -o dump.sql.gz

# Several destinations: --destination is repeatable / comma-separated. --list merges
# runs by timestamp (DESTINATIONS column), latest is picked across all of them and the
# chosen destination is printed to stderr; a Get that fails falls through to the next
# destination holding the same run.
backup-restore --destination hetzner,minio -n backup --target prod-users --list

# Convert a namespace from the legacy labelled-Secret model to CRs
backup-restore migrate -n backup --age-secret backup-operator-age            # print manifests
backup-restore migrate -n backup --age-secret backup-operator-age --apply    # create them (legacy Secrets kept)
backup-restore migrate -n backup --apply --delete-legacy                     # ...and remove the legacy Secrets
```

---

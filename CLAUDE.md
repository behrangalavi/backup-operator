# Backup Operator — Claude Code Guide

A Kubernetes-native backup operator in Go for **PostgreSQL**, **MySQL**, **MariaDB**, **MongoDB**, and **Redis**, with public-key encryption (`age`), multi-destination fan-out (SFTP + S3-compatible), semantic dump analysis, and Prometheus-driven alerting. Built on the **Operator-Reconciles-CronJobs** pattern: the operator does not run backups itself; Kubernetes does.

---

## 1. Project Overview

### Why this exists

K8up, Stash, and Velero all solve adjacent problems but none of them satisfy all three of:

1. **Discovery via labeled Secrets**, not CRDs — labelling a Secret is the entire user contract.
2. **Semantic dump analysis** — alerting on dump *content* (table disappeared, row-count collapsed, schema fingerprint changed), not just job status.
3. **Multi-destination fan-out as first-class** — one dump → N storage backends in parallel, mixed protocols.

This service is what you get when those three are non-negotiable.

### What it does, end to end

1. The operator watches Secrets in its namespace.
2. When a Secret carries `backup.mogenius.io/role=source` it produces a `batch/v1.CronJob` mirroring the Secret's schedule annotation.
3. At cron-tick, Kubernetes spawns a `Job` whose pod runs the **worker** binary.
4. The worker reads its source Secret, lists destination Secrets, dumps the database, encrypts with `age` (public key only), uploads to all destinations in parallel.
5. Before the dump it captures table-level statistics; after upload it compares with the previous run's stats and exposes the result as Prometheus metrics.
6. Retention prunes old artifacts, with a safety floor that prevents accidentally deleting the most recent N.
7. For recovery, an operator runs the **restore** CLI on their own machine with the offline `age` private key.

---

## 2. Architecture Overview

```
                                ┌──────────────────────────────────────────────┐
                                │ Kubernetes API                               │
                                │                                              │
   user labels Secret           │   Source Secret  ←┐                          │
            │                   │                   │ OwnerReference (GC)      │
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
                                                          ┌──────────────────┐
                                                          │ Destination Secrets │
                                                          │ (SFTP, S3, ...)     │
                                                          └──────────────────┘
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
| `backup-operator` | Operator Deployment | Reconciles Source Secret → managed CronJob | Stays small, can be replicated, cannot be crowded out by a big dump |
| `backup-worker` | CronJob-spawned Job pod | One-shot: dump → encrypt → fan-out → retention | One pod per backup run, isolated resources, native K8s observability |
| `backup-restore` | Operator's laptop | List + decrypt + extract a chosen artifact | Only place the age private key ever lives |

The same image ships all binaries; the entrypoint differs per pod.

---

## 3. Quick Start

```bash
# 1. Generate an age key pair OFFLINE on your machine
age-keygen -o ~/age.key
# Keep ~/age.key secret. It is the ONLY way to recover backups.
# Public line in the file looks like:  age1qx...
# Private line:                         AGE-SECRET-KEY-1...

# 2. Install with the public recipient
helm install backup-operator ./charts/backup-operator -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient-here"
# Multi-key rotation:
#   --set agePublicKeys[0]=age1qx...primary --set agePublicKeys[1]=age1yz...dr
# String form is also accepted:
#   --set agePublicKeys="age1qx..."  (single key only — CLI cannot pass a literal newline)

# 3. Label a database Secret as a source
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: prod-users-db
  labels:
    backup.mogenius.io/role: source
    backup.mogenius.io/db-type: postgres
  annotations:
    backup.mogenius.io/name: "prod-users"
    backup.mogenius.io/schedule: "0 2 * * *"
data:
  host: <base64>
  port: <base64>      # optional, defaults to 5432
  database: <base64>
  username: <base64>
  password: <base64>
EOF

# 4. Label a destination Secret (Hetzner Storage Box example)
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: hetzner-sb
  labels:
    backup.mogenius.io/role: destination
    backup.mogenius.io/storage-type: hetzner-sftp
  annotations:
    backup.mogenius.io/name: "hetzner"
    backup.mogenius.io/path-prefix: "/cluster-prod"
data:
  host: <base64>
  port: <base64>            # 23 for Hetzner
  username: <base64>
  ssh-private-key: <base64>
  known-hosts: <base64>     # ssh-keyscan output, recommended
EOF

# 5. Confirm a CronJob was created
kubectl -n backup get cronjobs
# NAME                       SCHEDULE      ...
# backup-prod-users-db       0 2 * * *

# 6. Trigger a manual run instead of waiting
kubectl -n backup create job --from=cronjob/backup-prod-users-db manual-$(date +%s)

# 7. Restore
backup-restore --storage-secret hetzner-sb -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost prod_clone
```

### 3.1 Securing the UI with an Authentication Proxy

The built-in UI (`UI_ENABLED=true`) has **no authentication**. In production, place an authenticating reverse proxy in front of it. Common options:

**OAuth2 Proxy (recommended for SSO):**

```yaml
# values.yaml snippet — deploy as sidecar or separate Deployment
# pointing at the operator's UI port.
extraContainers:
  - name: oauth2-proxy
    image: quay.io/oauth2-proxy/oauth2-proxy:v7
    args:
      - --upstream=http://localhost:8081
      - --http-address=0.0.0.0:4180
      - --provider=oidc
      - --oidc-issuer-url=https://accounts.google.com  # or Keycloak, Azure AD, etc.
      - --email-domain=yourcompany.com
      - --cookie-secret=$(head -c 32 /dev/urandom | base64)
    ports:
      - containerPort: 4180
```

**Kubernetes Ingress with basic-auth:**

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: backup-ui
  annotations:
    nginx.ingress.kubernetes.io/auth-type: basic
    nginx.ingress.kubernetes.io/auth-secret: backup-ui-htpasswd
spec:
  rules:
    - host: backup.internal.yourcompany.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: backup-operator
                port:
                  number: 8081
```

The operator UI exposes a full management interface: CRUD for source and destination Secrets, manual backup triggers, job status, and live SSE updates. Passwords and SSH keys are masked in API responses (`***`). Authentication prevents unauthorized users from modifying backup configurations or browsing backup history.

---

## 4. Directory Structure

```
src/
├── analyzer/            # Stats comparison: schema diffing, row-count drops, size collapse
├── assert/              # Fail-fast assertion utilities (panic on critical errors)
├── cmd/
│   ├── main.go          # Operator binary — pure reconciler
│   ├── worker/          # Worker binary — one-shot backup runner
│   └── restore/         # Restore CLI — out-of-cluster
├── config/              # Singleton env-var config with schema validation
├── controllers/
│   ├── cronjob_controller.go  # Source Secret → batch/v1.CronJob
│   ├── metrics_refresher.go   # Reconstructs run-level Gauges from each destination's latest meta.json
│   ├── recipient_reconciler.go # role=age-recipient Secrets → merged Secret AGE_PUBLIC_KEYS; one-time legacy migration in Bootstrap()
│   └── storage_scrub.go       # Periodic SHA256 verification of stored dumps (silent corruption detector)
├── crypto/              # age public-key encryption + private-key decryption
├── dumper/              # DB dump abstraction
│   ├── factory/         # Creates the right Dumper from db-type label
│   ├── postgres/        # pg_dump exec + stats via pgx
│   ├── mysql/           # mysqldump exec + stats via go-sql-driver/mysql
│   └── mongo/           # mongodump exec + stats via mongo-driver/v2
├── internal/
│   ├── backup/          # Pipeline (worker only): stats → dump → encrypt → fan-out → retention
│   ├── labels/          # Constants for backup.mogenius.io/* labels & annotations
│   ├── meta/            # MetaFile type — deserialized sidecar JSON (shared across pipeline, refresher, UI)
│   ├── safe/            # safe.Goroutine — panic recovery + stack trace; used by every long-lived goroutine in the operator
│   └── secrets/         # Parses Secrets into Source/Destination configs; FilterDestinations helper
├── metrics/             # Prometheus metrics — semantic signals for Alertmanager
├── storage/             # Upload destination abstraction
│   ├── factory/         # Creates the right Storage from storage-type label
│   ├── sftp/            # Hetzner Storage Box and generic SFTP
│   ├── ftps/            # FTP over TLS (QNAP, Synology, FreeNAS-style NAS firmware)
│   └── s3/              # AWS S3, MinIO, Hetzner Object Storage, R2, B2, ...
├── verifier/            # Restore-verification: prove the uploaded artifact is decryptable+parseable
│   ├── verifier.go      # Verifier interface, ShouldVerify schedule logic, FailureResult helper
│   ├── factory/         # Mode → Verifier (stream-validate, schema-only, sample, full)
│   ├── stream/          # ModeStreamValidate: in-process decrypt → gunzip → parser, no DB-pod-spawn
│   ├── ephemeral/       # K8s Spawner — creates short-lived DB pods with emptyDir; ownerRef-cascade-cleaned
│   └── restore/         # ModeSchemaOnly / Sample / Full: spawn DB → restore → smoke queries (Phase 2)
├── ui/                  # Built-in web dashboard and management API
│   ├── cache.go         # TTL key→value cache with both getOrLoad (blocking) and getOrRefreshAsync (stale-while-revalidate) — used to keep slow storage probes off the dashboard hot path
│   ├── data.go          # Read-side aggregation: listTargets, target detail, estimateDuration, fleetHeatmap (combined heatmap+storage+anomalies+durations+verification). Holds the shared StoragePool reference.
│   ├── errors_api.go    # Typed API error catalogue (codeValidation, codeNotFound, …) + writeError helper
│   ├── handlers.go      # Legacy HTML template handlers + /download/<target>/<ts>/{dump|meta} (uses StoragePool)
│   ├── handlers_api.go  # REST API: CRUD sources/destinations, trigger, SSE, destination-stats/health/consistency, dashboard heatmap; /api/jobs embeds duration estimate; test-connection does a real upload+readback+delete probe
│   ├── handlers_age_keys.go  # Age recipient CRUD (`/api/age-keys`), backed by per-recipient Secrets
│   ├── handlers_settings.go  # Settings API: GET/PUT /api/settings, values.yaml export
│   ├── middleware_cache.go   # cachedJSON: ETag + gzip + short Cache-Control on high-volume read endpoints
│   ├── server.go        # HTTP server, routing, SPA handler, SSE broker, StoragePool interface + storageFor helper
│   ├── static/          # SPA frontend (vanilla JS, no build step)
│   │   ├── index.html   # SPA shell with sidebar (Dashboard / Sources / Destinations / Jobs / Alerts / Audit / Age Keys / Settings), modal, toast containers
│   │   ├── style.css    # Dark theme, responsive layout, chart-svg + chart-svg-tall variants, gantt + heatmap + loading-spinner styles
│   │   ├── app.js       # Hash-router, API helpers, page renderers, forms; seven dashboard charts (Storage Donut, Next-Run Gantt, Fleet Heatmap, Storage Growth, Anomaly Stream, Duration Distribution, Verification Trend); running-job progress bar shared between Jobs page and Target detail
│   │   └── i18n/        # EN/DE/FR translation dictionaries (loaded by lang picker)
│   └── templates/       # Legacy Go HTML templates (kept for backward compat)
└── docs/                # Read-only docs portal: serves CLAUDE.md + README.md + generated tech stack
    ├── server.go        # HTTP server on DOCS_ADDR (default :8083); separate process from UI
    ├── render.go        # goldmark Markdown renderer; in-page search dropdown injected into shell
    ├── shell.go         # HTML shell + sidebar nav (no K8s client, no Secret access)
    ├── tech_stack.go    # Generates "tech stack" page from go.mod direct deps
    └── static/          # CSS/JS for the docs portal (vanilla, embedded)
charts/backup-operator/   # Helm chart (Deployment, RBAC, Service, ServiceMonitor, PrometheusRule)
test/local/              # Manifests for the Docker Desktop test stack (see section 16)
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

- Watch Secrets in `WATCH_NAMESPACE` (filtered by role-label transition predicate)
- For each Source Secret, ensure a managed `batch/v1.CronJob` exists with the right spec
- Owns the CronJob via `OwnerReference` so deleting the Secret cascades to the CronJob
- Detects role-label removal and deletes the orphaned CronJob
- Leader-elects so concurrent operator replicas don't race on `CreateOrUpdate`

The CronJob's pod-spec is templated from `WORKER_*` env values that the operator carries. Helm sets these to mirror its own image, so worker pods always run the same release as the operator that created them.

### 5.2 Worker (`cmd/worker/main.go` → `backup-worker`)

Launched by Kubernetes when the CronJob fires. Runs once and exits.

```
main()
  ├── flags: --source-secret, --namespace
  ├── config.Initialize  # AGE_PUBLIC_KEYS, RUN_TIMEOUT_SECONDS, TEMP_DIR, ...
  ├── load source Secret by name
  ├── parse to secrets.Source (annotations resolved against defaults)
  ├── list destination Secrets via label selector
  ├── filter by source.AllowsDestination()  # honors backup.mogenius.io/destinations
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

Local CLI. Uses your `~/.kube/config` to read the destination Secret, downloads the chosen artifact, decrypts with the offline private key, streams to stdout (or a file).

```bash
# Show what's available
backup-restore --storage-secret hetzner-sb -n backup --target prod-users --list

# Fetch the latest, gunzip, pipe to psql
backup-restore --storage-secret hetzner-sb -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost prod_clone

# Specific timestamp to file
backup-restore --storage-secret hetzner-sb -n backup --target prod-users \
  --age-key ~/age.key --timestamp 20260428T020000Z -o dump.sql.gz
```

---

## 6. The Discovery Contract

### 6.1 Labels

| Label | Required | Values |
|---|---|---|
| `backup.mogenius.io/role` | **yes** | `source` \| `destination` \| `age-recipient` |
| `backup.mogenius.io/db-type` | yes (sources only) | `postgres` \| `mysql` \| `mariadb` \| `mongo` \| `redis` |
| `backup.mogenius.io/storage-type` | yes (destinations only) | `sftp` \| `hetzner-sftp` \| `ftps` \| `s3` |

### 6.2 Source Secret annotations

| Annotation | Default | Effect |
|---|---|---|
| `backup.mogenius.io/name` | Secret name | Logical target name. Used in metrics labels, object paths, CronJob naming. |
| `backup.mogenius.io/schedule` | `DEFAULT_SCHEDULE` (`0 2 * * *`) | Cron expression for the managed CronJob |
| `backup.mogenius.io/jitter-minutes` | unset | Spread the cron's minute field across an N-minute window via a deterministic per-source hash, to avoid fleet-wide thundering-herd against the storage backend. Default behaviour (annotation absent): jitter only when the user wrote minute==`0` — non-zero literal minutes (e.g. `15 2 * * *`) are respected. Setting it explicitly opts in to spreading even on a non-zero literal minute (window = the value). `0` pins the schedule. Multi-fire schedules (`*/15 * * * *`, `0,30 * * * *`, `0-30 * * * *`, `* * * * *`) are always left alone. Capped at 60. |
| `backup.mogenius.io/suspended` | `false` | `true` → reconciler sets `Spec.Suspend=true` on the managed CronJob. Existing artifacts and source config are kept; manual triggers (`kubectl create job --from=cronjob/...` or the UI's Run button) ignore Suspend and still run. The Secret is the source of truth — manual `kubectl patch cronjob ... suspend=true` is overridden on the next reconcile. |
| `backup.mogenius.io/analyzer-enabled` | `true` | `false` → skip `CollectStats` and analyzer for this source |
| `backup.mogenius.io/destinations` | unset | Comma-separated allow-list of destination *names*. Empty = fan out to all. |
| `backup.mogenius.io/retention-days` | `DEFAULT_RETENTION_DAYS` (30) | Delete dumps older than N days. `0` = keep forever. |
| `backup.mogenius.io/min-keep` | `DEFAULT_MIN_KEEP` (3) | Safety floor — never delete below this many newest dumps. |
| `backup.mogenius.io/row-drop-threshold` | `0.5` | Analyzer anomaly threshold for row-count drops. `0.3` means flag when a table shrinks below 30% of its previous size. |
| `backup.mogenius.io/size-drop-threshold` | `0.5` | Analyzer anomaly threshold for dump size drops. Same semantics as row-drop. |
| `backup.mogenius.io/anonymize-tables` | `false` | `true` → hash table names in `meta.json` with SHA256 (16 hex chars). Row counts preserved. |
| `backup.mogenius.io/empty-dump-check` | `true` | Hard-fail when the dump appears empty despite the source DB having data. Two detection paths: SQL (postgres/mysql/mariadb) compares dump-stream INSERT/COPY rows to pre-dump stats; mongo / redis use a size heuristic against pre-dump `collStats` / key counts. Set to `false` for sources that are intentionally schema-only (empty template DBs). |
| `backup.mogenius.io/restore-verification-mode` | `off` | Restore-verification mode. `off` (default), `stream-validate` (Phase 1, no DB-pod-spawn), `schema-only` / `sample` / `full` (Phase 2: spawn ephemeral DB pod + restore). Phase-2 modes require the worker SA to have `pods: create/get/list/watch/delete`, `pods/status: get`, and `pods/log: get` in its own namespace — see `restoreVerification.enableEphemeralPodSpawn` in the chart. Unknown values fall back to `off` so a typo can't accidentally turn on a heavyweight mode. |
| `backup.mogenius.io/restore-verification-interval` | `168h` (when mode is active) | Minimum gap between verifier-runs (Go duration). State-driven: worker reads `latestMeta.restoreVerification.completedAt` and skips this run when the interval has not elapsed. Manual runs (`kubectl create job --from=cronjob/...`) verify whenever they're overdue regardless of cron drift. The "first verification" path runs immediately when mode is set but no `RestoreVerification` block exists on the latest meta yet — operators see signal without waiting one full interval after enabling. |
| `backup.mogenius.io/verification-image` | per-DB-type default | Container image for the verifier pod. Only consulted in Phase-2 modes. Pin this to match the source DB's exact major version when restore semantics depend on the engine version (charset defaults, function signatures, dump format compatibility). Per-DB defaults from `verifier/restore/engine.go.DefaultImage`: `postgres:16-alpine`, `mysql:8.0`, `mariadb:11`, `mongo:7`, `redis:7-alpine`. |
| `backup.mogenius.io/verification-volume-size` | per-mode default | `emptyDir.sizeLimit` for the verifier pod's data volume (e.g. `100Gi`, `5Gi`). Defaults: `1Gi` (schema-only), `5Gi` (sample), `50Gi` (full). Accepts decimal (`K`/`M`/`G`/`T`) and binary (`Ki`/`Mi`/`Gi`/`Ti`) suffixes. Override when a single source's restore needs more headroom — at scale, the node's ephemeral storage is a real budget. |
| `backup.mogenius.io/compression` | `gzip` | Compression algorithm for the dump before age encryption. `gzip` (default, backward compatible) or `zstd` (30-50% better compression at comparable CPU). Stored in `meta.json` so restore CLI and verifiers auto-detect the decompressor. |
| `backup.mogenius.io/extra-<key>` | none | Surfaced into `dumper.Config.Extra[key]`. Used for DB-specific options (e.g. `extra-sslmode`, `extra-authSource`). |

A typo on a feature-flag annotation falls back to the default rather than rejecting the Secret — backups must keep running even if a flag is misspelled.

### 6.3 Source Secret `data` keys

| Key | Required | Notes |
|---|---|---|
| `host` | **yes** | DB hostname |
| `port` | no | Defaults: 5432 (pg), 3306 (mysql/mariadb), 27017 (mongo), 6379 (redis) |
| `database` | (situational) | Postgres/MySQL/MariaDB: required for `pg_dump`/`mysqldump` to scope. Mongo: optional, omitted = all non-system DBs. Redis: optional DB index (`0`–`15`) — narrows stats only; the RDB dump is always full-instance. |
| `username` | **yes** for all except `redis` | Redis pre-6 uses password-only AUTH; ACL usernames came in 6.0 and are optional |
| `password` | **yes** | |

### 6.4 Destination Secret annotations

| Annotation | Effect |
|---|---|
| `backup.mogenius.io/name` | Logical destination name; matched against source allow-lists |
| `backup.mogenius.io/path-prefix` | Prefix prepended to every uploaded object path |

### 6.5 Destination Secret `data` keys

#### `storage-type: sftp` / `hetzner-sftp`

| Key | Required | Notes |
|---|---|---|
| `host` | **yes** | |
| `port` | no | Default 22; Hetzner Storage Box uses 23 |
| `username` | **yes** | |
| `ssh-private-key` | one of | PEM-encoded. Provide this OR `password`. Public-key auth is preferred when both are supplied. |
| `password` | one of | Plain SFTP password. Provide this OR `ssh-private-key`. Key-based auth is the recommended path; password support is mostly for legacy servers. |
| `known-hosts` | recommended | Standard `ssh-keyscan` output. Use `[host]:port` for non-22 ports. Without it the connection is **rejected** unless `insecure-skip-host-verify` is set. |
| `insecure-skip-host-verify` | no | Set to `"true"` to accept any host key when `known-hosts` is absent. Logs an `INSECURE` warning. Use only for initial testing. |

#### `storage-type: ftps`

For NAS firmware (QNAP, older Synology, FreeNAS) that only offers FTP over TLS, not SFTP. The data channel is always encrypted (PROT P) — plain control + plain data is intentionally not exposed. If your NAS supports SSH/SFTP, prefer that: FTPS has worse NAT/firewall behaviour (PASV data sockets), a weaker historical security record, and the protocol's data-channel/control-channel split makes it more fragile.

| Key | Required | Notes |
|---|---|---|
| `host` | **yes** | |
| `port` | no | Default 21 (explicit) / 990 (implicit) |
| `username` | **yes** | |
| `password` | **yes** | Key-based auth is not part of the FTP spec; password is the only option. |
| `tls-mode` | no | `explicit` (default, port 21 + AUTH TLS — what most NAS UIs call "FTP with SSL/TLS (explicit)") or `implicit` (port 990, TLS from byte zero). |
| `insecure-skip-cert-verify` | no | Set to `"true"` to skip TLS certificate validation. Logs an `INSECURE` warning. Use only when the NAS ships a self-signed cert you cannot replace. |

#### `storage-type: s3`

| Key | Required | Notes |
|---|---|---|
| `bucket` | **yes** | |
| `access-key-id` | **yes** | |
| `secret-access-key` | **yes** | |
| `region` | no | Defaults to `us-east-1`; non-AWS providers usually ignore this |
| `endpoint` | no | Required for non-AWS (MinIO, Hetzner Object Storage, R2, B2, Wasabi). Omit for AWS. |
| `path-style` | no | `"true"` for MinIO etc. that require path-style addressing. |

### 6.6 Age-recipient Secrets

Each age public key the worker should encrypt to lives in its **own**
Secret labeled `backup.mogenius.io/role=age-recipient`. The operator's
`RecipientReconciler` watches them and materialises a single merged
Secret (named by the operator's `AGE_SECRET_NAME` env, default
`<release>-age`) which worker pods mount via `secretKeyRef`. This
matches the source/destination discovery model — drop a labeled Secret,
the operator picks it up.

| Annotation | Effect |
|---|---|
| `backup.mogenius.io/name` | Logical recipient name; surfaced in events and the UI Age-Keys page. |

| Data key | Required | Notes |
|---|---|---|
| `public-key` | **yes** | A single age recipient line (`age1...`). Newline-separated multi-key strings are *not* supported here — one Secret per key. |

How they're created:

- **Helm install bootstrap:** the chart fans out `agePublicKeys` (string or array) into one labeled Secret per entry, named `<release>-recipient-<index>`.
- **UI Age-Keys page:** create-on-add, delete-on-remove. Names follow `backup-recipient-<sha256-prefix>` so re-adding the same key is idempotent.
- **GitOps:** apply your own labeled Secret manifests under `recipients/`. The operator picks them up like any other.

Removing the last recipient is refused by the UI — leaving zero
recipients makes the worker unable to encrypt at all. Apply via
`kubectl delete` bypasses that guard at your own risk.

---

## 7. Configuration Reference

The operator and the worker have separate (overlapping) config schemas. All values are env vars; the helm chart wires them.

### Operator (`cmd/main.go`)

| Key | Required | Default | Effect |
|---|---|---|---|
| `WATCH_NAMESPACE` | no | release namespace | Namespace cache scope |
| `POD_NAMESPACE` | no | (downward API) | Lease namespace for leader election |
| `LEADER_ELECTION_ID` | no | — | Empty = leader election disabled |
| `DEFAULT_SCHEDULE` | no | `0 2 * * *` | Fallback schedule for sources without annotation |
| `RUN_TIMEOUT_SECONDS` | no | `3600` | Set as `activeDeadlineSeconds` on every Job |
| `TEMP_DIR` | no | `/tmp/backup-operator` | Mount path inside worker pods |
| `TEMP_DIR_SIZE` | no | `10Gi` | `emptyDir.sizeLimit` on worker pods |
| `DEFAULT_RETENTION_DAYS` | no | `30` | Fallback for sources without annotation |
| `DEFAULT_MIN_KEEP` | no | `3` | Fallback for sources without annotation |
| `WORKER_IMAGE` | **yes** | — | Container image for worker pods (Helm sets to operator's image) |
| `WORKER_IMAGE_PULL_POLICY` | no | `IfNotPresent` | |
| `WORKER_SERVICE_ACCOUNT` | **yes** | — | SA bound to worker pods (separate from operator SA, minimal privileges) |
| `AGE_SECRET_NAME` | **yes** | — | Secret holding `AGE_PUBLIC_KEYS` for worker pods to mount |
| `WORKER_CPU_LIMIT` | no | `2000m` | CPU limit for worker pods |
| `WORKER_MEMORY_LIMIT` | no | `2Gi` | Memory limit for worker pods |
| `WORKER_CPU_REQUEST` | no | `250m` | CPU request for worker pods |
| `WORKER_MEMORY_REQUEST` | no | `256Mi` | Memory request for worker pods |
| `WORKER_BACKOFF_LIMIT` | no | `2` | K8s-native Job retry limit on failure. Initial attempt + N retries with exponential backoff (10s, 20s, 40s, ..., capped at 6m). Catches transient failures (DB blips, brief network partitions) so a 30-second outage during the cron tick doesn't cost a full cron interval of missing backups. `0` disables. Range: 0–10. |
| `METRICS_REFRESH_INTERVAL_SECONDS` | no | `30` | Tick interval of the `MetricsRefresher`. Floor: 5. Trade off frequency against destination read load. |
| `STORAGE_SCRUB_ENABLED` | no | `false` | Enables the periodic storage scrubber that re-hashes the most recent dump per (target, destination) and compares with `meta.json`'s SHA256. Off by default because each scrub re-streams a full encrypted dump. Surfaces as `backup_operator_storage_scrub_passed` and the `BackupStorageCorrupted` alert. |
| `STORAGE_SCRUB_INTERVAL_HOURS` | no | `24` | How often the scrubber runs. Lower bound: 1. Multiply by `(targets × destinations)` to estimate egress; for a fleet with large dumps that's a real bandwidth budget. |
| `UI_ENABLED` | no | `false` | Enable the built-in web dashboard and management API on `UI_ADDR`. |
| `UI_ADDR` | no | `:8081` | Listen address for the UI HTTP server. |
| `UI_MAX_BODY_BYTES` | no | `1048576` | Per-request body cap applied via `http.MaxBytesReader` to every UI route. Without this an unauthenticated POST of a multi-GB body OOMs the operator. |
| `UI_MAX_SSE_CLIENTS` | no | `256` | Concurrent SSE subscribers allowed on `/api/events`. Excess clients receive `503` so they retry instead of pinning operator memory. |
| `PROMETHEUS_URL` | no | — | When set (e.g. `http://prometheus-operated.alert.svc:9090`), `/api/alerts` proxies `/api/v1/alerts` filtered to `alertname=~"^Backup.*"` so the UI mirrors what Alertmanager will route. When unset, the UI falls back to a local heuristic over the operator's own metric registry — useful during onboarding but does not honor the rule's `for:` duration. |
| `ALERTMANAGER_URL` | no | — | Used for the "open in Alertmanager" link on the Alerts page, the `/api/alerts/status` connectivity check (`GET /api/v2/status`), and the `/api/alerts/test` endpoint that sends a test alert (`POST /api/v2/alerts`). |
| `SETTINGS_CONFIGMAP` | no | — | Name of the ConfigMap for runtime-configurable settings via the UI wizard. Set automatically by Helm when `ui.enabled=true`. |
| `DOCS_ENABLED` | no | `false` | Enable the read-only documentation portal on `DOCS_ADDR`. Off by default — flip on to expose CLAUDE.md / README.md / generated tech-stack page. The docs server holds no credentials and reads no Kubernetes state, so it is safe to expose to a wider audience than the management UI. |
| `DOCS_ADDR` | no | `:8083` | Listen address for the docs portal. Distinct port so cluster admins can scope ingress separately from the mutating UI. |
| `DOCS_DIR` | no | `/app/docs` | Directory holding `CLAUDE.md`, `README.md`, `go.mod`. Populated by the Dockerfile at image build time. Locally, point at the repo root via `DOCS_DIR=..` for `just run`. |

### Worker (`cmd/worker/main.go`)

| Key | Required | Default |
|---|---|---|
| `AGE_PUBLIC_KEYS` | **yes** | — |
| `RUN_TIMEOUT_SECONDS` | no | `3600` |
| `TEMP_DIR` | no | `/tmp/backup-operator` |
| `DEFAULT_RETENTION_DAYS` | no | `30` |
| `DEFAULT_MIN_KEEP` | no | `3` |
| `DEFAULT_SCHEDULE` | no | `0 2 * * *` (parser needs it for fallback) |
| `POD_NAMESPACE` | recommended | — (or pass via `--namespace`) |

### Restore (`cmd/restore/main.go`)

CLI flags only:

| Flag | Required | Default |
|---|---|---|
| `--storage-secret` | **yes** | — |
| `--target` | **yes** | — |
| `--namespace` | no | `default` |
| `--age-key` | yes (for download) | — |
| `--timestamp` | no | latest |
| `--list` | no | `false` |
| `-o` | no | `-` (stdout) |
| `--decompress` | no | `false` |

---

## 8. Coding Conventions

### 8.1 Everything is Generic — Program to Interfaces

Any component that touches a database, storage backend, encryption, or external system is an interface. Add new types by implementing the interface and registering it in the factory; **never branch on type strings outside the factory**.

Existing extension points:

| Interface | When to extend |
|---|---|
| `dumper.Dumper` | Adding a new database engine |
| `storage.Storage` | Adding a new upload backend |
| `crypto.Encryptor` / `crypto.Decryptor` | If we ever support a non-`age` scheme |
| `analyzer.Analyzer` | Stricter or more flexible diff rules |
| `backup.DestinationProvider` | Currently the worker uses a static list; the interface exists to keep the pipeline testable |

### 8.2 Factory Pattern is Mandatory

`dumper/factory/factory.go` and `storage/factory/factory.go` are the **only** places that branch on type strings:

```go
switch dbType {
case "postgres": return postgres.New(cfg, log), nil
case "mysql":    return mysql.New(cfg, log), nil
case "mongo":    return mongo.New(cfg, log), nil
default:         return nil, fmt.Errorf("unsupported db-type %q", dbType)
}
```

If you find yourself writing `if storage.Type() == "s3"` or similar in calling code, that's a smell — it belongs in the factory or as a method on the interface.

### 8.3 Configuration Access

- Declare new config values in the relevant binary's `InitializeConfigModule` call (`Optional`/`Default`/`Validate`).
- Access via `config.GetValue(KEY)` — never `os.Getenv` directly outside `cmd/`.
- The schema in `main.go` is the single source of truth for what the binary accepts.

### 8.4 Error Handling

- `fmt.Errorf("context: %w", err)` for normal wrapping.
- `assert.NoError()` / `assert.Assert()` only for unrecoverable startup failures (config init, manager creation).
- **Per-destination upload errors do NOT abort the whole run** — they are surfaced via `backup_operator_destination_failed`. A single bad destination cannot prevent all backups.
- **Retention errors do NOT fail the run** — old dumps are best-effort, fresh dumps are mandatory.
- **Stats collection errors do NOT fail the run** — the analyzer just skips the comparison.

### 8.5 Concurrency

- The pipeline writes the encrypted dump to a single temp file once, then fans out to N destinations in parallel (`sync.WaitGroup`).
- The worker is one-shot; no inter-run concurrency to worry about within a pod.
- Operator replicas race on `CreateOrUpdate` → harmless, last write wins. Leader election is still set so unnecessary work is minimised.
- K8s CronJob `concurrencyPolicy: Forbid` prevents *overlap* of runs against the same source — a 6-hour dump under an hourly schedule simply skips ticks until it finishes.

### 8.6 Logging

Use the `logr.Logger` interface throughout (injected, never global).

- `logger.Info(...)` for normal operational events
- `logger.V(1).Info(...)` for verbose/debug output
- `logger.Error(err, ...)` for errors with context
- Never `fmt.Println` or `log.Print` in production code paths

### 8.7 Tests

- The analyzer (`analyzer/`), the parser (`internal/secrets/`), the retention selector (`internal/backup/retention.go`), and the SFTP host-key callback (`storage/sftp/`) all have unit tests. Pure-function logic is the right place for tests; integration with real DBs and real storage is left to the cluster.
- `go test ./...` from `src/` is the gate. CI runs `just check` (vet + lint + test).

### 8.8 Comments

Comments explain **why**, not what. The expected reader knows Go and Kubernetes.

Good targets for a comment:

- A non-obvious workaround (e.g. "knownhosts.New only takes file paths, so we materialise...")
- A subtle invariant (e.g. "Object.Path is already prefix-stripped — passing it back to Get round-trips correctly")
- An intentional trade-off (e.g. "estimates from pg_stat_user_tables; exact COUNT(*) would be cost-prohibitive on large tables")

Avoid restating the code. Avoid comments that reference the current change ("added for issue #123") — those belong in the commit message.

---

## 9. Adding a New Backend

### 9.1 New database type

1. Create `src/dumper/<name>/<name>.go` implementing `dumper.Dumper`:
   - `Type() string` → return the type-string used in the label
   - `Dump(ctx, w io.Writer) error` → exec the dump tool, stream to `w`
   - `CollectStats(ctx) (*Stats, error)` → query the live DB for table-level rows + size and a schema fingerprint
2. Register the type-constant in `src/dumper/factory/factory.go`. If your new
   type can reuse an existing dumper (e.g. MariaDB shares MySQL's wire
   protocol and `mysqldump`), share the case rather than duplicating code:
   ```go
   case TypeMySQL, TypeMariaDB:
       return mysql.New(cfg, logger.WithName(dbType)), nil
   ```
3. Update the `Dockerfile` to install the matching client tool (`apk add ...`).
4. Document the type in section 6.1 of this file.

The DB driver is consumed only by `CollectStats`; if you can live without semantic alerts on the new type, returning `nil, fmt.Errorf("not implemented")` from `CollectStats` is acceptable as a stage-one ship — the dump still works, the analyzer just stays quiet.

### 9.2 New storage backend

1. Create `src/storage/<name>/<name>.go` implementing `storage.Storage`:
   - `Name() string`
   - `Upload(ctx, path string, r io.Reader) error`
   - `List(ctx, prefix string) ([]Object, error)` — **must return prefix-stripped logical paths**
   - `Get(ctx, path string) (io.ReadCloser, error)`
   - `Delete(ctx, path string) error`
2. Register the type-constant in `src/storage/factory/factory.go`.
3. Document the data-key schema in section 6.5.

If your backend has its own native encryption (e.g. server-side S3 encryption), still go through `age` first. The whole point of public-key encryption is that the cluster can't read its own backups; relying on the storage provider's keys breaks that.

---

## 10. Encryption Model

```
Operator's machine (offline):
  age-keygen -o age.key       →   ~/age.key
                                  ├── public:  age1qx...
                                  └── private: AGE-SECRET-KEY-1...

Cluster (online):
  Helm install --set agePublicKeys[0]=age1qx...
   └── creates one labeled Secret per recipient (role=age-recipient)
        └── operator's RecipientReconciler watches them
             └── materialises a merged Secret <release>-age with
                 AGE_PUBLIC_KEYS = newline-joined recipient list
                  └── mounted into every worker pod via secretKeyRef

Worker pod runtime:
  cmd/worker reads AGE_PUBLIC_KEYS env
   └── crypto.NewFromPublicKeys parses recipients
        └── pipeline writes:  pg_dump | gzip | ageEncrypt(recipients...) | tempfile

Restore:
  backup-restore --age-key ~/age.key
   └── crypto.NewDecryptorFromKeys parses identities
        └── reads ciphertext from storage, decrypts to stdout
```

**Rules the code enforces:**

- Worker refuses to start without `AGE_PUBLIC_KEYS`. There is no plaintext-backup code path.
- The age recipient list is newline-separated → supports recipient rotation (multiple public keys can decrypt; you can rotate by adding a new recipient and later retiring the old).
- The restore CLI accepts the same multi-key format → matches `age-keygen -o`'s output.
- Storage backends never see plaintext bytes; they receive `*.sql.gz.age` ciphertext only.

**Rules you enforce operationally:**

- Keep the private key offline. The whole security model breaks if it ends up in the cluster.
- Back up the private key separately (paper, hardware token, password manager). Losing it means losing every backup it can decrypt.
- For multi-region recovery, distribute multiple public keys to the cluster; each region's operator keeps its own private key.

---

## 11. Storage Layout

Every dump produces **two** objects per run:

```
<path-prefix>/<target>/<YYYY>/<MM>/<DD>/dump-<timestamp>.sql.gz.age   (encrypted)
<path-prefix>/<target>/<YYYY>/<MM>/<DD>/dump-<timestamp>.meta.json    (plaintext)
```

| Object | Contents | Why this format |
|---|---|---|
| `dump-<ts>.sql.gz.age` | gzipped DB dump, age-encrypted | The actual backup payload |
| `dump-<ts>.meta.json` | target name, db-type, encrypted size, SHA256, run start (`timestamp`), `completedAt` + `durationSeconds` (wall-clock duration), full Stats, full analyzer Report, dump verification, restore verification, per-destination upload results (`destinations` array with name, storageType, status, error), per-destination retention sweep results (`retention` array with name, status, deletedDumps, deletedMetas, error — pre-upload sweep only) | Lets the next run compute diffs without restoring; lets humans audit without the private key; per-destination results enable multi-storage health monitoring; `durationSeconds` feeds the UI's Jobs progress-bar estimate (median over last N successful runs); `retention` feeds the operator's retention gauges and the `BackupRetentionFailing` alert |

**Timestamp format:** `20060102T150405Z` (Go reference time, ISO-like, lexically sortable).

**The meta file is intentionally unencrypted.** Anyone with read access to the bucket can see schema fingerprints and row counts, but never the data itself. If that's not acceptable for your environment, plan to encrypt the meta files in a follow-up — the trade-off is that automated diffing then needs the private key in the cluster.

---

## 12. Metrics Catalog

Exposed by the operator pod on `:8080/metrics`. **Worker pods are short-lived** — Prometheus cannot scrape them in time, so the run-level metrics are reconstructed by the operator's `MetricsRefresher` (`controllers/metrics_refresher.go`). It runs on a tick (default 30s, see `METRICS_REFRESH_INTERVAL_SECONDS`), lists Source Secrets in the watch namespace, fetches the most recent `*.meta.json` from each allowed destination, and writes the result into the operator's local Prometheus registry. That is why everything below is a Gauge — counters would require an always-on producer the worker cannot provide.

The histograms (`dump_duration_seconds`, `upload_duration_seconds`, `run_duration_seconds`) are kept in the worker for code-coupling reasons but their samples never reach Prometheus. For run-level timing, the operator-side aggregator now reconstructs `last_run_duration_seconds` as a Gauge from each successful run's meta.json — that is the metric to query for trend alerts. The dump and per-destination upload phase histograms remain a known gap; if you need per-phase distribution, see the §18 ADR on OTel as a future second-track export.

`metrics.Register(reg)` also stashes the registry as a `prometheus.Gatherer` (`metrics.Gatherer()`), which the alerts package's `LocalProvider` reads to re-evaluate the chart's PrometheusRule conditions inline — this is the no-Prometheus-configured fallback path for the UI's `/api/alerts` endpoint (see §13 and the §18 ADR on alerts providers).

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
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts parsed from the most recent meta.json found at that destination |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if the destination's storage cannot be initialised, 0 once a meta.json was successfully read |
| `backup_operator_analyzer_baseline_unavailable` | Gauge | `target` | 1 when the worker tried to read the analyzer baseline but every destination errored before any could be read; 0 otherwise (including the legitimate first-run case where no baseline exists yet). Distinguishes "first run, nothing to compare" from "fleet-wide storage outage, analyzer running blind". |
| `backup_operator_retention_last_status` | Gauge | `target`, `destination` | 1 if the most recent pre-upload retention sweep for this pair succeeded, 0 otherwise. Reconstructed by the operator from the `retention` block in `meta.json`. Absent until at least one sweep has run. Drives the `BackupRetentionFailing` alert. |
| `backup_operator_retention_last_deleted_count` | Gauge | `target`, `destination` | Number of dump artifacts deleted by the most recent sweep (excludes meta sidecars). Lets dashboards show "we are actively trimming" vs. "retention runs but nothing to do". |
| `backup_operator_storage_scrub_passed` | Gauge | `target`, `destination` | 1 if the most recent scrub of this pair matched the recorded SHA256, 0 otherwise. Only present when `STORAGE_SCRUB_ENABLED=true` and at least one scrub has run. |
| `backup_operator_storage_scrub_last_check_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts of the most recent scrub attempt for this pair |
| `backup_operator_storage_scrub_failed_total` | Counter | `target`, `destination` | Cumulative scrub failures (mismatch or unreachable). Operator-side, scraped normally. |
| `backup_operator_restore_verification_passed` | Gauge | `target`, `mode` | 1 if the most recent restore-verifier run for this target+mode produced verdict `match`, 0 if `mismatch`/`skipped`. Absent until a verifier has run at least once — present-with-zero is meaningful, absent is "not configured / not yet due". |
| `backup_operator_restore_verification_last_timestamp_seconds` | Gauge | `target`, `mode` | Unix ts of the most recent restore-verifier completion for this pair. Drives the `BackupRestoreVerificationStale` alert. |
| `backup_operator_restore_verification_duration_seconds` | Histogram | `target`, `mode` | Worker-only; not visible to Prometheus (same as the dump/upload histograms). |

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
| `BackupStorageCorrupted` | `storage_scrub_passed == 0` | **critical** |
| `BackupRetentionFailing` | `retention_last_status == 0` for 24h | warning |
| `BackupAnomaliesAppearing` | `last_run_anomalies > 0` for 5m | warning |
| `BackupLastRunFailed` | `last_run_status == 0` for 5m | warning |
| `BackupSucceeded` | `time() - last_success_timestamp_seconds < 120` | info |
| `BackupRestoreVerificationFailed` | `restore_verification_passed == 0` for 5m | **critical** |
| `BackupRestoreVerificationStale` | `time() - restore_verification_last_timestamp > 14d` | warning |

`BackupSucceeded` is a heartbeat-style positive signal (firing + resolved per run) — useful when you want a notification on every successful backup, but expect one firing + one resolved mail per completed run per target. With a frequent cron (e.g. every 5 min in the test stack), this is intentionally noisy.

The semantic alerts (`DumpSizeCollapsed`, `SchemaChanged`, `AnomaliesAppearing`) are the project's main differentiator vs K8up — they alert on *content*, not just job exit code.

These same conditions also surface in the operator UI under `/api/alerts` and `#/alerts` — Prometheus-backed when `PROMETHEUS_URL` is set, otherwise re-evaluated locally over the registry returned by `metrics.Gatherer()`. The local path does NOT honour `for:` debounce; treat the in-UI list as advisory and Alertmanager as the audit-grade source. See the §18 ADR on alert providers.

---

## 14. Failure Modes

| Failure | What happens | What the operator sees |
|---|---|---|
| DB unreachable | `pg_dump` exits non-zero, pipeline returns error, worker exits 1. K8s retries the Job up to `WORKER_BACKOFF_LIMIT` times with exponential backoff before giving up | If the DB recovers within ~6 min the retry wins and a normal meta.json appears. Otherwise `kubectl get jobs` shows failed after all retries; `last_success_timestamp_seconds` stops advancing → eventually `BackupOverdue` |
| One destination down (e.g. SFTP host offline) | Other destinations upload normally, run succeeds | Refresher cannot read latest meta from that destination → `destination_failed{destination=sftp} = 1`, `BackupDestinationFailing` fires |
| All destinations down | Worker exits 1 (no successful upload) | Job failed; no destination has a fresh meta → `BackupOverdue` after 36h |
| `CollectStats` fails (no perms) | Analyzer skips, dump still succeeds | meta.json still written without stats; `schema_changed` / `table_count` stay at their last values |
| Dump shrinks 90% | Run succeeds (dump is what it is) | `BackupDumpSizeCollapsed` fires within 5 min |
| Schema changed | Run succeeds | `BackupSchemaChanged` fires; `schema_last_change_timestamp_seconds` jumps to current run |
| Charset/collation drift | Run succeeds | `BackupCharsetChanged` fires (warning) — restore may silently truncate multibyte chars |
| `mysqldump` exits 0 with empty data (perm denial) | Pipeline detects dump rows == 0 vs pre-stats > 0, fails with `dump-empty-content`, persists failure-meta | `BackupLastRunFailed` fires; Verification has `looksEmpty=true`; opt out per source via `backup.mogenius.io/empty-dump-check=false` |
| `mongodump` / `redis-cli --rdb` exit 0 with tiny output | Heuristic: encrypted size < 1% of preStats source size (mongo) or < 200 bytes with > 0 keys (redis) → `dump-empty-content` failure | Same as above; opt-out annotation works identically |
| Stored dump bit-rots | Scrubber re-hash mismatches meta SHA256 | `storage_scrub_passed=0` → `BackupStorageCorrupted` (critical); only fires when `STORAGE_SCRUB_ENABLED=true` |
| Encrypted dump won't decrypt+parse (verification due) | Worker generates ephemeral keypair, encrypts with DR+ephemeral recipients, re-streams the local file through age-decrypt → gunzip → parser, finds garbage / wrong header / empty content | `restore_verification_passed=0`, `BackupRestoreVerificationFailed` (critical). Run still succeeds — verification is observability, not a gate. The DR recipient remains valid; the offline restore path is unaffected. |
| Verifier itself dies (decryptor init / open file) | `RestoreVerificationResult` written with `Verdict=skipped` and `Error` populated | Same alert path; treated same as a hard mismatch from an operator's perspective (something needs investigation). |
| Run takes >`RUN_TIMEOUT_SECONDS` | K8s kills the pod via `activeDeadlineSeconds` | Job failed; configure higher timeout for big DBs |
| Two cron ticks overlap (long run) | Second tick is **skipped** by `concurrencyPolicy: Forbid` | No second Job created; run continues |
| Source Secret deleted | `OwnerReference` cascades; CronJob deleted by GC | No more runs; existing artifacts in storage untouched |
| Role label removed (Secret kept) | Reconciler observes label transition and deletes the CronJob | Same as above for scheduling |
| Worker pod evicted mid-run | Job fails; next tick produces a fresh run | Partial uploads to destinations may exist (they have their own object names per timestamp, so no clashes) |
| Retention can't delete (perms / list / session) | Old dumps remain; the run still succeeds (retention is best-effort) | Pipeline records the failure into the `retention` block of `meta.json`. Operator's `MetricsRefresher` reads it back and sets `retention_last_status{target,destination}=0`. After 24h of persistent failure → `BackupRetentionFailing` (warning) — long enough to ride out a single transient error, short enough to surface real problems before storage fills. Each pre/post-upload failure step (`init storage`, `open session`, `list`) also emits a Warning `RetentionFailed` Event tagged with the phase, so the cluster audit trail shows the failure even when the meta.json wasn't yet uploadable (post-upload phase). |
| Analyzer baseline unreachable (every destination down at baseline-read time) | Run still succeeds; analyzer just skips the comparison this round | `analyzer_baseline_unavailable{target}=1`, plus per-destination V(1) log lines from the worker. Cleared (back to 0) on the next run that successfully reads any meta. Distinguishes silently from the "first run, no baseline yet" case (which keeps the gauge at 0). |
| Long-lived operator goroutine panics (refresher, scrubber, UI handler) | `safe.Goroutine` recovers and logs an Error with `phase`, `key`, and the captured stack — the operator pod stays up | Single panic no longer crashes the operator (which would take down all reconcilers + the UI). Used by every long-lived goroutine; trivially auditable via grep `safe.Goroutine`. |
| `known-hosts` mismatch | `ssh.NewClientConn` fails before any data leaves | Run fails; worker logs the host-key error |
| `known-hosts` missing | Connection rejected unless `insecure-skip-host-verify: "true"` is set in destination Secret data | Run fails with descriptive error; set `insecure-skip-host-verify` or populate `known-hosts` via `ssh-keyscan` |
| Helm upgrade from pre-RecipientReconciler chart | Helm deletes the legacy single `<release>-age` Secret as part of removing it from its manifest set; the new operator pod runs `Bootstrap` on startup which re-materialises the merged Secret from per-recipient Secrets | Brief gap (a few seconds) between Helm-delete and operator-recreate. A CronJob tick that races into the gap fails with `secret not found`; the next tick succeeds. One-time on the first upgrade; subsequent upgrades are gap-free. |

---

## 15. Common Operations

### Trigger a manual run

```bash
kubectl -n backup create job --from=cronjob/backup-prod-users-db \
  manual-$(date +%s)
```

The Job runs the same worker code as a scheduled run; metrics, retention, fan-out all behave identically.

### Suspend a backup temporarily

Three equivalent ways:

```bash
# 1. Annotation on the source Secret (canonical — survives CronJob recreation)
kubectl -n backup annotate secret prod-users-db \
  backup.mogenius.io/suspended="true" --overwrite

# 2. UI: Pause / Resume button on the source card
# 3. UI API: POST /api/sources/{secret-name}/suspend  body: {"suspend": true}
```

The reconciler reads `backup.mogenius.io/suspended` and writes `Spec.Suspend` on the managed CronJob. **Manual `kubectl patch cronjob ... suspend=true` is overridden on the next reconcile** — the Secret is the source of truth. Use the annotation or the UI instead. Manual triggers (`kubectl create job --from=cronjob/...` and the UI's Run button) ignore Suspend, so paused sources can still be exercised on demand for restore drills. To resume: remove the annotation (preferred) or set it to `false`.

### Change the schedule

```bash
kubectl -n backup annotate secret prod-users-db \
  backup.mogenius.io/schedule="*/15 * * * *" --overwrite
```

The reconciler observes the change, patches the CronJob within seconds.

### Disable analyzer for one source

```bash
kubectl -n backup annotate secret prod-users-db \
  backup.mogenius.io/analyzer-enabled="false" --overwrite
```

Useful when the backup user lacks `pg_stat_*` access.

### Route one DB to a single destination

```bash
kubectl -n backup annotate secret prod-users-db \
  backup.mogenius.io/destinations="hetzner-offsite" --overwrite
```

The name is matched against the destination Secret's `backup.mogenius.io/name` annotation (or its Secret name if the annotation is absent).

### Inspect a run's metadata without restoring

```bash
backup-restore --storage-secret hetzner-sb -n backup --target prod-users \
  --age-key ~/age.key --list
# Pick a timestamp, then fetch the meta file via your storage's CLI:
# (the meta is unencrypted; a normal s3/sftp client retrieves it directly)
```

### Restore to a fresh database

```bash
backup-restore --storage-secret hetzner-sb -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h fresh-host new_database
```

---

## 16. Development Workflow

```bash
# Build all three binaries natively
just build           # operator
just build-worker    # worker
just build-restore   # restore CLI

# Tidy + vet + lint + test
just check

# Just tests
just test-unit

# Module hygiene
just tidy

# Build a multi-arch docker image (locally with buildx)
just build-docker ghcr.io/you/backup-operator amd64
```

**Verifying the image runs locally** (without K8s):

```bash
# Operator alone — needs k8s API; use a kind/minikube cluster
docker run --rm -e WORKER_IMAGE=... -e WORKER_SERVICE_ACCOUNT=... \
  -e AGE_SECRET_NAME=... ghcr.io/.../backup-operator:dev

# Worker against a real Postgres (smoke test)
docker run --rm \
  -e AGE_PUBLIC_KEYS="age1..." \
  -v $PWD:/work -w /work \
  ghcr.io/.../backup-operator:dev /app/backup-worker --help
```

### Local Test Setup (Docker Desktop K8s)

End-to-end smoke test with operator running locally and the worker pod
running inside Docker Desktop's Kubernetes. Docker Desktop shares its
image store with the cluster, so a `docker build` is immediately visible
to K8s — no registry, no `kind load`. The worker pod runs with
`imagePullPolicy: Never` so it never tries to pull the local tag.

```bash
# 1. Copy the env template and review (defaults match the test stack)
cp .env.example .env

# 2. Build the worker/operator image into the local Docker daemon
just build-image

# 3. Generate an age key pair offline (idempotent — runs once)
#    Public key is created as Secret backup-operator-age in the namespace.
just gen-age-key

# 4. Apply the test stack: namespace, worker SA/RBAC, in-cluster Postgres,
#    in-cluster MinIO with bucket init, source + destination Secrets.
just test-up

# 5. Run the operator locally — talks to the cluster via your kubeconfig.
#    It produces a CronJob `backup-test-postgres` from the source Secret.
just run

# 6. In another terminal: trigger a run without waiting for the schedule.
just test-trigger

# 7. Inspect: kubectl -n backup get jobs/pods,
#    kubectl -n backup logs -l job-name=manual-...
#    Browse MinIO at http://localhost:<console-port> via port-forward.

# Cleanup
just test-down
```

What lives where in this setup:

- The **operator** runs as `dist/native/backup-operator` on your machine.
  It authenticates as your kubeconfig user (Docker Desktop admin), so
  it does not need its own ServiceAccount — only the worker does.
- The **worker pod** runs inside the cluster with SA `backup-worker`,
  pulls `backup-operator:dev` from the local Docker daemon, mounts
  `AGE_PUBLIC_KEYS` from the `backup-operator-age` Secret, and dumps
  Postgres → encrypts → uploads to MinIO at `s3://backups/...`.
- The **age private key** stays at `~/age-backup-test.key`, never in
  the cluster — same security model as production. To download and
  decrypt a dump, use the `backup-restore` binary with `--age-key`.

---

## 17. Data Flow & Compliance

This section documents the complete data lifecycle for compliance audits (DSGVO/GDPR Art. 30, SOC2).

### 17.1 Data at Rest

| Location | Contents | Encryption | Retention |
|---|---|---|---|
| Source Secret (K8s) | DB credentials | Kubernetes Secret encryption (etcd) | Cluster lifecycle |
| Destination Secret (K8s) | Storage credentials (SSH keys, S3 keys) | Kubernetes Secret encryption (etcd) | Cluster lifecycle |
| Worker temp volume (`/tmp`) | Encrypted dump file (`.sql.gz.age`) | `age` public-key (X25519) | Pod lifecycle (emptyDir) |
| Storage backend (SFTP/S3) | Encrypted dump + unencrypted `meta.json` | `age` public-key (X25519) | `retention-days` annotation |
| Operator machine (offline) | `age` private key | Operator responsibility | Manual |

### 17.2 Data in Transit

| Path | Protocol | Encryption |
|---|---|---|
| Worker → Database | DB-native (pg, mysql, mongo) | TLS if configured via `extra-sslmode` / DB driver |
| Worker → SFTP destination | SSH (SFTP subsystem) | SSH transport encryption |
| Worker → S3 destination | HTTPS | TLS 1.2+ |
| Operator → Storage (metrics refresh) | SSH/HTTPS (same as worker) | Same as worker |
| Operator → Prometheus (alerts status) | HTTP | Plain HTTP (cluster-internal) |
| Operator → Alertmanager (status check, test alerts) | HTTP | Plain HTTP (cluster-internal) |
| Operator → K8s API | HTTPS (in-cluster ServiceAccount) | mTLS |

### 17.3 Key Management

- **Public keys** (`age` recipients): one Secret per recipient (`role=age-recipient` label, `public-key` data field). The operator's `RecipientReconciler` materialises a merged Secret (`<release>-age`) that worker pods mount via `secretKeyRef`. Used only for encryption. Per-Secret form means lifecycle is decoupled from Helm releases (UI-added or GitOps-applied recipients survive `helm uninstall`).
- **Private key** (`age` identity): **never enters the cluster**. Lives on the operator's machine. Required only for `backup-restore` CLI.
- **SSH keys** (SFTP destinations): stored in destination Secrets. Scoped to individual storage backends.
- **S3 credentials**: stored in destination Secrets. Should use scoped IAM roles with minimal write permissions.

### 17.4 Audit Trail

| Event | Source | Visible via |
|---|---|---|
| `BackupStarted` | Worker pod | `kubectl describe secret <source>`, cluster audit log |
| `BackupCompleted` | Worker pod | Same |
| `BackupFailed` | Worker pod | Same, includes failure phase |
| `RetentionDelete` | Worker pod | Same, lists deleted artifact + phase (`pre-upload`/`post-upload`) |
| `RetentionFailed` | Worker pod | Same; Warning. Emitted when a retention step (`init storage`, `open session`, `list`) fails for one destination. Carries the phase so post-upload failures (which can't reach `meta.json`) are still auditable. |
| CronJob/Job status | Kubernetes | `kubectl get jobs`, kube-state-metrics |
| Prometheus alerts | Alertmanager | Alert history, notification channels |

### 17.5 Data Deletion

- **Automated**: Retention policy deletes dumps older than `retention-days`, respecting `min-keep` safety floor. Deletion events are recorded as Kubernetes Events.
- **Manual**: Delete the source Secret → OwnerReference cascades to CronJob → no new backups. Existing dumps in storage must be deleted manually from the backend.
- **Right to erasure (DSGVO Art. 17)**: Use `backup-restore --purge --target X --before YYYY-MM-DD` to delete all artifacts for a target from storage. Add `--dry-run` to preview. Without the private key, the dump is cryptographically inaccessible, but storage-level deletion may still be required by your DPO.

### 17.6 Access Control Summary

| Principal | Can access | Cannot access |
|---|---|---|
| Operator pod | Source/Dest Secrets (CRUD), CronJobs (CRUD), ConfigMaps (get/update/patch), Jobs (create), Leases, Events, Prometheus query API (read), Alertmanager API (read status + write test alerts) | Private key, dump contents |
| Worker pod | Source/Dest Secrets (read), Events (create), **Pods (create/delete) when `restoreVerification.enableEphemeralPodSpawn=true`** | CronJobs, Leases, private key |
| Storage backend | Encrypted dumps, unencrypted meta.json | Private key, DB credentials |
| Restore operator (human) | Private key, storage backend | Cluster Secrets (unless they have kubectl access) |
| Prometheus/Alertmanager | Metrics (sizes, counts, anomalies); operator reads status + posts test alerts | Dump contents, credentials |

---

## 18. Architectural Decisions

The notable ones, with the reasoning that future readers should preserve.

- **No CRDs.** Every backup operator the team has worked with has a CRD. They add a documentation surface, version-skew handling, and an installation step. Labelling a Secret is the entire contract here — the operator is just a reconciler that watches Secrets.

- **Operator does not run backups.** Earlier iteration ran them in goroutines under an in-process cron. That model couples backup capacity to the operator pod's resource limits, requires hand-rolled overlap protection, and makes per-run logs scattered. K8s CronJobs solve all three for free.

- **OwnerReference for cascade delete.** The reconciler does not bookkeep "Secret deleted → delete CronJob" explicitly; K8s GC does. This eliminates a class of stale-state bugs.

- **Label-transition predicate.** The reconciler does need to handle `role` label *removal* (Secret kept, label dropped) — that's an explicit user signal "stop backing this up." Without watching label transitions, an orphan CronJob would persist.

- **Stats from live DB, not parsed dump.** Dump formats differ across versions, vendors, and tools. Querying `pg_stat_user_tables` / `INFORMATION_SCHEMA.TABLES` / Mongo `collStats` is portable and orders of magnitude faster.

- **Sidecar `meta.json` is unencrypted.** The whole point of analyzer alerts is that they fire automatically without restore. Encrypting the meta would force the operator to hold the private key, which collapses the security model.

- **Single dump → fan-out via temp file.** Streaming the encrypted dump to N destinations simultaneously means the slowest destination throttles the dump phase. Materialising once locally costs `emptyDir` space but decouples destinations.

- **`Storage.List()` returns logical paths.** Storage implementations apply `pathPrefix` internally on `Upload`/`Get`/`Delete`. Returning raw server-side paths from `List` would break the round-trip (caller passes `Object.Path` back to `Get`, gets double-prefixed). This is enforced by per-implementation `stripPrefix` helpers.

- **Per-recipient Secrets via `role=age-recipient`, materialised by an operator reconciler.** Originally the chart shipped a single Secret containing all recipients newline-joined under `AGE_PUBLIC_KEYS`. Worker contract was clean, but the *human* contract had three rough edges: (a) UI-added keys lived inside a Helm-managed object, so `helm upgrade --reuse-values` could silently revert them; (b) lifecycle of "this person's recipient" was not a first-class object, only a substring of the central Secret; (c) the discovery story diverged from the rest of the system, where sources and destinations are picked up by labels. Refactor: each recipient is its own Secret labeled `role=age-recipient` with one `public-key` data field. A new `RecipientReconciler` watches them, sorts deterministically, joins `\n`, and writes the result to the operator-managed merged Secret named by `AGE_SECRET_NAME` — the very name the CronJob template already references via `secretKeyRef`. Worker contract therefore unchanged; what changed is *who creates and owns* the merged Secret. Migration: `Bootstrap()` runs at operator startup BEFORE the manager — if it sees a legacy single-Secret with `AGE_PUBLIC_KEYS` and zero per-recipient Secrets, it splits the legacy keys into per-recipient Secrets (idempotent: `AlreadyExists` on Create is treated as success). Helm-upgrade scenario: Helm deletes the old chart-managed merged Secret as part of removing it from its manifest set; operator's Bootstrap immediately re-materialises it from the per-recipient Secrets that the new chart created. Brief gap (a few seconds) on the first upgrade — documented in §14, accepted as a one-time migration cost rather than a permanent helm hook. Considered alternatives: (1) Helm `pre-upgrade` hook job that bridges the migration — rejected because it leaves permanent hook complexity in the chart for a one-time event; (2) renaming the merged Secret to a new name to avoid the helm-delete window — rejected because it would force every CronJob to be re-templated and re-applied. Trade-offs preserved: real names stay in Prometheus metrics (we never expose recipient *content* there anyway), the existing `agePublicKeys` Helm value still works (chart fans it out into N labeled Secrets), the UI Age-Keys page now CRUDs per-recipient Secrets directly so add/remove is atomic without RMW races on a shared blob.

- **`agePublicKeys` accepts both string and array in Helm values, normalised to newline-separated env-var.** The original interface was a single newline-separated string — clean for the worker (its env-var format is one big string anyway) but fragile at the operator-experience layer: `--set agePublicKeys="age1aaa\nage1bbb"` from the CLI silently produces a single bogus recipient because `\n` doesn't expand without double-quoting through several shells, and YAML multi-line block-scalar indentation is its own foot-gun. Worst case is loud (`crypto.NewFromPublicKeys` fails parse, worker crashes at startup) — not silent encryption to the wrong recipient — but "operator wrote two keys, only one took effect, runs all encrypt to one recipient" is plausible enough to be worth defending against. Fix: `secret-age.yaml` accepts either `string` or `[]string` via `kindIs "slice"`, joins arrays with `\n`, then `trimAll`s before the empty-check. Worker contract is unchanged. The string form is preserved for backward compatibility with installs that predate this; array form is recommended for multi-key rotation because `--set agePublicKeys[0]=age1qx... --set agePublicKeys[1]=age1yz...` works cleanly from any CI shell. The reviewer who flagged this also suggested making it array-only as a breaking change — the dual-form approach captures the ergonomic win at zero migration cost.

- **`age` over GPG.** `age` is purpose-built for streaming public-key encryption, has a clean Go library, and produces compact recipients. GPG carries decades of legacy and a much larger attack surface for a problem we don't have.

- **No notifier built-in.** The cluster already has Alertmanager. Building a Slack/Email notifier would re-invent routing, deduplication, and silencing. Shipping `PrometheusRule` defaults instead is the right interface.

- **Three binaries, one image.** Two-binary distribution per service is awkward. A single image with two `cmd/` entrypoints means one CI build, one registry tag, one version to track. The 30 MB difference between the operator binary and the worker binary is irrelevant.

- **Restore is a separate binary.** It runs on the operator's machine, never in cluster — that's the only place the private key should ever be. Bundling it into the operator image would tempt people to mount the private key in the cluster "for convenience," which would defeat the entire encryption design.

- **Canonical `MetaFile` in `internal/meta`, not per-consumer copies.** The pipeline, metrics refresher, and UI all deserialise the same `*.meta.json` sidecar. Three private structs drifted independently (different field subsets, no shared methods). Consolidating into `internal/meta.MetaFile` gives them `IsFailure()` and `ParsedTimestamp()` for free and eliminates a class of serialisation-mismatch bugs.

- **`metrics` package, not `metricStore`.** Go convention is lowercase single-word package names. The rename also narrows `Register()` from `ctrlmetrics.RegistererGatherer` to `prometheus.Registerer`, decoupling the metrics layer from controller-runtime so it can be reused in non-operator contexts (e.g. a future standalone worker metrics endpoint).

- **`BatchStorage` interface for connection reuse.** SFTP operations (List, Delete) each opened a fresh SSH connection. During retention — one List + N Deletes — this meant N+1 handshakes. Rather than embedding pooling into the Storage interface (which S3 doesn't need), we added an optional `BatchStorage` interface with `WithSession()`. Callers type-assert and get a reusable session. This keeps the base `Storage` interface minimal while giving SFTP a proper batch path.

- **Structured error types in the pipeline.** Upload failures are `RetryableError` (transient network issues), storage-init failures are `PermanentError` (bad credentials), and config issues are `ValidationError`. This lets the fan-out distinguish error classes for logging and future retry logic, without changing the existing best-effort error handling contract.

- **Post-upload size verification.** After uploading a dump, the pipeline Lists the uploaded path and compares the remote object's size to the local file. This catches silent truncation or partial writes. If the List itself fails (not all backends support prefix-exact listing), verification is skipped rather than failing the backup — availability over strictness.

- **PSA-restricted SecurityContext on all pods.** Both operator and worker pods run with `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities: drop: ALL`, `seccompProfile: RuntimeDefault`, and `runAsNonRoot: true` (UID 1000). The worker writes only to its emptyDir temp volume. This passes Pod Security Admission in `restricted` mode, which is required for hardened production clusters.

- **Exponential retry on transient upload failures.** Upload operations tagged as `RetryableError` are retried up to 3 times with 2s/4s exponential backoff. `PermanentError` (bad credentials, missing bucket) aborts immediately. This makes the backup resilient to short network glitches without wasting time on configuration errors. Context cancellation is respected between retries.

- **SSH handshake timeout.** The SFTP `ssh.ClientConfig` sets `Timeout: 30s`. Without it, the SSH handshake (key exchange, auth) blocks indefinitely on an unresponsive server — the ctx-based TCP dialer only covers the initial connect, not the protocol handshake.

- **FTPS as a separate driver, not "SFTP with a TLS toggle".** A user with a QNAP that only advertises "FTP with SSL/TLS (explicit)" prompted this — same usage pattern as SFTP (NAS box, write a backup, read it back) but completely different wire protocol. Reusing the SFTP code path was never on the table: SFTP rides SSH (one encrypted bidirectional channel, port 22, public-key auth available), FTPS is FTP with a TLS upgrade (separate control and data sockets, port 21 explicit / 990 implicit, password-only auth, PASV firewall traversal). The Storage interface intentionally hides protocol — implementations only see byte-stream Upload/Get/List/Delete — so dropping in `storage/ftps/` was 300 lines and one factory line. PROT P (encrypt the data channel) is sent automatically by `jlaffaye/ftp` when a tls.Config is configured, so we can't accidentally ship plaintext data sockets even though FTPS technically allows it. Considered: (a) defer FTPS, tell users to enable SSH on the NAS; (b) build a generic "TLS-wrapped legacy protocol" abstraction. (a) is correct most of the time but doesn't help operators locked out of the device's SSH settings (compliance appliances, hosted NAS). (b) is over-engineered for two driver types; the protocols share so little at the wire level that the abstraction would be lipstick. Trade-off explicitly accepted: FTPS has worse NAT behaviour (PASV data sockets can be blocked) and weaker auth (no SSH-key equivalent), so the §6.5 doc recommends SFTP when both are available. Public-key auth was deliberately NOT bolted on — FTP doesn't have it in the spec, and emulating it via TLS client certificates would be a third auth path users would need to understand. Same reasoning as why we don't ship "FTP without TLS": if the user picked an FTP destination they want encryption, plaintext FTP would be a footgun, not a feature.

- **Worker resource limits via env vars.** `WORKER_CPU_LIMIT`, `WORKER_MEMORY_LIMIT`, `WORKER_CPU_REQUEST`, `WORKER_MEMORY_REQUEST` flow from Helm into every CronJob's container spec. Sensible defaults ship (2 CPU / 2Gi); empty disables. Without limits, a large dump can OOM the node.

- **K8s-native Job retries via `WORKER_BACKOFF_LIMIT` (default 2).** Previously hardcoded `BackoffLimit: 0` — one Job attempt, period. A 30-second DB blip during the cron tick meant no backup for a full cron interval (up to 24h on a daily schedule). The fix sets `BackoffLimit: 2` by default: initial attempt + 2 retries with K8s exponential backoff (10s, 20s, 40s, capped at 6m). Tradeoff considered: (a) typed error classification at the storage layer (sftp/s3 SDK error codes) to retry only transient errors; (b) leave it as-is and rely on the next cron tick. Picked (c) — dumb K8s-native retry: cheap, atomic at the pod level (a fresh worker pod re-resolves DNS, re-mounts age key, re-reads source secret), and bounded by `ActiveDeadlineSeconds` so permanent failures (wrong password) cost at most `(1 + N) × failure-time`. Concurrency stays safe: `concurrencyPolicy: Forbid` only blocks the *next scheduled tick*, not retries of the same logical run, which is exactly what we want. Range capped at 10 in the config validator so a typo can't accidentally turn a permanent-failure source into a thrash loop. Did not push classification down to storage backends (the alternative path) because the ADR's rationale for the existing `classifyUploadError` string-matching still holds: the marginal accuracy gain doesn't justify touching every backend, and false-classification cost was already bounded.

- **Kubernetes Events as audit trail.** The worker emits `BackupStarted`, `BackupCompleted`, `BackupFailed`, and `RetentionDelete` events against the source Secret. Visible via `kubectl describe secret <source>` and preserved in cluster audit logs. Satisfies DSGVO Art. 30 and SOC2 requirements. The pipeline uses an `EventEmitter` interface so tests stay API-server-free (`NoopEventEmitter`). RBAC grants `events: create, patch` to the shared ServiceAccount.

- **Health probes on the operator.** controller-runtime's built-in `/healthz` and `/readyz` are served on `:8082` (separate from metrics `:8080` and UI `:8081`). Without probes, Kubernetes cannot detect a stuck operator and restart it.

- **Operator-side metric aggregation, not Pushgateway.** Backup metrics are produced by short-lived worker pods that Prometheus cannot scrape in time. Three options were considered: (a) Pushgateway, (b) operator aggregates from `meta.json`, (c) drop semantic alerts and rely on kube-state-metrics for Job status. We picked (b): the operator's `MetricsRefresher` controller polls each destination's latest meta.json and writes the result into the operator's local registry. Pushgateway adds a stateful component with known counter-staleness footguns; (c) sacrifices the project's core differentiator (semantic alerts on dump *content*). Aggregating from storage reuses the artifacts we already produce and keeps the system stateless apart from the operator pod itself. Counter-style metrics (`runs_total`, `anomalies_total`) are converted to Gauges (`last_run_status`, `last_run_anomalies`) because monotonic counters require a continuously running producer; reconstructing them from storage would require summing across the retention window and break whenever retention prunes a run.

- **Retention status as Gauge from meta.json, not the worker-side Counter pair.** Two `retention_*_total` Counters used to live in `metrics/metrics.go` (`retention_deleted_total`, `retention_failed_total`). Both were incremented by short-lived worker pods that Prometheus cannot scrape — the metrics never reached Alertmanager. The visible-from-storage failure mode was "retention has been silently failing for weeks; storage fills up; all backups suddenly stop". The fix: pipeline records per-destination outcome (status, deletedDumps, deletedMetas, error) into a new `retention` block on `meta.json` during the pre-upload sweep, and the operator's `MetricsRefresher` reads it back into `retention_last_status{target,destination}` and `retention_last_deleted_count{target,destination}` Gauges. The Counters are deleted (not kept around as dead code). Only the **pre-upload sweep** is captured — the post-upload sweep runs after the meta is already in storage, so its outcome would arrive too late for the same artifact. That's an acceptable gap because the pre-upload sweep is the load-bearing path: if it fails, storage fills; if post-upload fails, the next run trims one extra cohort. New default alert `BackupRetentionFailing` fires after 24h of `retention_last_status==0` — long enough to ride out a single transient error, short enough to surface persistent problems before disk fills. The local-evaluator path (`internal/alerts.LocalProvider`) mirrors the rule but without the 24h debounce — same constraint already documented for the other rules. The retention CRUD events (`RetentionDelete`) on the source Secret remain unchanged, so the K8s-Event audit trail is unaffected.

- **Run duration as a Gauge from meta.json, not as a histogram via Pushgateway.** Run timing was a known observability gap: `run_duration_seconds` (histogram) is observed in the worker but never reaches Prometheus. Reviewers naturally suggested filling it with Pushgateway or OTel. We picked the same pattern the operator-side aggregator already uses for `last_run_status` / `dump_size_bytes`: read `DurationSeconds` out of the most recent successful meta.json and expose it as `last_run_duration_seconds{target,db_type}`. Cost is one Gauge declaration plus three lines in `MetricsRefresher.refreshSource`; the meta.json field already existed (it powers the UI's progress-bar median estimate). Trade-off explicitly accepted: no distribution metric. P95/P99 across runs is not directly available — `quantile_over_time(0.95, last_run_duration_seconds[7d])` is a quantile *over time samples* of the last-run gauge, not over runs. For "is this run slower than usual" the gauge is sufficient (`last_run_duration_seconds / avg_over_time(last_run_duration_seconds[7d]) > 3`); for genuine SLO-style distribution analysis a future OTel export remains the right path. Failed runs are excluded — same logic as the UI's `MedianDuration` and `dump_size_bytes`: failures fail fast (connection refused in <1 s) and would systematically underestimate. The worker-side `dump_duration_seconds` and `upload_duration_seconds` histograms remain unexposed; they observe a phase the meta.json does not currently break out, so giving them the same treatment would require a meta.json schema extension. Deferred until a concrete need surfaces.

- **Separate ServiceAccounts for operator and worker.** The operator SA retains Secret watch, CronJob CRUD, Job watch, Lease CRUD. The worker SA is reduced to Secret get/list + Event create/patch. A compromised worker pod can no longer modify CronJob schedules or leader election leases.

- **Writable `/tmp` via emptyDir, not relaxing `readOnlyRootFilesystem`.** The operator needs a small writable `/tmp` (1Mi) for SFTP known-hosts temp files. The worker's main emptyDir is mounted at the configured `TEMP_DIR` path. When `TEMP_DIR` is not under `/tmp`, a second small emptyDir covers `/tmp` for `os.CreateTemp` calls. This preserves PSA-restricted compliance.

- **EventBroadcaster shutdown before exit.** The worker defers `eventBroadcaster.Shutdown()` to flush buffered events. Without it, final events like `BackupCompleted` could be lost because the broadcaster sends asynchronously.

- **Signal-aware context for graceful shutdown.** The worker's context chains `signal.NotifyContext(SIGTERM, SIGINT)` → `context.WithTimeout`. SIGTERM from Kubernetes pod termination cancels the pipeline context, allowing in-flight operations to abort cleanly while deferred cleanup (event flush, temp file removal) still runs.

- **Upload concurrency semaphore.** Fan-out uses a channel-based semaphore (default 4) to limit concurrent uploads. Without it, N destinations each open the dump file and upload simultaneously, causing file-descriptor and bandwidth pressure on clusters with many destinations.

- **PodDisruptionBudget for the operator.** Only rendered when `replicaCount > 1`. Prevents voluntary evictions from killing the last operator pod during node drains or cluster upgrades. `minAvailable: 1` is the right choice for a leader-elected controller.

- **UI error sanitization.** HTTP error responses return generic messages ("internal error", "target not found") instead of raw `err.Error()`. Internal details are logged server-side. This prevents leaking implementation details (file paths, storage errors, internal state) to unauthorized clients, especially when the UI is exposed without an auth proxy.

- **SHA256 checksum in meta.json.** The pipeline computes SHA256 of the encrypted dump during file writing via `io.MultiWriter(file, hash)`. The hex-encoded hash is stored in `meta.json`. This enables offline integrity verification during restore and periodic bit-rot detection without downloading the full dump — compare `sha256` from meta with the stored object.

- **MetricsRefresher: long-lived storage pool + two-tier concurrency.** Three iterations on the refresher's hot path, the latest replacing the prior two:

    1. **Initial:** sequential `for src` loop, sequential per-destination Get inside each source. With 100 sources × 5 destinations the tick took O(N) wall-clock time and serialised every backend call regardless of how many CPUs the operator had.
    2. **Per-source semaphore (default=4):** parallelised destination fetches *within* a single source. Helped one-source benchmarks but did nothing for fleet load — sources were still processed serially, and the semaphore was scoped per-source, so a destination shared by 50 sources still saw every source's 4 calls in turn.
    3. **Current (this ADR):** `controllers/storage_pool.go` keeps one `Storage` instance per destination alive across ticks, keyed by `(name, sha256(SecretData))`. The hash means a Secret edit (rotated SSH key, swapped S3 endpoint) busts the cached client at the next tick rather than silently authenticating with the previous credentials forever. `pool.Retain(currentDests)` prunes entries for destinations that vanished between ticks. Same pool is reused by the storage scrubber to avoid two parallel client lifecycles for the same backend on the leader pod.

  Concurrency is split into two independent semaphores so the right knob limits the right thing:

    - **Global worker pool** (`defaultGlobalConcurrency`, 8): caps how many sources are processed simultaneously per tick. This is the operator pod's CPU/goroutine budget — it bounds how much work the refresher can do at once, nothing more.
    - **Per-destination slot** (`defaultPerDestConcurrency`, 4): caps in-flight calls against a single backend, summed across every source that fans out to it. This is the actual backend-protection knob. A Hetzner Storage Box accepts ~10 concurrent SSH sessions; staying at 4 leaves headroom for worker-pod uploads running in parallel. The previous per-source semaphore conflated these two concerns and protected only the smaller case.

  Why not a watch/event-driven model instead (S3 Event Notifications → SQS, inotify-style for SFTP)? The right end state, but it forces a fundamental change in shape: the refresher would become event-driven with a periodic reconciliation fallback, and SFTP has no native event mechanism, so half the deployments would still poll. The pool + two-tier concurrency captures most of the win without the architectural commit. Marked as a future ADR if event-shape becomes worth the cost.

  Why not ETag / If-Modified-Since on `meta.json`? The actual hot cost is **connection setup** (SFTP SSH handshake, ~100–300 ms per call), not the 5–50 KB meta payload. Fixing the connection-rebuild dominates fixing the response-body size. ETag remains a useful follow-up for S3 request-cost optimisation at very large scale (>1000 sources), but it's a leaf optimisation, not the root one.

  Trade-offs explicitly accepted:
    - **SFTP idle timeout.** Hetzner Storage Box drops idle SSH connections after ~30 min. The pool's clients can outlive that window if a destination has no traffic. Currently relies on `ClientConfig.ClientKeepAlive`-style heartbeats inside the SFTP storage; a future Reconnect-on-Error wrapper at the pool layer is the cleaner long-term fix.
    - **Memory.** One Storage value per destination, kept for the operator's lifetime. Bounded by the destination count (low tens at any realistic scale), not concerning.
    - **Per-destination slot map** is keyed by destination name and never pruned today; rare destination cycling would leak slots. Trivial follow-up, deferred until it matters.
    - **`current` map writes** are now concurrent and need a mutex (added). `sync.Map` was an alternative but adds overhead for a map this short-lived; the explicit mutex is more legible.

- **Schedule jitter at CronJob templating, not as `window-start`/`window-end` annotations.** When 100 sources share a `0 2 * * *` schedule, the storage backend takes a single thundering-herd burst at 02:00:00. The fix is per-source minute spreading. Two design routes were weighed: (a) a parallel scheduling vocabulary on top of cron (`window-start`, `window-end` annotations + operator-side fire-time selection), or (b) a one-shot rewrite of the cron's minute field at CronJob materialisation time. We picked (b). (a) introduces two languages for the same job, doubles the contract surface in §6.2, and the operator would then have to do its own scheduling logic (cron is single-fire-per-expression by design). (b) is ~80 lines of pure code, leaves cron as the single scheduling vocabulary, and keeps the CronJob spec the single source of truth visible to `kubectl get cronjob`. Conservative defaults explicitly chosen: jitter applies *only* when the user wrote minute==`0` (the canonical "no-statement" form). A user who wrote `15 2 * * *` had a reason — coordinating with another tool, app-quiet-window — and silently moving them to minute 37 would be the kind of magic that erodes trust. The `backup.mogenius.io/jitter-minutes` annotation is a two-way override: setting it explicitly opts a `15 2 * * *` source *into* spreading (user actively asked for fleet protection); setting it to `0` opts a `0 2 * * *` source *out* of spreading (user actively wants exact :00). Multi-fire schedules (`*/15`, `0,30`, `0-30`, `*`) are always left alone — rewriting their minute would change semantics from "fire N times" to "fire once". Hash input is `sha256(SecretName)`; matches the rest of the codebase's hash choice (storage_pool, anonymize, meta SHA256). Determinism across reconciles is non-negotiable: if the offset changed each tick, the operator would re-patch the CronJob on every reconcile and the schedule would visibly drift. Migration trade-off accepted: existing deployments with `0 H * * *` see their fire time shift on first reconcile after upgrade, but stay within the same hour the user wanted; release notes call this out, and the annotation is the escape hatch for any source where exact :00 was load-bearing. The reconcile log line now carries both the materialised schedule (what runs) and the annotation value (what was requested) so an operator inspecting `kubectl logs` after the upgrade can immediately see where each source landed. The UI's source list reflects the materialised schedule via the same pure `scheduler.ApplyJitter` call — single source of truth across reconciler, UI, and tests.

- **Table-name anonymization.** When `backup.mogenius.io/anonymize-tables=true`, table names in `meta.json` are replaced with truncated SHA256 hashes (16 hex chars). Row counts and sizes are preserved for anomaly detection. Real names stay in Prometheus metrics (scrape-only, not persisted to storage). Protects against information leakage through table names like `medical_records` in stored metadata. **Bug we hit and fixed:** the original implementation hashed the current run's stats only at meta-write time, but the analyzer's `Compare()` was fed the raw `stats` (real names) against `prevMeta.Stats` (which had been persisted with hashed names from the prior run). Every previous-run table looked "disappeared" because hashed-name vs real-name keys never collided in the analyzer's name-keyed map — every run produced N false-positive `table-disappeared` anomalies where N = previous run's table count. Fix: build a `cmpStats` (= `anonymizeStats(stats)` when `AnonymizeTables=true`) and pass that to `Compare`, so prev and curr line up. Side effects: (1) `report.Current` / `report.Previous` are then already in hashed form, so the previous `metaReport = anonymizeReport(report)` step is dropped — calling it now would double-hash anomaly subjects; (2) `emitAnalyzerMetrics` takes the original `stats` separately for table-row-count *labels*, preserving the "real names in Prometheus" property the original ADR promised. Legacy metas already in storage with hashed names work transparently because the next run hashes its own stats too — they line up without migration.

- **Purge CLI for DSGVO Art. 17.** `backup-restore --purge --target X --before YYYY-MM-DD` deletes all artifacts from storage. `--dry-run` previews. Enables right-to-erasure workflows without manual storage access. The cutoff is timezone-naive UTC.

- **Image digest pinning.** Helm supports `image.digest` in `values.yaml`. When set (e.g. `sha256:abc123...`), both operator and worker use `@sha256:...` instead of `:tag`. Tags are mutable — a compromised registry or accidental re-push can silently change what runs. Digest-pinning guarantees byte-identical images across deploys. Populate from CI: `crane digest ghcr.io/mogenius/backup-operator:v1.2.3`.

- **Optional NetworkPolicy for operator pod.** Helm template restricts egress to DNS (53), K8s API (443/6443), SSH (22/23), and HTTPS (443). Opt-in via `networkPolicy.enabled: true`. `extraEgressRules` allows non-standard ports (e.g. MinIO 9000). Limits blast radius of a compromised operator pod.

- **EmptyDir mount at configured TEMP_DIR.** The worker's emptyDir mounts at `r.Worker.TempDir` (not hardcoded `/tmp`). When `TempDir` is not under `/tmp`, a second small emptyDir covers `/tmp` for `os.CreateTemp` calls. Fixes a regression where custom `TEMP_DIR` broke with `readOnlyRootFilesystem`.

- **Vanilla JS SPA over React/Vue.** The UI is embedded via `go:embed` into the operator binary. A framework would require a Node.js build step, `node_modules`, and a bundler, adding complexity to CI and the Dockerfile. Vanilla JS with hash-based routing keeps the binary self-contained and the frontend instantly deployable without build tooling.

- **SSE over WebSocket for live updates.** Server-Sent Events are unidirectional (server→client), work through standard HTTP, require no protocol upgrade, and auto-reconnect natively. The broker pattern (subscribe/unsubscribe/publish via Go channels) fits Go idioms. WebSocket would add bidirectional complexity for a use case that only needs push.

- **Secret CRUD via Kubernetes API, not a database.** Source and destination configurations already live in Kubernetes Secrets (the discovery contract). The UI writes Secrets directly via the K8s API using the operator's ServiceAccount, preserving the single-source-of-truth model. Adding a database for configuration would create a sync problem between the DB and the actual Secrets.

- **Sensitive field masking in API GET responses.** Passwords, SSH private keys, and S3 secret keys are returned as `***` in GET endpoints. The UI sends these values only on create/update. This prevents accidental exposure through browser dev tools, network logging, or screen sharing — even without an auth proxy.

- **Manual trigger creates Job from CronJob spec.** `POST /api/trigger/{target}` reads the existing CronJob's pod template and creates a one-off Job. This guarantees the manual run uses the exact same image, env vars, and mounts as the scheduled run. No separate template or spec duplication needed.

- **SSE periodic refresh broadcast.** The SSE broker publishes a `refresh` event every 10 seconds in addition to CRUD-triggered events. This ensures clients eventually converge to the current state even if they miss an event (e.g. brief disconnect). The 10s interval is a balance between responsiveness and API load.

- **Legacy template routes preserved.** The old Go HTML template routes are served under `/legacy` for backward compatibility. The SPA serves from `/` via a catch-all handler. This allows gradual migration without breaking existing bookmarks or monitoring that targets the old UI.

- **Settings ConfigMap as runtime override layer.** Helm values are the master configuration source — they initialize the `{fullname}-settings` ConfigMap at install time. The UI Settings Wizard reads and writes this ConfigMap via the Kubernetes API (`GET/PUT /api/settings`), allowing live configuration changes without `helm upgrade`. An "Export values.yaml" button generates a downloadable values file so changes can be committed to Git for reproducible `helm upgrade` deployments. This gives operators the best of both worlds: interactive UI for quick tuning and declarative GitOps for controlled rollouts.

- **CRUD role verification on all Secret endpoints.** All GET, UPDATE, and DELETE handlers for sources and destinations verify the target Secret carries the expected `backup.mogenius.io/role` label before proceeding. This prevents the operator's expanded RBAC (full Secret CRUD) from being used to access or delete non-backup Secrets in the namespace.

- **Two-phase fan-out with per-destination result tracking.** The pipeline splits upload into Phase 1 (`fanOutDumps` — dump upload with retry) and Phase 2 (`uploadMeta` — meta upload with retry). Phase 1 collects `[]DestinationResult` (name, storageType, status, error per destination). Phase 2 builds `meta.json` including these results, then uploads to successful destinations only. This enables per-destination health monitoring in the UI without architecture changes to the storage layer. Previously, meta was uploaded alongside the dump in a single `uploadOne` call, so meta could not contain per-destination outcomes.

- **Run history merged from ALL destinations.** The UI data layer iterates through all destinations (not just first-available) and deduplicates runs by timestamp, preferring successful over failed runs only when timestamps match. This prevents hidden backups when one destination is offline and avoids the stale-success bug where an older successful run from destination B would mask a newer failure from destination A.

- **Multi-storage enterprise API endpoints.** Four new API endpoints support enterprise multi-destination monitoring: connectivity test (`POST /api/destinations/:name/test`), storage stats (`GET /api/destination-stats`), health matrix (`GET /api/destination-health`), and consistency check (`GET /api/consistency-check`). All use 4-goroutine semaphores for parallel storage queries, matching existing concurrency patterns.

- **DB credentials never appear on dumper command lines.** `mysqldump` consumes the password through `MYSQL_PWD`, `mongodump` reads it from a 0600 YAML config file, `redis-cli` reads it from `REDISCLI_AUTH`, and `pg_dump` keeps using `PGPASSWORD`. The previous `-pPASSWORD` and `--uri=mongodb://user:pass@…` patterns were visible in `ps`, container telemetry, and any sidecar with PID-namespace access — at the project's intended scale (10k+ daily backups across many tenants) that's a continuous exfiltration vector. The mongo config file is created via `os.CreateTemp` + explicit `Chmod(0600)` and removed via `defer`. Trade-off: an extra file write per Mongo run vs. a credential that may otherwise outlive the process in kernel buffers.

- **Centralised error sanitisation in `dumper.SanitizeStderr` / `WrapExecError`.** Stderr from exec'd dump tools and error strings from in-process drivers are scrubbed before they enter `fmt.Errorf` chains, logs, Kubernetes Events, or UI responses. The scrubber masks: any literal value passed in `secrets...` (typically the password), `scheme://user:pass@host` URI patterns, and `password=`/`passwd=`/`pwd=`/`auth=` key-value pairs. `SanitizeError(prefix, err, secrets...)` deliberately does NOT preserve `errors.Is` chains for driver errors that may echo the connection string back — preserving the chain would also preserve the leak. Adding a new dumper without using these helpers is a review-time blocker.

- **UI body-size cap and SSE client cap.** `limitBodyMiddleware` wraps every request body with `http.MaxBytesReader(MaxBodyBytes)` (default 1 MiB) so a multi-GB POST cannot OOM the operator before any handler runs. `sseBroker.maxClients` (default 256) refuses additional `/api/events` subscribers with `503` instead of letting them block in subscribe queues. Both are global rather than per-route so newly added mutating endpoints inherit the protection automatically. Tunable via `UI_MAX_BODY_BYTES` and `UI_MAX_SSE_CLIENTS`. These are defence-in-depth — they do not replace the auth proxy, they limit the blast radius if the proxy is misconfigured or removed.

- **Alerts surfaced via two independent providers.** `internal/alerts.PrometheusProvider` queries `/api/v1/alerts` on the configured Prometheus and filters by `alertname=~"^Backup.*"`; `internal/alerts.LocalProvider` re-evaluates the six PrometheusRule conditions inline against the operator's gathered metric registry. The UI handler exposes whichever is configured (Prometheus preferred, local as fallback chained automatically). This keeps the UI useful before kube-prometheus-stack is wired up — most of the value of alerts is "what's on fire right now", which we can answer locally — while preserving the audit-grade path through Prometheus → Alertmanager once it's set up. The two evaluators are deliberately independent: there is no shared PromQL evaluator, the local rules are 60 lines of Go that mirror the YAML in `values.yaml`. Keep both in sync when adding a rule. The `Source` field on each Alert (`"prometheus"` vs `"local"`) tells the UI to show a "for: not honored" disclaimer when in local mode.

- **Helm chart sets `release: kube-prometheus-stack` by default.** kube-prometheus-stack's Prometheus selects ServiceMonitors and PrometheusRules via `matchLabels: { release: kube-prometheus-stack }`. Without this label, our chart-shipped CRDs would render but never be scraped — exactly the failure mode that hid the project's semantic alerts in early enterprise pilots. The single top-level value `prometheusReleaseLabel` (default `kube-prometheus-stack`) applies the label to both objects; set to `""` to opt out and use `serviceMonitor.labels` / `prometheusRule.labels` manually instead.

- **Pre-upload retention sweep.** Retention previously ran only AFTER a successful upload. If storage was full, the upload failed and retention never ran — a deadlock where storage stays full forever and all future backups fail. Now retention runs as a pre-flight sweep BEFORE uploading, freeing space for the new dump. MinKeep floor still protects the N newest existing backups. Post-upload retention runs again to enforce thresholds after the new artifact lands. Edge case: with `MinKeep=0` and retention `Days=1`, the pre-flight sweep could delete all existing backups before the new upload. If the upload then fails, the target has zero backups. This is acceptable because `MinKeep=0` is an explicit "I don't need a safety floor" signal.

- **Upload error classification via `classifyUploadError`.** Upload errors are string-matched (case-insensitive) against known permanent failure patterns: `no space left on device`, `disk quota exceeded`, `permission denied`, `access denied`, `insufficient storage`. Matches produce `PermanentError` (no retry); everything else is `RetryableError`. This prevents wasting 3 retry attempts (~10 seconds) against a full or inaccessible storage backend. Trade-off: string matching is fragile across locales and storage implementations, but false negatives (retrying a permanent error) only waste time, while false positives (skipping retry on a transient error) are caught by the next scheduled run.

- **CronJob hash-suffix naming.** Kubernetes names are capped at 63 chars; CronJob names need headroom for the `-<job-suffix>` appended by the Job controller. The operator caps at 52 chars. Previously, long Secret names were silently truncated, meaning two Secrets sharing the same 52-char prefix would map to the same CronJob. Now, names exceeding the limit get a SHA256 hash suffix (8 hex chars) that makes collisions cryptographically improbable. The hash is computed from the original Secret name, not the truncated form.

- **SFTP partial file cleanup on upload failure.** When an SFTP write fails mid-stream (e.g. disk full, network drop), the partially written file is now explicitly removed via `sc.Remove(full)` before returning the error. Previously, partial files were left on the server, consuming space and potentially confusing listing/retention logic. The `f.Close()` return value is now checked (previously deferred and ignored) to catch flush errors.

- **Retention cleans empty ancestor directories.** Date-partitioned paths like `target/2024/01/15/dump-*.age` leave empty parent directories after file deletion. The retention loop now walks `path.Dir()` upward from deleted files to collect all ancestor directories (not just the immediate parent), then removes them deepest-first via an optional `RemoveDirectory` interface. S3 backends (which don't have directories) are unaffected — the interface check is a type assertion. `RemoveDirectory` errors are silently ignored because non-empty directories will correctly fail to delete.

- **Operator now calls Alertmanager directly for status checks and test alerts.** Previously, `ALERTMANAGER_URL` was purely cosmetic (an "open in Alertmanager" link). Two new UI endpoints call Alertmanager: `GET /api/alerts/status` queries `/api/v2/status` for connectivity and version info, and `POST /api/alerts/test` sends a self-resolving test alert via `/api/v2/alerts`. Rationale: users need to verify the full notification pipeline works before relying on it in production. The test alert auto-resolves after 2 minutes. Error responses are sanitized (no raw `err.Error()` in HTTP responses) to prevent leaking cluster topology through the unauthenticated UI.

- **Empty-dump hard-fail driven by row counter, not byte count.** `encryptedSize == 0` already failed the run, but mysqldump emits hundreds of bytes of headers/comments even when SELECT silently returns nothing — the artifact is "successful" by every byte-level measure yet contains zero data. The fix uses the existing `dumper.RowCounter` (sees the dump stream pre-gzip) and the existing pre-dump `Stats`: when pre-rows > 0 and dump-rows == 0, fail the run via `LooksEmpty`. Distinguishing "counter inactive" (mongo, no nil counts) from "counter active and empty" (SQL with no INSERTs) required a new `RowCounter.Active()` accessor — without it, an empty `map[string]int64{}` is indistinguishable from `nil` and the detector misfires on mongo. Annotation `backup.mogenius.io/empty-dump-check=false` is a deliberate escape hatch for legitimately empty schema-only sources; flagging that as a hard fail would be a worse failure mode than the silent emptiness we're catching.

- **Dual-path empty-dump detection: row counter for SQL, size heuristic for mongo/redis.** Mongo's BSON archive and Redis's RDB binary are not parsed in-stream, so the row counter cannot speak. Rather than write per-engine archive parsers (heavy, fragile), the second path compares `encryptedSize` against pre-dump `Stats`: mongo flags when `preTotalSize > 1 MiB` and the encrypted dump is < 1% of source (typical compression+encryption produces ~10-30%); redis flags when `preTotalKeys > 0` and the encrypted RDB is < 200 bytes (header-only). Thresholds are tuned conservatively — false-positives on real dumps are worse than missing the rare edge case, because operators learn to distrust noisy alerts. Both paths route through the same `verification.LooksEmpty` flag and the same `empty-dump-check` annotation, so the user contract is uniform across engines.

- **mysqldump flag hardening: `--events --default-character-set=utf8mb4` always, `--column-statistics=0` only when the binary is Oracle's mysqldump.** `--events` was missing — restoring a dump made without it loses any MySQL Event Scheduler entries the application depends on, with no warning. `--default-character-set=utf8mb4` forces a 4-byte session charset; the legacy "utf8" default in MySQL <8 is 3-byte and silently truncates emoji and some CJK characters at restore time. `--column-statistics=0` disables Oracle MySQL 8's client probe of `information_schema.column_statistics` that fails outright against MariaDB / MySQL <8 — but `mariadb-dump` does not know the flag and aborts with `unknown variable 'column-statistics=0'`. We probe `<binary> --version` once per binary path per worker process and treat the literal substring "MariaDB" in the banner as "do not pass the flag". The earlier `--help` text-content probe was fragile because future MariaDB releases could mention "column-statistics" in their help output as a compatibility note, falsely flipping the probe; `--version` is short, stable, and self-identifying.

- **One mysql-protocol dump binary per arch: Oracle's `mysqldump` on amd64, `mariadb-dump` on arm64.** Originally I tried to ship both `mysql-community-client-core` (Oracle) AND `mariadb-client` side by side, with the dumper picking per source `dbType`. apt refuses: both packages claim the `virtual-mysql-client-core` provider and `mariadb-client-core : Conflicts: virtual-mysql-client-core` is unconditional — there is no flag-overridable variant, and `dpkg --force-conflicts` would only paper over the same files-clash later. The pragmatic resolution: ship **one** mysql-protocol dump binary per build arch. On amd64 (the production target) we ship Oracle's MySQL 8.4 LTS `mysqldump` from `mysql-community-client-core`. On arm64 (where Oracle does not publish community packages) we ship `mariadb-client` and `mysqldump` resolves to `mariadb-dump`. Both speak the MySQL wire protocol, so source `dbType=mysql` and `dbType=mariadb` are both dumpable from either binary — the difference is fidelity for engine-specific quirks (column histograms, INVISIBLE INDEX, MariaDB Aria/Sequences). The runtime `--version` probe distinguishes Oracle from MariaDB and gates `--column-statistics=0` accordingly, so the dumper never has to know which binary it just executed. Image size landed at ~530MB (vs ~100MB alpine) — the price of correctness for a tool whose job is correctness, not throughput.

- **pg_dump from PGDG, not debian-bookworm.** debian-bookworm only ships `postgresql-client-15`, which refuses to dump from a server two major versions newer than itself ("aborting because of server version mismatch"). PG 17 servers are now common in the field, so the client lag broke real installations. Adding the official `apt.postgresql.org` repo and installing `postgresql-client-17` resolves it cleanly: pg_dump 17 is backward-compatible with PG 9.2+, so a single client version covers every supported source without per-version branching at exec time. Trade-off: one extra third-party APT repo in the runtime stage (already there for MySQL on amd64), worth it for actually being able to dump current PG versions.

- **Charset/collation captured into `dumper.Stats` and analyzed for drift.** Postgres `pg_database.datcollate`/`encoding` and MySQL `@@character_set_database`/`@@collation_database` are recorded into the meta. The analyzer flags drift between consecutive runs (`CharsetChanged`) and the operator exposes `backup_operator_charset_changed`, alert `BackupCharsetChanged` (severity warning, not info — utf8 → utf8mb4 transitions silently truncate at restore, so they need real attention). Mongo / Redis don't have a database-level charset, so the field stays empty and the drift detector skips them — the gauge is absent entirely rather than stuck at 0, matching how the rest of our optional metrics behave.

- **Storage scrubber re-hashes stored dumps periodically.** SHA256 has been written into `meta.json` since the integrity work; the scrubber finally consumes it. Operator-side controller (leader-elected, 24h default tick) lists each source's allowed destinations, fetches the most recent dump, streams it through `crypto/sha256`, and compares the hex against the meta. Failure → `storage_scrub_passed=0` + `BackupStorageCorrupted` (critical). Off by default because each scrub re-streams the full encrypted dump from storage — at scale that's a serious egress bill. Trade-off vs. a server-side checksum API (S3 `ETag`, `x-amz-content-sha256`): client-side re-hash catches storage backends that lie about their own content, and works uniformly across SFTP and S3 without per-backend code paths. The scrubber respects `Source.AllowsDestination` so destination allow-lists narrow scrub scope automatically.

- **Schema-change timestamp carried forward across runs.** The previous setup answered "did schema change since the last run?" but not "how old is the current schema?". Each meta now stores `schemaChangedAt`: bumped only when `SchemaHash` actually drifts, copied forward otherwise. Exposed as `backup_operator_schema_last_change_timestamp_seconds` so PromQL can answer "schema older than N days" — relevant when an old backup is being restored against a much newer application that no longer matches the dump's structure. Choice of "carry forward" over a counter / drift-rate metric: a single meta is self-describing without needing history, and the metric refresher reconstructs the value from any single recent meta rather than aggregating across the retention window.

- **Restore-verification Phase 2 spawns the ephemeral DB pod from the worker, owned by the worker pod's UID.** Phase 1's `stream-validate` is a parser test; it cannot tell you "psql would actually accept this" or "mongorestore can rebuild the indexes". Phase 2 modes (`schema-only`, `sample`, `full`) bridge that gap by running an honest restore. The implementation choices that shaped this:
  - **Worker-spawned, not operator-spawned.** Putting the spawn logic in the operator would require shuttling the dump file plus the ephemeral private key across pods. Doing it from the worker means everything stays on one box, in one process, with one lifecycle.
  - **Pod, not Job.** Jobs add a layer of indirection (the Job controls a Pod) and their TTL semantics are awkward when the parent that owns them (the worker) might exit before the Job's `ttlSecondsAfterFinished` kicks in. We create a bare Pod with `OwnerReference → worker pod`, so K8s GC cascades the delete the moment the worker terminates regardless of completion state.
  - **emptyDir, not PVC.** The verifier needs ~25 GB of scratch for a 10 GB DB at full restore. PVC adds storage-class-dependent provisioning latency, manual cleanup risk, and per-volume cost. emptyDir is local node disk, instant, automatic. The cost is that the node needs the headroom — documented in §6.2 / §14, and `verification-volume-size` lets users override the per-mode default when their dumps are large.
  - **One Spawner abstraction, not direct client-go in the verifier.** `ephemeral.Spawner` is a 4-method interface; the verifier package only sees that. The K8s implementation is the only file that imports `client-go`, which keeps the verifier driver fully unit-testable with a fake spawner.
  - **Engine-specific readiness probe runs after kubelet's Ready=true.** Stock postgres/mysql/mongo/redis images don't ship a meaningful `readinessProbe`, so `kubelet Ready` only tells us the container started. We poll an actual `SELECT 1` / `db.runCommand({ping:1})` / `PING` ourselves before declaring the pod restore-ready. Adds 0–10 seconds in practice and prevents the entire restore from racing the DB's startup phase.
  - **schema-only is implemented via stream-filtering for SQL engines, deferred for mongo/redis.** Postgres / mysql plain SQL streams can be filtered cheaply (drop everything between `COPY ... FROM stdin;` and `\.`; drop `INSERT INTO` lines), giving real "schema only" without parsing the SQL. mongo's BSON archive and redis's RDB don't decompose like that — schema-only on those engines is a label, not a meaningfully cheaper restore. Documented in the engine source files; users with cost concerns should pick `stream-validate` instead on those engines.
  - **redis Phase-2 verification is intentionally minimal in this iteration.** redis-cli has no clean "load this RDB into a running server" path; the supported routes are all "restart with `dbfilename=...`", which doesn't fit our spawn-then-restore model. Phase 2 redis verification confirms the verifier-pod is reachable and authenticatable; Phase 1 stream-validate already confirms decryptability + RDB header. Real RDB load is a follow-up.

- **Read-only docs portal on its own port (`:8083`).** The `/docs` portal serves the same `CLAUDE.md` and `README.md` you're reading now, plus a generated tech-stack page from `go.mod`. It runs in the operator pod but on a separate listener so cluster admins can scope ingress separately from the mutating UI: docs can be public or wide-internal while the management UI stays SSO-gated. The docs server holds **no Kubernetes client** and **no Secret access** — it's a static-file renderer. If it gets compromised, an attacker reads the same Markdown files anyone with the GitHub URL can read. That property is what justifies exposing it more loosely. Off by default (`docs.enabled=false`); the Dockerfile populates `/app/docs` at build time so the portal works out of the box once the flag flips. The in-page search dropdown is client-side only — no server-side indexing means the portal works on any read-only mount and survives ConfigMap-level configuration drift.

- **Job duration estimate from past meta.json, not Prometheus.** The UI's Jobs page renders a per-row progress bar for `status=running`. Three options were considered for sourcing the estimate: (a) Prometheus query on `run_duration_seconds`, (b) Kubernetes Job `status.startTime` plus a server-side rolling histogram, (c) median over the last N `meta.json` files for that target. We picked (c). Prometheus is out because the run-duration histogram is worker-only and never reaches the scrape (see §12 caveat); we'd be adding the same Pushgateway dependency we already rejected. A server-side rolling histogram in the operator would duplicate state Prometheus is supposed to own. Reading meta files is consistent with the rest of the operator-side aggregation pattern (the MetricsRefresher does exactly this), reuses the existing 30s `runsCache`, and works without any monitoring stack at all. **Median over last 10 successful runs**, not mean: a single index-rebuild outlier shouldn't double the estimate. **Failed runs excluded**: failures usually fail fast (connection refused in <1 s) and would systematically underestimate. **Cap at 99 %**: until `meta.json` lands, the run isn't actually done; showing 100 % would lie about state. Trade-off — the estimate is wrong precisely when it matters: a database that grew 3× since the last successful run will mispredict for one cycle. Stratifying by `dump_size_bytes` is the obvious next step but adds enough complexity that we shipped the simple version first.

- **Restore-verification uses an ephemeral keypair generated inside the worker pod.** The "a backup that hasn't been restored is not a backup" gap was real — the existing `DumpVerification` only validates the stream as it's *being produced*, never the encrypted artifact at rest. The classical fix (auto-restore against an ephemeral DB) requires the age private key inside the cluster, which §10 forbids. Resolution: **the worker generates a fresh X25519 identity in process memory at the moment it decides to verify**, encrypts the dump with both the long-lived DR recipient AND the ephemeral one (age supports multi-recipient natively, ~200 bytes header overhead per recipient), runs the in-process verifier, the pod terminates and the private half is gone. The DR key is unaffected — it can decrypt the same artifact for years. Trade-offs considered: a static "verifier-recipient" K8s Secret would have been simpler but exposes ALL backups if compromised (whereas an ephemeral keypair only exposes ONE run); a "schema-only without decrypt" mode would have skipped the key problem but doesn't actually prove the encryption layer works. The chosen design dominates both on security AND on coverage. Scheduling is **state-driven** (`ShouldVerify` reads `latestMeta.restoreVerification.completedAt` and compares with `interval`) rather than time-window-matched against cron, so manual runs verify when overdue and cron drift is irrelevant. Both phases are now shipped: Phase 1 (`stream-validate`) decrypts + parses in-process with no DB-pod-spawn and no RBAC expansion; Phase 2 (`schema-only` / `sample` / `full`) spawns an ephemeral DB pod via the `ephemeral.Spawner` abstraction, gated behind `restoreVerification.enableEphemeralPodSpawn=true` in the chart which grants the worker SA `pods: create/get/list/watch/delete` + `pods/status: get` + `pods/log: get` in its own namespace. Brand-new sources skip the very first verifier-run (no preStats baseline yet), then the "first verification" path fires on the next run regardless of interval so operators see signal without waiting a full week.

- **stream-validate uses engine-specific parsing, not byte equality.** A naive "decrypts and gunzips → match" check would pass for any well-formed gzip of garbage. Each engine has a parser that proves the stream actually shaped like a dump for that engine: SQL engines re-run `dumper.RowCounter` against the plaintext stream to count `INSERT`/`COPY` rows and compare against the pre-dump `Stats` total with a 99 % tolerance (the same tolerance the dump-time `BuildVerification` uses, so behavior is consistent). Header sanity-check is permissive — `pg_dump` / `mysqldump` banners shift between minor versions, so we accept any leading SQL comment block plus loose engine markers. Mongo's `mongodump --archive` format is asserted by its 32-bit little-endian magic `0x8199e26d`; Redis's RDB by the literal `REDIS` ASCII + 4 ASCII version digits. Both then drain the rest of the stream so a corrupt gzip layer mid-file fails loudly rather than silently. Per-table mismatch checks were intentionally omitted: SQL dumps reorder/batch/split tables legitimately and pre-stats from `pg_stat_user_tables` are estimates anyway, so per-table comparison would be false-positive-prone. Total-rows catches the alarming case ("dump has 0 rows but DB had 10k") without crying wolf on cosmetic differences.

- **Phase-2 schema-only is stream-filtering for SQL, a label for mongo/redis.** Postgres `psql` and MySQL's `mysql` CLI both replay plain-SQL dumps line-by-line; we can drop everything between `COPY ... FROM stdin;` and the trailing `\.` (or `INSERT INTO` lines) on the fly via a goroutine-piped scanner, keeping the DDL intact. The engine literally never sees the data. Mongo's BSON archive and Redis's RDB are binary and indivisible at this granularity — schema-only on those engines is a label, not a meaningfully cheaper restore. Documented per-engine in `verifier/restore/`; users who care about cost on those engines should pick `stream-validate` instead. Sample mode is currently equivalent to Full at the engine layer: pre-filtering hooks for true sampling are reserved for a future iteration.

- **Phase-2 readiness is engine-specific, not Pod `Ready`.** The stock postgres/mysql/mongo/redis container images don't ship a meaningful `readinessProbe`, and `kubelet Ready=true` only tells us the container started — not that the DB accepts connections. Each engine carries its own `Probe` function (`SELECT 1` via pgx, `mysqladmin ping`, `mongosh runCommand({ping:1})`, `redis-cli PING`) that runs after the kubelet probe passes. Adds 0–10 s in practice. Without it, the entire restore would race the DB's startup phase and produce flaky `psql: connection refused` mismatches.

- **Redis Phase-2 verification is auth + roundtrip, not RDB load.** `redis-cli` has no clean "load this RDB into a running server" path: the supported routes are all "restart the server with `dbfilename=...`", which doesn't fit the spawn-then-restore model. The `DEBUG RELOAD` strategy is fragile across versions. Redis Phase-2 currently confirms the verifier-pod is reachable and authenticatable, plus a `SET`/`GET` roundtrip; Phase-1 `stream-validate` already confirms decryptability + RDB header magic. Real RDB load is a follow-up. The `Notes` field on `SmokeResult` carries a `"redis Phase-2 verification is auth+roundtrip only"` advisory string so operators see the limitation in the meta without having to read the source.

- **Suspended state lives on the source Secret, not on the CronJob.** Pausing a backup used to mean either deleting the source (loses config + history) or `kubectl patch cronjob ... suspend=true` (the previous CLAUDE.md claimed the reconciler "doesn't revert this", but it does — `current.Spec = desired.Spec` in the reconcile loop unconditionally rebuilds the spec, so manual patches were silently lost on the next Secret reconcile). The fix: a new `backup.mogenius.io/suspended` annotation on the source Secret, parsed into `Source.Suspended`, which the reconciler translates into `Spec.Suspend = ptr(src.Suspended)` on every reconcile — deterministic in both directions. Considered alternatives: (a) leave the documented-but-broken kubectl-patch flow alone; (b) preserve manual patches by reading `current.Spec.Suspend` before overwriting; (c) Secret-as-truth (chosen). (a) ships a known foot-gun — operators who follow the doc lose their pause on the next innocuous Secret edit. (b) introduces a third source of truth (kubectl, annotation, code default) and makes "why is this paused?" un-answerable from the Secret alone, which is exactly the property §6.2 guarantees for every other knob. (c) makes the user contract uniform: every behavior toggle is an annotation on the source Secret. UI exposes Pause/Resume buttons that hit a dedicated endpoint (`POST /api/sources/{name}/suspend`) so a one-click pause does not require sending the full source body and risk overwriting a concurrent edit. Manual triggers (`kubectl create job --from=cronjob/...` and the UI's Run button) intentionally ignore Suspend — that is the documented escape hatch for restore drills against paused sources.

- **`internal/safe.Goroutine` for every long-lived operator goroutine.** `pipeline.go` already had a private `recoverGoroutine` helper that wrapped each fan-out goroutine with `recover()`. The operator side did NOT — six goroutines in `MetricsRefresher`, `StorageScrubber`, and the multi-destination UI handlers (`destination-stats`, `destination-health`, `consistency-check`) were unprotected. A panic in any of them would crash the operator pod, which is a much bigger blast radius than a worker-pod crash: the worker is short-lived and K8s respawns the next tick, but the operator pod hosts every reconciler AND the UI. Resolution: extract the helper to `internal/safe`, add `runtime/debug.Stack()` capture (the original logged just `panic: %v` with no stack — recovered panics with no stack are nearly undebuggable in production), and apply at every long-lived goroutine call-site. Alternative considered: leave it inline per-package as a tiny duplicated helper. Rejected because the rule "long-lived operator goroutines must recover" is a uniform contract — having a single import path makes that contract greppable and prevents the next added goroutine from quietly being unprotected. Worker-pod fan-out goroutines now use the same helper too (consistency over preserving pipeline.go's local copy).

- **ETag + gzip + short Cache-Control on the SSE-driven read endpoints.** `/api/targets`, `/api/jobs`, and `/api/destinations` are polled on every UI refresh tick. Each was sent uncompressed with no validators; a fleet with many sources × many concurrent operators repeated identical multi-KB JSON bodies over the wire constantly. `cachedJSON` middleware buffers the response (capped at 4 MiB to bound memory), computes a strong SHA256-prefix ETag, emits `Cache-Control: private, max-age=5, must-revalidate` + `Vary: Accept-Encoding`, and returns 304 on `If-None-Match`. Bodies ≥1 KiB are gzipped via a pooled writer when the client accepts. Errors and redirects skip caching so a transient 500 cannot stick. SSE (`/api/events`) is intentionally not wrapped — it streams. Trade-offs explicitly accepted: (a) `private` not `public` because a misbehaving shared cache could serve one tenant's data to another; (b) 4 MiB buffer cap means very large responses skip ETag — they take the slow path rather than risking memory pressure on the operator pod; (c) chose strong over weak ETag because we can compute it cheaply (SHA256 of the body we're about to send) and it gives byte-exact 304 semantics, which a polling SPA loves.

- **`analyzer_baseline_unavailable` as a side-effect setter inside `loadPreviousMeta`, not a typed return.** The function returns `*meta.MetaFile` — `nil` on "no baseline yet", non-nil on success. Both caller paths just take the meta and proceed; they don't care WHY it was nil. Changing the signature to `(*meta.MetaFile, baselineOutcome)` would force every caller to handle a state they don't act on. Instead the function counts per-destination accessibility internally and calls `metrics.SetAnalyzerBaselineUnavailable(target, allBroken)` before returning. Caller signature unchanged; the new signal lives in the metrics layer where the operators actually look. Trade-off: a side effect inside a "loadX" function is mildly surprising. Mitigated by an explicit comment on the function and by the fact that the rest of the operator already follows this "metrics-as-side-effect" pattern (`MetricsRefresher.refreshSource` does the same).

- **Post-upload retention visibility via logs + Events, not via metric or schema change.** The pre-upload retention sweep records its outcome into `meta.json`'s `retention` block and the operator-side aggregator turns that into the `retention_last_status` gauge that drives `BackupRetentionFailing`. The post-upload sweep runs AFTER `meta.json` was uploaded — the data has nowhere to go. Three options were considered: (a) re-upload `meta.json` with a second `retention` entry (cost: another upload per run, partial-failure complexity); (b) add a separate post-retention metric, requiring the same operator-side aggregation pattern but with a second sidecar file; (c) accept that post-upload is best-effort and surface the visibility through K8s Events + worker logs only. We picked (c). The `retainForDestination` failure path now emits a Warning `RetentionFailed` Event tagged with the phase, so post-upload failures appear in `kubectl describe secret <source>` and the cluster audit log even though `meta.json` is silent. The `applyRetention` call gets a `phase` parameter that flows into log fields and Event messages, so operators reading the audit trail can tell pre-upload from post-upload at a glance. The ADR's existing semantics — "pre-upload is load-bearing, post-upload is best-effort, may take one extra cohort if it fails persistently" — remain. What closes is the observability: silent retention failures in the post-upload phase used to be invisible until storage filled.

- **Typed API error codes (`apiResponse.Code`) as the frontend-backend contract.** Every 4xx/5xx response was just `{message: string}`. The frontend could only string-match to decide presentation, every typo broke logic, and there was no migration path to localised messages. New stable catalogue in `errors_api.go`: `validation`, `bad_request`, `method_not_allowed`, `not_found`, `conflict`, `forbidden`, `server_error`. New `writeError(w, status, code, message)` helper is the canonical 4xx/5xx path; 86 sites across `handlers_api / handlers_alerts / handlers_capabilities / handlers_settings / server` migrated. Frontend `api()` attaches `code + status` to thrown `Error`, and `apiErrorToast()` picks toast severity / friendly message by code — existing `toast(e.message, 'error')` call sites keep working unchanged because the shape is forward-compatible. Considered alternatives: (1) HTTP `Problem Details` (RFC 7807) — overkill for an in-cluster API, brings irrelevant fields like `type` URI; (2) per-handler typed error structs — explosion of types for the same five conceptual classes. Trade-off: the catalogue is closed (a new code requires a backend release). Mitigated by the catch-all `bad_request` for "this is a 400 but doesn't fit a more specific bucket".

- **SSE event routing + render debounce, not full DOM-diff incremental updates.** Previously every server-sent event triggered `renderPage(currentPage(), false)` regardless of whether the change was relevant. Editing a destination re-rendered the Audit log; a burst of N rapid CRUD events fired N back-to-back render calls. The "right" fix is per-page incremental DOM updates (e.g. append the new row to the table instead of stringifying everything). That's a major refactor of every page renderer for marginal extra benefit over a much simpler change: (a) a static `sseEventPages` map declares which pages care about which event types, drop everything else; (b) `scheduleSSERender` wraps `renderPage` in a 200 ms debounce so a burst coalesces into a single render. (a) eliminates the cross-page churn entirely (audit log no longer re-renders on destination edits); (b) folds bursts into one expensive call. 200 ms is below the eye's perceptual threshold for content changes, so the user sees no extra delay. Trade-off: pages still fully re-render when they ARE current — but only once per burst, only when relevant. True DOM-level diffing remains a future option if profiling shows the fully-rerendered case is expensive on big fleets.

- **SFTP password + keyboard-interactive, with key auth still preferred.** SFTP destinations used to require `ssh-private-key`. Operators with locked-down NAS firmware (QNAP, Synology) often can't drop a public key in `authorized_keys` for non-admin users — only password auth works. Two related fixes: (a) `password` and `ssh-private-key` are now both accepted, exactly one required; public-key still ranks first in the auth method list so openssh-style preference holds when both are supplied. (b) Auth-method order for password is `keyboard-interactive` THEN `ssh.Password`, not the natural openssh order. QNAP/Dropbear builds respond to a failed `ssh.Password` with `USERAUTH_FAILURE` (51) while the client is still in mid-state, manifesting as `unexpected message type 51 (expected 60)` instead of cleanly falling through. Keyboard-interactive first matches what those servers actually advertise as their primary method; classic password remains as a fallback for plain openssh. (c) Trailing CR/LF stripped from the password (`TrimRight \r\n`, NOT full TrimSpace — real passwords can have leading/trailing spaces, but a trailing newline is a paste-from-password-manager artifact that makes the server reject auth with no useful diagnostic).

- **Single combined `/api/dashboard/heatmap` endpoint for all fleet-wide projections.** The dashboard needs heatmap + per-day storage + analyzer anomaly stream + per-target duration stats + daily verification pass rate — five datasets, all derived from "iterate every target's run history". A single endpoint that emits all of them in one pass is much cheaper than five endpoints each calling `meta.List` on the same destinations. The trade-off is response shape coupling: adding a sixth dataset requires touching both Go and JS together. Worth it because the alternative is making the operator hammer destinations five times for data that's all coming from the same meta.json files. Cap on anomaly entries (200 newest-first) keeps the payload bounded on busy fleets.

- **Stale-while-revalidate cache for slow probes (`getOrRefreshAsync`).** `/api/targets` and `/api/dashboard/heatmap` both read meta.json from destinations. The original cache (`getOrLoad`, 30 s TTL) blocked every request that hit a cache miss for the full duration of the storage probe — when one destination was slow, the dashboard locked up on every SSE refresh (10 s cadence shorter than the 30 s TTL, so every third tick fell through). Fix: `cache.getOrRefreshAsync` returns whatever is cached (zero-value on first miss) immediately and spawns a single background refresh per key, dedup'd via a loading map. When the refresh completes, the server broadcasts an SSE `refresh` event so the frontend repaints with fresh data — without ever blocking the request that triggered the refresh. Frontend mirrors the pattern with `_slowProbes` and `_fleetSummary` caches keyed to the same TTL, plus an `inFlight` guard so SSE-tick storms can't pile up concurrent fetches. Trade-off: dashboard can briefly show stale data after a backup run completes; the follow-up SSE corrects it within seconds. The previous "block until storage replies" gave fresher data at the cost of UX that grinds to a halt when any one destination is slow or down.

- **UI handlers share the controllers.StoragePool.** UI handlers (`/api/destinations/<n>/test`, `/api/destination-stats`, `/api/destination-health`, `/api/consistency-check`, `/api/dashboard/heatmap`, `/download/...`) used to call `storageFactory.NewStorage` directly on every request. Each SFTP/FTPS call meant a fresh SSH/TLS handshake — and with SSE-driven 10 s refresh hitting 4–5 endpoints across N destinations, the operator opened dozens of fresh connections per minute. QNAP's Network Access Protection picked this up as a bruteforce attempt and IP-blocked the operator pod — the user had to manually unblock from the QNAP admin UI to recover. Fix: every UI storage access goes through the same `controllers.StoragePool` that MetricsRefresher and StorageScrubber already share. UI defines a small `StoragePool` interface to avoid importing `controllers` directly (operator reconcile loops should stay a leaf, not a UI dep); the concrete `*controllers.StoragePool` satisfies it structurally. `Server.storageFor` and `k8sData.storageFor` fall back to per-call construction when no pool is wired (tests), keeping the surface drop-in compatible with the prior call sites. Pool invalidates per-destination when Secret data changes (sha256-keyed signature), so credential rotation still flows through within one tick.

- **Test-destination button does a real Upload+Readback+Delete probe.** The original test handler did `dial → List(<nonexistent-path>)`. For SFTP this verified the SSH handshake; for FTPS my own `walkList` swallowed "directory does not exist" as empty. Result: a destination where the user could log in but had no write permission still showed green on the Test button — exactly the failure mode that produces "backup runs forever, nothing lands on the destination" with no diagnostic. New flow: upload a ~30-byte probe object with a random suffix, read it back, verify byte equality, delete it. Any failure surfaces with a clear message (`upload failed: 550 Permission denied`, `readback mismatch`, etc.). Best-effort cleanup via `defer` in a fresh context so even Delete-fails leave the destination provably writable.

- **FTPS data channel TLS session reuse via `tls.NewLRUClientSessionCache`.** Strict FTPS servers — QNAP, Pure-FTPd, vsftpd with `require_ssl_reuse=on` (default) — reject a fresh TLS handshake on the data socket as a session-hijacking countermeasure. Symptom: control channel logs in fine, login succeeds, every STOR/RETR fails the moment PASV opens the data socket. Adding `ClientSessionCache: tls.NewLRUClientSessionCache(32)` to the tls.Config lets the data channel resume the control channel's TLS session, which is what those servers expect. Per-destination cache so each backend gets its own session pool.

- **events.k8s.io/v1 required fields on UI-emitted Events.** Setting `EventTime` on a `corev1.Event` makes the API server apply the events.k8s.io/v1 schema rules: `ReportingController`, `ReportingInstance`, and `Action` must all be non-empty, or Create is rejected with `Required value` and the UI's audit trail goes silent. Three UI emitters (destination/source mutations, ConfigMap settings, age-recipient changes) hit this and went unnoticed because the failure is logged at error level but never user-visible. Single `emitUIEvent` helper now fills all three fields once; `ReportingInstance` falls back to `os.Hostname()` (the pod name inside K8s). The worker uses `record.EventRecorder` and is unaffected — only the manually-built UI events were broken.

- **Empty form-field clearing via explicit `removeKeys` on PUT, not via empty-value semantics.** PUT handlers merge `req.Data` into `existing.Data` so a field absent from the request keeps its stored value — that's the desired behaviour for sensitive fields (password, ssh-private-key) which are masked as `***` on edit and would otherwise be nuked on every save. But non-sensitive fields (host, port, known-hosts) need an explicit "clear" path: the user clears the input box, saves, and expects the field to actually be removed from the Secret. Three viable shapes: (a) empty-string-as-delete in `data` — but JSON has no way to distinguish "user typed nothing" from "field omitted"; (b) replace-not-merge — too destructive, would force the user to retype sensitive fields on every edit; (c) a separate `removeKeys []string` field in the request body — explicit, opt-in, composes cleanly with the existing merge. We picked (c). Frontend has a small `DEST_SENSITIVE_KEYS` constant (mirror of the backend's sensitiveKeys set) so the JS knows which fields to preserve-on-empty vs clear-on-empty. Same mechanism handles the SFTP auth-method toggle (switching key→password drops the old private key) and unchecked checkboxes (drop the flag entirely instead of storing the literal "false" string).

- **Six dashboard charts derived from the same fleet-summary endpoint.** Dashboard order: Storage Donut (per-destination actual bytes, from `/api/destination-stats`), Next-Run Gantt (24h horizontal timeline of upcoming backups, peak-coloured when multiple targets fire in the same minute), Fleet Heatmap (per-target × per-day status grid, last 30 days), Storage Growth (stacked area, daily upload bytes by db-type), Anomaly Stream (30-day timeline of analyzer findings, colour-coded by severity), Duration Distribution (per-target min↔max range with median+p95 marks, top 10 by median descending), Verification Pass Rate (daily % of restore-verifier-armed runs whose decrypt+parse succeeded, with grey volume bars behind the line). All seven render from data already in `/api/targets` and `/api/dashboard/heatmap` — no extra endpoints, no extra storage probes. The fleet summary endpoint computes all five projections (heatmap/storage/anomalies/durations/verification) in a single pass over each target's run history. Frontend caches via `_fleetSummary` with 60 s TTL + in-flight guard + SSE-driven repaint when fresh data lands. Chart-card loading-spinner + refreshing-dot patterns so the user always sees what's in-flight vs cached vs stale.

- **CronJob schedule's next fire time pre-computed server-side and surfaced everywhere.** Dashboard table, source list card, and target detail Configuration card all show "in 4h 32m (02:00)" or "in 3d (Mon 02:00)" alongside the cron expression. Parsing cron in JS is fragile across edge cases (`*/15`, `0,30`, ranges, day-of-week); the Go side uses `github.com/robfig/cron/v3`'s Standard parser and emits an RFC3339 timestamp in the Target API response. Frontend just formats. Suspended sources emit nil (no next-run badge). Important: `NextRun` is derived from the MATERIALISED schedule (post-jitter, what the CronJob actually fires on), not the user's annotation — so the dashboard shows the effective fire time, not the wishful one.

---

## 19. Helm Distribution

### 19.1 Installation

```bash
# From OCI registry (after release)
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient"

# From local chart (development)
helm install backup-operator ./charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient"

# Multi-key rotation:
#   --set agePublicKeys[0]=age1qx...primary --set agePublicKeys[1]=age1yz...dr
```

**Defaults to know:**

- `prometheusReleaseLabel: kube-prometheus-stack` — applied as the `release:` label on `ServiceMonitor` and `PrometheusRule` so kube-prometheus-stack picks them up. Set to `""` to opt out.
- `alerts.prometheusURL: http://prometheus-operated.alert.svc.cluster.local:9090` — assumes kube-prometheus-stack lives in the `alert` namespace. Override or set to `""` for the local heuristic.
- `alerts.alertmanagerURL: http://alertmanager-operated.alert.svc.cluster.local:9093` — used for the "open in Alertmanager" UI link, the `/api/alerts/status` connectivity check, and the `/api/alerts/test` endpoint.

### 19.2 CI/CD

Two GitHub Actions workflows:

| Workflow | Trigger | What it does |
|---|---|---|
| `ci.yaml` | PR to `main` | `go build`, `go test`, `go vet`, `helm lint` |
| `release.yaml` | Push to `main` | Semantic Release → multi-arch Docker image → GHCR, package + push Helm chart to OCI registry |

### 19.3 Release Process

Releases are fully automated via [Semantic Release](https://github.com/semantic-release/semantic-release). Merging a PR with conventional commit prefixes triggers the appropriate version bump:

| Prefix | Effect |
|---|---|
| `fix:` | Patch release (1.0.x) |
| `feat:` | Minor release (1.x.0) |
| `feat!:` / `BREAKING CHANGE:` | Major release (x.0.0) |
| `docs:`, `ci:`, `chore:` | No release |

No manual tagging required. The workflow runs `go test`, then semantic-release determines the next version, creates a GitHub Release, builds the Docker image, and publishes the Helm chart.

### 19.4 Settings Wizard

The UI Settings Wizard (`#/settings`) provides a 4-step form:

| Step | Fields |
|---|---|
| 1. Schedule & Timeout | `defaultSchedule`, `runTimeoutSeconds` |
| 2. Retention Policy | `defaultRetentionDays`, `defaultMinKeep`, `tempDir`, `tempDirSize` |
| 3. Worker Resources | CPU/Memory limits and requests |
| 4. Review & Apply | Summary of all settings, save button |

**API Endpoints:**
- `GET /api/settings` — reads the settings ConfigMap
- `PUT /api/settings` — validates and updates the ConfigMap
- `GET /api/settings/export` — generates a downloadable `values.yaml`

**Architecture:**
```
Helm values.yaml → ConfigMap (install-time defaults)
                         ↕
                    UI Settings Wizard (runtime overrides)
                         ↓
                    Export values.yaml → Git → helm upgrade (GitOps)
```

---

## 20. Important

- Every change to the directory structure should be reflected in section 4.
- New annotations or labels: update sections 6.1–6.5.
- New env vars: update section 7.
- New metrics: update section 12. New default alerts: update section 13.
- New failure modes worth flagging: update section 14.
- Data-flow or access-control changes: update section 17.
- Architectural decisions that change the behaviour of existing systems: update section 18 with the *reason*, not just the change.

Documentation that drifts from code is worse than no documentation. Bring this file with you when you change behaviour.

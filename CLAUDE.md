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
  --set agePublicKeys="age1qx...your-recipient-here"

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
│   └── secrets/         # Parses Secrets into Source/Destination configs; FilterDestinations helper
├── metrics/             # Prometheus metrics — semantic signals for Alertmanager
├── storage/             # Upload destination abstraction
│   ├── factory/         # Creates the right Storage from storage-type label
│   ├── sftp/            # Hetzner Storage Box and generic SFTP
│   └── s3/              # AWS S3, MinIO, Hetzner Object Storage, R2, B2, ...
├── verifier/            # Restore-verification: prove the uploaded artifact is decryptable+parseable
│   ├── verifier.go      # Verifier interface, ShouldVerify schedule logic, FailureResult helper
│   ├── factory/         # Mode → Verifier (stream-validate, schema-only, sample, full)
│   ├── stream/          # ModeStreamValidate: in-process decrypt → gunzip → parser, no DB-pod-spawn
│   ├── ephemeral/       # K8s Spawner — creates short-lived DB pods with emptyDir; ownerRef-cascade-cleaned
│   └── restore/         # ModeSchemaOnly / Sample / Full: spawn DB → restore → smoke queries (Phase 2)
├── ui/                  # Built-in web dashboard and management API
│   ├── cache.go         # Cached Secret data for dashboard rendering
│   ├── data.go          # Data aggregation helpers for templates; estimateDuration for the progress-bar feed
│   ├── handlers.go      # Legacy HTML template handlers (backward compat)
│   ├── handlers_api.go  # REST API: CRUD sources/destinations, trigger, SSE; /api/jobs embeds duration estimate for running jobs
│   ├── handlers_settings.go  # Settings API: GET/PUT /api/settings, values.yaml export
│   ├── server.go        # HTTP server, routing, SPA handler, SSE broker
│   ├── static/          # SPA frontend (vanilla JS, no build step)
│   │   ├── index.html   # SPA shell with sidebar, modal, toast containers
│   │   ├── style.css    # Dark theme, responsive layout, component styles
│   │   └── app.js       # Hash-router, API helpers, page renderers, forms; renders Jobs progress bar
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
| `backup.mogenius.io/role` | **yes** | `source` \| `destination` |
| `backup.mogenius.io/db-type` | yes (sources only) | `postgres` \| `mysql` \| `mariadb` \| `mongo` \| `redis` |
| `backup.mogenius.io/storage-type` | yes (destinations only) | `sftp` \| `hetzner-sftp` \| `s3` |

### 6.2 Source Secret annotations

| Annotation | Default | Effect |
|---|---|---|
| `backup.mogenius.io/name` | Secret name | Logical target name. Used in metrics labels, object paths, CronJob naming. |
| `backup.mogenius.io/schedule` | `DEFAULT_SCHEDULE` (`0 2 * * *`) | Cron expression for the managed CronJob |
| `backup.mogenius.io/analyzer-enabled` | `true` | `false` → skip `CollectStats` and analyzer for this source |
| `backup.mogenius.io/destinations` | unset | Comma-separated allow-list of destination *names*. Empty = fan out to all. |
| `backup.mogenius.io/retention-days` | `DEFAULT_RETENTION_DAYS` (30) | Delete dumps older than N days. `0` = keep forever. |
| `backup.mogenius.io/min-keep` | `DEFAULT_MIN_KEEP` (3) | Safety floor — never delete below this many newest dumps. |
| `backup.mogenius.io/row-drop-threshold` | `0.5` | Analyzer anomaly threshold for row-count drops. `0.3` means flag when a table shrinks below 30% of its previous size. |
| `backup.mogenius.io/size-drop-threshold` | `0.5` | Analyzer anomaly threshold for dump size drops. Same semantics as row-drop. |
| `backup.mogenius.io/anonymize-tables` | `false` | `true` → hash table names in `meta.json` with SHA256 (16 hex chars). Row counts preserved. |
| `backup.mogenius.io/empty-dump-check` | `true` | Hard-fail when the dump appears empty despite the source DB having data. Two detection paths: SQL (postgres/mysql/mariadb) compares dump-stream INSERT/COPY rows to pre-dump stats; mongo / redis use a size heuristic against pre-dump `collStats` / key counts. Set to `false` for sources that are intentionally schema-only (empty template DBs). |
| `backup.mogenius.io/restore-verification-mode` | `off` | Restore-verification mode. `off` (default), `stream-validate` (Phase 1, no DB-pod-spawn), `schema-only` / `sample` / `full` (Phase 2: spawn ephemeral DB pod + restore). Phase 2 modes require the worker SA to have `pods: create/delete` in its namespace — see `restoreVerification.enableEphemeralPodSpawn` in the chart. |
| `backup.mogenius.io/restore-verification-interval` | `168h` (when mode is active) | Minimum gap between verifier-runs (Go duration). State-driven: worker reads `latestMeta.restoreVerification.completedAt` and skips this run when the interval has not elapsed. Manual runs (`kubectl create job --from=cronjob/...`) verify whenever they're overdue regardless of cron drift. |
| `backup.mogenius.io/verification-image` | per-DB-type default | Container image for the verifier pod (e.g. `postgres:15.5-alpine`). Only consulted in Phase-2 modes. Pin this to match the source DB's exact major version when restore semantics depend on the engine version (charset defaults, function signatures, dump format compatibility). |
| `backup.mogenius.io/verification-volume-size` | per-mode default | `emptyDir.sizeLimit` for the verifier pod's data volume (e.g. `100Gi`, `5Gi`). Defaults: `1Gi` (schema-only), `5Gi` (sample), `50Gi` (full). Override when a single source's restore needs more headroom — at scale, the node's ephemeral storage is a real budget. |
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
| `ssh-private-key` | **yes** | PEM-encoded |
| `known-hosts` | recommended | Standard `ssh-keyscan` output. Use `[host]:port` for non-22 ports. Without it the worker logs a loud `INSECURE` warning and uses `InsecureIgnoreHostKey`. |

#### `storage-type: s3`

| Key | Required | Notes |
|---|---|---|
| `bucket` | **yes** | |
| `access-key-id` | **yes** | |
| `secret-access-key` | **yes** | |
| `region` | no | Defaults to `us-east-1`; non-AWS providers usually ignore this |
| `endpoint` | no | Required for non-AWS (MinIO, Hetzner Object Storage, R2, B2, Wasabi). Omit for AWS. |
| `path-style` | no | `"true"` for MinIO etc. that require path-style addressing. |

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
  Helm install --set agePublicKeys="age1qx..."
   └── creates Secret backup-operator-age with key AGE_PUBLIC_KEYS
        └── mounted into every worker pod via secretKeyRef in the CronJob spec

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
| `dump-<ts>.meta.json` | target name, db-type, encrypted size, SHA256, run start (`timestamp`), `completedAt` + `durationSeconds` (wall-clock duration), full Stats, full analyzer Report, dump verification, restore verification, per-destination upload results (`destinations` array with name, storageType, status, error) | Lets the next run compute diffs without restoring; lets humans audit without the private key; per-destination results enable multi-storage health monitoring; `durationSeconds` feeds the UI's Jobs progress-bar estimate (median over last N successful runs) |

**Timestamp format:** `20060102T150405Z` (Go reference time, ISO-like, lexically sortable).

**The meta file is intentionally unencrypted.** Anyone with read access to the bucket can see schema fingerprints and row counts, but never the data itself. If that's not acceptable for your environment, plan to encrypt the meta files in a follow-up — the trade-off is that automated diffing then needs the private key in the cluster.

---

## 12. Metrics Catalog

Exposed by the operator pod on `:8080/metrics`. **Worker pods are short-lived** — Prometheus cannot scrape them in time, so the run-level metrics are reconstructed by the operator's `MetricsRefresher` (`controllers/metrics_refresher.go`). It runs on a tick (default 30s, see `METRICS_REFRESH_INTERVAL_SECONDS`), lists Source Secrets in the watch namespace, fetches the most recent `*.meta.json` from each allowed destination, and writes the result into the operator's local Prometheus registry. That is why everything below is a Gauge — counters would require an always-on producer the worker cannot provide.

The histograms (`dump_duration_seconds`, `upload_duration_seconds`, `run_duration_seconds`) are kept in the worker for code-coupling reasons but their samples never reach Prometheus. Treat them as a known gap; rely on Job duration via kube-state-metrics if you need timing alerts today.

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
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts parsed from the most recent meta.json found at that destination |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if the destination's storage cannot be initialised, 0 once a meta.json was successfully read |
| `backup_operator_retention_deleted_total` | Counter | `target`, `destination`, `kind` | Worker-only; not visible to Prometheus |
| `backup_operator_retention_failed_total` | Counter | `target`, `destination` | Worker-only; not visible to Prometheus |
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
| DB unreachable | `pg_dump` exits non-zero, pipeline returns error, worker exits 1 | `kubectl get jobs` shows failed; no fresh meta.json arrives, so `last_success_timestamp_seconds` stops advancing → eventually `BackupOverdue` |
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
| Retention can't delete (perms) | Old dumps remain | Worker logs the error; not visible to Prometheus today (worker-only counters) |
| `known-hosts` mismatch | `ssh.NewClientConn` fails before any data leaves | Run fails; worker logs the host-key error |
| `known-hosts` missing | Worker logs `INSECURE` warning, accepts any host key | No automated alert (intentional — the user opted out) |

---

## 15. Common Operations

### Trigger a manual run

```bash
kubectl -n backup create job --from=cronjob/backup-prod-users-db \
  manual-$(date +%s)
```

The Job runs the same worker code as a scheduled run; metrics, retention, fan-out all behave identically.

### Suspend a backup temporarily

```bash
kubectl -n backup patch cronjob backup-prod-users-db \
  -p '{"spec":{"suspend":true}}'
```

The reconciler does **not** revert this — `suspend` is intentionally something you toggle out-of-band. To resume, set back to `false`.

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

- **Public key** (`age` recipient): stored in a K8s Secret (`backup-operator-age`), distributed to worker pods via env var. Used only for encryption.
- **Private key** (`age` identity): **never enters the cluster**. Lives on the operator's machine. Required only for `backup-restore` CLI.
- **SSH keys** (SFTP destinations): stored in destination Secrets. Scoped to individual storage backends.
- **S3 credentials**: stored in destination Secrets. Should use scoped IAM roles with minimal write permissions.

### 17.4 Audit Trail

| Event | Source | Visible via |
|---|---|---|
| `BackupStarted` | Worker pod | `kubectl describe secret <source>`, cluster audit log |
| `BackupCompleted` | Worker pod | Same |
| `BackupFailed` | Worker pod | Same, includes failure phase |
| `RetentionDelete` | Worker pod | Same, lists deleted artifact |
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

- **Worker resource limits via env vars.** `WORKER_CPU_LIMIT`, `WORKER_MEMORY_LIMIT`, `WORKER_CPU_REQUEST`, `WORKER_MEMORY_REQUEST` flow from Helm into every CronJob's container spec. Sensible defaults ship (2 CPU / 2Gi); empty disables. Without limits, a large dump can OOM the node.

- **Kubernetes Events as audit trail.** The worker emits `BackupStarted`, `BackupCompleted`, `BackupFailed`, and `RetentionDelete` events against the source Secret. Visible via `kubectl describe secret <source>` and preserved in cluster audit logs. Satisfies DSGVO Art. 30 and SOC2 requirements. The pipeline uses an `EventEmitter` interface so tests stay API-server-free (`NoopEventEmitter`). RBAC grants `events: create, patch` to the shared ServiceAccount.

- **Health probes on the operator.** controller-runtime's built-in `/healthz` and `/readyz` are served on `:8082` (separate from metrics `:8080` and UI `:8081`). Without probes, Kubernetes cannot detect a stuck operator and restart it.

- **Operator-side metric aggregation, not Pushgateway.** Backup metrics are produced by short-lived worker pods that Prometheus cannot scrape in time. Three options were considered: (a) Pushgateway, (b) operator aggregates from `meta.json`, (c) drop semantic alerts and rely on kube-state-metrics for Job status. We picked (b): the operator's `MetricsRefresher` controller polls each destination's latest meta.json and writes the result into the operator's local registry. Pushgateway adds a stateful component with known counter-staleness footguns; (c) sacrifices the project's core differentiator (semantic alerts on dump *content*). Aggregating from storage reuses the artifacts we already produce and keeps the system stateless apart from the operator pod itself. Counter-style metrics (`runs_total`, `anomalies_total`) are converted to Gauges (`last_run_status`, `last_run_anomalies`) because monotonic counters require a continuously running producer; reconstructing them from storage would require summing across the retention window and break whenever retention prunes a run.

- **Separate ServiceAccounts for operator and worker.** The operator SA retains Secret watch, CronJob CRUD, Job watch, Lease CRUD. The worker SA is reduced to Secret get/list + Event create/patch. A compromised worker pod can no longer modify CronJob schedules or leader election leases.

- **Writable `/tmp` via emptyDir, not relaxing `readOnlyRootFilesystem`.** The operator needs a small writable `/tmp` (1Mi) for SFTP known-hosts temp files. The worker's main emptyDir is mounted at the configured `TEMP_DIR` path. When `TEMP_DIR` is not under `/tmp`, a second small emptyDir covers `/tmp` for `os.CreateTemp` calls. This preserves PSA-restricted compliance.

- **EventBroadcaster shutdown before exit.** The worker defers `eventBroadcaster.Shutdown()` to flush buffered events. Without it, final events like `BackupCompleted` could be lost because the broadcaster sends asynchronously.

- **Signal-aware context for graceful shutdown.** The worker's context chains `signal.NotifyContext(SIGTERM, SIGINT)` → `context.WithTimeout`. SIGTERM from Kubernetes pod termination cancels the pipeline context, allowing in-flight operations to abort cleanly while deferred cleanup (event flush, temp file removal) still runs.

- **Upload concurrency semaphore.** Fan-out uses a channel-based semaphore (default 4) to limit concurrent uploads. Without it, N destinations each open the dump file and upload simultaneously, causing file-descriptor and bandwidth pressure on clusters with many destinations.

- **PodDisruptionBudget for the operator.** Only rendered when `replicaCount > 1`. Prevents voluntary evictions from killing the last operator pod during node drains or cluster upgrades. `minAvailable: 1` is the right choice for a leader-elected controller.

- **UI error sanitization.** HTTP error responses return generic messages ("internal error", "target not found") instead of raw `err.Error()`. Internal details are logged server-side. This prevents leaking implementation details (file paths, storage errors, internal state) to unauthorized clients, especially when the UI is exposed without an auth proxy.

- **SHA256 checksum in meta.json.** The pipeline computes SHA256 of the encrypted dump during file writing via `io.MultiWriter(file, hash)`. The hex-encoded hash is stored in `meta.json`. This enables offline integrity verification during restore and periodic bit-rot detection without downloading the full dump — compare `sha256` from meta with the stored object.

- **MetricsRefresher parallel with semaphore.** Destination meta-fetches within `refreshSource()` now run as goroutines bounded by a channel semaphore (default 4). Previously sequential — with many destinations, refresh could take long and open too many simultaneous connections. Caps I/O while being faster than serial.

- **Table-name anonymization.** When `backup.mogenius.io/anonymize-tables=true`, table names in `meta.json` are replaced with truncated SHA256 hashes (16 hex chars). Row counts and sizes are preserved for anomaly detection. Real names stay in Prometheus metrics (scrape-only, not persisted to storage). Protects against information leakage through table names like `medical_records` in stored metadata.

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

- **Restore-verification uses an ephemeral keypair generated inside the worker pod.** The "a backup that hasn't been restored is not a backup" gap was real — the existing `DumpVerification` only validates the stream as it's *being produced*, never the encrypted artifact at rest. The classical fix (auto-restore against an ephemeral DB) requires the age private key inside the cluster, which §10 forbids. Resolution: **the worker generates a fresh X25519 identity in process memory at the moment it decides to verify**, encrypts the dump with both the long-lived DR recipient AND the ephemeral one (age supports multi-recipient natively, ~200 bytes header overhead per recipient), runs the in-process verifier, the pod terminates and the private half is gone. The DR key is unaffected — it can decrypt the same artifact for years. Trade-offs considered: a static "verifier-recipient" K8s Secret would have been simpler but exposes ALL backups if compromised (whereas an ephemeral keypair only exposes ONE run); a "schema-only without decrypt" mode would have skipped the key problem but doesn't actually prove the encryption layer works. The chosen design dominates both on security AND on coverage. Scheduling is **state-driven** (`shouldVerify` reads `latestMeta.restoreVerification.completedAt` and compares with `interval`) rather than time-window-matched against cron, so manual runs verify when overdue and cron drift is irrelevant. Phase 1 ships only `stream-validate` (no DB-pod-spawn, no RBAC expansion); Phase 2 will add `schema-only`/`sample`/`full` against an ephemerally-spawned DB pod and require Worker SA `pods: create/delete` in own namespace.

---

## 19. Helm Distribution

### 19.1 Installation

```bash
# From OCI registry (after release)
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient"

# From local chart (development)
helm install backup-operator ./charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient"
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

# backup-operator

A Kubernetes-native backup operator in Go for **PostgreSQL**, **MySQL**, **MariaDB**, **MongoDB**, and **Redis**, with public-key encryption (`age`), multi-destination fan-out (SFTP + S3-compatible), semantic dump analysis, and Prometheus-driven alerting.

> **The contract:** label a `Secret`, get a backup. No CRDs to install, no extra resources to learn. The operator watches Secrets in its namespace, materialises a `CronJob` per labelled source, and Kubernetes does the running.

> **Built mostly with AI.** This project is an experiment: how far can you take a complete, production-shaped system — Kubernetes operator, three binaries, encryption, multi-destination fan-out, dashboard UI, restore verification, Helm chart, alerts, docs — with different AI coding assistants doing most of the typing? The architecture decisions, code review, and direction are human; the implementation, refactors, and large parts of the documentation (this file included) were written collaboratively with several AI agents and then audited. Treat the design choices in CLAUDE.md §18 as the human-curated record of what was actually decided.

---

## Table of Contents

- [Why this exists](#why-this-exists)
- [How it works](#how-it-works)
- [Quick Start](#quick-start)
- [Helm Installation & Distribution](#helm-installation--distribution)
- [Defining a Backup Target (Source)](#defining-a-backup-target-source)
- [Defining a Destination](#defining-a-destination)
- [The Dashboard UI](#the-dashboard-ui)
- [Settings](#settings)
- [Restore](#restore)
- [Restore Verification](#restore-verification)
- [Documentation Portal](#documentation-portal)
- [Alerting & Monitoring](#alerting--monitoring)
- [Encryption Model](#encryption-model)
- [CI/CD](#cicd)
- [Local Development](#local-development)
- [Troubleshooting](#troubleshooting)
- [More Documentation](#more-documentation)

---

## Why this exists

K8up, Stash, and Velero solve adjacent problems but none of them satisfy all three of:

1. **Typed configuration, credentials apart.** A `BackupSource` is validated at admission and reports `Ready` / last success / verifier verdict in `kubectl get`; the password stays in a plain Secret — optionally the one your database chart already created, so nothing is copied.
2. **Semantic dump analysis.** Alerts fire on dump *content* — table disappeared, row-count collapsed, schema fingerprint changed — not just on job exit codes. A backup that "succeeds" with an empty dump is a silent disaster everywhere else; here it pages you.
3. **Multi-destination fan-out as first-class.** One dump streams to N storage backends in parallel, mixed protocols (SFTP + S3 + …). Failure of one destination doesn't fail the run.

If you don't need all three, you have simpler choices.

---

## How it works

```
   user applies a BackupSource        ┌──────────────────────────────────────────┐
            │                         │ Kubernetes API                           │
            ▼                         │                                          │
   ┌──────────────────┐  watch        │   BackupSource ────┐  (+ creds Secret)   │
   │ Operator pod     ├───────────────┤                     │ OwnerReference     │
   │ (backup-operator)│  reconcile    │                     ▼                    │
   └──────────────────┘               │   batch/v1.CronJob ──tick──▶ Job pod     │
                                      │                                  │       │
                                      └──────────────────────────────────┼───────┘
                                                                         │
                              dump → gzip → age encrypt → temp file ◀────┘
                                                  │
                                       fan-out, parallel uploads
                                                  ▼
                                ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
                                │   AWS S3     │  │  MinIO/R2    │  │ Hetzner SFTP │
                                └──────────────┘  └──────────────┘  └──────────────┘
                                                  │
                                  before next run: read previous meta.json,
                                  diff stats, write new meta + analyzer report
                                  (alerts fire from Prometheus rules)

                   ┌──────────────────────────────────────────────┐
                   │ Operator's machine (offline)                 │
                   │   age private key  ──▶  backup-restore CLI  │
                   └──────────────────────────────────────────────┘
```

**Three binaries, one image:**

| Binary | Where it runs | Job |
|---|---|---|
| `backup-operator` | Operator Deployment | Reconciles `BackupSource` → managed `CronJob`, writes status. Optionally hosts the Dashboard UI. |
| `backup-worker` | CronJob-spawned Job pod | One-shot: dump → encrypt → fan-out → retention. Exits 0 / 1. |
| `backup-restore` | Operator's laptop | Lists, downloads, and decrypts artifacts. The only place the age private key ever lives. |

---

## Quick Start

```bash
# 1. Generate an age key pair OFFLINE on your machine.
age-keygen -o ~/age.key
# Two lines: the public recipient (age1qx...) and the private identity
# (AGE-SECRET-KEY-1...). Keep this file safe — it's the only way to
# decrypt your backups.

# 2. Install the operator with the public key as a Helm value.
#    From OCI registry (recommended):
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient-here"

#    Or from local chart (development):
# helm install backup-operator ./charts/backup-operator \
#   -n backup --create-namespace \
#   --set agePublicKeys="age1qx...your-recipient-here"

# 3. Declare a backup source: connection + policy in the CR, password in a Secret.
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: prod-users-credentials
type: Opaque
stringData:
  password: super-secret
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: prod-users
spec:
  database:
    type: postgres
    host: postgres.production.svc.cluster.local
    port: 5432
    name: users
    username: backup
    credentialsSecretRef:
      name: prod-users-credentials
  schedule: "0 2 * * *"
EOF

# 4. Declare a destination (S3 example).
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: prod-s3-credentials
type: Opaque
stringData:
  access-key-id: AKIA...
  secret-access-key: ...
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupDestination
metadata:
  name: prod-s3
spec:
  type: s3
  pathPrefix: backups/prod
  s3:
    bucket: my-backups
    region: eu-central-1
  credentialsSecretRef:
    name: prod-s3-credentials
EOF

# 5. Confirm the source is Ready and the CronJob was reconciled.
kubectl -n backup get backupsources
# NAME         TYPE       SCHEDULE    SUSPENDED   LAST RUN   LAST SUCCESS   VERIFIED   READY   AGE
# prod-users   postgres   37 2 * * *                                                  True    5s
kubectl -n backup get cronjobs
# NAME                 SCHEDULE      ...
# backup-prod-users    37 2 * * *

# 6. Trigger a manual run instead of waiting for the schedule.
kubectl -n backup create job --from=cronjob/backup-prod-users manual-$(date +%s)

# 7. Restore (run from your laptop, with the offline private key).
backup-restore --destination prod-s3 -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost prod_clone
```

---

## Helm Installation & Distribution

The chart is published as an OCI artifact to GitHub Container Registry on every tagged release.

### Install from OCI registry

```bash
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient"
```

### Upgrade

```bash
helm upgrade backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --reuse-values
```

### Install from source

```bash
git clone https://github.com/behrangalavi/backup-operator.git
helm install backup-operator ./backup-operator/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient"
```

### Key Helm values

| Value | Default | Description |
|---|---|---|
| `agePublicKeys` | (required) | age public keys (array or newline-separated string); seeded as `BackupRecipient` objects at startup |
| `crds.install` / `crds.keep` | `true` / `true` | Install the CRDs as chart templates (upgradable); keep them on `helm uninstall` |
| `config.defaultSchedule` | `0 2 * * *` | Default cron schedule for new sources |
| `config.runTimeoutSeconds` | `3600` | Max seconds per backup run |
| `config.defaultRetentionDays` | `30` | Days to keep backups (0 = forever) |
| `config.defaultMinKeep` | `3` | Minimum backups to keep regardless of age |
| `ui.enabled` | `false` | Enable the management UI on port 8081 |
| `workerResources.limits.cpu` | `2000m` | CPU limit for worker pods |
| `workerResources.limits.memory` | `2Gi` | Memory limit for worker pods |
| `networkPolicy.enabled` | `false` | Ingress + egress policy for the operator (metrics from the monitoring namespace, UI only from `networkPolicy.ui.allowedFrom`) plus a worker-pod policy (`networkPolicy.worker.*`) |
| `worker.nodeSelector` / `tolerations` / `affinity` / `priorityClassName` / `extraEnv` / `extraVolumes` | (empty) | Scheduling and pod knobs for the CronJob worker pods |
| `nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` / `priorityClassName` / `extraEnv` / `extraVolumes` / `probes.*` | (empty / hardened defaults) | Operator Deployment knobs |
| `config.metricsRefreshIntervalSeconds` | `30` | Metrics refresher tick |
| `image.digest` | (empty) | Pin image by SHA256 digest for supply-chain security |

Every value is typed in `values.schema.json`; `helm install` rejects typos and out-of-range numbers.

See `charts/backup-operator/values.yaml` for the full list.

---

## Defining a Backup Target (Source)

A **Source** is a `BackupSource` object (`backup.mogenius.io/v1alpha1`) in the operator's watch namespace. The spec carries everything non-secret — connection, schedule, retention, analyzer and verification settings — and references a Secret that holds the password. The operator materialises a `batch/v1.CronJob`, owns it via `OwnerReference` (deleting the source cascades), and writes `status` so `kubectl get backupsources` shows when it last succeeded.

Invalid values are rejected by the API server at `kubectl apply` (enums, ranges, name patterns), not silently defaulted.

### `spec.database`

| Field | Required | Notes |
|---|---|---|
| `type` | yes | `postgres` \| `mysql` \| `mariadb` \| `mongo` \| `redis` |
| `host` | yes | Reachable hostname from the worker pod |
| `port` | no | Defaults: 5432 (pg), 3306 (mysql/mariadb), 27017 (mongo), 6379 (redis) |
| `name` | pg/mysql/mariadb: yes; mongo/redis: optional | Mongo: omit to back up all non-system databases. Redis: optional DB index or comma-separated list — narrows stats only; the RDB dump is always full-instance. |
| `username` | all except `redis` | Redis pre-6 uses password-only AUTH. Ignored when `credentialsSecretRef.usernameKey` is set. |
| `credentialsSecretRef.name` | yes | Secret holding the password |
| `credentialsSecretRef.passwordKey` | no (`password`) | Point at the Secret your database chart already produced (CloudNativePG, Bitnami) instead of copying the password |
| `credentialsSecretRef.usernameKey` | no | Read the username from the Secret too |
| `tls.mode` | no | Engine-native TLS mode: postgres `sslmode` values, mysql/mariadb/redis `true`/`false` (+ `verify-ca`/`verify-identity` for mysql). Applies to the stats connection AND the dump tool. |
| `tls.caSecretRef` | no | `{name, key}` of a Secret holding a CA bundle; the worker materialises it and enables certificate verification |
| `extra` | no | Engine-specific options, e.g. `max-allowed-packet: "1G"` (MySQL/MariaDB), `authSource: admin` / `replicaSet` (Mongo) |

### Other `spec` fields

| Field | Default | Effect |
|---|---|---|
| `targetName` | `metadata.name` | Logical target name used in metrics, object paths and the CronJob name. Only set when migrating a source whose legacy name differed from its Secret name. |
| `schedule` | `0 2 * * *` (chart default) | Cron expression. Standard 5-field syntax or `@daily`-style descriptors. |
| `jitterMinutes` | unset | Spread the cron minute per source (0–60). Default: only when the minute is literally `0`. `0` pins the schedule. |
| `suspended` | `false` | Pause scheduled runs; manual triggers still work. |
| `destinations` | all | Allow-list of `BackupDestination` names. |
| `retention.days` / `retention.minKeep` | `30` / `3` (chart defaults) | Delete dumps older than N days (`0` = forever), never below `minKeep` newest. |
| `compression` | `gzip` | `gzip` or `zstd`. |
| `analyzer.enabled` | `true` | Set to `false` if the user lacks `pg_stat_*` access. |
| `analyzer.rowDropThresholdPercent` / `sizeDropThresholdPercent` | `50` | Flag a table / the dump when it shrinks below this percentage of the previous run. |
| `analyzer.anonymizeTables` | `false` | Hash table names in `meta.json`. |
| `analyzer.emptyDumpCheck` | `true` | Hard-fail a dump that has no data although the source has rows. |
| `restoreVerification.*` | `mode: off` | See [Restore Verification](#restore-verification). |

### Examples

**PostgreSQL:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: orders-credentials
stringData:
  password: ...
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: orders
spec:
  database:
    type: postgres
    host: postgres.orders.svc.cluster.local
    name: orders
    username: backup
    credentialsSecretRef: { name: orders-credentials }
    tls: { mode: require }
  schedule: "*/30 * * * *"
```

**PostgreSQL managed by CloudNativePG** (reuse the cluster's own app Secret, nothing copied):
```yaml
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: orders
spec:
  database:
    type: postgres
    host: orders-rw.orders.svc.cluster.local
    name: app
    credentialsSecretRef:
      name: orders-app          # created by CNPG
      usernameKey: username
      passwordKey: password
```

**MySQL / MariaDB** (same `mysqldump` tool; set `type: mariadb` for MariaDB):
```yaml
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: legacy
spec:
  database:
    type: mysql
    host: mysql.legacy.svc.cluster.local
    name: app
    username: backup
    credentialsSecretRef: { name: legacy-credentials }
    extra:
      max-allowed-packet: "1G"
  schedule: "0 3 * * *"
```

**MongoDB:**
```yaml
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: events
spec:
  database:
    type: mongo
    host: mongo.events.svc.cluster.local
    username: backup
    credentialsSecretRef: { name: events-credentials }
    extra:
      authSource: admin
  schedule: "0 4 * * *"
```

**Redis** (RDB snapshot via `redis-cli --rdb`; full-instance, all DB indexes):
```yaml
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: sessions
spec:
  database:
    type: redis
    host: redis.sessions.svc.cluster.local
    # username is optional — only needed for Redis 6+ ACL users
    credentialsSecretRef: { name: sessions-credentials }
```

### Status

```bash
kubectl -n backup get backupsources
kubectl -n backup describe backupsource orders    # conditions + Events
```

| Status field | Meaning |
|---|---|
| `conditions[Ready]` | The managed CronJob matches the spec (`False/InvalidSpec` with the reason otherwise) |
| `conditions[LastRunSucceeded]`, `lastRunTime`, `lastRunStatus`, `lastRunError` | Most recent run found in storage |
| `lastSuccessTime`, `lastDumpSizeBytes`, `schemaChangedAt` | Most recent successful run |
| `destinations[]` | Per-destination outcome of the last run |
| `restoreVerification`, `conditions[Verified]` | Latest verifier verdict |
| `cronJobName`, `materializedSchedule`, `nextScheduleTime` | What actually runs, and when |

---

## Defining a Destination

A **Destination** is a `BackupDestination` object. Destinations are discovered at run time by each worker; the operator probes them for the `Reachable` status condition and the dashboard.

| Field | Effect |
|---|---|
| `type` | `s3` \| `sftp` \| `ftps` \| `azure` \| `gcs` |
| `pathPrefix` | Prepended to every object path. Useful for separating clusters/environments inside a shared bucket. `..` segments are rejected. |
| `credentialsSecretRef.name` | Secret holding the backend credentials (see per-type tables) |
| `<type>` block | Exactly one of `s3`, `sftp`, `ftps`, `azure`, `gcs`, matching `type` |

`metadata.name` is the logical destination name matched against `BackupSource.spec.destinations`.

### S3-compatible (`type: s3`)

Works with **AWS S3, MinIO, Hetzner Object Storage, Cloudflare R2, Backblaze B2, Wasabi**, and anything else speaking the S3 API.

| `spec.s3` field | Required | Notes |
|---|---|---|
| `bucket` | yes | Must already exist; the operator does not create buckets. |
| `region` | no | Defaults to `us-east-1`; non-AWS providers usually ignore this. |
| `endpoint` | no | Required for non-AWS (`http://minio.backup.svc:9000`, `https://gateway.eu1.storjshare.io`, …). |
| `pathStyle` | no | `true` for MinIO and others that require path-style addressing. |

Credentials Secret keys: `access-key-id`, `secret-access-key`.

### SFTP (`type: sftp`)

| `spec.sftp` field | Required | Notes |
|---|---|---|
| `host` | yes | |
| `port` | no | Defaults to 22; Hetzner Storage Box uses 23. |
| `username` | yes | |
| `knownHosts` | recommended | Output of `ssh-keyscan host`. Use `[host]:port` for non-22 ports. **Without it the connection is rejected** unless `insecureSkipHostVerify: true` (logs an `INSECURE` warning). |

Credentials Secret keys: `ssh-private-key` (PEM) **or** `password` — exactly one.

### FTPS (`type: ftps`), Azure (`type: azure`), GCS (`type: gcs`)

| Type | `spec.<type>` fields | Credentials Secret keys |
|---|---|---|
| `ftps` | `host`, `port`, `username`, `tlsMode` (`explicit`/`implicit`), `insecureSkipCertVerify` | `password` |
| `azure` | `accountName`, `container` | `account-key` |
| `gcs` | `bucket` | `service-account-json` |

### Examples

**AWS S3:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aws-prod-credentials
stringData:
  access-key-id: AKIA...
  secret-access-key: ...
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupDestination
metadata:
  name: aws-prod
spec:
  type: s3
  pathPrefix: cluster-prod
  s3:
    bucket: my-backups
    region: eu-central-1
  credentialsSecretRef: { name: aws-prod-credentials }
```

**MinIO (in-cluster):**
```yaml
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupDestination
metadata:
  name: minio
spec:
  type: s3
  s3:
    bucket: backups
    endpoint: http://minio.backup.svc.cluster.local:9000
    pathStyle: true
  credentialsSecretRef: { name: minio-credentials }
```

**Hetzner Storage Box (SFTP, port 23):**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: hetzner-credentials
stringData:
  ssh-private-key: |-
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
    -----END OPENSSH PRIVATE KEY-----
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupDestination
metadata:
  name: hetzner
spec:
  type: sftp
  pathPrefix: /cluster-prod
  sftp:
    host: u123456.your-storagebox.de
    port: 23
    username: u123456
    knownHosts: |-
      [u123456.your-storagebox.de]:23 ssh-ed25519 AAAA...
  credentialsSecretRef: { name: hetzner-credentials }
```

### Migrating from the labelled-Secret model

Releases before the CRDs discovered sources and destinations through Secrets labelled `backup.mogenius.io/role`. The new operator does not read those. Convert them once:

```bash
backup-restore migrate -n backup --age-secret backup-operator-age          # print manifests (contain credentials!)
backup-restore migrate -n backup --age-secret backup-operator-age --apply  # create CRs + credentials Secrets, keep legacy Secrets
backup-restore migrate -n backup --apply --delete-legacy                   # ...and delete the legacy Secrets
```

Object names follow the legacy Secret names (sources) and logical names (destinations), so CronJob names, jitter offsets and storage paths are preserved; a destination whose logical name was not a valid DNS name is renamed and every allow-list is rewritten accordingly (printed as a warning).

---

## The Dashboard UI

The operator ships a full management UI — a single-page application (SPA) with CRUD operations, live updates, and a settings wizard. No build step, no external dependencies.

### Enable it

```bash
helm upgrade backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --reuse-values --set ui.enabled=true
```

This adds a second container port (default `8081`) and a Service port. The chart never creates an Ingress — bring your own.

### Access locally

```bash
kubectl -n backup port-forward svc/backup-operator 8081:8081
# Browser: http://localhost:8081
```

### What you get

- **Dashboard (`#/`):** overview with stats cards (source count, healthy/failed, running jobs), target table with status badges, manual trigger button per target.
- **Sources (`#/sources`):** card grid of all backup sources. Create, edit, and delete database backup sources via forms. Supports PostgreSQL, MySQL, MariaDB, MongoDB, and Redis with all configuration options.
- **Destinations (`#/destinations`):** manage storage destinations (SFTP, S3). Create, edit, and delete with full field support. Sensitive fields (passwords, SSH keys) are masked in API responses.
- **Jobs (`#/jobs`):** running and recent backup jobs with status and timing. For currently-running jobs, the Duration column shows a live progress bar driven by the median of past successful runs (last 10) for that target. The bar caps at 99 % until the run actually completes; if the run is overdue, the bar turns orange with a "länger als üblich" hint. Estimate-free fallback when no past runs are available yet.
- **Target detail (`#/target/<name>`):** full run history table — timestamps, sizes, SHA256 checksums, schema status, anomaly counts, and download buttons per run. Failed runs surface phase + full error message inline.
- **Alerts (`#/alerts`):** currently-firing backup alerts with severity counters and a sidebar pill counter. Pulls from Prometheus when configured (`alerts.prometheusURL`), otherwise re-evaluates the same conditions locally — see [Surfacing alerts in the operator UI](#surfacing-alerts-in-the-operator-ui).
- **Settings (`#/settings`):** effective operator defaults, read-only, with values.yaml export (see [Settings](#settings) below).
- **Live updates:** Server-Sent Events (SSE) push changes to all connected browsers in real time — no polling, no page refresh. Events are routed by page so editing a destination no longer re-renders the Audit log; bursts are coalesced into a single render via a 200 ms debounce.
- **Languages:** EN / DE / FR via a sidebar language picker; choice is stored in `localStorage` and falls back to `navigator.language`. Add a language by dropping `<code>.json` into `src/ui/static/i18n/` and registering the code in `app.js` — no build step.
- **Downloads:** `.age` (encrypted dump, pass-through) and `.json` (analyzer metadata).

### REST API

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/targets` | List all backup sources with latest run status |
| `GET` | `/api/targets/{name}/runs` | Run history for one target |
| `GET` | `/api/sources/{name}` | Get source configuration |
| `POST` | `/api/sources` | Create a BackupSource plus its credentials Secret |
| `PUT` | `/api/sources/{name}` | Update source configuration |
| `DELETE` | `/api/sources/{name}` | Delete the BackupSource (CronJob and UI-created credentials cascade) |
| `GET` | `/api/destinations` | List all destinations |
| `POST` | `/api/destinations` | Create a BackupDestination plus its credentials Secret |
| `GET` | `/api/destinations/{name}` | Get destination configuration |
| `PUT` | `/api/destinations/{name}` | Update destination |
| `DELETE` | `/api/destinations/{name}` | Delete the BackupDestination |
| `POST` | `/api/trigger/{target}` | Trigger a manual backup run |
| `GET` | `/api/jobs` | List running/recent jobs |
| `GET` | `/api/settings` | Get current operator settings |
| `PUT` | `/api/settings` | Update operator settings |
| `GET` | `/api/settings/export` | Download settings as `values.yaml` |
| `GET` | `/api/events` | SSE stream for live updates |
| `GET` | `/api/alerts` | Currently-firing backup alerts (Prometheus or local fallback) |
| `GET` | `/api/alerts/status` | Connectivity check for Prometheus + Alertmanager |
| `POST` | `/api/alerts/test` | Send a self-resolving test alert via Alertmanager v2 API |

### Security model

- **Typed CRUD, owned credentials.** The API addresses `BackupSource` / `BackupDestination` / `BackupRecipient` objects, never arbitrary Secrets. The only Secrets it writes are credentials Secrets that carry an `OwnerReference` to the object being edited; a Secret you manage yourself (for example the one your database chart created) is reported as a conflict instead of being overwritten.
- **Hardened headers.** Strict `Content-Security-Policy` (`script-src 'self'`, no inline handlers in the SPA, `frame-ancestors 'none'`), `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. Every request carries an `X-Request-Id`; Events emitted by the UI record the identity your auth proxy forwards (`X-Forwarded-User` / `X-Auth-Request-Email`) as an audit hint.
- **No built-in auth.** Cluster-internal use is the assumed default. To expose externally, put `oauth2-proxy`, an Ingress with basic-auth annotation, or your platform's SSO in front of the Service (see CLAUDE.md §3.1 for examples).
- **Sensitive data masked.** Passwords, SSH keys, and access keys are returned as `***` in API responses. They are only written, never read back.
- **Pass-through downloads.** The operator streams encrypted bytes from the destination to the client without decrypting. The age private key never enters the cluster.

---

## Settings

The Settings page (`#/settings`) shows the operator's **effective fleet-wide defaults** — default cron schedule, run timeout, retention defaults, temp directory, worker resources — read-only. These are Helm values turned into env vars, and the chart is their single source of truth. Per-source settings are typed fields on each `BackupSource` and are edited on the Sources page.

### Export for GitOps

Click **"Export values.yaml"** to download the effective defaults as a Helm-compatible values file. Commit it to your repo and apply with:

```bash
helm upgrade backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup -f values.yaml
```

---

## Restore

The `backup-restore` CLI runs **on your machine**, not in the cluster. It reads the `BackupDestination` and its credentials Secret via your kubeconfig, downloads the chosen artifact, and decrypts with the offline private key. If a download fails midway the partial output file is removed.

### Build the binary

```bash
just build-restore
# produces dist/native/backup-restore
```

### List available dumps

```bash
backup-restore --destination aws-prod -n backup --target prod-users --list

# 20260428T020005Z  prod-users/2026/04/28/dump-20260428T020005Z.sql.gz.age
# 20260427T020003Z  prod-users/2026/04/27/dump-20260427T020003Z.sql.gz.age
# ...
```

### Restore the latest

```bash
# To stdout, decompressed, ready to pipe into psql/mysql/mongorestore:
backup-restore --destination aws-prod -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost new_users_db
```

### Restore a specific timestamp

```bash
backup-restore --destination aws-prod -n backup --target prod-users \
  --age-key ~/age.key --timestamp 20260428T020005Z -o dump.sql.gz
gunzip dump.sql.gz   # or pipe `--decompress` directly
```

### Flags

| Flag | Required | Default |
|---|---|---|
| `--destination` | yes | — (repeatable or comma-separated; list/latest merge across all, `--purge` deletes from all) |
| `--target` | yes | — |
| `--namespace` (`-n`) | no | `default` |
| `--age-key` | yes (for download) | — |
| `--timestamp` | no | latest |
| `--list` | no | false |
| `--decompress` | no | false |
| `-o` | no | `-` (stdout) |
| `--purge` + `--before` + `--dry-run` | no | right-to-erasure: delete every artifact of a target (optionally older than a date) from every `--destination` given; exits non-zero with `ERASURE INCOMPLETE` if anything could not be deleted |
| `--compression` | no | auto: dump filename suffix, then `meta.json`, then gzip |

The downloaded ciphertext is checked against the `sha256` recorded in `meta.json`; a mismatch exits non-zero and removes the output file. Decrypt errors carry a hint (wrong private key vs. damaged object).

`backup-restore migrate ...` converts legacy labelled Secrets into CRs — see [Migrating from the labelled-Secret model](#migrating-from-the-labelled-secret-model).

---

## Restore Verification

> A backup that hasn't been restored is not a backup.

The operator can prove on a configurable cadence that an encrypted dump can actually be decrypted, parsed, and (optionally) restored — **without ever sending the age private key into the cluster**. The worker generates a one-shot X25519 keypair in process memory at the moment it decides to verify, encrypts the dump for *both* the long-lived disaster-recovery recipient and the ephemeral one (age supports multi-recipient natively, ~200 bytes header overhead per recipient), runs the verifier in-process, then the pod terminates and the ephemeral private half is gone. The DR key is unaffected and remains the only path that can decrypt the artifact for a real recovery.

### Modes

Set on a source via `spec.restoreVerification.mode`. Off by default. The field is an enum in the CRD schema, so a typo is rejected at `kubectl apply` instead of silently running no verification.

| Mode | What it does | RBAC needed | Cost |
|---|---|---|---|
| `off` | No verification. (default) | none | — |
| `stream-validate` | In-process: decrypt with the ephemeral identity → gunzip → engine-aware parser. **SQL engines** (postgres / mysql / mariadb): re-runs `dumper.RowCounter` against the plaintext stream, header sanity-check against the engine banner, then total-rows comparison vs pre-dump stats with the same 99 % tolerance the dump-time verifier uses. **Mongo:** asserts the BSON archive magic `0x8199e26d` and drains the stream so a corrupt gzip layer fails loudly. **Redis:** asserts the `REDIS` magic + 4 ASCII version digits, drains the body. Catches bit rot, broken encryption, corrupt gzip, truncated dumps, "looks like a dump but isn't" garbage. | none | seconds; no extra pods |
| `schema-only` | Spawns an ephemeral DB pod, restores DDL only. SQL engines stream-filter the `COPY ... FROM stdin` body and `INSERT INTO` lines on the fly so the engine literally never sees the data — proves schema restores cleanly without paying data-restore cost on a 50 GiB DB. **Mongo / Redis:** `schema-only` is a label, not a meaningfully cheaper restore — the BSON archive and RDB binary aren't decomposable that way. Pick `stream-validate` instead on those engines if cost matters. | `pods: create/get/list/watch/delete`, `pods/status: get`, `pods/log: get` in worker namespace | a small DB pod for ~30 s |
| `sample` | Spawns an ephemeral DB pod, restores schema + a sample of data. Catches data-encoding issues a schema-only run would miss. From the engine's perspective `sample` is currently equivalent to `full`; pre-filtering hooks are reserved for a future iteration. | same as above | small pod, modest disk |
| `full` | Spawns an ephemeral DB pod, restores the entire dump, runs smoke queries (per-table `SELECT count(*)` for SQL engines; ping + auth roundtrip for Redis whose RDB-restore is deferred to a follow-up iteration). Highest fidelity. | same as above | full-size pod and emptyDir; node needs the headroom |

### `spec.restoreVerification`

| Field | Default | Effect |
|---|---|---|
| `mode` | `off` | One of the modes above (enum). |
| `interval` | `168h` (weekly) | Minimum gap between verifier runs (Go duration: `30m`, `48h`, `168h` …). **State-driven:** the worker reads `latestMeta.restoreVerification.completedAt` and skips when `now - completedAt < interval`. Cron drift doesn't matter; manual runs (`kubectl create job --from=cronjob/...`) verify whenever overdue. The very first run after enabling verification falls through to a "first verification" path that runs immediately so operators see signal without waiting one full interval. |
| `image` | per-DB-type default | Container image for the verifier pod. Pin to the source DB's exact major version when restore semantics depend on it (charset defaults, function signatures, dump-format compatibility). **Phase-2 modes only** (`schema-only` / `sample` / `full`) — `stream-validate` ignores it. Per-engine defaults: `postgres:16-alpine`, `mysql:8.0`, `mariadb:11`, `mongo:7`, `redis:7-alpine`. |
| `volumeSize` | `1Gi` (schema-only), `5Gi` (sample), `50Gi` (full) | `emptyDir.sizeLimit` for the verifier pod's data volume (Kubernetes quantity). Override when one source's restore needs more headroom — at scale, the node's ephemeral storage is a real budget. **Phase-2 modes only.** |

```yaml
spec:
  restoreVerification:
    mode: schema-only
    interval: 72h
    image: postgres:15.5-alpine
    volumeSize: 20Gi
```

The result lands in `status.restoreVerification` and the `Verified` condition.

### Enabling Phase 2 in the chart

Phase-2 modes spawn ephemeral DB pods and need a wider RBAC grant on the worker ServiceAccount. Off by default; flip on with:

```yaml
# values.yaml
restoreVerification:
  enableEphemeralPodSpawn: true
```

The chart then grants the worker SA `pods: create/get/list/watch/delete`, `pods/status: get`, and `pods/log: get` in its own namespace. Without this flag, attempting to set a Phase-2 mode produces a `Verdict=Skipped` result with the RBAC error captured in `meta.json`.

The spawned pod is restricted-PSA compliant out of the box: `runAsNonRoot=true`, `RunAsUser=999`, `readOnlyRootFilesystem=true`, `capabilities.drop=ALL`, `seccompProfile=RuntimeDefault`. It carries an `OwnerReference → worker pod`, so the moment the worker exits, K8s GC cascades the verifier pod away regardless of completion state.

### When does verification actually run?

`ShouldVerify` (in `verifier/verifier.go`) skips a run for any of:

- mode is `off` or absent
- **No prior run for this target** — verifier needs a `preStats` baseline that the regular DumpVerification has already established. The very first backup of a new source establishes that baseline; verification starts from the second run.
- Interval not elapsed since the last `completedAt`.

It runs (and emits `restore_verification_passed` + `_last_timestamp`) when:

- Mode is set and the interval has elapsed, OR
- Mode is set but no `RestoreVerification` block exists on the latest meta yet (the "first verification" path fires immediately).

### Alerts

Two rules ship with the chart:

| Alert | Condition | Severity |
|---|---|---|
| `BackupRestoreVerificationFailed` | `max by (target, mode) (backup_operator_restore_verification_passed) == 0` for 5 m | **critical** |
| `BackupRestoreVerificationStale` | `time() - max by (target, mode) (backup_operator_restore_verification_last_timestamp_seconds) > 86400 * 14` for 1 h | warning |

A failed or skipped verification does **not** fail the backup run itself — verification is observability, not a gate. The dump still uploads, the artifact remains decryptable with the DR key, and downstream integrations are unaffected. The alerts exist so an operator can investigate before the next disaster-recovery drill.

### What the UI shows

- **Target Detail (`#/target/<name>`):** every run carries a verification badge — `match` / `mismatch` / `skipped` / `not configured` — plus the verifier mode, completed-at, and ephemeral-recipient fingerprint. The fingerprint is per-run; seeing the same one twice in a row would itself be a bug, since the keypair regenerates on every verifier-run.
- **Source form → "Restore Verification":** four fields mirroring `spec.restoreVerification` (mode, interval, verifier image, volume size). Phase-2-only fields are disabled in the form when mode is `off` or `stream-validate`.

---

## Documentation Portal

The operator can serve `CLAUDE.md`, `README.md`, and a generated tech-stack page (built from `go.mod`) on a separate port. Off by default. The portal is **read-only** — it has no Kubernetes client, no Secret access, no ability to mutate state. That's what justifies exposing it more loosely than the management UI.

### Enable it

```yaml
# values.yaml
docs:
  enabled: true
  port: 8083  # default
```

### Access locally

```bash
kubectl -n backup port-forward svc/backup-operator 8083:8083
# Browser: http://localhost:8083
```

### What you get

- **`/`** — README (this file), rendered with goldmark.
- **`/claude`** — CLAUDE.md (operator reference + architectural decisions).
- **`/tech-stack`** — direct dependencies from `go.mod`, with version + purpose.
- **In-page search** — Ctrl+K opens a dropdown with snippet highlighting across all pages. Client-side only; no server-side index, so the portal works on read-only mounts.

### Why a separate port

The management UI mutates Secrets, ConfigMaps, and Jobs; it must be SSO-gated in production. The docs portal renders Markdown files. Splitting them onto separate ports lets cluster admins write two distinct ingress rules — public docs, gated UI — without code changes.

---

## Alerting & Monitoring

### Metrics

The **operator pod** exposes Prometheus metrics on `:8080/metrics`. Run-level signals come from the operator's `MetricsRefresher` controller, which periodically reads the latest `*.meta.json` sidecar from each destination and writes the resulting state into the gauges below. This is why the run-state metrics are **gauges, not counters** — worker pods are too short-lived for Prometheus to scrape, so the operator reconstructs the state from storage instead.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backup_operator_dump_size_bytes` | Gauge | `target` | Encrypted size of the most recent successful dump |
| `backup_operator_dump_size_change_ratio` | Gauge | `target` | current/previous encrypted size; `<0.5` = suspicious shrinkage |
| `backup_operator_table_count` | Gauge | `target` | Tables/collections in the most recent successful run |
| `backup_operator_table_row_count` | Gauge | `target`, `table` | Per-table row count (estimate) at the most recent run |
| `backup_operator_schema_changed` | Gauge | `target` | 1 if schema hash differs from previous run, 0 otherwise |
| `backup_operator_charset_changed` | Gauge | `target` | 1 if database character set or collation differs from previous run; warns about silent multibyte truncation at restore time |
| `backup_operator_schema_last_change_timestamp_seconds` | Gauge | `target` | Unix ts of the most recent run where the schema fingerprint actually changed; carried forward across unchanged runs |
| `backup_operator_last_run_anomalies` | Gauge | `target` | Analyzer anomaly count in the most recent run |
| `backup_operator_last_run_status` | Gauge | `target` | 1 = most recent run wrote a usable artifact, 0 = failure |
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts of last successful upload to that destination |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if the destination is unreadable / last upload failed |
| `backup_operator_storage_scrub_passed` | Gauge | `target`, `destination` | 1 if the most recent scrub matched the recorded SHA256, 0 if mismatch. Only present when `STORAGE_SCRUB_ENABLED=true`. |
| `backup_operator_storage_scrub_last_check_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts of the most recent scrub attempt |
| `backup_operator_storage_scrub_failed_total` | Counter | `target`, `destination` | Cumulative scrub failures (mismatch or unreachable). Operator-side, scraped normally. |
| `backup_operator_restore_verification_passed` | Gauge | `target`, `mode` | 1 if the most recent restore-verifier run produced verdict `match`, 0 for `mismatch`/`skipped`. Absent until at least one verifier has run. |
| `backup_operator_restore_verification_last_timestamp_seconds` | Gauge | `target`, `mode` | Unix ts of the most recent restore-verifier completion. Drives the stale alert. |
| `backup_operator_retention_deleted_total` | Counter | `target`, `destination`, `kind` | Worker-only — see caveat below |
| `backup_operator_retention_failed_total` | Counter | `target`, `destination` | Worker-only — see caveat below |
| `backup_operator_dump_duration_seconds` | Histogram | `target`, `db_type` | Worker-only — see caveat below |
| `backup_operator_upload_duration_seconds` | Histogram | `target`, `destination`, `storage_type` | Worker-only — see caveat below |
| `backup_operator_run_duration_seconds` | Histogram | `target`, `db_type` | Worker-only — see caveat below |
| `backup_operator_restore_verification_duration_seconds` | Histogram | `target`, `mode` | Worker-only — see caveat below |

**Caveat — "worker-only" metrics:** the histograms and `retention_*` counters are observed inside the worker pod, but worker pods finish in seconds and Prometheus's scrape interval (≥15s) cannot catch them. They live in the codebase for future use (e.g. an aggregator that pushes to Pushgateway or rebuilds them from meta.json). For timing alerts today, rely on Job duration via `kube-state-metrics`.

### ServiceMonitor

Enabled by default when the Prometheus Operator CRDs are installed. The chart's templates render the ServiceMonitor only if `monitoring.coreos.com/v1` is present.

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 30s
  scrapeTimeout: 10s

# Top-level convenience: kube-prometheus-stack's Prometheus selects
# ServiceMonitors and PrometheusRules via `release: <name>`. The chart
# applies this label to BOTH out of the box. Set to "" to opt out and
# use your own labels via {serviceMonitor,prometheusRule}.labels instead.
prometheusReleaseLabel: "kube-prometheus-stack"
```

Without this label, kube-prometheus-stack's Prometheus *renders* the CRDs but never loads them — the most common reason backup alerts silently don't fire. If your stack uses a different release name, override accordingly.

If you don't run Prometheus Operator, set `serviceMonitor.enabled=false` and configure your scrape job manually pointing at `:8080/metrics`.

### Default Alert Rules

The chart ships a `PrometheusRule` with eleven default alerts. They're rendered only when `monitoring.coreos.com/v1` is present, and they live under `prometheusRule.rules` in `values.yaml` so you can override or add your own at install time.

| Alert | Expression (simplified) | For | Severity |
|---|---|---|---|
| `BackupOverdue` | `time() - max by (target) (last_success_timestamp_seconds) > 86400 * 1.5` | 10m | warning |
| `BackupDestinationFailing` | `max by (target, destination) (destination_failed) == 1` | 15m | warning |
| `BackupDumpSizeCollapsed` | `dump_size_change_ratio < 0.5` | 5m | **critical** |
| `BackupSchemaChanged` | `schema_changed == 1` | 1m | info |
| `BackupCharsetChanged` | `charset_changed == 1` | 1m | warning |
| `BackupStorageCorrupted` | `max by (target, destination) (storage_scrub_passed) == 0` | 1m | **critical** |
| `BackupAnomaliesAppearing` | `last_run_anomalies > 0` | 5m | warning |
| `BackupLastRunFailed` | `last_run_status == 0` | 5m | warning |
| `BackupSucceeded` | `time() - max by (target) (last_success_timestamp_seconds) < 120` | — | info |
| `BackupRestoreVerificationFailed` | `max by (target, mode) (restore_verification_passed) == 0` | 5m | **critical** |
| `BackupRestoreVerificationStale` | `time() - max by (target, mode) (restore_verification_last_timestamp_seconds) > 86400 * 14` | 1h | warning |

`BackupSucceeded` is a heartbeat-style positive signal — every successful run produces one firing + one resolved notification (Alertmanager has `send_resolved: true`). With a frequent cron this is intentionally noisy; silence it if you only want failure alerts.

The semantic alerts (`BackupDumpSizeCollapsed`, `BackupSchemaChanged`, `BackupAnomaliesAppearing`) are this project's main differentiator. They alert on *what's actually in the dump*, not on whether the job exited cleanly. A backup that succeeds with empty tables silently in another tool will page you here.

### Wiring to Alertmanager / Slack / PagerDuty

The chart deliberately ships **no notifier**. Routing, deduplication, silencing, and rendering belong in Alertmanager. Configure your Alertmanager once for all your alerts (cluster-wide), and the rules above will route there automatically via the Prometheus Operator's standard pipeline.

Minimal Alertmanager route example for these alerts:

```yaml
route:
  receiver: 'default'
  routes:
    - matchers: [ alertname=~"Backup.*", severity="critical" ]
      receiver: 'pagerduty'
    - matchers: [ alertname=~"Backup.*" ]
      receiver: 'slack-backups'

receivers:
  - name: 'default'
  - name: 'pagerduty'
    pagerduty_configs:
      - service_key: ${PAGERDUTY_KEY}
  - name: 'slack-backups'
    slack_configs:
      - api_url: ${SLACK_WEBHOOK}
        channel: '#alerts-backups'
        title: '{{ .CommonAnnotations.summary }}'
        text: |-
          target: {{ .CommonLabels.target }}
          {{- if .CommonLabels.destination }}
          destination: {{ .CommonLabels.destination }}
          {{- end }}
```

### Customising the alerts

Override `prometheusRule.rules` at install time to drop, modify, or add rules:

```bash
helm upgrade backup-operator ./charts/backup-operator -n backup --reuse-values \
  --values custom-rules.yaml
```

```yaml
# custom-rules.yaml
prometheusRule:
  rules:
    - alert: BackupOverdue
      expr: time() - max by (target) (backup_operator_last_success_timestamp_seconds) > 7200  # 2h instead of 36h
      for: 5m
      labels: { severity: critical, team: data }
      annotations:
        summary: "Backup target {{ $labels.target }} overdue >2h"
    # ... add or omit other rules ...
```

### Disabling alerts entirely

```bash
helm upgrade ... --set prometheusRule.enabled=false
```

### Surfacing alerts in the operator UI

When the UI is enabled (`ui.enabled=true`), an **Alerts** tab in the sidebar shows currently-firing alerts with severity counters (critical/warning/info) and a sortable list. Two providers feed it, picked automatically:

- **Prometheus mode** (preferred) — queries `<prometheusURL>/api/v1/alerts` filtered by `alertname=~"^Backup.*"` so the UI mirrors what Alertmanager will route. Honours each rule's `for:` duration.
- **Local heuristic** (fallback) — when `prometheusURL` is empty or unreachable, the operator re-evaluates the same six conditions on its own gathered metric registry. Useful before kube-prometheus-stack is wired up; does **not** honour `for:` debounce, so it fires immediately on threshold crossing.

The UI marks each alert with a `source` badge (`prometheus` vs `local`) so you can see which view you're looking at.

```yaml
# values.yaml — defaults assume kube-prometheus-stack lives in the `alert` namespace
alerts:
  prometheusURL: "http://prometheus-operated.alert.svc.cluster.local:9090"
  alertmanagerURL: "http://alertmanager-operated.alert.svc.cluster.local:9093"
```

`alertmanagerURL` is currently used for the *"Open in Alertmanager"* link plus a connectivity-status indicator and a one-click *"Send test alert"* button. The operator never *receives* notifications from Alertmanager — wiring stays one-way through Prometheus.

Set both to `""` to disable the integration entirely; the UI then falls back to local-heuristic mode silently.

---

## Encryption Model

Backups are encrypted with [`age`](https://age-encryption.org/) — modern, audited, public-key-only encryption.

### How it works

```
Operator's machine (offline):
  age-keygen -o age.key       →   ~/age.key
                                  ├── public:  age1qx...   (RECIPIENT)
                                  └── private: AGE-SECRET-KEY-1...  (NEVER LEAVES YOUR LAPTOP)

Cluster (online):
  Helm install --set agePublicKeys="age1qx..."
   └── operator seeds one BackupRecipient per key at startup
        └── RecipientReconciler merges all BackupRecipients into
            Secret backup-operator-age (key AGE_PUBLIC_KEYS)
             └── mounted into every worker pod via secretKeyRef

Worker pod runtime:
  pg_dump | gzip | age encrypt -r <recipient>  →  dump.sql.gz.age

Restore (offline):
  age -d -i ~/age.key dump.sql.gz.age | gunzip | psql ...
```

### Properties enforced by the code

- The operator refuses to start without `AGE_PUBLIC_KEYS`. There is no plaintext-backup code path.
- The age recipient list is newline-separated → key rotation works by adding the new public key to the list before retiring the old; both can decrypt during the transition.
- The restore CLI accepts the same multi-key format → matches `age-keygen -o`'s output.
- Storage backends never see plaintext bytes — they receive `*.sql.gz.age` ciphertext only.
- The dashboard UI streams encrypted bytes pass-through; the operator never decrypts.

### Properties you must enforce operationally

- **Keep the private key offline.** Putting it in the cluster collapses the entire security model.
- **Back up the private key separately.** Paper, hardware token, password manager, anything but the cluster. Losing it means losing every backup it can decrypt.
- **Rotate by adding, then retiring.** Add a new `BackupRecipient` (via `kubectl`, GitOps or the UI Age-Keys page), run a few backups (so they're encrypted to both keys), then delete the old recipient object. New recipients can decrypt going forward; old recipients still work for older artifacts.
- **For multi-region recovery, distribute multiple public keys to the cluster** and keep each region's private key in that region's safe.

---

## CI/CD

Two GitHub Actions workflows automate testing and releasing. See `.github/workflows/` for the full YAML.

### CI (`ci.yaml`)

Runs on every pull request to `main`:

| Job | Steps |
|---|---|
| `test` | `go build ./...` → `go test -race ./...` (coverage artifact) → `go vet ./...` |
| `generate-check` | regenerates DeepCopy + CRD YAML with the pinned controller-gen and fails on drift |
| `lint` | golangci-lint (pinned) |
| `govulncheck` | pinned govulncheck over `./...` |
| `ui-test` | `node --check` on every SPA file + `node --test 'src/ui/static/js/tests/*.test.mjs'` |
| `helm` | `helm lint --strict`, helm-unittest suites, two `helm template` variants (defaults / all features incl. worker knobs + NetworkPolicy), kubeconform |
| `docker-build` | native-arch smoke build + DB-client tool check |

### Release (`release.yaml`)

Triggered automatically on every push to `main` via [Semantic Release](https://github.com/semantic-release/semantic-release). The version is determined from commit messages — no manual tagging required.

1. Re-run the full CI gate (`ci.yaml` via `workflow_call`).
2. Analyze commits since the last release to determine the next version.
3. Build a multi-arch Docker image (`linux/amd64` + `linux/arm64`, base images digest-pinned), scan it with Trivy (fails on HIGH/CRITICAL; `.trivyignore` honoured) and sign the digest with cosign (keyless).
4. Push the image to `ghcr.io/behrangalavi/backup-operator:<version>` and `:latest`.
5. Package the Helm chart with the matching version, push it to `oci://ghcr.io/behrangalavi/charts/backup-operator` and sign the chart digest.
6. Create a GitHub Release with auto-generated release notes.

### Commit conventions

Releases are driven by [Conventional Commits](https://www.conventionalcommits.org/):

| Prefix | Effect | Example |
|---|---|---|
| `fix:` | Patch release (1.0.x) | `fix: correct retention day calculation` |
| `feat:` | Minor release (1.x.0) | `feat: add S3 destination support` |
| `feat!:` or `BREAKING CHANGE:` | Major release (x.0.0) | `feat!: rename source annotations` |
| `docs:`, `ci:`, `chore:` | No release | `docs: update README` |

Merging a PR with `fix:` or `feat:` commits triggers an automatic release. No manual `git tag` needed.

After the workflow completes, users can install with:

```bash
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys="age1qx...your-recipient"
```

---

## Local Development

### Prerequisites

- Go 1.26+ (for native binaries)
- [`just`](https://github.com/casey/just) for the task runner
- Docker Desktop with Kubernetes enabled
- `kubectl`, `helm`
- Optionally: `age` CLI (`brew install age`)

### Docker Desktop K8s caveat

Modern Docker Desktop versions implement Kubernetes via **`kind` under the hood** (`docker desktop kubernetes status` reports `Mode: kind`). The kind node has its own containerd, separate from the Docker daemon's image store.

**To make local builds visible to the cluster:**

1. Open **Docker Desktop → Settings → General**.
2. Check **"Use containerd for pulling and storing images"**.
3. Click **Apply & Restart** (the cluster gets reset — workloads will be wiped).

After this, `docker info` shows `Storage Driver: overlayfs` (containerd snapshotter). `docker build` outputs end with an `unpacking to docker.io/library/...` line confirming the image landed in the shared store.

**Use `imagePullPolicy: IfNotPresent`, not `Never`.** Docker Desktop's `desktop-containerd-registry-mirror` bridges pulls from the kind cluster to the daemon's containerd — but only when kubelet actually attempts a pull. With `Never`, kubelet never tries, and the mirror cannot help.

### One-command test stack

```bash
# Copy the env template; defaults match the test stack.
cp .env.example .env

# Build the operator/worker image into the (now-shared) containerd store.
just build-image

# Generate an age key pair offline + register the public key as a
# BackupRecipient (installs the CRDs first).
just gen-age-key

# Apply the test stack: namespace, worker SA/RBAC, in-cluster Postgres
# (with a seed table), in-cluster MinIO (with bucket init), BackupSource +
# BackupDestination with their credentials Secrets.
just test-up

# Install the operator via Helm.
helm install backup-operator ./charts/backup-operator -n backup \
  --set image.repository=backup-operator \
  --set image.tag=dev \
  --set image.pullPolicy=IfNotPresent \
  --set agePublicKeys="$(grep 'public key:' ~/age-backup-test.key | cut -d' ' -f4)" \
  --set serviceMonitor.enabled=false \
  --set prometheusRule.enabled=false \
  --set ui.enabled=true

# Trigger a manual run instead of waiting 5 minutes.
just test-trigger

# Browse the dashboard.
kubectl -n backup port-forward svc/backup-operator 8081:8081
# → http://localhost:8081

# Tear it all down.
just test-down
helm uninstall backup-operator -n backup
```

### Iterating on code

When you change the operator/worker code, you have to rebuild **and** make sure the cluster pulls the new image. The kind cluster doesn't notice rebuilt-with-same-tag images. Two options:

**Option A — unique tag per build (recommended):**
```bash
TAG="dev-$(date +%s)"
docker build -t backup-operator:$TAG .
helm upgrade backup-operator ./charts/backup-operator -n backup --reuse-values \
  --set image.tag=$TAG
```

**Option B — same tag, force pod recreation:**
```bash
just build-image
kubectl -n backup delete pod -l app.kubernetes.io/name=backup-operator
# Pod recreates and re-resolves the image from the daemon's containerd
# (which now has the new content under the same `dev` tag).
```

### Run the operator on your laptop instead of as a Pod

The operator is a normal controller-runtime app — `ctrl.GetConfigOrDie()` falls back to your kubeconfig when there's no in-cluster token. You can run it on your machine while watching a real cluster:

```bash
just build
just run    # reads .env for required envs
```

Worker pods still run in the cluster; only the operator-pod is replaced by your local process. Useful for fast reconciler iteration without touching the cluster's image cache.

### Tests, lint, vet

```bash
just check        # lint + unit tests
just test-unit
just golangci-lint
```

---

## Troubleshooting

### `ErrImageNeverPull` on Docker Desktop

The kind cluster's containerd doesn't have the locally built image. See [Docker Desktop K8s caveat](#docker-desktop-k8s-caveat) — enable the containerd image store toggle and use `imagePullPolicy: IfNotPresent`.

### Operator pod logs `events is forbidden`

Cosmetic — controller-runtime tries to record leader-election events but the chart's RBAC doesn't include `events` (intentional, to keep the role minimal). Backups run normally; only the leader-election event is suppressed.

### Worker pod `ErrImagePull` for `postgres:17-alpine` etc.

Transient Docker Hub network issues. Delete the pod (`kubectl delete pod ...`) to clear the kubelet backoff and retry. If persistent, check the cluster's network egress.

### `BackupDumpSizeCollapsed` firing on first run

False positive. The first run has no `previous` to compare against; `dump_size_change_ratio` defaults to 0. The chart's default expression checks `< 0.5` — a fresh install will trigger it once. After the second run completes, the metric reflects real change ratios. Workaround: silence in Alertmanager for an hour after deployment, or filter `dump_size_change_ratio < 0.5 and dump_size_change_ratio > 0` if you accept the slight loss of sensitivity.

### `INSECURE` warnings in worker logs (SFTP destinations)

The BackupDestination is missing `spec.sftp.knownHosts`. Add the output of `ssh-keyscan -p <port> <host>` there. Without it the worker accepts any host key — a downgrade in security but not a backup failure.

### `analyzer-enabled: false` not detected

Annotation typo (e.g. `analyzer-enable`). The parser is forgiving and falls back to the default rather than rejecting the Secret, so look closely at the annotation name. Use `kubectl get secret -o yaml` to inspect.

---

## More Documentation

- **`CLAUDE.md`** — full architectural reference: module layout, factories, retention semantics, failure modes, the rationale behind every non-obvious design decision.
- **Helm chart** — `charts/backup-operator/values.yaml` is the canonical list of every knob the chart exposes.
- **Source code** — well-commented Go. Start with `cmd/main.go` (operator), `cmd/worker/main.go` (worker), `internal/backup/pipeline.go` (the actual backup pipeline).

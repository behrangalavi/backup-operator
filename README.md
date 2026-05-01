# backup-operator

A Kubernetes-native backup operator in Go for **PostgreSQL**, **MySQL**, and **MongoDB**, with public-key encryption (`age`), multi-destination fan-out (SFTP + S3-compatible), semantic dump analysis, and Prometheus-driven alerting.

> **The contract:** label a `Secret`, get a backup. No CRDs to install, no extra resources to learn. The operator watches Secrets in its namespace, materialises a `CronJob` per labelled source, and Kubernetes does the running.

---

## Table of Contents

- [Why this exists](#why-this-exists)
- [How it works](#how-it-works)
- [Quick Start](#quick-start)
- [Helm Installation & Distribution](#helm-installation--distribution)
- [Defining a Backup Target (Source)](#defining-a-backup-target-source)
- [Defining a Destination](#defining-a-destination)
- [The Dashboard UI](#the-dashboard-ui)
- [Settings Wizard](#settings-wizard)
- [Restore](#restore)
- [Alerting & Monitoring](#alerting--monitoring)
- [Encryption Model](#encryption-model)
- [CI/CD](#cicd)
- [Local Development](#local-development)
- [Troubleshooting](#troubleshooting)
- [More Documentation](#more-documentation)

---

## Why this exists

K8up, Stash, and Velero solve adjacent problems but none of them satisfy all three of:

1. **Discovery via labelled `Secret`s, not CRDs.** Labelling a Secret is the entire user contract. No CRD to install, no API surface to learn, no version skew to worry about.
2. **Semantic dump analysis.** Alerts fire on dump *content* — table disappeared, row-count collapsed, schema fingerprint changed — not just on job exit codes. A backup that "succeeds" with an empty dump is a silent disaster everywhere else; here it pages you.
3. **Multi-destination fan-out as first-class.** One dump streams to N storage backends in parallel, mixed protocols (SFTP + S3 + …). Failure of one destination doesn't fail the run.

If you don't need all three, you have simpler choices.

---

## How it works

```
   user labels a Secret               ┌──────────────────────────────────────────┐
            │                         │ Kubernetes API                           │
            ▼                         │                                          │
   ┌──────────────────┐  watch        │   Source Secret  ──┐                     │
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
| `backup-operator` | Operator Deployment | Reconciles `Source` Secret → managed `CronJob`. Optionally hosts the read-only Dashboard UI. |
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

# 3. Label a database Secret as a backup source.
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
type: Opaque
stringData:
  host: postgres.production.svc.cluster.local
  port: "5432"
  database: users
  username: backup
  password: super-secret
EOF

# 4. Label a destination Secret (S3 example).
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: prod-s3
  labels:
    backup.mogenius.io/role: destination
    backup.mogenius.io/storage-type: s3
  annotations:
    backup.mogenius.io/name: "prod-s3"
    backup.mogenius.io/path-prefix: "backups/prod"
type: Opaque
stringData:
  bucket: my-backups
  access-key-id: AKIA...
  secret-access-key: ...
  region: eu-central-1
EOF

# 5. Confirm the CronJob was reconciled.
kubectl -n backup get cronjobs
# NAME                       SCHEDULE      ...
# backup-prod-users-db       0 2 * * *

# 6. Trigger a manual run instead of waiting for the schedule.
kubectl -n backup create job --from=cronjob/backup-prod-users-db manual-$(date +%s)

# 7. Restore (run from your laptop, with the offline private key).
backup-restore --storage-secret prod-s3 -n backup --target prod-users \
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
| `agePublicKeys` | (required) | Newline-separated age public keys for encryption |
| `config.defaultSchedule` | `0 2 * * *` | Default cron schedule for new sources |
| `config.runTimeoutSeconds` | `3600` | Max seconds per backup run |
| `config.defaultRetentionDays` | `30` | Days to keep backups (0 = forever) |
| `config.defaultMinKeep` | `3` | Minimum backups to keep regardless of age |
| `ui.enabled` | `false` | Enable the management UI on port 8081 |
| `workerResources.limits.cpu` | `2000m` | CPU limit for worker pods |
| `workerResources.limits.memory` | `2Gi` | Memory limit for worker pods |
| `networkPolicy.enabled` | `false` | Restrict operator egress to known ports |
| `image.digest` | (empty) | Pin image by SHA256 digest for supply-chain security |

See `charts/backup-operator/values.yaml` for the full list.

---

## Defining a Backup Target (Source)

A **Source** is any `Secret` with `backup.mogenius.io/role=source` in the operator's watch namespace. The operator parses the Secret's labels and annotations, materialises a `batch/v1.CronJob`, and mounts an `OwnerReference` so deleting the Secret cascades to the CronJob.

### Required labels

| Label | Value |
|---|---|
| `backup.mogenius.io/role` | `source` |
| `backup.mogenius.io/db-type` | `postgres` \| `mysql` \| `mongo` |

### Required `data` keys

| Key | Required | Notes |
|---|---|---|
| `host` | yes | Reachable hostname from the worker pod |
| `port` | no | Defaults: 5432 (pg), 3306 (mysql), 27017 (mongo) |
| `database` | yes for pg/mysql; optional for mongo | Mongo: omit to back up all non-system databases |
| `username` | yes | |
| `password` | yes | |

### Annotations

| Annotation | Default | Effect |
|---|---|---|
| `backup.mogenius.io/name` | Secret name | Logical target name. Used in metrics, object paths, CronJob name. |
| `backup.mogenius.io/schedule` | `0 2 * * *` (chart default) | Cron expression. Standard Linux/Vixie syntax. |
| `backup.mogenius.io/analyzer-enabled` | `true` | Set to `false` if the user lacks `pg_stat_*` access. Disables stats collection and dump analysis for this target. |
| `backup.mogenius.io/destinations` | unset | CSV allow-list of destination *names*. Empty = fan out to all destinations in the namespace. |
| `backup.mogenius.io/retention-days` | `30` (chart default) | Delete dumps older than N days. `0` = keep forever. |
| `backup.mogenius.io/min-keep` | `3` (chart default) | Safety floor: never delete below this many newest dumps. |
| `backup.mogenius.io/extra-<key>` | none | Surfaced into `dumper.Config.Extra[key]` for db-specific options (e.g. `extra-sslmode=require`, `extra-authSource=admin`). |

A typo on a feature-flag annotation (`analyzer-enabled: tru`) silently falls back to the default — backups must keep running even if a flag is misspelled.

### Examples

**PostgreSQL:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: orders-db
  labels:
    backup.mogenius.io/role: source
    backup.mogenius.io/db-type: postgres
  annotations:
    backup.mogenius.io/name: "orders"
    backup.mogenius.io/schedule: "*/30 * * * *"
    backup.mogenius.io/extra-sslmode: "require"
type: Opaque
stringData:
  host: postgres.orders.svc.cluster.local
  database: orders
  username: backup
  password: ...
```

**MySQL:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: legacy-mysql
  labels:
    backup.mogenius.io/role: source
    backup.mogenius.io/db-type: mysql
  annotations:
    backup.mogenius.io/name: "legacy"
    backup.mogenius.io/schedule: "0 3 * * *"
type: Opaque
stringData:
  host: mysql.legacy.svc.cluster.local
  database: app
  username: backup
  password: ...
```

**MongoDB:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: events-mongo
  labels:
    backup.mogenius.io/role: source
    backup.mogenius.io/db-type: mongo
  annotations:
    backup.mogenius.io/name: "events"
    backup.mogenius.io/schedule: "0 4 * * *"
    backup.mogenius.io/extra-authSource: "admin"
type: Opaque
stringData:
  host: mongo.events.svc.cluster.local
  username: backup
  password: ...
```

---

## Defining a Destination

A **Destination** is any `Secret` with `backup.mogenius.io/role=destination`. Destinations are discovered at run time by each worker — there is no managed object for them.

### Required label

| Label | Value |
|---|---|
| `backup.mogenius.io/role` | `destination` |
| `backup.mogenius.io/storage-type` | `s3` \| `sftp` \| `hetzner-sftp` |

### Annotations

| Annotation | Effect |
|---|---|
| `backup.mogenius.io/name` | Logical destination name. Matched against source's `destinations` allow-list. Defaults to Secret name. |
| `backup.mogenius.io/path-prefix` | Prepended to every object path. Useful for separating clusters/environments inside a shared bucket. |

### S3-compatible (`storage-type: s3`)

Works with **AWS S3, MinIO, Hetzner Object Storage, Cloudflare R2, Backblaze B2, Wasabi**, and anything else speaking the S3 API.

| Key | Required | Notes |
|---|---|---|
| `bucket` | yes | Must already exist; the operator does not create buckets. |
| `access-key-id` | yes | |
| `secret-access-key` | yes | |
| `region` | no | Defaults to `us-east-1`; non-AWS providers usually ignore this. |
| `endpoint` | no | Required for non-AWS (e.g. `https://s3.eu-central-1.amazonaws.com` for AWS implicit, `https://gateway.eu1.storjshare.io` for Storj, etc.). |
| `path-style` | no | `"true"` for MinIO and others that require path-style addressing. |

### SFTP (`storage-type: sftp` or `hetzner-sftp`)

| Key | Required | Notes |
|---|---|---|
| `host` | yes | |
| `port` | no | Defaults to 22; Hetzner Storage Box uses 23. |
| `username` | yes | |
| `ssh-private-key` | yes | PEM-encoded. |
| `known-hosts` | recommended | Output of `ssh-keyscan host`. Use `[host]:port` for non-22 ports. **Without it the worker logs a loud `INSECURE` warning and uses `InsecureIgnoreHostKey`.** |

### Examples

**AWS S3:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aws-prod
  labels:
    backup.mogenius.io/role: destination
    backup.mogenius.io/storage-type: s3
  annotations:
    backup.mogenius.io/name: "aws-prod"
    backup.mogenius.io/path-prefix: "cluster-prod"
type: Opaque
stringData:
  bucket: my-backups
  access-key-id: AKIA...
  secret-access-key: ...
  region: eu-central-1
```

**MinIO (in-cluster):**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: minio
  labels:
    backup.mogenius.io/role: destination
    backup.mogenius.io/storage-type: s3
  annotations:
    backup.mogenius.io/name: "minio"
type: Opaque
stringData:
  bucket: backups
  access-key-id: minioadmin
  secret-access-key: minioadmin
  endpoint: http://minio.backup.svc.cluster.local:9000
  path-style: "true"
```

**Hetzner Storage Box (SFTP, port 23):**
```yaml
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
type: Opaque
stringData:
  host: u123456.your-storagebox.de
  port: "23"
  username: u123456
  ssh-private-key: |-
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
    -----END OPENSSH PRIVATE KEY-----
  known-hosts: |-
    [u123456.your-storagebox.de]:23 ssh-ed25519 AAAA...
```

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
- **Sources (`#/sources`):** card grid of all backup sources. Create, edit, and delete database backup sources via forms. Supports PostgreSQL, MySQL, and MongoDB with all configuration options.
- **Destinations (`#/destinations`):** manage storage destinations (SFTP, S3). Create, edit, and delete with full field support. Sensitive fields (passwords, SSH keys) are masked in API responses.
- **Jobs (`#/jobs`):** running and recent backup jobs with status and timing.
- **Target detail (`#/target/<name>`):** full run history table — timestamps, sizes, SHA256 checksums, schema status, anomaly counts, and download buttons per run.
- **Settings (`#/settings`):** configuration wizard (see [Settings Wizard](#settings-wizard) below).
- **Live updates:** Server-Sent Events (SSE) push changes to all connected browsers in real time — no polling, no page refresh.
- **Downloads:** `.age` (encrypted dump, pass-through) and `.json` (analyzer metadata).

### REST API

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/targets` | List all backup sources with latest run status |
| `GET` | `/api/targets/{name}/runs` | Run history for one target |
| `GET` | `/api/sources/{name}` | Get source configuration |
| `POST` | `/api/sources` | Create a new source Secret |
| `PUT` | `/api/sources/{name}` | Update source configuration |
| `DELETE` | `/api/sources/{name}` | Delete source (verifies role label) |
| `GET` | `/api/destinations` | List all destinations |
| `POST` | `/api/destinations` | Create a new destination Secret |
| `GET` | `/api/destinations/{name}` | Get destination configuration |
| `PUT` | `/api/destinations/{name}` | Update destination |
| `DELETE` | `/api/destinations/{name}` | Delete destination (verifies role label) |
| `POST` | `/api/trigger/{target}` | Trigger a manual backup run |
| `GET` | `/api/jobs` | List running/recent jobs |
| `GET` | `/api/settings` | Get current operator settings |
| `PUT` | `/api/settings` | Update operator settings |
| `GET` | `/api/settings/export` | Download settings as `values.yaml` |
| `GET` | `/api/events` | SSE stream for live updates |

### Security model

- **Role-verified CRUD.** All Secret operations (GET, UPDATE, DELETE) verify the target Secret carries the expected `backup.mogenius.io/role` label before proceeding. Non-backup Secrets cannot be accessed or deleted through the API.
- **No built-in auth.** Cluster-internal use is the assumed default. To expose externally, put `oauth2-proxy`, an Ingress with basic-auth annotation, or your platform's SSO in front of the Service (see CLAUDE.md §3.1 for examples).
- **Sensitive data masked.** Passwords, SSH keys, and access keys are returned as `***` in API responses. They are only written, never read back.
- **Pass-through downloads.** The operator streams encrypted bytes from the destination to the client without decrypting. The age private key never enters the cluster.

---

## Settings Wizard

The Settings Wizard (`#/settings`) provides a guided 4-step form to configure the operator at runtime — no `helm upgrade` needed.

### Steps

| Step | What you configure |
|---|---|
| 1. Schedule & Timeout | Default cron schedule, run timeout |
| 2. Retention Policy | Retention days, minimum keep, temp directory, temp dir size |
| 3. Worker Resources | CPU/Memory limits and requests for backup worker pods |
| 4. Review & Apply | Summary of all settings, save button |

### How it works

Settings are stored in a Kubernetes ConfigMap (`{release}-settings`), created automatically when `ui.enabled=true`. The wizard reads and writes this ConfigMap via the API.

```
Helm values.yaml → ConfigMap (install-time defaults)
                         ↕
                    UI Settings Wizard (runtime overrides)
                         ↓
                    Export values.yaml → Git → helm upgrade (GitOps)
```

### Export for GitOps

Click **"Export values.yaml"** to download the current settings as a Helm-compatible values file. Commit it to your repo and apply with:

```bash
helm upgrade backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup -f values.yaml
```

This gives you the best of both worlds: interactive UI for quick tuning and declarative GitOps for controlled rollouts.

---

## Restore

The `backup-restore` CLI runs **on your machine**, not in the cluster. It reads the destination Secret via your kubeconfig, downloads the chosen artifact, and decrypts with the offline private key.

### Build the binary

```bash
just build-restore
# produces dist/native/backup-restore
```

### List available dumps

```bash
backup-restore --storage-secret aws-prod -n backup --target prod-users --list

# 20260428T020005Z  prod-users/2026/04/28/dump-20260428T020005Z.sql.gz.age
# 20260427T020003Z  prod-users/2026/04/27/dump-20260427T020003Z.sql.gz.age
# ...
```

### Restore the latest

```bash
# To stdout, decompressed, ready to pipe into psql/mysql/mongorestore:
backup-restore --storage-secret aws-prod -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost new_users_db
```

### Restore a specific timestamp

```bash
backup-restore --storage-secret aws-prod -n backup --target prod-users \
  --age-key ~/age.key --timestamp 20260428T020005Z -o dump.sql.gz
gunzip dump.sql.gz   # or pipe `--decompress` directly
```

### Flags

| Flag | Required | Default |
|---|---|---|
| `--storage-secret` | yes | — |
| `--target` | yes | — |
| `--namespace` (`-n`) | no | `default` |
| `--age-key` | yes (for download) | — |
| `--timestamp` | no | latest |
| `--list` | no | false |
| `--decompress` | no | false |
| `-o` | no | `-` (stdout) |

---

## Alerting & Monitoring

### Metrics

The operator pod exposes Prometheus metrics on `:8080/metrics`. Worker pods expose the same registry briefly during their ~20s lifetime — for run-level metrics, scrape via the included `ServiceMonitor` or rely on `kube-state-metrics` for Job status.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backup_operator_dump_duration_seconds` | Histogram | `target`, `db_type` | Dump time (pre-encrypt, pre-upload) |
| `backup_operator_upload_duration_seconds` | Histogram | `target`, `destination`, `storage_type` | Time per destination upload |
| `backup_operator_dump_size_bytes` | Gauge | `target` | Encrypted dump size of last run |
| `backup_operator_dump_size_change_ratio` | Gauge | `target` | current/previous encrypted size; `<0.5` = suspicious shrinkage |
| `backup_operator_table_count` | Gauge | `target` | Tables/collections in current dump |
| `backup_operator_table_row_count` | Gauge | `target`, `table` | Per-table row count (estimate) |
| `backup_operator_schema_changed` | Gauge | `target` | 1 if schema hash differs from previous |
| `backup_operator_anomalies_total` | Counter | `target`, `kind` | `size-collapse`, `table-disappeared`, `row-count-drop` |
| `backup_operator_runs_total` | Counter | `target`, `status` | `success` / `failure` |
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts of last successful upload |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if last upload to that destination failed |
| `backup_operator_retention_deleted_total` | Counter | `target`, `destination`, `kind` | dump / meta / other |
| `backup_operator_retention_failed_total` | Counter | `target`, `destination` | Retention errors (non-fatal) |

### ServiceMonitor

Enabled by default when the Prometheus Operator CRDs are installed. The chart's templates render the ServiceMonitor only if `monitoring.coreos.com/v1` is present.

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 30s
  scrapeTimeout: 10s
```

If you don't run Prometheus Operator, set `serviceMonitor.enabled=false` and configure your scrape job manually pointing at `:8080/metrics`.

### Default Alert Rules

The chart ships a `PrometheusRule` with six default alerts. They're rendered only when `monitoring.coreos.com/v1` is present, and they live under `prometheusRule.rules` in `values.yaml` so you can override or add your own at install time.

| Alert | Expression | For | Severity |
|---|---|---|---|
| `BackupOverdue` | `time() - max by (target) (last_success_timestamp_seconds) > 86400 * 1.5` | 10m | warning |
| `BackupDestinationFailing` | `max by (target, destination) (destination_failed) == 1` | 15m | warning |
| `BackupDumpSizeCollapsed` | `dump_size_change_ratio < 0.5` | 5m | **critical** |
| `BackupSchemaChanged` | `schema_changed == 1` | — | info |
| `BackupAnomaliesAppearing` | `increase(anomalies_total[1h]) > 0` | — | warning |
| `BackupRetentionFailing` | `increase(retention_failed_total[1h]) > 0` | 30m | warning |

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
   └── creates Secret backup-operator-age with key AGE_PUBLIC_KEYS
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
- **Rotate by adding, then retiring.** Add a new recipient to `agePublicKeys`, run a few backups (so they're encrypted to both keys), then remove the old recipient. New recipients can decrypt going forward; old recipients still work for older artifacts.
- **For multi-region recovery, distribute multiple public keys to the cluster** and keep each region's private key in that region's safe.

---

## CI/CD

Two GitHub Actions workflows automate testing and releasing. See `.github/workflows/` for the full YAML.

### CI (`ci.yaml`)

Runs on every push and pull request to `main`:

| Job | Steps |
|---|---|
| `test` | `go build ./...` → `go test ./...` → `go vet ./...` |
| `helm-lint` | `helm lint charts/backup-operator --set agePublicKeys="age1test"` |

### Release (`release.yaml`)

Triggered by pushing a semver tag (`v*`):

1. Run `go test` to gate the release.
2. Build a multi-arch Docker image (`linux/amd64` + `linux/arm64`).
3. Push the image to `ghcr.io/behrangalavi/backup-operator:<version>` and `:latest`.
4. Package the Helm chart with the matching version.
5. Push the chart to `oci://ghcr.io/behrangalavi/charts/backup-operator`.

### Creating a release

```bash
git tag v0.2.0
git push origin v0.2.0
# GitHub Actions builds + publishes automatically
```

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

# Generate an age key pair offline + apply the public key as a Secret.
just gen-age-key

# Apply the test stack: namespace, worker SA/RBAC, in-cluster Postgres
# (with a seed table), in-cluster MinIO (with bucket init), source +
# destination Secrets.
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

The destination Secret is missing `known-hosts`. Add the output of `ssh-keyscan -p <port> <host>` to the Secret's `data.known-hosts` field. Without it the worker accepts any host key — a downgrade in security but not a backup failure.

### `analyzer-enabled: false` not detected

Annotation typo (e.g. `analyzer-enable`). The parser is forgiving and falls back to the default rather than rejecting the Secret, so look closely at the annotation name. Use `kubectl get secret -o yaml` to inspect.

---

## More Documentation

- **`CLAUDE.md`** — full architectural reference: module layout, factories, retention semantics, failure modes, the rationale behind every non-obvious design decision.
- **Helm chart** — `charts/backup-operator/values.yaml` is the canonical list of every knob the chart exposes.
- **Source code** — well-commented Go. Start with `cmd/main.go` (operator), `cmd/worker/main.go` (worker), `internal/backup/pipeline.go` (the actual backup pipeline).

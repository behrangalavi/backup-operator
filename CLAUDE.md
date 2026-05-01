# Backup Operator — Claude Code Guide

A Kubernetes-native backup operator in Go for **PostgreSQL**, **MySQL**, and **MongoDB**, with public-key encryption (`age`), multi-destination fan-out (SFTP + S3-compatible), semantic dump analysis, and Prometheus-driven alerting. Built on the **Operator-Reconciles-CronJobs** pattern: the operator does not run backups itself; Kubernetes does.

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

The operator UI only exposes non-sensitive metadata (target names, timestamps, dump sizes). It **never** shows database credentials or decrypted backup content. Still, authentication prevents unauthorized users from browsing backup history or downloading encrypted dumps.

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
│   └── cronjob_controller.go  # Source Secret → batch/v1.CronJob
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
└── storage/             # Upload destination abstraction
    ├── factory/         # Creates the right Storage from storage-type label
    ├── sftp/            # Hetzner Storage Box and generic SFTP
    └── s3/              # AWS S3, MinIO, Hetzner Object Storage, R2, B2, ...
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
- shell out to `pg_dump`/`mysqldump`/`mongodump`
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
4. **Fan-out:** `sync.WaitGroup` over destinations, each goroutine opens the temp file independently and uploads. Per-destination errors are logged + metrified, never aborting peers.
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
| `backup.mogenius.io/db-type` | yes (sources only) | `postgres` \| `mysql` \| `mongo` |
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
| `backup.mogenius.io/extra-<key>` | none | Surfaced into `dumper.Config.Extra[key]`. Used for DB-specific options (e.g. `extra-sslmode`, `extra-authSource`). |

A typo on a feature-flag annotation falls back to the default rather than rejecting the Secret — backups must keep running even if a flag is misspelled.

### 6.3 Source Secret `data` keys

| Key | Required | Notes |
|---|---|---|
| `host` | **yes** | DB hostname |
| `port` | no | Defaults: 5432 (pg), 3306 (mysql), 27017 (mongo) |
| `database` | (situational) | Postgres/MySQL: required for `pg_dump`/`mysqldump` to scope. Mongo: optional, omitted = all non-system DBs. |
| `username` | **yes** | |
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
2. Register the type-constant in `src/dumper/factory/factory.go`:
   ```go
   const TypeMariaDB = "mariadb"
   case TypeMariaDB: return mariadb.New(cfg, logger.WithName("mariadb")), nil
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
| `dump-<ts>.meta.json` | target name, db-type, encrypted size, full Stats, full analyzer Report | Lets the next run compute diffs without restoring; lets humans audit without the private key |

**Timestamp format:** `20060102T150405Z` (Go reference time, ISO-like, lexically sortable).

**The meta file is intentionally unencrypted.** Anyone with read access to the bucket can see schema fingerprints and row counts, but never the data itself. If that's not acceptable for your environment, plan to encrypt the meta files in a follow-up — the trade-off is that automated diffing then needs the private key in the cluster.

---

## 12. Metrics Catalog

Exposed by the operator pod on `:8080/metrics`. **Worker pods are short-lived** — Prometheus cannot scrape them in time, so the run-level metrics are reconstructed by the operator's `MetricsRefresher` (`controllers/metrics_refresher.go`). It runs on a tick (default 30s, see `METRICS_REFRESH_INTERVAL_SECONDS`), lists Source Secrets in the watch namespace, fetches the most recent `*.meta.json` from each allowed destination, and writes the result into the operator's local Prometheus registry. That is why everything below is a Gauge — counters would require an always-on producer the worker cannot provide.

The histograms (`dump_duration_seconds`, `upload_duration_seconds`) are kept in the worker for code-coupling reasons but their samples never reach Prometheus. Treat them as a known gap; rely on Job duration via kube-state-metrics if you need timing alerts today.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backup_operator_dump_duration_seconds` | Histogram | `target`, `db_type` | Worker-only; not visible to Prometheus (see note above) |
| `backup_operator_upload_duration_seconds` | Histogram | `target`, `destination`, `storage_type` | Worker-only; not visible to Prometheus (see note above) |
| `backup_operator_dump_size_bytes` | Gauge | `target` | Encrypted size of the most recent meta.json's dump |
| `backup_operator_dump_size_change_ratio` | Gauge | `target` | current / previous size from the latest meta.json's report; <0.5 = suspicious shrinkage |
| `backup_operator_table_count` | Gauge | `target` | Tables/collections in the most recent run's stats |
| `backup_operator_table_row_count` | Gauge | `target`, `table` | Per-table row count (estimate) from the most recent run |
| `backup_operator_schema_changed` | Gauge | `target` | 1 if schema hash differs from the previous run |
| `backup_operator_last_run_anomalies` | Gauge | `target` | Analyzer anomaly count in the most recent run |
| `backup_operator_last_run_status` | Gauge | `target` | 1 = most recent run produced a meta.json, 0 otherwise |
| `backup_operator_last_success_timestamp_seconds` | Gauge | `target`, `destination` | Unix ts parsed from the most recent meta.json found at that destination |
| `backup_operator_destination_failed` | Gauge | `target`, `destination` | 1 if the destination's storage cannot be initialised, 0 once a meta.json was successfully read |
| `backup_operator_retention_deleted_total` | Counter | `target`, `destination`, `kind` | Worker-only; not visible to Prometheus |
| `backup_operator_retention_failed_total` | Counter | `target`, `destination` | Worker-only; not visible to Prometheus |

---

## 13. Default Alert Rules

Shipped in the Helm chart's `values.yaml` under `prometheusRule.rules`. The chart only renders the `PrometheusRule` if `monitoring.coreos.com/v1` is present (i.e. Prometheus Operator installed). Override at install time to fit your environment.

| Alert | Expr (simplified) | Severity |
|---|---|---|
| `BackupOverdue` | last success >36h | warning |
| `BackupDestinationFailing` | `destination_failed == 1` for 15m | warning |
| `BackupDumpSizeCollapsed` | `dump_size_change_ratio < 0.5` for 5m | **critical** |
| `BackupSchemaChanged` | `schema_changed == 1` | info |
| `BackupAnomaliesAppearing` | `last_run_anomalies > 0` for 5m | warning |
| `BackupLastRunFailed` | `last_run_status == 0` for 5m | warning |
| `BackupSucceeded` | `time() - last_success_timestamp_seconds < 120` | info |

`BackupSucceeded` is a heartbeat-style positive signal (firing + resolved per run) — useful when you want a notification on every successful backup, but expect one firing + one resolved mail per completed run per target. With a frequent cron (e.g. every 5 min in the test stack), this is intentionally noisy.

The semantic alerts (`DumpSizeCollapsed`, `SchemaChanged`, `AnomaliesAppearing`) are the project's main differentiator vs K8up — they alert on *content*, not just job exit code.

---

## 14. Failure Modes

| Failure | What happens | What the operator sees |
|---|---|---|
| DB unreachable | `pg_dump` exits non-zero, pipeline returns error, worker exits 1 | `kubectl get jobs` shows failed; no fresh meta.json arrives, so `last_success_timestamp_seconds` stops advancing → eventually `BackupOverdue` |
| One destination down (e.g. SFTP host offline) | Other destinations upload normally, run succeeds | Refresher cannot read latest meta from that destination → `destination_failed{destination=sftp} = 1`, `BackupDestinationFailing` fires |
| All destinations down | Worker exits 1 (no successful upload) | Job failed; no destination has a fresh meta → `BackupOverdue` after 36h |
| `CollectStats` fails (no perms) | Analyzer skips, dump still succeeds | meta.json still written without stats; `schema_changed` / `table_count` stay at their last values |
| Dump shrinks 90% | Run succeeds (dump is what it is) | `BackupDumpSizeCollapsed` fires within 5 min |
| Schema changed | Run succeeds | `BackupSchemaChanged` fires |
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
- **Right to erasure (DSGVO Art. 17)**: If a dump contains personal data subject to deletion, the encrypted dump must be deleted from all destinations. Without the private key, the dump is cryptographically inaccessible, but storage-level deletion may still be required by your DPO.

### 17.6 Access Control Summary

| Principal | Can access | Cannot access |
|---|---|---|
| Operator pod | Source/Dest Secrets (read), CronJobs (CRUD), Leases, Events | Private key, dump contents |
| Worker pod | Source/Dest Secrets (read), Events (create) | CronJobs, Leases, private key |
| Storage backend | Encrypted dumps, unencrypted meta.json | Private key, DB credentials |
| Restore operator (human) | Private key, storage backend | Cluster Secrets (unless they have kubectl access) |
| Prometheus/Alertmanager | Metrics (sizes, counts, anomalies) | Dump contents, credentials |

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

- **Writable `/tmp` via emptyDir, not relaxing `readOnlyRootFilesystem`.** The operator needs a small writable `/tmp` (1Mi) for SFTP known-hosts temp files. The worker's main emptyDir is mounted at `/tmp` (covering both `os.CreateTemp` and the `TEMP_DIR` subdirectory). This preserves PSA-restricted compliance.

- **EventBroadcaster shutdown before exit.** The worker defers `eventBroadcaster.Shutdown()` to flush buffered events. Without it, final events like `BackupCompleted` could be lost because the broadcaster sends asynchronously.

- **Signal-aware context for graceful shutdown.** The worker's context chains `signal.NotifyContext(SIGTERM, SIGINT)` → `context.WithTimeout`. SIGTERM from Kubernetes pod termination cancels the pipeline context, allowing in-flight operations to abort cleanly while deferred cleanup (event flush, temp file removal) still runs.

- **Upload concurrency semaphore.** Fan-out uses a channel-based semaphore (default 4) to limit concurrent uploads. Without it, N destinations each open the dump file and upload simultaneously, causing file-descriptor and bandwidth pressure on clusters with many destinations.

- **PodDisruptionBudget for the operator.** Only rendered when `replicaCount > 1`. Prevents voluntary evictions from killing the last operator pod during node drains or cluster upgrades. `minAvailable: 1` is the right choice for a leader-elected controller.

- **UI error sanitization.** HTTP error responses return generic messages ("internal error", "target not found") instead of raw `err.Error()`. Internal details are logged server-side. This prevents leaking implementation details (file paths, storage errors, internal state) to unauthorized clients, especially when the UI is exposed without an auth proxy.

---

## 19. Important

- Every change to the directory structure should be reflected in section 4.
- New annotations or labels: update sections 6.1–6.5.
- New env vars: update section 7.
- New metrics: update section 12. New default alerts: update section 13.
- New failure modes worth flagging: update section 14.
- Data-flow or access-control changes: update section 17.
- Architectural decisions that change the behaviour of existing systems: update section 18 with the *reason*, not just the change.

Documentation that drifts from code is worse than no documentation. Bring this file with you when you change behaviour.

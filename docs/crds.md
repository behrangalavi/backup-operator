# Custom Resources (Discovery Contract)

## 6. The Discovery Contract

Discovery runs through three CustomResourceDefinitions in the API group
`backup.mogenius.io/v1alpha1`, all namespaced. Everything non-secret is a
typed field on the CR; every secret (DB password, SSH key, object-store
keys) lives in a plain Secret the CR references by name. Validation (enums,
ranges, name patterns, CEL rules) is enforced by the API server at
admission, so a typo is rejected at `kubectl apply` instead of silently
falling back to a default. The Go types are in `src/api/v1alpha1`; the
generated schema in `charts/backup-operator/files/crds`.

| Kind | Short name | Replaces | Status |
|---|---|---|---|
| `BackupSource` | `bsrc` | Secret with `role=source` | `Ready`, `LastRunSucceeded`, `Verified` conditions; run-level fields written by the operator |
| `BackupDestination` | `bdst` | Secret with `role=destination` | `Reachable` condition from the metrics refresher |
| `BackupRecipient` | `brcp` | Secret with `role=age-recipient` | none |

`kubectl get backupsources` shows type, materialised schedule, suspended, last run status, last success, verifier verdict and Ready at a glance. The legacy labelled-Secret model is converted with `backup-restore migrate` ([§5.3](architecture.md#53-restore-cmdrestoremaingo--backup-restore)).

### 6.1 `BackupSource.spec`

| Field | Default | Effect |
|---|---|---|
| `targetName` | `metadata.name` | Logical target name used in storage object paths, metric labels and the CronJob name. Validated against `^[A-Za-z0-9][A-Za-z0-9._-]*$` (no `/`, no leading `.`, ≤253 chars) both in the CRD schema and in the flattener, so `../other-tenant` can never escape a destination's prefix. Only set it on sources migrated from the legacy model whose logical name differed from the Secret name — renaming a live target orphans its run history. |
| `database.type` | **required** | `postgres` \| `mysql` \| `mariadb` \| `mongo` \| `redis` (enum). |
| `database.host` | **required** | DB hostname. |
| `database.port` | per type | 5432 / 3306 / 27017 / 6379. |
| `database.name` | — | Database to dump. Required for postgres/mysql/mariadb; optional for mongo (all non-system DBs) and redis (DB index or comma-separated list — narrows *stats* only, the RDB is always full-instance). |
| `database.username` | — | Required for every engine except redis (pre-6 password-only AUTH). Ignored when `credentialsSecretRef.usernameKey` is set. |
| `database.credentialsSecretRef.name` | **required** | Secret in the same namespace holding the password. |
| `database.credentialsSecretRef.passwordKey` | `password` | Data key of the password. Lets a source point at the Secret the database chart itself produced (CloudNativePG, Bitnami) instead of copying the password. |
| `database.credentialsSecretRef.usernameKey` | — | When set, the username is read from the Secret too. |
| `database.tls.mode` | — | Engine-native TLS knob: postgres libpq `sslmode` values (`disable`, `prefer`, `require`, `verify-ca`, `verify-full`); mysql/mariadb and redis `true`/`false` plus `verify-ca`/`verify-identity` for mysql. Applies to BOTH the stats connection and the dump tool ([§17.2](compliance.md#172-data-in-transit)). |
| `database.tls.caSecretRef` | — | `{name, key}` of a Secret holding a CA bundle. The worker writes it to `TEMP_DIR` and hands the path to the dumper (`sslrootcert` / `ssl-ca`), which is what makes `verify-ca`/`verify-full` work. mongo/redis have no CA knob in the dumper; the ref is accepted but ignored there. |
| `database.extra` | — | Free-form engine options (`max-allowed-packet` for MySQL/MariaDB, `authSource`/`replicaSet` for Mongo). Replaces the legacy `extra-<key>` annotations. |
| `schedule` | `DEFAULT_SCHEDULE` (`0 2 * * *`) | 5-field cron expression (or `@daily`-style descriptor) for the managed CronJob. |
| `jitterMinutes` | unset | Spread the cron's minute field across an N-minute window via a deterministic per-source hash (hash input: `metadata.name`). Default behaviour (unset): jitter only when the user wrote minute==`0`; non-zero literal minutes are respected. Setting it explicitly opts in on a non-zero literal minute; `0` pins the schedule. Multi-fire schedules (`*/15`, `0,30`, ranges, `*`) are always left alone. 0–60. |
| `suspended` | `false` | `true` → `Spec.Suspend=true` on the managed CronJob. Existing artifacts and config are kept; manual triggers ignore Suspend. The CR is the source of truth — a manual `kubectl patch cronjob ... suspend=true` is overridden on the next reconcile. |
| `destinations` | all | Allow-list of `BackupDestination` names (set semantics). Empty = fan out to every destination in the namespace. |
| `retention.days` | `DEFAULT_RETENTION_DAYS` (30) | Delete dumps older than N days. `0` = keep forever. |
| `retention.minKeep` | `DEFAULT_MIN_KEEP` (3) | Safety floor — never delete below this many newest dumps. |
| `compression` | `gzip` | `gzip` or `zstd` (30–50 % better compression at comparable CPU). Stored in `meta.json` so restore and verifiers auto-detect the decompressor. |
| `analyzer.enabled` | `true` | `false` → skip `CollectStats` and the run-to-run comparison. |
| `analyzer.rowDropThresholdPercent` | `50` | Flag a table whose row count fell below this percentage of the previous run (1–100). Replaces the legacy `0.5` ratio. |
| `analyzer.sizeDropThresholdPercent` | `50` | Same for the encrypted dump size. |
| `analyzer.anonymizeTables` | `false` | Hash table names in `meta.json` with SHA256 (16 hex chars). Row counts preserved. |
| `analyzer.emptyDumpCheck` | `true` | Hard-fail when the dump appears empty although the source has data ([§14](failure-modes.md#14-failure-modes)). Disable for intentionally schema-only sources. |
| `restoreVerification.mode` | `off` | `off`, `stream-validate` (Phase 1, no DB-pod-spawn), `schema-only` / `sample` / `full` (Phase 2: spawn an ephemeral DB pod + restore; needs `restoreVerification.enableEphemeralPodSpawn` in the chart). Enum — a typo is rejected at admission, it cannot silently enable a heavyweight mode. |
| `restoreVerification.interval` | `168h` | Minimum gap between verifier runs (Go duration). State-driven: the worker reads `latestMeta.restoreVerification.completedAt`. The very first verification runs as soon as a baseline meta exists. A verifier that exceeds its time budget re-stamps `completedAt` (carrying the previous verdict forward, or `skipped` + explanatory error on the very first attempt), so the interval clock advances and a chronically slow Phase-2 verification does not re-spawn a DB pod on every run. |
| `restoreVerification.image` | per type | Verifier DB pod image (Phase 2 only). Defaults in `verifier/restore/engine.go`; the image's DB-user UID must match what the spawner runs the pod as (`runAsUIDForImage`). |
| `restoreVerification.volumeSize` | per mode | `emptyDir.sizeLimit` of the verifier pod (`1Gi` / `5Gi` / `50Gi`). Kubernetes quantity. |

### 6.2 `BackupSource.status`

Written by the operator only; the worker never touches it (worker RBAC stays minimal — everything here is reconstructed from the CronJob and the `meta.json` sidecars, exactly like the Prometheus gauges).

| Field | Writer | Meaning |
|---|---|---|
| `conditions[Ready]` | CronJob reconciler | `True` once the managed CronJob matches the spec; `False/InvalidSpec` when the spec cannot be flattened (unsafe `targetName`, missing host, …). |
| `conditions[LastRunSucceeded]` | metrics refresher | Outcome of the most recent run found in storage; message carries the sanitized error. |
| `conditions[Verified]` | metrics refresher | Most recent restore-verification verdict (`match` → True). Absent while verification is off. |
| `cronJobName`, `materializedSchedule` | CronJob reconciler | The managed CronJob and its post-jitter schedule. |
| `nextScheduleTime` | both | Next fire time of the materialised schedule; nil while suspended. |
| `lastRunTime`, `lastRunStatus`, `lastRunError` | metrics refresher | Most recent run (any outcome). |
| `lastSuccessTime`, `lastDumpSizeBytes`, `schemaChangedAt` | metrics refresher | Most recent successful run. |
| `lastRunAnomalies` | metrics refresher | Analyzer anomaly count of the most recent run. |
| `destinations[]` | metrics refresher | Per-destination upload outcome of the most recent run plus that destination's last success time. |
| `restoreVerification` | metrics refresher | `{mode, verdict, completedAt, error}` of the latest verifier run. |

Status writes are change-only (`updateSourceStatus` compares before writing), so a 30 s refresher tick on 10 000 sources produces writes only for sources whose run data actually moved.

### 6.3 Source credentials Secret

| Key | Required | Notes |
|---|---|---|
| `password` (or `credentialsSecretRef.passwordKey`) | **yes** (redis: optional) | The only key the worker reads unless `usernameKey` is set. |
| `credentialsSecretRef.usernameKey` | no | Optional username key for Secrets produced by a database chart. |

Secrets created by the UI are named `<source>-credentials`, carry an `OwnerReference` to the `BackupSource` (deleting the source cascades), and are the only Secrets the UI will edit in place. A user-managed Secret (no OwnerReference) is never overwritten from the dashboard — the UI reports a conflict and tells you to edit it directly.

### 6.4 `BackupDestination.spec`

Exactly one backend block must be set and must match `type` (CEL rule); the credentials Secret carries the keys that backend needs.

| Field | Effect |
|---|---|
| `type` | `sftp` \| `ftps` \| `s3` \| `azure` \| `gcs`. The legacy `hetzner-sftp` alias is gone — use `sftp` with `port: 23`. |
| `pathPrefix` | Prefix prepended to every uploaded object path. May contain `/` (it is the documented tenant/cluster isolation root); a `..` segment is rejected by a CEL rule and again by the flattener. |
| `credentialsSecretRef.name` | Secret holding the backend credentials (6.5). |
| `sftp.{host, port, username, knownHosts, insecureSkipHostVerify}` | Generic SFTP and Hetzner Storage Boxes (port 23). Without `knownHosts` the connection is **rejected** unless `insecureSkipHostVerify: true` (logs an `INSECURE` warning). |
| `ftps.{host, port, username, tlsMode, insecureSkipCertVerify}` | FTP over TLS for NAS firmware without SFTP. `tlsMode` `explicit` (port 21 + AUTH TLS, default) or `implicit` (port 990). Data channel is always PROT P. Prefer SFTP when both are available. |
| `s3.{bucket, endpoint, region, pathStyle}` | AWS S3 and every S3-compatible store. `endpoint` is required for non-AWS (MinIO, Hetzner Object Storage, R2, B2, Wasabi); `pathStyle: true` for MinIO-style addressing. |
| `azure.{accountName, container}` | Azure Blob Storage with shared-key auth. |
| `gcs.bucket` | Google Cloud Storage. |

`metadata.name` is the logical destination name used in `BackupSource.spec.destinations` allow-lists, metric labels and the storage pool key. Names are unique per namespace by construction, so the legacy "duplicate logical name" de-duplication no longer exists.

`status.conditions[Reachable]` is `True` while the metrics refresher can list the destination for at least one source, `False` (with the scrubbed error in `status.lastError`) on the first tick that cannot, and `Unknown/NoSources` while no source fans out to it.

### 6.5 Destination credentials Secret

The flattener (`secrets.FromBackupDestination`) merges exactly these keys into the `Data` map the storage factory consumes; any other key in the Secret is ignored, so a credentials Secret can never override a connection parameter the CR declares.

| `type` | Keys |
|---|---|
| `sftp` | `ssh-private-key` (PEM) **or** `password` — exactly one required; public-key auth is preferred when both are present. |
| `ftps` | `password` — FTP has no key-based auth. |
| `s3` | `access-key-id`, `secret-access-key`. |
| `azure` | `account-key` (base64 account access key). |
| `gcs` | `service-account-json` (full service-account JSON key; needs object read/write on the bucket). |

`secrets.CredentialKeys(type)` is the single table behind this: the UI masks exactly these keys as `***` on GET, and `backup-restore migrate` moves exactly these out of a legacy Secret. Adding a backend means extending that one function.

### 6.6 `BackupRecipient`

Each age public key the worker should encrypt to is one `BackupRecipient` with `spec.publicKey: age1...` (pattern-validated X25519 recipient, one key per object). The operator's `RecipientReconciler` merges every recipient in the namespace into a single Secret named by `AGE_SECRET_NAME` (default `<release>-age`, data key `AGE_PUBLIC_KEYS`, newline-joined, sorted, de-duplicated) which worker pods mount via `secretKeyRef` — the worker contract never changed.

How they're created:

- **Helm install bootstrap:** the chart passes `agePublicKeys` to the operator as `BOOTSTRAP_AGE_RECIPIENTS`; at startup the operator creates one `BackupRecipient` per key, named `backup-recipient-<sha256-prefix>`. Idempotent; keys removed from values are NOT deleted (the object outlives the value — remove it with kubectl or the UI).
- **UI Age-Keys page:** create-on-add, delete-on-remove, same deterministic naming so re-adding a key is a no-op.
- **GitOps:** apply your own `BackupRecipient` manifests.

Removing the last recipient is refused by the UI — zero recipients makes the worker unable to encrypt. `kubectl delete` bypasses that guard at your own risk. An out-of-band edit or delete of the merged Secret is repaired within `mergedResyncInterval` (10 min) — the reconciler deliberately does not watch Secrets.

---

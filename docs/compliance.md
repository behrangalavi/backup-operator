# Data Flow & Compliance

## 17. Data Flow & Compliance

This section documents the complete data lifecycle for compliance audits (DSGVO/GDPR Art. 30, SOC2).

### 17.1 Data at Rest

| Location | Contents | Encryption | Retention |
|---|---|---|---|
| `BackupSource` / `BackupDestination` (K8s) | Connection parameters and policy — no secrets | etcd (plain) | Cluster lifecycle |
| Credentials Secrets (K8s) | DB password; storage credentials (SSH keys, S3 keys) | Kubernetes Secret encryption (etcd) | Cluster lifecycle; UI-created ones cascade with their CR |
| Worker temp volume (`/tmp`) | Encrypted dump file (`.sql.gz.age`) | `age` public-key (X25519) | Pod lifecycle (emptyDir) |
| Storage backend (SFTP/S3) | Encrypted dump + unencrypted `meta.json` | `age` public-key (X25519) | `spec.retention.days` (floor `spec.retention.minKeep`) |
| Operator machine (offline) | `age` private key | Operator responsibility | Manual |

### 17.2 Data in Transit

| Path | Protocol | Encryption |
|---|---|---|
| Worker → Database | DB-native (pg, mysql, mongo, redis) | TLS if configured via `BackupSource.spec.database.tls.mode` (postgres libpq `sslmode` values; mysql/mariadb `true`/`false`/`verify-ca`/`verify-identity`; redis `true` adds `redis-cli --tls`). For pg/mysql/mariadb it applies to BOTH the stats connection AND the dump tool (`pg_dump` via `PGSSLMODE`, `mysqldump`/`mariadb-dump` via `--ssl-mode`/`--ssl`); the mysql stats connection maps `verify-*` modes to a valid go-sql-driver value (identity modes → system-root verification, other require-TLS values → encrypted-without-identity) while the dump path still verifies against the CA. Certificate verification (`verify-ca`/`verify-full`) additionally needs `spec.database.tls.caSecretRef`, which the worker materialises to `TEMP_DIR` and hands to the dumper as `sslrootcert`/`ssl-ca`; without one, an enabled TLS value still enforces an encrypted channel. |
| Worker → SFTP destination | SSH (SFTP subsystem) | SSH transport encryption |
| Worker → S3 destination | HTTPS | TLS 1.2+ |
| Operator → Storage (metrics refresh) | SSH/HTTPS (same as worker) | Same as worker |
| Operator → Prometheus (alerts status) | HTTP | Plain HTTP (cluster-internal) |
| Operator → Alertmanager (status check, test alerts) | HTTP | Plain HTTP (cluster-internal) |
| Operator → K8s API | HTTPS (in-cluster ServiceAccount) | mTLS |

### 17.3 Key Management

- **Public keys** (`age` recipients): one `BackupRecipient` object per recipient (`spec.publicKey`). The operator's `RecipientReconciler` materialises a merged Secret (`<release>-age`) that worker pods mount via `secretKeyRef`. Used only for encryption. Per-object form means lifecycle is decoupled from Helm releases (UI-added or GitOps-applied recipients survive `helm uninstall`; the CRDs carry `helm.sh/resource-policy: keep`).
- **Private key** (`age` identity): **never enters the cluster**. Lives on the operator's machine. Required only for `backup-restore` CLI.
- **SSH keys** (SFTP destinations) and **S3 credentials**: stored in the credentials Secret each `BackupDestination` references. Scoped to individual storage backends; S3 should use scoped IAM roles with minimal write permissions.
- **DB passwords**: stored in the credentials Secret each `BackupSource` references — optionally the very Secret the database chart produced (`passwordKey` / `usernameKey`), so the password is never copied.

### 17.4 Audit Trail

| Event | Source | Visible via |
|---|---|---|
| `BackupStarted` | Worker pod | `kubectl describe backupsource <name>`, cluster audit log |
| `BackupCompleted` | Worker pod | Same |
| `BackupFailed` | Worker pod | Same, includes failure phase |
| `RetentionDelete` | Worker pod | Same, lists deleted artifact + phase (`pre-upload`/`post-upload`) |
| `RetentionFailed` | Worker pod | Same; Warning. Emitted when a retention step (`init storage`, `open session`, `list`) fails for one destination. Carries the phase so post-upload failures (which can't reach `meta.json`) are still auditable. |
| `BackupMetaPartial` | Worker pod | Same; Warning. The `meta.json` sidecar reached some but not all destinations whose dump succeeded — run is still discoverable, but per-destination history diverges. |
| `BackupEmptyCheckDegraded` | Worker pod | Same; Warning. `empty-dump-check` is on but no pre-dump stats baseline was available, so the empty-dump heuristic could not run for this run. |
| `UploadVerifySkipped` | Worker pod | Same; Warning. The post-upload size verification could not run for a destination (listing failed / object absent from the listing). |
| `SourceCreated` / `DestinationUpdated` / `AgeKeyAdded` … | Operator UI | `kubectl describe backupsource|backupdestination|backuprecipient`. Message carries the auth-proxy identity and request id when available (`… (by alice@example.com, request 7f3a…)`). |
| CronJob/Job status | Kubernetes | `kubectl get jobs`, kube-state-metrics |
| Prometheus alerts | Alertmanager | Alert history, notification channels |

### 17.5 Data Deletion

- **Automated**: Retention policy deletes dumps older than `retention-days`, respecting `min-keep` safety floor. Deletion events are recorded as Kubernetes Events.
- **Manual**: Delete the `BackupSource` → OwnerReference cascades to the CronJob (and a UI-created credentials Secret) → no new backups. Existing dumps in storage must be deleted manually from the backend.
- **Right to erasure (DSGVO Art. 17)**: Use `backup-restore --purge --target X --before YYYY-MM-DD --destination a,b,c` to delete all artifacts for a target. The purge covers exactly the destinations you pass — list every destination the source fans out to, or the erasure is partial; the CLI exits non-zero with `ERASURE INCOMPLETE` when any delete or listing fails. Add `--dry-run` to preview. Without the private key, the dump is cryptographically inaccessible, but storage-level deletion may still be required by your DPO.

### 17.6 Access Control Summary

| Principal | Can access | Cannot access |
|---|---|---|
| Operator pod | BackupSources/BackupDestinations/BackupRecipients (get/list/watch + status; `backuprecipients: create` always, for the startup bootstrap of chart-provided keys; create/update/delete on all three only with a writable UI), Secrets (get by name, create/update for the merged age Secret and UI-created credentials — never list/watch, read uncached), CronJobs (CRUD), Jobs (get/list/watch/create), Leases (create, plus get/update restricted to the election lease by `resourceNames`), Events, SubjectAccessReviews (create — cluster-scoped, only rendered with the UI), Prometheus query API (read), Alertmanager API (read status + write test alerts) | Private key, dump contents, the namespace's other Secrets (no list) |
| Worker pod | Its BackupSource (get), BackupDestinations (get/list), the referenced credentials Secrets (get by name — no list), Events (create), **Pods (create/delete) when `restoreVerification.enableEphemeralPodSpawn=true`**. Network: with `networkPolicy.enabled` a second NetworkPolicy limits worker egress to DNS, the configured DB ports (default 5432/3306/27017/6379), 443/22/23 and `networkPolicy.worker.extraEgressRules` | CronJobs, Leases, private key, enumeration of Secrets |
| Storage backend | Encrypted dumps, unencrypted meta.json | Private key, DB credentials |
| Restore operator (human) | Private key, storage backend | Cluster Secrets (unless they have kubectl access) |
| Prometheus/Alertmanager | Metrics (sizes, counts, anomalies); operator reads status + posts test alerts | Dump contents, credentials |

---

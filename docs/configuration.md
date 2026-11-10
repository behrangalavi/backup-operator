# Configuration Reference

## 7. Configuration Reference

The operator and the worker have separate (overlapping) config schemas. All values are env vars; the helm chart wires them.

### Operator (`cmd/main.go`)

| Key | Required | Default | Effect |
|---|---|---|---|
| `WATCH_NAMESPACE` | no | release namespace | Namespace the operator watches for `BackupSource` / `BackupDestination` / `BackupRecipient` objects |
| `POD_NAMESPACE` | no | (downward API) | Lease namespace for leader election |
| `LEADER_ELECTION_ID` | no | — | Empty = leader election disabled |
| `DEFAULT_SCHEDULE` | no | `0 2 * * *` | Fallback schedule for sources without `spec.schedule`. Validated at startup with the CronJob parser — an invalid default fails the operator loudly instead of failing every CronJob create. |
| `RUN_TIMEOUT_SECONDS` | no | `3600` | Set as `activeDeadlineSeconds` on every Job |
| `TEMP_DIR` | no | `/tmp/backup-operator` | Mount path inside worker pods |
| `TEMP_DIR_SIZE` | no | `10Gi` | `emptyDir.sizeLimit` on worker pods |
| `DEFAULT_RETENTION_DAYS` | no | `30` | Fallback for sources without `spec.retention.days` |
| `DEFAULT_MIN_KEEP` | no | `3` | Fallback for sources without `spec.retention.minKeep` |
| `WORKER_IMAGE` | **yes** | — | Container image for worker pods (Helm sets to operator's image) |
| `WORKER_IMAGE_PULL_POLICY` | no | `IfNotPresent` | |
| `WORKER_IMAGE_PULL_SECRETS` | no | — | Comma-separated pull-secret names, stamped into every worker pod so a private-registry image can be pulled on any node. Helm mirrors `imagePullSecrets` into this. Without it, private-registry installs hit node-dependent `ImagePullBackOff` on worker pods only. |
| `WORKER_SERVICE_ACCOUNT` | **yes** | — | SA bound to worker pods (separate from operator SA, minimal privileges) |
| `AGE_SECRET_NAME` | **yes** | — | Operator-managed merged Secret holding `AGE_PUBLIC_KEYS` for worker pods to mount; rebuilt from the `BackupRecipient` objects |
| `BOOTSTRAP_AGE_RECIPIENTS` | no | — | Comma- or newline-separated age public keys seeded as `BackupRecipient` objects at startup (what the chart's `agePublicKeys` becomes). Idempotent; never deletes. Exists because Helm cannot ship a CR for a CRD it installs in the same release ([§18](adr/README.md)). |
| `WORKER_CPU_LIMIT` | no | `2000m` | CPU limit for worker pods |
| `WORKER_MEMORY_LIMIT` | no | `2Gi` | Memory limit for worker pods |
| `WORKER_CPU_REQUEST` | no | `250m` | CPU request for worker pods |
| `WORKER_MEMORY_REQUEST` | no | `256Mi` | Memory request for worker pods |
| `WORKER_NODE_SELECTOR` | no | — | JSON object → `nodeSelector` on worker pods (chart: `worker.nodeSelector`). Strict JSON: unknown fields fail startup. |
| `WORKER_TOLERATIONS` | no | — | JSON `[]corev1.Toleration` (chart: `worker.tolerations`) |
| `WORKER_AFFINITY` | no | — | JSON `corev1.Affinity` (chart: `worker.affinity`) |
| `WORKER_PRIORITY_CLASS_NAME` | no | — | `priorityClassName` for worker pods |
| `WORKER_POD_ANNOTATIONS` | no | — | JSON object merged into worker pod annotations |
| `WORKER_EXTRA_ENV` | no | — | JSON `[]corev1.EnvVar` appended to the worker container (e.g. proxy settings) |
| `WORKER_EXTRA_VOLUMES` / `WORKER_EXTRA_VOLUME_MOUNTS` | no | — | JSON `[]corev1.Volume` / `[]corev1.VolumeMount` appended to the worker pod — the way to mount a CA bundle or a cloud credential file |
| `WORKER_BACKOFF_LIMIT` | no | `2` | K8s-native Job retry limit on failure. Initial attempt + N retries with exponential backoff (10s, 20s, 40s, ..., capped at 6m). Catches transient failures (DB blips, brief network partitions) so a 30-second outage during the cron tick doesn't cost a full cron interval of missing backups. `0` disables. Range: 0–10. |
| `METRICS_REFRESH_INTERVAL_SECONDS` | no | `30` | Tick interval of the `MetricsRefresher` (chart: `config.metricsRefreshIntervalSeconds`). Floor: 5. Trade off frequency against destination read load. |
| `STORAGE_SCRUB_ENABLED` | no | `false` | Enables the periodic storage scrubber that re-hashes the most recent dump per (target, destination) and compares with `meta.json`'s SHA256. Off by default because each scrub re-streams a full encrypted dump. Surfaces as `backup_operator_storage_scrub_passed` and the `BackupStorageCorrupted` alert. |
| `STORAGE_SCRUB_INTERVAL_HOURS` | no | `24` | How often the scrubber runs. Lower bound: 1. Multiply by `(targets × destinations)` to estimate egress; for a fleet with large dumps that's a real bandwidth budget. |
| `STORAGE_SCRUB_MAX_PAIRS_PER_TICK` | no | `500` | Upper bound on `(target, destination)` pairs re-hashed per tick; the remainder is picked up round-robin on following ticks, so a tick can never outlast the interval. Effective per-pair cadence is `interval × ceil(pairs / 500)`. |
| `UI_ENABLED` | no | `false` | Enable the built-in web dashboard and management API on `UI_ADDR`. |
| `UI_ADDR` | no | `:8081` | Listen address for the UI HTTP server. |
| `UI_MAX_BODY_BYTES` | no | `1048576` | Per-request body cap applied via `http.MaxBytesReader` to every UI route. Without this an unauthenticated POST of a multi-GB body OOMs the operator. |
| `UI_MAX_SSE_CLIENTS` | no | `256` | Concurrent SSE subscribers allowed on `/api/events`. Excess clients receive `503` so they retry instead of pinning operator memory. |
| `PROMETHEUS_URL` | no | — | When set (e.g. `http://prometheus-operated.alert.svc:9090`), `/api/alerts` proxies `/api/v1/alerts` filtered to `alertname=~"^Backup.*"` so the UI mirrors what Alertmanager will route. When unset, the UI falls back to a local heuristic over the operator's own metric registry — useful during onboarding but does not honor the rule's `for:` duration. |
| `ALERTMANAGER_URL` | no | — | Used for the "open in Alertmanager" link on the Alerts page, the `/api/alerts/status` connectivity check (`GET /api/v2/status`), and the `/api/alerts/test` endpoint that sends a test alert (`POST /api/v2/alerts`). |
| `DOCS_ENABLED` | no | `false` | Enable the read-only documentation portal on `DOCS_ADDR`. Off by default — flip on to expose CLAUDE.md / README.md / generated tech-stack page. The docs server holds no credentials and reads no Kubernetes state, so it is safe to expose to a wider audience than the management UI. |
| `DOCS_ADDR` | no | `:8083` | Listen address for the docs portal. Distinct port so cluster admins can scope ingress separately from the mutating UI. |
| `DOCS_DIR` | no | `/app/docs` | Directory holding `CLAUDE.md`, `README.md`, `go.mod`. Populated by the Dockerfile at image build time. Locally, point at the repo root via `DOCS_DIR=..` for `just run`. |
| `UI_READ_ONLY` | no | `false` | `true` → every mutating UI endpoint (source/destination CRUD, trigger, suspend, test-connection, settings, age-key mutation) returns `403`. Enforced as a global middleware so new mutating endpoints inherit it. Use when exposing the dashboard to a read-only audience. Does not replace the auth proxy. |
| `UI_ALLOW_KEY_MUTATION` | no | `false` | Gates the age-recipient add/remove endpoints specifically. `false` (default) → `/api/age-keys` POST/DELETE return `403` even when `UI_READ_ONLY=false`. Age keys are the encryption root of trust, so mutating them through the unauthenticated UI is opt-in. |
| `PPROF_ADDR` | no | — (disabled) | When set (e.g. `127.0.0.1:6060`) the operator serves `net/http/pprof` on a dedicated listener for heap/goroutine/CPU profiling. Empty = disabled. **Bind to loopback only** — a `containerPort` does not restrict reachability, so a `0.0.0.0` bind exposes heap dumps (holding in-flight Secret data) to every pod in the cluster. The chart binds `127.0.0.1` when `pprof.enabled=true`; reach it via `kubectl port-forward`. A CPU/trace profile is also a cheap DoS, so enable only transiently for diagnosis. |

### Worker (`cmd/worker/main.go`)

| Key | Required | Default |
|---|---|---|
| `AGE_PUBLIC_KEYS` | **yes** | — |
| `RUN_TIMEOUT_SECONDS` | no | `3600` |
| `TEMP_DIR` | no | `/tmp/backup-operator` |
| `DEFAULT_RETENTION_DAYS` | no | `30` |
| `DEFAULT_MIN_KEEP` | no | `3` |
| `DEFAULT_SCHEDULE` | no | `0 2 * * *` (the flattener needs it for fallback) |
| `POD_NAMESPACE` | recommended | — (or pass via `--namespace`) |
| `POD_NAME` | no (Phase-2) | — | Worker pod name (Downward API). Used as the `OwnerReference` name for spawned ephemeral verifier DB pods so K8s GC cascades them. Only needed for Phase-2 restore-verification modes. |
| `POD_UID` | no (Phase-2) | — | Worker pod UID (Downward API); the `OwnerReference` UID for spawned verifier pods. Only needed for Phase-2 restore-verification modes. |

### Restore (`cmd/restore/main.go`)

CLI flags only. Worker flags are `--source <BackupSource name>` and `--namespace`.

| Flag | Required | Default |
|---|---|---|
| `--destination` | **yes** | — (name of the `BackupDestination`; repeatable or comma-separated — list/latest merge across all, `--purge` deletes from all) |
| `--target` | **yes** | — |
| `--namespace` | no | `default` |
| `--age-key` | yes (for download) | — |
| `--timestamp` | no | latest |
| `--list` | no | `false` |
| `-o` | no | `-` (stdout) |
| `--decompress` | no | `false` |
| `--purge` / `--before` / `--dry-run` | no | right-to-erasure ([§17.5](compliance.md#175-data-deletion)); runs against every `--destination` given, per-destination counts on stderr, non-zero exit (`ERASURE INCOMPLETE`) if any delete or listing failed |

`backup-restore migrate --namespace <ns> [--age-secret <name>] [--apply [--delete-legacy]]` converts legacy labelled Secrets into CRs + credentials Secrets. Without `--apply` it prints the manifests (including plaintext credentials — review before committing).

---

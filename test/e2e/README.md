# End-to-end test: backup → restore

`run.sh` (or `just test-e2e`) proves the one thing the unit tests can't: a
backup made by the real operator, chart and worker can be restored by the
real `backup-restore` CLI, with its data intact.

## What it does

1. Creates a kind cluster `backup-e2e` with its **own kubeconfig** in the
   workdir. `~/.kube/config` and other contexts are never read or written.
2. Builds the image (or uses `E2E_IMAGE` with `E2E_SKIP_BUILD=1`), loads it
   into kind, builds the restore CLI and generates two age keys: the real
   one and a wrong one.
3. Deploys Postgres 17 and MinIO (`bitnamilegacy/minio`, pinned tag) and
   installs the chart with the public key, a 5 s metrics-refresh tick and
   `workerBackoffLimit=0` (failures surface immediately).
4. Seeds Postgres with two tables (37 users, 1234 orders), applies the
   credentials Secrets, the `BackupSource` and the `BackupDestinations`
   (`manifests/30-backup.yaml`), waits for `Ready`, triggers the run with
   `kubectl create job --from=cronjob/backup-e2e-postgres`, waits for the
   Job to complete, then for `LastRunSucceeded=True`, `Reachable=True` and a
   populated `lastDumpSizeBytes`.
5. Restores out of cluster: `--list` must show exactly one run, a restore
   with the wrong key must exit non-zero and leave no output file, and a
   restore with the right key is loaded into a fresh database. Row counts
   and `sum(orders.amount)` must match the seed.
6. `--purge --dry-run` must find the artifacts and delete nothing.

On failure it writes the resources, CRs, events, operator log and worker
logs to `$E2E_WORKDIR/logs` (CI uploads them as the `e2e-logs` artifact).
It deletes the cluster on exit unless `E2E_KEEP=1` is set.

## How the restore CLI reaches the bucket

The CLI runs on the host, the way an operator runs it on a laptop. It reads
the `BackupDestination` through the kubeconfig and then dials the endpoint
written in the CR. The worker has to use in-cluster DNS
(`http://minio:9000`), which the host cannot resolve.

So the test declares a **second destination on the same bucket and
prefix**, `minio-host`, with endpoint `http://127.0.0.1:$E2E_S3_PORT`, and
the test `kubectl port-forward`s the MinIO Service to that port. The
source's `spec.destinations` allow-list contains only `minio`. That means:

- the worker never uploads to `minio-host`,
- the metrics refresher never probes it (no source fans out to it),
- the CLI goes through the production code path unchanged: CR lookup,
  credentials Secret, S3 backend, sha256 check, age decrypt.

The alternative, running the CLI inside a pod, would need the binary copied
in, extra RBAC for a pod ServiceAccount, and the private key inside the
cluster. That last one is exactly what the encryption model forbids, so the
test shouldn't model it either.

## Knobs

| Env | Default | |
|---|---|---|
| `E2E_WORKDIR` | `mktemp -d` | kubeconfig, keys, restored dump, logs |
| `E2E_CLUSTER` | `backup-e2e` | kind cluster name |
| `E2E_IMAGE` | `backup-operator:e2e` | image under test |
| `E2E_SKIP_BUILD` | `0` | `1` = don't `docker build` (CI builds with the GHA cache) |
| `E2E_KEEP` | `0` | `1` = keep the cluster for debugging (`export KUBECONFIG=$E2E_WORKDIR/kubeconfig`) |
| `E2E_S3_PORT` | `19000` | host port for the MinIO port-forward |
| `E2E_HELM_ARGS` | — | extra `helm install` flags, word-split (e.g. `--set ui.enabled=true`) |

All credentials in `manifests/` are dummies for the throwaway cluster.

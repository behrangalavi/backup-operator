# Development Workflow

## 16. Development Workflow

```bash
# Build all three binaries natively
just build           # operator
just build-worker    # worker
just build-restore   # restore CLI

# Generate check + lint + Go tests (-race) + frontend tests + chart tests
just check

# Individual gates
just test-unit     # go test -race via pinned gotestsum
just test-ui       # node --test on src/ui/static/js/tests/*.test.mjs
just test-chart    # helm unittest + helm lint --strict + kubeconform
just build-linux amd64   # operator + worker + restore for one arch

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

# 3. Generate an age key pair offline (idempotent — runs once). Installs the
#    CRDs and registers the public key as BackupRecipient `local-test`.
just gen-age-key

# 4. Apply the test stack: namespace, worker SA/RBAC, in-cluster Postgres,
#    in-cluster MinIO with bucket init, BackupSource + BackupDestination
#    with their credentials Secrets.
just test-up

# 5. Run the operator locally — talks to the cluster via your kubeconfig.
#    It produces a CronJob `backup-test-postgres` from the BackupSource and
#    materialises the merged age Secret from the BackupRecipients.
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
  `AGE_PUBLIC_KEYS` from the operator-managed `backup-operator-age`
  Secret, and dumps Postgres → encrypts → uploads to MinIO at
  `s3://backups/...`. The kind node caches the `dev` tag: after a rebuild,
  retag (`docker tag backup-operator:dev backup-operator:dev-2`) and set
  `WORKER_IMAGE` accordingly, or the pod runs the previous build.
- `just generate` regenerates DeepCopy + CRD YAML after editing
  `src/api/v1alpha1`; `just check` fails when they drift.
- The **age private key** stays at `~/age-backup-test.key`, never in
  the cluster — same security model as production. To download and
  decrypt a dump, use the `backup-restore` binary with `--age-key`.

---

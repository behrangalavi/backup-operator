# Backup Operator — Claude Code Guide

A Kubernetes operator in Go that backs up **PostgreSQL, MySQL, MariaDB, MongoDB and Redis**. Dumps are encrypted with `age` to public keys only, uploaded to several destinations in parallel (SFTP, FTPS, S3-compatible, Azure Blob, GCS) and analysed run-over-run, so alerts can fire on dump *content* (a table vanished, row counts collapsed, schema changed), not just on job status.

This file is the short version. The full reference lives in [`docs/`](docs/), one file per topic, and every design decision has its own record in [`docs/adr/`](docs/adr/README.md). Read the file for the area you are about to change before changing it.

## The model in one screen

- **CRD-based.** Configuration is typed: `BackupSource` (`bsrc`), `BackupDestination` (`bdst`) and `BackupRecipient` (`brcp`) in `backup.mogenius.io/v1alpha1`, all namespaced. Only passwords and keys live in Secrets, referenced via `credentialsSecretRef`. The API server rejects a bad spec at admission. The old labelled-Secret model is gone; `backup-restore migrate` converts it ([ADR 0001](docs/adr/0001-crds-after-all-backupsource-backupdestination-backuprecipien.md)).
- **Operator reconciles CronJobs; it never runs backups.** One managed `batch/v1.CronJob` per `BackupSource`, owned by it (OwnerReference, GC cascade). Status is written by the operator only, change-only.
- **Three binaries, one image for the in-cluster two.**
  - `backup-operator` (`src/cmd/main.go`): reconciler, metrics refresher, storage scrubber, optional UI (`:8081`) and docs portal (`:8083`).
  - `backup-worker` (`src/cmd/worker`): one-shot Job pod. Stats → dump → gzip/zstd → age → temp file → fan-out upload → `meta.json` sidecar → retention.
  - `backup-restore` (`src/cmd/restore`): runs on the operator's laptop, the only place the age private key ever exists. Not in the image on purpose.
- **Metrics come from storage, not from workers.** Worker pods are too short-lived to scrape, so the operator rebuilds run-level gauges from each destination's newest `meta.json`.

## Where to look

| You are changing… | Read first |
|---|---|
| Overall flow, package layout, the three binaries | [docs/architecture.md](docs/architecture.md) |
| CRD fields, status, credentials Secret keys (`src/api/v1alpha1`, `internal/secrets`) | [docs/crds.md](docs/crds.md) |
| Env vars of operator, worker, restore CLI (`config/`, `cmd/`) | [docs/configuration.md](docs/configuration.md) |
| A new database engine or storage backend (`dumper/`, `storage/`) | [docs/extending.md](docs/extending.md) |
| Encryption, recipients, key handling (`crypto/`, `recipient_reconciler.go`) | [docs/encryption.md](docs/encryption.md) |
| Object paths, `meta.json` contents (`internal/meta`, `internal/backup`) | [docs/storage-layout.md](docs/storage-layout.md) |
| Metrics, PrometheusRule, local alert evaluator (`metrics/`, `internal/alerts`) | [docs/observability.md](docs/observability.md) |
| Error handling paths, what the operator sees when X breaks | [docs/failure-modes.md](docs/failure-modes.md) |
| Day-2 kubectl recipes | [docs/operations.md](docs/operations.md) |
| Data flow, RBAC, audit trail, erasure | [docs/compliance.md](docs/compliance.md) |
| Helm chart, CI/CD, releases (`charts/`, `.github/`) | [docs/helm.md](docs/helm.md) |
| Local test stack, build targets | [docs/development.md](docs/development.md) |
| Install walkthrough, UI auth proxy | [docs/getting-started.md](docs/getting-started.md) |
| Anything that looks odd or deliberate | grep [docs/adr/](docs/adr/README.md) — the reason is usually recorded |

## Package map

```
src/
├── api/v1alpha1/   CRD types + generated DeepCopy (CRD YAML → charts/backup-operator/files/crds)
├── cmd/            main.go (operator), worker/, restore/ (+ migrate subcommand)
├── controllers/    CronJob reconciler, status writers, metrics refresher, scrubber, recipient reconciler, StoragePool
├── internal/       backup (pipeline), secrets (CR+Secret → flat Source/Destination), meta, scheduler, safe, redact, migrate, labels, agekeys, alerts
├── dumper/         Dumper interface + factory + postgres, mysql (also MariaDB), mongo, redis
├── storage/        Storage interface + factory + sftp, ftps, s3, azure, gcs; storagetest = conformance suite + fake
├── verifier/       restore verification: stream-validate (in-process) and ephemeral DB pods (schema-only/sample/full)
├── analyzer/       run-to-run comparison → anomalies
├── crypto/ config/ metrics/ assert/
├── ui/             management UI + API; static/ is a vanilla-JS SPA (no build step)
└── docs/           read-only docs portal serving README.md, CLAUDE.md and docs/**
charts/backup-operator/   Helm chart (CRDs as templates, tests in tests/)
test/e2e/                 kind-based backup → restore round trip (CI job `e2e`)
```

Full annotated tree: [docs/architecture.md](docs/architecture.md#4-directory-structure).

## Commands

```bash
just check          # generate-check + lint + go test -race + node --test + chart tests — the CI gate
just test-unit      # Go tests only
just test-ui        # SPA tests (node --test)
just test-chart     # helm-unittest + lint + kubeconform
just test-e2e       # kind cluster, real Postgres + S3, backup → restore round trip
just generate       # after editing src/api/v1alpha1 (DeepCopy + CRD YAML); CI fails on drift
just build / build-worker / build-restore   # native binaries into dist/
just run            # operator locally against your kubeconfig (copy .env.example → .env)
```

Build into `dist/` through the Justfile. A bare `go build` inside `src/` drops a 100 MB binary next to the sources; those paths are gitignored, but never commit a binary.

## Coding conventions

@docs/conventions.md

## Keeping the docs true

Documentation that drifts from code is worse than none. With a behaviour change, update the matching file in the same commit:

- Package added, moved or removed → [docs/architecture.md](docs/architecture.md) and the package map above.
- CRD field → [docs/crds.md](docs/crds.md), then `just generate`.
- Env var → [docs/configuration.md](docs/configuration.md).
- Metric or default alert → [docs/observability.md](docs/observability.md) (and keep `internal/alerts.LocalProvider` in sync with the chart's rules).
- New failure mode worth flagging → [docs/failure-modes.md](docs/failure-modes.md).
- Data-flow or access-control change → [docs/compliance.md](docs/compliance.md).
- A decision that changes how an existing system behaves → a new file in [docs/adr/](docs/adr/README.md) with the *reason*, and mark what it replaces as `Superseded`.

Keep this file short. Detail goes into `docs/`; this file only points there.

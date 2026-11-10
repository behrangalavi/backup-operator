# Helm Distribution

## 19. Helm Distribution

### 19.1 Installation

```bash
# From OCI registry (after release)
helm install backup-operator oci://ghcr.io/behrangalavi/charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient"

# From local chart (development)
helm install backup-operator ./charts/backup-operator \
  -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient"

# Multi-key rotation:
#   --set agePublicKeys[0]=age1qx...primary --set agePublicKeys[1]=age1yz...dr
```

The chart installs the three CRDs as templates (`crds.install`, default on) with `helm.sh/resource-policy: keep` (`crds.keep`), so `helm upgrade` updates the schema and `helm uninstall` leaves your BackupSources in place. CRDs that were applied with `kubectl` first (`just install-crds`) must be handed over before the chart can manage them — Helm refuses to adopt objects without its ownership metadata (`just adopt-crds`, or install with `crds.install=false`). Upgrading an install that still uses labelled Secrets: run `backup-restore migrate -n <ns> --age-secret <release>-age --apply` — the new operator does not read labelled Secrets ([§14](failure-modes.md#14-failure-modes)).

**Defaults to know:**

- `crds.install: true`, `crds.keep: true` — see above.
- `networkPolicy.enabled: false` — when on, the policy is `[Ingress, Egress]`: metrics ingress from `networkPolicy.metrics.allowedFrom` (default: the `networkPolicy.monitoringNamespace`), UI/docs ingress only from `networkPolicy.ui.allowedFrom` (empty = denied), egress to the API server limited by `networkPolicy.apiServerCIDRs` when set, 9090/9093 scoped to the monitoring namespace, plus a worker-pod policy (`networkPolicy.worker.*`).
- `worker.{nodeSelector,tolerations,affinity,priorityClassName,podAnnotations,extraEnv,extraVolumes,extraVolumeMounts}` — rendered into the `WORKER_*` JSON env vars ([§7](configuration.md#7-configuration-reference)). Operator pod knobs: `strategy`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName`, `podAnnotations`, `podLabels`, `extraEnv`, `envFrom`, `extraVolumes`, `extraVolumeMounts`, `podSecurityContext`, `securityContext`, `probes.{startup,liveness,readiness}`.
- `values.schema.json` types every value (e.g. `workerBackoffLimit` 0–10, `agePublicKeys` string|array); `templates/NOTES.txt` prints how to reach the UI and warns when the UI is on without NetworkPolicy ingress; `tests/*_test.yaml` are helm-unittest suites run by CI (`just test-chart`).
- `prometheusReleaseLabel: kube-prometheus-stack` — applied as the `release:` label on `ServiceMonitor` and `PrometheusRule` so kube-prometheus-stack picks them up. Set to `""` to opt out.
- `alerts.prometheusURL: http://prometheus-operated.alert.svc.cluster.local:9090` — assumes kube-prometheus-stack lives in the `alert` namespace. Override or set to `""` for the local heuristic.
- `alerts.alertmanagerURL: http://alertmanager-operated.alert.svc.cluster.local:9093` — used for the "open in Alertmanager" UI link, the `/api/alerts/status` connectivity check, and the `/api/alerts/test` endpoint.

### 19.2 CI/CD

Two GitHub Actions workflows:

All GitHub Actions are pinned to commit SHAs with a `# vX.Y.Z` comment that Renovate updates. `backup-worker` carries the same `-X main.Version` build-time version as the operator (logged at startup).

| Workflow | Trigger | What it does |
|---|---|---|
| `ci.yaml` | PR to `main` | `go build` / `go test -race` / `go vet`, generate-check (CRD + DeepCopy drift), golangci-lint, govulncheck (pinned), `node --test` for the SPA, `helm lint --strict` + helm-unittest + two renders + kubeconform, Docker smoke build + DB-tool check |
| `release.yaml` | Push to `main` | Re-runs the full CI gate via `workflow_call`, Semantic Release → multi-arch Docker image → GHCR (Trivy scan fails on HIGH/CRITICAL, `.trivyignore` honoured; cosign keyless signature on the image digest), package + push Helm chart to OCI registry (cosign-signed) |

### 19.3 Release Process

Releases are fully automated via [Semantic Release](https://github.com/semantic-release/semantic-release). Merging a PR with conventional commit prefixes triggers the appropriate version bump:

| Prefix | Effect |
|---|---|
| `fix:` | Patch release (0.x.y) |
| `feat:` | Minor release (0.x.0) |
| `feat!:` / `BREAKING CHANGE:` | Minor release while pre-1.0 — `.releaserc.json` maps breaking → minor; delete that rule to cut 1.0.0 |
| `docs:`, `ci:`, `chore:` | No release |

No manual tagging required. The workflow runs `go test`, then semantic-release determines the next version, creates a GitHub Release, builds the Docker image, and publishes the Helm chart.

### 19.4 Settings page

`#/settings` shows the operator's **effective fleet-wide defaults** read-only — default schedule, run timeout, retention defaults, temp dir, worker resources — and exports them as a `values.yaml` for GitOps. Per-source configuration is edited on the Sources page and lands typed in `BackupSource.spec`.

**API Endpoints:**
- `GET /api/settings` — effective defaults (`readOnly: true`)
- `PUT /api/settings` — `405`; defaults are Helm-managed
- `GET /api/settings/export` — downloadable `values.yaml`

```
Helm values.yaml → env vars → operator (fleet defaults)
BackupSource.spec                      (per-source overrides, typed)
                 ↓
        Export values.yaml → Git → helm upgrade (GitOps)
```

---

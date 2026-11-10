# 0031. Health probes on the operator

**Status:** Accepted.

controller-runtime's built-in `/healthz` and `/readyz` are served on `:8082` (separate from metrics `:8080` and UI `:8081`). Without probes, Kubernetes cannot detect a stuck operator and restart it.

# 0028. Worker resource limits via env vars

**Status:** Accepted.

`WORKER_CPU_LIMIT`, `WORKER_MEMORY_LIMIT`, `WORKER_CPU_REQUEST`, `WORKER_MEMORY_REQUEST` flow from Helm into every CronJob's container spec. Sensible defaults ship (2 CPU / 2Gi); empty disables. Without limits, a large dump can OOM the node.

# 0003. `BackupSource.status` is written by the operator only, change-only

**Status:** Accepted.

The worker could patch status directly for immediacy, but that would hand every worker pod `backupsources/status: patch` — and the project's standing principle is that the worker's RBAC stays at "read what you need, emit Events". So the same `MetricsRefresher` tick that rebuilds the Prometheus gauges from `meta.json` also mirrors them onto status, and the CronJob reconciler writes `Ready` / `cronJobName` / `materializedSchedule`. Both go through `updateSourceStatus`, which re-reads, mutates and compares before writing: at 10 000 sources a 30 s tick must not turn into 10 000 PATCHes, and every status write bumps the list ResourceVersion the refresher uses to skip idle ticks, so unconditional writes would have defeated that optimisation. The CronJob reconciler uses `GenerationChangedPredicate` on its `For` for the same reason — status writes do not bump generation, so they never wake it.

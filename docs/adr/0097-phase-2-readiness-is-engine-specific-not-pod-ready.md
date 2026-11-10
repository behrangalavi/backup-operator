# 0097. Phase-2 readiness is engine-specific, not Pod `Ready`

**Status:** Accepted.

The stock postgres/mysql/mongo/redis container images don't ship a meaningful `readinessProbe`, and `kubelet Ready=true` only tells us the container started — not that the DB accepts connections. Each engine carries its own `Probe` function (`SELECT 1` via pgx, `mysqladmin ping`, `mongosh runCommand({ping:1})`, `redis-cli PING`) that runs after the kubelet probe passes. Adds 0–10 s in practice. Without it, the entire restore would race the DB's startup phase and produce flaky `psql: connection refused` mismatches.

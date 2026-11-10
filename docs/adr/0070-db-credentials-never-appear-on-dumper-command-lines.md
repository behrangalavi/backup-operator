# 0070. DB credentials never appear on dumper command lines

**Status:** Accepted.

`mysqldump` consumes the password through `MYSQL_PWD`, `mongodump` reads it from a 0600 YAML config file, `redis-cli` reads it from `REDISCLI_AUTH`, and `pg_dump` keeps using `PGPASSWORD`. The previous `-pPASSWORD` and `--uri=mongodb://user:pass@…` patterns were visible in `ps`, container telemetry, and any sidecar with PID-namespace access — at the project's intended scale (10k+ daily backups across many tenants) that's a continuous exfiltration vector. The mongo config file is created via `os.CreateTemp` + explicit `Chmod(0600)` and removed via `defer`. Trade-off: an extra file write per Mongo run vs. a credential that may otherwise outlive the process in kernel buffers.

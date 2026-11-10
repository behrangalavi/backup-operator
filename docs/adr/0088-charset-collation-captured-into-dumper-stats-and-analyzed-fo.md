# 0088. Charset/collation captured into `dumper.Stats` and analyzed for drift

**Status:** Accepted.

Postgres `pg_database.datcollate`/`encoding` and MySQL `@@character_set_database`/`@@collation_database` are recorded into the meta. The analyzer flags drift between consecutive runs (`CharsetChanged`) and the operator exposes `backup_operator_charset_changed`, alert `BackupCharsetChanged` (severity warning, not info — utf8 → utf8mb4 transitions silently truncate at restore, so they need real attention). Mongo / Redis don't have a database-level charset, so the field stays empty and the drift detector skips them — the gauge is absent entirely rather than stuck at 0, matching how the rest of our optional metrics behave.

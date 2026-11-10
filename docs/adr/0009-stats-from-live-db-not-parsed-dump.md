# 0009. Stats from live DB, not parsed dump

**Status:** Accepted.

Dump formats differ across versions, vendors, and tools. Querying `pg_stat_user_tables` / `INFORMATION_SCHEMA.TABLES` / Mongo `collStats` is portable and orders of magnitude faster.

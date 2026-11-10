# 0096. Phase-2 schema-only is stream-filtering for SQL, a label for mongo/redis

**Status:** Accepted.

Postgres `psql` and MySQL's `mysql` CLI both replay plain-SQL dumps line-by-line; we can drop everything between `COPY ... FROM stdin;` and the trailing `\.` (or `INSERT INTO` lines) on the fly via a goroutine-piped scanner, keeping the DDL intact. The engine literally never sees the data. Mongo's BSON archive and Redis's RDB are binary and indivisible at this granularity — schema-only on those engines is a label, not a meaningfully cheaper restore. Documented per-engine in `verifier/restore/`; users who care about cost on those engines should pick `stream-validate` instead. Sample mode is currently equivalent to Full at the engine layer: pre-filtering hooks for true sampling are reserved for a future iteration.

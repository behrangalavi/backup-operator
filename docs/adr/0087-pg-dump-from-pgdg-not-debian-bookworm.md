# 0087. pg_dump from PGDG, not debian-bookworm

**Status:** Accepted.

debian-bookworm only ships `postgresql-client-15`, which refuses to dump from a server two major versions newer than itself ("aborting because of server version mismatch"). PG 17 servers are now common in the field, so the client lag broke real installations. Adding the official `apt.postgresql.org` repo and installing `postgresql-client-17` resolves it cleanly: pg_dump 17 is backward-compatible with PG 9.2+, so a single client version covers every supported source without per-version branching at exec time. Trade-off: one extra third-party APT repo in the runtime stage (already there for MySQL on amd64), worth it for actually being able to dump current PG versions.

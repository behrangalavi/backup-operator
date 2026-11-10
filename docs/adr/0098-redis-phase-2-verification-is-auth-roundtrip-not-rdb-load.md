# 0098. Redis Phase-2 verification is auth + roundtrip, not RDB load

**Status:** Accepted.

`redis-cli` has no clean "load this RDB into a running server" path: the supported routes are all "restart the server with `dbfilename=...`", which doesn't fit the spawn-then-restore model. The `DEBUG RELOAD` strategy is fragile across versions. Redis Phase-2 currently confirms the verifier-pod is reachable and authenticatable, plus a `SET`/`GET` roundtrip; Phase-1 `stream-validate` already confirms decryptability + RDB header magic. Real RDB load is a follow-up. The `Notes` field on `SmokeResult` carries a `"redis Phase-2 verification is auth+roundtrip only"` advisory string so operators see the limitation in the meta without having to read the source.

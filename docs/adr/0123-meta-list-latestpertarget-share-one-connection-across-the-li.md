# 0123. `meta.List` / `LatestPerTarget` share one connection across the List + per-file Gets

**Status:** Accepted.

Both enumerate a prefix then `Get` each `*.meta.json` to parse it. On SFTP/FTPS every `Get` dials a fresh SSH/TLS connection (~100-300 ms handshake), so a target with N runs cost 1 + N handshakes — and the UI hot paths (`target()`, `fleetHeatmap`, `estimateDuration`, `listTargets`) hit this constantly. The metrics refresher's `loadLatestMeta` already fixed the same pattern via `WithSession`; these two were missed. Fix: type-assert `storage.BatchStorage`, open one session, route the List + all Gets through it (S3 has no session concept and falls through to per-call dialing unchanged). Unlike the refresher's version, a session-open failure falls back to per-call dialing rather than returning nil — the UI prefers stale-but-present data over a blank page.

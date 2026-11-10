# 0012. `Storage.List()` returns logical paths

**Status:** Accepted.

Storage implementations apply `pathPrefix` internally on `Upload`/`Get`/`Delete`. Returning raw server-side paths from `List` would break the round-trip (caller passes `Object.Path` back to `Get`, gets double-prefixed). This is enforced by per-implementation `stripPrefix` helpers.

# 0019. Canonical `MetaFile` in `internal/meta`, not per-consumer copies

**Status:** Accepted.

The pipeline, metrics refresher, and UI all deserialise the same `*.meta.json` sidecar. Three private structs drifted independently (different field subsets, no shared methods). Consolidating into `internal/meta.MetaFile` gives them `IsFailure()` and `ParsedTimestamp()` for free and eliminates a class of serialisation-mismatch bugs.

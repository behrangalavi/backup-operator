# 0067. Two-phase fan-out with per-destination result tracking

**Status:** Accepted.

The pipeline splits upload into Phase 1 (`fanOutDumps` — dump upload with retry) and Phase 2 (`uploadMeta` — meta upload with retry). Phase 1 collects `[]DestinationResult` (name, storageType, status, error per destination). Phase 2 builds `meta.json` including these results, then uploads to successful destinations only. This enables per-destination health monitoring in the UI without architecture changes to the storage layer. Previously, meta was uploaded alongside the dump in a single `uploadOne` call, so meta could not contain per-destination outcomes.

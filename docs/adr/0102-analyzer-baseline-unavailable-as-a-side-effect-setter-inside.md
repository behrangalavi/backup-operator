# 0102. `analyzer_baseline_unavailable` as a side-effect setter inside `loadPreviousMeta`, not a typed return

**Status:** Accepted.

The function returns `*meta.MetaFile` — `nil` on "no baseline yet", non-nil on success. Both caller paths just take the meta and proceed; they don't care WHY it was nil. Changing the signature to `(*meta.MetaFile, baselineOutcome)` would force every caller to handle a state they don't act on. Instead the function counts per-destination accessibility internally and calls `metrics.SetAnalyzerBaselineUnavailable(target, allBroken)` before returning. Caller signature unchanged; the new signal lives in the metrics layer where the operators actually look. Trade-off: a side effect inside a "loadX" function is mildly surprising. Mitigated by an explicit comment on the function and by the fact that the rest of the operator already follows this "metrics-as-side-effect" pattern (`MetricsRefresher.refreshSource` does the same).

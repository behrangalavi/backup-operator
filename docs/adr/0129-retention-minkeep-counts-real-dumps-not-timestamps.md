# 0129. Retention MinKeep counts real dumps, not timestamps

**Status:** Accepted.

Failed runs write a `dump-<ts>.meta.json` sidecar with no dump file; `groupByTimestamp` bucketed those as full timestamps, so `selectForDeletion`'s "keep the MinKeep newest" floor could be entirely consumed by a streak of failure sidecars at the top of the list — pushing every real dump below the floor and letting the age sweep delete all recoverable backups, precisely during the outage when they are needed. Fix: the floor now only decrements for groups that contain an actual dump artifact (`groupHasDump`); failure-only groups are never floor-protected and are still age-pruned like orphan metas. Regression-tested (`TestSelectForDeletion_FailureMetasDoNotConsumeFloor`).

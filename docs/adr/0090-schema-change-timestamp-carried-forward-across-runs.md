# 0090. Schema-change timestamp carried forward across runs

**Status:** Accepted.

The previous setup answered "did schema change since the last run?" but not "how old is the current schema?". Each meta now stores `schemaChangedAt`: bumped only when `SchemaHash` actually drifts, copied forward otherwise. Exposed as `backup_operator_schema_last_change_timestamp_seconds` so PromQL can answer "schema older than N days" — relevant when an old backup is being restored against a much newer application that no longer matches the dump's structure. Choice of "carry forward" over a counter / drift-rate metric: a single meta is self-describing without needing history, and the metric refresher reconstructs the value from any single recent meta rather than aggregating across the retention window.

# 0065. Settings page is read-only; fleet defaults have one source of truth (the chart)

**Status:** Accepted.

The earlier Settings wizard wrote a `{fullname}-settings` ConfigMap that no operator code ever read — the UI promised "takes effect immediately" and nothing happened. Rather than wire a second configuration layer into the reconcilers, the CRD move made the decision: everything per-source is a typed field on `BackupSource`; everything fleet-wide (`DEFAULT_SCHEDULE`, retention defaults, worker resources) stays a Helm value → env var. The page now shows the operator's effective defaults and exports them as `values.yaml`; `PUT /api/settings` answers 405 with a pointer to `helm upgrade`. A `BackupDefaults` singleton CR was considered and deferred — it would reintroduce the second layer for a benefit nobody has asked for yet.

# 0030. Kubernetes Events as audit trail

**Status:** Accepted. Written under the labelled-Secret model; Secret/annotation names map to CRD fields as listed in [the mapping](README.md#pre-crd-names).

The worker emits `BackupStarted`, `BackupCompleted`, `BackupFailed`, and `RetentionDelete` events against the `BackupSource` (against the source Secret before the CRD move). Visible via `kubectl describe backupsource <name>` and preserved in cluster audit logs. Satisfies DSGVO Art. 30 and SOC2 requirements. The pipeline uses an `EventEmitter` interface so tests stay API-server-free (`NoopEventEmitter`). RBAC grants `events: create, patch` to the shared ServiceAccount.

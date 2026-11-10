# 0004. RecipientReconciler watches `BackupRecipient` only; merged-Secret drift is repaired by requeue, not by a Secret watch

**Status:** Accepted.

The previous implementation watched the merged Secret itself to repair an out-of-band delete immediately. Keeping that would require a Secret informer (full data of every Secret in the namespace held in operator memory) or at least `secrets: list/watch` RBAC for a metadata-only informer — both contradict the point of moving config out of Secrets. The reconcile now returns `RequeueAfter: 10m`, so a deleted merged Secret is rebuilt within ten minutes; a worker pod that races into the gap fails to mount `AGE_PUBLIC_KEYS` and its Job retries, which is loud and self-healing.

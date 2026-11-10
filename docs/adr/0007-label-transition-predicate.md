# 0007. Label-transition predicate

**Status:** Superseded. Superseded by [0001](0001-crds-after-all-backupsource-backupdestination-backuprecipien.md). There are no role labels any more: the CronJob carries a controller OwnerReference to its `BackupSource`, and deleting the source lets Kubernetes GC remove it.

The reconciler does need to handle `role` label *removal* (Secret kept, label dropped) — that's an explicit user signal "stop backing this up." Without watching label transitions, an orphan CronJob would persist. The same cleanup runs on a `source`→`destination` relabel: the `RoleDestination` reconcile branch is not a no-op — it routes through the ownership-checked delete, because a Secret that was a source still owns the CronJob the source branch created, and leaving it would keep spawning worker Jobs against a Secret that is now a destination.

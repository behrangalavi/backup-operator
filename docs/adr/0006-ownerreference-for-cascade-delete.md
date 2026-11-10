# 0006. OwnerReference for cascade delete

**Status:** Accepted.

The reconciler does not bookkeep "Secret deleted → delete CronJob" explicitly; K8s GC does. This eliminates a class of stale-state bugs.

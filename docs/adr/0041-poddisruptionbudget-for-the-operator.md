# 0041. PodDisruptionBudget for the operator

**Status:** Accepted.

Only rendered when `replicaCount > 1`. Prevents voluntary evictions from killing the last operator pod during node drains or cluster upgrades. `minAvailable: 1` is the right choice for a leader-elected controller.

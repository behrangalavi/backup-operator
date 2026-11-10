# 0005. Operator does not run backups

**Status:** Accepted.

Earlier iteration ran them in goroutines under an in-process cron. That model couples backup capacity to the operator pod's resource limits, requires hand-rolled overlap protection, and makes per-run logs scattered. K8s CronJobs solve all three for free.

# 0036. Separate ServiceAccounts for operator and worker

**Status:** Accepted. Written under the labelled-Secret model; Secret/annotation names map to CRD fields as listed in [the mapping](README.md#pre-crd-names). Since the CRD move the worker SA no longer has `secrets: list` — it gets its `BackupSource` and the referenced credentials Secrets by name only (see [compliance](../compliance.md#176-access-control-summary)).

The operator SA retains Secret watch, CronJob CRUD, Job watch, Lease CRUD. The worker SA is reduced to Secret get/list + Event create/patch. A compromised worker pod can no longer modify CronJob schedules or leader election leases.

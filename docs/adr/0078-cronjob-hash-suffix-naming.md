# 0078. CronJob hash-suffix naming

**Status:** Accepted. Written under the labelled-Secret model; Secret/annotation names map to CRD fields as listed in [the mapping](README.md#pre-crd-names). The hash input is now the `BackupSource` name (`cronJobNameFor` in `controllers/cronjob_controller.go`).

Kubernetes names are capped at 63 chars; CronJob names need headroom for the `-<job-suffix>` appended by the Job controller. The operator caps at 52 chars. Previously, long Secret names were silently truncated, meaning two Secrets sharing the same 52-char prefix would map to the same CronJob. Now, names exceeding the limit get a SHA256 hash suffix (8 hex chars) that makes collisions cryptographically improbable. The hash is computed from the original Secret name, not the truncated form.

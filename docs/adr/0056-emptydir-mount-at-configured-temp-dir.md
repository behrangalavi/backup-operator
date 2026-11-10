# 0056. EmptyDir mount at configured TEMP_DIR

**Status:** Accepted.

The worker's emptyDir mounts at `r.Worker.TempDir` (not hardcoded `/tmp`). When `TempDir` is not under `/tmp`, a second small emptyDir covers `/tmp` for `os.CreateTemp` calls. Fixes a regression where custom `TEMP_DIR` broke with `readOnlyRootFilesystem`.

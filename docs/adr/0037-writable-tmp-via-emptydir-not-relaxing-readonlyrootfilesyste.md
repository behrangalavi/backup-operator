# 0037. Writable `/tmp` via emptyDir, not relaxing `readOnlyRootFilesystem`

**Status:** Accepted.

The operator needs a small writable `/tmp` (1Mi) for SFTP known-hosts temp files. The worker's main emptyDir is mounted at the configured `TEMP_DIR` path. When `TEMP_DIR` is not under `/tmp`, a second small emptyDir covers `/tmp` for `os.CreateTemp` calls. This preserves PSA-restricted compliance.

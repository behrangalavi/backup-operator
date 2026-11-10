# 0079. SFTP partial file cleanup on upload failure

**Status:** Accepted.

When an SFTP write fails mid-stream (e.g. disk full, network drop), the partially written file is now explicitly removed via `sc.Remove(full)` before returning the error. Previously, partial files were left on the server, consuming space and potentially confusing listing/retention logic. The `f.Close()` return value is now checked (previously deferred and ignored) to catch flush errors.

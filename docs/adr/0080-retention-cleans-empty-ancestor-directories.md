# 0080. Retention cleans empty ancestor directories

**Status:** Accepted.

Date-partitioned paths like `target/2024/01/15/dump-*.age` leave empty parent directories after file deletion. The retention loop now walks `path.Dir()` upward from deleted files to collect all ancestor directories (not just the immediate parent), then removes them deepest-first via an optional `RemoveDirectory` interface. S3 backends (which don't have directories) are unaffected — the interface check is a type assertion. `RemoveDirectory` errors are silently ignored because non-empty directories will correctly fail to delete.

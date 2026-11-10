# 0138. Restore CLI operates on all given destinations

**Status:** Accepted.

`--purge` on one destination made the DSGVO erasure story in [§17.5](../compliance.md#175-data-deletion) misleading, and `--list`/latest on one destination could silently restore an older run when destinations had diverged. `--destination` is repeatable; runs are merged by timestamp, the chosen destination is printed, a failing `Get` falls through to the next destination holding the same run, and purge reports per destination and exits non-zero on any failure (an unlistable destination counts). The streamed ciphertext is hashed and compared with `meta.json`'s `sha256` (B6), compression is resolved suffix-first (the authoritative source when the sidecar is lost in a DR), and the dump-suffix list is exported once from `internal/labels` instead of being duplicated in the CLI.

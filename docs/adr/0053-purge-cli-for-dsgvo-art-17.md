# 0053. Purge CLI for DSGVO Art. 17

**Status:** Accepted.

`backup-restore --purge --target X --before YYYY-MM-DD` deletes all artifacts from storage. `--dry-run` previews. Enables right-to-erasure workflows without manual storage access. The cutoff is timezone-naive UTC.

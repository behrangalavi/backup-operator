# 0076. Pre-upload retention sweep

**Status:** Accepted.

Retention previously ran only AFTER a successful upload. If storage was full, the upload failed and retention never ran — a deadlock where storage stays full forever and all future backups fail. Now retention runs as a pre-flight sweep BEFORE uploading, freeing space for the new dump. MinKeep floor still protects the N newest existing backups. Post-upload retention runs again to enforce thresholds after the new artifact lands. Edge case: with `MinKeep=0` and retention `Days=1`, the pre-flight sweep could delete all existing backups before the new upload. If the upload then fails, the target has zero backups. This is acceptable because `MinKeep=0` is an explicit "I don't need a safety floor" signal.

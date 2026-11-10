# 0023. Post-upload size verification

**Status:** Accepted.

After uploading a dump, the pipeline Lists the uploaded path and compares the remote object's size to the local file. This catches silent truncation or partial writes. If the List itself fails (not all backends support prefix-exact listing), verification is skipped rather than failing the backup — availability over strictness.

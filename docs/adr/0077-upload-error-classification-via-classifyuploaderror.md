# 0077. Upload error classification via `classifyUploadError`

**Status:** Accepted.

Upload errors are string-matched (case-insensitive) against known permanent failure patterns: `no space left on device`, `disk quota exceeded`, `permission denied`, `access denied`, `insufficient storage`. Matches produce `PermanentError` (no retry); everything else is `RetryableError`. This prevents wasting 3 retry attempts (~10 seconds) against a full or inaccessible storage backend. Trade-off: string matching is fragile across locales and storage implementations, but false negatives (retrying a permanent error) only waste time, while false positives (skipping retry on a transient error) are caught by the next scheduled run.

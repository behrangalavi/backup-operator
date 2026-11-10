# 0025. Exponential retry on transient upload failures

**Status:** Accepted.

Upload operations tagged as `RetryableError` are retried up to 3 times with 2s/4s exponential backoff. `PermanentError` (bad credentials, missing bucket) aborts immediately. This makes the backup resilient to short network glitches without wasting time on configuration errors. Context cancellation is respected between retries.

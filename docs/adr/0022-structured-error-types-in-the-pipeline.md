# 0022. Structured error types in the pipeline

**Status:** Accepted.

Upload failures are `RetryableError` (transient network issues), storage-init failures are `PermanentError` (bad credentials), and config issues are `ValidationError`. This lets the fan-out distinguish error classes for logging and future retry logic, without changing the existing best-effort error handling contract.

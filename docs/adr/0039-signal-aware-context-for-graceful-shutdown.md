# 0039. Signal-aware context for graceful shutdown

**Status:** Accepted.

The worker's context chains `signal.NotifyContext(SIGTERM, SIGINT)` → `context.WithTimeout`. SIGTERM from Kubernetes pod termination cancels the pipeline context, allowing in-flight operations to abort cleanly while deferred cleanup (event flush, temp file removal) still runs.

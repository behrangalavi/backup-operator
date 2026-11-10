# 0038. EventBroadcaster shutdown before exit

**Status:** Accepted.

The worker defers `eventBroadcaster.Shutdown()` to flush buffered events. Without it, final events like `BackupCompleted` could be lost because the broadcaster sends asynchronously.

# 0040. Upload concurrency semaphore

**Status:** Accepted.

Fan-out uses a channel-based semaphore (default 4) to limit concurrent uploads. Without it, N destinations each open the dump file and upload simultaneously, causing file-descriptor and bandwidth pressure on clusters with many destinations.

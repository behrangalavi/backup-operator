# 0069. Multi-storage enterprise API endpoints

**Status:** Accepted.

Four new API endpoints support enterprise multi-destination monitoring: connectivity test (`POST /api/destinations/:name/test`), storage stats (`GET /api/destination-stats`), health matrix (`GET /api/destination-health`), and consistency check (`GET /api/consistency-check`). All use 4-goroutine semaphores for parallel storage queries, matching existing concurrency patterns.

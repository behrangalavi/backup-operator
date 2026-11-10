# 0089. Storage scrubber re-hashes stored dumps periodically

**Status:** Accepted.

SHA256 has been written into `meta.json` since the integrity work; the scrubber finally consumes it. Operator-side controller (leader-elected, 24h default tick) lists each source's allowed destinations, fetches the most recent dump, streams it through `crypto/sha256`, and compares the hex against the meta. Failure → `storage_scrub_passed=0` + `BackupStorageCorrupted` (critical). Off by default because each scrub re-streams the full encrypted dump from storage — at scale that's a serious egress bill. Trade-off vs. a server-side checksum API (S3 `ETag`, `x-amz-content-sha256`): client-side re-hash catches storage backends that lie about their own content, and works uniformly across SFTP and S3 without per-backend code paths. The scrubber respects `Source.AllowsDestination` so destination allow-lists narrow scrub scope automatically.

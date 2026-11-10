# 0045. MetricsRefresher: long-lived storage pool + two-tier concurrency

**Status:** Accepted.

Three iterations on the refresher's hot path, the latest replacing the prior two:

  1. **Initial:** sequential `for src` loop, sequential per-destination Get inside each source. With 100 sources × 5 destinations the tick took O(N) wall-clock time and serialised every backend call regardless of how many CPUs the operator had.
  2. **Per-source semaphore (default=4):** parallelised destination fetches *within* a single source. Helped one-source benchmarks but did nothing for fleet load — sources were still processed serially, and the semaphore was scoped per-source, so a destination shared by 50 sources still saw every source's 4 calls in turn.
  3. **Current (this ADR):** `controllers/storage_pool.go` keeps one `Storage` instance per destination alive across ticks, keyed by `(name, sha256(SecretData))`. The hash means a Secret edit (rotated SSH key, swapped S3 endpoint) busts the cached client at the next tick rather than silently authenticating with the previous credentials forever. `pool.Retain(currentDests)` prunes entries for destinations that vanished between ticks. Same pool is reused by the storage scrubber to avoid two parallel client lifecycles for the same backend on the leader pod.

Concurrency is split into two independent semaphores so the right knob limits the right thing:

  - **Global worker pool** (`defaultGlobalConcurrency`, 8): caps how many sources are processed simultaneously per tick. This is the operator pod's CPU/goroutine budget — it bounds how much work the refresher can do at once, nothing more.
  - **Per-destination slot** (`defaultPerDestConcurrency`, 4): caps in-flight calls against a single backend, summed across every source that fans out to it. This is the actual backend-protection knob. A Hetzner Storage Box accepts ~10 concurrent SSH sessions; staying at 4 leaves headroom for worker-pod uploads running in parallel. The previous per-source semaphore conflated these two concerns and protected only the smaller case.

Why not a watch/event-driven model instead (S3 Event Notifications → SQS, inotify-style for SFTP)? The right end state, but it forces a fundamental change in shape: the refresher would become event-driven with a periodic reconciliation fallback, and SFTP has no native event mechanism, so half the deployments would still poll. The pool + two-tier concurrency captures most of the win without the architectural commit. Marked as a future ADR if event-shape becomes worth the cost.

Why not ETag / If-Modified-Since on `meta.json`? The actual hot cost is **connection setup** (SFTP SSH handshake, ~100–300 ms per call), not the 5–50 KB meta payload. Fixing the connection-rebuild dominates fixing the response-body size. ETag remains a useful follow-up for S3 request-cost optimisation at very large scale (>1000 sources), but it's a leaf optimisation, not the root one.

Trade-offs explicitly accepted:
  - **SFTP idle timeout.** Hetzner Storage Box drops idle SSH connections after ~30 min. The pool's clients can outlive that window if a destination has no traffic. Currently relies on `ClientConfig.ClientKeepAlive`-style heartbeats inside the SFTP storage; a future Reconnect-on-Error wrapper at the pool layer is the cleaner long-term fix.
  - **Memory.** One Storage value per destination, kept for the operator's lifetime. Bounded by the destination count (low tens at any realistic scale), not concerning.
  - **Per-destination slot map** is keyed by destination name and never pruned today; rare destination cycling would leak slots. Trivial follow-up, deferred until it matters.
  - **`current` map writes** are now concurrent and need a mutex (added). `sync.Map` was an alternative but adds overhead for a map this short-lived; the explicit mutex is more legible.

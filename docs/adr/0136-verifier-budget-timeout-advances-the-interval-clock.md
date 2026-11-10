# 0136. Verifier budget timeout advances the interval clock

**Status:** Accepted.

The earlier fix (carry the previous verdict on budget timeout, no false critical) left `completedAt` untouched, so `ShouldVerify` saw the verification as still overdue and a chronically slow Phase-2 restore spawned a full DB pod on *every* run. The carried verdict is now re-stamped `completedAt=now` with a note; a first-ever verification that cannot fit the budget writes `skipped` with an explanatory error and a fresh clock, so it alerts once per interval instead of silently burning a pod per run. Budget exhaustion is detected as `errors.Is(vctx.Err(), context.DeadlineExceeded)` while the run context is still alive — a verifier that fails for its own reasons near the deadline stays a hard failure.

# 0117. CronJob schedule's next fire time pre-computed server-side and surfaced everywhere

**Status:** Accepted. Written under the labelled-Secret model; Secret/annotation names map to CRD fields as listed in [the mapping](README.md#pre-crd-names).

Dashboard table, source list card, and target detail Configuration card all show "in 4h 32m (02:00)" or "in 3d (Mon 02:00)" alongside the cron expression. Parsing cron in JS is fragile across edge cases (`*/15`, `0,30`, ranges, day-of-week); the Go side uses `github.com/robfig/cron/v3`'s Standard parser and emits an RFC3339 timestamp in the Target API response. Frontend just formats. Suspended sources emit nil (no next-run badge). Important: `NextRun` is derived from the MATERIALISED schedule (post-jitter, what the CronJob actually fires on), not the user's annotation — so the dashboard shows the effective fire time, not the wishful one.

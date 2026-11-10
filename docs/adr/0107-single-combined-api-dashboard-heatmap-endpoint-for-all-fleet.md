# 0107. Single combined `/api/dashboard/heatmap` endpoint for all fleet-wide projections

**Status:** Accepted.

The dashboard needs heatmap + per-day storage + analyzer anomaly stream + per-target duration stats + daily verification pass rate — five datasets, all derived from "iterate every target's run history". A single endpoint that emits all of them in one pass is much cheaper than five endpoints each calling `meta.List` on the same destinations. The trade-off is response shape coupling: adding a sixth dataset requires touching both Go and JS together. Worth it because the alternative is making the operator hammer destinations five times for data that's all coming from the same meta.json files. Cap on anomaly entries (200 newest-first) keeps the payload bounded on busy fleets.

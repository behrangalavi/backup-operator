# 0081. Operator now calls Alertmanager directly for status checks and test alerts

**Status:** Accepted.

Previously, `ALERTMANAGER_URL` was purely cosmetic (an "open in Alertmanager" link). Two new UI endpoints call Alertmanager: `GET /api/alerts/status` queries `/api/v2/status` for connectivity and version info, and `POST /api/alerts/test` sends a self-resolving test alert via `/api/v2/alerts`. Rationale: users need to verify the full notification pipeline works before relying on it in production. The test alert auto-resolves after 2 minutes. Error responses are sanitized (no raw `err.Error()` in HTTP responses) to prevent leaking cluster topology through the unauthenticated UI.

# 0072. UI body-size cap and SSE client cap

**Status:** Accepted.

`limitBodyMiddleware` wraps every request body with `http.MaxBytesReader(MaxBodyBytes)` (default 1 MiB) so a multi-GB POST cannot OOM the operator before any handler runs. `sseBroker.maxClients` (default 256) refuses additional `/api/events` subscribers with `503` instead of letting them block in subscribe queues. Both are global rather than per-route so newly added mutating endpoints inherit the protection automatically. Tunable via `UI_MAX_BODY_BYTES` and `UI_MAX_SSE_CLIENTS`. These are defence-in-depth — they do not replace the auth proxy, they limit the blast radius if the proxy is misconfigured or removed.

# 0058. SSE over WebSocket for live updates

**Status:** Accepted.

Server-Sent Events are unidirectional (server→client), work through standard HTTP, require no protocol upgrade, and auto-reconnect natively. The broker pattern (subscribe/unsubscribe/publish via Go channels) fits Go idioms. WebSocket would add bidirectional complexity for a use case that only needs push.

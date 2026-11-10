# 0063. SSE periodic refresh broadcast

**Status:** Accepted.

The SSE broker publishes a `refresh` event every 10 seconds in addition to CRUD-triggered events. This ensures clients eventually converge to the current state even if they miss an event (e.g. brief disconnect). The 10s interval is a balance between responsiveness and API load.

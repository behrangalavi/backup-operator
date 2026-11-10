# 0042. UI error sanitization

**Status:** Accepted.

HTTP error responses return generic messages ("internal error", "target not found") instead of raw `err.Error()`. Internal details are logged server-side. This prevents leaking implementation details (file paths, storage errors, internal state) to unauthorized clients, especially when the UI is exposed without an auth proxy.

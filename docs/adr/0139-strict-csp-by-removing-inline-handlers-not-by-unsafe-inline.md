# 0139. Strict CSP by removing inline handlers, not by `'unsafe-inline'`

**Status:** Accepted.

Seventy-five string-built `on*=` attributes made `script-src 'self'` impossible and were the enabling condition for every stored-XSS finding in the SPA. They are gone: templates emit `data-action` / `data-submit` / `data-change` + `data-arg…`, and document-level delegated listeners dispatch to a registry; a node test fails the build if an `on*="` attribute or an unregistered action name reappears. `tr()` now HTML-escapes interpolated variables by default (`trText()` for textContent sinks), every path segment built from an object name is `encodeURIComponent`'d and decoded in the router, and the UI server adds `X-Request-Id`, a V(1) access log and the auth-proxy identity to its Events. The identity is an audit hint — the operator never authorises on a header the proxy may or may not set.

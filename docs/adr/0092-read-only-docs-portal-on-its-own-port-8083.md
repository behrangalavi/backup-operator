# 0092. Read-only docs portal on its own port (`:8083`)

**Status:** Accepted.

The `/docs` portal serves the same `CLAUDE.md` and `README.md` you're reading now, plus a generated tech-stack page from `go.mod`. It runs in the operator pod but on a separate listener so cluster admins can scope ingress separately from the mutating UI: docs can be public or wide-internal while the management UI stays SSO-gated. The docs server holds **no Kubernetes client** and **no Secret access** — it's a static-file renderer. If it gets compromised, an attacker reads the same Markdown files anyone with the GitHub URL can read. That property is what justifies exposing it more loosely. Off by default (`docs.enabled=false`); the Dockerfile populates `/app/docs` at build time so the portal works out of the box once the flag flips. The in-page search dropdown is client-side only — no server-side indexing means the portal works on any read-only mount and survives ConfigMap-level configuration drift.

# 0145. GitHub Actions are SHA-pinned

**Status:** Accepted.

Pinning by SHA with a `# vX.Y.Z` trailer is the one supply-chain control that costs nothing at runtime, and resolving the pins surfaced two references that would have failed on the next run: `aquasecurity/trivy-action@0.33.1` (the tag is `v0.33.1`) and `cycjimmy/semantic-release-action@v5` (no moving `v5` tag exists). Renovate's github-actions manager keeps SHA-pinned lines current.

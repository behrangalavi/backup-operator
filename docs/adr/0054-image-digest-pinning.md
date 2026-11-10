# 0054. Image digest pinning

**Status:** Accepted.

Helm supports `image.digest` in `values.yaml`. When set (e.g. `sha256:abc123...`), both operator and worker use `@sha256:...` instead of `:tag`. Tags are mutable — a compromised registry or accidental re-push can silently change what runs. Digest-pinning guarantees byte-identical images across deploys. Populate from CI: `crane digest ghcr.io/mogenius/backup-operator:v1.2.3`.

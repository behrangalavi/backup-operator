# 0024. PSA-restricted SecurityContext on all pods

**Status:** Accepted.

Both operator and worker pods run with `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities: drop: ALL`, `seccompProfile: RuntimeDefault`, and `runAsNonRoot: true` (UID 1000). The worker writes only to its emptyDir temp volume. This passes Pod Security Admission in `restricted` mode, which is required for hardened production clusters.

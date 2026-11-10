# 0055. Optional NetworkPolicy for operator pod

**Status:** Accepted.

Helm template restricts egress to DNS (53), K8s API (443/6443), SSH (22/23), and HTTPS (443). Opt-in via `networkPolicy.enabled: true`. `extraEgressRules` allows non-standard ports (e.g. MinIO 9000). Limits blast radius of a compromised operator pod.

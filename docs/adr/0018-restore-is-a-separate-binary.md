# 0018. Restore is a separate binary

**Status:** Accepted.

It runs on the operator's machine, never in cluster — that's the only place the private key should ever be. Bundling it into the operator image would tempt people to mount the private key in the cluster "for convenience," which would defeat the entire encryption design.

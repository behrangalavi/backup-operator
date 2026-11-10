# 0010. Sidecar `meta.json` is unencrypted

**Status:** Accepted.

The whole point of analyzer alerts is that they fire automatically without restore. Encrypting the meta would force the operator to hold the private key, which collapses the security model.

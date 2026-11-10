# 0021. `BatchStorage` interface for connection reuse

**Status:** Accepted.

SFTP operations (List, Delete) each opened a fresh SSH connection. During retention — one List + N Deletes — this meant N+1 handshakes. Rather than embedding pooling into the Storage interface (which S3 doesn't need), we added an optional `BatchStorage` interface with `WithSession()`. Callers type-assert and get a reusable session. This keeps the base `Storage` interface minimal while giving SFTP a proper batch path.

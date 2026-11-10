# 0044. SHA256 checksum in meta.json

**Status:** Accepted.

The pipeline computes SHA256 of the encrypted dump during file writing via `io.MultiWriter(file, hash)`. The hex-encoded hash is stored in `meta.json`. This enables offline integrity verification during restore and periodic bit-rot detection without downloading the full dump — compare `sha256` from meta with the stored object.

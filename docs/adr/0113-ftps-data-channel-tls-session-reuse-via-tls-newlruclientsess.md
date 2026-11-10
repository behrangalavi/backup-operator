# 0113. FTPS data channel TLS session reuse via `tls.NewLRUClientSessionCache`

**Status:** Accepted.

Strict FTPS servers — QNAP, Pure-FTPd, vsftpd with `require_ssl_reuse=on` (default) — reject a fresh TLS handshake on the data socket as a session-hijacking countermeasure. Symptom: control channel logs in fine, login succeeds, every STOR/RETR fails the moment PASV opens the data socket. Adding `ClientSessionCache: tls.NewLRUClientSessionCache(32)` to the tls.Config lets the data channel resume the control channel's TLS session, which is what those servers expect. Per-destination cache so each backend gets its own session pool.

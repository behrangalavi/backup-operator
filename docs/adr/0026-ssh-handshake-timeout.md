# 0026. SSH handshake timeout

**Status:** Accepted.

The SFTP `ssh.ClientConfig` sets `Timeout: 30s`. Without it, the SSH handshake (key exchange, auth) blocks indefinitely on an unresponsive server — the ctx-based TCP dialer only covers the initial connect, not the protocol handshake.

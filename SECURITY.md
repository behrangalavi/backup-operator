# Security Considerations

## Authentication

The operator UI (`UI_ENABLED=true`) has **no built-in authentication**.
All mutation endpoints (create/update/delete sources, destinations, trigger
backups, manage age keys) are accessible to anyone who can reach the UI port.

**Always** place an authenticating reverse proxy in front of the UI in
production. See `CLAUDE.md` § 3.1 for OAuth2 Proxy and Ingress basic-auth
examples.

## Credential Visibility in Browser DevTools

Passwords, SSH private keys, and S3 secret keys sent via the UI are
transmitted as JSON in POST/PUT request bodies. While GET responses mask
these fields as `***`, the **original values remain visible** in the
browser's Network tab (DevTools → Network → request → Payload/Preview).

This is inherent to any browser-based management UI. Mitigations:

1. **Use an auth proxy** — prevents unauthorised users from reaching the UI
   in the first place.
2. **Avoid shared screens** — close DevTools before screen-sharing or
   recording.
3. **Use short-lived credentials** — rotate database passwords and storage
   keys regularly; do not reuse across services.
4. **Prefer Kubernetes-native Secret management** — create source and
   destination Secrets via `kubectl apply` or GitOps (Sealed Secrets, SOPS,
   External Secrets Operator) instead of the browser UI when handling
   highly sensitive credentials.

## SFTP Host-Key Verification

By default, SFTP destinations **reject** connections when no `known-hosts`
data is provided in the destination Secret. This prevents silent
man-in-the-middle attacks on the storage transport.

To populate `known-hosts`, run:

```bash
ssh-keyscan -p <port> <host> 2>/dev/null
```

and store the output in the destination Secret's `known-hosts` key.

If host-key verification must be skipped (e.g. during initial testing),
explicitly opt in by adding `insecure-skip-host-verify: "true"` to the
destination Secret's data:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-sftp-dest
  labels:
    backup.mogenius.io/role: destination
    backup.mogenius.io/storage-type: sftp
data:
  host: <base64>
  username: <base64>
  ssh-private-key: <base64>
  insecure-skip-host-verify: dHJ1ZQ==   # base64("true")
```

> **Warning:** Skipping host-key verification allows any server to accept
> uploads. Backup payloads are encrypted with `age`, but an attacker can
> silently discard or collect encrypted artifacts. Use this only for
> development or initial bootstrapping, then switch to proper `known-hosts`.

## Age Encryption Model

- Backups are encrypted with `age` **public keys only**. The private key
  never enters the cluster.
- Keep the private key offline (paper, hardware token, password manager).
  Losing it means losing access to every backup it can decrypt.
- Multiple recipients are supported for key rotation and disaster recovery.
- The `UI_ALLOW_KEY_MUTATION` flag (default `false`) gates age-key
  add/remove via the UI — the most security-critical mutation, since a
  hostile add silently widens future-backup decryption.

## Pod Security

Both operator and worker pods run with a restricted SecurityContext:

- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- `capabilities: drop: [ALL]`
- `seccompProfile: RuntimeDefault`
- `runAsNonRoot: true` (UID 1000)

This passes Pod Security Admission in `restricted` mode.

## Credential Handling in Workers

Database credentials never appear on command lines:

| Database   | Mechanism                                    |
|------------|----------------------------------------------|
| PostgreSQL | `PGPASSWORD` environment variable            |
| MySQL      | `MYSQL_PWD` environment variable              |
| MongoDB    | `0600`-permission YAML config file (cleaned up via `defer`) |
| Redis      | `REDISCLI_AUTH` environment variable          |

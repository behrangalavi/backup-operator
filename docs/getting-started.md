# Getting Started

## 3. Quick Start

```bash
# 1. Generate an age key pair OFFLINE on your machine
age-keygen -o ~/age.key
# Keep ~/age.key secret. It is the ONLY way to recover backups.
# Public line in the file looks like:  age1qx...
# Private line:                         AGE-SECRET-KEY-1...

# 2. Install with the public recipient
helm install backup-operator ./charts/backup-operator -n backup --create-namespace \
  --set agePublicKeys[0]="age1qx...your-recipient-here"
# Multi-key rotation:
#   --set agePublicKeys[0]=age1qx...primary --set agePublicKeys[1]=age1yz...dr
# String form is also accepted:
#   --set agePublicKeys="age1qx..."  (single key only — CLI cannot pass a literal newline)

# 3. Declare a source: connection in the CR, password in a Secret
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: prod-users-credentials
stringData:
  password: <db password>
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupSource
metadata:
  name: prod-users          # also the target name in storage paths and metrics
spec:
  database:
    type: postgres
    host: pg.prod.svc
    port: 5432              # optional, defaults to 5432
    name: users
    username: backup
    credentialsSecretRef:
      name: prod-users-credentials
  schedule: "0 2 * * *"
EOF

# 4. Declare a destination (Hetzner Storage Box example)
kubectl -n backup apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: hetzner-credentials
stringData:
  ssh-private-key: |
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
---
apiVersion: backup.mogenius.io/v1alpha1
kind: BackupDestination
metadata:
  name: hetzner
spec:
  type: sftp
  pathPrefix: /cluster-prod
  sftp:
    host: u123.your-storagebox.de
    port: 23                # Hetzner Storage Box
    username: u123
    knownHosts: |           # ssh-keyscan output, recommended
      [u123.your-storagebox.de]:23 ssh-rsa AAAA...
  credentialsSecretRef:
    name: hetzner-credentials
EOF

# 5. Confirm the source is Ready and a CronJob was created
kubectl -n backup get backupsources
# NAME         TYPE       SCHEDULE    SUSPENDED   LAST RUN   LAST SUCCESS   VERIFIED   READY   AGE
# prod-users   postgres   37 2 * * *                                                  True    5s
kubectl -n backup get cronjobs
# NAME                 SCHEDULE      ...
# backup-prod-users    37 2 * * *

# 6. Trigger a manual run instead of waiting
kubectl -n backup create job --from=cronjob/backup-prod-users manual-$(date +%s)

# 7. Restore
backup-restore --destination hetzner -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h localhost prod_clone

# Migrating from the pre-CRD model (Secrets labelled backup.mogenius.io/role):
backup-restore migrate -n backup --age-secret backup-operator-age          # prints manifests
backup-restore migrate -n backup --age-secret backup-operator-age --apply  # creates them
```

### 3.1 Securing the UI with an Authentication Proxy

The built-in UI (`UI_ENABLED=true`) has **no authentication**. In production, place an authenticating reverse proxy in front of it. Common options:

**OAuth2 Proxy (recommended for SSO):**

```yaml
# values.yaml snippet — deploy as sidecar or separate Deployment
# pointing at the operator's UI port.
extraContainers:
  - name: oauth2-proxy
    image: quay.io/oauth2-proxy/oauth2-proxy:v7
    args:
      - --upstream=http://localhost:8081
      - --http-address=0.0.0.0:4180
      - --provider=oidc
      - --oidc-issuer-url=https://accounts.google.com  # or Keycloak, Azure AD, etc.
      - --email-domain=yourcompany.com
      - --cookie-secret=$(head -c 32 /dev/urandom | base64)
    ports:
      - containerPort: 4180
```

**Kubernetes Ingress with basic-auth:**

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: backup-ui
  annotations:
    nginx.ingress.kubernetes.io/auth-type: basic
    nginx.ingress.kubernetes.io/auth-secret: backup-ui-htpasswd
spec:
  rules:
    - host: backup.internal.yourcompany.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: backup-operator
                port:
                  number: 8081
```

The operator UI exposes a full management interface: CRUD for BackupSources, BackupDestinations and BackupRecipients (plus the credentials Secrets it creates for them), manual backup triggers, job status, and live SSE updates. Passwords and SSH keys are masked in API responses (`***`).

The UI server ships hardened response headers on every route (`ui/middleware_security.go`): a strict `Content-Security-Policy` (`default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and a minimal `Permissions-Policy`. The SPA has no inline event handlers (everything is `data-action` + delegated listeners), which is what makes `script-src 'self'` possible. Every request gets an `X-Request-Id` (passed through or generated) and a V(1) access log; when the auth proxy forwards an identity (`X-Forwarded-User`, `X-Forwarded-Email`, `X-Auth-Request-User/Email`) it is recorded, sanitised and length-capped, in the Kubernetes Events the UI emits (`… (by alice@example.com, request 7f3a…)`). That identity is an audit hint only — the operator never authorises on it; the proxy does. Authentication prevents unauthorized users from modifying backup configurations or browsing backup history.

---

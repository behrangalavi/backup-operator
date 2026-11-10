# Encryption Model

## 10. Encryption Model

```
Operator's machine (offline):
  age-keygen -o age.key       →   ~/age.key
                                  ├── public:  age1qx...
                                  └── private: AGE-SECRET-KEY-1...

Cluster (online):
  Helm install --set agePublicKeys[0]=age1qx...
   └── operator seeds one BackupRecipient per key at startup
        └── operator's RecipientReconciler watches BackupRecipients
             └── materialises a merged Secret <release>-age with
                 AGE_PUBLIC_KEYS = newline-joined recipient list
                  └── mounted into every worker pod via secretKeyRef

Worker pod runtime:
  cmd/worker reads AGE_PUBLIC_KEYS env
   └── crypto.NewFromPublicKeys parses recipients
        └── pipeline writes:  pg_dump | gzip | ageEncrypt(recipients...) | tempfile

Restore:
  backup-restore --age-key ~/age.key
   └── crypto.NewDecryptorFromKeys parses identities
        └── reads ciphertext from storage, decrypts to stdout
```

**Rules the code enforces:**

- Worker refuses to start without `AGE_PUBLIC_KEYS`. There is no plaintext-backup code path.
- The age recipient list is newline-separated → supports recipient rotation (multiple public keys can decrypt; you can rotate by adding a new recipient and later retiring the old).
- The restore CLI accepts the same multi-key format → matches `age-keygen -o`'s output.
- Storage backends never see plaintext bytes; they receive `*.sql.gz.age` ciphertext only.

**Rules you enforce operationally:**

- Keep the private key offline. The whole security model breaks if it ends up in the cluster.
- Back up the private key separately (paper, hardware token, password manager). Losing it means losing every backup it can decrypt.
- For multi-region recovery, distribute multiple public keys to the cluster; each region's operator keeps its own private key.

---

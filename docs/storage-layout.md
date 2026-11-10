# Storage Layout

## 11. Storage Layout

Every dump produces **two** objects per run:

```
<path-prefix>/<target>/<YYYY>/<MM>/<DD>/dump-<timestamp>.sql.gz.age   (encrypted)
<path-prefix>/<target>/<YYYY>/<MM>/<DD>/dump-<timestamp>.meta.json    (plaintext)
```

| Object | Contents | Why this format |
|---|---|---|
| `dump-<ts>.sql.gz.age` | gzipped DB dump, age-encrypted | The actual backup payload |
| `dump-<ts>.meta.json` | target name, db-type, encrypted size, SHA256, run start (`timestamp`), `completedAt` + `durationSeconds` (wall-clock duration) + `dumpDurationSeconds` (dump phase alone), full Stats, full analyzer Report, dump verification, restore verification, per-destination upload results (`destinations` array with name, storageType, status, error), per-destination retention sweep results (`retention` array with name, status, deletedDumps, deletedMetas, error — pre-upload sweep only) | Lets the next run compute diffs without restoring; lets humans audit without the private key; per-destination results enable multi-storage health monitoring; `durationSeconds` feeds the UI's Jobs progress-bar estimate (median over last N successful runs); `retention` feeds the operator's retention gauges and the `BackupRetentionFailing` alert |

**Timestamp format:** `20060102T150405Z` (Go reference time, ISO-like, lexically sortable).

**The meta file is intentionally unencrypted.** Anyone with read access to the bucket can see schema fingerprints and row counts, but never the data itself. If that's not acceptable for your environment, plan to encrypt the meta files in a follow-up — the trade-off is that automated diffing then needs the private key in the cluster.

---

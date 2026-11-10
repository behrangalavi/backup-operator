# Common Operations

## 15. Common Operations

### Trigger a manual run

```bash
kubectl -n backup create job --from=cronjob/backup-prod-users-db \
  manual-$(date +%s)
```

The Job runs the same worker code as a scheduled run; metrics, retention, fan-out all behave identically.

### Suspend a backup temporarily

Three equivalent ways:

```bash
# 1. spec.suspended on the BackupSource (canonical — survives CronJob recreation)
kubectl -n backup patch backupsource prod-users --type=merge -p '{"spec":{"suspended":true}}'

# 2. UI: Pause / Resume button on the source card
# 3. UI API: POST /api/sources/{name}/suspend  body: {"suspend": true}
```

The reconciler reads `spec.suspended` and writes `Spec.Suspend` on the managed CronJob. **Manual `kubectl patch cronjob ... suspend=true` is overridden on the next reconcile** — the `BackupSource` is the source of truth. Manual triggers (`kubectl create job --from=cronjob/...` and the UI's Run button) ignore Suspend, so paused sources can still be exercised on demand for restore drills. To resume: set `suspended: false` (or drop the field).

### Change the schedule

```bash
kubectl -n backup patch backupsource prod-users --type=merge -p '{"spec":{"schedule":"*/15 * * * *"}}'
```

The reconciler observes the change, patches the CronJob within seconds, and `status.materializedSchedule` shows what actually runs.

### Disable analyzer for one source

```bash
kubectl -n backup patch backupsource prod-users --type=merge -p '{"spec":{"analyzer":{"enabled":false}}}'
```

Useful when the backup user lacks `pg_stat_*` access.

### Route one DB to a single destination

```bash
kubectl -n backup patch backupsource prod-users --type=merge -p '{"spec":{"destinations":["hetzner-offsite"]}}'
```

Names are `BackupDestination` object names.

### See when it last worked

```bash
kubectl -n backup get backupsources
kubectl -n backup describe backupsource prod-users   # conditions + Events (BackupStarted/Completed/Failed, RetentionDelete)
kubectl -n backup get backupdestinations             # Reachable column
```

### Inspect a run's metadata without restoring

```bash
backup-restore --destination hetzner -n backup --target prod-users \
  --age-key ~/age.key --list
# Pick a timestamp, then fetch the meta file via your storage's CLI:
# (the meta is unencrypted; a normal s3/sftp client retrieves it directly)
```

### Restore to a fresh database

```bash
backup-restore --destination hetzner -n backup --target prod-users \
  --age-key ~/age.key --decompress | psql -h fresh-host new_database
```

---

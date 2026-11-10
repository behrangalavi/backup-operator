# 0059. UI CRUD via the Kubernetes API, not a database

**Status:** Accepted.

Source and destination configuration lives in `BackupSource` / `BackupDestination` objects (the discovery contract); the UI writes them directly using the operator's ServiceAccount, plus a credentials Secret it owns via OwnerReference. Adding a database for configuration would create a sync problem between the DB and the cluster. The UI only ever edits a credentials Secret that carries an OwnerReference to the CR — a Secret the user manages (say, the one CloudNativePG generated) is reported as a conflict instead of being overwritten from the backup dashboard.

# 0066. CRUD endpoints address typed kinds, so the "is this really a backup Secret" check is gone

**Status:** Accepted. Written under the labelled-Secret model; Secret/annotation names map to CRD fields as listed in [the mapping](README.md#pre-crd-names).

Under the Secret model every source/destination handler had to verify the role label before touching a Secret, because the operator's Secret CRUD could otherwise be steered at any Secret in the namespace. With `BackupSource` / `BackupDestination` the kind is the guard; Secret writes from the UI are limited to credentials Secrets that carry an OwnerReference to the CR being edited.

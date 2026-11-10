#!/usr/bin/env bash
# End-to-end backup → restore test on a throwaway kind cluster.
#
# Proves the one property unit tests cannot: a real BackupSource, reconciled
# by the real operator into a CronJob, produces a worker run whose encrypted
# artifact the real restore CLI can decrypt and load back into Postgres with
# the seeded row counts intact. See test/e2e/README.md.
#
# Env knobs:
#   E2E_WORKDIR       scratch dir (kubeconfig, age keys, logs); default mktemp
#   E2E_CLUSTER       kind cluster name                     (backup-e2e)
#   E2E_IMAGE         image tag to test                     (backup-operator:e2e)
#   E2E_SKIP_BUILD=1  use an already-built $E2E_IMAGE (CI builds it with cache)
#   E2E_KEEP=1        keep the cluster after the run for debugging
#   E2E_S3_PORT       host port for the MinIO port-forward  (19000)
#   E2E_HELM_ARGS     extra `helm install` flags, word-split (e.g. "--set ui.enabled=true")
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLUSTER="${E2E_CLUSTER:-backup-e2e}"
IMAGE="${E2E_IMAGE:-backup-operator:e2e}"
NS=backup-e2e
RELEASE=backup-operator
SOURCE=e2e-postgres
S3_PORT="${E2E_S3_PORT:-19000}"
WORKDIR="${E2E_WORKDIR:-$(mktemp -d "${TMPDIR:-/tmp}/backup-e2e.XXXXXX")}"
mkdir -p "$WORKDIR/logs"

# Isolated kubeconfig: kind, kubectl and helm only ever see this cluster,
# never ~/.kube/config or any other context.
export KUBECONFIG="$WORKDIR/kubeconfig"
PF_PID=""

log()  { printf '\n\033[1m==> %s\033[0m\n' "$*" >&2; }
fail() { printf '\n\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
k()    { kubectl -n "$NS" "$@"; }

collect_diagnostics() {
  [ -f "$KUBECONFIG" ] || return 0
  log "collecting diagnostics into $WORKDIR/logs"
  k get all,cronjobs,backupsources,backupdestinations,backuprecipients -o wide >"$WORKDIR/logs/resources.txt" 2>&1 || true
  k get backupsources,backupdestinations -o yaml >"$WORKDIR/logs/crs.yaml" 2>&1 || true
  k get events --sort-by=.lastTimestamp >"$WORKDIR/logs/events.txt" 2>&1 || true
  k logs deploy/$RELEASE --tail=-1 >"$WORKDIR/logs/operator.log" 2>&1 || true
  for p in $(k get pods -l job-name -o name 2>/dev/null); do
    k logs "$p" --all-containers >"$WORKDIR/logs/worker-${p#pod/}.log" 2>&1 || true
  done
  tail -n 40 "$WORKDIR/logs/operator.log" >&2 || true
  for f in "$WORKDIR"/logs/worker-*.log; do [ -f "$f" ] && { echo "--- $f" >&2; tail -n 40 "$f" >&2; }; done
  cat "$WORKDIR/logs/resources.txt" >&2 || true
}

cleanup() {
  local rc=$?
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  [ "$rc" -ne 0 ] && collect_diagnostics
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    log "keeping cluster $CLUSTER (KUBECONFIG=$KUBECONFIG)"
  else
    kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
  fi
  [ "$rc" -eq 0 ] && log "E2E PASSED" || log "E2E FAILED (rc=$rc, logs in $WORKDIR/logs)"
  exit "$rc"
}
trap cleanup EXIT

# wait_until <timeout-seconds> <description> <command...>
wait_until() {
  local timeout=$1 desc=$2; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -ge "$deadline" ] && fail "timed out after ${timeout}s waiting for: $desc"
    sleep 2
  done
}

condition() { # condition <kind/name> <type> → prints status
  k get "$1" -o jsonpath="{.status.conditions[?(@.type==\"$2\")].status}"
}
condition_is_true() { [ "$(condition "$1" "$2")" = "True" ]; }

psql_e2e() { k exec -i deploy/postgres -- psql -v ON_ERROR_STOP=1 -U e2e -At "$@"; }

age_keygen() {
  if command -v age-keygen >/dev/null 2>&1; then age-keygen -o "$1" 2>/dev/null
  else (cd "$ROOT/src" && go run filippo.io/age/cmd/age-keygen -o "$1" 2>/dev/null); fi
}

# --- 0. preconditions -------------------------------------------------------
for bin in docker kind kubectl helm go; do
  command -v "$bin" >/dev/null || fail "missing required tool: $bin"
done
log "workdir $WORKDIR"

# --- 1. cluster -------------------------------------------------------------
# Idempotent: a leftover cluster of the same name (E2E_KEEP, crashed run) is
# replaced so every run starts from nothing.
kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s

# --- 2. image + binaries ----------------------------------------------------
if [ "${E2E_SKIP_BUILD:-0}" != "1" ]; then
  log "building image $IMAGE"
  docker build -t "$IMAGE" "$ROOT"
fi
log "loading $IMAGE into kind"
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "building backup-restore CLI"
(cd "$ROOT/src" && CGO_ENABLED=0 go build -trimpath -o "$WORKDIR/backup-restore" ./cmd/restore)
RESTORE="$WORKDIR/backup-restore"

age_keygen "$WORKDIR/age.key"
age_keygen "$WORKDIR/wrong.key"
PUB=$(grep 'public key:' "$WORKDIR/age.key" | awk '{print $4}')
[ -n "$PUB" ] || fail "could not read age public key"

# --- 3. workloads -----------------------------------------------------------
kubectl create namespace "$NS"
log "deploying Postgres and MinIO"
k apply -f "$ROOT/test/e2e/manifests/10-postgres.yaml" -f "$ROOT/test/e2e/manifests/20-minio.yaml"

log "installing the chart"
helm install "$RELEASE" "$ROOT/charts/backup-operator" -n "$NS" \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set "agePublicKeys[0]=$PUB" \
  --set config.metricsRefreshIntervalSeconds=5 \
  --set config.runTimeoutSeconds=600 \
  --set workerBackoffLimit=0 \
  --set prometheusReleaseLabel="" \
  --set alerts.prometheusURL="" --set alerts.alertmanagerURL="" \
  ${E2E_HELM_ARGS:-} \
  --wait --timeout 180s

# Bootstrap must have seeded the chart's key into a BackupRecipient and the
# merged Secret; without it every worker exits with AGE_PUBLIC_KEYS unset.
recipient_seeded() { [ -n "$(k get secret backup-operator-age -o jsonpath='{.data.AGE_PUBLIC_KEYS}' 2>/dev/null)" ]; }
wait_until 60 "merged age Secret backup-operator-age populated from agePublicKeys" recipient_seeded

k rollout status deploy/postgres --timeout=180s
k rollout status deploy/minio --timeout=180s

# --- 4. seed ----------------------------------------------------------------
log "seeding Postgres"
psql_e2e -d e2edb <<'SQL'
CREATE TABLE users  (id serial PRIMARY KEY, email text NOT NULL UNIQUE);
CREATE TABLE orders (id serial PRIMARY KEY, user_id int NOT NULL REFERENCES users(id), amount numeric(10,2) NOT NULL);
INSERT INTO users (email) SELECT 'user' || g || '@example.invalid' FROM generate_series(1, 37) g;
INSERT INTO orders (user_id, amount) SELECT 1 + (g % 37), g * 1.25 FROM generate_series(1, 1234) g;
ANALYZE;
SQL
EXPECTED="users=37 orders=1234"
row_counts() { # row_counts <db>
  printf 'users=%s orders=%s' \
    "$(psql_e2e -d "$1" -c 'SELECT count(*) FROM users')" \
    "$(psql_e2e -d "$1" -c 'SELECT count(*) FROM orders')"
}
[ "$(row_counts e2edb)" = "$EXPECTED" ] || fail "seed produced $(row_counts e2edb)"

# --- 5. backup --------------------------------------------------------------
log "applying BackupSource + BackupDestinations"
sed "s/\${E2E_S3_PORT}/$S3_PORT/" "$ROOT/test/e2e/manifests/30-backup.yaml" | k apply -f -

wait_until 60 "BackupSource Ready" condition_is_true "backupsource/$SOURCE" Ready
wait_until 30 "CronJob backup-$SOURCE" k get "cronjob/backup-$SOURCE"

JOB="e2e-manual-$(date +%s)"
log "triggering job $JOB"
k create job --from="cronjob/backup-$SOURCE" "$JOB"

job_done() {
  local s f
  s=$(k get "job/$JOB" -o jsonpath='{.status.succeeded}')
  f=$(k get "job/$JOB" -o jsonpath='{.status.failed}')
  [ "${f:-0}" -gt 0 ] && { echo "job failed" >&2; return 2; }
  [ "${s:-0}" -ge 1 ]
}
deadline=$((SECONDS + 420))
while :; do
  set +e; job_done 2>/dev/null; rc=$?; set -e
  [ "$rc" -eq 0 ] && break
  [ "$rc" -eq 2 ] && fail "worker job $JOB failed"
  [ "$SECONDS" -ge "$deadline" ] && fail "worker job $JOB did not finish within 420s"
  sleep 3
done
k logs "job/$JOB" --all-containers >"$WORKDIR/logs/worker-$JOB.log" 2>&1 || true
log "worker job succeeded"

# The refresher rebuilds status from the uploaded meta.json on its tick.
wait_until 90 "BackupSource LastRunSucceeded=True" condition_is_true "backupsource/$SOURCE" LastRunSucceeded
wait_until 60 "BackupDestination minio Reachable=True" condition_is_true backupdestination/minio Reachable
LAST_STATUS=$(k get "backupsource/$SOURCE" -o jsonpath='{.status.lastRunStatus}')
SIZE=$(k get "backupsource/$SOURCE" -o jsonpath='{.status.lastDumpSizeBytes}')
log "status: lastRunStatus=$LAST_STATUS lastDumpSizeBytes=$SIZE"
[ "${SIZE:-0}" -gt 0 ] || fail "lastDumpSizeBytes not populated"

# --- 6. restore (out of cluster, like an operator's laptop) ------------------
log "port-forwarding MinIO to 127.0.0.1:$S3_PORT"
k port-forward svc/minio "$S3_PORT:9000" >"$WORKDIR/logs/port-forward.log" 2>&1 &
PF_PID=$!
wait_until 30 "port-forward" curl -sf "http://127.0.0.1:$S3_PORT/minio/health/ready"

log "restore --list"
"$RESTORE" --destination minio-host -n "$NS" --target "$SOURCE" --list | tee "$WORKDIR/logs/list.txt"
LISTED=$(grep -c 'minio-host' "$WORKDIR/logs/list.txt" || true)
[ "$LISTED" -eq 1 ] || fail "--list should show exactly one run, saw $LISTED"

log "restore with the WRONG key must fail and leave no output file"
set +e
"$RESTORE" --destination minio-host -n "$NS" --target "$SOURCE" \
  --age-key "$WORKDIR/wrong.key" -o "$WORKDIR/wrong.sql" 2>"$WORKDIR/logs/wrong-key.txt"
rc=$?
set -e
cat "$WORKDIR/logs/wrong-key.txt" >&2
[ "$rc" -ne 0 ] || fail "restore with a wrong key exited 0"
[ ! -e "$WORKDIR/wrong.sql" ] || fail "wrong-key restore left a partial output file"

log "restore into a fresh database"
psql_e2e -d e2edb -c 'CREATE DATABASE restored'
"$RESTORE" --destination minio-host -n "$NS" --target "$SOURCE" \
  --age-key "$WORKDIR/age.key" --decompress -o "$WORKDIR/restored.sql"
psql_e2e -q -d restored <"$WORKDIR/restored.sql" >/dev/null

GOT=$(row_counts restored)
log "row counts: seeded [$EXPECTED] restored [$GOT]"
[ "$GOT" = "$EXPECTED" ] || fail "restored row counts differ"
SUM_SRC=$(psql_e2e -d e2edb -c 'SELECT sum(amount) FROM orders')
SUM_DST=$(psql_e2e -d restored -c 'SELECT sum(amount) FROM orders')
[ "$SUM_SRC" = "$SUM_DST" ] || fail "orders.amount checksum differs ($SUM_SRC vs $SUM_DST)"

log "purge --dry-run must find the artifacts and delete nothing"
"$RESTORE" --destination minio-host -n "$NS" --target "$SOURCE" --purge --dry-run 2>&1 | tee "$WORKDIR/logs/purge.txt"
grep -q 'would be deleted' "$WORKDIR/logs/purge.txt" || fail "purge --dry-run reported nothing"
"$RESTORE" --destination minio-host -n "$NS" --target "$SOURCE" --list | grep -q minio-host \
  || fail "dry-run purge deleted artifacts"

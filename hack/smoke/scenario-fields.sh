#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Exercises the v2-shaped JsonnetSnippet fields end to end: spec.entryFile
# (custom entry point), spec.history (revision retention), spec.interval, the
# events.v1 emissions on status transitions, and spec.suspend pause/resume.
# Env: NS, NAME. Assumes jaas is deployed (webhook not required).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-fields-demo}"

log "grant the tenant SA the RBAC the operator needs to publish (impersonated)"
grant_tenant_publish_rbac "$NS"

log "apply snippet $NAME (entryFile + history + interval)"
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: dashboards/main.jsonnet
  files:
    dashboards/main.jsonnet: |
      { mode: "fields-demo", version: 1 }
  history: 3
  interval: 30s
EOF

wait_ready jsonnetsnippet "$NAME" "$NS"

log "verify status.history is populated"
ENTRIES="$(kubectl -n "$NS" get jsonnetsnippet "$NAME" -o jsonpath='{.status.history[*].revision}' | wc -w)"
log "history entries: $ENTRIES"
[ "$ENTRIES" -ge 1 ] || die "status.history is empty"

log "verify a Normal/Synced event was emitted"
for i in $(seq 1 30); do
  has_event "$NAME" "$NS" Normal Synced && { log "Normal/Synced event seen after $i polls"; break; }
  sleep 1
done
has_event "$NAME" "$NS" Normal Synced || { kubectl -n "$NS" get events --field-selector "involvedObject.name=$NAME"; die "no Normal/Synced event"; }

log "suspend the snippet"
kubectl -n "$NS" patch jsonnetsnippet "$NAME" --type=merge -p '{"spec":{"suspend":true}}'
wait_reason jsonnetsnippet "$NAME" "$NS" Suspended
has_event "$NAME" "$NS" Warning Suspended || { kubectl -n "$NS" get events --field-selector "involvedObject.name=$NAME"; die "no Warning/Suspended event"; }

log "resume the snippet — reconcile must re-run"
kubectl -n "$NS" patch jsonnetsnippet "$NAME" --type=merge -p '{"spec":{"suspend":false}}'
wait_ready jsonnetsnippet "$NAME" "$NS"

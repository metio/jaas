#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# High-availability path: with replicas.min=2 the operator runs two pods but only
# the Lease holder reconciles (leader election). Killing the leader must hand the
# Lease to the surviving replica (LeaderElectionReleaseOnCancel makes this fast),
# and a snippet applied after the handover must still go Ready — proving the new
# leader took over reconciliation. Env: NS, NAME, LEASE (the Lease name; the
# chart defaults it to "<release>-operator" → "jaas-operator"), JNS (jaas
# install namespace). Assumes jaas is deployed with replicas.min/max >= 2.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-ha-demo}"
JNS="${JNS:-jaas-system}"; LEASE="${LEASE:-jaas-operator}"

log "wait for 2 operator pods to be Ready"
for i in $(seq 1 60); do
  ready=$(kubectl -n "$JNS" get pods -l app.kubernetes.io/name=jaas \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
    | grep -c True || true)
  [ "$ready" -ge 2 ] && { log "$ready operator pods Ready after $i polls"; break; }
  sleep 5
done
[ "$ready" -ge 2 ] || { kubectl -n "$JNS" get pods -l app.kubernetes.io/name=jaas -o wide; die "fewer than 2 operator pods became Ready"; }

# lease_holder — echoes the current Lease holderIdentity (or "").
lease_holder() {
  kubectl -n "$JNS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true
}

log "wait for a leader to acquire the Lease"
HOLDER=""
for i in $(seq 1 60); do
  HOLDER="$(lease_holder)"
  [ -n "$HOLDER" ] && { log "Lease $LEASE held by $HOLDER after $i polls"; break; }
  sleep 2
done
[ -n "$HOLDER" ] || { kubectl -n "$JNS" get lease; die "no leader acquired the Lease"; }

# The holderIdentity is "<pod-name>_<uuid>"; the pod name is the prefix.
LEADER_POD="${HOLDER%%_*}"
log "current leader pod: $LEADER_POD"
kubectl -n "$JNS" get pod "$LEADER_POD" >/dev/null 2>&1 || die "leader pod $LEADER_POD not found"

log "delete the leader pod to force a handover"
kubectl -n "$JNS" delete pod "$LEADER_POD" --wait=false

log "wait for a NEW holder to be elected"
NEW=""
for i in $(seq 1 60); do
  NEW="$(lease_holder)"
  if [ -n "$NEW" ] && [ "$NEW" != "$HOLDER" ]; then
    log "Lease handed over to $NEW after $i polls"; break
  fi
  sleep 2
done
[ -n "$NEW" ] && [ "$NEW" != "$HOLDER" ] || { kubectl -n "$JNS" get lease "$LEASE" -o yaml; die "Lease was not handed to a new holder"; }

log "apply a snippet after the handover — the new leader must reconcile it"
grant_tenant_publish_rbac "$NS"
apply_retry <<EOF
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, mode: "ha-after-failover" }
EOF

wait_ready jsonnetsnippet "$NAME" "$NS" 90 2
log "snippet went Ready after leader failover — HA verified"

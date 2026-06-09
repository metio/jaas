#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Applies N inline-files snippets at once and asserts they all converge to
# Ready=True inside a generous window — catches obvious reconcile-throughput
# regressions (workqueue saturation under fan-out, leader-election thrashing, GC
# pressure from many parallel publishes). Env: NS, N. Assumes jaas is deployed.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; N="${N:-50}"

log "apply $N inline-files snippets"
for i in $(seq 1 "$N"); do
  cat <<EOF
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: scale-${i}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, idx: ${i} }
EOF
done | kubectl apply -f -

log "wait for every snippet to reach Ready=True"
# 50 snippets serialised through the workqueue at MaxConcurrentReconciles=1 take
# under a minute on kind; allow 4× headroom.
deadline=$(( $(date +%s) + 240 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  total=$(kubectl -n "$NS" get jsonnetsnippet -o name | wc -l)
  ready=$(kubectl -n "$NS" get jsonnetsnippet \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
    | grep -c True || true)
  log "ready ${ready}/${total}"
  if [ "$ready" = "$total" ] && [ "$total" = "$N" ]; then
    log "all $N snippets converged"
    exit 0
  fi
  sleep 5
done
log "convergence timeout — laggards:"
kubectl -n "$NS" get jsonnetsnippet \
  -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="Ready")].status!="True")]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].reason}{"\n"}{end}'
die "not all snippets converged within the window"

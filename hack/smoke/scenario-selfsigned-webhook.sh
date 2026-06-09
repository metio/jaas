#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Verifies the self-signed webhook path: the operator must patch a non-empty
# caBundle into its own ValidatingWebhookConfiguration, and admission must then
# succeed for a snippet apply. Env: NS, NAME, VWC (the VWC name, default derives
# from release name `jaas`). Assumes jaas was deployed with
# operator.webhook.certMode=self-signed.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-smoke-ss}"; VWC="${VWC:-jaas-jsonnetsnippet}"

log "wait for the operator to patch the VWC's caBundle"
CA=""
for i in $(seq 1 30); do
  CA="$(kubectl get validatingwebhookconfiguration "$VWC" \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)"
  if [ -n "$CA" ]; then
    log "caBundle populated after $i polls"
    echo "$CA" | base64 -d | openssl x509 -noout -subject -enddate || true
    break
  fi
  sleep 2
done
[ -n "$CA" ] || die "operator never patched the VWC caBundle"

log "apply a snippet — proves admission works against the self-signed cert"
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, mode: "self-signed" }
EOF

wait_ready jsonnetsnippet "$NAME" "$NS"

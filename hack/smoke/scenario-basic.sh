#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Basic operator path: an inline-files JsonnetSnippet must go Ready=True, publish
# an ExternalArtifact, and that tarball must be reachable in-cluster. Env: NS,
# NAME. Assumes jaas is deployed and Flux source-controller is present.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-smoke}"

log "grant the tenant SA the RBAC the operator needs to publish (impersonated)"
grant_tenant_publish_rbac "$NS"

log "apply snippet $NAME"
# Some deployments enable the admission webhook (cert-manager mode); retry the
# apply across the webhook server's startup window rather than failing on a
# transient refusal. Without a webhook the first attempt simply succeeds.
apply_retry <<EOF
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, n: 42 }
EOF

wait_ready jsonnetsnippet "$NAME" "$NS"
kubectl -n "$NS" describe jsonnetsnippet "$NAME"

URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL"
log "ExternalArtifact URL: $URL"
fetch_artifact "$URL"

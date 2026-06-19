#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# S3 storage-backend round-trip: an inline-files JsonnetSnippet must go
# Ready=True, the Publisher must write the tarball into the configured S3 bucket,
# and the operator's storage server must STREAM it back out — so a successful
# fetch of the ExternalArtifact URL proves the tarball round-tripped through the
# bucket (the storage server proxies the object out of S3, it does not serve a
# local file). This scenario is deliberately backend-agnostic: the workflow
# installs jaas pointed at any S3-compatible server (the smoke uses SeaweedFS —
# real SigV4 + multipart + SSE) and this same script asserts the round-trip.
# Env: NS, NAME. Assumes jaas is deployed with storage.backend=s3 and Flux
# source-controller is present.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-s3-demo}"

log "grant the tenant SA the RBAC the operator needs to publish (impersonated)"
grant_tenant_publish_rbac "$NS"

log "apply snippet $NAME"
# Some deployments enable the admission webhook; retry the apply across the
# webhook server's startup window rather than failing on a transient refusal.
apply_retry <<EOF
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, mode: "s3-backend", n: 42 }
EOF

wait_ready jsonnetsnippet "$NAME" "$NS"
kubectl -n "$NS" describe jsonnetsnippet "$NAME"

URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL"
log "ExternalArtifact URL (served by the storage server streaming from S3): $URL"
# A successful fetch proves the tarball was written to and read back from the
# bucket; assert the rendered marker is present in the streamed tarball.
fetch_artifact "$URL" "s3-backend"

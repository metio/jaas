#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# sourceRef chain: a Flux Bucket → ExternalArtifact → JsonnetSnippet pipeline.
# The sourceRef path is a genuinely different reconciler code path from inline
# spec.files — it walks status.artifact, downloads the tarball, untars under the
# tenant SA's RBAC, and detects upstream republishes via the Bucket watch.
# Requires setup-minio.sh to have run first (bucket `dashboards` populated).
# Env: NS, NAME. Assumes jaas + Flux source-controller are present.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; NAME="${NAME:-chain-demo}"

log "create the Bucket secret (Flux requires it in the Bucket's namespace)"
kubectl -n "$NS" create secret generic minio-bucket-creds \
  --from-literal=accesskey=jaas-smoke \
  --from-literal=secretkey=jaas-smoke-secret \
  --dry-run=client -o yaml | kubectl apply -f -

log "grant the tenant SA get on buckets (impersonated sourceRef lookup)"
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { namespace: ${NS}, name: jaas-tenant-sources }
rules:
  - apiGroups: ["source.toolkit.fluxcd.io"]
    resources: ["buckets"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { namespace: ${NS}, name: jaas-tenant-sources }
subjects:
  - kind: ServiceAccount
    name: default
    namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: jaas-tenant-sources
EOF

log "grant the tenant SA the RBAC the operator needs to publish (impersonated)"
grant_tenant_publish_rbac "$NS"

log "apply Bucket + snippet referencing it"
cat <<EOF | kubectl apply -f -
apiVersion: source.toolkit.fluxcd.io/v1
kind: Bucket
metadata: { name: dashboards, namespace: ${NS} }
spec:
  interval: 30s
  provider: generic
  bucketName: dashboards
  endpoint: minio.minio.svc:9000
  insecure: true
  secretRef:
    name: minio-bucket-creds
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: default
  entryFile: main.jsonnet
  sourceRef:
    kind: Bucket
    name: dashboards
EOF

log "wait for the Bucket to advertise an artifact"
for i in $(seq 1 60); do
  [ -n "$(kubectl -n "$NS" get bucket dashboards -o jsonpath='{.status.artifact.url}' 2>/dev/null || true)" ] \
    && { log "Bucket published its artifact after $i polls"; break; }
  sleep 2
done
[ -n "$(kubectl -n "$NS" get bucket dashboards -o jsonpath='{.status.artifact.url}')" ] || { kubectl -n "$NS" describe bucket dashboards; die "Bucket never published an artifact"; }

log "wait for the snippet to publish its own ExternalArtifact"
wait_ready jsonnetsnippet "$NAME" "$NS" 90 2

URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL"
log "verify the artifact content carries the source marker"
fetch_artifact "$URL" "sourceRef-bucket-chain"

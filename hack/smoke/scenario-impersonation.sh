#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Tenant ServiceAccount impersonation: the operator mints SA tokens
# (serviceaccounts/token: create, no `impersonate` verb on its own SA) and acts
# AS the snippet's spec.serviceAccountName, so the TENANT SA's RBAC — not the
# operator's own (privileged) identity — governs every apiserver call the
# reconcile makes. The publish step (upserting the snippet's ExternalArtifact)
# is the cleanest end-to-end probe:
#   - negative: a snippet bound to an SA with NO publish RBAC fails Ready=False
#     with reason RBACDenied, and NO ExternalArtifact is created — proving the
#     operator did not fall back to its own identity to write it;
#   - positive: granting the same SA the publish RBAC flips the snippet to
#     Ready=True and an ExternalArtifact appears — proving the impersonated
#     token now carries the verb.
# Env: NS, SA. Assumes jaas + Flux source-controller are present.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
NS="${NS:-default}"; SA="${SA:-imp-tenant}"
NAME="imp-demo"

cleanup() {
  kubectl -n "$NS" delete jsonnetsnippet "$NAME" --timeout=120s --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete rolebinding jaas-tenant-publish --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete role jaas-tenant-publish --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete serviceaccount "$SA" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "create a tenant ServiceAccount with NO RBAC bound to it"
kubectl -n "$NS" create serviceaccount "$SA" --dry-run=client -o yaml | kubectl apply -f -

log "apply a snippet bound to the un-privileged SA"
# The webhook (cert-manager mode) may still be coming up; retry the apply across
# its startup window. Without a webhook the first attempt simply succeeds.
apply_retry <<EOF
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: ${NAME}, namespace: ${NS} }
spec:
  serviceAccountName: ${SA}
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      { ok: true, tenant: "impersonated" }
EOF

log "negative: the impersonated SA cannot publish — Ready=False/RBACDenied"
wait_reason jsonnetsnippet "$NAME" "$NS" RBACDenied 60 5
test "$(ready_status jsonnetsnippet "$NAME" "$NS")" = "False" \
  || die "snippet $NAME should be Ready=False while the tenant SA lacks publish RBAC"
if kubectl -n "$NS" get externalartifact "$NAME" >/dev/null 2>&1; then
  die "an ExternalArtifact exists despite the tenant SA lacking RBAC — the operator did not impersonate the SA"
fi

log "grant the tenant SA the publish RBAC the operator needs (impersonated)"
grant_tenant_publish_rbac "$NS" "$SA"

log "positive: with the verb granted, the impersonated publish succeeds"
wait_ready jsonnetsnippet "$NAME" "$NS" 90 5
URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL after the grant"
log "ExternalArtifact URL: $URL"
fetch_artifact "$URL" "impersonated"

log "scenario-impersonation PASSED"

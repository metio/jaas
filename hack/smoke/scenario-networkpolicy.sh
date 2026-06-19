#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the chart-shipped NetworkPolicy applies cleanly and the operator
# keeps working with it in place. When networkPolicy.enabled=true the chart
# renders a NetworkPolicy in the install namespace that scopes the jaas pod's
# ingress to the jsonnet, management, storage, and (when enabled) webhook /
# metrics ports. This pins two contracts: (1) the policy object actually lands
# (it renders and the apiserver accepts it), and (2) with it in place a snippet
# still reconciles to Ready, publishes an ExternalArtifact, and that tarball is
# still reachable in-cluster on the storage port the policy admits.
#
# kind's default CNI (kindnet) does NOT enforce NetworkPolicy, so this is NOT an
# enforcement test — a deny would be a silent no-op there. The assertion is that
# the chart-managed policy applies cleanly and does not break the operator path;
# the per-engine enforcement dialects (cilium/calico/clusterNetworkPolicy) are
# rendered and unit-tested in the helm-charts repo.
#
# Env: JAAS_NS (the install namespace the policy is rendered into; default
# jaas-system), NS / NAME (the snippet's namespace / name).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"; . "$DIR/lib.sh"
JAAS_NS="${JAAS_NS:-jaas-system}"; NS="${NS:-default}"; NAME="${NAME:-np-smoke}"

log "the chart-shipped NetworkPolicy must exist in the install namespace ($JAAS_NS)"
# helm install is expected to have run with --set networkPolicy.enabled=true, so
# the chart-managed allowlist policy targeting the jaas pod is already present.
NP="$(kubectl -n "$JAAS_NS" get networkpolicy jaas \
  -o jsonpath='{.metadata.name}' 2>/dev/null || true)"
[ "$NP" = "jaas" ] || die "chart-shipped NetworkPolicy 'jaas' not found in $JAAS_NS (was networkPolicy.enabled=true set on install?)"

log "the policy must select the jaas pod and admit the storage port"
SEL="$(kubectl -n "$JAAS_NS" get networkpolicy jaas \
  -o jsonpath='{.spec.podSelector.matchLabels.app\.kubernetes\.io/name}' 2>/dev/null || true)"
[ "$SEL" = "jaas" ] || die "NetworkPolicy 'jaas' does not select the jaas pod (podSelector name=$SEL)"
PORTS="$(kubectl -n "$JAAS_NS" get networkpolicy jaas \
  -o jsonpath='{.spec.ingress[*].ports[*].port}' 2>/dev/null || true)"
log "NetworkPolicy 'jaas' ingress ports: $PORTS"
[ -n "$PORTS" ] || die "NetworkPolicy 'jaas' declares no ingress ports"

log "with the policy in place a snippet must still go Ready and publish"
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
      { networkPolicy: "applied", ok: true }
EOF

wait_ready jsonnetsnippet "$NAME" "$NS"
kubectl -n "$NS" describe jsonnetsnippet "$NAME"

log "the published artifact must still be reachable on the policy-admitted storage port"
URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL"
log "ExternalArtifact URL: $URL"
# Fetch from flux-system: the chart's NetworkPolicy admits the storage port
# (8082) only from that namespace (the Flux consumers source-controller /
# kustomize-controller / helm-controller live there). Fetching from the
# snippet's own namespace would exercise an ingress source the policy doesn't
# admit, so reachability must be checked from an admitted namespace.
fetch_artifact "$URL" '"networkPolicy": "applied"' flux-system

kubectl -n "$NS" delete jsonnetsnippet "$NAME" --timeout=120s || true
log "scenario-networkpolicy PASSED"

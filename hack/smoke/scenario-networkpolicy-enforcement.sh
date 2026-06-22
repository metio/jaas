#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the chart's NetworkPolicy is actually ENFORCED by a real CNI — what
# scenario-networkpolicy.sh cannot test, because kind's default kindnet treats
# NetworkPolicy as a no-op. The calling workflow installs an enforcing CNI
# (Calico / Cilium) and the chart with networkPolicy.enabled=true and
# networkPolicy.engine matching the CNI. It is deliberately engine-AGNOSTIC: it
# asserts traffic behaviour, not a specific policy object kind.
#
# It pins:
#   1. the operator still reconciles a snippet to Ready under the rendered
#      policy + real CNI (the dialect applies and does not break the operator);
#   2. ALLOW: the published artifact is reachable on the storage port from an
#      admitted namespace (flux-system) — the allowlist re-permits the real Flux
#      consumers under enforcement;
#   3. DENY (ENFORCE=1): the same artifact is NOT reachable from a non-admitted
#      namespace. The chart's kubernetes-engine allowlist scopes the storage port
#      to flux-system, so under a real CNI a fetch from the snippet's own
#      namespace is dropped. This is the true-negative the kindnet job can't make.
#
# ENFORCE=0 keeps only the allow + operator assertions. It is used where the
# rendered dialect is pod-scoped allow-all on the required ports (the chart's
# cilium / calico engines apply no per-source filter, so there is no per-source
# deny to assert), and for the clusterNetworkPolicy engine whose v1alpha2 API has
# no on-kind enforcer yet (the CRDs are installed so the objects apply and the
# operator path is exercised — apply-level validation).
#
# Env: JAAS_NS (install namespace; default jaas-system), NS / NAME (snippet
# namespace / name), ENFORCE (1 = also assert deny, 0 = allow + apply only).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=hack/smoke/lib.sh
. "$DIR/lib.sh"
JAAS_NS="${JAAS_NS:-jaas-system}"
NS="${NS:-default}"
NAME="${NAME:-np-enforce}"
ENFORCE="${ENFORCE:-1}"
log "install namespace: $JAAS_NS (engine policy is rendered there)"

log "the operator must reconcile a snippet with the policy in place under a real CNI"
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
      { networkPolicy: "enforced", ok: true }
EOF
wait_ready jsonnetsnippet "$NAME" "$NS"

log "ALLOW: the artifact must be reachable on the storage port from an admitted namespace (flux-system)"
URL="$(ea_url "$NAME" "$NS")"
[ -n "$URL" ] || die "snippet $NAME published no ExternalArtifact URL"
log "ExternalArtifact URL: $URL"
# Probe by the storage Service's ClusterIP, not its DNS name: this scenario
# asserts L3/L4 policy enforcement, so a probe-pod DNS failure (e.g. under the
# clusterNetworkPolicy engine's cluster-scoped baseline egress) must not be
# mistaken for a policy result.
PROBE_URL="$(cluster_ip_url "$URL")"
log "probing storage via ClusterIP: $PROBE_URL"
fetch_artifact "$PROBE_URL" '"networkPolicy": "enforced"' flux-system

if [ "$ENFORCE" = "1" ]; then
  log "DENY: the storage port must be BLOCKED from a non-admitted namespace ($NS)"
  fetch_artifact_denied "$PROBE_URL" "$NS"
else
  log "ENFORCE=0: skipping the deny assertion (dialect is allow-all on the ports, or has no on-kind enforcer)"
fi

kubectl -n "$NS" delete jsonnetsnippet "$NAME" --timeout=120s || true
log "scenario-networkpolicy-enforcement PASSED (ENFORCE=$ENFORCE)"

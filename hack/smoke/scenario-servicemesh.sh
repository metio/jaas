#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the chart's service-mesh authorization is actually ENFORCED, and
# enforced by WORKLOAD IDENTITY (ServiceAccount), which is what both meshes key
# on — Istio matches source.principals (the SPIFFE id cluster.local/ns/<ns>/sa/<sa>),
# Linkerd matches the MeshTLSAuthentication identity. To prove it is the identity
# and not merely the namespace, the allowed and denied clients run in the SAME
# mesh-injected namespace and differ only by ServiceAccount: the workflow scopes
# serviceMesh.storage.from to the ALLOW_SA's identity.
#
#   - ALLOW: a meshed client running as ALLOW_SA reaches the storage port;
#   - DENY: a meshed client in the same namespace running as the default SA is
#     rejected by the mesh (Istio 403; Linkerd resets → curl reports 000). A
#     2xx/404 from the denied client means the authz is NOT enforcing.
#
# It needs no Flux/artifact: the operator boots with its Flux watches gated when
# the ExternalArtifact CRD is absent, and the storage HTTP server still answers
# (404 for an unknown path), which is all the authz check requires. Clients are
# long-running meshed Deployments hit via `kubectl exec` (an injected sidecar
# never lets a one-shot pod complete).
#
# Env: JAAS_NS (install ns; default jaas-system), CLIENT_NS (meshed client ns;
# default mesh-clients), ALLOW_SA (authorized ServiceAccount; default mesh-reader),
# ENGINE (istio|linkerd). The denied client uses the namespace default SA.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=hack/smoke/lib.sh
. "$DIR/lib.sh"
JAAS_NS="${JAAS_NS:-jaas-system}"
CLIENT_NS="${CLIENT_NS:-mesh-clients}"
ALLOW_SA="${ALLOW_SA:-mesh-reader}"
ENGINE="${ENGINE:-istio}"
URL="http://jaas-storage.${JAAS_NS}:8082/"
log "engine=$ENGINE  storage URL=$URL  client ns=$CLIENT_NS  allowed SA=$ALLOW_SA  denied SA=default"

log "deploy two meshed curl clients in $CLIENT_NS — one as $ALLOW_SA (allowed), one as default (denied)"
deploy_meshed_curl "$CLIENT_NS" mesh-allow "$ALLOW_SA"
deploy_meshed_curl "$CLIENT_NS" mesh-deny default

log "ALLOW: the client running as $ALLOW_SA must reach the storage port (mesh authz admits its identity)"
allow_code="$(meshed_http_status "$CLIENT_NS" mesh-allow "$URL")"
log "allowed client got HTTP $allow_code"
case "$allow_code" in
  403 | 000) die "client as $ALLOW_SA was rejected (HTTP $allow_code) — mesh authz over-denied" ;;
esac

log "DENY: the same-namespace client running as the default SA must be rejected (identity, not namespace)"
deny_code="$(meshed_http_status "$CLIENT_NS" mesh-deny "$URL")"
log "denied client got HTTP $deny_code"
case "$deny_code" in
  403 | 000) log "denied client correctly rejected (HTTP $deny_code)" ;;
  *) die "client as default SA reached the storage port (HTTP $deny_code) — mesh authz is NOT enforcing by identity" ;;
esac

kubectl -n "$CLIENT_NS" delete deploy mesh-allow mesh-deny --timeout=60s || true
log "scenario-servicemesh PASSED (engine=$ENGINE)"

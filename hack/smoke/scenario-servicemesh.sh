#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the chart's service-mesh authorization is actually ENFORCED. The
# workflow installs a mesh (Istio / Linkerd), labels the install + client
# namespaces for sidecar injection, and deploys the chart with
# serviceMesh.enabled, serviceMesh.engine=<mesh>, and serviceMesh.storage.from
# scoped to the ALLOW_NS source. This asserts the mesh authz behaviour, engine-
# agnostically:
#   - ALLOW: a meshed client whose identity the policy admits reaches the storage
#     port (the app answers — 404 for "/" is fine, the point is the mesh let it
#     through);
#   - DENY: a meshed client the policy does not admit is rejected by the mesh
#     (Istio returns 403; Linkerd resets → curl reports 000). A 2xx/404 from the
#     denied client means the authz is NOT enforcing.
#
# It needs no Flux/artifact: the operator boots with its Flux watches gated when
# the ExternalArtifact CRD is absent, and the storage HTTP server still listens
# (404 for an unknown path), which is all the authz check requires.
#
# Env: JAAS_NS (install ns; default jaas-system), ALLOW_NS / DENY_NS (meshed
# client namespaces; default mesh-allowed / mesh-denied), ENGINE (istio|linkerd).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=hack/smoke/lib.sh
. "$DIR/lib.sh"
JAAS_NS="${JAAS_NS:-jaas-system}"
ALLOW_NS="${ALLOW_NS:-mesh-allowed}"
DENY_NS="${DENY_NS:-mesh-denied}"
ENGINE="${ENGINE:-istio}"
URL="http://jaas-storage.${JAAS_NS}:8082/"
log "engine=$ENGINE  storage URL=$URL  allow=$ALLOW_NS  deny=$DENY_NS"

log "deploy meshed curl clients in the allowed and denied namespaces"
deploy_meshed_curl "$ALLOW_NS" mesh-allow
deploy_meshed_curl "$DENY_NS" mesh-deny

log "ALLOW: a meshed client from $ALLOW_NS must reach the storage port"
allow_code="$(meshed_http_status "$ALLOW_NS" mesh-allow "$URL")"
log "allowed client got HTTP $allow_code"
case "$allow_code" in
  403 | 000) die "allowed client from $ALLOW_NS was rejected (HTTP $allow_code) — mesh authz over-denied" ;;
esac

log "DENY: a meshed client from $DENY_NS must be rejected by the mesh authz"
deny_code="$(meshed_http_status "$DENY_NS" mesh-deny "$URL")"
log "denied client got HTTP $deny_code"
case "$deny_code" in
  403 | 000) log "denied client correctly rejected (HTTP $deny_code)" ;;
  *) die "denied client from $DENY_NS reached the storage port (HTTP $deny_code) — mesh authz is NOT enforcing" ;;
esac

kubectl -n "$ALLOW_NS" delete deploy mesh-allow --timeout=60s || true
kubectl -n "$DENY_NS" delete deploy mesh-deny --timeout=60s || true
log "scenario-servicemesh PASSED (engine=$ENGINE)"

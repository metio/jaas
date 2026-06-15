#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Installs Flux's source-controller into the current kube-context. The operator
# depends on the `source.toolkit.fluxcd.io/v1` API and the `ExternalArtifact`
# kind, which ships in source-controller v1.7.0+ (Flux v2.7.0+) — earlier
# bundles have no ExternalArtifact CRD and the publish path fails with
# "no matches for kind ExternalArtifact". Usage: setup-flux.sh [flux-version]
set -euo pipefail
VERSION="${1:-v2.7.0}"
kubectl create namespace flux-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "https://github.com/fluxcd/flux2/releases/download/${VERSION}/install.yaml"
kubectl -n flux-system rollout status deploy/source-controller --timeout=240s

# Flux's default `allow-egress` NetworkPolicy admits ingress to source-controller
# only from pods inside flux-system, and `allow-scraping` opens only the metrics
# port (8080). The artifact HTTP server listens on 9090, so an operator running
# outside flux-system can't fetch artifacts once the CNI enforces NetworkPolicies
# (recent kindnet does; older kindnet treated them as no-ops). Open the artifact
# port cluster-wide so the operator — and any tenant namespace — can reach it.
kubectl apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-artifact-fetch
  namespace: flux-system
spec:
  podSelector:
    matchLabels:
      app: source-controller
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector: {}
      ports:
        - port: 9090
          protocol: TCP
EOF

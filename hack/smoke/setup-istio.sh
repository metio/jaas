#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install the Istio control plane for the service-mesh enforcement smoke. The
# chart's serviceMesh engine=istio renders security.istio.io AuthorizationPolicy
# (+ optional PeerAuthentication); this installs istiod via the official Helm
# charts (istio/base provides the CRDs, istio/istiod the control plane). Sidecar
# injection is left to per-namespace `istio-injection=enabled` labels the
# workflow/scenario sets BEFORE the workloads start. Usage:
#   setup-istio.sh [istio-version]
set -euo pipefail
VERSION="${1:-1.24.2}"

helm repo add istio https://istio-release.storage.googleapis.com/charts >/dev/null
helm repo update >/dev/null
kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install istio-base istio/base --version "${VERSION}" \
  --namespace istio-system --set defaultRevision=default --wait
helm upgrade --install istiod istio/istiod --version "${VERSION}" \
  --namespace istio-system --wait
kubectl -n istio-system rollout status deployment/istiod --timeout=300s

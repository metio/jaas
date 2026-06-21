#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install Linkerd for the service-mesh enforcement smoke. The chart's serviceMesh
# engine=linkerd renders policy.linkerd.io Server / AuthorizationPolicy /
# MeshTLSAuthentication objects. Installed via the Linkerd CLI, which
# auto-generates the mTLS trust anchor + issuer (the Helm install would require
# supplying them out of band). Sidecar injection is by the linkerd.io/inject
# annotation the workflow sets on the namespaces, not by this script.
#
# The CLI version is pinned (run.linkerd.io installs edge releases only now, a
# fast-moving target) for a reproducible smoke. Recent Linkerd builds its policy
# CRDs on the Gateway API, so those CRDs must be installed BEFORE
# `linkerd install --crds` — otherwise it refuses and emits nothing. Usage:
#   setup-linkerd.sh [linkerd-edge-version]
set -euo pipefail
VERSION="${1:-edge-26.6.3}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.2.1}"
# The installer (run via `| sh`) reads INSTALLROOT and LINKERD2_VERSION from the
# environment, so both are exported for the child shell to inherit. INSTALLROOT
# keeps the CLI self-contained under ~/.linkerd2/bin.
export INSTALLROOT="${HOME}/.linkerd2"
export LINKERD2_VERSION="${VERSION}"
curl --proto '=https' --tlsv1.2 -fsSL https://run.linkerd.io/install-edge | sh
export PATH="${INSTALLROOT}/bin:${PATH}"
linkerd version --client

# Linkerd's policy resources extend the Gateway API; its CRDs must exist first.
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
# Linkerd CRDs (split out since 2.12), then the control plane.
linkerd install --crds | kubectl apply -f -
linkerd install | kubectl apply -f -
linkerd check --wait=5m

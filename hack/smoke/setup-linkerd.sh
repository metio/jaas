#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install Linkerd for the service-mesh enforcement smoke. The chart's serviceMesh
# engine=linkerd renders policy.linkerd.io Server / AuthorizationPolicy /
# MeshTLSAuthentication objects. Installed via the Linkerd CLI, which
# auto-generates the mTLS trust anchor + issuer (the Helm install would require
# supplying them out of band). Sidecar injection is by the linkerd.io/inject
# annotation the workflow sets on the namespaces, not by this script. Usage:
#   setup-linkerd.sh [linkerd-version]   # e.g. edge-25.x.x / stable-2.x.x
set -euo pipefail
VERSION="${1:-}"
# The installer (run via `| sh`) reads INSTALLROOT and LINKERD2_VERSION from the
# environment, so both are exported for the child shell to inherit. INSTALLROOT
# keeps the CLI self-contained under ~/.linkerd2/bin.
export INSTALLROOT="${HOME}/.linkerd2"
if [ -n "${VERSION}" ]; then
  export LINKERD2_VERSION="${VERSION}"
fi
curl --proto '=https' --tlsv1.2 -fsSL https://run.linkerd.io/install | sh
export PATH="${INSTALLROOT}/bin:${PATH}"

linkerd version --client
# CRDs first (split out since Linkerd 2.12), then the control plane.
linkerd install --crds | kubectl apply -f -
linkerd install | kubectl apply -f -
linkerd check --wait=5m

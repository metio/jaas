#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install the SIG-Network "Network Policy API" CRDs (policy.networking.k8s.io),
# which define ClusterNetworkPolicy (v1alpha2) — the API the chart renders for
# networkPolicy.engine=clusterNetworkPolicy. Installing the CRDs lets the
# apiserver accept the chart's ClusterNetworkPolicy objects, so the engine's
# dialect is validated against the REAL schema (kubeconform only ignores it as a
# missing schema, and helm-unittest just renders it).
#
# This installs the API only, NOT an enforcer: on kind there is no controller
# that enforces v1alpha2 ClusterNetworkPolicy yet (the API merged ANP/BANP in
# late 2025 and is still pre-v1beta1). So engine=clusterNetworkPolicy is
# validated at APPLY level — objects accepted + the operator unaffected — not by
# traffic enforcement. The latest release is resolved at runtime so the CRD set
# tracks the evolving alpha API. Usage: setup-networkpolicy-api.sh [api-version]
set -euo pipefail
VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  VERSION="$(curl -fsSL https://api.github.com/repos/kubernetes-sigs/network-policy-api/releases/latest \
    | grep -oP '"tag_name":\s*"\K[^"]+')"
fi
[ -n "${VERSION}" ] || { echo "could not resolve a network-policy-api release version" >&2; exit 1; }
echo "installing network-policy-api CRDs ${VERSION}" >&2
kubectl apply -f "https://github.com/kubernetes-sigs/network-policy-api/releases/download/${VERSION}/install.yaml"

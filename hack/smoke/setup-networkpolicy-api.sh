#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install the SIG-Network "Network Policy API" ClusterNetworkPolicy CRD
# (policy.networking.k8s.io/v1alpha2) — the API the chart renders for
# networkPolicy.engine=clusterNetworkPolicy. Installing it lets the apiserver
# accept the chart's ClusterNetworkPolicy objects, so the dialect is validated
# against the REAL schema (kubeconform only ignores it as a missing schema, and
# helm-unittest just renders it).
#
# The CRD comes from the project's STANDARD channel (not experimental), for
# long-term stability: the standard channel serves v1alpha2 and is the set that
# graduates toward GA, so this tracks the supported shape. The releases ship no
# install.yaml, so the single channel CRD file is applied by raw URL at a pinned
# tag.
#
# This installs the API only, NOT an enforcer: on kind there is no controller
# that enforces v1alpha2 ClusterNetworkPolicy yet, so engine=clusterNetworkPolicy
# is validated at APPLY level — the objects are accepted and the operator is
# unaffected — not by traffic enforcement. Usage:
#   setup-networkpolicy-api.sh [api-version]
set -euo pipefail
VERSION="${1:-v0.2.0}"
echo "installing network-policy-api ClusterNetworkPolicy CRD (standard channel) ${VERSION}" >&2
kubectl apply -f "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/${VERSION}/config/crd/standard/policy.networking.k8s.io_clusternetworkpolicies.yaml"

#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install Cilium as the enforcing CNI on a disableDefaultCNI kind cluster (see
# kind-cilium.yaml). kind's built-in kindnet does NOT enforce NetworkPolicy;
# Cilium does — both the vanilla networking.k8s.io NetworkPolicy and its own
# CiliumNetworkPolicy dialect — so it backs the chart's `cilium` engine for the
# enforcement smoke. ipam.mode=kubernetes makes Cilium use the nodes' PodCIDRs
# (the kind default). Nodes only go Ready once the CNI is up, so this waits for
# that. Usage: setup-cilium.sh [cilium-chart-version]
set -euo pipefail
VERSION="${1:-1.16.5}"

helm repo add cilium https://helm.cilium.io >/dev/null
helm repo update >/dev/null
helm install cilium cilium/cilium --version "${VERSION}" --namespace kube-system \
  --set image.pullPolicy=IfNotPresent \
  --set ipam.mode=kubernetes
kubectl -n kube-system rollout status daemonset/cilium --timeout=300s
kubectl wait --for=condition=Ready nodes --all --timeout=300s

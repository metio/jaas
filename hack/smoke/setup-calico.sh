#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install Calico as the enforcing CNI on a disableDefaultCNI kind cluster (see
# kind-calico.yaml). kind's built-in kindnet does NOT enforce NetworkPolicy;
# Calico does — both the vanilla networking.k8s.io NetworkPolicy and its own
# projectcalico.org dialect — so it backs the chart's `kubernetes` and `calico`
# engines for the enforcement smoke. The manifest's default IP pool is
# 192.168.0.0/16, which matches kind-calico.yaml's podSubnet. Nodes only go
# Ready once the CNI is up, so this waits for that. Usage:
#   setup-calico.sh [calico-version]
set -euo pipefail
VERSION="${1:-v3.29.1}"

kubectl apply -f "https://raw.githubusercontent.com/projectcalico/calico/${VERSION}/manifests/calico.yaml"
kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s
kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=300s
kubectl wait --for=condition=Ready nodes --all --timeout=300s

#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Install Calico as the enforcing CNI on a disableDefaultCNI kind cluster (see
# kind-calico.yaml). kind's built-in kindnet does NOT enforce NetworkPolicy;
# Calico does. This uses the Tigera operator install (the recommended path)
# rather than the flat calico.yaml manifest, because the chart's `calico` engine
# renders projectcalico.org/v3 NetworkPolicy — the AGGREGATED Calico API, served
# by the calico-apiserver, which only the operator install (APIServer CR) brings
# up. The operator install also enforces vanilla networking.k8s.io NetworkPolicy,
# so it backs the chart's `kubernetes` engine too, and serves as the CNI for the
# clusterNetworkPolicy apply-level job. Usage: setup-calico.sh [calico-version]
set -euo pipefail
VERSION="${1:-v3.29.1}"

kubectl create -f "https://raw.githubusercontent.com/projectcalico/calico/${VERSION}/manifests/tigera-operator.yaml"
kubectl -n tigera-operator rollout status deployment/tigera-operator --timeout=180s

# Installation pins the IP pool to kind-calico.yaml's podSubnet; APIServer turns
# on the projectcalico.org/v3 aggregated API so engine=calico objects apply.
kubectl create -f - <<'EOF'
apiVersion: operator.tigera.io/v1
kind: Installation
metadata:
  name: default
spec:
  calicoNetwork:
    ipPools:
      - cidr: 192.168.0.0/16
        encapsulation: VXLANCrossSubnet
        natOutgoing: Enabled
---
apiVersion: operator.tigera.io/v1
kind: APIServer
metadata:
  name: default
spec: {}
EOF

# tigerastatus is the operator's aggregate health (calico-node + apiserver +
# ippools). It is created once the operator reconciles, so wait for the resource
# to appear before waiting on its Available condition.
for _ in $(seq 1 60); do
  kubectl get tigerastatus >/dev/null 2>&1 && break
  sleep 5
done
kubectl wait --for=condition=Available tigerastatus --all --timeout=420s
kubectl wait --for=condition=Ready nodes --all --timeout=420s

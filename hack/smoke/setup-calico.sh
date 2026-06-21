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

# Wait for the projectcalico.org/v3 aggregated API (calico-apiserver) to be
# served — exactly what engine=calico needs. `kubectl get
# networkpolicies.projectcalico.org` errors ("server doesn't have a resource
# type") until the calico-apiserver is registered and the aggregation layer is
# healthy, then exits 0 (empty list). This is more robust than waiting on
# tigerastatus, which the CRD makes list-able (exit 0) before the operator has
# created any instance — so a `kubectl wait --all` there races to "no matching
# resources found".
for _ in $(seq 1 120); do
  kubectl get networkpolicies.projectcalico.org -A >/dev/null 2>&1 && break
  sleep 5
done
kubectl get networkpolicies.projectcalico.org -A >/dev/null 2>&1 \
  || { echo "projectcalico.org/v3 API never became available" >&2; kubectl get tigerastatus >&2 || true; exit 1; }
# calico-node makes the nodes Ready once the CNI is up.
kubectl wait --for=condition=Ready nodes --all --timeout=420s

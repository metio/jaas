#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Installs Flux's source-controller into the current kube-context. The operator
# depends on the `source.toolkit.fluxcd.io/v1` API and the `ExternalArtifact`
# kind (landed in Flux v2.3.x). Usage: setup-flux.sh [flux-version]
set -euo pipefail
VERSION="${1:-v2.6.0}"
kubectl create namespace flux-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "https://github.com/fluxcd/flux2/releases/download/${VERSION}/install.yaml"
kubectl -n flux-system rollout status deploy/source-controller --timeout=240s

#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Installs cert-manager into the current kube-context. Version pinned; the
# webhook client-config wiring is API-stable, so multi-versioning adds little
# signal. Usage: setup-cert-manager.sh [version]
set -euo pipefail
VERSION="${1:-v1.16.2}"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${VERSION}/cert-manager.yaml"
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=240s

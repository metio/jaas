#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Creates the jaas-system namespace and a cert-manager self-signed Issuer named
# `jaas-selfsigned` for the webhook cert-manager smoke. Usage:
# setup-selfsigned-issuer.sh [namespace]
set -euo pipefail
NS="${1:-jaas-system}"
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata: { name: ${NS} }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: jaas-selfsigned
  namespace: ${NS}
spec:
  selfSigned: {}
EOF

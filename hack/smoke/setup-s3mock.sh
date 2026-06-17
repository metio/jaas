#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Deploys Adobe S3Mock into the `s3mock` namespace. S3Mock is an in-memory,
# zero-credentials S3 server: it accepts any (or no) signature, so it backs both
# the operator's S3 storage path (operator.storage.backend=s3 with anonymous
# creds) and the sourceRef-chain scenario's Flux Bucket. `initialBuckets` is
# created at startup; the `dashboards` object the chain scenario reads is written
# afterwards by a one-shot unsigned PUT (S3Mock honours unsigned requests).
set -euo pipefail
kubectl create namespace s3mock --dry-run=client -o yaml | kubectl apply -f -
cat <<'EOF' | kubectl -n s3mock apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: { name: s3mock }
spec:
  replicas: 1
  selector: { matchLabels: { app: s3mock } }
  template:
    metadata: { labels: { app: s3mock } }
    spec:
      containers:
        - name: s3mock
          image: docker.io/adobe/s3mock:4.7.0
          env:
            - { name: initialBuckets, value: "jaas-artifacts,dashboards" }
          ports: [{ containerPort: 9090 }]
          readinessProbe:
            tcpSocket: { port: 9090 }
            initialDelaySeconds: 2
            periodSeconds: 3
---
apiVersion: v1
kind: Service
metadata: { name: s3mock }
spec:
  selector: { app: s3mock }
  ports: [{ port: 9090, targetPort: 9090 }]
EOF
kubectl -n s3mock rollout status deploy/s3mock --timeout=180s

# Pre-populate the `dashboards` bucket's root object the chain scenario reads.
# S3Mock has no CLI, but it accepts unsigned PUTs, so a one-shot curl pod is
# enough — no AWS signing, no client tooling. The object body is the jsonnet the
# Flux Bucket serves as an artifact tarball.
kubectl -n s3mock run s3mock-seed \
  --image=docker.io/curlimages/curl:8.10.1 \
  --restart=Never --rm -i --quiet --command -- sh -ec '
    curl -fsS -X PUT \
      --data "{ ok: true, mode: \"sourceRef-bucket-chain\", n: 7 }" \
      http://s3mock.s3mock.svc:9090/dashboards/main.jsonnet
    echo "seeded dashboards/main.jsonnet"
  '

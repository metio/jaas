#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Deploys a single-replica MinIO into the `minio` namespace, creates a
# `dashboards` bucket, and uploads a `main.jsonnet` at its root. Backs the
# sourceRef-chain scenario: a Flux Bucket fetches this payload and serves it as
# an artifact tarball the snippet's sourceRef walks. The bucket lifecycle is
# "create, populate, leave alone" — single-replica is sufficient.
set -euo pipefail
kubectl create namespace minio --dry-run=client -o yaml | kubectl apply -f -
cat <<'EOF' | kubectl -n minio apply -f -
apiVersion: v1
kind: Secret
metadata: { name: minio-creds }
type: Opaque
stringData:
  accesskey: jaas-smoke
  secretkey: jaas-smoke-secret
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: minio }
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: docker.io/minio/minio:RELEASE.2024-10-29T16-01-48Z
          args: ["server", "/data", "--address", ":9000"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom: { secretKeyRef: { name: minio-creds, key: accesskey } }
            - name: MINIO_ROOT_PASSWORD
              valueFrom: { secretKeyRef: { name: minio-creds, key: secretkey } }
          ports: [{ containerPort: 9000 }]
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 9000, targetPort: 9000 }]
EOF
kubectl -n minio rollout status deploy/minio --timeout=180s

# One-shot Pod: create the bucket and write a tiny .jsonnet at its root. printf
# (not a heredoc) keeps the file body inline without column-0 terminator
# bookkeeping inside the YAML-indented shell string.
kubectl -n minio run mc-setup \
  --image=docker.io/minio/mc:RELEASE.2024-10-29T15-34-59Z \
  --restart=Never --rm -i --quiet --command -- /bin/sh -ec '
    mc alias set local http://minio:9000 jaas-smoke jaas-smoke-secret
    mc mb --ignore-existing local/dashboards
    printf "%s\n" "{ ok: true, mode: \"sourceRef-bucket-chain\", n: 7 }" > /tmp/main.jsonnet
    mc cp /tmp/main.jsonnet local/dashboards/main.jsonnet
    mc ls local/dashboards/
  '

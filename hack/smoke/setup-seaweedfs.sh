#!/usr/bin/env bash
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Deploys SeaweedFS's S3 gateway into the `seaweedfs` namespace as the
# real-store fidelity check for the operator's S3 storage backend. Unlike Adobe
# S3Mock (which accepts any signature and only fakes SSE), SeaweedFS validates
# AWS SigV4 against a static identity, does genuine multipart uploads, and
# applies real server-side encryption — so this is where the operator's
# minio-go credentialed/multipart/SSE path is exercised for real. The operator
# never creates buckets, so `jaas-artifacts` is created here explicitly via the
# S3 API before jaas is installed.
set -euo pipefail
kubectl create namespace seaweedfs --dry-run=client -o yaml | kubectl apply -f -

# Static S3 identity SeaweedFS validates SigV4 against. The same accessKey /
# secretKey are handed to the operator via a credentials Secret in the job, so
# the round-trip proves real signing — anonymous access is denied (no anonymous
# identity is configured below).
cat <<'EOF' | kubectl -n seaweedfs apply -f -
apiVersion: v1
kind: ConfigMap
metadata: { name: seaweedfs-s3-config }
data:
  s3.config.json: |
    {
      "identities": [
        {
          "name": "jaas",
          "credentials": [
            { "accessKey": "jaas-smoke", "secretKey": "jaas-smoke-secret" }
          ],
          "actions": ["Admin", "Read", "Write"]
        }
      ]
    }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: seaweedfs }
spec:
  replicas: 1
  selector: { matchLabels: { app: seaweedfs } }
  template:
    metadata: { labels: { app: seaweedfs } }
    spec:
      containers:
        - name: seaweedfs
          image: docker.io/chrislusf/seaweedfs:3.80
          args:
            - server
            - -dir=/data
            - -s3
            - -s3.port=8333
            - -s3.config=/etc/seaweedfs/s3.config.json
          ports: [{ containerPort: 8333 }]
          volumeMounts:
            - { name: config, mountPath: /etc/seaweedfs }
          readinessProbe:
            tcpSocket: { port: 8333 }
            initialDelaySeconds: 3
            periodSeconds: 3
      volumes:
        - name: config
          configMap: { name: seaweedfs-s3-config }
---
apiVersion: v1
kind: Service
metadata: { name: seaweedfs }
spec:
  selector: { app: seaweedfs }
  ports: [{ port: 8333, targetPort: 8333 }]
EOF
kubectl -n seaweedfs rollout status deploy/seaweedfs --timeout=180s

# Create the bucket the operator publishes into. SeaweedFS's S3 API rejects a
# PUT-bucket without a valid SigV4 signature, so a signing client is required —
# the awscli image points at the gateway with the static identity above. This
# also doubles as a smoke test that SigV4 against this identity works at all
# before jaas tries it.
kubectl -n seaweedfs run seaweedfs-mkbucket \
  --image=docker.io/amazon/aws-cli:2.18.13 \
  --restart=Never --rm -i --quiet \
  --env=AWS_ACCESS_KEY_ID=jaas-smoke \
  --env=AWS_SECRET_ACCESS_KEY=jaas-smoke-secret \
  --env=AWS_DEFAULT_REGION=us-east-1 \
  --command -- /bin/sh -ec '
    aws --endpoint-url http://seaweedfs.seaweedfs.svc:8333 \
      s3api create-bucket --bucket jaas-artifacts
    aws --endpoint-url http://seaweedfs.seaweedfs.svc:8333 s3 ls
    echo "created bucket jaas-artifacts"
  '

---
title: Synced
description: The snippet reconciled end-to-end and its ExternalArtifact is current; no action is required
tags: [runbooks, troubleshooting, lifecycle]
---

## Symptom

`kubectl get jsonnetsnippet` shows `READY=True` with `REASON=Synced`. This is the healthy state — no action required.

## Cause

The most recent reconcile pass completed end-to-end: source resolved, libraries resolved, eval succeeded, tarball published, ExternalArtifact upserted.

## Diagnosis

To inspect the published artifact:

```shell
kubectl --namespace <ns> get externalartifact <snippet-name> --output yaml
```

The `status.artifact.url` points at the operator's storage HTTP server. Curl it from a pod in the cluster to confirm the bytes match:

```shell
kubectl --namespace <ns> run --rm --stdin --tty --restart=Never tmp --image=docker.io/library/curlimages/curl -- \
  curl -sL <status.artifact.url> | tar tzv
```

## Remediation

None — this is the healthy state.

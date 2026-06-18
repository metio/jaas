---
title: SourceRefNotYetSupported
description: The snippet uses spec.sourceRef but the running binary was built without a Fetcher wired in
tags: [runbooks, troubleshooting, sources]
---

## Symptom

`READY=False`, `REASON=SourceRefNotYetSupported`.

## Cause

The snippet sets `spec.sourceRef` but the operator was built without a Fetcher wired in. This is a mis-deployment in practice — production binary always wires `sources.New()`. Seeing this in a real cluster means you're running:

- a test/dev binary
- a custom build where `defaultBuilder` was modified

## Diagnosis

```shell
kubectl --namespace <jaas-ns> logs deploy/jaas | grep -i "fetcher"
```

If the operator logs no Fetcher initialization, the binary is incomplete.

## Remediation

Use a release binary, or convert the snippet to `spec.files` inline as a temporary workaround.

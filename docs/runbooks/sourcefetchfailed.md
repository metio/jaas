---
title: SourceFetchFailed
description: The operator resolved the source CR but the artifact download failed due to an HTTP error, digest mismatch, or oversized tarball
tags: [runbooks, troubleshooting, sources]
---

## Symptom

`READY=False`, `REASON=SourceFetchFailed`. The Message describes what went wrong (HTTP error, digest mismatch, tarball too large, etc.).

## Cause

The Fetcher resolved the source CR and started downloading the artifact, but the download itself failed. Three subcategories:

- **HTTP failure** — connection refused, 5xx from the source-controller endpoint, TLS handshake error
- **Digest mismatch** — the bytes don't hash to `status.artifact.digest`. Possible truncation or in-flight tampering
- **Tarball oversized** — extracted bytes exceed `MaxArchiveBytes` (default 64 MiB)

## Diagnosis

Check the source CR's `status.artifact.url` is reachable from the operator pod:

```shell
kubectl --namespace <jaas-ns> exec deploy/jaas -- wget -O- <status.artifact.url> | wc -c
```

A connection refused means the storage endpoint of source-controller (or another publisher) is unreachable — usually a NetworkPolicy issue.

For digest mismatches, the source CR has likely been republished mid-fetch — the next reconcile typically succeeds.

For oversized tarballs, the snippet's `spec.sourceRef.path` filter is too broad — narrow it so only the files the snippet actually `import`s come through.

## Remediation

- **Network**: fix the NetworkPolicy / DNS / TLS that's blocking the fetch
- **Digest**: re-reconcile (manual: `kubectl --namespace <ns> annotate jsonnetsnippet <name> jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite`)
- **Oversized**: narrow `spec.sourceRef.path` to the subdirectory the snippet needs, or split the source repo

## A message ending in `Unauthorized`

A Message like `get JsonnetLibrary <ns>/<name>: Unauthorized` is authentication, not authorization, so it is not an RBAC problem — checking the tenant `ServiceAccount`'s `Role` will send you down the wrong path, and `kubectl auth can-i` answers yes throughout. Every tenant read runs on a short-lived TokenRequest token minted for `spec.serviceAccountName`, and that token is bound to the `ServiceAccount`'s UID. Deleting and recreating the `ServiceAccount` — which happens to every `ServiceAccount` in a namespace that is torn down and rebuilt — gives it the same name and a new UID, so a token minted before the rebuild authenticates as an object the apiserver no longer knows.

The operator handles this: a `401` evicts the cached credential, mints a fresh one, and retries the call once. A message that names the `ServiceAccount` and says the fresh token was refused too means the retry did not help, so look at the `ServiceAccount` itself:

```shell
kubectl --namespace <ns> get serviceaccount <sa-name>
```

A missing `ServiceAccount`, or a namespace in `Terminating`, is the answer. Recreate the `ServiceAccount` (or let the namespace finish terminating) and the next reconcile resolves normally.

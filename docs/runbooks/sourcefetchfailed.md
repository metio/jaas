<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: SourceFetchFailed

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
kubectl exec deploy/jaas -- wget -O- <status.artifact.url> | wc -c
```

A connection refused means the storage endpoint of source-controller (or another publisher) is unreachable — usually a NetworkPolicy issue.

For digest mismatches, the source CR has likely been republished mid-fetch — the next reconcile typically succeeds.

For oversized tarballs, the snippet's `spec.sourceRef.path` filter is too broad — narrow it so only the files the snippet actually `import`s come through.

## Remediation

- **Network**: fix the NetworkPolicy / DNS / TLS that's blocking the fetch
- **Digest**: re-reconcile (manual: `kubectl annotate jsonnetsnippet <name> jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite`)
- **Oversized**: narrow `spec.sourceRef.path` to the subdirectory the snippet needs, or split the source repo

---
title: ArtifactTooLarge
description: The snippet's rendered output exceeds the operator's per-artifact byte cap
tags: [runbooks, troubleshooting, storage]
---

## Symptom

`READY=False`, `REASON=ArtifactTooLarge`. The Message states the rendered byte count and the configured cap.

## Cause

The snippet's rendered output exceeds the operator's `--max-artifact-bytes` (Helm: `operator.storage.maxArtifactBytes`). The cap is a defense-in-depth control — one runaway snippet shouldn't fill a shared storage volume.

Common triggers:

- a snippet generating massive arrays via `std.range(n)` with a much larger `n` than intended
- accidentally inlining a large data fixture via `importstr`
- forgetting to project / filter when fanning out per-tenant configs

## Diagnosis

Check the rendered size locally:

```shell
jsonnet /tmp/snippet/<entry-file> | wc -c
```

## Remediation

Two paths:

1. **Shrink the output.** Inspect the snippet for unintended fan-out; project only the fields downstream consumers actually need.
2. **Raise the cap.** `--max-artifact-bytes=10485760` (10 MiB) gives more headroom. Pair with PVC sizing in the chart so the volume can hold N rev's worth of the new max.

If many snippets are bumping against the cap, the cap itself may be too low for the workload — review the cluster-wide ratio of total storage to per-snippet rev count.

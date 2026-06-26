---
title: Backup and disaster recovery
description: Back up the right thing and rebuild a JaaS install after cluster or storage loss — the artifact store is a regeneratable cache, your GitOps repository is the source of truth.
tags: [installation, backup, disaster-recovery, storage, gitops]
---

Back up your GitOps repository and rebuild a lost JaaS install from it. The
operator reconstructs every derived artifact from the `JsonnetSnippet` and
`JsonnetLibrary` resources you already keep under version control — so a full
recovery is "reinstall the chart, restore the resources, wait for one
reconcile loop".

## Recovery philosophy

JaaS is a renderer. On every reconcile it re-evaluates a snippet's spec and
re-publishes the result, so the bytes it writes to the artifact store are
**derived state**, not data you author. Two facts make recovery cheap:

- The artifact store — the filesystem volume behind `--storage-path`, or the
  S3 bucket behind `--storage-backend=s3` — is a **cache**. Losing it costs a
  re-render, not your work.
- `Publish` is **deterministic and idempotent**: the same snippet at the same
  revision renders to byte-identical tarball bytes and therefore the same
  `sha256` digest. Re-publishing after storage loss recreates the exact same
  artifact at the exact same URL.

The source of truth is your **GitOps repository** — the `JsonnetSnippet` and
`JsonnetLibrary` resources, plus the upstream Flux sources (`GitRepository`,
`OCIRepository`, `Bucket`) those snippets fetch from. Back that up and you can
rebuild everything else.

Backing up the artifact store is therefore **optional**. It buys one thing: a
shorter [re-render gap](#the-re-render-gap) on recovery, because consumers can
keep fetching the restored tarballs while the operator catches up. It is never
required for correctness.

## What to back up

| Component | Source of truth | Regeneratable? | Back up? |
|---|---|---|---|
| `JsonnetSnippet` / `JsonnetLibrary` resources | Your GitOps repository | No | **Yes** — this is the only required backup |
| Upstream Flux sources (`GitRepository`, `OCIRepository`, `Bucket`) | Your GitOps repository | No | **Yes** — same repository |
| Artifact tarballs (PVC or S3 bucket) | The snippet spec | Yes — re-rendered on reconcile | Optional — only shortens the recovery gap |
| `ExternalArtifact` resources | The Publisher | Yes — re-published every reconcile | No |
| `status.history` revisions | Incremental publishes | Yes — rebuilt as snippets re-render | No |
| Webhook serving CA (self-signed mode) | Generated in-pod | Yes — regenerated on startup | No |
| Leader-election lease | Ephemeral | Yes — re-elected on startup | No |

## Rebuild a cluster from scratch

Restore in this order. There is **no manual re-render step** — the operator
re-publishes automatically once the resources exist.

1. **Install the chart.**

   ```shell
   helm --namespace <jaas-ns> install jaas oci://ghcr.io/metio/helm-charts/jaas \
     --create-namespace \
     --set operator.enabled=true
   ```

2. **Restore the resources.** If Flux manages the cluster, point it at your
   GitOps repository and let it sync the `JsonnetSnippet` / `JsonnetLibrary`
   resources and their upstream sources back in. Without Flux, re-apply them
   from your backup:

   ```shell
   kubectl apply --filename <your-gitops-checkout>/
   ```

3. **(Optional) Restore the artifact store.** If you snapshot the PVC or
   replicate the S3 bucket, restore it now to skip the re-render gap. Skipping
   this step is safe — the operator repopulates the store from the specs.

4. **Wait for reconciliation.** The operator re-evaluates every snippet and
   re-publishes its tarball + `ExternalArtifact`. With the chart default
   (`replicas.max: 1`) the store is fully repopulated within one reconcile
   loop.

## The re-render gap

Between a storage loss and the first re-publish, an `ExternalArtifact`'s
`status.artifact.url` points at a tarball that is not yet on disk, so
downstream Flux consumers (kustomize-controller, helm-controller,
grafana-operator) see `404 Not Found`. The operator marks the artifact
not-ready until `Publish` runs again, and consumers gate on `Ready=True`, so
they retry with backoff and recover on their own once the re-render lands.

To shrink the gap:

- **Restore the store** (step 3 above) so consumers keep fetching while the
  operator catches up.
- **Force a reconcile** instead of waiting for the next watch tick:

  ```shell
  kubectl annotate --all-namespaces jsonnetsnippet --all \
    jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite
  ```

  Re-running this against a healthy store is safe — `Publish` is idempotent,
  so it rewrites byte-identical tarballs.

## Verify recovery

Confirm every snippet rendered and every artifact is fetchable.

```shell
# Every snippet should report Ready=True.
kubectl get jsonnetsnippet --all-namespaces

# Every ExternalArtifact should carry a populated artifact URL.
kubectl get externalartifact --all-namespaces \
  --output custom-columns=NS:.metadata.namespace,NAME:.metadata.name,URL:.status.artifact.url
```

Dereference an artifact URL to confirm the store serves bytes — port-forward
the storage Service and fetch one tarball:

```shell
kubectl --namespace <jaas-ns> port-forward svc/jaas-storage 8082:8082 &
curl -fsSL http://localhost:8082/<namespace>/<snippet>/<rev>.tar.gz | wc -c
```

A non-zero byte count and a `2xx` status mean the artifact is recovered and
downstream consumers can fetch it.

## Related pages

- [Storage backend recovery](/runbooks/storage-recovery/) — per-incident
  procedures for a degraded store (PVC lost, S3 outage, disk full, OOM,
  forced withdraw, HA lease handoff).
- [Metrics](/observability/metrics/) and [Alerting](/observability/alerting/)
  — signals that confirm a recovery completed and catch the next incident
  before it spreads.

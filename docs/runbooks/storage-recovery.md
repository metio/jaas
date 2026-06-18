---
title: Storage backend recovery
description: The artifact store is degraded (PVC lost, S3 endpoint down, or storage HTTP server unreachable) and downstream Flux consumers can no longer fetch tarballs
tags: [runbooks, troubleshooting, storage]
---

The artifact store itself is degraded — a PVC was lost, the S3 endpoint is unavailable, or the storage HTTP server is down. Downstream Flux consumers (kustomize-controller, helm-controller, grafana-operator) dereference `ExternalArtifact.status.artifact.url` to fetch tarballs; when that URL stops returning bytes, dependent resources stall. This state is not tied to a single status `Reason`.

## Symptom

One or more of:

- Downstream Flux consumers report `404 Not Found` or `connection refused` against the JaaS storage URL.
- `kubectl get externalartifact --all-namespaces` shows resources whose URL is unreachable from the consumer pods.
- The operator pod is healthy (Ready=True on snippets), but the storage Service is unresponsive.
- `helm upgrade` of the chart from `persistence.enabled: false` to `true` — or vice versa — caused a gap.

## Triage: which backend are you running?

```shell
kubectl --namespace <jaas-ns> get deploy jaas \
  --output jsonpath='{.spec.template.spec.containers[0].args}' \
  | tr ',' '\n' | grep -E 'storage-backend|storage-path|s3-endpoint'
```

- `--storage-backend=local` → filesystem behind `--storage-path`. Either an emptyDir (chart default) or a PVC.
- `--storage-backend=s3` → an external S3-compatible bucket; the storage HTTP server in-pod is a thin streaming proxy over `minio-go`.

## Filesystem backend

### PVC lost or replaced

Symptom: every `ExternalArtifact` URL returns 404 even though the snippet's Ready=True. The Publisher writes idempotently on every reconcile, so making the operator re-render every snippet is the fix:

```shell
# Roll the operator — the cache is rebuilt from the apiserver and every
# snippet is reconciled. Each reconcile re-runs the Publisher, which
# writes the tarball back to disk. With a clean PVC, the gap closes in
# one reconcile loop.
kubectl --namespace <jaas-ns> rollout restart deploy/jaas
```

If reconciles do not produce tarballs again, force a reconcile per snippet:

```shell
kubectl annotate --all-namespaces jsonnetsnippet --all \
  jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite
```

The window between PVC loss and the first re-render is the only outage downstream consumers see. With `replicas.max: 1` (chart default) that window is bounded by the rollout time; with multi-replica HA + RWX PVC, the lease-holder writes immediately and the gap is sub-second.

### emptyDir reset (pod restart)

`persistence.enabled: false` is fine for low-stakes deployments but every pod restart re-renders every snippet. The "fix" is to enable persistence:

```shell
helm --namespace <jaas-ns> upgrade jaas oci://ghcr.io/metio/helm-charts/jaas \
  --reuse-values \
  --set operator.storage.persistence.enabled=true \
  --set operator.storage.persistence.size=10Gi
```

After the upgrade, follow the PVC-lost steps above to repopulate the new volume.

### Storage HTTP server unreachable but operator healthy

Diagnose:

```shell
kubectl --namespace <jaas-ns> port-forward svc/jaas-storage <port>:8082 &
curl -fsSL http://localhost:<port>/<namespace>/<snippet>/<rev>.tar.gz | wc -c
```

If port-forward works but in-cluster fetches fail, look at NetworkPolicy:

```shell
kubectl get networkpolicy --all-namespaces | grep -i jaas
```

The chart's optional NetworkPolicy locks the storage port to a single source-controller selector. If your Flux install lives elsewhere or carries different labels, the NetworkPolicy will silently drop the traffic. Either widen `networkPolicy.fromSourceControllerSelector` or disable the NetworkPolicy on this chart and rely on a cluster-wide policy.

## S3 backend

### Endpoint unreachable / 5xx from the provider

The pod-side storage HTTP server is a proxy. When the upstream S3 endpoint is down, the proxy returns 502/504 and downstream Flux consumers retry with backoff. Operator pod health is unaffected.

Diagnose:

```shell
# Pull a recent tarball directly to confirm it's the upstream
kubectl --namespace <jaas-ns> exec deploy/jaas -- \
  wget -O- http://localhost:8082/<namespace>/<snippet>/<rev>.tar.gz | wc -c
```

If the in-pod fetch fails too, check the operator logs for `minio-go` errors. Auth problems (expired session token, rotated access key) show up here distinctly from network problems.

### Bucket gone or wrong prefix

If the bucket was emptied or the `--s3-prefix` changed, the proxy returns 404 even though the snippet is Ready. Re-render every snippet to repopulate:

```shell
kubectl annotate --all-namespaces jsonnetsnippet --all \
  jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite
```

The Publisher writes idempotently — running this against a working bucket is safe.

### Credentials rotated

With static credentials (`--s3-access-key` / `--s3-secret-key` / inline chart values), a rotation requires a Deployment restart for `minio-go` to pick up the new values. With IAM/IRSA, the discovery chain re-reads at request time — the operator picks the new identity up automatically.

Force the new keys to take effect:

```shell
kubectl --namespace <jaas-ns> rollout restart deploy/jaas
```

## Disk full (`ENOSPC`)

When the volume backing `--storage-path` fills up, `Store.Put` returns the kernel's `ENOSPC` verbatim → `Publisher.Publish` wraps it → no specific sentinel matches → classified as transient `ReasonSourceFetchFailed` → controller-runtime backoff retries forever at the ~16 min cap. The operator pod stays healthy; every snippet using local storage starts looping.

### Symptom

- Multiple snippets simultaneously flip to `Ready=False` with messages mentioning `no space left on device`.
- `JaaSControllerWorkqueueDepthHigh` alert fires (the backoff queue saturates).
- `kubectl --namespace <jaas-ns> exec deploy/jaas -- df -h /var/lib/jaas/artifacts` shows the volume at 100%.

### Recovery

1. **Free space.** Either resize the PVC (if `operator.storage.persistence.enabled: true`), increase `operator.storage.sizeLimit` (emptyDir), or prune retained revisions:

   ```shell
   # Lower spec.history on noisy snippets so the next reconcile prunes
   # older revisions. The Publisher's Prune step removes everything
   # outside the keep-set, freeing space proportional to the change.
   kubectl --namespace <ns> patch jsonnetsnippet <name> --type=merge --patch '{"spec":{"history":1}}'
   ```

   For an immediate flush, force-prune by removing the artifact directory of a snippet you're certain doesn't need its history:

   ```shell
   kubectl --namespace <jaas-ns> exec deploy/jaas -- \
       rm -rf /var/lib/jaas/artifacts/<namespace>/<expendable-snippet>
   ```

2. **Drive reconciliation.** The backoff cap is ~16 min, so failing snippets retry within that window automatically. To force immediate re-render:

   ```shell
   # Annotate every snippet that flipped to Ready=False:
   kubectl get jsonnetsnippets --all-namespaces \
     --output jsonpath='{range .items[?(@.status.conditions[?(@.type=="Ready")].status=="False")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' \
     | xargs -n1 -I {} sh -c 'ns=$(echo "{}" | cut -d/ -f1); n=$(echo "{}" | cut -d/ -f2); \
         kubectl --namespace "$ns" annotate jsonnetsnippet "$n" \
         jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite'
   ```

3. **Prevention.** Set `operator.storage.maxArtifactBytes` to cap individual snippet renders before they hit the disk. Use `JaaSSnippetArtifactGrowing` (opt-in PrometheusRule alert) to catch creep before saturation.

## OOM during render (kubelet kills the operator pod mid-publish)

A snippet that evaluates into a multi-MB JSON tree can push the operator past its `resources.memory` limit. The kubelet SIGKILLs the pod. Effects:

- The pod restarts cleanly. Probes flip back to Ready within seconds.
- The leader-election lease is dropped on the killed pod's process death; the next replica picks it up immediately (or, on single-replica installs, the new pod re-acquires).
- **Mid-write `.tmp` files** in the local store are orphaned because the `Store.Put` was interrupted between `Create` and the atomic `Rename`. The background `Sweep` (default every 10 min, configurable via `operator.storage.sweep.interval`) cleans them up once their `ModTime` falls outside the `maxTmpAge` window.
- **The snippet that triggered the OOM** keeps failing in the same way on the next reconcile because its rendered output is what blew memory in the first place. The pod loop-restarts until either the snippet is fixed or its memory cost falls below the limit.

### Symptom

- Operator pod restarts every few minutes; `kubectl describe pod` shows `Last State: Terminated, Reason: OOMKilled`.
- `JaaSOperatorPodDown` alert fires (the restart window is shorter than the recovery, so probes flap).
- One specific snippet correlates with each restart.

### Diagnose

The killing snippet is whichever the operator was reconciling when memory peaked. Easiest way to identify:

```shell
kubectl --namespace <jaas-ns> logs deploy/jaas --previous --tail=200 \
    | grep -B2 -i 'reconcil\|publish' | tail -30
```

The last `Reconcile` log line before the kill names the snippet. Confirm via `jaas_snippet_rendered_bytes` if the operator's metrics endpoint was reachable before the kill (the histogram captures bytes per Synced reconcile; a runaway snippet stands out).

### Remediate

1. **Cap the runaway snippet.** Set `operator.storage.maxArtifactBytes` cluster-wide to refuse renders past a threshold (e.g., `16777216` for 16 MiB). The Publisher fails the snippet with `ReasonArtifactTooLarge` instead of attempting the write. The operator pod stops OOM-restarting.

2. **Raise operator memory.** The chart's `resources.memory` default is conservative (`64Mi`); a cluster with large rendered artifacts may need `256Mi` or more. Update via:

   ```shell
   helm --namespace <jaas-ns> upgrade jaas oci://ghcr.io/metio/helm-charts/jaas \
     --reuse-values --set resources.memory=256Mi
   ```

3. **Wait for `Sweep`.** The orphan `.tar.gz.tmp` files clear automatically once the surrounding issue is fixed and `maxTmpAge` (default 30 min) elapses. `jaas_storage_sweep_failures_total` flags any persistent issue.

For S3 backends, OOM during a multipart upload leaves an incomplete upload at the S3 endpoint — most providers expire these automatically (AWS S3: 7-day default). No JaaS-side action needed.

## Multi-replica considerations

With leader election on (the chart default when operator mode is enabled), only the lease-holder writes to storage. A storage-incident on the lease-holder is the worst case: the standby reads but cannot fill the gap until the lease transfers. To force a handover during a storage incident on one replica:

```shell
kubectl --namespace <jaas-ns> delete lease <release-name>-operator
```

The next replica acquires the lease within `LeaseDuration` (15s default), and its Publisher writes against its own (presumably healthy) view of the backend.

## Prevention

- Use `persistence.enabled: true` in production. Default-off is for quick demos.
- Run the chart's opt-in PrometheusRule (`operator.metrics.prometheusRule.enabled: true`) — `JaaSSnippetArtifactGrowing` catches runaway tarballs before they fill the PVC.
- Set `operator.storage.maxArtifactBytes` to cap pathological snippets at admission time, not after they've written to disk.
- For S3, configure a bucket lifecycle policy that does not delete tarballs the operator still considers live. The Publisher's `Prune` only deletes revisions the snippet's `status.history` no longer references.

## `JaaSStorageSweepFailures` alert

Linked from the alert by name. The sweep is a background GC that removes orphaned `.tar.gz.tmp` residue left by Puts whose process died after the tmpfile landed but before the rename. The reconcile hot path is unaffected — Put still works — but stale `.tmp` files accumulate until the underlying issue is fixed.

**Symptom:** `jaas_storage_sweep_failures_total` increases over time; `JaaSStorageSweepFailures` alert fires after >3 failures/hour (configurable).

**Diagnose:**

```shell
# Operator logs carry the underlying sweep error:
kubectl --namespace <jaas-ns> logs deploy/jaas --tail=200 | grep "Storage sweep failed"

# For local backend: check the volume's free space + permissions.
kubectl --namespace <jaas-ns> exec deploy/jaas -- df -h /var/lib/jaas/artifacts
kubectl --namespace <jaas-ns> exec deploy/jaas -- ls -la /var/lib/jaas/artifacts

# For S3 backend: sweep is a no-op (Put is atomic, no .tmp residue),
# so this alert firing on S3 is a wiring bug. Confirm backend:
kubectl --namespace <jaas-ns> get deploy jaas --output jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep storage-backend
```

**Remediate:**

- Disk full → increase the PVC size or shrink `operator.storage.maxArtifactBytes`.
- Permission errors → ensure the operator's `securityContext.runAsUser` matches the PVC's filesystem ownership; reset with `chown` on a one-shot Job.
- Sustained S3 listing throttling → unlikely on the local backend; for S3 this alert shouldn't fire at all because Sweep is a no-op there.

Manual cleanup once the underlying issue is fixed:

```shell
kubectl --namespace <jaas-ns> exec deploy/jaas -- find /var/lib/jaas/artifacts -name '*.tar.gz.tmp' -mmin +30 -delete
```

## `WithdrawForced` event on snippet deletion

If a snippet stuck in `Terminating` carries a `Warning WithdrawForced` Kubernetes Event, the operator has already done what it could — the finalizer was dropped after `--max-withdraw-wait` (default 1h) of failing Withdraws against the backend, and the snippet itself is GC'd. The tarball it owned is now orphaned in storage. To clean up:

```shell
# Read the elapsed time + last backend error from the event message:
kubectl --namespace <ns> describe jsonnetsnippet <name>
# Locate the orphan in the configured backend:
#   local:  <--storage-path>/<namespace>/<name>/<rev>.tar.gz
#   s3:     <--s3-prefix>/<namespace>/<name>/<rev>.tar.gz
# Remove it once the backend is reachable again.
```

A force-drop should be rare — it means the backend was broken for the full wait window. Investigate **why** before lowering `maxWithdrawWait`: aggressive timeouts make transient apiserver/S3 incidents cause orphans you'd otherwise have recovered from naturally.

## Related runbooks

- [artifacttoolarge](/runbooks/artifacttoolarge/) — one snippet's output exceeds the cap (different symptom: snippet Ready=False, not "URL unreachable")
- [sourcefetchfailed](/runbooks/sourcefetchfailed/) — the operator *consuming* an upstream artifact, not its own storage

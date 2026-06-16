---
title: Storage and high availability
description: The local and S3 artifact backends, leader election, multi-replica HA, revision retention, and the orphan-tmp sweep.
tags: [operator, storage, ha]
---

In [operator mode](/usage/operator-mode/) JaaS renders each `JsonnetSnippet` into
a tar.gz artifact, stores it, and publishes an `ExternalArtifact` CR that points a
Flux consumer at the tarball over HTTP. JaaS publishes artifacts through one of two
storage backends — local filesystem or S3-compatible object storage — with optional
leader election for multi-replica high availability and configurable revision
retention.

## Serving the tarballs

Regardless of backend, the operator runs an HTTP server that Flux consumers fetch
artifacts from. Three flags govern it, and `--storage-base-url` and
`--storage-path` are required whenever `--enable-flux-integration` is set:

- `--storage-base-url` — the public URL prefix stamped into each
  `ExternalArtifact`'s `status.artifact.url`. This is what downstream Flux
  controllers dial, so it must be reachable from them.
- `--storage-listen-address` (default `0.0.0.0`) and `--storage-port` (default
  `8082`) — the bind address of the storage HTTP server.

## Local backend

`--storage-backend=local` (the default) writes tarballs to the filesystem under
`--storage-path`. The Helm chart pairs this with an `emptyDir` by default, or a
`PersistentVolumeClaim` when `operator.storage.persistence.enabled: true`.

A ReadWriteOnce PVC caps the install at a single replica, because only one pod can
mount the volume for writing. If you need more than one replica, use the S3
backend below.

## S3 backend

`--storage-backend=s3` stores tarballs in any S3-compatible bucket (AWS S3, MinIO,
Ceph RGW, Backblaze B2, and similar). The bucket must already exist. Configure it
with:

| Flag | Purpose |
|---|---|
| `--s3-endpoint` | S3 host:port, e.g. `s3.amazonaws.com` or `minio.minio.svc:9000`. Required. |
| `--s3-bucket` | Bucket the artifacts live in. Required. |
| `--s3-prefix` | Optional key prefix so JaaS can coexist with other tenants in one bucket. |
| `--s3-region` | Region the bucket lives in. Required for AWS multi-region; ignored by most other servers. |
| `--s3-use-ssl` | Talk HTTPS to the endpoint (default `true`). Set `false` only for local MinIO over HTTP. |
| `--s3-access-key` | Static access key ID. |
| `--s3-secret-key` | Static secret access key, paired with `--s3-access-key`. |
| `--s3-session-token` | Optional session token for temporary credentials. |
| `--s3-anonymous` | Skip request signing entirely; only for a public bucket, test and dev only. |

Leave `--s3-access-key` and `--s3-secret-key` empty to engage the IAM/IRSA
discovery chain — environment credentials, web-identity tokens, and EC2/EKS
instance metadata — so a pod running with an IRSA-annotated ServiceAccount needs
no static keys.

## Leader election

Leader election is on by default in operator mode (`--leader-election`, honored
only when `--enable-flux-integration` is set). The lease lets exactly one replica
reconcile at a time. On `SIGTERM` during a rolling update the lease is released
immediately rather than waiting out the 15-second lease duration, so the next
replica picks up reconciliation within seconds.

Set `--leader-election=false` only when running a single replica with no rollout
overlap.

## Multi-replica HA

High availability is the S3 backend plus leader election: every replica reads from
the same bucket, and only the lease-holder writes. No ReadWriteMany storage class
is required. During a rolling update the lease hands over on `SIGTERM`, so the
write path moves to the new leader without a manual step.

## Revision retention and rollback

`spec.history` on a `JsonnetSnippet` (default `1`, maximum `50`) keeps the last N
rendered revisions in storage. Downstream consumers can pin to an older `sha256`
for rollback or blue-green cutover, instead of always tracking the newest render.

Two flags shape how superseded revisions age out:

- `--artifact-gc-grace` (default `5m`) retains a revision for a short window after
  it leaves the keep-set. This closes the pin→fetch race in which a Flux consumer
  reads `status.artifact` a moment before the operator garbage-collects the
  revision that consumer pinned. Set it to `0` to disable the grace and restore
  eager pruning. Snippet teardown (the deletion path) is unaffected by this flag.
- `--max-artifact-bytes` (default `0`, disabled) caps the rendered artifact size in
  bytes. A snippet whose render exceeds the cap fails with `ReasonArtifactTooLarge`
  rather than publishing an oversized tarball.

## Orphan-tmp sweep

A `Put` that dies after writing the temporary file but before the atomic rename
leaves a `<rev>.tar.gz.tmp` residue. A background sweep removes it:

- `--storage-sweep-interval` (default `10m`) — how often the sweep runs. `0`
  disables it.
- `--storage-sweep-max-tmp-age` (default `30m`) — the minimum age before an
  orphaned `.tmp` file is eligible, set wider than the longest plausible in-flight
  `Put` so the sweep never races a live writer.

For production sizing of these knobs, see the
[production guide](/installation/production/). The full flag list with defaults is
on the [configuration page](/installation/configuration/).

---
title: Creating source artifacts
description: Step-by-step recipes to prepare GitRepository, OCIRepository, and Bucket sources for a JsonnetSnippet — including the single-layer rule for OCI.
tags: [operator, sources, oci, flux]
---

A `JsonnetSnippet`'s `spec.sourceRef` consumes a Flux source. The operator reads
the referenced source CR's `status.artifact.url`, downloads the tarball Flux's
source-controller serves there, verifies its `status.artifact.digest`, and
extracts it into the snippet's file tree. Every supported kind —
`GitRepository`, `OCIRepository`, and `Bucket` — reaches the operator through
that same `status.artifact` contract, so the operator never talks to a git
remote, an OCI registry, or an object store directly. Flux owns that fetch; the
operator consumes the artifact Flux already produced.

The recipes below show how to produce each source kind so a snippet can reference
it. [Snippet sources](/usage/snippet-sources/) covers wiring the finished source
into a `JsonnetSnippet`. For the source CRDs themselves and their full field
reference, see the [Flux documentation](https://fluxcd.io/).

A `JsonnetSnippet` references the source you create with a `spec.sourceRef`:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: dashboards
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  entryFile: dashboards/api-latency.jsonnet
  sourceRef:
    kind: GitRepository      # or OCIRepository, or Bucket
    name: dashboards-source
    path: dashboards/        # optional: narrow extraction to a subtree
```

The tenant ServiceAccount needs `get` on the referenced source kind. See
[Tenancy and RBAC](/usage/tenancy-and-rbac/) for the exact verbs.

## GitRepository

A `GitRepository` source tracks a branch, tag, or commit of a git repository.
source-controller clones the ref and packs the tree into the tarball the
operator fetches. There is no packaging or layer constraint — the operator
extracts whatever files the commit contains.

1. Lay out your Jsonnet files in a directory. File names and the directory
   structure carry over verbatim into the snippet's file tree, so place the
   entry file where `spec.entryFile` expects it:

   ```text
   dashboards/
   ├── api-latency.jsonnet
   ├── error-budget.jsonnet
   └── lib/
       └── panels.libsonnet
   ```

2. Commit the files and push them to a git repository:

   ```shell
   git add dashboards/
   git commit -m "Add Grafana dashboards"
   git push origin main
   ```

3. Create the `GitRepository` source. With the Flux CLI:

   ```shell
   flux create source git dashboards-source \
     --url=https://github.com/example-org/grafana-dashboards \
     --branch=main \
     --interval=5m \
     --namespace=default \
     --export
   ```

   The equivalent CR YAML, which is authoritative:

   ```yaml
   apiVersion: source.toolkit.fluxcd.io/v1
   kind: GitRepository
   metadata:
     name: dashboards-source
     namespace: default
   spec:
     interval: 5m
     url: https://github.com/example-org/grafana-dashboards
     ref:
       branch: main
   ```

4. Point a snippet's `spec.sourceRef` at the source. Set `kind: GitRepository`,
   `name: dashboards-source`, and optionally `path:` to extract only a subtree:

   ```yaml
   apiVersion: jaas.metio.wtf/v1
   kind: JsonnetSnippet
   metadata:
     name: api-latency-dashboard
     namespace: default
   spec:
     serviceAccountName: dashboards-tenant
     entryFile: dashboards/api-latency.jsonnet
     sourceRef:
       kind: GitRepository
       name: dashboards-source
       path: dashboards/
   ```

When a new commit lands on the tracked branch, source-controller republishes the
artifact and the operator's watch re-renders the snippet.

## OCIRepository

An `OCIRepository` source pulls an OCI artifact from a registry. source-controller
unpacks the artifact's single gzipped-tar layer into the tarball the operator
fetches. Producing the artifact with `flux push artifact` packs a directory into
exactly that shape.

1. Lay out your Jsonnet files in a directory, the same as for a git source:

   ```text
   ./
   ├── main.jsonnet
   └── lib/
       └── panels.libsonnet
   ```

2. Push the directory as an OCI artifact with the Flux CLI. `flux push artifact`
   packs the directory into one gzipped-tar layer and pushes it to the registry:

   ```shell
   flux push artifact oci://ghcr.io/example-org/dashboards:v1 \
     --path=. \
     --source="$(git config --get remote.origin.url)" \
     --revision="$(git rev-parse HEAD)"
   ```

   `--source` and `--revision` stamp provenance metadata onto the artifact;
   set them to a URL and a version identifier of your choosing.

3. Create the `OCIRepository` source. With the Flux CLI:

   ```shell
   flux create source oci dashboards-source \
     --url=oci://ghcr.io/example-org/dashboards \
     --tag=v1 \
     --interval=5m \
     --namespace=default \
     --export
   ```

   The equivalent CR YAML, which is authoritative:

   ```yaml
   apiVersion: source.toolkit.fluxcd.io/v1
   kind: OCIRepository
   metadata:
     name: dashboards-source
     namespace: default
   spec:
     interval: 5m
     url: oci://ghcr.io/example-org/dashboards
     ref:
       tag: v1
   ```

4. Point a snippet's `spec.sourceRef` at the source with `kind: OCIRepository`:

   ```yaml
   apiVersion: jaas.metio.wtf/v1
   kind: JsonnetSnippet
   metadata:
     name: api-latency-dashboard
     namespace: default
   spec:
     serviceAccountName: dashboards-tenant
     entryFile: main.jsonnet
     sourceRef:
       kind: OCIRepository
       name: dashboards-source
   ```

> **Single layer is mandatory.** `flux push artifact` produces an OCI artifact
> with exactly one gzipped-tar layer, which is what source-controller expects
> and the only shape it unpacks. An artifact built any other way — a hand-rolled
> `oras push` with one file per layer, a `Dockerfile`/container-image build, or
> any tool that splits content across multiple layers — is not consumed
> correctly. source-controller cannot reconstruct the file tree, the snippet's
> source never resolves, and the snippet reports `Ready=False`. Always build OCI
> sources with `flux push artifact`.

Verify the layer count before relying on an artifact. Fetch the manifest and
confirm the `layers` array has length 1:

```shell
oras manifest fetch oci://ghcr.io/example-org/dashboards:v1 | \
  jq '.layers | length'
```

A result of `1` is required. Any other number means the artifact was not built
with `flux push artifact` and will not resolve.

### Private registries and Amazon ECR

source-controller performs the pull, so registry credentials belong on the
`OCIRepository` (or on source-controller itself) — never on the JaaS operator or a
snippet's ServiceAccount. The same applies to a `JsonnetLibrary` whose `sourceRef`
points at an `OCIRepository`.

For a generic private registry, add a `spec.secretRef` to a `docker-registry`
Secret. For **Amazon ECR you need no pull Secret at all**: set `spec.provider: aws`
and source-controller authenticates with its own ambient AWS identity. On EKS that
is an IRSA role bound to **source-controller's** ServiceAccount with ECR read
permissions:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: dashboards-source
  namespace: default
spec:
  interval: 5m
  provider: aws
  url: oci://111122223333.dkr.ecr.eu-west-1.amazonaws.com/dashboards
  ref:
    tag: v1
```

The IRSA role needs `ecr:GetAuthorizationToken` (resource `*`) plus
`ecr:BatchGetImage` and `ecr:GetDownloadUrlForLayer` on the repository. Because the
credential is source-controller's, one role covers every `OCIRepository` it pulls,
and the JaaS operator stays out of the registry path entirely.

This IRSA role is source-controller's, not the JaaS operator's. The JaaS operator
uses IRSA only for its own [S3 storage backend](/usage/storage-and-ha/) — a
separate concern from pulling sources.

There is a third way to load OCI content that does not go through a `sourceRef` at
all: the chart can mount snippets and libraries from **OCI image volumes**
(`snippets` / `additionalLibraries`), read straight from a registry into the pod.
Those volumes are pulled by the **kubelet**, exactly like a container image — so
they authenticate the way images do, not through IRSA. On EKS that means the
**node's** IAM role with ECR read (the `AmazonEC2ContainerRegistryReadOnly`
managed policy the default node role already carries), or an `imagePullSecret` on
the pod. Pod-level IRSA grants the *pod's* ServiceAccount AWS API access, which the
kubelet does not use when pulling images, so it is not the mechanism for this path.
With a node role that can read ECR, image-volume snippets and libraries load with
no pull Secret. Static OCI mounts and operator mode are mutually exclusive in one
release, so a given install uses either these mounts or the `sourceRef` path above,
not both.

The [jsonnet-oci-images (JOI)](https://github.com/metio/jsonnet-oci-images)
project enforces this same single-layer rule for every image it publishes, so
its images are ready-made single-layer `OCIRepository` sources. Reference a JOI
image directly when you need a shared Jsonnet library tree (grafonnet, the
jsonnet-libs catalog) rather than building and maintaining your own OCI source.

## Bucket

A `Bucket` source mirrors objects from an S3- or GCS-compatible bucket.
source-controller fetches the matching objects, packs them into the tarball the
operator fetches, and there is no layer constraint — the only requirement is
that the objects laid out under the bucket prefix form the file tree your snippet
expects.

1. Produce the files to upload. Either upload the individual `.jsonnet` /
   `.libsonnet` files under a prefix, or pack them into a single archive — both
   work, source-controller flattens the mirrored objects into the file tree:

   ```text
   dashboards/
   ├── main.jsonnet
   └── lib/
       └── panels.libsonnet
   ```

2. Upload the files to the bucket under a prefix. With the AWS CLI against an
   S3-compatible endpoint:

   ```shell
   aws s3 cp dashboards/ s3://example-bucket/dashboards/ \
     --recursive \
     --endpoint-url=https://s3.example.com
   ```

3. Create the `Bucket` source. With the Flux CLI:

   ```shell
   flux create source bucket dashboards-source \
     --bucket-name=example-bucket \
     --endpoint=s3.example.com \
     --provider=generic \
     --secret-ref=bucket-credentials \
     --interval=5m \
     --namespace=default \
     --export
   ```

   The equivalent CR YAML, which is authoritative:

   ```yaml
   apiVersion: source.toolkit.fluxcd.io/v1
   kind: Bucket
   metadata:
     name: dashboards-source
     namespace: default
   spec:
     interval: 5m
     provider: generic
     bucketName: example-bucket
     endpoint: s3.example.com
     secretRef:
       name: bucket-credentials
   ```

   The referenced Secret carries the bucket credentials (`accesskey` /
   `secretkey`). See the [Flux documentation](https://fluxcd.io/) for the Secret
   layout and provider-specific fields.

4. Point a snippet's `spec.sourceRef` at the source with `kind: Bucket`. Use
   `path:` to extract only the prefix that holds your Jsonnet:

   ```yaml
   apiVersion: jaas.metio.wtf/v1
   kind: JsonnetSnippet
   metadata:
     name: api-latency-dashboard
     namespace: default
   spec:
     serviceAccountName: dashboards-tenant
     entryFile: main.jsonnet
     sourceRef:
       kind: Bucket
       name: dashboards-source
       path: dashboards/
   ```

## Which source should I use?

| Source          | Use when                                                                                              |
|-----------------|-------------------------------------------------------------------------------------------------------|
| `GitRepository` | Your Jsonnet is human-authored configuration living in a version-controlled git repository.           |
| `OCIRepository` | You want an immutable, content-addressed artifact; must be a single layer, and pairs with JOI images. |
| `Bucket`        | Your artifacts already live in S3- or GCS-compatible object storage.                                  |

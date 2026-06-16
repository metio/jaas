---
title: Operator mode
description: Boot JaaS as a Kubernetes operator that evaluates JsonnetSnippet CRs and publishes the results as Flux ExternalArtifacts.
tags: [operator, flux]
---

JaaS runs as a Kubernetes operator alongside its HTTP renderer. In this mode it
watches custom resources, evaluates the Jsonnet they describe, and publishes the
result as a Flux [`ExternalArtifact`](https://fluxcd.io/) that downstream
controllers consume. The HTTP renderer keeps running; the operator is an
additional set of goroutines, not a separate binary.

## Enabling the operator

Set `--enable-flux-integration` on the binary:

```shell
jaas --enable-flux-integration \
  --storage-path=/var/lib/jaas/artifacts \
  --storage-base-url=http://jaas-storage.jaas.svc:8082
```

`--storage-path` and `--storage-base-url` are required in operator mode — they
tell the operator where to write artifact tarballs and the public URL prefix
downstream consumers fetch them from.

With the [Helm chart](/installation/) set `operator.enabled: true`:

```yaml
operator:
  enabled: true
```

The chart wires the storage paths, leader election, RBAC, and the metrics
Service for you.

## The two custom resources

The operator watches two CRDs in the `jaas.metio.wtf/v1` API group. Both are
namespaced.

| Kind             | Scope      | Purpose                                                                          |
|------------------|------------|----------------------------------------------------------------------------------|
| `JsonnetSnippet` | Namespaced | A Jsonnet snippet to evaluate and publish as an `ExternalArtifact`.              |
| `JsonnetLibrary` | Namespaced | Reusable `.libsonnet` files that snippets in the same namespace import.          |

A `JsonnetSnippet` is the published unit. A `JsonnetLibrary` carries no artifact
of its own — it exists to be imported by snippets. The full field reference for
each lives at [/api/jsonnetsnippet/](/api/jsonnetsnippet/); the library CRD is
covered in [Jsonnet libraries](/usage/jsonnet-libraries/).

## What the operator produces

Each reconcile of a `JsonnetSnippet` evaluates the snippet and writes the result
into a tar.gz, then upserts a Flux `ExternalArtifact` CR whose
`status.artifact.url` points at the operator's storage HTTP server. In the
default `rendered` output mode the archive holds a single `rendered.json` — the
evaluated JSON. The published artifact's URL is also mirrored onto the snippet's
own `status.artifactURL`, so `kubectl describe jsonnetsnippet` answers "where is
my rendered output?" without a second lookup.

Any controller that understands Flux's `ExternalArtifact` reads the result by
pointing a `sourceRef` at it:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: consume-rendered
  namespace: default
spec:
  sourceRef:
    kind: ExternalArtifact
    name: hello-world
```

Real consumers of the published artifact include:

- [grafana-operator](https://grafana.github.io/grafana-operator/) — renders
  Grafana dashboards from the evaluated JSON.
- [stageset-controller](https://stageset.projects.metio.wtf/) — drives a staged
  rollout of the rendered manifests.
- Flux's own `kustomize-controller` and `helm-controller`, which apply the
  rendered output as part of a GitOps pipeline.

## A minimal snippet

The simplest `JsonnetSnippet` carries its Jsonnet inline in `spec.files` and
seeds two external variables:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: hello-world-tenant
  namespace: default
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: hello-world
  namespace: default
spec:
  serviceAccountName: hello-world-tenant
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      {
        greeting: 'hello',
        recipient: std.extVar('audience'),
        timestamp: std.extVar('now'),
      }
  externalVariables:
    audience: world
    now: "2026-06-09T12:00:00Z"
```

`spec.serviceAccountName` names the ServiceAccount the operator impersonates for
every API call this snippet drives — the artifact write, source fetches, library
reads. That ServiceAccount's RBAC, not the operator's, governs what the snippet
can reach. See [Tenancy and RBAC](/usage/tenancy-and-rbac/) for the verbs the
tenant ServiceAccount needs.

## Lifecycle knobs

Two `spec` fields control when and whether the operator reconciles a snippet.
Both mirror Flux's source-controller conventions.

### `spec.suspend`

Set `spec.suspend: true` to pause reconciliation without deleting the snippet.
The operator skips the evaluation pipeline, leaves the existing
`ExternalArtifact` in place, and reports `Ready=False` with reason `Suspended`.
Setting it back to `false` resumes reconciliation. The published artifact stays
available the whole time, so downstream consumers keep reading the last rendered
output while the snippet is paused.

```yaml
spec:
  suspend: true
```

### `spec.interval`

Set `spec.interval` to re-render the snippet on a fixed cadence even when no
watch event fires:

```yaml
spec:
  interval: 10m
```

A `JsonnetSnippet` re-renders whenever its source, libraries, or referenced Flux
sources change. `spec.interval` adds a steady-state cadence on top of that, so
the snippet picks up state outside the watched graph — external-variable
environment drift on the operator pod, OCI library refreshes, and similar. The
interval is bounded at admission to between `30s` and `24h`. Failed reconciles
still use controller-runtime's exponential backoff; the interval governs only
the steady-state cadence.

## Where to go next

- [Snippet sources](/usage/snippet-sources/) — inline files, a Flux `sourceRef`,
  multi-snippet trees, and chaining one snippet's output into another.
- [Jsonnet libraries](/usage/jsonnet-libraries/) — the `JsonnetLibrary` CRD,
  OCI-mounted shared libraries, and how imports resolve.
- [Tenancy and RBAC](/usage/tenancy-and-rbac/) — per-snippet impersonation and
  the tenant ServiceAccount's permissions.
- [Storage and HA](/usage/storage-and-ha/) — the local and S3 backends, leader
  election, and revision retention.
- [/api/jsonnetsnippet/](/api/jsonnetsnippet/) — the exhaustive field-by-field
  reference.

---
title: Network policy
description: The opt-in NetworkPolicy the chart ships — pod-scoped allowlists vs. a namespace-wide default-deny, choosing a policy engine, the ingress and egress traffic JaaS needs, and how to tighten each port.
tags: [network-policy, security, networking]
---

The Helm chart ships an opt-in `NetworkPolicy` for the JaaS pod. It is off by
default and renders only when `networkPolicy.enabled` is `true`. Two independent
layers are on offer: pod-scoped allowlists that lock down only JaaS's own pods
(the safe default), and an additional namespace-wide default-deny for a zero-trust
namespace. The ingress and egress tables below describe exactly what traffic JaaS
depends on — in both renderer mode and [operator mode](/usage/operator-mode/) — so
everything else can be denied.

## Two layers: pod-scoped allowlists vs. namespace default-deny

`networkPolicy.enabled: true` renders per-workload, **pod-scoped allowlist**
policies. They select only JaaS's own pods through their `app.kubernetes.io/*`
labels and lock down just those pods to the required ports. This is the safe
default and is fine in a shared namespace: co-located workloads — including
anything in `flux-system` if JaaS shares that namespace — are untouched.

`networkPolicy.defaultDeny.enabled` (default `false`) **additionally** renders a
namespace-wide default-deny so every pod in the namespace is denied by default and
the allowlists become the only exceptions (a zero-trust namespace). The default-deny
sits at a lower precedence than the allowlists, so the allowlists always win for the
JaaS pods while everything else is denied.

Pick the layer that matches namespace ownership:

- **`defaultDeny.enabled: false`** (default) — pod-scoped setup. Only JaaS's pods
  are locked down; neighbours keep whatever posture their own policies give them.
- **`defaultDeny.enabled: true`** — namespace zero-trust. Enable this **only when
  JaaS owns its namespace**, because the deny-all also denies every co-located
  workload that does not have its own allowing policy.

`defaultDeny.order` (default `2000`) tunes the Calico `order` / ClusterNetworkPolicy
`priority` that keeps the deny-all subordinate to the allowlists. The `kubernetes`
and `cilium` engines have no precedence knob — deny and allow combine additively and
allow wins — so the value matters only for the `calico` and `clusterNetworkPolicy`
engines.

```yaml
networkPolicy:
  enabled: true
  defaultDeny:
    enabled: true   # only when JaaS owns this namespace
    order: 2000
```

## Choosing a policy engine

`networkPolicy.engine` selects which policy dialect the chart renders. It is
explicit, not auto-detected: a chart that sniffed the running CNI would render
different objects on different clusters from identical values, which breaks GitOps
determinism. You name the engine, and the rendered manifest is the same everywhere.

| `engine` | Renders | API | FQDN egress |
| --- | --- | --- | --- |
| `kubernetes` (default) | `NetworkPolicy` | `networking.k8s.io/v1` | No |
| `cilium` | `CiliumNetworkPolicy` | `cilium.io/v2` | Yes — free `toFQDNs` egress |
| `calico` | `NetworkPolicy` | `projectcalico.org/v3` | No — OSS Calico has no FQDN egress; that is Calico Enterprise only |
| `clusterNetworkPolicy` | `ClusterNetworkPolicy` | `policy.networking.k8s.io/v1alpha2` | No |

`clusterNetworkPolicy` renders the SIG-Network `ClusterNetworkPolicy` that
consolidates the deprecated `AdminNetworkPolicy` + `BaselineAdminNetworkPolicy`
APIs into one resource. It is alpha, cluster-scoped, and rendered in the `Baseline`
tier so a developer-authored `NetworkPolicy` still takes precedence over it.

```yaml
networkPolicy:
  enabled: true
  engine: cilium
```

The per-port `.from` knobs documented under [Configuring ingress](#configuring-ingress)
apply to the `kubernetes` engine only. For the other engines the allowlists are
pod-scoped allow-all on the required ports, and you tighten them through that
engine's native passthrough lists — `networkPolicy.<engine>.ingress` and
`networkPolicy.<engine>.egress` — which are merged verbatim into the rendered
policy's `spec`. For example, adding identity-based ingress and a `toFQDNs` egress
under the Cilium engine:

```yaml
networkPolicy:
  enabled: true
  engine: cilium
  cilium:
    ingress:
      - fromEndpoints:
          - matchLabels:
              app.kubernetes.io/name: kustomize-controller
    egress:
      - toFQDNs:
          - matchName: bucket.example.com
        toPorts:
          - ports:
              - port: "443"
                protocol: TCP
```

## Required traffic

The traffic JaaS needs depends on the mode it runs in. The renderer-mode rows apply
to every install; the operator-mode rows apply only when `operator.enabled` is
`true`.

### Ingress

| Port | Source | Mode | Selectable by label? |
|---|---|---|---|
| Jsonnet HTTP (`ports.http`, `8080`) | Callers of the `/jsonnet` endpoint, or an Ingress controller fronting the Service | always | Yes — or open when an Ingress fronts it |
| Management probes (`ports.management`, `8081`) | The kubelet, dialing the readiness, liveness, and startup probes from the node IP | always | No — the node IP is not a pod, so it cannot be a `podSelector` |
| Storage HTTP (`ports.storage`, `8082`) | The Flux consumers that dereference `ExternalArtifact` tarballs — kustomize-controller, helm-controller, and custom consumers such as stageset-controller | operator | Yes — by consumer namespace |
| Webhook (`ports.webhook`, `9443`) | The kube-apiserver, dialing the validating admission webhook | operator + webhook | No — the apiserver is not a pod |
| Metrics (`ports.metrics`, `8083`) | Prometheus scraping `/metrics` | operator + metrics | Yes — by the scraper's pod or namespace |

The Jsonnet HTTP and management ports always get an ingress rule. The storage,
webhook, and metrics ports each get their own rule when their mode is active —
storage when `operator.enabled`, webhook when the operator's webhook is enabled,
and metrics when the operator's metrics endpoint is enabled.

The kubelet and the apiserver source traffic from addresses that are not pods, so
their rules cannot be narrowed with a `podSelector` or `namespaceSelector`. Leaving
the management and webhook `from` lists empty keeps those ports reachable, which is
what lets probes succeed and the apiserver reach the webhook. Authenticity on the
webhook port is enforced by TLS and the CA bundle on the
`ValidatingWebhookConfiguration`, not by the network layer — see the
[admission webhook page](/usage/admission-webhook/).

### Egress

Egress only matters when you opt into it (`networkPolicy.egress.enabled`). The JaaS
operator needs the following outbound flows; in renderer mode it needs only DNS, if
that.

| Destination | Purpose | Mode | Selectable by label? |
|---|---|---|---|
| Cluster DNS | Name resolution — without it every other egress flow fails | always | Yes — by the DNS namespace |
| kube-apiserver | TokenRequest minting, CR reads, `ExternalArtifact` writes, leader election, and webhook caBundle patching | operator | No — `ipBlock` CIDR only |
| source-controller | Fetching upstream artifacts for snippets that use a `sourceRef` | operator | Yes — the `flux-system` namespace |
| S3 endpoint | Reading and writing tarballs when `storage.backend` is `s3` | operator + S3 | Depends — in-cluster MinIO is label-selectable; an external bucket is `ipBlock` only |
| OTLP collector | Shipping traces when `operator.tracing.endpoint` is set | operator + tracing | Depends — in-cluster collector is label-selectable; an external one is `ipBlock` only |

The kube-apiserver is never label-selectable, so its egress rule must be an
`ipBlock` CIDR. The same applies to any S3 bucket or OTLP collector that lives
outside the cluster.

## Configuring ingress

Under the `kubernetes` engine, enable the policy and tighten each port through its
`from` knob. An empty `from` list leaves that port open; a non-empty list restricts
it to the listed peers.

```yaml
networkPolicy:
  enabled: true
  # Open by default — typical when an Ingress fronts the Service. Set a
  # from-list to restrict callers of the /jsonnet endpoint.
  http:
    from: []
  # Leave empty — the kubelet probes source from the node IP.
  management:
    from: []
  # Leave empty — the kube-apiserver cannot be expressed as a podSelector.
  webhook:
    from: []
```

The storage port defaults to allowing any pod in `flux-system`, the namespace where
the stock Flux consumers run. Add an entry per extra consumer namespace — for
example a stageset-controller running in `stageset-system`:

```yaml
networkPolicy:
  enabled: true
  storage:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: flux-system
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: stageset-system
```

The metrics port has its own ingress rule, rendered when both `operator.enabled` and
`operator.metrics.enabled` are set. Scope it to your monitoring namespace through
`networkPolicy.metrics.from`:

```yaml
networkPolicy:
  enabled: true
  metrics:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```

Anything the per-port knobs do not cover goes into `additionalIngress`, which is
merged verbatim into the policy:

```yaml
networkPolicy:
  enabled: true
  additionalIngress:
    - ports:
        - protocol: TCP
          port: 8080
      from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
```

## Opt-in egress

Egress is off by default, and deliberately so. Adding the `Egress` policy type
flips the JaaS pod to default-deny for outbound traffic — everything not explicitly
allowed is dropped. Getting the allow-list complete is the cluster operator's risk,
because the two destinations the operator needs most — the kube-apiserver and any
external S3 or OTLP endpoint — are not label-selectable and so depend on `ipBlock`
CIDRs that vary per cluster. An incomplete list does not fail loudly; it silently
cuts the operator off.

> **Warning:** Enabling egress **without** an `ipBlock` for the kube-apiserver cuts
> the operator off from the control plane. It can no longer mint tokens, read CRs,
> publish `ExternalArtifact` resources, hold the leader-election lease, or patch the
> webhook caBundle. Always include the apiserver CIDR before turning egress on.

Find the apiserver's address with:

```shell
kubectl --namespace default get endpoints kubernetes -o jsonpath='{.subsets[*].addresses[*].ip}'
```

Use that IP as a `/32` (or your control plane's CIDR for an HA apiserver). A
complete operator egress block — DNS, the apiserver, source-controller, S3, and an
OTLP collector — looks like this:

```yaml
networkPolicy:
  enabled: true
  egress:
    enabled: true
    # DNS to the cluster DNS namespace. Without this, every flow below
    # fails name resolution.
    dns: true
    dnsNamespace: kube-system
    to:
      # kube-apiserver — not label-selectable, so an ipBlock CIDR.
      # Replace with the IP(s) from the command above.
      - to:
          - ipBlock:
              cidr: 10.0.0.1/32
        ports:
          - protocol: TCP
            port: 443
      # source-controller — fetching upstream artifacts for sourceRef snippets.
      - to:
          - namespaceSelector:
              matchLabels:
                kubernetes.io/metadata.name: flux-system
      # S3 bucket — an ipBlock CIDR for an external endpoint. For in-cluster
      # MinIO, use a namespaceSelector instead.
      - to:
          - ipBlock:
              cidr: 203.0.113.0/24
        ports:
          - protocol: TCP
            port: 443
      # OTLP collector — an ipBlock CIDR for an external endpoint. For an
      # in-cluster collector, use a namespaceSelector instead.
      - to:
          - ipBlock:
              cidr: 198.51.100.10/32
        ports:
          - protocol: TCP
            port: 4317
```

Trim this to what your install actually uses: drop the S3 block on the local storage
backend, and drop the OTLP block when [tracing](/usage/observability/) is off. The
apiserver and DNS rules are non-negotiable for the operator. Storage destinations
are covered on the [storage and high availability page](/usage/storage-and-ha/), and
tenancy on the [tenancy and RBAC page](/usage/tenancy-and-rbac/).

For the full set of chart values, see the
[chart README](https://github.com/metio/helm-charts/tree/main/charts/jaas).

---
title: Service mesh
description: The opt-in service-mesh authorization the chart ships — Istio or Linkerd identity-based authorization and mTLS layered over networkPolicy, per-port allowed mesh identities, the non-mesh carve-outs for the apiserver and kubelet, and native passthrough.
tags: [service-mesh, istio, linkerd, security, networking]
---

The Helm chart ships an opt-in service-mesh authorization layer for the JaaS pod.
It is off by default and renders only when `serviceMesh.enabled` is `true`. Where
the [network policy](/security/network-policy/) operates at L3/L4 — which pods and IP
ranges may reach which ports — the service mesh operates at L7 with **identity-based
authorization** and **mTLS**: it asks *which mesh identity* is calling, proven by a
cryptographic workload certificate, not merely which IP the packet came from.

`serviceMesh` and `networkPolicy` are separate fields and compose additively. They
solve different problems and are best enabled together: the network policy draws the
L3/L4 perimeter, and the mesh authorizes meshed callers by SPIFFE identity on top of
it. Enabling one does not require the other, and neither weakens the other.

## Opt-in and explicit engine

`serviceMesh.engine` selects which mesh dialect the chart renders. It is explicit,
not auto-detected: a chart that sniffed the running mesh would render different
objects on different clusters from identical values, which breaks GitOps determinism.
You name the engine, and the rendered manifest is the same everywhere.

| `engine` | Renders | API |
| --- | --- | --- |
| `istio` (default) | `AuthorizationPolicy` + (optional) `PeerAuthentication` | `security.istio.io/v1` |
| `linkerd` | `Server` + `AuthorizationPolicy` + `MeshTLSAuthentication` | `policy.linkerd.io` |

The rendered objects are inert unless the named mesh is actually installed and the
JaaS pod is injected into it. Enabling `serviceMesh` on an un-meshed pod renders the
manifests but changes nothing about how traffic flows.

```yaml
serviceMesh:
  enabled: true
  engine: istio
```

## Per-port authorization

Each mesh-reachable port carries a `from` list naming the mesh identities allowed to
call it. An **empty `from` list leaves that port open** to any meshed caller,
mirroring `networkPolicy`'s empty-`from`-is-open semantics; a non-empty list
restricts the port to the listed identities.

The mesh-reachable ports are the Jsonnet HTTP port, the storage port, and the metrics
port:

| Port | Mode | `from` restricts |
|---|---|---|
| Jsonnet HTTP (`ports.http`, `8080`) | always | Callers of the `/jsonnet` endpoint |
| Storage HTTP (`ports.storage`, `8082`) | operator | Flux consumers that dereference `ExternalArtifact` tarballs |
| Metrics (`ports.metrics`, `8083`) | operator + metrics | Prometheus scraping `/metrics` |

A `from` entry is a source matcher with two fields:

- **`principals`** — SPIFFE/mesh identities. On Istio these are
  `source.principals` (`cluster.local/ns/<ns>/sa/<sa>`). On Linkerd they map to
  `MeshTLSAuthentication` identities (`<sa>.<ns>.serviceaccount.identity.linkerd.cluster.local`,
  or `*`).
- **`namespaces`** — source namespaces (Istio `source.namespaces`). **Istio-only** —
  Linkerd authenticates by workload identity, not by namespace, so this field is
  ignored under the `linkerd` engine.

```yaml
serviceMesh:
  enabled: true
  engine: istio
  # Restrict the storage port to the kustomize-controller's identity.
  storage:
    from:
      - principals:
          - cluster.local/ns/flux-system/sa/kustomize-controller
          - cluster.local/ns/flux-system/sa/helm-controller
  # Scope metrics scraping to the monitoring namespace.
  metrics:
    from:
      - namespaces:
          - monitoring
  # Leave http open so any meshed caller can reach the renderer.
  http:
    from: []
```

## Non-mesh clients

The kube-apiserver (which dials the admission webhook) and the kubelet (which dials
the readiness, liveness, and startup probes) are **not part of the mesh**. They carry
no mesh identity and present no workload certificate, so any authorization rule that
demanded one would reject them — admission would break and probes would fail.

The chart deliberately leaves the **webhook and management ports open** so these
non-mesh clients always connect:

- **Istio** adds an allow-any rule covering the webhook (`9443`) and management
  (`8081`) ports, so no identity is required on them.
- With **`mtls: strict`**, the chart additionally sets `portLevelMtls: PERMISSIVE` on
  those two ports, so a plaintext connection from the apiserver or kubelet is still
  accepted even while every other port enforces mTLS.
- **Linkerd** renders no `Server` for those ports, leaving them outside the mesh's
  authorization scope entirely.

Admission and probes therefore connect even with the mesh fully enabled.
Authenticity on the webhook port is enforced by TLS and the CA bundle on the
`ValidatingWebhookConfiguration`, not by the mesh — see the
[admission webhook page](/security/admission-webhook/).

## mTLS

`serviceMesh.mtls` sets the mTLS posture and applies to the **Istio engine only**;
Linkerd negotiates mTLS automatically between meshed pods, so the knob is ignored
there.

| `mtls` | Effect (Istio) |
| --- | --- |
| `""` (default) | Defers to the mesh's own default (mesh-wide `PeerAuthentication` / `MeshConfig`) — no `PeerAuthentication` is rendered |
| `permissive` | Renders a `PeerAuthentication` accepting both mTLS and plaintext |
| `strict` | Requires mTLS on the workload's ports, **except** the webhook and management ports, which get a port-level `PERMISSIVE` carve-out |

```yaml
serviceMesh:
  enabled: true
  engine: istio
  mtls: strict
```

Under `strict`, every mesh-reachable port enforces mTLS while the apiserver and
kubelet still reach the webhook and probes over plaintext via the carve-out above.

## Default-deny

`serviceMesh.defaultDeny.enabled` (default `false`) additionally renders a
namespace-wide default-deny so every pod in the install namespace rejects
unauthorized mesh traffic and the per-workload allows become the only exceptions (a
zero-trust namespace).

- **Istio** renders an empty-spec `AuthorizationPolicy` (deny-all) scoped to the
  whole namespace; it sits at lower precedence than the workload `ALLOW`, so the
  per-port allows always win for the JaaS pod while everything else is denied.
- **Linkerd** has no per-object deny-all; the namespace default is set via the
  `config.linkerd.io/default-inbound-policy` annotation, stamped onto the
  chart-managed Namespace (requires `namespace.create=true`) — otherwise annotate the
  namespace out of band.

Enable this **only when JaaS owns its namespace**, because the deny-all also denies
every co-located workload that does not have its own allowing authorization.

```yaml
serviceMesh:
  enabled: true
  engine: istio
  defaultDeny:
    enabled: true   # only when JaaS owns this namespace
```

## Native passthrough

Anything the per-port `from` knobs cannot express goes into the engine's native
passthrough list, merged verbatim into the rendered objects.

Under the `istio` engine, `serviceMesh.istio.rules` are merged into the
`AuthorizationPolicy`'s `spec.rules` (the `security.istio.io/v1` rule schema) — use it
for path/method matchers, `when` JWT-claim conditions, `ipBlocks`, and similar:

```yaml
serviceMesh:
  enabled: true
  engine: istio
  istio:
    rules:
      - from:
          - source:
              requestPrincipals:
                - "https://accounts.example.com/*"
        to:
          - operation:
              paths:
                - /jsonnet/*
```

Under the `linkerd` engine, `serviceMesh.linkerd.authorizations` are appended verbatim
as additional documents after the rendered `Server` / `AuthorizationPolicy` /
`MeshTLSAuthentication` set — each entry must be a complete `policy.linkerd.io` object:

```yaml
serviceMesh:
  enabled: true
  engine: linkerd
  linkerd:
    authorizations:
      - apiVersion: policy.linkerd.io/v1beta3
        kind: AuthorizationPolicy
        metadata:
          name: jaas-extra
        spec:
          targetRef:
            group: policy.linkerd.io
            kind: Server
            name: jaas-http
          requiredAuthenticationRefs:
            - kind: ServiceAccount
              name: custom-caller
              namespace: tenant-a
```

## Which traffic gets authorized

The mesh authorizes only meshed callers on the mesh-reachable ports; the non-mesh
ports stay open by design so the control plane keeps working.

| Port | Authorized by the mesh? | Why |
|---|---|---|
| Jsonnet HTTP (`8080`) | Yes — `serviceMesh.http.from` | Meshed callers of the renderer |
| Storage HTTP (`8082`) | Yes — `serviceMesh.storage.from` | Meshed Flux consumers |
| Metrics (`8083`) | Yes — `serviceMesh.metrics.from` | Meshed Prometheus scrapers |
| Webhook (`9443`) | No — open carve-out | The kube-apiserver is not in the mesh |
| Management probes (`8081`) | No — open carve-out | The kubelet is not in the mesh |

For the full set of chart values, see
[Helm chart values](/reference/helm-values/).

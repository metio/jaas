---
title: Deploying manifests with StageSet
description: Render Kubernetes manifests in Jsonnet, publish them as an ExternalArtifact, and hand them to stageset-controller for a gated rollout.
tags: [stageset, manifests, externalartifact]
---

JaaS pairs with [stageset-controller](https://stageset.projects.metio.wtf/) to
deploy Kubernetes manifests as code: you author the manifests in Jsonnet, the
JaaS operator renders and publishes them as a Flux `ExternalArtifact`, and
stageset-controller rolls that artifact out across ordered, gated stages.

This tutorial covers the JaaS side — authoring the manifests with top-level
arguments and external variables, and publishing the rendered JSON. The rollout
side (the `StageSet` resource, its stages, gates, and actions) lives on the
stageset-controller site and is linked at the end.

## Prerequisites

- The JaaS operator installed and a tenant ServiceAccount granted the
  `externalartifacts` write verbs. The [Quickstart](/tutorials/quickstart/)
  covers both.
- stageset-controller installed, if you intend to follow the handoff section and
  roll the manifests out.

This tutorial uses the namespace `default` and the tenant ServiceAccount
`manifests-tenant`.

## Step 1 — Grant the tenant ServiceAccount its verbs

The snippet publishes an `ExternalArtifact`, so the tenant needs the
`externalartifacts` write verbs:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: manifests-tenant
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: default
  name: manifests-tenant
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: default
  name: manifests-tenant
subjects:
  - kind: ServiceAccount
    name: manifests-tenant
    namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: manifests-tenant
EOF
```

Verify the ServiceAccount and binding:

```shell
kubectl -n default get serviceaccount manifests-tenant
kubectl -n default get rolebinding manifests-tenant
```

## Step 2 — Author and apply the manifest snippet

The snippet renders a `List` of a `Deployment` and a `Service` from top-level
arguments (`spec.tlas`) and an external variable (`spec.externalVariables`). A
single-value TLA arrives as a string; the snippet parses the replica count with
`std.parseInt`. `spec.output` stays at its default `rendered` so the artifact
carries the evaluated manifest JSON:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: web-app
  namespace: default
spec:
  serviceAccountName: manifests-tenant
  output: rendered
  files:
    main.jsonnet: |
      function(name='web', replicas='2')
        local image = std.extVar('image');
        local labels = { 'app.kubernetes.io/name': name };
        {
          apiVersion: 'v1',
          kind: 'List',
          items: [
            {
              apiVersion: 'apps/v1',
              kind: 'Deployment',
              metadata: { name: name, labels: labels },
              spec: {
                replicas: std.parseInt(replicas),
                selector: { matchLabels: labels },
                template: {
                  metadata: { labels: labels },
                  spec: {
                    containers: [{
                      name: name,
                      image: image,
                      ports: [{ containerPort: 8080 }],
                    }],
                  },
                },
              },
            },
            {
              apiVersion: 'v1',
              kind: 'Service',
              metadata: { name: name, labels: labels },
              spec: {
                selector: labels,
                ports: [{ port: 80, targetPort: 8080 }],
              },
            },
          ],
        }
  tlas:
    name: [web]
    replicas: ["3"]
  externalVariables:
    image: "ghcr.io/example/web:1.4.0"
EOF
```

Each `spec.tlas` value is a list, matching the HTTP query-parameter convention:
a single element becomes a string TLA, multiple elements a JSON-encoded array.
External variables seed `std.extVar` lookups.

## Step 3 — Confirm the manifests rendered

```shell
kubectl -n default get jsonnetsnippet web-app
# NAME      READY   URL                                                                                     AGE
# web-app   True    http://jaas-storage.jaas-system.svc.cluster.local:8082/default/web-app/<sha256>.tar.gz  5s
```

If `READY` is `False`, describe the snippet — the Ready condition's `Reason` and
`Message` name the cause:

```shell
kubectl -n default describe jsonnetsnippet web-app
```

## Step 4 — Inspect the published manifests

Fetch the artifact from a one-shot pod to see the rendered manifests:

```shell
URL=$(kubectl -n default get jsonnetsnippet web-app -o jsonpath='{.status.artifactURL}')
kubectl run --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 fetch -- \
    sh -c "curl -fsSL '$URL' | tar -xzO rendered.json"
# {
#    "apiVersion": "v1",
#    "kind": "List",
#    "items": [ { "kind": "Deployment", ... }, { "kind": "Service", ... } ]
# }
```

`rendered.json` is the manifest set a Flux consumer applies to the cluster.

## Handoff: roll the manifests out with StageSet

The published `ExternalArtifact` is now ready for a Flux consumer. A consumer
references it in one of two ways:

- **Directly** — name the `ExternalArtifact` (which shares the snippet's name and
  namespace) in a `sourceRef`.
- **Producer-aware** — name the producing `JsonnetSnippet` and let the consumer
  resolve it to the `ExternalArtifact`. JaaS writes a three-field back-pointer
  (`apiVersion`, `kind`, `name`) under the artifact's `spec.sourceRef` for this,
  which is the contract producer-aware resolvers match on.

stageset-controller consumes the published artifact the producer-aware way and
rolls it out across ordered, gated stages. The `StageSet` resource, its stages,
gates, and actions live on the stageset-controller documentation:

- **stageset-controller producer-aware sources guide:**
  <https://stageset.projects.metio.wtf/usage/producer-aware-sources/>
- **stageset-controller project:** <https://stageset.projects.metio.wtf/>

Follow that guide for the rollout side; it picks up exactly where this tutorial
leaves off — at the published `ExternalArtifact`.

## Where to go next

- [Operator mode](/usage/operator-mode/) — the full operator reference,
  including the `ExternalArtifact` `spec.sourceRef` back-pointer contract that
  producer-aware consumers match on.
- [Snippet sources](/usage/snippet-sources/) — back the manifests with a
  `GitRepository` or OCIRepository instead of inline `spec.files`.

---
title: JOI images
description: The catalog of prebuilt Jsonnet OCI Images (JOI) — every published library, its image reference, upstream source, and description — ready to import into snippets.
tags: [jsonnet, libraries, oci, joi, reference]
---

[Jsonnet OCI Images](https://github.com/metio/jsonnet-oci-images) (JOI) package
popular Jsonnet libraries as single-layer OCI images, one per upstream library,
published at `ghcr.io/metio/joi-<org>-<repo>`. Because each image is a single
layer, the same artifact serves two roles: a container **image volume** mounted
into jaas, and a Flux **`OCIRepository`** source the operator fetches — so a
snippet imports a vendored library without bundling it.

Deploy them with the [JOI Helm chart](/reference/helm-values/#joi-library-chart),
which renders a `JsonnetLibrary` + `OCIRepository` pair for each enabled library.
With Flux, install it via a `HelmRelease` and enable the libraries you need under
`values.libraries`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: joi
  namespace: jaas-system
spec:
  interval: 1h
  url: oci://ghcr.io/metio/helm-charts/joi
  ref:
    # Latest released chart; pin to a tag for production (Renovate can bump it).
    semver: ">=0.0.0"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: joi
  namespace: jaas-system
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: joi
  values:
    libraries:
      k8s-libsonnet:
        enabled: true
      grafonnet:
        enabled: true
```

Install it in the namespace where your snippets live: a `JsonnetLibrary` is
namespaced, and a snippet references one in its own namespace. (`helm upgrade
--install joi oci://ghcr.io/metio/helm-charts/joi --set
libraries.grafonnet.enabled=true` does the same from the command line.)

A snippet then imports a library by its alias, choosing the version in the import
path:

```jsonnet
import 'github.com/jsonnet-libs/k8s-libsonnet/1.34/main.libsonnet'
```

The catalog below is generated from the
[jsonnet-oci-images manifest](https://github.com/metio/jsonnet-oci-images/blob/main/libraries.json),
so it always reflects the currently published set. Pin an image with the moving
`:latest` tag or an immutable dated `:<YYYY.M.D>` snapshot.

{{< joi-images >}}

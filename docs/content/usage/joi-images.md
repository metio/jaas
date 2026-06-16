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

Deploy them with the [joi Helm chart](/installation/helm-values/#joi-library-chart),
which renders a `JsonnetLibrary` + `OCIRepository` pair for each enabled library.
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

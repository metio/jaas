---
title: Verifying releases
description: Verify the jaas container image and release archives with cosign keyless signatures before deploying.
tags: [installation, security, supply-chain, cosign, sigstore]
---

Every jaas release is signed with [cosign](https://github.com/sigstore/cosign)
keyless. There is no GPG key to import — identity is proven by the release
workflow's OIDC certificate from Sigstore's Fulcio CA, recorded in the public
Rekor transparency log. Verify before you deploy.

You need `cosign` (any v2/v4 release) on your PATH. Replace `<version>` with the
calendar-version tag you are installing (for example `2026.6.15`).

## Verify the container image

Both the image and the build attestations are signed against the workflow's
Fulcio identity. The certificate-identity is the release workflow path; the OIDC
issuer is GitHub Actions:

```shell
cosign verify ghcr.io/metio/jaas:<version> \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A passing run prints the verified signature payload and exits `0`. Any other
exit code means the image is unsigned or was not produced by the jaas release
workflow — do not deploy it.

Pin to an immutable digest in production rather than the mutable tag. Resolve the
digest once (with `crane`, or `docker buildx imagetools inspect`), then verify and
deploy by digest:

```shell
digest=$(crane digest ghcr.io/metio/jaas:<version>)
cosign verify "ghcr.io/metio/jaas@${digest}" \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### SBOM and provenance

The image build attaches an SBOM and SLSA provenance. Inspect them with:

```shell
cosign download sbom ghcr.io/metio/jaas:<version>
cosign verify-attestation ghcr.io/metio/jaas:<version> \
  --type slsaprovenance \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Verify the release archives

The GitHub release attaches one `SHA256SUMS` file covering every archive, a
cosign bundle (`SHA256SUMS.bundle`) that signs it, and the per-platform
`.tar.gz` / `.zip` archives. Download `jaas_<version>_SHA256SUMS`, its
`.bundle`, and the archive you want into the same directory, then verify the
checksum file's signature and check your download against it:

```shell
cosign verify-blob jaas_<version>_SHA256SUMS \
  --bundle jaas_<version>_SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c jaas_<version>_SHA256SUMS
```

The first command proves the checksum file came from the jaas release workflow;
the second proves your downloaded archive matches the trusted checksums.
`sha256sum -c` reports `OK` per file present in the directory and ignores
checksums for files you did not download.

The GitHub release notes link here so they stay short — the canonical, copy-pasteable
commands live on this page.

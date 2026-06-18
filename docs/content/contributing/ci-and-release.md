---
title: CI and Releases
description: The verify.yml PR gate, the static-analysis tool set, and the calendar-based hand-rolled release pipeline.
tags: [contributing, ci, release]
---

## Static analysis

golangci-lint is not used. The tools below run directly, both in CI and locally inside the dev shell (see [Building and Testing](/contributing/building/)). Every tool is a separate, auditable binary with its own config file.

| Tool | Scope | Config |
|------|-------|--------|
| `go vet` (all analyzers) | Go correctness | — |
| [staticcheck](https://staticcheck.dev) | Bugs, simplifications, style | `staticcheck.conf` (`checks = ["all"]`) |
| [gosec](https://github.com/securego/gosec) | Security patterns | inline `#nosec` justifications |
| [govulncheck](https://go.dev/security/vuln/) | Known vulnerabilities in the dependency graph | — |
| [arch-go](https://github.com/arch-go/arch-go) | Architecture rules | `arch-go.yml` |
| [gofumpt](https://github.com/mvdan/gofumpt) | Strict formatting | — |
| [REUSE](https://reuse.software) | License / copyright metadata on every file | `REUSE.toml` |
| [yamllint](https://yamllint.readthedocs.io) | YAML | `.yamllint.yaml` |
| [actionlint](https://github.com/rhysd/actionlint) | GitHub Actions workflows | — |
| [markdownlint](https://github.com/DavidAnson/markdownlint-cli2) | Markdown | `.markdownlint.yaml` |
| [typos](https://github.com/crate-ci/typos) | Spelling | `.typos.toml` |
| [Trivy](https://github.com/aquasecurity/trivy) | Container image CVEs | — |

### Architecture rules

`arch-go.yml` pins two invariants enforced with 100% compliance:

- `api/v1` depends on neither the operator internals nor `sigs.k8s.io/controller-runtime`. The CRD types stay importable by external consumers without dragging the manager in. Scheme registration uses apimachinery's `runtime.NewSchemeBuilder` for exactly this reason.
- `internal/urlguard` — the SSRF-defence layer — depends on the standard library only, with no internal and no external imports. This keeps the IP/URL validation logic self-contained and straightforward to fuzz in isolation.

## The verify.yml PR gate

`.github/workflows/verify.yml` fans out into one job per concern. A failure points straight at the offending gate. CI installs each tool fresh via `go run <tool>@latest`; the dev shell pre-installs the same tools at the same versions, so local and CI runs agree.

| Job | What it runs |
|-----|--------------|
| `test` | `go build ./...` then `go test -v -race -shuffle=on -coverprofile=cover.out ./...` |
| `lint-go` | `go vet ./...`, `staticcheck ./...`, `gosec ./...`, `gofumpt -l .` (fails on any output) |
| `vulnerabilities` | `govulncheck ./...` — reachable-from-code advisories are a hard merge gate |
| `architecture` | `arch-go` against `arch-go.yml` |
| `reuse` | `fsfe/reuse-action` — every file must carry SPDX headers |
| `yaml` | `yamllint` against `.yamllint.yaml` |
| `github-actions` | `actionlint` |
| `markdown` | `markdownlint-cli2` against `.markdownlint.yaml` |
| `typos` | `typos` against `.typos.toml` |
| `prose` | Vale against the shared metio/vale-config style; error-level findings (naming/branding) fail the gate |
| `container-image` | `docker buildx` build (load, no push) followed by Trivy scan; hard-fails on any fixable `CRITICAL`/`HIGH` |

### All-green aggregate

The workflow ends with a single `all-green` job:

- `needs` every other job
- runs `if: always()`
- fails unless each dependency `result` is `success` or `skipped`

That one job is the **only** check marked required in branch protection. Adding a new job to the `needs` list of `all-green` covers it automatically; no new required check needs to be registered.

The `govulncheck` gate is a hard blocker. A reachable-from-code advisory that cannot be fixed by bumping a dependency blocks the PR until resolved. Resolution is usually a `toolchain` bump in `go.mod` (for stdlib advisories) or `go get -u` (for module advisories).

## The release pipeline

Releases are calendar-based and automated. `.github/workflows/release.yml` runs on a Monday cron (`47 7 * * MON`) plus manual `workflow_dispatch`. The version is computed from the run date:

```shell
date +'%Y.%-m.%-d'
```

For a Monday run on 2026-06-22 that produces `2026.6.22`.

goreleaser is not used. GPG is not used. The pipeline is hand-rolled across three jobs.

### prepare

Counts commits since the last release touching the build-relevant paths (`go.mod main.go internal api config Dockerfile`). Every downstream job gates on that count being non-zero (or there being no prior release at all), so an empty week publishes nothing.

### build

A cross-compile matrix over ten platform/arch combinations:

- `linux/amd64`, `linux/arm` (v7), `linux/arm64`, `linux/ppc64le`, `linux/riscv64`, `linux/s390x`
- `windows/amd64`, `windows/arm64`
- `darwin/amd64`, `darwin/arm64`

Each platform compiles with:

```shell
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X main.version=<ver> -X main.commit=<sha>" .
```

Archives are `tar.gz` on linux/darwin and `zip` on windows (with a `.exe` binary), each bundling `LICENSE` and `README.md`.

### container

A single `docker buildx` multi-arch push to `ghcr.io/metio/jaas:{latest,<version>}` over the six linux arches. The `Dockerfile` builder is pinned to `$BUILDPLATFORM` and cross-compiles via Go's `GOARCH`, so the multi-arch build needs no QEMU.

SBOM and provenance are attached. The image is signed with cosign keyless immediately after push:

```shell
cosign sign \
  --yes \
  --annotations "repo=metio/jaas" \
  --annotations "workflow=Automated Release" \
  ghcr.io/metio/jaas@<digest>
```

Identity is proven by the workflow's OIDC certificate issued by Fulcio; there is no key to distribute.

### github

Gates on both `build` and `container` succeeding. Downloads all platform archives, computes a single `SHA256SUMS` over them, signs it with cosign keyless (Sigstore bundle format), and publishes the GitHub Release with all archives, the checksum file, and the bundle attached.

To verify a release download:

```shell
cosign verify-blob jaas_<version>_SHA256SUMS \
  --bundle jaas_<version>_SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c jaas_<version>_SHA256SUMS
```

To verify the container image:

```shell
cosign verify ghcr.io/metio/jaas:<version> \
  --certificate-identity-regexp '^https://github.com/metio/jaas/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

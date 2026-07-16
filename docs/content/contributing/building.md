---
title: Building and Testing
description: Build JaaS and run its full test suite through the flake's development shell.
tags: [contributing, building, testing]
---

The host needs no Go toolchain. Every build and test command runs through the
development shell that `flake.nix` defines, with `flake.lock` pinning each tool
to an exact version:

```shell
nix develop --command <command>
```

CI runs the same shell, so a gate that is green locally is green there by
construction. Run `nix develop` on its own to drop into an interactive shell and
invoke the tools bare.

The shell carries the Go toolchain, the static-analysis suite, the docs and
dashboard tooling, and the envtest assets. `KUBEBUILDER_ASSETS` points at an
`etcd` + `kube-apiserver` + `kubectl` bundle assembled from nixpkgs, so the
envtest-backed tests run offline with nothing to download.

## Building

```shell
nix develop --command go build -o jaas .
```

The `Dockerfile` builds the production image. It accepts `VERSION` and `COMMIT`
build args:

```shell
docker build -t ghcr.io/metio/jaas:dev .
docker build --build-arg VERSION=v1 --build-arg COMMIT=abc123 -t ghcr.io/metio/jaas:dev .
```

## Regenerating generated code

The CRD manifests under `config/crd/bases/` and `api/v1/zz_generated.deepcopy.go`
are produced by `controller-gen`. Regenerate both after touching `api/v1/` types,
and commit the result:

```shell
nix develop --command generate
```

`verify.yml`'s `generated` job runs that same command and fails on any diff, so
stale manifests cannot ship. The command is declared in `flake.nix` and reads
`scripts/generate.sh` — one definition the gate and this page both point at.

`controller-gen` itself comes from the `go.mod` `tool` directive, so its version
lives there and Renovate bumps it.

## Static analysis

golangci-lint is not used. The standalone tools below run directly, both in CI
and in the development shell:

```shell
nix develop --command go vet ./...
nix develop --command staticcheck ./...     # config: staticcheck.conf, checks = ["all"]
nix develop --command gofumpt -l .          # empty output means clean; any output is a failure
nix develop --command gosec ./...           # inline #nosec justifications silence false positives
nix develop --command govulncheck ./...     # reachable-from-code advisories only
nix develop --command arch-go               # architecture rules; config: arch-go.yml
nix develop --command modernize ./...       # newer-Go idiom check
```

## Test layers

### Pure unit tests

Table-driven tests with no external state. They live next to the code they cover
across `internal/...` and `api/v1/`. Several act as drift gates:
`conditions_test.go` verifies that every `Reason*` constant has a matching
`docs/runbooks/<reason>.md`, and `TestErrorResponse_StableCodeValues` pins the
wire-level `ErrCode*` strings against accidental rename.

```shell
nix develop --command go test -count=1 -race -cover ./...
```

To run a single test by name:

```shell
nix develop --command go test -count=1 -v -run TestName ./internal/handler/
```

### Envtest-backed operator tests

Files named `envtest_*_test.go` (in `internal/operator/`,
`internal/webhook/selfsigned/`, and `main_envtest_test.go` at the repo root) boot
a real `kube-apiserver` and `etcd` via controller-runtime's `envtest` package and
run the reconciler, webhook, and full `run(...)` function against them.

The tests share one apiserver instance per test binary, guarded by a `sync.Once`,
so the startup cost is paid once. Each test `t.Skip`s when `KUBEBUILDER_ASSETS`
is unset — there is no build tag. The development shell exports it, so these
tests run by default; on a host without the assets they silently skip.

The envtest harness sets `Config.SkipImpersonation` (the only place that setting
is allowed) and defaults `MetricsBindAddress` to `"0"` so parallel test cases do
not fight over the metrics port.

```shell
nix develop --command go test -count=1 -race -cover ./...
```

### Golden / example end-to-end tests

`examples_test.go` boots the full binary via `runInBackground` and asserts HTTP
responses against golden files under `testdata/golden/`. Comparison is semantic —
both sides are parsed as JSON and compared on the parsed values, so whitespace
and key ordering are irrelevant.

After changing an example or adding a new one, regenerate the golden files:

```shell
nix develop --command go test -update ./...
```

Inspect and commit the diff in `testdata/golden/`.

### Fuzz tests

Fuzz targets in `internal/handler/`, `internal/sources/`, and `internal/urlguard/`
harden the request path, the tar/gzip artifact unpacker, and the SSRF URL/IP
parser against adversarial input. CI exercises their seed corpus as ordinary unit
tests. To fuzz interactively:

```shell
nix develop --command go test -fuzz=FuzzName -fuzztime=30s ./internal/urlguard/
```

### Benchmarks

Throughput benchmarks in `internal/eval/`, `internal/storage/`, and
`internal/operator/` cover reconcile throughput, watch mapping, and the
tenant-client cache. They are baselines, not merge gates. The reconcile benchmark
is envtest-backed and skips without `KUBEBUILDER_ASSETS`.

```shell
nix develop --command go test -bench=. -benchmem -run=^$ ./internal/operator/
```

### Kind operator smoke tests

The cluster-level layer runs outside `go test`. Pure-`kubectl` bash scenarios in
`hack/smoke/` run against a real kind cluster via
`.github/workflows/kind-smoke.yml`. To run a scenario locally against any
reachable cluster, deploy JaaS and invoke the scenario scripts directly:

```shell
hack/smoke/scenario-basic.sh
```

See [CI and releases](/contributing/ci-and-release/) for how the smoke layer fits
into the two-angle end-to-end strategy.

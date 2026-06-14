<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

JaaS (Jsonnet-as-a-Service) is a Go webservice that evaluates Jsonnet snippets on demand and returns JSON. It is commonly paired with the grafana-operator for managing Grafana dashboards, but accepts any Jsonnet input.

## Common commands

This host has no Go toolchain installed; commands run inside a containerized dev shell driven by `dev/Containerfile`. A `.ilo.rc` at the repo root supplies all the `ilo shell` args, so the short form works:

```shell
ilo bash -c 'go build -o jaas .'
ilo bash -c 'go vet ./...'                         # CI: Go correctness
ilo bash -c 'staticcheck ./...'                    # CI: staticcheck (checks=all via staticcheck.conf)
ilo bash -c 'gofumpt -l .'                         # CI: strict formatting (empty output == clean)
ilo bash -c 'gosec ./...'                          # CI: security scanner
ilo bash -c 'arch-go'                              # CI: architecture rules (arch-go.yml)
ilo bash -c 'go test -count=1 -race -cover ./...'  # full suite with race detector
ilo bash -c 'go test -count=1 -v -run TestName ./internal/handler/'  # single test
ilo bash -c 'go test -bench=BenchmarkReconcile -benchmem -run=^$ ./internal/operator/'  # reconciler throughput baseline
```

To add a tool, edit `dev/Containerfile`; the next `ilo bash` invocation rebuilds the image.

**Static analysis is the standalone tools above — never golangci-lint** (it is banned project-wide). The Go gate is `go vet` + `staticcheck` (`staticcheck.conf`, `checks = ["all"]`) + `gofumpt` + `gosec` + `arch-go` (`arch-go.yml`). Separate CI jobs run `yamllint` (`.yamllint.yaml`), `actionlint`, `markdownlint` (`.markdownlint.yaml`), and `typos` (`.typos.toml`). gosec false positives are silenced with inline `// #nosec <rule> -- <invariant>` comments, not config-wide exclusions. These configs are kept identical to the `stageset-controller` repo so both projects lint the same way.

Container image (`gcr.io/distroless/static:nonroot` runtime base, built for `linux/amd64,arm64,arm/v7,ppc64le,riscv64,s390x` — the builder is pinned to `$BUILDPLATFORM` and cross-compiles via Go's `GOARCH`, so the multi-arch build needs no QEMU; `VERSION` / `COMMIT` are build args, defaulting to `development` / `unknown`):

```shell
docker build -t jaas .
docker build --build-arg VERSION=v1 --build-arg COMMIT=abc123 -t jaas .
```

The Helm chart lives in the [metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/jaas) monorepo (`charts/jaas`, published at `oci://ghcr.io/metio/helm-charts/jaas`), not this repo. Chart edits, helm-unittest, helm-schema, and kube-score all happen there. This repo owns the binary, the CRDs (`config/crd/bases/`, vendored into the chart by helm-charts' bump automation), and the runtime contract the chart deploys against.

## Architecture

`main.go` keeps `main()` itself to a 4-line shell that creates a signal channel, calls `signal.Notify(sigs, SIGINT, SIGTERM)`, and `os.Exit(run(args, env, stdout, stderr, sigs))`. All real work lives in `run(...) int`, which takes its dependencies (args, env, writers, signal channel) as parameters so it can be exercised from tests without touching globals. `run` uses `flag.NewFlagSet("jaas", flag.ContinueOnError)` per call — not the global `flag.CommandLine` — so two consecutive invocations in one process don't panic with "flag redefined". Exit codes follow Unix convention: 0 success, 1 runtime failure (bind, shutdown), 2 flag parse error.

`run` wires two independent `http.Server` instances and a shared `*handler.HealthState`, plus — when `--enable-flux-integration` is set — a third HTTP server and a controller-runtime manager:

- **Jsonnet server** (default `127.0.0.1:8080`): `GET /<jsonnet-endpoint-path>/{snippet...}` via `handler.JsonnetHandler(handler.Config{…})`. Endpoint path defaults to `jsonnet`.
- **Management server** (default `127.0.0.1:8081`): three distinct handlers — `StartupHandler(state)` at `/start`, `ReadinessHandler(state)` at `/ready`, `LivenessHandler()` at `/live`. Liveness is unconditional 200; startup/readiness consult `HealthState` and return 503 + a `{"status":"…"}` JSON body when not yet started / not ready.
- **Storage server** (default `0.0.0.0:8082`, opt-in via `--enable-flux-integration`): `http.FileServer` rooted at `--storage-path`, serving the tar.gz artifacts the operator publishes. Downstream Flux consumers (kustomize-controller, helm-controller) fetch ExternalArtifact tarballs from here.

Startup sequence: every listener `net.Listen` first (so bind failures exit the process with a clear log line); only after every bind succeeds does `state.MarkStarted()` + `state.SetReady(true)` flip the probes green, and the goroutines call `http.Server.Serve(listener)`. On `SIGINT` or `SIGTERM`, `drainBeforeShutdown(state, shutdownDelay, …)` runs first — it flips readiness to false and (when `shutdownDelay > 0`) blocks for that long so Kubernetes can propagate the not-ready status to endpoint controllers before in-flight requests start being aborted. Only then do the servers `Shutdown(ctx)` against a fresh `context.WithTimeout(context.Background(), 30s)` — separate from the long-running app context. The operator goroutine cancels its context at the same time and is awaited (bounded 30s). The distroless `static` runtime image has no `sleep` binary, so the drain delay is implemented in the binary itself rather than via a `preStop` hook.

## Operator mode (`--enable-flux-integration`)

JaaS doubles as a Flux source-controller-like operator when launched with `--enable-flux-integration`. The operator watches two CRDs at `jaas.metio.wtf/v1`:

| Kind | Scope | Purpose |
|---|---|---|
| `JsonnetSnippet` | Namespaced | A snippet to evaluate and publish as an `ExternalArtifact`. |
| `JsonnetLibrary` | Namespaced | Reusable .libsonnet files referenced by snippets in the same namespace (inline `files` or a `sourceRef`). |

Two ways a library reaches a snippet, both feeding the same import-alias namespace (alias = `LibraryRef.ImportPath`, default `.Name`; collisions rejected at admission + reconciler fallback): a namespaced `JsonnetLibrary` (read tenant-impersonated; inline `files` or a `sourceRef` to a Flux source), and OCI-mounted `additionalLibraries` (static, folded in additively after CR refs). `LibraryRef.kind` enum is `JsonnetLibrary` only. There is deliberately **no cluster-scoped library or snippet CRD**: a snippet produces a namespaced `ExternalArtifact`, so producers stay namespaced; cluster-wide shared libraries are served by the static OCI-mount path (the chart's `additionalLibraries`), which doubles as the high-DX, cluster-free local renderer input. The chart enforces that static OCI mounts (`snippets`/`additionalLibraries`) and Flux integration (`operator.enabled`) are mutually exclusive in one release — the two modes don't mix.

The operator's package layout:

- `api/v1/` — CRD types with kubebuilder annotations + handwritten `zz_generated.deepcopy.go`. Regenerate via `ilo bash -c 'controller-gen object paths=./api/v1/...'` (CRDs via `controller-gen crd paths=./api/v1/... output:crd:dir=./config/crd/bases`). Scheme registration uses apimachinery's `runtime.NewSchemeBuilder` — not controller-runtime's deprecated helper — so the API package stays free of controller-runtime as a dep.
- `internal/eval/` — shared go-jsonnet wrapper. `EvaluateFile` (file-based, used by the HTTP handler via `FileImporter`) and `EvaluateAnonymousSnippet` (string-based, used by the reconciler via `InMemoryImporter`) both flow through `buildVM`. `InMemoryImporter` resolves imports with the **same semantics as `jsonnet -J vendor`**, so jb-vendored libraries (grafonnet, docsonnet, …) render identically on the operator path and locally: (1) relative to the importing file within its own root (bare-sibling `import 'dashboard.libsonnet'`, `./x`, `../x`), then (2) a bare registered alias → its `main.libsonnet`, then (3) explicit `alias/file` (authoritative when the head is a registered alias), then (4) a JPATH/vendor search of the import path across `Self.Files` then every library — which is what lets absolute `import 'github.com/grafana/grafonnet/gen/...'` resolve against a `JsonnetLibrary` whose `sourceRef.path` is empty (whole vendor tree). A slash-prefixed path whose head is not a registered alias is NOT an alias error — it falls through to the vendor search. Resolution depends on the importing file, so the importer caches on `(importedFrom, importedPath)` and records each `foundAt`'s location for transitive relative imports. Sibling files (step 1) win over a library's default entry. The `joi` helm chart in helm-charts publishes JOI images as `JsonnetLibrary` + `OCIRepository` pairs with empty `path` for exactly this reason.
- `internal/storage/` — artifact store backends behind the `storage.Backend` interface (`Put` / `Prune` / `Delete` / `Sweep` / `HTTPHandler` / `Close`). Two production implementations: `*Store` (filesystem, rooted at `--storage-path`, surfaced via `http.FileServer`; `os.OpenRoot` guards against `..` traversal) and `*S3Backend` (any S3-compatible bucket via minio-go, surfaced via a streaming proxy handler so source-controller's contract — fetch a bare HTTP URL — stays unchanged). Both produce byte-identical tar.gz output for the same input (sorted entries + zero ModTime), so digests don't drift across backends. `S3Backend.Put` streams the tarball through an `io.Pipe` directly into `minio.PutObject` with `objectSize=-1` (forces multipart) — no full-tarball buffer; resident set during a publish is bounded by the configured PartSize (16 MiB) regardless of artifact size. Selection is via `--storage-backend=local|s3`; S3 setup lives behind `--s3-endpoint`, `--s3-bucket`, `--s3-prefix`, `--s3-region`, `--s3-use-ssl`, `--s3-access-key` / `-s3-secret-key` / `-s3-session-token`, `--s3-anonymous`. Empty static creds engage minio-go's IAM/IRSA discovery chain (env → EC2/EKS metadata). Multi-replica HA pairs the S3 backend with leader election: every replica reads from the same bucket, only the lease-holder writes.
- `internal/sources/` — Flux source-controller artifact fetcher. `Fetcher.Fetch(ctx, client, ref, ownerNs)` looks up the source CR (Unstructured), waits for `status.conditions[Ready]=True`, runs the URL through `internal/urlguard` (rejecting loopback / link-local / multicast / unspecified plus inet_aton-form alt-IPv4 that bypasses `net.ParseIP`), streams the artifact body into a tempfile while sha256-hashing inline, verifies the computed digest against `status.artifact.digest` via a `<algo>:<hex>` parser that catches missing prefix / wrong hex length / non-string types (ErrDigestInvalid distinct from ErrDigestMismatch), then gunzips+untars into a path→content map (filtered by `SourceRef.Path`). Three byte caps enforced: aggregate `MaxArchiveBytes` (default 64 MiB), per-entry `MaxPerEntryBytes` (16 MiB, with `io.LimitReader` body bound that catches lying headers), decompressed-stream `MaxDecompressedBytes` (512 MiB, via `cappedReader` wrapping the gzip stream) — defends against gzip-bombs and crafted hdr.Size overflow. Tar entry path validation: rejects NUL, backslash, `..`, absolute paths, and any byte outside `[A-Za-z0-9._/-]` plus drops dot-prefixed segments silently. The tenant-impersonating client passed in is the same one used for library Gets — the tenant SA's RBAC governs both source-CR read and artifact fetch. The HTTP client also installs `safeDialContext`, which resolves each host once and rejects/pins the dialed IP through `urlguard.ForbiddenIP` on the initial dial and every redirect hop — so a name that passes the string-level URL check but resolves (or rebinds) to a forbidden address never connects. Tests install `urlguard.PermissiveHTTPURL` via `Fetcher.URLValidator` AND a permissive `Fetcher.IPValidator` so httptest's 127.0.0.1 listeners stay reachable.
- `internal/urlguard/` — SSRF defence layer: `ValidateHTTPURL` rejects non-http(s) schemes, missing hosts, and literal IPs on the denylist (loopback / link-local incl. cloud metadata / multicast / unspecified) plus the "localhost" string, stripping a trailing FQDN-root dot first so `127.0.0.1.` / `localhost.` can't slip past. Also parses inet_aton-form alt-IPv4 (single-int, hex/octal, short-dotted) that libc resolvers honor but `net.ParseIP` rejects. The denylist deliberately does NOT cover RFC1918 / CGNAT / IPv6-ULA private ranges — the operator's primary fetch target is an in-cluster source-controller on private addresses, so blocking them would break the main use case; internal-service reachability is NetworkPolicy's boundary. `ForbiddenIP` exposes the IP-level check for the sources dialer (dial-time rebinding defence); `PermissiveHTTPURL` is the scheme-only variant for tests and dev clusters.
- `internal/statusretry/` — generic `UpdateWithRetry[T]` + `UpdateUnstructuredStatusWithRetry` helpers that re-Get the latest object, apply a mutate fn, and `Status().Update` with `IsConflict` retry via bounded backoff. Used by `SnippetReconciler.failReady` / `markSynced` and `Publisher.writeStatus` so a sibling controller bumping resourceVersion between Get and Update retries locally instead of bubbling up and forcing controller-runtime to redo the whole reconcile.
- `internal/operator/`:
  - `manager.go` — boots `ctrl.NewManager` and wires the reconciler + (optional) validator via a builder seam; tests substitute a fake builder.
  - `reconciler.go` — `SnippetReconciler.Reconcile`: finalizer lifecycle, dependency-cycle detection (`detectSourceRefCycle` BFS through `spec.sourceRef → ExternalArtifact → publishing snippet` AND `spec.libraries → JsonnetLibrary → sourceRef → publishing snippet` edges, rejecting on revisit of the starting snippet; the BFS is gated by `hasCycleSourceEdge` — snippets with no ExternalArtifact sourceRef and no library refs return "no cycle" without allocating the BFS state — and by `cycleCache` keyed by `(UID, Generation)` which memoizes the verdict for snippets that do have deps. `mapJsonnetLibrary` and `mapFluxSource` Forget cache entries for the snippets they enqueue so a transitively-introduced cycle still triggers a fresh walk on the next reconcile), source resolution (`spec.files` inline OR `spec.sourceRef` via `internal/sources/Fetcher`), library resolution + publish via a per-reconcile impersonating client (`tenantClient` mints a Bearer token through the SA TokenRequest API via `internal/operator/token.go`'s `tokenCache`, then stamps it on a clone of `mgr.GetConfig()`; `tenantClientCache` memoizes the constructed `client.Client` per `<namespace>/<SA>` and rebuilds only when the token rotates — `client.New` builds a fresh RESTMapper + transport per call, so caching it drops a non-trivial fixed cost from the hot path), eval via `internal/eval`, status update via the operator's own client. The operator SA needs only `serviceaccounts/token: create` — no `impersonate` verb. `Config.SkipImpersonation` opts out — only the envtest harness sets it; production must keep impersonation on so a compromised snippet can't reach beyond the tenant SA's RBAC. `SetupWithManager` registers watches on JsonnetLibrary and `FluxSourceKinds` (GitRepository / OCIRepository / Bucket / `ExternalArtifact` — chained snippets re-render when an upstream republishes; cycles are prevented by `detectSourceRefCycle`). Watches resolve indirect `Snippet → LibraryRef → JsonnetLibrary → SourceRef` chains in `mapFluxSource`. The Flux watches are gated on the RESTMapper resolving each GVK (with a `Reset()` first to bypass stale discovery cache) so the operator boots cleanly in clusters without source-controller installed; missing kinds are tracked on `SnippetReconciler.missingFluxKinds` (RWMutex-protected) and surface to `crdWatcher` (a manager-runnable) which uses a client-go apiextensions informer to subscribe to the cluster's CRD stream. When a previously-missing Flux source CRD becomes `Established=True` the watcher calls `reconciler.EngageFluxWatch(gvk)`, which adds a new `source.Kind` to the live controller via `controller.Watch` — no process restart. The reconciler keeps a reference to the controller via `builder.Build(r)` instead of `Complete(r)` for this reason. Requires `get/list/watch` on `customresourcedefinitions.apiextensions.k8s.io` in the operator's ClusterRole.
    - **Watch-scope** (`--watch-namespaces`, JAAS_WATCH_NAMESPACES env): comma-separated namespace list scopes `Cache.DefaultNamespaces`. Empty (default) is cluster-wide. The chart's `operator.watchNamespaces` mirrors this — when set, it threads the value into the deployment's `-watch-namespaces` arg AND pivots the rendered RBAC: `clusterrole-operator-tenants` (snippets, libraries, ExternalArtifact, Flux sources, SAs, token mint, events) is bound via one RoleBinding per listed namespace instead of a cluster-wide ClusterRoleBinding. `clusterrole-operator-cluster` (CRDs + optional VWC) stays bound via a ClusterRoleBinding — those resources are inherently cluster-scoped.
    - **Transient/non-transient classifier** (`classifyFetchError`): returns `(reason, msg, transient bool)`. Transient fetch errors (`ErrSourceNotReady`, generic network) propagate as errors to engage controller-runtime backoff; non-transient (digest mismatch / invalid, urlguard rejections, oversized archives) fail status as steady-state. The reconciler writes the failure status either way; only the backoff vs no-backoff differs.
    - **Permanent-API-error classification** (`isPermanentAPIError` + `rbacDenialMessage` helpers): wraps `apierrors.IsForbidden`, `apimeta.IsNoMatchError`, and `runtime.IsNotRegisteredError`. Applied at every tenant-side apiserver call — Fetcher source-CR reads, `resolveLibraries`'s `tenant.Get(JsonnetLibrary)`, and the Publisher's `ExternalArtifact` upsert — routing the error to `ReasonRBACDenied` with a message that names the SA, verb, and resource the cluster operator must grant. Non-transient by design: retry can't grant a verb or install a CRD. Stops the workqueue burning cycles on permanently-failing snippets and keeps `JaaSControllerWorkqueueDepthHigh` a reliable health signal. `ReasonRBACDenied` is wire-stable; its runbook is `docs/runbooks/rbacdenied.md`. The watch-layer failure mode (the operator's OWN ClusterRole missing a verb so an informer can't start) is NOT covered here — that's controller-runtime's domain and surfaces only via `Failed to watch` log lines; see `docs/runbooks/operator-watch-silent.md` for diagnosis.
    - **Cross-namespace error scrubbing**: when a `sourceRef` targets a different namespace and the fetch fails, the raw error is replaced with a constant `cross-namespace <kind> %q is not reachable; check the source CR's status in %q` so a tenant can't fingerprint other namespaces (NotFound vs Forbidden vs digest mismatch vs 5xx all collapse to one message). Same-namespace failures stay verbatim.
    - **Pre-publish staleness gate** (`publishConsistencyGate`): captures `judgedGen := snip.Generation` at Reconcile entry. Just before `Publisher.Publish`, does an uncached `APIReader.Get` to recheck Generation. On mismatch, the publish is deferred — the spec-bump watch event already enqueued the next reconcile, which will work against the fresh spec. Stops a render against a stale spec from landing as a stale `ExternalArtifact`.
    - **Status retries via `internal/statusretry`**: `failReady`, `markSynced`, and `Publisher.writeStatus` all route through retry-on-Conflict helpers that re-Get and re-apply the mutation. Sibling controllers (or manual kubectl edits) bumping resourceVersion no longer force the whole reconcile to redo.
    - **happyReasonsNoRunbook skip list** in `decorateMessage`: `ReasonSynced`, `ReasonSuspended`, `ReasonPending` get no runbook URL suffix even when `--runbook-base-url` is set — these are healthy/intentional states with no remediation page.

**Leader election** is on by default when `--enable-flux-integration` is set. The lease lives at `--leader-election-namespace/--leader-election-id` (the chart defaults to `<release-namespace>/<release-name>-operator`). `LeaderElectionReleaseOnCancel: true` so a SIGTERM (rolling update) hands the lease over to the next replica without waiting out the 15s LeaseDuration. The HTTP path and `/live`,`/ready`,`/start` probes run independently of LE — only reconcilers + the cache + the webhook server are gated.

**Metrics.** controller-runtime's Prometheus server is bound by `Config.MetricsBindAddress` (`-metrics-bind-address`, default `:8083`). The default explicitly avoids controller-runtime's own `:8080` — which collides with the jsonnet HTTP port. `"0"` disables the endpoint entirely. The chart exposes the port via a dedicated `jaas-metrics` Service and gates an opt-in `monitoring.coreos.com/v1` ServiceMonitor on `operator.metrics.serviceMonitor.enabled`. envtest helpers default `MetricsBindAddress` to `"0"` so multiple test cases in one process don't fight over a port. A second opt-in template, `prometheusrule-operator.yaml` gated on `operator.metrics.prometheusRule.enabled`, ships a starter alert set on the custom metrics plus a handful of controller-runtime signals (workqueue depth, reconcile latency, pod readiness); every threshold is a knob under `operator.metrics.prometheusRule.thresholds`, and `extraAlertLabels` merges onto every rendered alert so all jaas alerts can be routed through one Alertmanager receiver.

Custom JaaS metrics (defined in `internal/operator/metrics.go`, registered against controller-runtime's `metrics.Registry` so they ride the same endpoint):

- `jaas_snippet_reconcile_total{namespace, name, status, reason}` — counter, incremented on every status transition (one bump per reconcile that touches the Ready condition).
- `jaas_snippet_rendered_bytes{namespace, name}` — histogram (buckets 256B…64MiB), observed only on Synced reconciles.
- `jaas_snippet_rate_limited_total{namespace, name}` — counter, per-snippet rate-limiter denials. Paired with the `RateLimited` Warning event.
- `jaas_snippet_eval_unavailable_total{namespace, name}` — counter, reconciles deferred because the global concurrent-eval cap was full. Paired with the `EvalUnavailable` Warning event.
- `jaas_eval_in_flight` — gauge, live count of evaluations holding a slot in the global semaphore (reads through to `eval.InFlightEvals()` so the value is current per scrape). Slot release fires *inside the eval goroutine* (via `defer release()` in the eval closure that runs in the spawned goroutine), not on parent return — so a timed-out parent doesn't free its slot until the orphan goroutine actually completes. The gauge therefore reflects real in-flight goroutine count, not parent-attached reservations.
- `jaas_eval_max_concurrent` — gauge, configured ceiling of the global semaphore (`-max-concurrent-evals`). Zero means the gate is disabled — any saturation alert must guard on this being non-zero.
- `jaas_eval_unavailable_total` — counter, monotonic process-global accumulator of rejected evals (HTTP + operator). The labeled `jaas_snippet_eval_unavailable_total` covers the operator path.
- `jaas_eval_outstanding_timed_out` — gauge, eval goroutines whose parent's ctx fired before the synchronous go-jsonnet call returned. Sustained non-zero readings flag a runaway-snippet pattern the synchronous API otherwise hides.
- `jaas_storage_sweep_failures_total` — counter, failing background sweep passes.

**Webhook TLS provisioning** is governed by `-webhook-cert-mode`:

- `cert-manager` (default): chart renders a `cert-manager.io/v1` Certificate, the issued Secret is mounted read-only at `-webhook-cert-dir`. Cert-manager handles renewal; controller-runtime's webhook server uses `sigs.k8s.io/controller-runtime/pkg/certwatcher` internally to hot-reload TLS when the file changes.
- `self-signed`: `internal/webhook/selfsigned` generates an ECDSA P-256 CA + serving cert in-pod, writes them to `-webhook-cert-dir` (emptyDir mount), and stamps the named `ValidatingWebhookConfiguration`'s `caBundle` so the apiserver trusts the chain. A `Renewer` goroutine runs alongside the manager — every `validity/3` it regenerates and re-writes the files; certwatcher's fsnotify hook in the webhook server hot-reloads TLS without restart. The caBundle is treated as a CA **set**: `mergeCABundle` (also behind the bootstrap's `CombineCABundles`) preserves every still-valid foreign CA, prunes expired blocks, and the rotation's trim step removes only *this* pod's previous CA — tracked via `Renewer.CurrentCA`, seeded from the bootstrap — so a rotation never evicts another replica's CA. Single-replica installs collapse to "NewCA only". Every VWC write goes through `UpdateVWCCABundle`, a read-merge-write under optimistic concurrency (Update carries the resourceVersion; a Conflict re-reads and re-applies the commutative set-mutation) — so several pods bootstrapping at once during a multi-replica rolling update **converge** instead of clobbering each other's CAs. The renewer goroutine has panic recovery and is awaited on shutdown. Requires `get/update` on the named VWC (scoped via `resourceNames` in the ClusterRole).
  - `publisher.go` — `Publisher.Publish` writes the tarball + upserts the `ExternalArtifact` CR (Unstructured, so source-controller's typed API isn't a dep). The status write stamps both `status.artifact` (url/revision/digest/size) AND a `status.conditions[Ready]=True` (`setReadyCondition`) — every Flux consumer and JaaS's own `internal/sources.readyState` gate on `Ready=True`, so an artifact written without it is treated as not-yet-consumable and chained snippets hang on `ErrSourceNotReady`. `lastTransitionTime` is preserved across a steady republish so the timestamp doesn't churn. `Withdraw` runs on deletion before the finalizer drops.
  - `webhook.go` — `SnippetValidator` rejects `spec.externalVariables` keys colliding with operator `--ext-var`. The reconciler enforces the same invariant as a fallback when admission is bypassed.
  - `ratelimiter.go` — per-snippet token-bucket via `golang.org/x/time/rate`; checked right before eval.
  - `conditions.go` — wire-stable `Reason*` constants on the Ready status condition. Renaming any of them is a breaking change. `AllReasons` enumerates every constant; `conditions_test.go` is a drift gate (every entry must have a matching `docs/runbooks/<reason>.md`; the constant count in the source file must match the slice length). `--runbook-base-url` threads a URL prefix into the Ready condition Message via `SnippetReconciler.decorateMessage`, so `kubectl describe` surfaces a direct link to the per-reason remediation page. `docs/runbooks/` carries one page per `Reason*` plus a `Cross-cutting runbooks` section in its README — those pages cover incidents that don't map to a single Reason (currently `storage-recovery.md`, for PVC loss / S3 outages / downstream 404s).
  - **spec.entryFile** names the file (relative to the resolved source root) go-jsonnet evaluates. Defaults to `main.jsonnet` so existing snippets stay valid; lets operators point at a specific file in a multi-snippet sourceRef tree.
  - **spec.suspend** pauses reconciliation without deleting the snippet. The reconciler short-circuits before any expensive work and flips Ready=False/Suspended. Mirrors Flux's spec.suspend convention.
  - **spec.history** + **status.history** retain N (default 1, max 50) revisions in storage so downstream consumers can pin to an older sha256. The Publisher's `keepRevisions []string` argument carries the keep-set; the Backend's `Prune(ns, name, keepRevisions []string)` honors it.
  - **EventRecorder** (events.v1 API via `mgr.GetEventRecorder`) emits standard Kubernetes Events on every Ready-condition transition (Normal for Synced, Warning for everything else, dedup'd against the prior condition). The reason string fills both the events.v1 `reason` and `action` slots. Flux's `notification-controller` routes via `Alert` CRs targeting `kind: JsonnetSnippet` — JaaS needs no Provider/Alert plumbing of its own. The reconciler also emits a `Warning WithdrawForced` event when a deletion path force-drops the finalizer after `--max-withdraw-wait` (default 1h) of failing `Publisher.Withdraw` — guards against a permanently-broken backend (S3 perma-down, revoked RBAC, deleted bucket) pinning the snippet in `Terminating` forever. The trade-off is an orphan tarball that an operator clears by hand; see `docs/runbooks/storage-recovery.md` for the recovery procedure.
  - **OCI-library merge** (`OCILibraries map[string]eval.Library`): the operator scans every `-library-path` subdirectory at startup, reads every `.libsonnet` / `.jsonnet` / `.json` file into memory, and the reconciler folds those entries into `resolveLibraries`'s output map after the CR loop. Snippets `import "<alias>/file"` against operator-shipped shared libraries without a CR `LibraryRef`. The same scan populates `KnownLibraryAliases`, which the admission webhook + reconciler use to reject any `LibraryRef.ImportPath`/`Name` that shadows an OCI mount — admission prevents the collision that the additive merge would silently resolve in favor of the CR. First-write-wins across `-library-path` entries (matching the HTTP path's `FileImporter.JPaths` order).

Wire-stable strings the operator owns (each of these is a contract with downstream consumers — do not rename):

- `FinalizerName = "jaas.metio.wtf/finalizer"`
- `Reason*` constants in `internal/operator/conditions.go`
- `ConditionReady = "Ready"`, `OutputRendered = "rendered"`, `OutputSource = "source"` in `api/v1/shared_types.go`
- The wire codes in `internal/handler/jsonnet.go` (`ErrCode*`) — unchanged from v1.

### `internal/handler/jsonnet.go`

`JsonnetHandler` takes a `Config` struct (not positional args):

| Field | Purpose |
|---|---|
| `Snippets`, `SnippetDirectories`, `LibraryPaths` | File resolution and library import paths. |
| `ExtVars map[string]string` | Built once at startup by `ParseExtVars(os.Environ())` from `JAAS_EXT_VAR_*` env vars. **Not re-read per request.** |
| `EvaluationTimeout time.Duration` | Passed as `Options.Timeout` to `eval.EvaluateFile`; 0 disables. |
| `MaxStack int` | Passed as `Options.MaxStack`; 0 keeps go-jsonnet's default. |
| `Logger *slog.Logger` | Injectable; nil falls back to `slog.Default()`. |

Per request the handler:

1. `ctx := request.Context()` — logs carry request-scoped values, and an injected `Logger` (or the default) receives them.
2. **Snippet resolution** via `resolveSnippet`: exact match against `-snippet` first, then under each `-snippet-directory` via `os.OpenRoot(dir).Stat(name+"/main.jsonnet")`. `os.OpenRoot` (Go 1.24+) rejects `..` traversal and symlinks that escape the directory. **Security-critical** — do not refactor back to `filepath.Join + os.Stat`.
3. Builds an `eval.Options` carrying ExtVars, query-derived TLAs, MaxStack, Timeout, and a fresh `&jsonnet.FileImporter{JPaths: cfg.LibraryPaths}` per request. The importer **must not** be hoisted out of the per-request scope; go-jsonnet's `FileImporter` cache is not concurrency-safe (confirmed by `-race`).
4. Hands off to `eval.EvaluateFile(ctx, fileName, opts)` — the same entry point the operator reconciler uses. That function builds the VM, threads TLAs (1 value → `TLAVar`, >1 → JSON-encoded array via `TLACode`), reserves a slot in the global concurrent-eval semaphore (`-max-concurrent-evals`), and wraps the synchronous go-jsonnet call in `evaluateWithDeadline`. Cancelled-mid-call orphans increment `jaas_eval_outstanding_timed_out` until they finish naturally.
5. Error mapping: `eval.ErrEvalUnavailable` → 503 `evaluation_unavailable`; `context.DeadlineExceeded` → 504; `context.Canceled` (client gave up) → no response written; other errors → 400.

Non-2xx responses carry a JSON body produced by `writeJSONError(ctx, logger, w, status, ErrorResponse{…})`. The `Error` field uses the exported constants `ErrCodeMethodNotAllowed` / `ErrCodeSnippetNotFound` / `ErrCodeEvaluationTimeout` / `ErrCodeEvaluationUnavailable` / `ErrCodeEvaluationFailed` — these are **wire-level identifiers** that programmatic callers (`flux-jaas-controller`) match on, so renaming them is a breaking change. The five codes map 1:1 onto 405/404/504/503/400 as listed in the README, and `TestErrorResponse_StableCodeValues` pins the strings against accidental drift. The `Message` field for `evaluation_failed` is the raw go-jsonnet diagnostic (file+line included), which can leak snippet paths on disk — fine inside a cluster, worth gating behind a flag if exposed to untrusted callers in future.

### `internal/handler/health.go`

`HealthState` is a small RWMutex-guarded struct with `MarkStarted()` (monotonic), `SetReady(bool)` (cycleable), and `Started()` / `Ready()` accessors. The three probe handlers each take it as an argument so tests can drive lifecycle states explicitly.

### Misc

- Snippet / library / extvar flag types are accumulator slices (`stringArray` in `main.go`) so `-snippet`, `-snippet-directory`, `-library-path` can be repeated.

## Build & release

`version` and `commit` are package-level vars in `main.go`. Defaults are `"development"` / `commitSentinel = "unknown"` for local builds. The release pipeline overrides them via `-ldflags="-X main.version=… -X main.commit=…"`. Locally, `commit` is further refined by an `init()` that reads `vcs.revision` from `runtime/debug.ReadBuildInfo()`, appending `-dirty` if the worktree had uncommitted changes — so a plain `go build` already shows the actual SHA. The sentinel comparison in `resolveCommit` is strict; do not "simplify" `"unknown"` to anything else without updating the `init()` logic.

`Dockerfile` accepts `VERSION` and `COMMIT` build args and threads them through `-ldflags`. `release.yml` passes them via `build-args:` on `docker/build-push-action`.

## Helm chart

The Helm chart lives in the [metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/jaas) monorepo (`charts/jaas`, published at `oci://ghcr.io/metio/helm-charts/jaas`) — not this repo. Its templates, `values.yaml`, `values.schema.json`, helm-unittest suite, and kube-score gate all live and run there. This repo owns the binary and the CRDs (`config/crd/bases/`), which helm-charts vendors into the chart at each release tag via its `hack/vendor-crds.sh` + CRD-sync gate. The binary's config surface the chart drives — the `JAAS_EXT_VAR_*` env prefix, the bind-address flags, `--watch-namespaces`, `--max-concurrent-evals`, the webhook/cert flags — is documented per-flag in the sections above and in `README.md`.

## Examples & golden tests

The files under `examples/` aren't just documentation — they're also fixtures for end-to-end tests in `examples_test.go`. Each test boots jaas via `runInBackground` (the same seam used by `main_test.go`), fetches a `/jsonnet/…` URL against the real `examples/` directory, and compares the response to a golden file under `testdata/golden/<name>.json`. Comparison is semantic — both sides are parsed as JSON and `reflect.DeepEqual`'d on the parsed values, so whitespace and object-key ordering don't matter.

If you edit an example (or add a new one), regenerate the goldens:

```shell
ilo bash -c 'go test -update ./...'
```

Then inspect and commit the diff in `testdata/golden/`. The `-update` flag is defined in `examples_test.go` and is a no-op for the rest of the suite.

`examples/snippets/dashboards/recursion-depth/main.jsonnet` is a deliberately recursive snippet used to probe the stack limit. At the default `-max-stack=500` the snippet succeeds up to roughly `?depth=496` — confirmed by `TestExamples_RecursionDepth_FindsLimitAtDefaultMaxStack`, which does a binary search and logs the actual boundary. Useful when explaining the trade-off to operators considering tuning `-max-stack`.

The `examples/` directory is organised so each subdirectory exercises a distinct jsonnet feature or feature combination, with a matching golden test:

| Snippet | Feature exercised |
|---|---|
| `examples/snippets/example.jsonnet` (file mode) | library `import` |
| `dashboards/example1/` | library + `std.extVar` |
| `dashboards/tla-example/` | library + TLAs with defaults + `std.parseJson` |
| `dashboards/multi-tla/` | multi-value TLA (`?tags=a&tags=b` → array via `TLACode`) |
| `dashboards/embed-text/` | `importstr` on a non-jsonnet file (`examples/libraries/text/welcome.txt`) |
| `dashboards/inheritance/` | object `+`, `tags+:`, hidden `::` fields, dynamic `self` |
| `dashboards/transitive-imports/` | library importing another library (`greeter` → `utils`) |
| `dashboards/k8s-manifest/` | realistic Deployment manifest from four TLAs |
| `dashboards/recursion-depth/` | stack-limit probe (`-max-stack` knob) |
| `dashboards/library-precedence/` | `-library-path` precedence (rightmost wins, pinned by test) |

The `examples/libraries-overrides/` directory exists solely to exercise the library-precedence test — it redefines `examplonet/main.libsonnet` so `TestExamples_LibraryPrecedence_WithOverride` can prove the rightmost `-library-path` takes effect. Don't put production-style libraries there.

## Licensing / REUSE

The repo is REUSE-compliant (0BSD). Every source file carries an SPDX header. `REUSE.toml` overrides licensing for paths that can't carry inline headers (`examples/**`, `http/**`, generated CRD/JSON files). The `reuse` GitHub workflow enforces this on every push — new files without SPDX headers will fail CI.

## CI

`.github/workflows/verify.yml` is the PR gate. The `verify` job runs the Go gates: `go vet ./...`, `staticcheck ./...` (`checks=all`), `gofumpt -l .`, `gosec ./...`, `arch-go`, `govulncheck ./...`, `go test -v -cover ./...`, and a Docker buildx image build + Trivy scan. Separate parallel jobs run the text linters — `yamllint` (`.yamllint.yaml`), `actionlint`, `markdownlint` (`.markdownlint.yaml`), and `typos` (`.typos.toml`). golangci-lint is **not** used anywhere (banned project-wide); the Go gate is the standalone tools above. Chart gates (helm-unittest, helm-schema drift, kube-score) run in the metio/helm-charts repo, which owns the chart.

`kind-smoke.yml` is the operator end-to-end gate, and it's **angle 1 of a two-angle strategy**: the dev binary (HEAD build) is tested against the **latest released chart** (`oci://ghcr.io/metio/helm-charts/jaas`, version discovered from helm-charts' `jaas-*` GitHub releases). It builds the HEAD image, loads it into kind, installs the released chart with `--set image.*` pointing at the dev image, and overlays HEAD's CRDs (`kubectl apply --server-side -f config/crd/bases/`) since the released chart's vendored CRDs lag HEAD. **Angle 2** lives in helm-charts (`operator-smoke.yml`): the dev chart deploys the **released binary** (its `appVersion`) and runs the identical scenarios. Each angle holds one moving part and tests it against the released counterpart, so neither couples to the other repo's `main`.

The operator scenarios themselves are **shared bash scripts in `hack/smoke/`** (`lib.sh` helpers + `setup-*.sh` cluster prereqs + `scenario-*.sh` assertions), pure `kubectl`, agnostic to how jaas was deployed. Both repos run them: the jaas workflow calls them from the HEAD checkout; the helm-charts workflow `actions/checkout`s `metio/jaas` at the released tag (so the scenarios match the released binary's contract) and calls them from there. Only the `helm install` differs between angles. Both angles **skip (green) until the first release exists** — a `discover` job emits an empty version and `if:`-gates the smoke jobs; they activate automatically once helm-charts releases the chart (angle 1) / the chart's `appVersion` advances past `0.0.0` (angle 2).

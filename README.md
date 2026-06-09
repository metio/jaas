<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# Jsonnet-as-a-Service (JaaS)

JaaS is a webservice that evaluates Jsonnet snippets on the fly.

> **New here?** Walk through [`docs/quickstart.md`](docs/quickstart.md) — five steps from `helm install` to a published artifact, no optional knobs. Then come back here for depth. Going to production? [`docs/production.md`](docs/production.md) names the knobs to flip.

## Usage

You can find pre-built binaries on our GitHub release page, a container image at `ghcr.io/metio/jaas:latest`, and a [helm chart](https://github.com/metio/helm-charts/tree/main/charts/jaas) published at `oci://ghcr.io/metio/helm-charts/jaas`.

JaaS is controlled through command line flags. The minimal way to run it is just:

```shell
./jaas
```

You can then use the service by sending a GET request to `http://127.0.0.1:8080/jsonnet/<SNIPPET>`. `<SNIPPET>` is the name of Jsonnet snippet you want to evaluate. The service will return the evaluated Jsonnet snippet as JSON.

### Declaring Snippets

Snippets can be declared in two ways:

1. **Directory Snippets**: Specify directories with `-snippet-directory` and place your Jsonnet files in subdirectories of the given directory. For example, if you have a file `main.jsonnet` in a directory `snippet/directory/something`, you can access it via the URL `http://<IP>:<PORT>/jsonnet/something` if `-snippet-directory` is set to `snippet/directory`.
2. **File Snippets**: You can also specify individual Jsonnet files using the `-snippet` flag. For example, if you have a file `path/to/somewhere/something.jsonnet`, you can access it via the URL `http://<IP>:<PORT>/jsonnet/path/to/somewhere/something.jsonnet`.

Consider the `examples` directory of this repository:

```text
examples
└── snippets
    ├── dashboards
    │   └── example1
    │       └── main.jsonnet
    └── example.jsonnet
```

Using `-snippet-directory examples/snippets/dashboards` exposes all subdirectories as retrievable snippets, so you can access `example1` via `http://<IP>:<PORT>/jsonnet/example1`.

Similarly, using `-snippet examples/snippets/example.jsonnet` allows you to access the `example.jsonnet` snippet directly via `http://<IP>:<PORT>/jsonnet/examples/snippets/example.jsonnet`.

### Declaring Libraries

Libraries can be declared using the `-library-path` flag. This allows you to specify directories containing Jsonnet libraries that can be used in your snippets. The rightmost matching library will be used if multiple library paths match.

Consider the following directory structure:

```text
examples
└── libraries
    └── examplonet
        └── main.libsonnet
```

Using `-library-path examples/libraries` allows you to use the `examplonet` library in your Jsonnet snippets. You can then import it in your Jsonnet files like this:

```jsonnet
local examplonet = import 'examplonet/main.libsonnet';

{
  ...
}
```

### External Variables

You can set the value for external variables by defining environment variables starting with the prefix `JAAS_EXT_VAR_`, e.g., `JAAS_EXT_VAR_your_external_var=something` will expose the external variable `your_external_var` and set it to the value `something`.

### Top Level Arguments (TLA)

You can specify top level arguments using URL query parameters like this:

- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var2=value2`: Set `var1` to `value1` and `var2` to `value2` for the snippet evaluation.
- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var2`: Set `var1` to `value1` and `var2` to an empty string for the snippet evaluation.
- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var1=value2`: Set `var1` to a list containing `value1` and `value2` for the snippet evaluation.

### Error responses

Non-2xx responses carry a JSON body with `Content-Type: application/json` so programmatic callers can pick the failure apart:

```json
{
  "error":   "snippet_not_found",
  "message": "snippet \"missing\" not found",
  "snippet": "missing"
}
```

`error` is a stable identifier — callers may match on it. The currently defined codes are:

| code                     | HTTP status | When                                                                |
|--------------------------|------------:|---------------------------------------------------------------------|
| `method_not_allowed`     | `405`       | Anything other than `GET` on `/jsonnet/…`                           |
| `snippet_not_found`      | `404`       | The requested snippet name resolves to no file                      |
| `evaluation_timeout`     | `504`       | Evaluation exceeded `-evaluation-timeout`                           |
| `evaluation_unavailable` | `503`       | Global concurrent-eval cap (`-max-concurrent-evals`) is full        |
| `evaluation_failed`      | `400`       | go-jsonnet returned an error (syntax, missing import, stack…)       |

`message` is human-readable detail; for `evaluation_failed` it is the raw go-jsonnet diagnostic, including file and line numbers from the snippet on disk. `snippet` echoes the requested snippet name when one was parsed, and is omitted otherwise.

A client that closes the connection mid-evaluation receives no body and no status line — the handler detects the cancellation and bails without writing anything.

### Security Considerations

JaaS evaluates Jsonnet on the server and serves the result over HTTP. Before exposing it to a wider audience, operators should be aware of the following:

**Library paths are an unrestricted read scope.** Any file reachable under a configured `-library-path` (or under the snippet's own directory) can be `import`-ed or `importstr`-ed by *any* snippet — go-jsonnet's `FileImporter` does not sandbox per snippet. Scope `-library-path` directories tightly; do not point them at `/`, `/etc`, or anywhere holding credentials.

**Snippets are operator-controlled, not caller-controlled.** Callers only supply Top Level Arguments via URL query parameters; jsonnet's `import` / `importstr` require string literals, so TLAs and external variables cannot be used to construct arbitrary import paths. That said, deploying a snippet authored by someone you don't trust is equivalent to running their code on the server.

**Snippet name resolution is sandboxed.** The URL's `{snippet...}` segment is resolved via Go's `os.Root`, which rejects `..` traversal and symlinks that escape the configured `-snippet-directory`. So a malicious URL like `/jsonnet/../etc/passwd` is rejected with 404, even though the OS would otherwise resolve it.

**Evaluation has caps but isn't cancellable mid-flight.** `-evaluation-timeout` bounds wall-clock time per request and `-max-stack` bounds Jsonnet's call-stack depth, but go-jsonnet has no mid-evaluation cancellation — a slow snippet keeps consuming CPU on the server until it finishes naturally or the timeout fires the HTTP response. Size container CPU/memory limits accordingly. `-max-concurrent-evals` caps how many evaluations can be in-flight at once; excess requests return `503 evaluation_unavailable` (HTTP) or backpressure events (operator). The default (`max(GOMAXPROCS*4, 16)`) sized to bound worst-case goroutine pile-up; tune via the `arguments.maxConcurrentEvals` chart value. The `jaas_eval_outstanding_timed_out` Prometheus gauge surfaces orphaned evals (parent timed out, eval still running), and `jaas_eval_in_flight` / `jaas_eval_unavailable_total` track the cap's live state.

### Operator Mode

> **Support tier: GA** since release `2026.6.15`. CRDs are at `jaas.metio.wtf/v1`. The wire contracts in scope — `Reason*` constants, `ErrCode*` HTTP error codes, `ExternalArtifact` spec/status shape, chart values keys under `operator.*`, and the `JsonnetSnippet` / `JsonnetLibrary` field set — are committed. Breaking changes require a new CRD version + deprecation window, never an in-place rename. Known limits at GA are documented in [`MIGRATIONS.md`](MIGRATIONS.md).

Set `--enable-flux-integration` (or `operator.enabled: true` in the Helm chart) to boot JaaS as a Kubernetes operator alongside its HTTP path. The operator watches two CRDs in the `jaas.metio.wtf/v1` group and publishes evaluated snippets as Flux `ExternalArtifact` resources:

| Kind | Scope | Purpose |
|---|---|---|
| `JsonnetSnippet` | Namespaced | A snippet to evaluate and publish. |
| `JsonnetLibrary` | Namespaced | Reusable `.libsonnet` files referenced by snippets in the same namespace. |

Cluster-wide shared libraries (typings, organization standards) are mounted via the chart's `additionalLibraries` map — they live in the operator's filesystem rather than as a CR, which keeps the cluster-scoped CRUD surface smaller and ties the library lifecycle to the operator's release cycle.

Downstream Flux consumers (kustomize-controller, helm-controller) point a `sourceRef` at the published `ExternalArtifact` to consume the rendered JSON. Every snippet reconcile uses an **impersonating client** — the operator mints a Bearer token via the snippet's `spec.serviceAccountName` (TokenRequest API) and uses that to read libraries and Flux sources, so a tenant snippet can only reach what its own ServiceAccount can.

#### Tenant ServiceAccount RBAC

Every `JsonnetSnippet` runs against `spec.serviceAccountName`'s RBAC (or `--default-service-account`'s, if unset). The operator itself only needs `serviceaccounts/token: create` to mint Bearer tokens — every other API call is done as the tenant. **The tenant SA needs explicit verbs or the first reconcile fails with `Forbidden` and chases the wrong cause.** Minimum permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
rules:
  # Required: the operator writes the snippet's ExternalArtifact as
  # the tenant SA. Without these the publish step 403s.
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
  # Required only when the snippet uses spec.libraries (JsonnetLibrary refs).
  - apiGroups: [jaas.metio.wtf]
    resources: [jsonnetlibraries]
    verbs: [get, list]
  # Required only when the snippet uses spec.sourceRef. Grant only
  # the source kinds your tenants actually reference.
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories, ocirepositories, buckets, externalartifacts]
    verbs: [get]
```

The `externalartifacts` write verbs (`create`, `update`, `patch`) are non-negotiable — the operator writes the artifact CR through the impersonating client deliberately, so a tenant's RBAC governs both source-side reads and artifact-side writes uniformly. The `get` on `externalartifacts` covers the chained-snippets case (snippet B `sourceRef`s the `ExternalArtifact` snippet A publishes).

For namespace-scoped multitenancy, bind this Role to each tenant SA in its own namespace via a `RoleBinding`. The operator's own ClusterRole stays small — `serviceaccounts/token: create` plus watches on JaaS CRs and Flux source kinds.

End-to-end examples live under [`examples/operator/`](examples/operator/):

| Example | What it shows |
|---|---|
| [`inline-files.yaml`](examples/operator/inline-files.yaml) | Snippet whose source is `spec.files` (inline). Simplest case. |
| [`source-gitrepository.yaml`](examples/operator/source-gitrepository.yaml) | Snippet whose source is a Flux `GitRepository`. Snippet rerenders when the source republishes. |
| [`with-library.yaml`](examples/operator/with-library.yaml) | Snippet that `import`s a `JsonnetLibrary` from the same namespace. |
| [`chained-snippets.yaml`](examples/operator/chained-snippets.yaml) | Snippet B's source is the `ExternalArtifact` snippet A publishes. Cycles are detected at reconcile time. |

**Storage backend.** Two production backends:

- `--storage-backend=local` (default) — filesystem under `--storage-path`. The Helm chart pairs this with an `emptyDir` (default) or `PersistentVolumeClaim` (`operator.storage.persistence.enabled: true`). RWO PVCs cap the install at a single replica.
- `--storage-backend=s3` — any S3-compatible bucket (AWS S3, MinIO, Ceph RGW, Backblaze B2, etc.). Pairs with leader election for **multi-replica HA**: every replica reads from the same bucket, only the lease-holder writes. No RWX storage class required.

**Leader election** is on by default in operator mode (`--leader-election`); a SIGTERM-on-rolling-update hands the lease over without waiting out the 15s lease duration. Set `--leader-election=false` only when running a single replica with no rollout overlap.

**Validating admission webhook** is independently opt-in via `--enable-webhook`. It rejects `spec.externalVariables` keys that collide with operator-level `--ext-var`s. The reconciler enforces the same invariant as a fallback when admission is bypassed.

The chart defaults `operator.webhook.failurePolicy: Fail` — a webhook outage blocks every JsonnetSnippet create/update cluster-wide until the operator is back. During a rolling update the window is typically under five seconds (leader election releases the lease on context-cancel so the next replica picks up immediately). If your CI/GitOps tooling can't tolerate even that, scope the webhook with `operator.webhook.objectSelector` (e.g. require a `jaas.metio.wtf/managed: "true"` label) or `operator.webhook.namespaceSelector` (opt-in per namespace), or switch to `failurePolicy: Ignore` and rely on the reconciler-side fallback.

**Metrics** are exposed on `--metrics-bind-address` (default `:8083`) in standard controller-runtime / Prometheus text format. The Helm chart renders a dedicated `jaas-metrics` Service and an opt-in `ServiceMonitor` (`operator.metrics.serviceMonitor.enabled: true`).

**Notifications.** The operator emits standard Kubernetes `Event` objects on every Ready-condition transition — `Normal` for `Synced`, `Warning` for every other reason. Flux's `notification-controller` routes them via `Alert` CRs:

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: jaas-snippets
  namespace: flux-system
spec:
  providerRef:
    name: slack
  eventSeverity: warn   # 'info' to include success events
  eventSources:
    - kind: JsonnetSnippet
      name: '*'
```

JaaS needs no `Provider`/`Alert` plumbing of its own — operators wire whatever they already use for Flux source CRs.

**Pause + history.** A snippet's `spec.suspend: true` pauses reconciliation without deleting the artifact (Flux's convention). `spec.history` (default 1, max 50) retains the last N revisions on disk so downstream consumers can pin to a historical sha256 for rollback / blue-green. `spec.interval` (optional duration) re-renders the snippet on a cadence even with no watch event — picks up env-var drift or OCI library refreshes.

**Multi-snippet sources.** `spec.entryFile` (default `main.jsonnet`) names the file go-jsonnet evaluates. For a `sourceRef`-based snippet whose tarball contains many dashboards, point each `JsonnetSnippet` at its specific file (`dashboards/api-latency.jsonnet`, etc.) while sharing the same `GitRepository` source.

**Library alias safety.** When the operator starts with `-library-path` flags (OCI-mounted shared libraries), it scans the subdirectories and rejects any `JsonnetSnippet.spec.libraries[*].importPath` that shadows one of those names. Catches the "I mounted grafonnet via OCI but my CR alias is also called grafonnet" footgun at admission time.

### Command Line Flags

See all available command line flags with `jaas --help`:

```text
  -default-service-account string
    	ServiceAccount the operator impersonates when a JsonnetSnippet has no spec.serviceAccountName. Empty rejects such snippets at reconcile time.
  -enable-flux-integration
    	Boot the Kubernetes operator that watches JsonnetSnippet / JsonnetLibrary CRs and publishes evaluated results as Flux ExternalArtifacts.
  -enable-webhook
    	Boot the validating admission webhook for JsonnetSnippet. Requires -enable-flux-integration and a TLS cert/key in -webhook-cert-dir.
  -evaluation-timeout duration
    	Maximum duration a single Jsonnet evaluation is allowed to take. Set to 0 to disable. (default 5s)
  -ext-var value
    	External variable as KEY=VALUE for std.extVar lookups (can be specified multiple times). Takes precedence over JAAS_EXT_VAR_* env vars on conflict.
  -jsonnet-endpoint-path string
    	The path to the jsonnet endpoint (default "jsonnet")
  -kubeconfig string
    	Path to a kubeconfig file for the operator. Empty falls back to KUBECONFIG env, then to in-cluster service-account credentials.
  -label-selector string
    	Narrow the operator's watch to CRs matching this label selector. Empty selects every CR in the watched scope.
  -leader-election
    	Enable controller-runtime leader election so only one operator replica reconciles at a time. Honored only when -enable-flux-integration is set. (default true)
  -leader-election-id string
    	Lease object name used for leader election. Must be unique across JaaS installations sharing a namespace. (default "jaas-operator")
  -leader-election-namespace string
    	Namespace holding the leader-election Lease. Empty defaults to the operator pod's namespace.
  -library-path value
    	The path of a directory containing jsonnet libraries (can be specified multiple times). Rightmost matching library will be used.
  -listen-address string
    	The listen address to bind to for the Jsonnet server (default "127.0.0.1")
  -log-level string
    	The log level to use (debug, info, warn, error) (default "info")
  -management-listen-address string
    	The listen address to bind to for the management server (default "127.0.0.1")
  -management-port string
    	The port to bind to for the management server (default "8081")
  -management-read-timeout duration
    	maximum duration for reading the entire request, including the body in the management server (default 10s)
  -metrics-bind-address string
    	Bind address for the controller-runtime Prometheus metrics endpoint. Use "0" to disable. The default avoids the conflict between controller-runtime's built-in :8080 and the jsonnet HTTP server. (default ":8083")
  -storage-backend string
    	Artifact backend the operator publishes ExternalArtifact tarballs through. local (default; emptyDir/PVC) or s3 (any S3-compatible object store; pairs with leader election for HA across replicas). (default "local")
  -s3-endpoint string
    	S3 service host:port (e.g. s3.amazonaws.com or minio.minio.svc:9000). Required when -storage-backend=s3.
  -s3-bucket string
    	S3 bucket the artifacts live in. Must already exist. Required when -storage-backend=s3.
  -s3-prefix string
    	Optional object-key prefix prepended under the bucket, so jaas can coexist with other tenants in one bucket.
  -s3-region string
    	S3 region the bucket lives in. Required for AWS multi-region setups; ignored by most S3-compatible servers.
  -s3-use-ssl
    	Talk HTTPS to the S3 endpoint. Set to false only for local MinIO over HTTP. (default true)
  -s3-access-key string
    	Static AWS_ACCESS_KEY_ID. Empty triggers the IAM/IRSA discovery chain (AWS_*, web-identity, EC2 metadata).
  -s3-secret-key string
    	Static AWS_SECRET_ACCESS_KEY. Pairs with -s3-access-key.
  -s3-session-token string
    	Optional AWS_SESSION_TOKEN, paired with -s3-access-key/-s3-secret-key for temporary credentials.
  -s3-anonymous
    	Skip request signing entirely. Only useful against a public bucket — test/dev only.
  -runbook-base-url string
    	Optional URL prefix appended to every Ready condition Message as (runbook: <base>/<reason>.md). Empty disables.
  -max-withdraw-wait duration
    	Bound the time a deleted JsonnetSnippet's finalizer can hold while Publisher.Withdraw keeps failing. Past this, the operator emits a Warning WithdrawForced event, drops the finalizer, and lets the snippet be garbage-collected — possibly leaving an orphan tarball in storage. Required so a permanently-broken backend doesn't block namespace teardown. (default 1h0m0s)
  -max-artifact-bytes int
    	Cap the rendered artifact size in bytes. Snippets whose rendered output exceeds this fail with ReasonArtifactTooLarge. Zero disables. (default 0)
  -artifact-gc-grace duration
    	Minimum time a superseded artifact revision is retained after being evicted from the keep-set. Closes the pin→fetch race in which a Flux consumer reads status.artifact a moment before the operator garbage-collects the superseded revision. Zero disables and restores eager pruning. The deletion path (snippet teardown) is unaffected. (default 5m0s)
  -storage-sweep-interval duration
    	How often the operator sweeps orphaned <rev>.tar.gz.tmp residue. Zero disables. (default 10m0s)
  -storage-sweep-max-tmp-age duration
    	Minimum age before an orphaned .tmp file is eligible for sweep. (default 30m0s)
  -tracing-endpoint string
    	OTLP gRPC collector host:port (e.g. otel-collector.observability.svc:4317). Empty disables tracing entirely.
  -tracing-insecure
    	Skip TLS when dialing the OTLP collector. Use only for in-cluster collectors that don't terminate TLS themselves.
  -tracing-sample-ratio float
    	TraceID-ratio sampling (0.0..1.0). 1.0 samples every trace. (default 1)
  -webhook-cert-mode string
    	How TLS material for the webhook is provisioned: cert-manager (chart renders a Certificate; Secret mounted into the pod) or self-signed (operator generates a CA + serving cert in-pod and patches its own ValidatingWebhookConfiguration's caBundle). (default "cert-manager")
  -webhook-service-name string
    	Service name the webhook is reachable through. Used to build cert SANs in self-signed mode. (default "jaas-webhook")
  -webhook-service-namespace string
    	Namespace the webhook Service lives in. Empty falls back to -leader-election-namespace.
  -webhook-validating-config-name string
    	Name of the ValidatingWebhookConfiguration whose caBundle this operator patches. Required when -webhook-cert-mode=self-signed.
  -webhook-cert-validity duration
    	Validity of the self-signed serving cert. (default 8760h0m0s)
  -management-write-timeout duration
    	The maximum duration before timing out writes of the response in the management server (default 10s)
  -max-concurrent-evals int
    	Maximum number of in-flight Jsonnet evaluations. Excess requests return 503 (HTTP) or RequeueAfter (operator). Set to 0 to disable. Defaults to max(GOMAXPROCS*4, 16).
  -max-stack int
    	Maximum Jsonnet call-stack depth. Set to 0 to use go-jsonnet's default. (default 500)
  -no-cross-namespace-refs
    	When true (default), reject JsonnetSnippet / library CRs that reference a SourceRef in a different namespace. (default true)
  -port string
    	The port to bind to for the Jsonnet server (default "8080")
  -read-timeout duration
    	maximum duration for reading the entire request, including the body in the Jsonnet server (default 10s)
  -rerender-burst int
    	Per-snippet token-bucket depth for re-render rate limiting. (default 120)
  -rerender-rate string
    	Per-snippet steady-state re-render budget, as N/period (sec|min|hour). Token-bucket combined with -rerender-burst. (default "60/min")
  -shutdown-delay duration
    	Time to wait after readiness flips to false before initiating graceful shutdown; gives Kubernetes time to propagate the not-ready status to endpoint controllers. Set to 0 to disable. (default 5s)
  -storage-base-url string
    	Public URL prefix the operator's storage HTTP server serves tarballs at. Required when -enable-flux-integration is set.
  -storage-listen-address string
    	The listen address to bind to for the storage HTTP server (default "0.0.0.0")
  -storage-path string
    	Directory the operator writes ExternalArtifact tarballs to. Required when -enable-flux-integration is set.
  -storage-port string
    	The port to bind to for the storage HTTP server (default "8082")
  -storage-read-timeout duration
    	Maximum duration for reading the entire request on the storage server. (default 30s)
  -storage-write-timeout duration
    	Maximum duration before timing out writes of the response on the storage server. Tarballs can be MBs, so this is generous by default. (default 5m0s)
  -snippet value
    	The path of a jsonnet file or directory containing snippets (can be specified multiple times). Snippets will be loaded from the given path, where the file name is the snippet name.
  -snippet-directory value
    	The path of a directory containing snippets as subdirectories (can be specified multiple times). Snippets will be loaded from subdirectories of the given path, where the directory name is the snippet name.
  -version
    	Print version and exit
  -webhook-cert-dir string
    	Directory holding the TLS cert (tls.crt) and key (tls.key) the webhook server presents. (default "/tmp/k8s-webhook-server/serving-certs")
  -webhook-port int
    	Port the validating webhook server binds to. (default 9443)
  -write-timeout duration
    	The maximum duration before timing out writes of the response in the Jsonnet server (default 10s)
```

## Static analysis

Static analysis is part of the build, not an afterthought. CI (`.github/workflows/verify.yml`) runs every tool below, and the same checks run locally inside the dev shell (`ilo bash -c '…'`). golangci-lint is intentionally **not** used — the standalone tools below run directly.

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

The architecture rules in `arch-go.yml` pin two invariants: `api/v1` depends on neither the operator internals nor controller-runtime (the CRD types stay importable on their own), and `internal/urlguard` — the SSRF-defence layer — depends on the standard library only.

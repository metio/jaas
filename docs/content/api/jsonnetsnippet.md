---
title: JsonnetSnippet
description: Field-by-field reference for the JsonnetSnippet custom resource at apiVersion jaas.metio.wtf/v1.
tags: [api, snippets, operator, flux]
---

`JsonnetSnippet` (`jsnip`) is the published unit of Jsonnet evaluation. The JaaS
operator watches these namespaced CRs, evaluates the Jsonnet they describe, and
upserts a Flux `ExternalArtifact` whose `status.artifact.url` points at the
rendered result. Task-oriented guidance lives in
[Operator mode](/usage/operator-mode/) and [Snippet sources](/usage/snippet-sources/).

## Example

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: hello-world
  namespace: default
spec:
  serviceAccountName: hello-world-tenant
  entryFile: main.jsonnet
  output: rendered
  history: 3
  interval: 10m
  suspend: false
  files:
    main.jsonnet: |
      local lib = import 'mylib/main.libsonnet';
      lib.dashboard(std.extVar('env'), std.extVar('cluster'))
  libraries:
    - kind: JsonnetLibrary
      name: mylib
      importPath: mylib
  externalVariables:
    env: production
    cluster: eu-west-1
  tlas:
    title:
      - My Dashboard
```

Exactly one of `spec.files` or `spec.sourceRef` must be set. Admission rejects
CRs that set neither or both.

## Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `serviceAccountName` | string | — | ServiceAccount the operator impersonates for every Kubernetes API call made on behalf of this snippet (source fetches, ExternalArtifact upserts). Must exist in the snippet's namespace. When empty, the operator's `--default-service-account` applies. Reconciliation is denied when neither is set (`ReasonServiceAccountMissing`). |
| `entryFile` | string | `main.jsonnet` | File (relative to the resolved source root) that go-jsonnet evaluates. Restricted to `[A-Za-z0-9._/-]+` with no `..` segments. Maximum 255 characters. |
| `files` | map[string]string | — | Inline map of filename to Jsonnet source. Exactly one of `files` or `sourceRef` must be set. |
| `sourceRef.apiVersion` | string | `source.toolkit.fluxcd.io/v1` | APIVersion of the referenced Flux source CR. |
| `sourceRef.kind` | string | — | Kind of the referenced source. One of: `GitRepository`, `OCIRepository`, `Bucket`, `ExternalArtifact`. Required when `sourceRef` is set. |
| `sourceRef.name` | string | — | Name of the referenced source CR. Required when `sourceRef` is set. Minimum length 1. |
| `sourceRef.namespace` | string | snippet's namespace | Namespace of the referenced source CR. Cross-namespace references are rejected when the operator is started with `--no-cross-namespace-refs`. |
| `sourceRef.path` | string | — (artifact root) | Subdirectory within the fetched tarball to treat as the source root. Empty means the archive root. |
| `libraries` | []LibraryRef | — | `JsonnetLibrary` CRs importable from this snippet. Libraries not listed here are invisible to the snippet even when present in the cluster. See [Jsonnet libraries](/usage/jsonnet-libraries/). |
| `libraries[*].apiVersion` | string | `jaas.metio.wtf/v1` | APIVersion of the library CR. |
| `libraries[*].kind` | string | — | Kind of the library CR. Currently only `JsonnetLibrary` is accepted. Required. |
| `libraries[*].name` | string | — | Name of the referenced `JsonnetLibrary` CR. Required. Minimum length 1. |
| `libraries[*].namespace` | string | snippet's namespace | Namespace of the referenced library CR. Cross-namespace references are rejected when `--no-cross-namespace-refs` is set. |
| `libraries[*].importPath` | string | library's `metadata.name` | Alias used in `import` statements inside the snippet's Jsonnet source. Collisions with OCI-mounted shared library aliases are rejected at admission. |
| `tlas` | `map[string][]string` | — | Top-level arguments passed to the snippet's outermost function. A single-element value becomes a string TLA; multiple values are passed as a JSON-encoded array, matching the HTTP query-parameter convention. |
| `externalVariables` | map[string]string | — | Seeds `std.extVar` lookups for this snippet's evaluation. Keys that conflict with the operator's `--ext-var` set are rejected at admission; if admission is bypassed, the reconciler refuses the conflicting key with `ReasonExternalVariableConflict`. |
| `output` | string | `rendered` | What bytes the published ExternalArtifact carries. `rendered`: the evaluated JSON (a single `rendered.json` in the tarball). `source`: the raw `.jsonnet`/`.libsonnet` files, for downstream consumers that re-evaluate themselves. |
| `suspend` | bool | `false` | When `true`, the operator skips the evaluation pipeline, leaves the existing ExternalArtifact in place, and reports `Ready=False` with reason `Suspended`. Setting back to `false` resumes reconciliation. Mirrors Flux's `spec.suspend` convention. |
| `history` | int32 | `1` | Number of past revisions retained in storage. Minimum 1, maximum 50. Setting to N > 1 lets downstream consumers pin to an older revision via its sha256 for rollback or blue-green flows. The keep-set is tracked in `status.history`. |
| `interval` | Duration | — (watch-only) | Period between successful reconciles regardless of watch events. Picks up state outside the watched graph (environment drift, OCI library refreshes, etc.). Bounded at admission to between `30s` and `24h`. Failed reconciles use controller-runtime's exponential backoff; `interval` governs only the steady-state cadence. |

## Status

`status` follows the `SyncStatus` shape shared by all JaaS CRs.

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | int64 | `.metadata.generation` of the spec the controller last reconciled. Lets clients distinguish stale status from up-to-date. |
| `conditions` | []Condition | Standard apimachinery conditions. The `Ready` condition summarises whether the most recent reconcile succeeded; `reason` and `message` carry per-stage failure detail. See Ready condition reasons below. |
| `revision` | string | `sha256:<hex>` content hash of the last successfully reconciled source. Empty until the first successful reconcile. |
| `artifactURL` | string | HTTP URL of the last successfully published artifact tarball. Preserved across subsequent failures so the last-known-good URL stays observable. Empty until the first successful publish. |
| `lastSyncTime` | Time | Timestamp of the most recent successful reconcile. |
| `history` | []RevisionEntry | Most-recent N revisions retained in storage (`N` = `spec.history`). Index 0 is the most recent (matches `revision`). Each entry carries `revision` (sha256:hex) and `time` (publish time). |

### Ready condition reasons

Every reason string is wire-stable — runbooks key off these values.

| Reason | Status | Description |
|---|---|---|
| `Synced` | True | Most recent reconcile completed end-to-end and produced a publishable artifact. |
| `Pending` | False | Snippet observed but not yet reconciled (transient). |
| `Suspended` | False | `spec.suspend` is `true`; evaluation is paused. |
| `InvalidSpec` | False | Spec-level validation failure (missing `main.jsonnet`, invalid source combination, etc.). |
| `LibraryNotFound` | False | A `spec.libraries` entry references a `JsonnetLibrary` CR that cannot be found. |
| `CrossNamespaceRefRejected` | False | `--no-cross-namespace-refs` is enabled and a library or source reference is outside the snippet's namespace. |
| `ExternalVariableConflict` | False | `spec.externalVariables` names a key already owned by the operator's `--ext-var` set. |
| `ServiceAccountMissing` | False | Neither `spec.serviceAccountName` nor `--default-service-account` is set. |
| `EvaluationFailed` | False | go-jsonnet returned a diagnostic (syntax error, runtime error, etc.). |
| `EvaluationTimeout` | False | The eval deadline fired before the snippet finished. |
| `SourceNotReady` | False | The referenced Flux source CR exists but is not yet `Ready` or has no `status.artifact`. |
| `SourceFetchFailed` | False | Fetching or verifying the source artifact failed (HTTP error, digest mismatch, tar corruption). |
| `SourceRefNotYetSupported` | False | `spec.sourceRef` is set but the operator is running without `--enable-flux-integration`. Start the operator with that flag, or remove `spec.sourceRef` from the snippet. |
| `DependencyCycle` | False | The snippet's dependency chain (via `spec.sourceRef` or `spec.libraries`) transitively points back at itself. |
| `ArtifactTooLarge` | False | Rendered content exceeds the operator's `--max-artifact-bytes` limit. |
| `RBACDenied` | False | An apiserver call failed with Forbidden, or the source CR's kind is not registered. Non-transient — backoff is disabled. The message names the verb and resource the cluster operator must grant. |

Each reason has a remediation page at `/runbooks/<reason-lowercased>/`. See [Operator mode](/usage/operator-mode/) for lifecycle details and [ExternalArtifact output contract](/api/externalartifact/) for the artifact contract.

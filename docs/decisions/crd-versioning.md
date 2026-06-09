<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# CRD versioning

## Decision

JaaS CRDs ship at `jaas.metio.wtf/v1` from the first release. No `v1alpha1` or `v1beta1` is published.

## Context

Kubernetes's CRD versioning lifecycle (`v1alpha1` → `v1beta1` → `v1`) is a contract: each stage signals what guarantees operators can rely on. `v1alpha1` warns "may break without notice", `v1beta1` warns "may break with notice", `v1` commits to wire compatibility.

JaaS's CRD surface (`JsonnetSnippet`, `JsonnetLibrary`) is deliberately small. The fields and their semantics are validated by:

- inline kubebuilder validation (CEL + structural schema)
- a validating admission webhook (`SnippetValidator`)
- the reconciler's fallback checks
- a comprehensive envtest suite (chart unit tests live alongside the chart in metio/helm-charts)

The contract was designed conservatively from day one: no fields whose meaning is in flux, no escape hatches with TBD semantics. There's no reason to ship a pre-v1 stage that warns of imminent breakage when we have no breakage planned.

## Wire-stable surfaces

Beyond the CRD schema itself, several string-typed wire surfaces hold the contract:

- `FinalizerName = "jaas.metio.wtf/finalizer"` — set on every JsonnetSnippet under management
- `Reason*` constants in [`internal/operator/conditions.go`](../../internal/operator/conditions.go) — set on the Ready condition, enumerated in `AllReasons`
- `ConditionReady = "Ready"`, `OutputRendered = "rendered"`, `OutputSource = "source"` in `api/v1/shared_types.go`
- The wire codes in `internal/handler/jsonnet.go` (`ErrCode*`)

Renaming any of these is a breaking change. The drift-gate tests in `conditions_test.go` and `jsonnet_test.go` (`TestErrorResponse_StableCodeValues`) protect against accidental rename.

## If a breaking change becomes necessary

Two options, neither speculative:

1. **Additive new kind.** Add `JsonnetSnippetV2` (or similar) under the same group at `jaas.metio.wtf/v1`. Operators migrate by re-applying their CRs to the new kind. No conversion webhook needed. Best for incompatible reshapes.
2. **Multi-version CRD with conversion.** Add `jaas.metio.wtf/v2` under the existing CRD, ship a conversion webhook that translates between v1 and v2. `helm install` provisions the webhook alongside the operator. Best for small, mechanical reshapes where automatic conversion is feasible.

Either way, the change goes through a deprecation window: announce in `MIGRATIONS.md`, ship the new version, mark the old one as deprecated in the CRD's `served`/`storage` fields, then remove after at least one release cycle.

## Why not multi-version from day one

It's tempting to ship a v1 + v2 setup upfront so future migrations don't reshape the install. Two reasons against:

- conversion webhooks are operational overhead (extra cert-manager certificates, extra failure modes)
- premature versioning hides the actual evolutionary pressure on the API — without real-world usage you can't tell which fields will need to change

We'd rather take the breakage seriously when it comes than carry conversion machinery indefinitely.

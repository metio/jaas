<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Consuming JaaS artifacts from Flux

JaaS publishes every successfully-evaluated `JsonnetSnippet` as an
[RFC-0012](https://github.com/fluxcd/flux2/tree/main/rfcs) `ExternalArtifact`
in the same namespace as the snippet. Downstream Flux controllers
(`kustomize-controller`, `helm-controller`, `stageset-controller`, etc.)
fetch the tarball from the URL on the artifact's `status.artifact.url`.

This page covers the two ways a consumer can reference a JaaS-produced
artifact, and the trade-offs between `spec.history` (pinned retention) and
`--artifact-gc-grace` (race-window protection).

## Direct reference: the `ExternalArtifact`

The simplest pattern. JaaS publishes the artifact with the same name and
namespace as the originating `JsonnetSnippet`, so any Flux controller that
already speaks the source-controller contract can point at it directly.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-app
  namespace: apps
spec:
  sourceRef:
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: ExternalArtifact
    name: grafana-dashboards     # same name as the JsonnetSnippet
  path: ./
  prune: true
```

`kubectl get externalartifact <snippet-name>` in the snippet's namespace
returns the artifact JaaS published. Consumers that need only the URL can
also read `JsonnetSnippet.status.artifactURL` — the snippet's status
mirrors the last-known-good URL for `kubectl describe` convenience, but
producer-aware consumers should resolve through the `ExternalArtifact`,
not the snippet status.

## Producer-aware reference: stageset-controller

Some consumers prefer to name the producing snippet directly and let the
controller resolve it to an `ExternalArtifact` — useful when a `StageSet`
or other higher-level pipeline wants to track the artifact's
generations without referencing the intermediate `ExternalArtifact` by
name. The
[`stageset-controller`](https://github.com/metio/stageset-controller)
does exactly that:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: dashboards-rollout
  namespace: apps
spec:
  stages:
    - name: dashboards
      sourceRef:
        apiVersion: jaas.metio.wtf/v1   # JaaS's group/version
        kind: JsonnetSnippet
        name: grafana-dashboards
```

The controller does a reverse lookup on the published `ExternalArtifact`'s
`spec.sourceRef`, which JaaS writes as a three-field back-pointer:

```yaml
# Inside the published ExternalArtifact:
spec:
  sourceRef:
    apiVersion: jaas.metio.wtf/v1
    kind: JsonnetSnippet
    name: grafana-dashboards
# (no namespace — same-namespace publishing is implicit)
```

That shape is a **public contract** for JaaS. The exact triple
(`apiVersion`, `kind`, `name`) under `spec.sourceRef`, with the
back-pointer landing in the snippet's own namespace, is what every
producer-aware resolver matches on; renaming a field or splitting
`apiVersion` into separate `group`/`version` would be a breaking change.

## `spec.history` vs `--artifact-gc-grace`

Two separate knobs, often confused. They solve different problems:

| Concern | Mechanism | When to use |
|---|---|---|
| A consumer wants to **pin** to an older revision (`stageset-controller`'s `rollbackOnFailure`, manual rollback, blue/green) | `spec.history: N` on the `JsonnetSnippet` (default `1`, max `50`) | When N specific historical sha256 references must keep working indefinitely. Storage cost scales linearly with N × artifact size. |
| A consumer reads `status.artifact.url` and dereferences moments later — the **pin→fetch race** | `--artifact-gc-grace` operator flag (default `5m`) | Always-on. Closes the race for every consumer at negligible storage cost. Zero disables. |

**When to raise `spec.history`.** The primary motivating case is
`stageset-controller`'s `rollbackOnFailure` — when a stage's deploy
fails, the controller re-fetches the previous revision's URL and
re-applies the older bytes. For that to work, the previous revision
must still be on storage and its URL must still resolve to the
originally-published content. Set `history: 2` (or higher, if you
want multiple rollback steps available) on every `JsonnetSnippet` you
deploy through a `StageSet` that has rollback enabled. The same
applies to manual rollback / blue-green flows: any time a consumer
holds an older sha256 reference and expects it to keep working,
`spec.history` is the knob.

**`spec.history` is not the race-protection mechanism.** Raising it
to `2` to "give consumers time to fetch" mixes two concerns: every
snippet now permanently retains two revisions even though only the
most recent one is actually being consumed for steady-state reads.
The grace flag gets you the same race window for free, without
touching `spec.history`. If you do not need deliberate rollback,
leave `spec.history` at the default `1`.

Producer-aware consumers (like `stageset-controller`) pin all
referenced artifacts at the start of a run, so they observe the
pin→fetch race the same as anyone else — the grace flag covers them
too.

## See also

- [`docs/production.md`](production.md) — operator-side configuration
  knobs including `--artifact-gc-grace` defaults.
- [`docs/quickstart.md`](quickstart.md) — end-to-end walkthrough.
- [`MIGRATIONS.md`](../MIGRATIONS.md) — wire-contract changes per
  release.

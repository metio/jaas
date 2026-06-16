---
title: Admission webhook
description: The opt-in validating webhook for JsonnetSnippet, what it rejects, the failure-policy trade-off, and the two TLS provisioning modes.
tags: [operator, webhook, tls]
---

JaaS ships an optional validating admission webhook for `JsonnetSnippet`. It is
independent of, but layered on top of, [operator mode](/usage/operator-mode/).

## Enabling it

Set `--enable-webhook` to boot the webhook server. It requires
`--enable-flux-integration` (the webhook is wired only inside the operator boot
path; enabling it alone is rejected as a flag error) and TLS material — `tls.crt`
and `tls.key` — under `--webhook-cert-dir` (default
`/tmp/k8s-webhook-server/serving-certs`). The server binds `--webhook-port`
(default `9443`).

## What it validates

The webhook rejects a `JsonnetSnippet` whose `spec.externalVariables` declares a
key that collides with an operator-level `--ext-var`. An operator-supplied
external variable always wins, so a snippet that tries to redeclare one would
render against a value it does not control; the webhook refuses the snippet at
admission time instead.

The reconciler enforces the same invariant as a fallback, so a snippet that
bypasses admission — for example when the webhook is unreachable under a
`failurePolicy: Ignore` — still fails at reconcile rather than rendering with the
wrong value.

## Failure policy trade-off

The Helm chart defaults `operator.webhook.failurePolicy: Fail`. With `Fail`, a
webhook outage blocks every `JsonnetSnippet` create and update cluster-wide until
the operator is back. During a rolling update that window is typically under five
seconds, because leader election releases the lease on context-cancel and the next
replica takes over immediately.

If your CI or GitOps tooling cannot tolerate even that window, narrow or relax the
webhook:

- `operator.webhook.objectSelector` — match only snippets carrying a label, e.g.
  require `jaas.metio.wtf/managed: "true"`.
- `operator.webhook.namespaceSelector` — opt in per namespace.
- `failurePolicy: Ignore` — let create/update through when the webhook is
  unreachable, relying on the reconciler-side fallback to catch the colliding-key
  case.

## TLS provisioning

`--webhook-cert-mode` selects how the serving certificate is provisioned.

### cert-manager (default)

`--webhook-cert-mode=cert-manager` expects external tooling to provision
`tls.crt`/`tls.key`. The Helm chart renders a `cert-manager.io/v1` Certificate and
mounts the issued Secret into the pod at `--webhook-cert-dir`. cert-manager
handles renewal; the webhook server hot-reloads TLS when the mounted files change.

### self-signed

`--webhook-cert-mode=self-signed` makes the operator generate a CA and serving
certificate in-pod, write them to `--webhook-cert-dir`, and patch the named
`ValidatingWebhookConfiguration`'s `caBundle` so the apiserver trusts the chain. A
background renewer regenerates and re-writes the files before expiry, and the
webhook server hot-reloads without a restart. The relevant flags:

| Flag | Purpose |
|---|---|
| `--webhook-validating-config-name` | Name of the `ValidatingWebhookConfiguration` whose `caBundle` is patched. Required in this mode. |
| `--webhook-service-name` | Service name the webhook is reachable through; used to build the certificate SANs (default `jaas-webhook`). |
| `--webhook-service-namespace` | Namespace the webhook Service lives in. Empty falls back to `--leader-election-namespace`, then to the in-cluster downward API. |
| `--webhook-cert-validity` | Validity of the self-signed serving certificate (default `8760h`, one year). |
| `--webhook-port` | Port the webhook server binds to (default `9443`). |

In self-signed mode the operator needs `get`/`update` on the named
`ValidatingWebhookConfiguration`. Multiple replicas bootstrapping at once during a
rolling update converge on a combined `caBundle` rather than clobbering each
other, so each replica's CA stays trusted across the rollout.

The full flag list with defaults is on the
[configuration page](/installation/configuration/).

---
title: Service level objectives
description: The JaaS operator's SLOs — reconcile availability and reconcile latency — with their SLIs, targets, error budget, the dashboard that shows them, and how to tune them.
tags: [operator, slo, metrics, alerts]
---

The JaaS operator tracks two service-level objectives. Each is an objective on a
service-level indicator (SLI) computed from the [metrics](/observability/metrics/),
measured over a rolling window. The published dashboard renders them, and the Helm
chart can alert on them.

## SLO 1 — reconcile availability

**Objective:** ≥ **99%** of syncing reconciles reach `Ready=True`, over a **28-day**
window.

The SLI counts only reconciles that were actually trying to sync — the intentional
`Pending` and `Suspended` states are excluded from both halves:

```promql
sum(rate(jaas_snippet_reconcile_total{status="True"}[28d]))
/
(
  sum(rate(jaas_snippet_reconcile_total{status="True"}[28d]))
  + sum(rate(jaas_snippet_reconcile_total{status="False",reason!~"Suspended|Pending"}[28d]))
)
```

The **error budget** is the 1% of reconciles allowed to fail over the window.
Remaining budget, normalised so `1` is full and `0` is exhausted:

```promql
(<availability> - 0.99) / (1 - 0.99)
```

## SLO 2 — reconcile latency

**Objective:** the JsonnetSnippet controller's **p95** reconcile duration stays
**below 30s** over the window.

```promql
histogram_quantile(0.95, sum by (le) (
  rate(controller_runtime_reconcile_time_seconds_bucket{controller="jsonnetsnippet"}[28d])
))
```

## See the SLOs on the dashboard

The published [dashboard](/observability/dashboard/) opens with an SLO band:
current availability against its objective, error budget remaining, p95 latency
against its objective, and an availability-versus-objective trend. The
operator-internals panels below explain any movement.

The objectives and window are top-level arguments, so you set them per environment
when you render the dashboard through a `JsonnetSnippet`:

```yaml
spec:
  tlas:
    - name: datasource
      value: prometheus # your Prometheus datasource UID
    - name: window
      value: 28d # SLO window
    - name: availabilityTarget
      value: "0.99" # 99%
      code: true
    - name: latencyTarget
      value: "30" # seconds
      code: true
```

The two objectives set `code: true` so their values arrive as numbers rather
than strings.

A short window is fine for a demo; a real `28d` SLI needs at least that much
Prometheus retention. For long windows, precompute the SLI with a recording rule
and point `window` at the recorded series instead of a raw `rate(...[28d])`.

## Alert on the budget

The shipped [alerts](/observability/alerting/) already page on the *causes* of SLO
loss (reconcile errors, latency, eval saturation). To alert on the objective
itself, add an availability-SLO rule through the chart's `extraRules` passthrough —
here it fires when recent availability drops below 99%:

```yaml
operator:
  metrics:
    prometheusRule:
      enabled: true
      extraRules:
        - alert: JaaSReconcileAvailabilityBelowSLO
          expr: |
            (
              sum(rate(jaas_snippet_reconcile_total{status="True"}[1h]))
              /
              (
                sum(rate(jaas_snippet_reconcile_total{status="True"}[1h]))
                + sum(rate(jaas_snippet_reconcile_total{status="False",reason!~"Suspended|Pending"}[1h]))
              )
            ) < 0.99
          for: 1h
          labels:
            severity: warning
          annotations:
            summary: JaaS reconcile availability is below its 99% objective
```

The alert measures a short recent window (`1h`) so it pages while the budget is
actively burning; the dashboard's `window` shows the full SLO window. See
[Alerting](/observability/alerting/) for the rest of the catalog.

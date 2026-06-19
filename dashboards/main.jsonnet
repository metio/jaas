// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

// A Grafana dashboard for the JaaS operator, authored with grafonnet and
// rendered through JaaS itself. grafonnet is not vendored here — it is supplied
// at render time as a JsonnetLibrary (e.g. the JOI grafonnet image), exactly as
// any snippet's libraries are.
//
// Top-level arguments (TLAs), supplied by the consumer — a JsonnetSnippet's
// spec.tlas, or ?datasource=... on the HTTP renderer:
//   datasource  Prometheus datasource UID the panels query (default "prometheus").
//   title       dashboard title (default "JaaS operator").
function(datasource='prometheus', title='JaaS operator')
  local g = import 'github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet';
  local prom(expr, legend) =
    g.query.prometheus.new(datasource, expr)
    + g.query.prometheus.withLegendFormat(legend);
  local ts(t, unit, targets) =
    g.panel.timeSeries.new(t)
    + g.panel.timeSeries.standardOptions.withUnit(unit)
    + g.panel.timeSeries.queryOptions.withTargets(targets);
  local stat(t, unit, targets) =
    g.panel.stat.new(t)
    + g.panel.stat.standardOptions.withUnit(unit)
    + g.panel.stat.queryOptions.withTargets(targets);

  local panels = [
    ts('Reconciles by reason', 'ops', [
      prom('sum by (reason) (rate(jaas_snippet_reconcile_total[5m]))', '{{reason}}'),
    ]),
    ts('Reconcile latency (p95)', 's', [
      prom('histogram_quantile(0.95, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket[5m])))', 'p95'),
    ]),
    stat('Evaluations in flight', 'short', [
      prom('sum(jaas_eval_in_flight)', 'in flight'),
      prom('max(jaas_eval_max_concurrent)', 'ceiling'),
    ]),
    ts('Eval backpressure (rejected/s)', 'ops', [
      prom('sum(rate(jaas_eval_unavailable_total[5m]))', 'rejected'),
    ]),
    stat('Outstanding timed-out evals', 'short', [
      prom('sum(jaas_eval_outstanding_timed_out)', 'orphans'),
    ]),
    ts('Rendered bytes (p95)', 'bytes', [
      prom('histogram_quantile(0.95, sum by (le) (rate(jaas_snippet_rendered_bytes_bucket[5m])))', 'p95'),
    ]),
    ts('Storage sweep failures/s', 'ops', [
      prom('sum(rate(jaas_storage_sweep_failures_total[5m]))', 'failures'),
    ]),
    ts('Workqueue depth', 'short', [
      prom('sum by (name) (workqueue_depth)', '{{name}}'),
    ]),
  ];

  g.dashboard.new(title)
  + g.dashboard.withUid('jaas-operator')
  + g.dashboard.withDescription('Golden signals for the JaaS operator: reconciles, evaluation throughput and backpressure, rendered output size, storage sweeps, and controller-runtime workqueue/latency.')
  + g.dashboard.withTags(['jaas', 'operator'])
  + g.dashboard.withRefresh('30s')
  + g.dashboard.time.withFrom('now-6h')
  + g.dashboard.withPanels(g.util.grid.makeGrid(panels, panelWidth=12, panelHeight=8))

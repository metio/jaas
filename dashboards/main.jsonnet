// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

// A Grafana dashboard for the JaaS operator, authored with grafonnet and
// rendered through JaaS itself. grafonnet is not vendored here — it is supplied
// at render time as a JsonnetLibrary (e.g. the JOI grafonnet image), exactly as
// any snippet's libraries are.
//
// The top band tracks the JaaS operator's service-level objectives (SLOs); the
// band below shows the operator internals that explain SLO movement.
//
// Top-level arguments (TLAs), supplied by the consumer — a JsonnetSnippet's
// spec.tlas, or ?datasource=... on the HTTP renderer:
//   datasource          Prometheus datasource UID the panels query (default "prometheus").
//   title               dashboard title (default "JaaS operator").
//   selector            extra label matcher folded into EVERY query, e.g.
//                       'job="jaas"' or 'cluster="prod"' (default ""). Use it to
//                       scope the dashboard to one scrape job / cluster, or to
//                       adapt to a Prometheus that relabels series. Series labels
//                       are not assumed beyond the metric's own (the queries never
//                       touch `namespace`, so the exported_namespace rename does
//                       not apply); this knob is how you pin the rest.
//   window              SLO rolling window (default "28d"). Needs at least this
//                       much Prometheus retention; for long windows prefer SLO
//                       recording rules and point the panels at those.
//   availabilityTarget  reconcile-availability SLO objective, 0..1 (default 0.99).
//   latencyTarget       reconcile-latency p95 SLO objective, seconds (default 30).
function(
  datasource='prometheus',
  title='JaaS operator',
  selector='',
  window='28d',
  availabilityTarget=0.99,
  latencyTarget=30,
)
  local g = import 'github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet';

  // Numeric TLAs arrive as numbers from the jsonnet defaults, from a
  // JsonnetSnippet's spec.tlas entry marked `code: true`, and from `jsonnet
  // --tla-code`; they arrive as strings from an HTTP query (single values are
  // passed as string TLAs) and from a spec.tlas entry left as a string. Coerce
  // so thresholds stay numeric on every path.
  local toNum(v) = if std.isString(v) then std.parseJson(v) else v;
  local availTarget = toNum(availabilityTarget);
  local latTarget = toNum(latencyTarget);

  // m composes a metric selector that always folds in the `selector` TLA, so
  // every query can be scoped (job/cluster) or adapted to a relabeling Prometheus
  // from one knob. extra is the panel-specific matcher list.
  local m(name, extra=[]) =
    local parts = (if selector == '' then [] else [selector]) + extra;
    if std.length(parts) == 0 then name else '%s{%s}' % [name, std.join(',', parts)];

  // --- SLO definitions (single source of truth for the panels) -------------
  // Reconcile availability SLI: of the reconciles that were actually trying to
  // sync (excluding the intentional Pending/Suspended states), the fraction that
  // reached Ready=True.
  local good = 'sum(rate(%s[%s]))' % [m('jaas_snippet_reconcile_total', ['status="True"']), window];
  local bad = 'sum(rate(%s[%s]))' % [m('jaas_snippet_reconcile_total', ['status="False"', 'reason!~"Suspended|Pending"']), window];
  local availability = '%(good)s / (%(good)s + %(bad)s)' % { good: good, bad: bad };
  // Error budget remaining, normalised to [<0 exhausted .. 1 full].
  local errorBudget = '(%(a)s - %(t)s) / (1 - %(t)s)' % { a: availability, t: std.toString(availTarget) };
  // Reconcile latency p95 (the JsonnetSnippet controller only).
  local latencyP95 = 'histogram_quantile(0.95, sum by (le) (rate(%s[%s])))' % [m('controller_runtime_reconcile_time_seconds_bucket', ['controller="jsonnetsnippet"']), window];

  local prom(expr, legend) =
    g.query.prometheus.new(datasource, expr)
    + g.query.prometheus.withLegendFormat(legend);
  local instant(expr) =
    g.query.prometheus.new(datasource, expr)
    + g.query.prometheus.withInstant(true);
  local steps(pairs) = [
    g.panel.stat.standardOptions.threshold.step.withColor(p.color)
    + g.panel.stat.standardOptions.threshold.step.withValue(p.value)
    for p in pairs
  ];

  // SLO stat: bigger-is-better (availability, budget). Red below the first
  // green step, green at/above it.
  local sloStat(t, desc, unit, expr, thresholds) =
    g.panel.stat.new(t)
    + g.panel.stat.panelOptions.withDescription(desc)
    + g.panel.stat.standardOptions.withUnit(unit)
    + g.panel.stat.standardOptions.thresholds.withSteps(steps(thresholds))
    + g.panel.stat.queryOptions.withTargets([instant(expr)]);
  local ts(t, unit, targets) =
    g.panel.timeSeries.new(t)
    + g.panel.timeSeries.standardOptions.withUnit(unit)
    + g.panel.timeSeries.queryOptions.withTargets(targets);
  local stat(t, unit, targets) =
    g.panel.stat.new(t)
    + g.panel.stat.standardOptions.withUnit(unit)
    + g.panel.stat.queryOptions.withTargets(targets);

  // --- SLO band ------------------------------------------------------------
  local sloPanels = [
    sloStat(
      'Reconcile availability (%s)' % window,
      'Fraction of syncing reconciles that reached Ready=True over the SLO window. Objective: %s.' % availTarget,
      'percentunit',
      availability,
      [{ color: 'red', value: null }, { color: 'green', value: availTarget }],
    ),
    sloStat(
      'Error budget remaining',
      'Share of the availability error budget still unspent. 0 means the budget is exhausted for the window.',
      'percentunit',
      errorBudget,
      [{ color: 'red', value: null }, { color: 'orange', value: 0 }, { color: 'green', value: 0.25 }],
    ),
    sloStat(
      'Reconcile latency p95 (%s)' % window,
      'p95 reconcile duration of the JsonnetSnippet controller. Objective: < %ss.' % latTarget,
      's',
      latencyP95,
      [{ color: 'green', value: null }, { color: 'red', value: latTarget }],
    ),
    g.panel.timeSeries.new('Availability vs objective')
    + g.panel.timeSeries.standardOptions.withUnit('percentunit')
    + g.panel.timeSeries.standardOptions.thresholds.withSteps(steps([{ color: 'red', value: null }, { color: 'green', value: availTarget }]))
    + g.panel.timeSeries.fieldConfig.defaults.custom.thresholdsStyle.withMode('line')
    + g.panel.timeSeries.queryOptions.withTargets([prom(availability, 'availability')]),
  ];

  // --- Operator internals band --------------------------------------------
  local internalPanels = [
    ts('Reconciles by reason', 'ops', [
      prom('sum by (reason) (rate(%s[5m]))' % m('jaas_snippet_reconcile_total'), '{{reason}}'),
    ]),
    ts('Reconcile latency (p95)', 's', [
      prom(latencyP95, 'p95'),
    ]),
    stat('Evaluations in flight', 'short', [
      prom('sum(%s)' % m('jaas_eval_in_flight'), 'in flight'),
      prom('max(%s)' % m('jaas_eval_max_concurrent'), 'ceiling'),
    ]),
    ts('Eval backpressure (rejected/s)', 'ops', [
      prom('sum(rate(%s[5m]))' % m('jaas_eval_unavailable_total'), 'rejected'),
    ]),
    stat('Outstanding timed-out evals', 'short', [
      prom('sum(%s)' % m('jaas_eval_outstanding_timed_out'), 'orphans'),
    ]),
    ts('Rendered bytes (p95)', 'bytes', [
      prom('histogram_quantile(0.95, sum by (le) (rate(%s[5m])))' % m('jaas_snippet_rendered_bytes_bucket'), 'p95'),
    ]),
    ts('Storage sweep failures/s', 'ops', [
      prom('sum(rate(%s[5m]))' % m('jaas_storage_sweep_failures_total'), 'failures'),
    ]),
    ts('Workqueue depth', 'short', [
      prom('sum by (name) (%s)' % m('workqueue_depth', ['controller="jsonnetsnippet"']), '{{name}}'),
    ]),
  ];

  g.dashboard.new(title)
  + g.dashboard.withUid('jaas-operator')
  + g.dashboard.withDescription('JaaS operator SLOs (reconcile availability + error budget, reconcile-latency p95) over the band on top, and the operator internals that explain them below: reconciles by reason, evaluation throughput and backpressure, rendered output size, storage sweeps, and the controller-runtime workqueue. Scope to a job/cluster with the `selector` argument.')
  + g.dashboard.withTags(['jaas', 'operator', 'slo'])
  + g.dashboard.withRefresh('30s')
  + g.dashboard.time.withFrom('now-6h')
  + g.dashboard.withPanels(
    g.util.grid.makeGrid(sloPanels, panelWidth=6, panelHeight=6, startY=0)
    + g.util.grid.makeGrid(internalPanels, panelWidth=12, panelHeight=8, startY=6)
  )

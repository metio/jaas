---
title: Logging
description: JaaS logs through log/slog with configurable level and format; in operator mode controller-runtime's own logs share the same handler. Reading JSON logs with kubectl and jq, and the Helm chart keys that drive it.
tags: [operator, logging, observability]
---

JaaS logs through Go's `log/slog`. Every request, reconcile, and lifecycle event
is a structured record you can filter and parse rather than scrape with a regex.
In operator mode, controller-runtime's own output — leader election, cache sync,
manager startup — flows through the **same** slog handler via the logr bridge
(`ctrl.SetLogger(logr.FromSlogHandler(...))`), so the manager's logs share the
configured level and format instead of emitting controller-runtime's default zap
output.

## The binary

Two flags control logging. They apply in every mode JaaS runs in:

- `--log-level` — `debug`, `info`, `warn`, or `error`. Default `info`.
- `--log-format` — `json` or `text`. Default `json`.

`json` emits one JSON object per line, the right choice for a log pipeline
(Loki, Elasticsearch, Cloud Logging) that indexes structured fields. `text`
emits human-readable key=value lines, handy when tailing logs at a terminal
during local development.

The full flag list with defaults is on the
[configuration page](/installation/configuration/).

### Reading logs

With the default JSON format, pipe `kubectl logs` through `jq`. Tail the
operator and pretty-print:

```shell
kubectl --namespace jaas logs deployment/jaas --follow | jq .
```

Filter to warnings and errors only:

```shell
kubectl --namespace jaas logs deployment/jaas | jq 'select(.level == "WARN" or .level == "ERROR")'
```

Follow a single snippet's reconciles by selecting on the logged fields:

```shell
kubectl --namespace jaas logs deployment/jaas | jq 'select(.namespace == "team-a" and .name == "dashboards")'
```

Turn `--log-level=debug` on temporarily to see per-request evaluation detail and
the operator's reconcile decisions; leave it at `info` in production to keep the
volume down.

## The Helm chart

The chart exposes both flags under `arguments`:

```yaml
arguments:
  # debug, info, warn, error
  logLevel: info
  # json, text
  logFormat: json
```

These map one-to-one onto `--log-level` and `--log-format`. Keep `logFormat:
json` for any cluster whose logs are ingested by a structured pipeline; switch
to `text` only for ad-hoc local clusters where a human reads the raw stream.

---
title: Eval-concurrency saturation
description: The global concurrent-eval cap is full and the operator is shedding new evaluations, typically because a runaway snippet is holding slots past its deadline
tags: [runbooks, troubleshooting, evaluation]
---

The global concurrent-eval cap (`--max-concurrent-evals`, default `max(GOMAXPROCS*4, 16)`) is full and new evaluations are being shed. The cap exists because the synchronous go-jsonnet API has no context-aware cancellation: once an eval starts it runs to natural completion, so an unbounded queue lets a runaway snippet pile up goroutines that outlive every caller's deadline. This state is not tied to a single status `Reason`.

## Symptom

One or more of:

- `JaaSEvalSaturation` is firing — `jaas_eval_in_flight / jaas_eval_max_concurrent` has been above the threshold (default `0.9`) for the alert window.
- `JaaSEvalRejected` is firing — `rate(jaas_eval_unavailable_total[5m])` has been above the threshold.
- HTTP clients see `503 Service Unavailable` with body `{"error": "evaluation_unavailable", "message": "concurrent-eval cap is full; retry after backoff"}`.
- `kubectl describe jsonnetsnippet` shows recurring `Warning EvalUnavailable` events with message `reconcile deferred for 1s by --max-concurrent-evals`. The Ready condition is untouched — backpressure is not failure.
- `jaas_eval_outstanding_timed_out` is also elevated — confirms the runaway-snippet diagnosis: orphaned evals are pinning slots while their parents have already given up.

## Diagnosis: why is the cap full?

The cap fills for two distinct reasons. The right remediation depends on which.

### Path A — runaway snippet (goroutines outliving their ctx)

Read the leak gauge. If it's non-zero and trending up, evaluations are starting but not finishing — almost always a single snippet whose work dwarfs `--evaluation-timeout`.

```shell
# Live count of evals whose parent reconcile already timed out:
kubectl --namespace <jaas-ns> exec deploy/jaas -- \
  wget -qO- http://localhost:8083/metrics | grep jaas_eval_outstanding_timed_out
```

To find the culprit, scan recent reconcile logs for `Jsonnet evaluation timed out` followed by repeated `EvalUnavailable` warnings on the same snippet:

```shell
kubectl --namespace <jaas-ns> logs deploy/jaas --since=15m \
  | grep -E 'EvaluationTimeout|EvalUnavailable' \
  | sort | uniq -c | sort -rn | head
```

The snippet whose name dominates that list is the culprit. Common causes:

- Deep recursion that takes seconds-to-minutes to complete naturally even after the parent deadline fires.
- Pathological library import that triggers go-jsonnet's worst-case eval order.
- A `std.foldl` over a generated array of millions of entries.

### Path B — genuine load above the cap

Leak gauge is at zero (or steady, not growing), `jaas_eval_in_flight` is pegged near the cap, and many distinct snippets show `EvalUnavailable` events. The cap is sized too small for the workload.

```shell
# Distribution of which snippets are seeing rejections — a flat
# distribution across many snippets is path B; a single dominant
# snippet is path A.
kubectl --namespace <jaas-ns> exec deploy/jaas -- \
  wget -qO- http://localhost:8083/metrics \
  | grep jaas_snippet_eval_unavailable_total
```

## Remediation

### Path A — runaway snippet

1. **Suspend the offender** to stop new evals while you fix the snippet:

   ```shell
   kubectl --namespace <ns> patch jsonnetsnippet <name> --type merge \
     --patch '{"spec":{"suspend":true}}'
   ```

2. **Inspect the snippet** to understand the cost. Lower `--max-stack` is a blunt clamp that rejects pathological recursion before it can leak. The chart's `operator.maxStack` defaults to 500; pull it down to ~200 if the snippet doesn't legitimately need deeper recursion.

3. **Tighten `--evaluation-timeout`** if the snippet's natural completion time is the load-bearing factor. A 5s default lets a 60s pathological eval leak for nearly a minute; dropping to 1s shrinks the worst-case leak window.

4. **Re-enable** after the snippet spec is fixed:

   ```shell
   kubectl --namespace <ns> patch jsonnetsnippet <name> --type merge \
     --patch '{"spec":{"suspend":false}}'
   ```

### Path B — genuine load

1. **Raise the cap** if the operator has CPU headroom. The default is `max(GOMAXPROCS*4, 16)`; double it via the chart:

   ```shell
   helm upgrade <release> <chart> --reuse-values \
     --set arguments.maxConcurrentEvals=64
   ```

   Each in-flight eval pins roughly one CPU when actively running, so the practical ceiling is bounded by node CPU. Past 2-3× GOMAXPROCS the gains drop sharply — more contention, same throughput.

2. **Tune the per-snippet rate limiter** if a small number of snippets dominate the request rate. `--rerender-rate` + `--rerender-burst` cap each snippet's reconcile frequency independent of the global eval cap.

3. **Scale horizontally** if a single replica can't keep up even at the raised cap. The chart's `replicas.max` controls the HPA ceiling; combined with the storage layer's leader election (S3 backend) you get multi-replica HA where every replica reads but only the lease-holder writes.

## When NOT to raise the cap

If the leak gauge is non-zero AND growing, raising the cap lets more goroutines pile up before the next saturation event. Diagnose path A first. The cap is a backpressure boundary, not a throughput knob.

## Disable the gate (not recommended)

`--max-concurrent-evals=0` disables the gate entirely. The leak gauge keeps working, but rejections never fire — a single runaway snippet can OOM the pod. Use only if you've sized the workload precisely and want to surface saturation purely via the leak gauge.

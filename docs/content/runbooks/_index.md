---
title: Runbooks
description: Per-reason remediation pages for every Ready-condition the JaaS operator sets, plus cross-cutting incident guides.
tags: [runbooks, troubleshooting]
---

One page per Ready-condition `Reason` the operator sets, plus cross-cutting
incident guides. Each page covers the symptom, the cause, how to diagnose it, and
how to remediate.

The operator automatically appends a link to the matching page on each actionable
Ready-condition message — `(runbook: https://jaas.projects.metio.wtf/runbooks/<reason>/)` —
so `kubectl describe` points straight at the remediation page. Healthy reasons
(`Synced`, `Suspended`, `Pending`) get no link.

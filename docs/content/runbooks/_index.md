---
title: Runbooks
description: Per-reason remediation pages for every Ready-condition the JaaS operator sets, plus cross-cutting incident guides.
tags: [runbooks, troubleshooting]
---

One page per Ready-condition `Reason` the operator sets, plus cross-cutting
incident guides. Each page covers the symptom, the cause, how to diagnose it, and
how to remediate.

Point the operator at a published copy of these pages with `--runbook-base-url`
(for example `https://jaas.projects.metio.wtf/runbooks`); the reason is then
appended to each actionable Ready message so `kubectl describe` links straight to
the remediation page. Healthy reasons (`Synced`, `Suspended`, `Pending`) get no
link.

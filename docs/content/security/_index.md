---
title: Security & multi-tenancy
description: Run JaaS safely — tenant impersonation and RBAC, admission validation, network policy, and service mesh.
tags: [security, multi-tenancy, rbac]
---

How to run JaaS without granting it broad cluster power and how to constrain what
it can reach. In operator mode, snippets resolve under a tenant ServiceAccount, so
a snippet can do exactly what its RBAC allows and no more.

- **[Tenancy and RBAC](/security/tenancy-and-rbac/)** — tenant impersonation and
  the permissions the operator needs.
- **[Admission webhook](/security/admission-webhook/)** — validate resources at
  admission time.
- **[Network policy](/security/network-policy/)** — lock JaaS's traffic down to
  what it needs.
- **[Service mesh](/security/service-mesh/)** — run JaaS inside a mesh.

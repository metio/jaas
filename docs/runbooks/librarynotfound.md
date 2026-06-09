<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: LibraryNotFound

## Symptom

`READY=False`, `REASON=LibraryNotFound`. The Message names the missing library.

## Cause

A `spec.libraries[*]` entry references a `JsonnetLibrary` CR that the operator cannot Get. Common reasons:

- the library CR doesn't exist (typo, wrong namespace, not yet applied)
- the tenant ServiceAccount doesn't have `get` on the library kind in the library's namespace
- the library is in a different namespace and `--no-cross-namespace-refs=true`

## Diagnosis

```shell
# Confirm the library exists.
kubectl get jsonnetlibrary <name> -n <ns>

# Test the tenant's RBAC.
kubectl auth can-i get jsonnetlibrary <name> \
  --as=system:serviceaccount:<ns>:<tenant-sa> -n <library-ns>
```

If `can-i` returns `no`, RBAC is the gap.

## Remediation

- create the library CR if it doesn't exist
- grant `get` on `jsonnetlibraries.jaas.metio.wtf` to the tenant SA via a Role + RoleBinding
- if cross-namespace is intended, either move the library into the snippet's namespace or set `--no-cross-namespace-refs=false` cluster-wide

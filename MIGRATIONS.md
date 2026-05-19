<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

This file contains migration guidelines for updating JaaS on your systems.

# 2026.5.25

The labels in the Deployment and PDB `spec.selector.matchLabels` have changed. Kubernetes treats those fields as immutable, so `helm upgrade` from an older chart version will fail with `field is immutable`.

Delete both resources before upgrading:

```shell
kubectl -n <namespace> delete deployment jaas
kubectl -n <namespace> delete pdb jaas --ignore-not-found
```

Expect a brief outage while the new Pod comes up.

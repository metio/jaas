<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# Jsonnet-as-a-Service (JaaS) Helm Chart

This [Helm chart](https://helm.sh/) defines a minimal JaaS deployment with limited resource usage. See the [values.yaml](./values.yaml) for configuration options.

The chart is published in OCI format and can be downloaded from `oci://ghcr.io/metio/jaas` using `helm`.

Install it with:

```shell
helm install SOME_RELEASE oci://ghcr.io/metio/jaas --version SOME_VERSION
```

Replace `SOME_RELEASE` and `SOME_VERSION` with appropriate values for your environment. In general, we recommend to run the latest available version.

## Usage with grafana-operator

JaaS is intended to be used together with the grafana-operator to manage Grafana dashboards using Jsonnet. However, it can evaluate any kind of Jsonnet, so using it for something else is fine too. See the [official upstream documentation](https://grafana.github.io/grafana-operator/docs/examples/dashboard/jaas/readme/) on how to integrate with the grafana-operator.

## Adding Jsonnet snippets

Once you have your Jsonnet snippets as OCI objects, add them under the `snippets` key in the [values.yaml](./values.yaml) like this:

```yaml
snippets:
  your-dashboard: docker.io/your-org/your-repo:some-tag
  other-dashboard: docker.io/your-org/other-repo:other-tag
  ...
```

## Adding Jsonnet libraries

Add all libraries similarly under the `additionalLibraries` key like this:

```yaml
additionalLibraries:
  your-library: docker.io/your-org/your-library:your-tag
  other-library: docker.io/your-org/other-library:other-tag
  ...
```

## Defining external variables

In order to define external variables (`std.extVar`), use the `externalVariables` key like this:

```yaml
externalVariables:
  your-variable: some-value
  other-variable: something-else
```

If you want to load external variables from either a `ConfigMap` or a `Secret` use the `externalVariablesFrom` key like this:

```yaml
externalVariablesFrom:
  configMaps:
    - some-config-map
  secrets:
    - some-secret
```

In order for environment variables to be picked up from `ConfigMaps` or `Secrets` make sure that they start with `JAAS_EXT_VAR_`.

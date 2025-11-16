# Jsonnet-as-a-Service (JaaS) Helm Chart

This [Helm chart](https://helm.sh/) defines a minimal JaaS deployment with limited resource usage. See the [values.yaml](./values.yaml) for configuration options.

The chart is published in OCI format and can be downloaded from `oci://ghcr.io/metio/jaas` using `helm`.

## Usage with grafana-operator

JaaS is intended to be used together with the grafana-operator to manage Grafana dashboards using Jsonnet. However, it can evaluate any kind of Jsonnet, so using it for something else is fine too.

In general, you need to perform the following steps to template Grafana dashboards with JaaS:

1. Write your dashboards in Jsonnet
2. Publish them as OCI objects
3. Add them to an instance of this chart
4. Use the `.spec.url` field of a `GrafanaDashboard` resource to call your JaaS instance

### Write your dashboards in Jsonnet

The JaaS Helm chart includes [Grafonnet](https://github.com/grafana/grafonnet) and other libraries as [OCI volumes](https://github.com/metio/jsonnet-oci-images) to write Grafana dashboards using Jsonnet. Those OCI volumes all expose Grafonnet like this:

```jsonnet
// using 'latest'
local grafonnet = import "github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet";

// using a specific version
local grafonnet = import "github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet";
```

In general, I highly recommend to use the `latest` version and control the actual version of Grafonnet you want to use by changing it in the [values.yaml](./values.yaml) of this Helm chart. If you follow this recommendation, you only will have to change the version of Grafonnet in one single place instead of touching all of your dashboards every time Grafonnet releases a new version.

At the moment, this chart only allows using a single version of those predefined libraries - open a ticket in case you need more control here.

### Publish them as OCI objects

In order to publish your dashboards as an OCI objects make sure that it contains a file called `/main.jsonnet`. It must be called like that, and it must be in the root folder. An example `Dockerfile` looks like this:

```dockerfile
FROM scratch

COPY src/path/to/dashboards/dashboard.jsonnet /main.jsonnet
```

In case you have split your dashboard definition into multiple files, adjust as necessary so that the `/main.jsonnet` file can find them in the `Dockerfile` as well, e.g., by using relative imports and copying everything into the root folder.

Writing your own Jsonnet library is supported as well, you just need to make sure that you recreate the same folder structure that is used by [jsonnet-bundler](https://github.com/jsonnet-bundler/jsonnet-bundler/) in non-legacy mode, e.g., if your library is in a repository at https://git.example.com/my/jsonnet/library and all your Jsonnet files are in a `src/something` subdirectory of that repository, your `Dockerfile` for your Jsonnet library should look like this:

```dockerfile
FROM scratch

# The fully qualified path is the URL of your repository + the (optional) subfolder of your Jsonnet files
COPY src/something /git.example.com/my/jsonnet/library/src/something
```

If you follow this structure, you can use `jsonnet-bundler` locally to develop your dashboards and use them as-is from JaaS as well. In case you do not care about `jsonnet-bundler`, you are free to choose any structure you want.

There is no restriction on file names for libraries since it's up to you to import them in your dashboards, e.g., the above example library could be imported like this, assuming that there is a file called `main.libsonnet` in `src/something`:

```jsonnet
// Always import grafonnet
local grafonnet = import "github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet";

// Import your own stuff
local something = import "git.example.com/my/jsonnet/library/src/something/main.libsonnet";
```

### Add them to an instance of this chart

Once you have your dashboards and libraries packaged as OCI objects, add all dashboards under the `snippets` key in the [values.yaml](./values.yaml) like this:

```yaml
snippets:
  your-dashboard: docker.io/your-org/your-repo:some-tag
  other-dashboard: docker.io/your-org/other-repo:other-tag
  ...
```

Add all libraries similarly under the `additionalLibraries` key like this:

```yaml
additionalLibraries:
  your-library: docker.io/your-org/your-library:your-tag
  other-library: docker.io/your-org/other-library:other-tag
  ...
```

Once you have modified your `values.yaml` file, run `helm upgrade` (or `helm install` in case you install for the first time) to deploy the changes.

### Use the `.spec.url` field of a `GrafanaDashboard` resource to call your JaaS instance

The Helm chart will create a `Deployment` and an associated `Service` in the Kubernetes namespace your specified. JaaS will then be available from within the cluster at the address `jaas.<NAMESPACE>.svc.cluster.local` (assuming your local cluster address is `cluster.local`) on port `8080`. You can call your JaaS instance using the grafana-operator like this:

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: your-dashboard
spec:
  url: "http://jaas.jaas.svc.cluster.local:8080/jsonnet/your-dashboard"
```

The above example assumes that JaaS was installed in the `jaas` namespace and that you have packaged a Grafana dashboard as described above using the name `your-dashboard`.

Only the grafana-operator needs to communicate with the running JaaS instance, so if you want to define network policies, allow traffic from the grafana-operator to the `http` port of Jaas. JaaS needs no outgoing traffic, therefore a single incoming ingress rule is fine.

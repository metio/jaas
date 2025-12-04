<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# Jsonnet-as-a-Service (JaaS)

JaaS is a webservice that evaluates Jsonnet snippets on the fly.

## Usage

You can find pre-built binaries on our GitHub release page, a container image at `docker.io/metio/jaas:latest`, and a helm chart [here](./helm).

JaaS is controlled through command line flags. The minimal way to run it is just:

```shell
./jaas
```

You can then use the service by sending a GET request to `http://127.0.0.1:8080/jsonnet/<SNIPPET>`. `<SNIPPET>` is the name of Jsonnet snippet you want to evaluate. The service will return the evaluated Jsonnet snippet as JSON.

### Declaring Snippets

Snippets can be declared in two ways:
1. **Directory Snippets**: Specify directories with `-snippet-directory` and place your Jsonnet files in subdirectories of the given directory. For example, if you have a file `main.jsonnet` in a directory `snippet/directory/something`, you can access it via the URL `http://<IP>:<PORT>/jsonnet/something` if `-snippet-directory` is set to `snippet/directory`.
2. **File Snippets**: You can also specify individual Jsonnet files using the `-snippet` flag. For example, if you have a file `path/to/somewhere/something.jsonnet`, you can access it via the URL `http://<IP>:<PORT>/jsonnet/path/to/somewhere/something.jsonnet`.

Consider the `examples` directory of this repository:

```
examples
└── snippets
    ├── dashboards
    │   └── example1
    │       └── main.jsonnet
    └── example.jsonnet
```

Using `-snippet-directory examples/snippets/dashboards` exposes all subdirectories as retrievable snippets, so you can access `example1` via `http://<IP>:<PORT>/jsonnet/example1`.

Similarly, using `-snippet examples/snippets/example.jsonnet` allows you to access the `example.jsonnet` snippet directly via `http://<IP>:<PORT>/jsonnet/examples/snippets/example.jsonnet`.

### Declaring Libraries

Libraries can be declared using the `-library-path` flag. This allows you to specify directories containing Jsonnet libraries that can be used in your snippets. The rightmost matching library will be used if multiple library paths match.

Consider the following directory structure:

```
examples
└── libraries
    └── examplonet
        └── main.libsonnet
```

Using `-library-path examples/libraries` allows you to use the `examplonet` library in your Jsonnet snippets. You can then import it in your Jsonnet files like this:

```jsonnet
local examplonet = import 'examplonet/main.libsonnet';

{
  ...
}
```

### External Variables

You can specify external variables using URL query parameters like this:

- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var2=value2`: Set `var1` to `value1` and `var2` to `value2` for the snippet evaluation.
- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var2`: Set `var1` to `value1` and `var2` to an empty string for the snippet evaluation.
- `http://<IP>:<PORT>/jsonnet/snippet?var1=value1&var1=value2`: Set `var1` to a list containing `value1` and `value2` for the snippet evaluation.

### Command Line Flags

See all available command line flags with `jaas --help`:

```
  -jsonnet-endpoint-path string
    	The path to the jsonnet endpoint (default "jsonnet")
  -library-path value
    	The path of a directory containing jsonnet libraries (can be specified multiple times). Rightmost matching library will be used.
  -listen-address string
    	The listen address to bind to for the Jsonnet server (default "127.0.0.1")
  -log-level string
    	The log level to use (debug, info, warn, error) (default "info")
  -management-listen-address string
    	The listen address to bind to for the management server (default "127.0.0.1")
  -management-port string
    	The port to bind to for the management server (default "8081")
  -management-read-timeout duration
    	maximum duration for reading the entire request, including the body in the management server (default 10s)
  -management-write-timeout duration
    	The maximum duration before timing out writes of the response in the management server (default 10s)
  -port string
    	The port to bind to for the Jsonnet server (default "8080")
  -read-timeout duration
    	maximum duration for reading the entire request, including the body in the Jsonnet server (default 10s)
  -snippet value
    	The path of a jsonnet file or directory containing snippets (can be specified multiple times). Snippets will be loaded from the given path, where the file name is the snippet name.
  -snippet-directory value
    	The path of a directory containing snippets as subdirectories (can be specified multiple times). Snippets will be loaded from subdirectories of the given path, where the directory name is the snippet name.
  -version
    	Print version and exit
  -write-timeout duration
    	The maximum duration before timing out writes of the response in the Jsonnet server (default 10s)
```

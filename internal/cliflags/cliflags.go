/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package cliflags defines the jaas binary's CLI surface in one place so the
// runtime (main.run) and the documentation generator (hack/flaggen) derive
// from a single source. Register declares every flag on a FlagSet, co-locates
// each with its documentation group, and returns the typed value pointers run
// dereferences after parsing; flaggen introspects the same FlagSet without
// dereferencing.
package cliflags

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/pflag"
)

// Groups is the ordered list of flag-group section names. The order here is
// the stable emission order for generated documentation (hack/flaggen) and
// matches the section headings in docs/content/installation/configuration.md.
func Groups() []string {
	return []string{
		"Jsonnet server",
		"Management server",
		"Snippets and libraries",
		"External variables",
		"Evaluation limits",
		"Lifecycle",
		"Operator (Flux integration)",
		"Storage server (local and S3)",
		"S3 flags",
		"Webhook (TLS provisioning)",
		"Leader election",
		"Metrics",
		"Tracing",
		"Logging and lifecycle",
	}
}

// Flags holds pointers to every value registered on the FlagSet. run
// dereferences these after fs.Parse; flaggen never does.
type Flags struct {
	LibraryPaths       *[]string
	Snippets           *[]string
	SnippetDirectories *[]string
	ExtVarFlags        *[]string
	ShowVersion        *bool
	LogLevel           *string
	LogFormat          *string

	ListenAddress       *string
	Port                *string
	JsonnetEndpointPath *string
	WriteTimeout        *time.Duration
	ReadTimeout         *time.Duration

	ManagementListenAddress *string
	ManagementPort          *string
	ManagementWriteTimeout  *time.Duration
	ManagementReadTimeout   *time.Duration

	EvaluationTimeout  *time.Duration
	MaxStack           *int
	MaxConcurrentEvals *int

	ShutdownDelay *time.Duration

	EnableFluxIntegration *bool
	DefaultServiceAccount *string
	NoCrossNamespaceRefs  *bool
	LabelSelector         *string
	WatchNamespaces       *string
	RerenderRate          *string
	RerenderBurst         *int
	Kubeconfig            *string
	MaxWithdrawWait       *time.Duration
	MaxArtifactBytes      *int64
	ArtifactGCGrace       *time.Duration

	StoragePath           *string
	StorageBaseURL        *string
	StorageListenAddress  *string
	StoragePort           *string
	StorageReadTimeout    *time.Duration
	StorageWriteTimeout   *time.Duration
	StorageSweepInterval  *time.Duration
	StorageSweepMaxTmpAge *time.Duration
	StorageBackend        *string

	S3Endpoint     *string
	S3Bucket       *string
	S3Prefix       *string
	S3Region       *string
	S3UseSSL       *bool
	S3AccessKey    *string
	S3SecretKey    *string
	S3SessionToken *string
	S3Anonymous    *bool

	EnableWebhook           *bool
	WebhookCertDir          *string
	WebhookPort             *int
	WebhookCertMode         *string
	WebhookServiceName      *string
	WebhookServiceNamespace *string
	WebhookVWCName          *string
	WebhookCertValidity     *time.Duration

	LeaderElection          *bool
	LeaderElectionID        *string
	LeaderElectionNamespace *string

	MetricsBindAddress *string

	EnableMCP      *bool
	MCPBindAddress *string

	TracingEndpoint    *string
	TracingInsecure    *bool
	TracingSampleRatio *float64
}

// DefaultFunc supplies a flag's dynamic default. Register takes one for
// --max-concurrent-evals so the runtime can pass its GOMAXPROCS-derived value
// while the doc generator passes a stub (the generator never reads it, and the
// published default is the symbolic formula — see hack/flaggen).
type DefaultFunc func() int

// Validate rejects flag values pflag accepts as well-typed but that are
// semantically invalid: negative caps, ports outside 1..65535, and negative
// durations. pflag only type-coerces, so these surface here. run maps a non-nil
// result to exit code 2 (flag-usage error). Zero is a valid, documented value
// for the caps (disable / use the engine default), so only negatives are
// rejected.
func (f *Flags) Validate() error {
	nonNegInts := []struct {
		name string
		val  int
	}{
		{"--max-stack", *f.MaxStack},
		{"--max-concurrent-evals", *f.MaxConcurrentEvals},
		{"--rerender-burst", *f.RerenderBurst},
	}
	for _, c := range nonNegInts {
		if c.val < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", c.name, c.val)
		}
	}
	if *f.MaxArtifactBytes < 0 {
		return fmt.Errorf("--max-artifact-bytes must be >= 0, got %d", *f.MaxArtifactBytes)
	}

	stringPorts := []struct {
		name string
		val  string
	}{
		{"--port", *f.Port},
		{"--management-port", *f.ManagementPort},
		{"--storage-port", *f.StoragePort},
	}
	for _, p := range stringPorts {
		n, err := strconv.Atoi(p.val)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s must be an integer in 1..65535, got %q", p.name, p.val)
		}
	}
	if *f.WebhookPort < 1 || *f.WebhookPort > 65535 {
		return fmt.Errorf("--webhook-port must be in 1..65535, got %d", *f.WebhookPort)
	}

	durations := []struct {
		name string
		val  time.Duration
	}{
		{"--write-timeout", *f.WriteTimeout},
		{"--read-timeout", *f.ReadTimeout},
		{"--management-write-timeout", *f.ManagementWriteTimeout},
		{"--management-read-timeout", *f.ManagementReadTimeout},
		{"--evaluation-timeout", *f.EvaluationTimeout},
		{"--shutdown-delay", *f.ShutdownDelay},
		{"--max-withdraw-wait", *f.MaxWithdrawWait},
		{"--artifact-gc-grace", *f.ArtifactGCGrace},
		{"--storage-read-timeout", *f.StorageReadTimeout},
		{"--storage-write-timeout", *f.StorageWriteTimeout},
		{"--storage-sweep-interval", *f.StorageSweepInterval},
		{"--storage-sweep-max-tmp-age", *f.StorageSweepMaxTmpAge},
		{"--webhook-cert-validity", *f.WebhookCertValidity},
	}
	for _, d := range durations {
		if d.val < 0 {
			return fmt.Errorf("%s must be >= 0, got %s", d.name, d.val)
		}
	}
	return nil
}

// Register declares every CLI flag on fs, co-locates each with its
// documentation group via the "group" annotation, and returns a struct of the
// registered value pointers. maxConcurrentEvalsDefault supplies the dynamic
// default for --max-concurrent-evals so the runtime and the generator can pass
// their own (the generator never reads the computed value).
func Register(fs *pflag.FlagSet, maxConcurrentEvalsDefault DefaultFunc) *Flags {
	f := &Flags{}

	f.LibraryPaths = fs.StringArray("library-path", nil, "The path of a directory containing jsonnet libraries (can be specified multiple times). Rightmost matching library will be used.")
	f.Snippets = fs.StringArray("snippet", nil, "The path of a jsonnet file or directory containing snippets (can be specified multiple times). Snippets will be loaded from the given path, where the file name is the snippet name.")
	f.SnippetDirectories = fs.StringArray("snippet-directory", nil, "The path of a directory containing snippets as subdirectories (can be specified multiple times). Snippets will be loaded from subdirectories of the given path, where the directory name is the snippet name.")
	f.ExtVarFlags = fs.StringArray("ext-var", nil, "External variable as KEY=VALUE for std.extVar lookups (can be specified multiple times). Takes precedence over JAAS_EXT_VAR_* env vars on conflict.")
	f.ShowVersion = fs.Bool("version", false, "Print version and exit")
	f.LogLevel = fs.String("log-level", "info", "The log level to use (debug, info, warn, error)")
	f.LogFormat = fs.String("log-format", "json", "The log output format to use (json, text)")
	f.ListenAddress = fs.String("listen-address", "127.0.0.1", "The listen address to bind to for the Jsonnet server")
	f.Port = fs.String("port", "8080", "The port to bind to for the Jsonnet server")
	f.JsonnetEndpointPath = fs.String("jsonnet-endpoint-path", "jsonnet", "The path to the jsonnet endpoint")
	f.WriteTimeout = fs.Duration("write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the Jsonnet server")
	f.ReadTimeout = fs.Duration("read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the Jsonnet server")
	f.ManagementListenAddress = fs.String("management-listen-address", "127.0.0.1", "The listen address to bind to for the management server")
	f.ManagementPort = fs.String("management-port", "8081", "The port to bind to for the management server")
	f.ManagementWriteTimeout = fs.Duration("management-write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the management server")
	f.ManagementReadTimeout = fs.Duration("management-read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the management server")
	f.EvaluationTimeout = fs.Duration("evaluation-timeout", 5*time.Second, "Maximum duration a single Jsonnet evaluation is allowed to take. Set to 0 to disable.")
	f.MaxStack = fs.Int("max-stack", 500, "Maximum Jsonnet call-stack depth. Set to 0 to use go-jsonnet's default.")
	f.MaxConcurrentEvals = fs.Int("max-concurrent-evals", maxConcurrentEvalsDefault(), "Maximum number of in-flight Jsonnet evaluations. Excess requests return 503 (HTTP) or RequeueAfter (operator). Set to 0 to disable. Defaults to max(GOMAXPROCS*4, 16).")
	f.ShutdownDelay = fs.Duration("shutdown-delay", 5*time.Second, "Time to wait after readiness flips to false before initiating graceful shutdown; gives Kubernetes time to propagate the not-ready status to endpoint controllers. Set to 0 to disable.")

	f.EnableFluxIntegration = fs.Bool("enable-flux-integration", false, "Boot the Kubernetes operator that watches JsonnetSnippet / JsonnetLibrary CRs and publishes evaluated results as Flux ExternalArtifacts.")
	f.DefaultServiceAccount = fs.String("default-service-account", "", "ServiceAccount the operator impersonates when a JsonnetSnippet has no spec.serviceAccountName. Empty rejects such snippets at reconcile time.")
	f.NoCrossNamespaceRefs = fs.Bool("no-cross-namespace-refs", true, "When true (default), reject JsonnetSnippet / library CRs that reference a SourceRef in a different namespace.")
	f.LabelSelector = fs.String("label-selector", "", "Narrow the operator's watch to CRs matching this label selector. Empty selects every CR in the watched scope.")
	f.WatchNamespaces = fs.String("watch-namespaces", "", "Comma-separated list of namespaces this operator watches. Empty (the default) means cluster-wide. When set, the manager's cache only observes CRs in these namespaces — multi-tenant operator-instances pattern. Falls back to JAAS_WATCH_NAMESPACES env var when the flag is empty.")
	f.RerenderRate = fs.String("rerender-rate", "60/min", "Per-snippet steady-state re-render budget, as N/period (sec|min|hour). Token-bucket combined with --rerender-burst.")
	f.RerenderBurst = fs.Int("rerender-burst", 120, "Per-snippet token-bucket depth for re-render rate limiting.")
	f.Kubeconfig = fs.String("kubeconfig", "", "Path to a kubeconfig file for the operator. Empty falls back to KUBECONFIG env, then to in-cluster service-account credentials.")
	f.StoragePath = fs.String("storage-path", "", "Directory the operator writes ExternalArtifact tarballs to. Required when --enable-flux-integration is set.")
	f.StorageBaseURL = fs.String("storage-base-url", "", "Public URL prefix the operator's storage HTTP server serves tarballs at. Required when --enable-flux-integration is set.")
	f.StorageListenAddress = fs.String("storage-listen-address", "0.0.0.0", "The listen address to bind to for the storage HTTP server")
	f.StoragePort = fs.String("storage-port", "8082", "The port to bind to for the storage HTTP server")
	f.StorageReadTimeout = fs.Duration("storage-read-timeout", 30*time.Second, "Maximum duration for reading the entire request on the storage server.")
	f.StorageWriteTimeout = fs.Duration("storage-write-timeout", 5*time.Minute, "Maximum duration before timing out writes of the response on the storage server. Tarballs can be MBs, so this is generous by default.")
	f.EnableWebhook = fs.Bool("enable-webhook", false, "Boot the validating admission webhook for JsonnetSnippet. Requires --enable-flux-integration and a TLS cert/key in --webhook-cert-dir.")
	f.WebhookCertDir = fs.String("webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory holding the TLS cert (tls.crt) and key (tls.key) the webhook server presents.")
	f.WebhookPort = fs.Int("webhook-port", 9443, "Port the validating webhook server binds to.")
	f.WebhookCertMode = fs.String("webhook-cert-mode", "cert-manager", "How the webhook's TLS material is provisioned: cert-manager (chart renders a Certificate; cert injected via Secret mount), or self-signed (operator generates a CA + serving cert in-pod and patches the ValidatingWebhookConfiguration's caBundle).")
	f.WebhookServiceName = fs.String("webhook-service-name", "jaas-webhook", "Service name the webhook is reachable through. Used to build cert SANs when --webhook-cert-mode=self-signed.")
	f.WebhookServiceNamespace = fs.String("webhook-service-namespace", "", "Namespace the webhook Service lives in. Empty falls back to --leader-election-namespace, then to in-cluster downward API.")
	f.WebhookVWCName = fs.String("webhook-validating-config-name", "", "Name of the ValidatingWebhookConfiguration whose caBundle this operator patches. Required when --webhook-cert-mode=self-signed.")
	f.WebhookCertValidity = fs.Duration("webhook-cert-validity", 365*24*time.Hour, "Validity of the self-signed serving cert. Operators that want short-lived rotation should use cert-manager instead.")
	f.LeaderElection = fs.Bool("leader-election", true, "Enable controller-runtime leader election so only one operator replica reconciles at a time. Honored only when --enable-flux-integration is set.")
	f.LeaderElectionID = fs.String("leader-election-id", "jaas-operator", "Lease object name used for leader election. Must be unique across JaaS installations sharing a namespace.")
	f.LeaderElectionNamespace = fs.String("leader-election-namespace", "", "Namespace holding the leader-election Lease. Empty defaults to the operator pod's namespace.")
	f.MetricsBindAddress = fs.String("metrics-bind-address", ":8083", "Bind address for the controller-runtime Prometheus metrics endpoint. Use \"0\" to disable. The default avoids the conflict between controller-runtime's built-in :8080 and the jsonnet HTTP server.")
	f.EnableMCP = fs.Bool("enable-mcp", false, "Serve the operator's read tools over the Model Context Protocol (streamable HTTP). Requires --enable-flux-integration.")
	f.MCPBindAddress = fs.String("mcp-bind-address", ":8084", "Bind address for the MCP streamable-HTTP server. Only used when --enable-mcp is set; chosen to avoid the jsonnet (:8080), management (:8081), storage (:8082), and metrics (:8083) ports.")
	f.StorageBackend = fs.String("storage-backend", "local", "Artifact backend the operator publishes ExternalArtifact tarballs through. local (default; emptyDir/PVC) or s3 (any S3-compatible object store; pairs with leader election for HA across replicas).")
	f.S3Endpoint = fs.String("s3-endpoint", "", "S3 service host:port (e.g. s3.amazonaws.com or minio.minio.svc:9000). Required when --storage-backend=s3.")
	f.S3Bucket = fs.String("s3-bucket", "", "S3 bucket the artifacts live in. Must already exist. Required when --storage-backend=s3.")
	f.S3Prefix = fs.String("s3-prefix", "", "Optional object-key prefix prepended under the bucket, so jaas can coexist with other tenants in one bucket.")
	f.S3Region = fs.String("s3-region", "", "S3 region the bucket lives in. Required for AWS multi-region setups; ignored by most S3-compatible servers.")
	f.S3UseSSL = fs.Bool("s3-use-ssl", true, "Talk HTTPS to the S3 endpoint. Set to false only for local MinIO over HTTP.")
	f.S3AccessKey = fs.String("s3-access-key", "", "Static AWS_ACCESS_KEY_ID. Empty triggers the IAM/IRSA discovery chain (AWS_*, web-identity, EC2 metadata).")
	f.S3SecretKey = fs.String("s3-secret-key", "", "Static AWS_SECRET_ACCESS_KEY. Pairs with --s3-access-key.")
	f.S3SessionToken = fs.String("s3-session-token", "", "Optional AWS_SESSION_TOKEN, paired with --s3-access-key/--s3-secret-key for temporary credentials.")
	f.S3Anonymous = fs.Bool("s3-anonymous", false, "Skip request signing entirely. Only useful against a public bucket — test/dev only.")
	f.MaxWithdrawWait = fs.Duration("max-withdraw-wait", 1*time.Hour, "Bound the time a deleted JsonnetSnippet's finalizer can hold while Publisher.Withdraw keeps failing. Past this, the operator emits a Warning WithdrawForced event, drops the finalizer, and lets the snippet be garbage-collected — possibly leaving an orphan tarball in storage. Required so a permanently-broken backend doesn't block namespace teardown.")
	f.MaxArtifactBytes = fs.Int64("max-artifact-bytes", 0, "Cap the published artifact content size in bytes (rendered output in Output=rendered mode, the whole source tree in Output=source mode). Snippets exceeding this fail with ReasonArtifactTooLarge. Zero disables.")
	f.ArtifactGCGrace = fs.Duration("artifact-gc-grace", 5*time.Minute, "Minimum time a superseded artifact revision is retained after being evicted from the keep-set. Closes the pin→fetch race in which a Flux consumer reads status.artifact a moment before the operator garbage-collects the superseded revision. Zero disables and restores eager pruning. The deletion path (snippet teardown) is unaffected.")
	f.StorageSweepInterval = fs.Duration("storage-sweep-interval", 10*time.Minute, "How often the operator sweeps orphaned <rev>.tar.gz.tmp residue left by Puts whose process died mid-rename. Zero disables.")
	f.StorageSweepMaxTmpAge = fs.Duration("storage-sweep-max-tmp-age", 30*time.Minute, "Minimum age before an orphaned .tmp file is eligible for sweep. Wider than the longest plausible in-flight Put to avoid racing live writers.")
	f.TracingEndpoint = fs.String("tracing-endpoint", "", "OTLP gRPC collector host:port (e.g. otel-collector.observability.svc:4317). Empty disables tracing entirely.")
	f.TracingInsecure = fs.Bool("tracing-insecure", false, "Skip TLS when dialing the OTLP collector. Use only for in-cluster collectors that don't terminate TLS themselves.")
	f.TracingSampleRatio = fs.Float64("tracing-sample-ratio", 1.0, "TraceID-ratio sampling (0.0..1.0). 1.0 samples every trace.")

	// Co-locate each flag with its documentation group. The group names match
	// Groups() and the section headings in the configuration reference page;
	// flaggen reads this annotation to bucket flags into their rendered tables.
	groups := map[string][]string{
		"Jsonnet server":         {"listen-address", "port", "jsonnet-endpoint-path", "read-timeout", "write-timeout"},
		"Management server":      {"management-listen-address", "management-port", "management-read-timeout", "management-write-timeout"},
		"Snippets and libraries": {"snippet", "snippet-directory", "library-path"},
		"External variables":     {"ext-var"},
		"Evaluation limits":      {"evaluation-timeout", "max-stack", "max-concurrent-evals"},
		"Lifecycle":              {"shutdown-delay"},
		"Operator (Flux integration)": {
			"enable-flux-integration", "default-service-account", "no-cross-namespace-refs",
			"label-selector", "watch-namespaces", "rerender-rate", "rerender-burst",
			"kubeconfig", "max-withdraw-wait", "max-artifact-bytes", "artifact-gc-grace",
		},
		"Storage server (local and S3)": {
			"storage-path", "storage-base-url", "storage-backend", "storage-listen-address",
			"storage-port", "storage-read-timeout", "storage-write-timeout",
			"storage-sweep-interval", "storage-sweep-max-tmp-age",
		},
		"S3 flags": {
			"s3-endpoint", "s3-bucket", "s3-prefix", "s3-region", "s3-use-ssl",
			"s3-access-key", "s3-secret-key", "s3-session-token", "s3-anonymous",
		},
		"Webhook (TLS provisioning)": {
			"enable-webhook", "webhook-cert-mode", "webhook-cert-dir", "webhook-port",
			"webhook-service-name", "webhook-service-namespace",
			"webhook-validating-config-name", "webhook-cert-validity",
		},
		"Leader election":       {"leader-election", "leader-election-id", "leader-election-namespace"},
		"Metrics":               {"metrics-bind-address"},
		"MCP":                   {"enable-mcp", "mcp-bind-address"},
		"Tracing":               {"tracing-endpoint", "tracing-insecure", "tracing-sample-ratio"},
		"Logging and lifecycle": {"log-level", "log-format", "version"},
	}
	for group, names := range groups {
		for _, name := range names {
			// SetAnnotation errors only on an unknown flag name, which here
			// would be a programmer typo in the map above.
			if err := fs.SetAnnotation(name, "group", []string{group}); err != nil {
				panic(err)
			}
		}
	}

	return f
}

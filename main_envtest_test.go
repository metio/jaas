/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// syncBuffer is a bytes.Buffer safe for concurrent Write and String. A test that
// boots the operator manager and then reads the captured log output can race
// controller-runtime's shutdown logging, which a background goroutine emits via
// the slog handler after run() returns. A plain bytes.Buffer is not safe for
// that concurrent Write/read; in production the sink is os.Stdout, where this
// does not arise.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// envtestMainSetup spins up a one-off envtest with the JaaS CRDs installed
// and writes a kubeconfig for it to a tempfile. Both are returned so the
// test can pass the file path to `run` via -kubeconfig.
func envtestMainSetup(t *testing.T) (kubeconfigPath string) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	crdDir := mainCRDDir(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := env.Start(); err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	user, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("envtest AddUser: %v", err)
	}
	bytes, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("envtest KubeConfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, bytes, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// mainCRDDir resolves the config/crd/bases path relative to this test
// file via runtime.Caller so the lookup works regardless of caller cwd.
// envtest points at the controller-gen output directly (no chart-style
// templating to strip).
func mainCRDDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(here)
	return filepath.Join(repoRoot, "config", "crd", "bases")
}

// TestRun_FluxIntegration_BindFailureOnStoragePort returns 1: a pre-bound
// port can't be claimed by the storage server, which exits the process
// before any goroutine runs.
func TestRun_FluxIntegration_BindFailureOnStoragePort(t *testing.T) {
	kubeconfig := envtestMainSetup(t)

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	blockedPort := strconv.Itoa(blocker.Addr().(*net.TCPAddr).Port)

	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--listen-address=127.0.0.1",
		"--port=" + freePort(t),
		"--management-listen-address=127.0.0.1",
		"--management-port=" + freePort(t),
		"--shutdown-delay=0",
		"--enable-flux-integration",
		"--kubeconfig=" + kubeconfig,
		"--storage-path=" + t.TempDir(),
		"--storage-base-url=http://x",
		"--storage-listen-address=127.0.0.1",
		"--storage-port=" + blockedPort,
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Cannot bind storage listener") {
		t.Errorf("expected 'Cannot bind storage listener' in logs; got %q", stdout.String())
	}
}

// TestRun_FluxIntegration_InvalidWebhookCertModeFailsWithExit2 pins the
// --webhook-cert-mode enum: an unrecognised mode is a usage error caught by
// Flags.Validate at parse time (exit 2, naming the accepted values) before any
// kubeconfig load or listener bind — so it needs no apiserver.
func TestRun_FluxIntegration_InvalidWebhookCertModeFailsWithExit2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--enable-flux-integration",
		"--enable-webhook",
		"--webhook-cert-mode=bogus",
		"--storage-path=" + t.TempDir(),
		"--storage-base-url=http://example.test/artifacts",
	}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "webhook-cert-mode") {
		t.Errorf("stderr = %q, want it to name the invalid --webhook-cert-mode", stderr.String())
	}
}

// TestRun_FluxIntegration_SelfSignedRequiresVWCName pins that the self-signed
// cert mode rejects a missing --webhook-validating-config-name (exit 1) before
// it tries to provision anything — the named VWC is where the issued CA bundle
// is stamped, so the mode is inoperable without it.
func TestRun_FluxIntegration_SelfSignedRequiresVWCName(t *testing.T) {
	kubeconfig := envtestMainSetup(t)

	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--listen-address=127.0.0.1",
		"--port=" + freePort(t),
		"--management-listen-address=127.0.0.1",
		"--management-port=" + freePort(t),
		"--shutdown-delay=0",
		"--enable-flux-integration",
		"--enable-webhook",
		"--webhook-cert-mode=self-signed",
		"--webhook-validating-config-name=",
		"--kubeconfig=" + kubeconfig,
		"--storage-path=" + t.TempDir(),
		"--storage-base-url=http://example.test/artifacts",
		"--leader-election-namespace=default",
		"--metrics-bind-address=0",
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "Invalid --webhook-validating-config-name") {
		t.Errorf("stderr = %q, want it to name the missing --webhook-validating-config-name", stderr.String())
	}
}

// TestRun_FluxIntegration_BootsAgainstEnvtestAndShutsDownCleanly is the
// end-to-end envtest for the main entry point: launches `run()` with
// --enable-flux-integration pointing at a real apiserver, waits for the
// management probes to come up (i.e. operator initialized successfully),
// then SIGTERMs and asserts a clean exit code.
//
// The metrics endpoint wiring is covered by
// TestRunWithBuilder_PropagatesMetricsBindAddress in internal/operator;
// proving that controller-runtime actually binds and serves Prometheus
// text from that address is an upstream contract, not ours.
func TestRun_FluxIntegration_BootsAgainstEnvtestAndShutsDownCleanly(t *testing.T) {
	kubeconfig := envtestMainSetup(t)

	jsonnetPort := freePort(t)
	mgmtPort := freePort(t)
	storagePort := freePort(t)
	storagePath := t.TempDir()

	args := []string{
		"--listen-address=127.0.0.1",
		"--port=" + jsonnetPort,
		"--management-listen-address=127.0.0.1",
		"--management-port=" + mgmtPort,
		"--shutdown-delay=0",
		"--enable-flux-integration",
		"--kubeconfig=" + kubeconfig,
		"--storage-path=" + storagePath,
		"--storage-base-url=http://example.test/artifacts",
		"--storage-listen-address=127.0.0.1",
		"--storage-port=" + storagePort,
		// envtest runs outside a pod, so controller-runtime's downward-
		// API namespace lookup fails. Set the LE namespace explicitly.
		"--leader-election-namespace=default",
		// Disable the metrics server so a parallel run doesn't fight
		// over the chart-default ":8083" port.
		"--metrics-bind-address=0",
	}

	sigs := make(chan os.Signal, 1)
	done := make(chan int, 1)
	var stdout, stderr syncBuffer

	withRestoredSlogDefault(t)
	go func() {
		done <- run(args, nil, &stdout, &stderr, sigs)
	}()

	// Probes online ⇒ operator manager + all three HTTP servers booted.
	waitForReady(t, "127.0.0.1:"+mgmtPort, 30*time.Second)

	// The storage HTTP server serves an empty 404 for missing paths but
	// the socket itself must be accepting connections.
	storageURL := "http://127.0.0.1:" + storagePort + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", storageURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET storage root: %v", err)
	}
	resp.Body.Close()
	// Any status (200 from a FileServer over an empty dir is OK,
	// 404 also fine) — what matters is the dial succeeded.

	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0; stdout=%s; stderr=%s",
				code, stdout.String(), stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return within 30s")
	}

	// Sanity: the logs prove the operator wired up its scheme.
	if !strings.Contains(stdout.String(), "Operator manager ready") {
		t.Errorf("expected 'Operator manager ready' in logs; got %q", stdout.String())
	}
}

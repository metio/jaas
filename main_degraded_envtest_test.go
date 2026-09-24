/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// apiserverGate is a TCP proxy in front of the envtest apiserver whose
// reachability a test can switch at will. Closed, it accepts a connection and
// drops it, which is what an apiserver behind a NetworkPolicy that admits no
// egress looks like from the pod. Open, it forwards, so the same process sees
// the apiserver appear without being restarted.
//
// It proxies TCP rather than terminating TLS, so the client's handshake still
// runs against the real apiserver certificate. Only the port changes, and the
// envtest serving certificate covers 127.0.0.1, so verification is unaffected.
type apiserverGate struct {
	listener net.Listener
	target   string
	open     atomic.Bool
	wg       sync.WaitGroup

	// Forwarded connections are tracked so teardown can close them. A watch
	// that the client leaves in its idle pool keeps its copy loops blocked for
	// as long as the peer holds the socket open, and waiting that out would add
	// a minute and a half to the test.
	mu    sync.Mutex
	conns []net.Conn
}

func (g *apiserverGate) track(conns ...net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conns = append(g.conns, conns...)
}

func (g *apiserverGate) closeTracked() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.conns {
		_ = c.Close()
	}
	g.conns = nil
}

func newAPIServerGate(t *testing.T, target string) *apiserverGate {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gate listen: %v", err)
	}
	g := &apiserverGate{listener: l, target: target}
	g.wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			if !g.open.Load() {
				_ = conn.Close()
				continue
			}
			g.wg.Go(func() { g.forward(conn) })
		}
	})
	t.Cleanup(func() {
		_ = l.Close()
		g.closeTracked()
		g.wg.Wait()
	})
	return g
}

func (g *apiserverGate) forward(client net.Conn) {
	defer func() { _ = client.Close() }()
	upstream, err := net.DialTimeout("tcp", g.target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	g.track(client, upstream)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(upstream, client) })
	wg.Go(func() { _, _ = io.Copy(client, upstream) })
	wg.Wait()
}

func (g *apiserverGate) addr() string { return g.listener.Addr().String() }

// envtestGatedSetup boots envtest and writes a kubeconfig that reaches it only
// through the returned gate.
func envtestGatedSetup(t *testing.T) (kubeconfigPath string, gate *apiserverGate) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{mainCRDDir(t)},
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
	raw, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("envtest KubeConfig: %v", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}
	if len(cfg.Clusters) != 1 {
		t.Fatalf("kubeconfig has %d clusters, want 1", len(cfg.Clusters))
	}
	for _, cluster := range cfg.Clusters {
		upstream := strings.TrimPrefix(cluster.Server, "https://")
		gate = newAPIServerGate(t, upstream)
		cluster.Server = "https://" + gate.addr()
	}
	rewritten, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("write kubeconfig file: %v", err)
	}
	return path, gate
}

// mgmtGet returns the status code and body of a GET against the management
// server. A code of 0 means the request itself failed.
func mgmtGet(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func mgmtStatus(t *testing.T, addr, path string) int {
	t.Helper()
	code, _ := mgmtGet(t, addr, path)
	return code
}

// awaitStatus polls until path answers with want. The failure names the last
// body seen, which for /operator is the reason the operator is down.
func awaitStatus(t *testing.T, addr, path string, want int, timeout time.Duration) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(timeout)
	last, body := -1, ""
	for time.Now().Before(deadline) {
		last, body = mgmtGet(t, addr, path)
		if last == want {
			// The wait is logged because these transitions are paced by the
			// supervisor's backoff, which is the interesting number when this
			// test turns slow.
			t.Logf("GET %s returned %d after %v", path, want, time.Since(started).Round(time.Millisecond))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d within %v (last %d: %s)", path, want, timeout, last, body)
}

// TestRun_OperatorDegradesWhileTheApiserverIsUnreachable covers both
// transitions the degraded operator has to survive.
//
// While the apiserver cannot be reached, the process keeps running and the
// Jsonnet renderer keeps answering: neither needs a cluster, and exiting would
// hand the kubelet a restart loop for as long as the cause lasts. /operator
// reports the failure, and readiness stays down because this pod has never
// reconciled, so a rollout stops here rather than replacing a working replica.
//
// Once the apiserver becomes reachable, the manager comes up in the same
// process — no restart — and both signals flip.
func TestRun_OperatorDegradesWhileTheApiserverIsUnreachable(t *testing.T) {
	kubeconfig, gate := envtestGatedSetup(t)

	metricsAddr := "127.0.0.1:" + freePort(t)
	h := runInBackground(t, []string{
		"--enable-flux-integration",
		"--kubeconfig=" + kubeconfig,
		"--storage-path=" + t.TempDir(),
		"--storage-base-url=http://example.test/artifacts",
		"--storage-listen-address=127.0.0.1",
		"--storage-port=" + freePort(t),
		"--leader-election=false",
		"--metrics-bind-address=" + metricsAddr,
		"--snippet=examples/snippets/example.jsonnet",
		"--library-path=examples/libraries",
	}, nil)

	// The operator reports itself down, with a reason.
	awaitStatus(t, h.mgmt, "/operator", http.StatusServiceUnavailable, 60*time.Second)

	// Liveness is unconditional, so the kubelet never restarts the pod over
	// this; readiness is the signal that stops a rollout.
	if got := mgmtStatus(t, h.mgmt, "/live"); got != http.StatusOK {
		t.Errorf("GET /live = %d, want %d while the operator is down", got, http.StatusOK)
	}
	if got := mgmtStatus(t, h.mgmt, "/ready"); got != http.StatusServiceUnavailable {
		t.Errorf("GET /ready = %d, want %d for a pod whose operator has never synced", got, http.StatusServiceUnavailable)
	}

	// The core function is unaffected.
	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/examples/snippets/example.jsonnet")
	if err != nil {
		t.Fatalf("GET /jsonnet: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /jsonnet = %d, want %d while the operator is down; body=%s", resp.StatusCode, http.StatusOK, body)
	}

	// The gauge that an alert would fire on is readable in the state it
	// describes, which is only true because jaas binds that listener itself.
	metricsBody := scrapeMetrics(t, metricsAddr)
	if !strings.Contains(metricsBody, "jaas_operator_available 0") {
		t.Errorf("metrics do not report jaas_operator_available 0; got %q", excerpt(metricsBody, "jaas_operator"))
	}

	// Now let the apiserver through.
	gate.open.Store(true)
	awaitStatus(t, h.mgmt, "/operator", http.StatusOK, 90*time.Second)
	awaitStatus(t, h.mgmt, "/ready", http.StatusOK, 30*time.Second)

	metricsBody = scrapeMetrics(t, metricsAddr)
	if !strings.Contains(metricsBody, "jaas_operator_available 1") {
		t.Errorf("metrics do not report jaas_operator_available 1 after recovery; got %q", excerpt(metricsBody, "jaas_operator"))
	}

	h.shutdown(t, 0)
}

// TestRun_ReadinessNeverRequiresOperatorKeepsThePodInService pins the opt-out:
// with --readiness-requires-operator=never the renderer stays in its Service
// through an apiserver outage, and /operator remains the place the outage shows.
func TestRun_ReadinessNeverRequiresOperatorKeepsThePodInService(t *testing.T) {
	kubeconfig, _ := envtestGatedSetup(t)

	h := runInBackground(t, []string{
		"--enable-flux-integration",
		"--readiness-requires-operator=never",
		"--kubeconfig=" + kubeconfig,
		"--storage-path=" + t.TempDir(),
		"--storage-base-url=http://example.test/artifacts",
		"--storage-listen-address=127.0.0.1",
		"--storage-port=" + freePort(t),
		"--leader-election=false",
		"--metrics-bind-address=0",
	}, nil)

	awaitStatus(t, h.mgmt, "/operator", http.StatusServiceUnavailable, 60*time.Second)
	if got := mgmtStatus(t, h.mgmt, "/ready"); got != http.StatusOK {
		t.Errorf("GET /ready = %d, want %d with --readiness-requires-operator=never", got, http.StatusOK)
	}

	h.shutdown(t, 0)
}

// scrapeMetrics reads the metrics endpoint the binary bound.
func scrapeMetrics(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	return string(body)
}

// excerpt returns the lines of s containing substr, for a readable failure.
func excerpt(s, substr string) string {
	var kept []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, substr) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

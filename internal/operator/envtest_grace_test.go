/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// TestEnvtest_SourceRefContract_PinsBackPointerShape pins the public
// contract Publisher.setSpec documents: the published ExternalArtifact's
// spec.sourceRef is exactly {apiVersion: jaas.metio.wtf/v1, kind:
// JsonnetSnippet, name: <snippet name>}, no namespace. Producer-aware
// consumers (stageset-controller) reverse-resolve on this triple, so a
// silent drift here breaks every downstream that does the lookup.
func TestEnvtest_SourceRefContract_PinsBackPointerShape(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "contract", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, true)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("ExternalArtifact not created: %v", err)
	}

	// Same-namespace publishing — half of the contract. Producer-
	// aware consumers scope reverse lookups to the artifact's
	// namespace and trust the snippet resolves there.
	if ea.GetNamespace() != ns {
		t.Errorf("ExternalArtifact.namespace = %q, want %q (same-namespace publishing is part of the contract)",
			ea.GetNamespace(), ns)
	}
	if ea.GetName() != snip.Name {
		t.Errorf("ExternalArtifact.name = %q, want %q (same-name publishing is part of the contract)",
			ea.GetName(), snip.Name)
	}

	sourceRef, found, err := unstructured.NestedMap(ea.Object, "spec", "sourceRef")
	if err != nil {
		t.Fatalf("read spec.sourceRef: %v", err)
	}
	if !found {
		t.Fatal("spec.sourceRef missing — the back-pointer is part of the public contract")
	}

	wantAPIVersion := jaasv1.GroupVersion.String()
	if v, ok := sourceRef["apiVersion"].(string); !ok || v != wantAPIVersion {
		t.Errorf("spec.sourceRef.apiVersion = %v, want %q", sourceRef["apiVersion"], wantAPIVersion)
	}
	if v, ok := sourceRef["kind"].(string); !ok || v != "JsonnetSnippet" {
		t.Errorf("spec.sourceRef.kind = %v, want %q", sourceRef["kind"], "JsonnetSnippet")
	}
	if v, ok := sourceRef["name"].(string); !ok || v != snip.Name {
		t.Errorf("spec.sourceRef.name = %v, want %q", sourceRef["name"], snip.Name)
	}
	if _, hasNamespace := sourceRef["namespace"]; hasNamespace {
		t.Errorf("spec.sourceRef.namespace must remain unset — same-namespace publishing is implicit")
	}
	if len(sourceRef) != 3 {
		t.Errorf("spec.sourceRef has %d fields (%v), want exactly 3 (apiVersion, kind, name)",
			len(sourceRef), sourceRef)
	}
}

// TestEnvtest_RevisionURLImmutability_RetainedRevisionStaysFetchable
// is the second half of the spec.sourceRef contract: revision-
// addressed artifact URLs are immutable while the revision is in the
// keep-set. stageset-controller's planned `rollbackOnFailure`
// re-fetches previously recorded URLs and digest-verifies them; a
// storage refactor that rewrote URLs (different host prefix, embedded
// generation counter, anything) for retained revisions would silently
// break rollback even though every individual reconcile looks fine.
//
// Publish rev A (history=2), capture rev A's recorded URL + digest,
// publish rev B, then fetch the originally-recorded URL_A over HTTP
// and re-hash the body. Identical-bytes means the storage layer's
// URL→content mapping is stable, the file on disk is byte-for-byte
// what it was, and the recorded digest still verifies.
func TestEnvtest_RevisionURLImmutability_RetainedRevisionStaysFetchable(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	storeRoot := t.TempDir()
	store, err := storage.New(storeRoot)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Real HTTP server in front of the store so the test fetches via
	// the same code path source-controller / kustomize-controller would.
	httpSrv := httptest.NewServer(store.HTTPHandler())
	t.Cleanup(httpSrv.Close)

	r := &SnippetReconciler{
		Client: c,
		Scheme: envtestScheme(t),
		Logger: discardLoggerEnvtest(),
	}
	r.Publisher = NewPublisher(store, httpSrv.URL)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "rollback", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ rev: "A" }`},
			},
			Output:  jaasv1.OutputRendered,
			History: 2, // both rev A and rev B must stay fetchable
		},
	}
	key := applyJsonnetSnippet(t, c, snip)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// Snapshot the EA status JaaS wrote for rev A — its URL and
	// digest. This is the wire-level contract: whatever a Flux
	// consumer reads here must keep resolving identically while the
	// revision is in the keep-set.
	urlA, digestA := readArtifactURLAndDigest(t, c, key)
	if urlA == "" || digestA == "" {
		t.Fatalf("rev A status.artifact incomplete: url=%q digest=%q", urlA, digestA)
	}
	if !strings.HasPrefix(urlA, httpSrv.URL+"/") {
		t.Fatalf("rev A URL %q does not start with the test server URL %q", urlA, httpSrv.URL)
	}

	// Publish rev B; with history=2 the rev A tarball must stay on
	// disk and its URL must remain valid (no rename, no rewrite).
	if err := mutateSnippetFiles(c, key, map[string]string{"main.jsonnet": `{ rev: "B" }`}); err != nil {
		t.Fatalf("update snippet to rev B: %v", err)
	}
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// Refetch the EA — status.artifact now points at rev B; we'll
	// use the captured rev A URL.
	urlB, _ := readArtifactURLAndDigest(t, c, key)
	if urlB == urlA {
		t.Fatalf("rev B URL equals rev A URL %q — different revisions must produce different URLs", urlA)
	}

	// And the status.history must list both revisions, head-first.
	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.History) != 2 {
		t.Fatalf("status.history has %d entries, want 2 (rev B, rev A)", len(got.Status.History))
	}

	// Fetch rev A through the originally-recorded URL — the URL a
	// consumer that pinned rev A would still hold — and re-hash.
	body := httpGetBytes(t, urlA)
	got256 := sha256.Sum256(body)
	gotDigest := "sha256:" + hex.EncodeToString(got256[:])
	if gotDigest != digestA {
		t.Errorf("rev A digest drifted while retained in keep-set:\n  want %s\n  got  %s",
			digestA, gotDigest)
	}
}

// readArtifactURLAndDigest pulls status.artifact.url and
// status.artifact.digest off the live ExternalArtifact for key.
// Returns ("", "") when either is missing — the caller decides
// whether that's a failure.
func readArtifactURLAndDigest(t *testing.T, c client.Client, key types.NamespacedName) (string, string) {
	t.Helper()
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("get ExternalArtifact: %v", err)
	}
	url, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "url")
	digest, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "digest")
	return url, digest
}

// httpGetBytes does a GET against url, fails the test on any non-2xx,
// and returns the body bytes. Used by the URL-immutability test to
// verify a retained revision still serves the same content over HTTP.
func httpGetBytes(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// TestEnvtest_ArtifactGCGrace_RetainsThenPrunes drives the publish →
// re-render → fetch-within-grace → pruned-past-grace flow against a
// real envtest apiserver and a real on-disk Store. Uses the real
// wall-clock against a small grace so file mtimes (always real wall-
// clock from Put) and the comparison clock agree without per-file
// chtimes bookkeeping.
func TestEnvtest_ArtifactGCGrace_RetainsThenPrunes(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	storeRoot := t.TempDir()
	store, err := storage.New(storeRoot)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const grace = 250 * time.Millisecond

	r := &SnippetReconciler{
		Client: c,
		Scheme: envtestScheme(t),
		Logger: discardLoggerEnvtest(),
	}
	r.Publisher = NewPublisher(store, "http://jaas-storage.test.svc.cluster.local:8082")
	r.Publisher.GCGrace = grace

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "grace", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ rev: 1 }`},
			},
			Output:  jaasv1.OutputRendered,
			History: 1, // keep-set holds only the head — eviction kicks in on rev 2
		},
	}
	key := applyJsonnetSnippet(t, c, snip)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	firstTarball := singleTarballFilename(t, storeRoot, ns, "grace")

	// Re-render immediately. With grace=250ms, the second reconcile's
	// supersession proxy is "now-ish" and the older tarball must
	// survive — closing the pin→fetch race for consumers that read
	// status.artifact a moment before the rewrite.
	if err := mutateSnippetFiles(c, key, map[string]string{"main.jsonnet": `{ rev: 2 }`}); err != nil {
		t.Fatalf("update snippet to rev 2: %v", err)
	}
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)
	if _, err := os.Stat(filepath.Join(storeRoot, ns, "grace", firstTarball)); err != nil {
		t.Fatalf("first tarball gone within grace window — pin→fetch race not closed: %v", err)
	}

	// Wait past grace, trigger a third reconcile, and the first
	// tarball must be pruned. The next re-render is the natural
	// occasion for the expiry check; suspended/interval-driven
	// reconciles cover snippets that never re-render.
	time.Sleep(grace + 100*time.Millisecond)
	if err := mutateSnippetFiles(c, key, map[string]string{"main.jsonnet": `{ rev: 3 }`}); err != nil {
		t.Fatalf("update snippet to rev 3: %v", err)
	}
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)
	if _, err := os.Stat(filepath.Join(storeRoot, ns, "grace", firstTarball)); !os.IsNotExist(err) {
		t.Errorf("first tarball still present past grace: %v", err)
	}
}

// singleTarballFilename returns the only `.tar.gz` filename under
// <root>/<ns>/<name>/. Fails the test if the directory holds zero or
// more than one tarball, which means the test fixture is broken.
func singleTarballFilename(t *testing.T, root, ns, name string) string {
	t.Helper()
	dir := filepath.Join(root, ns, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read storage dir %s: %v", dir, err)
	}
	var tarballs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			tarballs = append(tarballs, e.Name())
		}
	}
	if len(tarballs) != 1 {
		t.Fatalf("expected exactly one tarball under %s, got %v", dir, tarballs)
	}
	return tarballs[0]
}

// mutateSnippetFiles refetches the snippet and replaces spec.files.
// Wraps the Get→edit→Update cycle so callers don't have to deal with
// resourceVersion bookkeeping inline.
func mutateSnippetFiles(c client.Client, key types.NamespacedName, files map[string]string) error {
	var s jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &s); err != nil {
		return err
	}
	s.Spec.Files = files
	return c.Update(context.Background(), &s)
}

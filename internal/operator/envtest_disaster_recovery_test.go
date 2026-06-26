/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// TestReconcile_StorageLossRePublishesIdenticalArtifact pins the core
// disaster-recovery guarantee: the artifact store is a regeneratable
// cache, not source of truth. The JsonnetSnippet spec (kept in Git under
// GitOps) is authoritative, and the Publisher's Put is deterministic and
// idempotent — same namespace/name/revision renders to byte-identical
// tarball bytes and therefore the same sha256 digest.
//
// The test reconciles a snippet to publish a tarball, deletes the tarball
// off the on-disk store to simulate storage loss (PVC wiped, S3 bucket
// emptied), forces another reconcile, and asserts the tarball is
// re-materialized at the same path with the same digest and the
// ExternalArtifact is Ready=True again. This is the invariant the
// cluster-rebuild checklist in docs/content/installation/disaster-recovery.md
// relies on: restore the CRs, and the derived store rebuilds itself with
// no manual re-render step.
func TestReconcile_StorageLossRePublishesIdenticalArtifact(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	// A known store root (not directReconciler's hidden TempDir) so the
	// test can locate and delete the tarball on disk.
	storeRoot := t.TempDir()
	store, err := storage.New(storeRoot)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	r := &SnippetReconciler{
		Client: c,
		Scheme: envtestScheme(t),
		Logger: discardLoggerEnvtest(),
	}
	r.Publisher = NewPublisher(store, "http://jaas-storage.test.svc.cluster.local:8082")

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "recover", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ recovered: true, value: 42 }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	// First reconcile: render + publish. Capture revision + digest off
	// the ExternalArtifact status — these must survive storage loss.
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	_, digestBefore := readArtifactURLAndDigest(t, c, key)
	if digestBefore == "" {
		t.Fatal("first publish wrote no artifact digest")
	}

	tarball := singleTarballFilename(t, storeRoot, ns, "recover")
	tarballPath := filepath.Join(storeRoot, ns, "recover", tarball)
	if _, err := os.Stat(tarballPath); err != nil {
		t.Fatalf("tarball not on disk after first publish: %v", err)
	}

	// Simulate storage loss: wipe the snippet's tarball directory the way
	// a lost PVC or emptied bucket would.
	if err := os.RemoveAll(filepath.Join(storeRoot, ns, "recover")); err != nil {
		t.Fatalf("simulate storage loss: %v", err)
	}
	if _, err := os.Stat(tarballPath); !os.IsNotExist(err) {
		t.Fatalf("tarball still present after simulated loss: %v", err)
	}

	// Force another reconcile by bumping an annotation. The Publisher
	// re-renders from the (untouched) spec and writes the tarball back.
	bumpReconcileAnnotation(t, c, key)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// The tarball must be re-materialized at the same path.
	if _, err := os.Stat(tarballPath); err != nil {
		t.Fatalf("tarball not re-materialized after storage loss: %v", err)
	}

	// And byte-for-byte the same artifact — same digest — because the
	// spec did not change. This is the idempotency guarantee that makes
	// the store a safe-to-lose cache.
	_, digestAfter := readArtifactURLAndDigest(t, c, key)
	if digestAfter != digestBefore {
		t.Errorf("artifact digest drifted across storage loss + re-publish:\n  before %s\n  after  %s",
			digestBefore, digestAfter)
	}

	// The ExternalArtifact is Ready=True again — downstream consumers
	// recover automatically once the re-render lands.
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("get ExternalArtifact after recovery: %v", err)
	}
	if !externalArtifactReady(ea) {
		t.Error("ExternalArtifact is not Ready=True after recovery re-publish")
	}
}

// bumpReconcileAnnotation refetches the snippet and stamps a reconcile
// annotation so the next Reconcile call does fresh work. Mirrors the
// kubectl annotate force-reconcile the disaster-recovery runbook documents.
func bumpReconcileAnnotation(t *testing.T, c client.Client, key types.NamespacedName) {
	t.Helper()
	var s jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &s); err != nil {
		t.Fatalf("refetch for annotation bump: %v", err)
	}
	ann := s.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["jaas.metio.wtf/reconcile-at"] = "2026-06-19T00:00:00Z"
	s.SetAnnotations(ann)
	if err := c.Update(context.Background(), &s); err != nil {
		t.Fatalf("bump reconcile annotation: %v", err)
	}
}

// externalArtifactReady reports whether the ExternalArtifact carries a
// Ready=True status condition.
func externalArtifactReady(ea *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(ea.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		return s == "True"
	}
	return false
}

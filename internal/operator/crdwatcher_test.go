/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
)

func TestCRDWatchBackoff_DoublesPerAttempt(t *testing.T) {
	base := crdWatchInitialDelay
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, base},
		{1, 2 * base},
		{3, 8 * base},
		{5, 32 * base},
	}
	for _, tc := range cases {
		if got := crdWatchBackoff(tc.attempt); got != tc.want {
			t.Errorf("crdWatchBackoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func establishedCRD(name string) *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: apiextv1.CustomResourceDefinitionStatus{
			Conditions: []apiextv1.CustomResourceDefinitionCondition{
				{Type: apiextv1.Established, Status: apiextv1.ConditionTrue},
			},
		},
	}
}

func notEstablishedCRD(name string) *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: apiextv1.CustomResourceDefinitionStatus{
			Conditions: []apiextv1.CustomResourceDefinitionCondition{
				{Type: apiextv1.Established, Status: apiextv1.ConditionFalse},
			},
		},
	}
}

func TestMatchedCRD_RelevantAndEstablishedMatches(t *testing.T) {
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	gvk, ok := w.matchedCRD(establishedCRD("gitrepositories.source.toolkit.fluxcd.io"))
	if !ok {
		t.Fatal("expected match")
	}
	if gvk.Kind != "GitRepository" {
		t.Errorf("got %v, want GitRepository", gvk)
	}
}

func TestMatchedCRD_RelevantButNotEstablishedSkipped(t *testing.T) {
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	if _, ok := w.matchedCRD(notEstablishedCRD("gitrepositories.source.toolkit.fluxcd.io")); ok {
		t.Error("matched a not-yet-established CRD")
	}
}

func TestMatchedCRD_NoConditionsAtAllSkipped(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "gitrepositories.source.toolkit.fluxcd.io"},
	}
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	if _, ok := w.matchedCRD(crd); ok {
		t.Error("matched a CRD with no conditions")
	}
}

func TestMatchedCRD_UnrelatedCRDSkipped(t *testing.T) {
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	if _, ok := w.matchedCRD(establishedCRD("widgets.example.com")); ok {
		t.Error("matched an unrelated CRD")
	}
}

func TestMatchedCRD_NonCRDObjectSkipped(t *testing.T) {
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	if _, ok := w.matchedCRD(&metav1.PartialObjectMetadata{}); ok {
		t.Error("matched a non-CRD object")
	}
}

func TestMatchedCRD_KindNotInKnownNamesIsSkipped(t *testing.T) {
	// A "Receiver" GVK is conceivable but not in our fluxSourceCRDNames
	// map; the watcher must skip it even if it's been added to w.kinds.
	w := &crdWatcher{kinds: []schema.GroupVersionKind{
		{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "Receiver"},
	}}
	if _, ok := w.matchedCRD(establishedCRD("receivers.source.toolkit.fluxcd.io")); ok {
		t.Error("matched an unknown Flux kind")
	}
}

func TestMatchedCRD_AllKnownFluxKindsMappedToCRDNames(t *testing.T) {
	for _, kind := range FluxSourceKinds {
		w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK(kind)}}
		name, ok := fluxSourceCRDNames[kind]
		if !ok {
			t.Errorf("FluxSourceKind %s has no entry in fluxSourceCRDNames", kind)
			continue
		}
		if _, matched := w.matchedCRD(establishedCRD(name)); !matched {
			t.Errorf("matchedCRD failed for kind %s (CRD %s)", kind, name)
		}
	}
}

func TestIsCRDEstablished_NoConditions(t *testing.T) {
	if isCRDEstablished(&apiextv1.CustomResourceDefinition{}) {
		t.Error("empty conditions reported Established")
	}
}

func TestIsCRDEstablished_OnlyNamesAccepted(t *testing.T) {
	crd := &apiextv1.CustomResourceDefinition{
		Status: apiextv1.CustomResourceDefinitionStatus{
			Conditions: []apiextv1.CustomResourceDefinitionCondition{
				{Type: apiextv1.NamesAccepted, Status: apiextv1.ConditionTrue},
			},
		},
	}
	if isCRDEstablished(crd) {
		t.Error("NamesAccepted-only reported Established")
	}
}

func TestIsCRDEstablished_EstablishedTrue(t *testing.T) {
	crd := establishedCRD("x.example.com")
	if !isCRDEstablished(crd) {
		t.Error("Established=True misread")
	}
}

func TestCRDWatcher_NoMissingKindsBlocksUntilContextCanceled(t *testing.T) {
	w := &crdWatcher{} // empty kinds
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit after cancel")
	}
}

// recordingEngager records every EngageFluxWatch call so tests can
// assert the dynamic-watch handoff fired the way they expected.
type recordingEngager struct {
	calls []schema.GroupVersionKind
	err   error
}

func (r *recordingEngager) EngageFluxWatch(_ context.Context, gvk schema.GroupVersionKind) error {
	r.calls = append(r.calls, gvk)
	return r.err
}

// TestCRDWatcher_RejectsNilEngager ensures Start refuses to run without
// the dynamic-watch dependency wired.
func TestCRDWatcher_RejectsNilEngager(t *testing.T) {
	w := &crdWatcher{kinds: []schema.GroupVersionKind{fluxSourceGVK("GitRepository")}}
	err := w.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for nil engager")
	}
}

// TestCRDWatcher_CacheSyncFailureDegradesNotFatal pins that a cache-sync
// failure does NOT propagate out of Start as a fatal error — that would take
// down the whole manager (including the working snippet/library reconcilers),
// contradicting the watcher's "boot cleanly without Flux CRDs" purpose. Start
// must log a warning and return nil; dynamic engagement is disabled until the
// process restarts.
func TestCRDWatcher_CacheSyncFailureDegradesNotFatal(t *testing.T) {
	orig := waitForCacheSync
	t.Cleanup(func() { waitForCacheSync = orig })
	waitForCacheSync = func(_ <-chan struct{}, _ ...toolscache.InformerSynced) bool {
		return false // simulate a never-syncing informer
	}

	w := &crdWatcher{
		restCfg: &rest.Config{Host: "http://127.0.0.1:1"},
		kinds:   []schema.GroupVersionKind{fluxSourceGVK("GitRepository")},
		engager: &recordingEngager{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start returned %v on cache-sync failure, want nil (degrade, not crash)", err)
	}
}

// TestCRDWatcher_NoMissingKindsIgnoresEngager keeps the original
// invariant: when there are no missing kinds, Start blocks without
// touching the engager (so Run is safe even when no Flux source is
// expected to ever land).
func TestCRDWatcher_NoMissingKindsIgnoresEngager_Engager(t *testing.T) {
	rec := &recordingEngager{}
	w := &crdWatcher{engager: rec} // empty kinds
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := w.Start(ctx); err != nil {
		t.Errorf("Start returned %v, want nil", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("EngageFluxWatch called %d times with no kinds", len(rec.calls))
	}
}

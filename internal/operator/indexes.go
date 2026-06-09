/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// Field-index names used by the watch event handlers. Wire-stable inside
// this package — callers and tests both reference them as constants.
const (
	// snippetByLibraryRefIndex maps a JsonnetSnippet to every library it
	// references. Used by mapJsonnetLibrary to translate a Library event
	// into the snippets that need re-reconciling.
	snippetByLibraryRefIndex = "spec.libraries[].id"
	// snippetBySourceRefIndex maps a JsonnetSnippet to the Flux source
	// (GitRepository / OCIRepository / Bucket / ExternalArtifact) named
	// in spec.sourceRef. Used by mapFluxSource for the direct edge.
	snippetBySourceRefIndex = "spec.sourceRef.id"
	// libraryBySourceRefIndex mirrors snippetBySourceRefIndex for the
	// indirect chain: when a Flux source updates, every library whose own
	// spec.sourceRef points at it needs its dependent snippets enqueued.
	libraryBySourceRefIndex = "spec.sourceRef.id"
)

// registerWatchIndexes installs the field indexers the watch handlers
// rely on. Called once per manager during SetupWithManager. Adding new
// watches that need O(1) lookup goes here.
func registerWatchIndexes(ctx context.Context, fi client.FieldIndexer) error {
	if err := fi.IndexField(ctx, &jaasv1.JsonnetSnippet{}, snippetByLibraryRefIndex,
		snippetLibraryRefIDs); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, &jaasv1.JsonnetSnippet{}, snippetBySourceRefIndex,
		snippetSourceRefIDs); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, &jaasv1.JsonnetLibrary{}, libraryBySourceRefIndex,
		librarySourceRefIDs); err != nil {
		return err
	}
	return nil
}

// snippetLibraryRefIDs extracts the index keys for every library a snippet
// references. The key shape is "<kind>|<namespace>|<name>". A JsonnetLibrary
// ref with an empty namespace defaults to the snippet's own namespace, matching
// resolveLibraries' resolution rule. Returns nil for non-snippet objects.
func snippetLibraryRefIDs(obj client.Object) []string {
	snip, ok := obj.(*jaasv1.JsonnetSnippet)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(snip.Spec.Libraries))
	for _, ref := range snip.Spec.Libraries {
		if ref.Kind == "" || ref.Name == "" {
			continue
		}
		ns := ref.Namespace
		if ns == "" {
			ns = snip.Namespace
		}
		out = append(out, libID(ref.Kind, ns, ref.Name))
	}
	return out
}

// snippetSourceRefIDs returns the index key for a snippet's direct
// spec.sourceRef. Snippets with no sourceRef return nil — they index
// against no key, so a Flux source event never enqueues them via the
// direct edge.
func snippetSourceRefIDs(obj client.Object) []string {
	snip, ok := obj.(*jaasv1.JsonnetSnippet)
	if !ok {
		return nil
	}
	return sourceRefIDs(snip.Spec.SourceRef, snip.Namespace)
}

// librarySourceRefIDs is the same shape as snippetSourceRefIDs but for
// JsonnetLibrary.spec.sourceRef. Both indexers share sourceRefIDs so the
// key encoding stays in lock-step across the snippet and library halves
// of the dependency graph.
func librarySourceRefIDs(obj client.Object) []string {
	lib, ok := obj.(*jaasv1.JsonnetLibrary)
	if !ok {
		return nil
	}
	return sourceRefIDs(lib.Spec.SourceRef, lib.Namespace)
}

// sourceRefIDs encodes a spec.sourceRef as a single index key. Returns
// nil when ref is nil or missing a kind/name — those refs can't match
// any source event anyway. Namespace defaults to ownerNs when empty,
// matching the cross-namespace defaulting the reconciler applies. The
// snippet's spec.sourceRef.apiVersion is deliberately NOT part of the
// key: a Kubernetes CRD has at most one storage version per cluster, so
// (Kind, namespace, name) uniquely identifies the underlying object
// regardless of which schema version a snippet pins. Including
// apiVersion in the key was a correctness bug — SetupWithManager
// registers Flux source watches against a single version (v1) and events
// arrive stamped with that version, but a snippet pinning v1beta2 in
// its spec.sourceRef.apiVersion would index under v1beta2 and never
// match the live event.
func sourceRefIDs(ref *jaasv1.SourceRef, ownerNs string) []string {
	if ref == nil || ref.Kind == "" || ref.Name == "" {
		return nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = ownerNs
	}
	return []string{sourceRefIndexKey(ref.Kind, ns, ref.Name)}
}

// sourceRefIndexKey composes the field-index key from the three parts of
// a SourceRef identity that uniquely identify the underlying Kubernetes
// object. The watched-kind set (FluxSourceKinds + ExternalArtifact) is
// uniquely named within source.toolkit.fluxcd.io, so Kind alone resolves
// the group/version ambiguity for our purposes.
func sourceRefIndexKey(kind, namespace, name string) string {
	return kind + "|" + namespace + "|" + name
}

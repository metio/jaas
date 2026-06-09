/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// withReconcilerIndexes installs the same field indexers SetupWithManager
// registers, but onto a fake client builder. Unit tests that exercise
// mapJsonnetLibrary / mapFluxSource (or any other indexed lookup) chain
// this between WithScheme and Build so MatchingFields queries resolve
// the same way they do against the real cache.
func withReconcilerIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.
		WithIndex(&jaasv1.JsonnetSnippet{}, snippetByLibraryRefIndex, snippetLibraryRefIDs).
		WithIndex(&jaasv1.JsonnetSnippet{}, snippetBySourceRefIndex, snippetSourceRefIDs).
		WithIndex(&jaasv1.JsonnetLibrary{}, libraryBySourceRefIndex, librarySourceRefIDs)
}

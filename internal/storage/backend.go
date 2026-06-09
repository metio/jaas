/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Backend is the pluggable artifact-store contract the reconciler's Publisher
// writes against. Two production implementations satisfy it:
//
//   - *Store (this package) — single-pod filesystem store backed by an
//     emptyDir or PVC, surfaced over HTTP via http.FileServer.
//   - *S3Backend (this package) — multi-pod-safe object store fronting
//     any S3-compatible endpoint, surfaced over HTTP via a streaming
//     proxy. Pairs with leader-election so only the lease-holder writes.
//
// Backend's contract holds across both impls: Put is idempotent on
// (namespace,name,revision); Prune drops every revision other than
// keepRevision; Delete removes everything under (namespace,name);
// HTTPHandler returns a handler rooted at the artifact directory tree so
// downstream consumers fetch tarballs by relative path.
type Backend interface {
	// Put writes the artifact under (namespace, name, revision). ctx
	// cancellation aborts the underlying write (S3 multipart upload,
	// filesystem rename). Filesystem implementations ignore ctx —
	// disk writes are fast enough that operator shutdown can wait for
	// completion — but S3 implementations honor it so SIGTERM,
	// leader handoff, and per-reconcile cancellation propagate
	// instead of holding the operator open for the full upload
	// timeout.
	Put(ctx context.Context, namespace, name, revision string, entries []FileEntry) (Result, error)

	// Prune removes every artifact under <namespace>/<name>/ that does
	// NOT match one of keepRevisions, subject to a per-revision grace
	// window. Callers track the keep-set externally (typically from
	// JsonnetSnippet.Status.History). An empty keepRevisions slice is a
	// no-op — Prune never wipes all revisions; use Delete for that.
	//
	// grace is the minimum time a non-keep revision is retained after it
	// was superseded. Supersession time is derived from storage metadata
	// (the earliest mtime of any newer artifact under the same key) so
	// the window survives operator restarts without extra bookkeeping.
	// grace == 0 disables the window and prunes eagerly — the historical
	// behavior; grace > 0 closes the pin→fetch race in which a consumer
	// reads status.artifact and dereferences the URL only after the
	// operator pruned the superseded revision.
	Prune(ctx context.Context, namespace, name string, keepRevisions []string, grace time.Duration) error

	Delete(ctx context.Context, namespace, name string) error

	// Sweep removes orphaned `<revision>.tar.gz.tmp` files left behind
	// by Puts whose process died after writing the tmpfile but before
	// the rename to the final name. Only `.tmp` files older than
	// maxTmpAge are removed so an in-flight Put isn't stomped on.
	// Returns the count removed. S3-style backends where Put is
	// already atomic return (0, nil).
	Sweep(ctx context.Context, maxTmpAge time.Duration) (int, error)

	// HTTPHandler returns the handler the storage HTTP server mounts at
	// "/". The returned handler is safe for concurrent use.
	HTTPHandler() http.Handler

	// Close releases any resources held by the backend. For the
	// filesystem impl this closes *os.Root; for S3 it's a no-op.
	Close() error
}

// HTTPHandler on *Store serves the on-disk artifact tree via
// http.FileServerFS over the store's os.Root view. Serving through
// os.Root (rather than http.Dir/os.DirFS) means the read path inherits
// the same no-escape traversal guard the write path has: a symlink
// planted under the root that points outside it — on a shared PVC, or by
// a co-tenant on the node — is refused rather than followed to an
// out-of-root target. Wrapped with a suffix-allowlist so only
// `<rev>.tar.gz` requests reach the file server — directory listings
// (which would enumerate every published snippet's revision history to
// any in-cluster caller that can reach this port) and unrelated paths
// return 404. The allowlist matches the only filename shape
// `Backend.Put` ever produces.
func (s *Store) HTTPHandler() http.Handler {
	fileServer := http.FileServerFS(s.fs.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

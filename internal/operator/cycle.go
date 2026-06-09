/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// cycleCache memoizes the cycle-detection verdict per snippet UID,
// invalidated by snippet generation OR by an external signal when a
// dependency CR changes.
//
// Cache key: snippet UID. Stored value: (generation, verdict, path) plus
// an epoch counter that increments on every Forget. A reconcile that
// finds entry.generation == snip.Generation can skip the BFS walk and
// reuse the verdict.
//
// Invalidation: mapJsonnetLibrary + mapFluxSource Forget the entries for
// snippets they enqueue — a library or upstream-source change does not
// bump the dependent snippet's Generation, so the cache cannot rely on
// generation alone to catch transitively-introduced cycles. The
// finalizer path Forgets a snippet's entry on delete.
//
// Forget vs. in-flight walk: Lookup returns the per-UID epoch alongside
// the verdict; Store includes the epoch the caller observed and writes
// only when the epoch is unchanged. A Forget landing between Lookup and
// Store bumps the epoch and causes the post-walk Store to drop the
// (now-stale) write. The caller (cycleVerdict) retries the walk so the
// invalidating event isn't silently absorbed.
type cycleCache struct {
	mu      sync.Mutex
	entries map[types.UID]cycleVerdict
	epochs  map[types.UID]int64
}

type cycleVerdict struct {
	generation int64
	hasCycle   bool
	path       string
}

func newCycleCache() *cycleCache {
	return &cycleCache{
		entries: map[types.UID]cycleVerdict{},
		epochs:  map[types.UID]int64{},
	}
}

// Lookup returns the cached verdict when one exists for uid AND matches
// generation, plus the per-UID epoch the caller must hand back to Store
// to prove no Forget invalidated the entry between the two calls. A
// generation mismatch (or absence) returns miss but still surfaces the
// current epoch so the caller's walk-then-Store path can detect concurrent
// invalidation.
func (c *cycleCache) Lookup(uid types.UID, generation int64) (cycleVerdict, int64, bool) {
	if c == nil || uid == "" {
		return cycleVerdict{}, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	epoch := c.epochs[uid]
	v, ok := c.entries[uid]
	if !ok || v.generation != generation {
		return cycleVerdict{}, epoch, false
	}
	return v, epoch, true
}

// Store records the verdict for uid at the supplied generation, provided
// the per-UID epoch hasn't moved since epochAtLookup. A mismatch means a
// Forget landed mid-walk and the verdict the caller computed may be
// stale; Store drops the write and returns false. nil-receiver / empty
// uid quietly returns false too.
func (c *cycleCache) Store(uid types.UID, generation int64, epochAtLookup int64, hasCycle bool, path string) bool {
	if c == nil || uid == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epochs[uid] != epochAtLookup {
		return false
	}
	c.entries[uid] = cycleVerdict{generation: generation, hasCycle: hasCycle, path: path}
	return true
}

// Forget evicts the cached verdict for uid and bumps the per-UID epoch.
// Any concurrent Store for this uid carrying the pre-Forget epoch will
// drop its write — preserving the invalidation against an in-flight
// walk that already passed the cache's Lookup. nil-safe.
func (c *cycleCache) Forget(uid types.UID) {
	if c == nil || uid == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
	c.epochs[uid]++
}

// hasCycleSourceEdge reports whether snip carries any spec.sourceRef or
// spec.libraries entry that could participate in an ExternalArtifact-backed
// dependency chain. When false, the BFS in detectSourceRefCycle has nothing
// to follow and the verdict is trivially "no cycle"; reconcilers use this
// to fast-path the common case (inline files, or a non-EA sourceRef like
// GitRepository / OCIRepository / Bucket, with no library refs).
//
// Library refs short-circuit to true because the library's spec.sourceRef
// may itself point at an ExternalArtifact — we have to walk to know.
func hasCycleSourceEdge(snip *jaasv1.JsonnetSnippet) bool {
	if snip == nil {
		return false
	}
	if len(snip.Spec.Libraries) > 0 {
		return true
	}
	if snip.Spec.SourceRef != nil && snip.Spec.SourceRef.Kind == "ExternalArtifact" {
		return true
	}
	return false
}

// detectSourceRefCycle walks the dependency graph of snip and reports a
// cycle when any path leads back to snip. Edges followed:
//
//   - Snippet → Snippet.spec.sourceRef when Kind == ExternalArtifact
//     (the EA is named after the publishing snippet, so the target snippet
//     identity is (sourceRef.namespace, sourceRef.name))
//   - Snippet → Library → Library.spec.sourceRef when the library's source
//     is an ExternalArtifact. Library Gets use the operator client; the
//     library's resolved sourceRef.Namespace defaults to the CURRENT
//     snippet's namespace, matching resolveSnippetSource's ownerNs.
//
// Returns (cycleFound, path, transientErr). Path renders as
// "ns/a → ns/b → ns/a" so operators can see exactly where the loop closes.
// Library Gets that return NotFound are treated as leaves (no further
// chain); other errors propagate so the caller requeues.
func detectSourceRefCycle(ctx context.Context, c client.Client, snip *jaasv1.JsonnetSnippet) (bool, string, error) {
	if snip == nil {
		return false, "", nil
	}
	if !hasCycleSourceEdge(snip) {
		return false, "", nil
	}
	start := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	// BFS over the snippet graph. visited tracks every snippet we've
	// expanded; parent lets us reconstruct the cycle path on detection.
	visited := map[types.NamespacedName]bool{start: true}
	parent := map[types.NamespacedName]types.NamespacedName{}
	queue := []*jaasv1.JsonnetSnippet{snip}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		curID := types.NamespacedName{Name: cur.Name, Namespace: cur.Namespace}

		deps, err := snippetDependencies(ctx, c, cur)
		if err != nil {
			return false, "", err
		}
		for _, dep := range deps {
			if dep == start {
				path := reconstructCyclePath(parent, curID, start)
				return true, formatCyclePath(path), nil
			}
			if visited[dep] {
				continue
			}
			visited[dep] = true
			parent[dep] = curID

			var next jaasv1.JsonnetSnippet
			if err := c.Get(ctx, dep, &next); err != nil {
				if apierrors.IsNotFound(err) {
					// dep is named like a snippet but no such snippet
					// exists — leaf, no further chain.
					continue
				}
				return false, "", fmt.Errorf("cycle detection: get %s: %w", dep, err)
			}
			queue = append(queue, &next)
		}
	}
	return false, "", nil
}

// snippetDependencies returns the set of OTHER snippets that snip depends
// on via ExternalArtifact references (direct sourceRef + library sourceRefs).
// Non-ExternalArtifact sourceRefs are terminal and excluded. Missing
// libraries are treated as no-op edges (the reconciler surfaces them as
// LibraryNotFound on its own path).
func snippetDependencies(ctx context.Context, c client.Client, snip *jaasv1.JsonnetSnippet) ([]types.NamespacedName, error) {
	var ids []types.NamespacedName

	if id, ok := externalArtifactDep(snip.Spec.SourceRef, snip.Namespace); ok {
		ids = append(ids, id)
	}

	for _, ref := range snip.Spec.Libraries {
		src, err := librarySourceRef(ctx, c, ref, snip.Namespace)
		if err != nil {
			return nil, err
		}
		if id, ok := externalArtifactDep(src, snip.Namespace); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// externalArtifactDep returns (publishingSnippetID, true) when ref points at
// a JaaS-published ExternalArtifact, or zero/false otherwise. Empty
// ref.Namespace defaults to ownerNs (the snippet's own namespace) to match
// resolveSnippetSource.
func externalArtifactDep(ref *jaasv1.SourceRef, ownerNs string) (types.NamespacedName, bool) {
	if ref == nil || ref.Kind != "ExternalArtifact" {
		return types.NamespacedName{}, false
	}
	ns := ref.Namespace
	if ns == "" {
		ns = ownerNs
	}
	return types.NamespacedName{Name: ref.Name, Namespace: ns}, true
}

// librarySourceRef fetches the referenced library (JsonnetLibrary) and
// returns its spec.sourceRef. nil means "no dependency edge" (library
// has inline files, doesn't exist, or unknown kind). The library
// namespace defaults to ownerNs — same rule the reconciler applies
// during eval.
func librarySourceRef(ctx context.Context, c client.Client, ref jaasv1.LibraryRef, ownerNs string) (*jaasv1.SourceRef, error) {
	switch ref.Kind {
	case "JsonnetLibrary":
		ns := ref.Namespace
		if ns == "" {
			ns = ownerNs
		}
		var lib jaasv1.JsonnetLibrary
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &lib); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("cycle detection: get JsonnetLibrary %s/%s: %w", ns, ref.Name, err)
		}
		return lib.Spec.SourceRef, nil
	}
	return nil, nil
}

// reconstructCyclePath walks parent pointers from cur back to start,
// producing the chain start → ... → cur, then appends start to close the
// loop visually. parent is populated as BFS expands; entries point each
// dep at the snippet that introduced it.
func reconstructCyclePath(parent map[types.NamespacedName]types.NamespacedName, cur, start types.NamespacedName) []types.NamespacedName {
	if cur == start {
		// Self-cycle: the start snippet's own dependency loops back.
		return []types.NamespacedName{start, start}
	}
	var stack []types.NamespacedName
	for p := cur; p != start; {
		stack = append(stack, p)
		next, ok := parent[p]
		if !ok {
			break
		}
		p = next
	}
	out := make([]types.NamespacedName, 0, len(stack)+2)
	out = append(out, start)
	for i := len(stack) - 1; i >= 0; i-- {
		out = append(out, stack[i])
	}
	out = append(out, start)
	return out
}

// formatCyclePath renders the visited chain for the status message and
// admission error. The output reads "ns/a → ns/b → ns/a" so operators see
// exactly where the chain loops.
func formatCyclePath(path []types.NamespacedName) string {
	if len(path) == 0 {
		return ""
	}
	out := path[0].String()
	for _, p := range path[1:] {
		out += " → " + p.String()
	}
	return out
}

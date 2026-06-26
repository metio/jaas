/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fluxpatch "github.com/fluxcd/pkg/runtime/patch"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// Flux source-controller's ExternalArtifact lives at this GVK. The reconciler
// upserts one of these for every successfully-evaluated JsonnetSnippet so
// downstream Flux consumers (kustomize-controller, helm-controller) can
// reference the rendered output.
const (
	fluxSourceGroup      = "source.toolkit.fluxcd.io"
	fluxSourceVersion    = "v1"
	externalArtifactKind = "ExternalArtifact"
)

var externalArtifactGVK = schema.GroupVersionKind{
	Group:   fluxSourceGroup,
	Version: fluxSourceVersion,
	Kind:    externalArtifactKind,
}

// ExternalArtifactGVK returns the GVK our reconciler upserts. Exported for
// tests and main.go-level scheme registration.
func ExternalArtifactGVK() schema.GroupVersionKind { return externalArtifactGVK }

// Publisher writes the snippet's tarball to the configured storage
// backend and upserts the matching ExternalArtifact CR. The API client is
// supplied per call so the reconciler can hand in a fresh per-snippet
// impersonating client without rebuilding the Publisher. Construct via
// NewPublisher.
//
// The Store field is satisfied by any storage.Backend — currently the
// filesystem-backed *storage.Store and the object-store *storage.S3Backend.
// Tests substitute fakes here to inject Put / Prune / Delete failures.
type Publisher struct {
	Store   storage.Backend
	BaseURL string
	Clock   func() time.Time

	// MaxArtifactBytes caps the published content size in bytes. The cap
	// measures the sum of the tarball members' content — len(rendered) in
	// Output=rendered mode, the whole sourceFiles tree in Output=source
	// mode — so it bounds what actually lands on disk/S3 in either mode. A
	// snippet over the cap fails with ErrArtifactTooLarge before any
	// write, so one runaway snippet can't fill the storage volume. Zero
	// disables.
	MaxArtifactBytes int64

	// GCGrace is the minimum time a revision evicted from the keep-set
	// remains fetchable before Prune removes it. Closes the pin→fetch
	// race in which a Flux consumer reads status.artifact a moment
	// before the operator garbage-collects the superseded revision and
	// then 404s on the dereference. Threaded through to
	// storage.Backend.Prune verbatim; zero restores eager pruning.
	GCGrace time.Duration
}

// ErrArtifactTooLarge is returned from Publish when the published
// content (summed across the tarball members) exceeds
// Publisher.MaxArtifactBytes. The reconciler surfaces this as
// ReasonArtifactTooLarge on the Ready condition.
var ErrArtifactTooLarge = errors.New("publisher: artifact exceeds MaxArtifactBytes")

// NewPublisher returns a Publisher whose Clock falls back to time.Now.
func NewPublisher(store storage.Backend, baseURL string) *Publisher {
	return &Publisher{Store: store, BaseURL: baseURL, Clock: time.Now}
}

// PublishResult is what a successful Publish returns. Carries the
// revision (for status.revision) and the public URL the published
// tarball is served at (mirrored onto status.artifactURL so the
// snippet is self-describing — see SyncStatus.ArtifactURL).
type PublishResult struct {
	Revision string
	URL      string
}

// Publish writes the artifact tarball, computes the URL the operator's
// storage HTTP server will serve it from, and upserts the ExternalArtifact CR
// with that URL on its status. The returned revision is the "sha256:<hex>"
// form ready to copy into JsonnetSnippet.Status.Revision. The supplied client
// is used for every API call — pass an impersonating client to bound the
// reconcile to the tenant's permissions.
//
// sourceFiles is the resolved snippet source — for inline snippets this is
// snip.Spec.Files verbatim; for sourceRef snippets it's the file map the
// Fetcher extracted from the upstream tarball. Used only in Output=source
// mode; ignored otherwise.
//
// keepRevisions lists the sha256 shortRevs (no "sha256:" prefix) to retain in
// storage after this publish; always include the just-published revision. An
// empty slice keeps only the just-published revision.
func (p *Publisher) Publish(ctx context.Context, c client.Client, snip *jaasv1.JsonnetSnippet, rendered string, sourceFiles map[string]string, keepRevisions []string) (PublishResult, error) {
	if p.Store == nil || c == nil {
		return PublishResult{}, errors.New("publisher: store and client are required")
	}
	entries, revision, err := p.buildEntries(snip, rendered, sourceFiles)
	if err != nil {
		return PublishResult{}, err
	}
	// Cap the actual published content for every Output mode. In rendered
	// mode the single entry's content is len(rendered); in source mode it
	// is the whole sourceFiles tree — len(rendered) would not measure that.
	// Summing the built entries' bytes is the right measure because those
	// are exactly the bytes Store.Put writes into the tarball.
	if p.MaxArtifactBytes > 0 {
		var total int64
		for _, e := range entries {
			total += int64(len(e.Content))
		}
		if total > p.MaxArtifactBytes {
			return PublishResult{}, fmt.Errorf("%w: %d > %d", ErrArtifactTooLarge, total, p.MaxArtifactBytes)
		}
	}
	shortRev := strings.TrimPrefix(revision, "sha256:")

	res, err := p.Store.Put(ctx, snip.Namespace, snip.Name, shortRev, entries)
	if err != nil {
		return PublishResult{}, fmt.Errorf("publisher: store put: %w", err)
	}

	// Upsert BEFORE Prune so a transient apiserver failure can't leave
	// the ExternalArtifact CR pointing at a revision Prune just wiped
	// — downstream Flux consumers would see 404s on every fetch until
	// the next reconcile rewrote the CR. With this ordering Prune only
	// runs after we've confirmed the apiserver carries the new
	// revision.
	if err := p.upsertExternalArtifact(ctx, c, snip, revision, res); err != nil {
		return PublishResult{}, err
	}

	if len(keepRevisions) == 0 {
		keepRevisions = []string{shortRev}
	} else if !slices.Contains(keepRevisions, shortRev) {
		// Defensive: caller may forget to include the just-published
		// rev. Without it, Prune would wipe what we just wrote.
		keepRevisions = append([]string{shortRev}, keepRevisions...)
	}
	if err := p.Store.Prune(ctx, snip.Namespace, snip.Name, keepRevisions, p.GCGrace); err != nil {
		return PublishResult{}, fmt.Errorf("publisher: store prune: %w", err)
	}
	return PublishResult{Revision: revision, URL: p.url(res.Path)}, nil
}

// PruneStored runs a keep-set Prune on the snippet's stored revisions
// without a fresh Put or ExternalArtifact upsert. Used by the suspended
// reconcile path: a paused snippet still re-enters the reconciler on
// every watch tick + interval, so calling PruneStored there keeps
// grace-expired evicted revisions from leaking when the snippet stays
// suspended for the operator's lifetime. keepRevisions follows the same
// shape Publish wants — short SHA-256 hex strings, no "sha256:" prefix.
func (p *Publisher) PruneStored(ctx context.Context, namespace, name string, keepRevisions []string) error {
	if p.Store == nil {
		return errors.New("publisher: store is required")
	}
	if err := p.Store.Prune(ctx, namespace, name, keepRevisions, p.GCGrace); err != nil {
		return fmt.Errorf("publisher: store prune: %w", err)
	}
	return nil
}

// Withdraw removes the ExternalArtifact CR and the snippet's stored tarballs.
// Called from the deletion path before the finalizer is dropped; the client
// is supplied per call for the same impersonation reason as Publish.
func (p *Publisher) Withdraw(ctx context.Context, c client.Client, snip *jaasv1.JsonnetSnippet) error {
	if p.Store == nil || c == nil {
		return errors.New("publisher: store and client are required")
	}
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	ea.SetName(snip.Name)
	ea.SetNamespace(snip.Namespace)
	if err := c.Delete(ctx, ea); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("publisher: delete ExternalArtifact: %w", err)
	}
	return p.Store.Delete(ctx, snip.Namespace, snip.Name)
}

// buildEntries derives the tarball members from the snippet's Output mode.
// In "rendered" mode the archive holds a single rendered.json; in "source"
// mode it carries every entry from sourceFiles verbatim, useful for
// downstream consumers that want to re-evaluate themselves. sourceFiles
// is the resolved source — same map for inline spec.files and for
// sourceRef-fetched content — so source-mode publishing works uniformly
// across both source shapes.
func (p *Publisher) buildEntries(snip *jaasv1.JsonnetSnippet, rendered string, sourceFiles map[string]string) ([]storage.FileEntry, string, error) {
	switch snip.Spec.Output {
	case jaasv1.OutputSource:
		entries := make([]storage.FileEntry, 0, len(sourceFiles))
		for path, body := range sourceFiles {
			entries = append(entries, storage.FileEntry{Path: path, Content: []byte(body)})
		}
		return entries, sourceArchiveRevision(sourceFiles), nil
	case jaasv1.OutputRendered, "":
		entries := []storage.FileEntry{{Path: "rendered.json", Content: []byte(rendered)}}
		sum := sha256.Sum256([]byte(rendered))
		return entries, "sha256:" + hex.EncodeToString(sum[:]), nil
	default:
		return nil, "", fmt.Errorf("publisher: unknown spec.output %q", snip.Spec.Output)
	}
}

// sourceArchiveRevision hashes the source files deterministically so the
// same input always produces the same revision.
func sourceArchiveRevision(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(files[k]))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (p *Publisher) upsertExternalArtifact(ctx context.Context, c client.Client, snip *jaasv1.JsonnetSnippet, revision string, res storage.Result) error {
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	ea.SetName(snip.Name)
	ea.SetNamespace(snip.Namespace)

	// CreateOrUpdate handles the Get-then-Update race on its own: a
	// concurrent writer (another leader, manual kubectl edit) that
	// bumps resourceVersion between our Get and Update surfaces as a
	// Conflict, which controller-runtime translates into a retry of
	// the mutate function. The manual pattern this replaces would
	// have returned the Conflict to the reconciler and required a
	// full requeue.
	if _, err := controllerutil.CreateOrUpdate(ctx, c, ea, func() error {
		return setSpec(ea, snip)
	}); err != nil {
		return fmt.Errorf("publisher: upsert ExternalArtifact: %w", err)
	}

	if err := p.writeStatus(ctx, c, ea, revision, res); err != nil {
		return err
	}
	return nil
}

// setSpec writes the published ExternalArtifact's spec.sourceRef as a
// three-field back-pointer to the originating JsonnetSnippet.
//
// **Public contract.** The exact shape — `apiVersion`, `kind`, `name`,
// in that nesting under `spec.sourceRef` — is part of JaaS's wire
// contract with downstream Flux consumers that do producer-aware
// resolution. The stageset-controller's RFC-0012 reverse lookup matches
// on this triple to resolve a `JsonnetSnippet` reference back to its
// published `ExternalArtifact`; other consumers may do the same.
// Renaming a field, splitting the apiVersion into group/version,
// changing the kind string, or shifting these out of `spec.sourceRef`
// is a breaking change that requires an upgrade-notes entry.
//
// The back-pointer deliberately omits `namespace`: every
// ExternalArtifact JaaS publishes lives in the snippet's own namespace
// (see Publisher.upsertExternalArtifact's `ea.SetNamespace(snip.Namespace)`).
// Same-namespace publishing is part of the contract too — consumers
// scope their reverse lookup to the ExternalArtifact's namespace and
// trust that the back-pointer resolves there.
//
// A second invariant on the same contract: the URL Publisher records
// on `status.artifact.url` is revision-addressed and byte-stable while
// the revision is in the keep-set. stageset-controller's
// `rollbackOnFailure` re-fetches previously recorded URLs and
// digest-verifies them; a refactor that rewrote the URL form, or
// rewrote the file on disk for a retained revision, would silently
// break rollback. Keep the URL = `BaseURL/namespace/name/<rev>.tar.gz`
// shape and the byte-for-byte tarball stable across re-publishes of
// other revisions.
func setSpec(ea *unstructured.Unstructured, snip *jaasv1.JsonnetSnippet) error {
	return unstructured.SetNestedMap(ea.Object, map[string]any{
		"sourceRef": map[string]any{
			"apiVersion": jaasv1.GroupVersion.String(),
			"kind":       "JsonnetSnippet",
			"name":       snip.Name,
		},
	}, "spec")
}

func (p *Publisher) writeStatus(ctx context.Context, c client.Client, ea *unstructured.Unstructured, revision string, res storage.Result) error {
	now := p.now().UTC().Format(time.RFC3339)
	artifact := map[string]any{
		"url":            p.url(res.Path),
		"path":           res.Path,
		"revision":       revision,
		"digest":         "sha256:" + res.DigestSHA256,
		"size":           res.SizeBytes,
		"lastUpdateTime": now,
	}
	// The status write goes through the Flux patch.Helper. ea carries the
	// server-latest state from the preceding CreateOrUpdate, which the helper
	// snapshots as its "before"; the status fields we set below land via a
	// status merge-patch with no resourceVersion precondition. Source-controller
	// and other downstream observers commonly bump resourceVersion as they
	// observe the new artifact, and a precondition-free merge patch can't
	// conflict on that — the write no longer propagates a Conflict out of the
	// reconcile and forces the whole render+upload cycle to re-run. The merge
	// patch touches only status.artifact and status.conditions, so the spec we
	// set in the CreateOrUpdate phase is preserved.
	helper, err := fluxpatch.NewHelper(ea, c)
	if err != nil {
		return fmt.Errorf("publisher: status patch helper: %w", err)
	}
	_ = unstructured.SetNestedMap(ea.Object, artifact, "status", "artifact")
	setReadyCondition(ea, now)
	if err := helper.Patch(ctx, ea); err != nil {
		return fmt.Errorf("publisher: status update: %w", err)
	}
	return nil
}

// setReadyCondition stamps a single Ready=True condition onto the
// ExternalArtifact's status.conditions. This is the signal every Flux
// consumer — kustomize-controller, helm-controller, JaaS's own
// internal/sources.readyState, and any RFC-0012 producer-aware
// resolver — gates on: an artifact whose producer has not marked it
// Ready is treated as not-yet-consumable. Writing only status.artifact
// leaves a chained snippet's sourceRef stuck on ErrSourceNotReady
// forever, so the condition rides the same retried status update as the
// artifact fields.
//
// lastTransitionTime is preserved while the status stays True so a
// steady republish (new revision, same Ready=True) doesn't churn the
// timestamp and the resourceVersion bumps it would trigger downstream.
// now is the RFC3339 string writeStatus already computed.
func setReadyCondition(latest *unstructured.Unstructured, now string) {
	lastTransition := now
	existing, _, _ := unstructured.NestedSlice(latest.Object, "status", "conditions")
	// Rebuild the conditions, keeping every non-Ready condition and replacing
	// only our Ready one. A wholesale overwrite would drop any condition a
	// co-managing Flux controller stamps on the ExternalArtifact (Reconciling,
	// Stalled, …); the artifact's conditions are a set keyed by type, and this
	// owns only Ready.
	conditions := make([]any, 0, len(existing)+1)
	for _, c := range existing {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			conditions = append(conditions, c)
			continue
		}
		if s, _ := m["status"].(string); s == "True" {
			if lt, ok := m["lastTransitionTime"].(string); ok && lt != "" {
				lastTransition = lt
			}
		}
	}
	ready := map[string]any{
		"type":               "Ready",
		"status":             "True",
		"reason":             "Succeeded",
		"message":            "artifact published",
		"lastTransitionTime": lastTransition,
		"observedGeneration": latest.GetGeneration(),
	}
	conditions = append(conditions, ready)
	_ = unstructured.SetNestedSlice(latest.Object, conditions, "status", "conditions")
}

func (p *Publisher) now() time.Time {
	// NewPublisher always sets Clock to time.Now; this nil-guard is
	// here for hand-constructed Publishers (some tests) so the
	// zero-value usage doesn't panic.
	if p.Clock == nil {
		return time.Now()
	}
	return p.Clock()
}

func (p *Publisher) url(path string) string {
	return strings.TrimSuffix(p.BaseURL, "/") + "/" + path
}

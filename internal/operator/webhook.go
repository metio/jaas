/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
)

// SnippetValidator is the admission webhook for JsonnetSnippet. It rejects:
//
//   - CRs whose spec.externalVariables collide with the operator's
//     --ext-var set
//   - CRs whose spec.sourceRef chain forms a dependency cycle through other
//     JaaS-published ExternalArtifacts
//
// The reconciler enforces the same invariants as fallbacks, so a bypassed
// webhook still produces Ready=False with the matching reason — but
// admission gives the user immediate feedback on `kubectl apply`.
type SnippetValidator struct {
	// OperatorExtVars is the operator-level external-variable set.
	OperatorExtVars map[string]string

	// KnownLibraryAliases enumerates OCI-mounted library aliases the
	// operator was started with. A LibraryRef.ImportPath that shadows
	// one is rejected at admission so the user notices the OCI mount is
	// being silently overridden — empty disables the check.
	KnownLibraryAliases []string

	// Client reads the existing snippet graph for cycle detection. nil
	// disables cycle checks at admission — the reconciler still enforces
	// the invariant. defaultBuilder always wires the manager's client.
	Client client.Client

	// APIReader is the UNCACHED reader the cycle walk uses when set. The
	// manager's cache-backed Client is scoped by --watch-namespaces and
	// filtered by --label-selector, so a dependency Get for an unwatched
	// namespace / unlabeled object returns "unknown namespace for the cache"
	// (not NotFound) — which this cluster-wide webhook would turn into a hard
	// denial of a snippet in a namespace the operator does not even manage.
	// Reading the graph uncached matches the reconciler's cycleReader. nil
	// falls back to Client (tests).
	APIReader client.Reader
}

// cycleReader returns the reader the admission cycle walk uses — the uncached
// APIReader when wired, else Client.
func (v *SnippetValidator) cycleReader() client.Reader {
	if v.APIReader != nil {
		return v.APIReader
	}
	return v.Client
}

// ValidateCreate is called on every create request before persistence.
func (v *SnippetValidator) ValidateCreate(ctx context.Context, snip *jaasv1.JsonnetSnippet) (admission.Warnings, error) {
	return v.validate(ctx, snip)
}

// ValidateUpdate is called on every update request before persistence.
func (v *SnippetValidator) ValidateUpdate(ctx context.Context, old *jaasv1.JsonnetSnippet, snip *jaasv1.JsonnetSnippet) (admission.Warnings, error) {
	// An object being deleted has no spec-validity invariant left to enforce,
	// and admission must never block its own controller's cleanup: the
	// finalizer-removal Update carries the unchanged spec, so re-running cycle
	// detection on a snippet that is part of a dependency cycle would reject the
	// removal and wedge the snippet in Terminating forever. Editing the spec to
	// break the cycle still validates the new spec via the create/normal-update
	// path, so this only skips the spec-unchanged teardown writes.
	if !snip.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}
	// When the validation-relevant inputs are unchanged, an update touching only
	// other fields (spec.suspend, annotations, spec.interval, …) cannot
	// introduce a violation this webhook enforces. Skip re-validation so such an
	// update is not blocked by a violation an EXTERNAL change created without a
	// snippet-spec change — a referenced JsonnetLibrary edited to form a cycle
	// (libraries have no webhook), or an operator restart with a new --ext-var
	// that now collides. Otherwise suspend — the documented remediation for a
	// wedged snippet — and the operator's own MCP suspend/reconcile tools would
	// be denied on the very snippet they exist to unstick.
	if old != nil && validationInputsUnchanged(old, snip) {
		return nil, nil
	}
	return v.validate(ctx, snip)
}

// validationInputsUnchanged reports whether every spec field the webhook
// validates is identical between old and new. It deliberately excludes fields
// the webhook does not check (suspend, interval, history, entryFile), so a
// change to one of those is not treated as needing re-validation.
func validationInputsUnchanged(old, snip *jaasv1.JsonnetSnippet) bool {
	return reflect.DeepEqual(old.Spec.ExternalVariables, snip.Spec.ExternalVariables) &&
		reflect.DeepEqual(old.Spec.Libraries, snip.Spec.Libraries) &&
		reflect.DeepEqual(old.Spec.SourceRef, snip.Spec.SourceRef) &&
		reflect.DeepEqual(old.Spec.Files, snip.Spec.Files) &&
		old.Spec.Output == snip.Spec.Output
}

// ValidateDelete is called on every delete request. We have no delete-time
// invariants to enforce, so this always passes.
func (v *SnippetValidator) ValidateDelete(_ context.Context, _ *jaasv1.JsonnetSnippet) (admission.Warnings, error) {
	return nil, nil
}

func (v *SnippetValidator) validate(ctx context.Context, snip *jaasv1.JsonnetSnippet) (admission.Warnings, error) {
	conflicts := v.conflicts(snip.Spec.ExternalVariables)
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("spec.externalVariables conflicts with operator --ext-var: %v", conflicts)
	}
	if alias := v.libraryAliasCollision(snip); alias != "" {
		return nil, fmt.Errorf("spec.libraries import alias %q shadows OCI-mounted library; rename or drop the LibraryRef", alias)
	}
	if bad := invalidLibraryImportAlias(snip); bad != "" {
		return nil, fmt.Errorf("spec.libraries import alias %q must be a single path segment (allowed: one [A-Za-z0-9._-] segment, no slash or traversal)", bad)
	}
	if dup := duplicateLibraryImportPath(snip); dup != "" {
		return nil, fmt.Errorf("spec.libraries import path %q is used by more than one entry; each library must resolve to a distinct import path", dup)
	}
	// Output=source ships the file NAMES to consumers, whose extractors
	// silently drop names outside the safe charset — the file would just
	// never arrive downstream. Reject at admission so the author hears it
	// on apply; the Publisher enforces the same rule as the fallback.
	if snip.Spec.Output == jaasv1.OutputSource {
		for name := range snip.Spec.Files {
			if !sources.SafeEntryName(name) {
				return nil, fmt.Errorf("spec.files key %q would be silently dropped by artifact consumers in output=source mode (allowed: [A-Za-z0-9._/-] segments, no dot-prefixed segments, no traversal)", name)
			}
		}
	}
	if reader := v.cycleReader(); reader != nil {
		cycle, path, err := detectSourceRefCycle(ctx, reader, snip)
		if err != nil {
			return nil, fmt.Errorf("cycle detection failed: %w", err)
		}
		if cycle {
			return nil, fmt.Errorf("spec.sourceRef chain forms a cycle: %s", path)
		}
	}
	return softWarnings(snip), nil
}

// softWarnings returns admission warnings for soft pitfalls — common
// misconfigurations that won't break admission but predictably surface
// as Ready=False at reconcile time. kubectl apply prints these inline
// (one "Warning: <msg>" line per entry) so the operator sees the
// problem at the moment they apply the CR, not on the next describe.
//
// Each helper here is self-contained so a future addition can be
// dropped in as one more function call.
func softWarnings(snip *jaasv1.JsonnetSnippet) admission.Warnings {
	var w admission.Warnings
	if msg := warnEntryFileMissing(snip); msg != "" {
		w = append(w, msg)
	}
	if msg := warnLikelySelfReference(snip); msg != "" {
		w = append(w, msg)
	}
	return w
}

// warnEntryFileMissing catches the inline-files case where spec.files
// doesn't carry the key spec.entryFile names (default "main.jsonnet").
// Caught at reconcile time as ReasonInvalidSpec, but kubectl apply
// shouldn't pass silently on something this preventable. SourceRef
// mode is skipped — we can't introspect the upstream tarball at
// admission time, so the validator there can't say what files exist.
func warnEntryFileMissing(snip *jaasv1.JsonnetSnippet) string {
	if snip.Spec.SourceRef != nil {
		return ""
	}
	if len(snip.Spec.Files) == 0 {
		return ""
	}
	entry := snip.Spec.EntryFile
	if entry == "" {
		entry = EntryFileName
	}
	if _, ok := snip.Spec.Files[entry]; !ok {
		return fmt.Sprintf("spec.entryFile=%q is not a key in spec.files (%d files supplied); reconcile will fail with ReasonInvalidSpec",
			entry, len(snip.Spec.Files))
	}
	return ""
}

// duplicateLibraryImportPath returns the first effective import path
// (ImportPath, or Name when ImportPath is empty) shared by two or more
// spec.libraries entries — empty when every entry resolves to a distinct
// path. A collision is unrepresentable in the import-alias namespace: the
// reconciler's resolveLibraries keys its map by import path, so two
// entries on one path would mean one silently overwriting the other. Both
// admission and the reconciler reject it outright, mirroring the
// OCI-alias collision (libraryAliasCollision), so there is never an
// ambiguous "which library did this alias resolve to" at eval time.
// effectiveLibraryAlias returns the import-alias namespace name a LibraryRef
// occupies: ImportPath, or Name when ImportPath is empty.
func effectiveLibraryAlias(ref jaasv1.LibraryRef) string {
	if ref.ImportPath != "" {
		return ref.ImportPath
	}
	return ref.Name
}

// invalidLibraryImportAlias returns the first effective library alias that is
// not a single safe path segment, or "" when every alias is safe. The alias
// becomes the head of the importer's foundAt string (alias + "/" + fileWithin);
// a slash-bearing alias makes that encoding non-injective across libraries —
// "vendor" with file "lib/x" and "vendor/lib" with file "x" both resolve to
// foundAt "vendor/lib/x". go-jsonnet keys its content on foundAt, so the two
// distinct library files would share one Contents, silently rendering one
// library's bytes in place of the other's. Restricting the alias to a single
// [A-Za-z0-9._-] segment (matching the OCI single-directory alias convention)
// keeps foundAt injective across libraries.
func invalidLibraryImportAlias(snip *jaasv1.JsonnetSnippet) string {
	for _, ref := range snip.Spec.Libraries {
		if a := effectiveLibraryAlias(ref); !safeLibraryAlias(a) {
			return a
		}
	}
	return ""
}

// safeLibraryAlias reports whether a is a single path segment with no slash,
// NUL, or "."/".." traversal — the charset artifact consumers and os.OpenRoot
// both accept for one directory component.
func safeLibraryAlias(a string) bool {
	if a == "" || a == "." || a == ".." {
		return false
	}
	for _, r := range a {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func duplicateLibraryImportPath(snip *jaasv1.JsonnetSnippet) string {
	if len(snip.Spec.Libraries) < 2 {
		return ""
	}
	seen := make(map[string]struct{}, len(snip.Spec.Libraries))
	for _, ref := range snip.Spec.Libraries {
		key := ref.ImportPath
		if key == "" {
			key = ref.Name
		}
		if _, ok := seen[key]; ok {
			return key
		}
		seen[key] = struct{}{}
	}
	return ""
}

// warnLikelySelfReference catches the case where spec.sourceRef is an
// ExternalArtifact whose name matches this snippet's own name in the
// same namespace. The reconciler's cycle detector catches the actual
// loop, but the warning surfaces at apply time so a user typo
// (`name: <copy of metadata.name>`) doesn't burn a reconcile to learn
// about it.
func warnLikelySelfReference(snip *jaasv1.JsonnetSnippet) string {
	if snip.Spec.SourceRef == nil || snip.Spec.SourceRef.Kind != "ExternalArtifact" {
		return ""
	}
	if snip.Spec.SourceRef.Name != snip.Name {
		return ""
	}
	ns := snip.Spec.SourceRef.Namespace
	if ns == "" || ns == snip.Namespace {
		return fmt.Sprintf("spec.sourceRef points at ExternalArtifact %q in the snippet's own namespace — this is the snippet's own published artifact and forms a cycle; the reconciler will reject with ReasonDependencyCycle",
			snip.Spec.SourceRef.Name)
	}
	return ""
}

// libraryAliasCollision returns the first ImportPath (or .Name when
// empty) that shadows one of the operator's OCI-mounted aliases. Empty
// string means no collision (or check disabled because the operator
// has no OCI mounts).
func (v *SnippetValidator) libraryAliasCollision(snip *jaasv1.JsonnetSnippet) string {
	if len(v.KnownLibraryAliases) == 0 {
		return ""
	}
	known := make(map[string]struct{}, len(v.KnownLibraryAliases))
	for _, a := range v.KnownLibraryAliases {
		known[a] = struct{}{}
	}
	for _, ref := range snip.Spec.Libraries {
		alias := ref.ImportPath
		if alias == "" {
			alias = ref.Name
		}
		if _, hit := known[alias]; hit {
			return alias
		}
	}
	return ""
}

func (v *SnippetValidator) conflicts(crLevel []jaasv1.JsonnetVariable) []string {
	if len(v.OperatorExtVars) == 0 || len(crLevel) == 0 {
		return nil
	}
	var hits []string
	for _, entry := range crLevel {
		if _, exists := v.OperatorExtVars[entry.Name]; exists {
			hits = append(hits, entry.Name)
		}
	}
	sort.Strings(hits)
	return hits
}

// SetupWithManager registers v as a validating webhook on mgr. The path is
// fixed; the helm chart's ValidatingWebhookConfiguration must point at it.
func (v *SnippetValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &jaasv1.JsonnetSnippet{}).
		WithValidator(v).
		Complete()
}

// validatorBound is a tiny compile-time check that *SnippetValidator
// satisfies controller-runtime's typed-validator contract.
var _ admission.Validator[*jaasv1.JsonnetSnippet] = &SnippetValidator{}

/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Wire-stable output mode constants for JsonnetSnippet.spec.output.
// Programmatic callers match on these strings; do not rename.
const (
	OutputRendered = "rendered"
	OutputSource   = "source"
)

// Wire-stable condition type names. Status conditions on every JaaS CR use
// these strings.
const (
	ConditionReady = "Ready"
)

// JsonnetVariable binds one name to one value for a snippet's evaluation. It
// is the element type of both spec.tlas and spec.externalVariables, whose
// only difference is which go-jsonnet binding the name lands in.
//
// The list-of-named-entries shape (rather than a map keyed by name) mirrors
// the container `env:` convention, so the ordering, patch semantics, and
// server-side-apply behavior are the ones operators already know from
// PodSpec. Name uniqueness is enforced by the apiserver via the owning
// field's listMapKey — it is not a reconcile-time check.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.code) && self.code) || (has(self.value) && size(self.value) > 0)",message="value must be non-empty when code is true"
type JsonnetVariable struct {
	// Name is the identifier the snippet binds to — the argument name for a
	// TLA, or the std.extVar("<name>") key for an external variable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Value is the bound value. With Code false (the default) it is passed
	// verbatim as a Jsonnet string. With Code true it is parsed as Jsonnet
	// source and the result is bound, so `"3"` becomes the number 3 and
	// `["a","b"]` an array. Omitting it binds the empty string; a code
	// variable requires a non-empty value.
	// +optional
	Value string `json:"value,omitempty"`

	// Code selects how Value is interpreted: false (the default) binds it as
	// a string — vm.ExtVar / vm.TLAVar, matching `jsonnet --ext-str` and
	// `--tla-str`; true parses it as Jsonnet source — vm.ExtCode /
	// vm.TLACode, matching `--ext-code` and `--tla-code`. A syntactically
	// invalid code value fails the evaluation, not admission.
	// +optional
	Code bool `json:"code,omitempty"`
}

// SnippetSource is the bytes a JsonnetSnippet or JsonnetLibrary materializes.
// Exactly one of Files or SourceRef must be set; admission enforces this via
// CEL on each owning Spec.
type SnippetSource struct {
	// Files is an inline map of filename to jsonnet source. Each filename is
	// resolved verbatim; the snippet's top-level file is conventionally
	// "main.jsonnet".
	// +optional
	Files map[string]string `json:"files,omitempty"`

	// SourceRef points at a Flux source CR (GitRepository, OCIRepository,
	// Bucket, ExternalArtifact) whose status.artifact.url exposes a tarball
	// the controller fetches and extracts.
	// +optional
	SourceRef *SourceRef `json:"sourceRef,omitempty"`
}

// SourceRef is a typed reference to a FluxCD source CR.
type SourceRef struct {
	// APIVersion of the referenced source. Defaults to
	// source.toolkit.fluxcd.io/v1 when empty.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referenced source. Required. One of GitRepository,
	// OCIRepository, Bucket, ExternalArtifact.
	// +kubebuilder:validation:Enum=GitRepository;OCIRepository;Bucket;ExternalArtifact
	Kind string `json:"kind"`

	// Name of the referenced source. Required.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the referenced source. Optional for namespaced owners
	// (defaults to the owner's namespace). Cross-namespace references are
	// rejected by default; they are allowed only when the operator runs with
	// --no-cross-namespace-refs=false.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Path narrows extraction to a subdirectory of the artifact's tarball.
	// Empty means the artifact root.
	// +optional
	Path string `json:"path,omitempty"`
}

// LibraryRef enumerates a single library available to a JsonnetSnippet at
// evaluation time. The K8s-native APIVersion+Kind+Name+Namespace shape lets
// the operator add new library kinds without reshaping the field.
type LibraryRef struct {
	// APIVersion of the library CR. Defaults to jaas.metio.wtf/v1 when empty.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the library CR. Required. The namespaced JsonnetLibrary is the
	// only library kind; OCI-mounted shared libraries (helm
	// `additionalLibraries`) need no ref and are imported by alias directly.
	// +kubebuilder:validation:Enum=JsonnetLibrary
	Kind string `json:"kind"`

	// Name of the referenced library CR. Required.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the referenced library CR. Cross-namespace references
	// are rejected by default; they are allowed only when the operator runs
	// with --no-cross-namespace-refs=false.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ImportPath is the alias the snippet's Jsonnet source uses in `import`
	// statements. Defaults to the referenced library's metadata.name.
	// +optional
	ImportPath string `json:"importPath,omitempty"`
}

// SyncStatus is the common Conditions+ObservedGeneration shape every JaaS CR
// uses for status reporting.
type SyncStatus struct {
	// ObservedGeneration is the .metadata.generation of the spec the
	// controller last reconciled. Lets clients tell stale status apart from
	// up-to-date.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard apimachinery shape. The Ready condition
	// summarizes whether the most recent reconcile succeeded; per-stage
	// failure detail is carried in Reason+Message.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Revision identifies the last successfully-reconciled source revision
	// (the artifact checksum for SourceRef mode, or the inline files'
	// content hash). Empty until the first successful reconcile.
	// +optional
	Revision string `json:"revision,omitempty"`

	// ArtifactURL is the HTTP URL of the last-successfully-published
	// artifact tarball. Downstream Flux consumers fetch this; surfaced
	// on the snippet's own status so `kubectl describe jsonnetsnippet`
	// answers "where can I download my rendered JSON?" without a
	// second `kubectl get externalartifact`. Empty until the first
	// successful publish; preserved across subsequent failures so the
	// last-known-good URL stays observable even when the latest
	// reconcile fails.
	// +optional
	ArtifactURL string `json:"artifactURL,omitempty"`

	// LastSyncTime stamps the most recent successful reconcile.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// LastHandledReconcileAt is the value of the
	// reconcile.fluxcd.io/requestedAt annotation the controller most
	// recently acted on. `flux reconcile` (and `kubectl annotate
	// <cr> reconcile.fluxcd.io/requestedAt=<token> --overwrite`) stamps
	// a fresh token to force an out-of-band reconcile; once the
	// controller completes a reconcile it copies that token here, so
	// tooling can poll status to learn the manual trigger was handled.
	// Plain string rather than the Flux meta.ReconcileRequestStatus
	// type so api/ takes no dependency on fluxcd/controller-runtime
	// packages.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`

	// History records the most-recent N revisions that the operator
	// has retained in storage (the chronological keep-set). N comes
	// from JsonnetSnippetSpec.History (default 1, max 50). Index 0 is
	// the most recent revision (== Revision); higher indexes are
	// older. Downstream consumers can pin to a specific historical
	// rev via its sha256 — useful for rollback / blue-green flows.
	// Only populated on JsonnetSnippet (libraries don't publish
	// artifacts), but lives here so the generated DeepCopy covers it
	// uniformly.
	// +optional
	History []RevisionEntry `json:"history,omitempty"`
}

// GetConditions returns the status conditions of the JsonnetSnippet.
//
// This and SetConditions satisfy the conditions-accessor contract that
// generic condition-manipulation helpers expect. The methods deal only in
// apimachinery's metav1.Condition, so the API package takes no dependency
// on the controller-runtime or Flux condition packages — the helpers live
// in the operator package and assert the interface there.
func (in *JsonnetSnippet) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions replaces the status conditions of the JsonnetSnippet.
func (in *JsonnetSnippet) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetConditions returns the status conditions of the JsonnetLibrary.
func (in *JsonnetLibrary) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions replaces the status conditions of the JsonnetLibrary.
func (in *JsonnetLibrary) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// RevisionEntry is one row in SyncStatus.History — the revision string
// (sha256:hex) plus the wall-clock time the reconciler published it.
type RevisionEntry struct {
	// Revision is the sha256-prefixed content hash, matching the
	// format of SyncStatus.Revision.
	Revision string `json:"revision"`

	// Time the operator published this revision.
	Time metav1.Time `json:"time"`
}

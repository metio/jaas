/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JsonnetSnippetSpec describes a Jsonnet snippet whose evaluation result the
// controller publishes as an ExternalArtifact for downstream Flux consumers.
//
// +kubebuilder:validation:XValidation:rule="(has(self.files) && size(self.files) > 0 ? 1 : 0) + (has(self.sourceRef) ? 1 : 0) == 1",message="exactly one of spec.files or spec.sourceRef must be set"
type JsonnetSnippetSpec struct {
	// ServiceAccountName is the ServiceAccount the controller impersonates
	// for every Kubernetes API call done on behalf of this snippet — source
	// fetches and ExternalArtifact upserts. Empty means the operator's
	// --default-service-account is used; missing both denies reconciliation.
	// The SA must live in this snippet's namespace.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// EntryFile names the file (relative to the resolved snippet source
	// root) that go-jsonnet evaluates. Defaults to "main.jsonnet" so
	// existing snippets that omit the field keep working. Operators
	// embedding a snippet inside a larger Flux source whose root has
	// multiple .jsonnet files can point at a specific one — typical
	// pattern for shared dashboards repos that publish many snippets
	// from one tree.
	//
	// Restricted to relative `[A-Za-z0-9._/-]+` paths with no `..`
	// segments by the CRD's structural schema (the Pattern + XValidation
	// markers below), which the apiserver enforces on every write — a
	// validating-webhook bypass does not disable it. The reconciler never
	// resolves this as a filesystem path: it is only a key into the
	// resolved source's in-memory file map and the eval diagnostic label,
	// so there is no on-disk traversal to re-guard at reconcile time.
	// +kubebuilder:default=main.jsonnet
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._/-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="entryFile must not contain '..'"
	// +optional
	EntryFile string `json:"entryFile,omitempty"`

	// Source provides the snippet's bytes (inline or via a Flux source).
	SnippetSource `json:",inline"`

	// Libraries explicitly enumerates which JsonnetLibrary CRs are
	// importable from this snippet. Resolved by import path at eval
	// time; libraries not listed here are invisible to the snippet even
	// when present in the cluster.
	// +optional
	Libraries []LibraryRef `json:"libraries,omitempty"`

	// TLAs are top-level arguments passed to the snippet's outermost
	// function. Each entry binds one argument by name; set code: true to
	// pass a number, array, or object instead of a string.
	// +listType=map
	// +listMapKey=name
	// +optional
	TLAs []JsonnetVariable `json:"tlas,omitempty"`

	// ExternalVariables seed std.extVar lookups for this snippet's evaluation.
	// Each entry binds one variable by name; set code: true to bind a
	// number, array, or object instead of a string.
	//
	// Names conflicting with the operator's --ext-var set are rejected at
	// admission; if the webhook is bypassed, the reconciler still refuses
	// the conflicting name and reports Ready=False with reason
	// ExternalVariableConflict.
	// +listType=map
	// +listMapKey=name
	// +optional
	ExternalVariables []JsonnetVariable `json:"externalVariables,omitempty"`

	// Output selects what bytes the published ExternalArtifact carries. With
	// "rendered" (the default) the artifact contains the evaluated JSON;
	// with "source" it carries the snippet's raw .jsonnet/.libsonnet files,
	// useful for downstream consumers that want to re-evaluate themselves.
	// +kubebuilder:default=rendered
	// +kubebuilder:validation:Enum=rendered;source
	// +optional
	Output string `json:"output,omitempty"`

	// Suspend pauses reconciliation for this snippet without deleting it.
	// While true, the operator skips the eval pipeline, leaves the
	// existing ExternalArtifact in place, and flips Ready=False with
	// reason "Suspended". Setting it back to false (or omitting the
	// field) resumes reconciliation. Mirrors Flux's spec.suspend
	// convention so operators familiar with Flux source CRs can pause
	// JsonnetSnippets the same way.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// History caps how many past revisions of the published artifact
	// the operator retains in storage. Default 1 keeps only the
	// currently-published revision (the historical behavior). Setting
	// to N > 1 lets downstream Flux consumers pin to an older
	// revision via its sha256 — useful for rollback / blue-green
	// flows. The reconciler tracks the keep-set in status.history.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=50
	// +optional
	History int32 `json:"history,omitempty"`

	// Interval is the period between successful reconciles. When set,
	// the controller re-renders the snippet on this cadence even with
	// no watch event — picks up external-variable env drift, OCI
	// library refreshes, and any other state outside the watched
	// graph. Mirrors the Flux source-controller convention. Failed
	// reconciles still use controller-runtime's exponential backoff;
	// interval governs only the steady-state cadence.
	//
	// Bounded at admission: 30s ≤ interval ≤ 24h. A sub-30s cadence
	// floods the apiserver + Publisher with churn for no observable
	// gain (Flux source watches already wake the snippet on real
	// changes); past 24h, a snippet may as well rely solely on watch
	// events. The CEL rule converts the duration string to seconds
	// for comparison so "1h", "60m", and "3600s" all parse uniformly.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s') && duration(self) <= duration('24h')",message="interval must be between 30s and 24h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=jsnip
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Revision",type=string,priority=1,JSONPath=`.status.revision`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.artifactURL`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,priority=1,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// JsonnetSnippet is the published unit of Jsonnet evaluation.
type JsonnetSnippet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JsonnetSnippetSpec `json:"spec,omitempty"`
	Status SyncStatus         `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// JsonnetSnippetList is the list wrapper for JsonnetSnippet.
type JsonnetSnippetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JsonnetSnippet `json:"items"`
}

/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JsonnetLibrarySpec describes a namespaced Jsonnet library that a
// JsonnetSnippet in the same tenancy boundary can import.
//
// +kubebuilder:validation:XValidation:rule="(has(self.files) && size(self.files) > 0 ? 1 : 0) + (has(self.sourceRef) ? 1 : 0) == 1",message="exactly one of spec.files or spec.sourceRef must be set"
type JsonnetLibrarySpec struct {
	SnippetSource `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=jlib
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// Ready/Revision printcolumns are intentionally absent: no controller
// reconciles JsonnetLibrary today, so any status-derived column would
// always be blank and look like a broken object. Status fields stay on
// the type (shared SyncStatus) so adding a reconciler later is purely
// additive — at that point, restore the printcolumns.

// JsonnetLibrary is a reusable bundle of .libsonnet files that snippets can
// import. The import alias is set on the snippet side via LibraryRef.ImportPath
// (defaulting to metadata.name) — the library itself carries no registration
// name.
type JsonnetLibrary struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JsonnetLibrarySpec `json:"spec,omitempty"`
	Status SyncStatus         `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// JsonnetLibraryList is the list wrapper for JsonnetLibrary.
type JsonnetLibraryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JsonnetLibrary `json:"items"`
}

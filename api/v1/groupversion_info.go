/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package v1 holds the JaaS CRD types served at jaas.metio.wtf/v1 when the
// binary is launched with --enable-flux-integration. The HTTP-only evaluator
// path does not import this package.
//
// +kubebuilder:object:generate=true
// +groupName=jaas.metio.wtf
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "jaas.metio.wtf", Version: "v1"}

	// SchemeBuilder uses apimachinery's runtime helper rather than
	// sigs.k8s.io/controller-runtime/pkg/scheme so this package stays free
	// of the controller-runtime dependency. That keeps it cheap for tools
	// (CRD generation, validators) to import without dragging in the
	// manager runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(
		GroupVersion,
		&JsonnetSnippet{}, &JsonnetSnippetList{},
		&JsonnetLibrary{}, &JsonnetLibraryList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

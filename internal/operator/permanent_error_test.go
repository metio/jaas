/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestIsPermanentAPIError_ClassifiesEveryPermanentKind pins that the
// classifier must catch every kind of apiserver error that can't heal
// by retry. Forbidden (RBAC), NoMatch / NotRegistered (CRD missing),
// Invalid / BadRequest (schema mismatch), MethodNotSupported (verb
// not accepted on the resource).
func TestIsPermanentAPIError_ClassifiesEveryPermanentKind(t *testing.T) {
	gr := schema.GroupResource{Group: "test.io", Resource: "widgets"}
	gk := schema.GroupKind{Group: "test.io", Kind: "Widget"}

	cases := map[string]error{
		"Forbidden":          apierrors.NewForbidden(gr, "w", errors.New("rbac")),
		"NoMatchError":       &apimeta.NoKindMatchError{GroupKind: gk},
		"Invalid":            apierrors.NewInvalid(gk, "w", nil),
		"BadRequest":         apierrors.NewBadRequest("bad payload"),
		"MethodNotSupported": apierrors.NewMethodNotSupported(gr, "patch"),
	}
	for name, err := range cases {
		if !isPermanentAPIError(err) {
			t.Errorf("%s should be permanent; isPermanentAPIError reported transient", name)
		}
	}

	// Negative controls — these must NOT be classified permanent.
	transient := map[string]error{
		"NotFound":           apierrors.NewNotFound(gr, "w"),
		"Conflict":           apierrors.NewConflict(gr, "w", errors.New("rv moved")),
		"ServerTimeout":      apierrors.NewServerTimeout(gr, "get", 5),
		"ServiceUnavailable": apierrors.NewServiceUnavailable("apiserver"),
		"plain error":        errors.New("network blip"),
	}
	for name, err := range transient {
		if isPermanentAPIError(err) {
			t.Errorf("%s should be transient; isPermanentAPIError reported permanent", name)
		}
	}
}

// TestRbacDenialMessage_NamesEachPermanentKind pins that the user-facing
// message routes through a kind-specific branch so the diagnosis names
// the right remediation (grant the verb, install the CRD, fix the
// payload). A generic "%v" fallthrough would lose the actionability.
func TestRbacDenialMessage_NamesEachPermanentKind(t *testing.T) {
	gr := schema.GroupResource{Group: "test.io", Resource: "widgets"}
	gk := schema.GroupKind{Group: "test.io", Kind: "Widget"}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"Forbidden", apierrors.NewForbidden(gr, "w", errors.New("rbac")), "grant the tenant ServiceAccount the missing verb"},
		{"NoMatchError", &apimeta.NoKindMatchError{GroupKind: gk}, "install the corresponding CRD"},
		{"Invalid", apierrors.NewInvalid(gk, "w", nil), "violates the CRD's validation"},
		{"BadRequest", apierrors.NewBadRequest("bad payload"), "violates the CRD's validation"},
		{"MethodNotSupported", apierrors.NewMethodNotSupported(gr, "patch"), "deprecated or the operator is talking to an unexpected schema version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rbacDenialMessage("publishing the artifact", tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("rbacDenialMessage for %s = %q, want substring %q", tc.name, got, tc.want)
			}
		})
	}
}

/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func restMapperWithKinds(kinds ...string) apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{jaasv1.GroupVersion})
	for _, k := range kinds {
		m.Add(jaasv1.GroupVersion.WithKind(k), apimeta.RESTScopeNamespace)
	}
	return m
}

func TestEnsureOwnCRDsInstalled(t *testing.T) {
	tests := []struct {
		name      string
		kinds     []string
		wantErr   bool
		wantInErr string
	}{
		{name: "both CRDs present", kinds: []string{"JsonnetSnippet", "JsonnetLibrary"}, wantErr: false},
		{name: "library CRD missing", kinds: []string{"JsonnetSnippet"}, wantErr: true, wantInErr: "JsonnetLibrary"},
		{name: "snippet CRD missing", kinds: []string{"JsonnetLibrary"}, wantErr: true, wantInErr: "JsonnetSnippet"},
		{name: "both CRDs missing", kinds: nil, wantErr: true, wantInErr: "JsonnetSnippet"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ensureOwnCRDsInstalled(restMapperWithKinds(tc.kinds...))
			if tc.wantErr != (err != nil) {
				t.Fatalf("ensureOwnCRDsInstalled() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not name the missing kind %q", err.Error(), tc.wantInErr)
			}
			// The message must point the operator at the remedy, not just
			// report the absence.
			if !strings.Contains(err.Error(), "config/crd/bases") {
				t.Errorf("error %q does not name the remedy", err.Error())
			}
		})
	}
}

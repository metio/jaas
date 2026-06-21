// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package cliflags_test

import (
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/metio/jaas/internal/cliflags"
)

// validFlags registers the flag set and parses no arguments, so every field
// holds its (valid) default. Tests then mutate a single field to an invalid
// value to pin one rejection at a time.
func validFlags(t *testing.T) *cliflags.Flags {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f := cliflags.Register(fs, func() int { return 16 })
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	return f
}

func TestFlags_Validate_AcceptsDefaults(t *testing.T) {
	if err := validFlags(t).Validate(); err != nil {
		t.Fatalf("defaults should validate, got %v", err)
	}
}

// Zero is a documented, valid value for the caps (disable / use the engine
// default) and for the timeouts — only negatives are rejected.
func TestFlags_Validate_AcceptsZeroCapsAndTimeouts(t *testing.T) {
	f := validFlags(t)
	*f.MaxStack = 0
	*f.MaxConcurrentEvals = 0
	*f.RerenderBurst = 0
	*f.MaxArtifactBytes = 0
	*f.EvaluationTimeout = 0
	*f.ShutdownDelay = 0
	if err := f.Validate(); err != nil {
		t.Fatalf("zero caps/timeouts should validate, got %v", err)
	}
}

func TestFlags_Validate_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cliflags.Flags)
	}{
		{"negative max-stack", func(f *cliflags.Flags) { *f.MaxStack = -1 }},
		{"negative max-concurrent-evals", func(f *cliflags.Flags) { *f.MaxConcurrentEvals = -1 }},
		{"negative rerender-burst", func(f *cliflags.Flags) { *f.RerenderBurst = -1 }},
		{"negative max-artifact-bytes", func(f *cliflags.Flags) { *f.MaxArtifactBytes = -1 }},
		{"port zero", func(f *cliflags.Flags) { *f.Port = "0" }},
		{"port too large", func(f *cliflags.Flags) { *f.Port = "70000" }},
		{"port non-numeric", func(f *cliflags.Flags) { *f.Port = "http" }},
		{"management-port out of range", func(f *cliflags.Flags) { *f.ManagementPort = "99999" }},
		{"storage-port negative", func(f *cliflags.Flags) { *f.StoragePort = "-1" }},
		{"webhook-port zero", func(f *cliflags.Flags) { *f.WebhookPort = 0 }},
		{"webhook-port too large", func(f *cliflags.Flags) { *f.WebhookPort = 70000 }},
		{"negative evaluation-timeout", func(f *cliflags.Flags) { *f.EvaluationTimeout = -time.Second }},
		{"negative shutdown-delay", func(f *cliflags.Flags) { *f.ShutdownDelay = -time.Second }},
		{"negative webhook-cert-validity", func(f *cliflags.Flags) { *f.WebhookCertValidity = -time.Hour }},
		// An endpoint path with a space / brace / slash would panic net/http's
		// pattern parser at bind time; empty registers a dead route. Validate
		// must reject all of these as a clean exit-2 flag error instead.
		{"endpoint-path with space", func(f *cliflags.Flags) { *f.JsonnetEndpointPath = "a b" }},
		{"endpoint-path with brace", func(f *cliflags.Flags) { *f.JsonnetEndpointPath = "a{x}" }},
		{"endpoint-path with slash", func(f *cliflags.Flags) { *f.JsonnetEndpointPath = "a/b" }},
		{"endpoint-path empty", func(f *cliflags.Flags) { *f.JsonnetEndpointPath = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := validFlags(t)
			tt.mutate(f)
			if err := f.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error, got nil", tt.name)
			}
		})
	}
}

/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"strings"
	"testing"
)

func TestParseRerenderRate_HappyCases(t *testing.T) {
	cases := []struct {
		in     string
		wantPS float64
	}{
		{"1/sec", 1.0},
		{"60/min", 1.0},
		{"3600/hour", 1.0},
		{"30/s", 30.0},
		{"30/seconds", 30.0},
		{"120/m", 2.0},
		{"120/minutes", 2.0},
		{"0/sec", 0.0},
		{"7200/h", 2.0},
		{"7200/hr", 2.0},
		{"7200/hours", 2.0},
		{"  60  /  min  ", 1.0},
		{"0.5/sec", 0.5},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseRerenderRate(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if abs(got-c.wantPS) > 1e-9 {
				t.Errorf("got %v, want %v per sec", got, c.wantPS)
			}
		})
	}
}

func TestParseRerenderRate_ErrorCases(t *testing.T) {
	cases := []struct {
		in       string
		wantText string
	}{
		{"", "want N/period"},
		{"60", "want N/period"},
		{"60/", "unknown period"},
		{"abc/min", "parse N"},
		{"-5/sec", "non-negative"},
		{"5/decade", "unknown period"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := ParseRerenderRate(c.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantText)
			}
			if !strings.Contains(err.Error(), c.wantText) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantText)
			}
		})
	}
}

func TestParseExtVars_HappyCases(t *testing.T) {
	got, err := ParseExtVars([]string{
		"cluster=prod",
		"region=eu-west-1",
		"empty=",
		"with=value=signs=are=ok",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"cluster": "prod",
		"region":  "eu-west-1",
		"empty":   "",
		"with":    "value=signs=are=ok",
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

func TestParseExtVars_NilInputReturnsNilMap(t *testing.T) {
	got, err := ParseExtVars(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for nil input", got)
	}
}

func TestParseExtVars_EmptySliceReturnsNilMap(t *testing.T) {
	got, err := ParseExtVars([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for empty input", got)
	}
}

func TestParseExtVars_ErrorCases(t *testing.T) {
	cases := []struct {
		in       []string
		wantText string
	}{
		{[]string{"noequals"}, "missing '='"},
		{[]string{"=value"}, "empty key"},
		{[]string{"good=ok", "=bad"}, "empty key"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.in, ","), func(t *testing.T) {
			_, err := ParseExtVars(c.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantText)
			}
			if !strings.Contains(err.Error(), c.wantText) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantText)
			}
		})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

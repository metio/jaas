// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
)

// A single entry above the consumers' hard per-entry extraction cap must fail
// the publish — with no operator MaxArtifactBytes set at all. Publishing it
// would stamp Ready=True on an artifact every fetcher (jaas chaining,
// stageset) silently refuses, permanently.
func TestPublisher_RejectsEntryOverConsumerCap(t *testing.T) {
	p := newTestPublisher(t, nil)
	c := newPublisherClient(t)
	snip := sampleSnippet() // Output defaults to rendered: one rendered.json entry

	huge := strings.Repeat("x", int(sources.DefaultMaxPerEntryBytes)+1)
	_, err := p.Publish(context.Background(), c, snip, huge, nil, nil)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Publish = %v, want ErrArtifactTooLarge for an entry consumers cannot extract", err)
	}
	if !strings.Contains(err.Error(), "rendered.json") {
		t.Errorf("error should name the offending entry, got: %v", err)
	}
}

// Entries individually under the per-entry cap but summing past the aggregate
// extraction cap must fail too — the consumer's ErrTarballTooLarge would
// reject the whole fetch.
func TestPublisher_RejectsTotalOverConsumerCap(t *testing.T) {
	p := newTestPublisher(t, nil)
	c := newPublisherClient(t)
	snip := sampleSnippet()
	snip.Spec.Output = jaasv1.OutputSource

	chunk := strings.Repeat("y", 13<<20) // 13 MiB, under the 16 MiB per-entry cap
	files := map[string]string{}
	for _, name := range []string{"a.jsonnet", "b.jsonnet", "c.jsonnet", "d.jsonnet", "e.jsonnet"} {
		files[name] = chunk // 65 MiB total, over the 64 MiB aggregate cap
	}
	_, err := p.Publish(context.Background(), c, snip, "", files, nil)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Publish = %v, want ErrArtifactTooLarge for a total consumers cannot extract", err)
	}
}

// The operator's own MaxArtifactBytes knob still tightens below the consumer
// caps — the unconditional consumer bound is a ceiling, not a replacement.
func TestPublisher_OperatorCapStillTightens(t *testing.T) {
	p := newTestPublisher(t, nil)
	p.MaxArtifactBytes = 16
	c := newPublisherClient(t)
	snip := sampleSnippet()

	_, err := p.Publish(context.Background(), c, snip, strings.Repeat("z", 64), nil, nil)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Publish = %v, want ErrArtifactTooLarge from the operator cap", err)
	}
}

// A source entry whose name every consumer's extractor silently drops must
// fail the publish as a spec problem — the file would never arrive downstream.
func TestPublisher_RejectsUnsafeSourceEntryName(t *testing.T) {
	p := newTestPublisher(t, nil)
	c := newPublisherClient(t)
	snip := sampleSnippet()
	snip.Spec.Output = jaasv1.OutputSource

	cases := map[string]string{
		"space":       "deploy config.yaml",
		"dot-segment": ".hidden/main.jsonnet",
		"traversal":   "../escape.jsonnet",
		"backslash":   `windows\path.jsonnet`,
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"main.jsonnet": "{}", key: "{}"}
			_, err := p.Publish(context.Background(), c, snip, "", files, nil)
			if !errors.Is(err, ErrUnsafeEntryName) {
				t.Fatalf("Publish with key %q = %v, want ErrUnsafeEntryName", key, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", key)) {
				t.Errorf("error should name the offending key, got: %v", err)
			}
		})
	}
}

// Safe names keep publishing — the guard must not overshoot the charset every
// consumer accepts.
func TestPublisher_SafeSourceNamesPublish(t *testing.T) {
	p := newTestPublisher(t, nil)
	c := newPublisherClient(t)
	snip := sampleSnippet()
	snip.Spec.Output = jaasv1.OutputSource

	files := map[string]string{
		"main.jsonnet":              "{}",
		"lib/helpers.libsonnet":     "{}",
		"data/config_v2.snake.json": "{}",
		"UPPER-case.and-dash.txt":   "ok",
	}
	if _, err := p.Publish(context.Background(), c, snip, "", files, nil); err != nil {
		t.Fatalf("Publish with consumer-safe names should succeed: %v", err)
	}
}

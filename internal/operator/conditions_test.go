/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/metio/jaas/internal/sources"
	"github.com/metio/jaas/internal/urlguard"
)

// TestAllReasons_HaveRunbookPages is the drift gate: every wire-stable
// Reason listed in AllReasons must have a matching docs/runbooks/<reason>.md.
// Adding a new Reason requires shipping the page in the same change.
func TestAllReasons_HaveRunbookPages(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(here))) // …/internal/operator/<file> → repo root
	runbookDir := filepath.Join(repoRoot, "docs", "runbooks")
	for _, reason := range AllReasons {
		name := strings.ToLower(reason) + ".md"
		path := filepath.Join(runbookDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Reason %q has no runbook page at docs/runbooks/%s", reason, name)
		}
	}
}

// TestAllReasons_CoversEveryConstant guards the reverse direction: a
// new ReasonXxx constant added to conditions.go without being appended
// to AllReasons would silently bypass the drift gate. We can't reflect
// over Go consts, so we grep the source file for the constant pattern
// and compare counts.
func TestAllReasons_CoversEveryConstant(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(here), "conditions.go"))
	if err != nil {
		t.Fatalf("read conditions.go: %v", err)
	}
	var declared []string
	for line := range strings.SplitSeq(string(src), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Reason") {
			continue
		}
		// Match "ReasonName = \"...\"".
		before, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		declared = append(declared, strings.TrimSpace(before))
	}
	if len(declared) != len(AllReasons) {
		t.Errorf("conditions.go declares %d Reason* constants but AllReasons has %d entries — keep them in sync.\n  declared: %v\n  AllReasons (len %d)", len(declared), len(AllReasons), declared, len(AllReasons))
	}
}

func TestSnippetReconciler_DecorateMessage(t *testing.T) {
	r := &SnippetReconciler{}
	got := r.decorateMessage(ReasonInvalidSpec, "boom")
	want := "boom (runbook: https://jaas.projects.metio.wtf/runbooks/invalidspec/)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Healthy / intentional reasons (Synced, Suspended, Pending) must NOT
// carry a runbook suffix — these states are not actionable and the link
// would point at a page that doesn't exist.
func TestDecorateMessage_HappyReasonsHaveNoRunbookSuffix(t *testing.T) {
	r := &SnippetReconciler{}
	for _, reason := range []string{ReasonSynced, ReasonSuspended, ReasonPending} {
		t.Run(reason, func(t *testing.T) {
			got := r.decorateMessage(reason, "all good")
			if got != "all good" {
				t.Errorf("Reason=%s got %q, want unchanged %q", reason, got, "all good")
			}
		})
	}
}

// TestClassifyFetchError_NonTransientCases pins the non-transient
// classifications of the Fetcher's error taxonomy. Non-transient means
// the reconciler stops engaging backoff and lets the next genuine
// watch event drive the retry — corruption / RBAC / missing-CRD don't
// recover by retrying.
func TestClassifyFetchError_NonTransientCases(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantMsgIn string
	}{
		{
			name:      "Forbidden from apiserver",
			err:       apierrors.NewForbidden(schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "gitrepositories"}, "configs", errors.New("user system:serviceaccount:team-a:default cannot get resource")),
			wantMsgIn: "RBAC denied",
		},
		{
			name:      "NoMatchError (CRD not registered)",
			err:       &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "source.toolkit.fluxcd.io", Kind: "GitRepository"}, SearchedVersions: []string{"v1"}},
			wantMsgIn: "kind not registered with the apiserver",
		},
		{
			name:      "digest mismatch",
			err:       sources.ErrDigestMismatch,
			wantMsgIn: "digest",
		},
		{
			name:      "urlguard forbidden host",
			err:       urlguard.ErrForbiddenHost,
			wantMsgIn: "forbidden surface",
		},
		// Tarball-shape sentinels — retry can't shrink/sanitize
		// the upstream artifact. Must classify non-transient.
		{
			name:      "ErrArtifactBodyTooLarge",
			err:       sources.ErrArtifactBodyTooLarge,
			wantMsgIn: "exceeded aggregate cap",
		},
		{
			name:      "ErrTarballTooLarge",
			err:       sources.ErrTarballTooLarge,
			wantMsgIn: "aggregate size exceeded cap",
		},
		{
			name:      "ErrTarEntryTooLarge",
			err:       sources.ErrTarEntryTooLarge,
			wantMsgIn: "exceeded per-entry cap",
		},
		{
			name:      "ErrDecompressedTooLarge",
			err:       sources.ErrDecompressedTooLarge,
			wantMsgIn: "decompressed gzip stream exceeded",
		},
		{
			name:      "ErrArtifactNotFound (permanent 4xx)",
			err:       sources.ErrArtifactNotFound,
			wantMsgIn: "permanent HTTP error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, msg, transient := classifyFetchError(tc.err)
			if transient {
				t.Errorf("transient=true, want false (this class is non-recoverable)")
			}
			if !strings.Contains(msg, tc.wantMsgIn) {
				t.Errorf("msg = %q, want it to contain %q", msg, tc.wantMsgIn)
			}
		})
	}
}

// TestClassifyFetchError_TransientCases pins the cases that SHOULD
// engage backoff (because retry is the right response).
func TestClassifyFetchError_TransientCases(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"source not ready", sources.ErrSourceNotReady},
		{"artifact missing", sources.ErrArtifactMissing},
		{"unclassified network error", errors.New("connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, transient := classifyFetchError(tc.err)
			if !transient {
				t.Error("transient=false, want true (this class is recoverable by retry)")
			}
		})
	}
}

// silence unused-import warnings when the assertions above don't
// reference every newly-imported symbol in some refactor.
var _ = metav1.ConditionTrue

// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package sources

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- verifyExpectedDigest -------------------------------------------------

func TestVerifyExpectedDigest_EmptyExpectedIsAccepted(t *testing.T) {
	if err := verifyExpectedDigest("deadbeef", ""); err != nil {
		t.Fatalf("empty expected should be accepted, got %v", err)
	}
}

func TestVerifyExpectedDigest_MatchingSHA256Passes(t *testing.T) {
	got := strings.Repeat("a", 64)
	if err := verifyExpectedDigest(got, "sha256:"+got); err != nil {
		t.Fatalf("matching digest rejected: %v", err)
	}
}

func TestVerifyExpectedDigest_UpperCaseExpectedNormalisesAndMatches(t *testing.T) {
	got := strings.Repeat("a", 64)
	if err := verifyExpectedDigest(got, "SHA256:"+strings.ToUpper(got)); err != nil {
		t.Fatalf("upper-case digest rejected: %v", err)
	}
}

func TestVerifyExpectedDigest_MismatchReturnsErrDigestMismatch(t *testing.T) {
	got := strings.Repeat("a", 64)
	want := strings.Repeat("b", 64)
	err := verifyExpectedDigest(got, "sha256:"+want)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
}

func TestVerifyExpectedDigest_MalformedExpectedReturnsErrDigestInvalid(t *testing.T) {
	cases := map[string]string{
		"missing colon":      strings.Repeat("a", 64),
		"missing algorithm":  ":" + strings.Repeat("a", 64),
		"missing value":      "sha256:",
		"unsupported algo":   "sha512:" + strings.Repeat("a", 128),
		"short hex":          "sha256:" + strings.Repeat("a", 63),
		"long hex":           "sha256:" + strings.Repeat("a", 65),
		"non-hex characters": "sha256:" + strings.Repeat("z", 64),
		"only whitespace":    "   ",
	}
	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			err := verifyExpectedDigest(strings.Repeat("a", 64), expected)
			if !errors.Is(err, ErrDigestInvalid) {
				t.Fatalf("expected ErrDigestInvalid for %q, got %v", expected, err)
			}
		})
	}
}

// --- parseDigest ----------------------------------------------------------

func TestParseDigest_ValidSHA256SplitsParts(t *testing.T) {
	hexVal := strings.Repeat("0", 64)
	algo, got, err := parseDigest("  sha256:" + strings.ToUpper(hexVal) + "  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if algo != "sha256" {
		t.Errorf("algo = %q, want sha256", algo)
	}
	if got != hexVal {
		t.Errorf("hex = %q, want %q (normalised lower-case)", got, hexVal)
	}
}

func TestParseDigest_EmptyReturnsErrDigestInvalid(t *testing.T) {
	if _, _, err := parseDigest(""); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("got %v, want ErrDigestInvalid", err)
	}
}

func TestParseDigest_ColonAtBoundaryReturnsErrDigestInvalid(t *testing.T) {
	// colon at index 0 (no algorithm) and trailing colon (no value).
	for _, in := range []string{":abc", "sha256:"} {
		if _, _, err := parseDigest(in); !errors.Is(err, ErrDigestInvalid) {
			t.Errorf("parseDigest(%q) = %v, want ErrDigestInvalid", in, err)
		}
	}
}

// --- expectedHexLength ----------------------------------------------------

func TestExpectedHexLength_KnownAndUnknown(t *testing.T) {
	if got := expectedHexLength("sha256"); got != 64 {
		t.Errorf("sha256 hex length = %d, want 64", got)
	}
	if got := expectedHexLength("sha512"); got != 0 {
		t.Errorf("unknown algo hex length = %d, want 0", got)
	}
}

// --- readArtifactDigest ---------------------------------------------------

func TestReadArtifactDigest_AbsentReturnsEmpty(t *testing.T) {
	got, err := readArtifactDigest(map[string]any{})
	if err != nil || got != "" {
		t.Fatalf("absent digest = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestReadArtifactDigest_NilValueReturnsEmpty(t *testing.T) {
	got, err := readArtifactDigest(map[string]any{"digest": nil})
	if err != nil || got != "" {
		t.Fatalf("nil digest = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestReadArtifactDigest_StringValueReturned(t *testing.T) {
	got, err := readArtifactDigest(map[string]any{"digest": "sha256:abc"})
	if err != nil || got != "sha256:abc" {
		t.Fatalf("string digest = (%q, %v), want (\"sha256:abc\", nil)", got, err)
	}
}

func TestReadArtifactDigest_NonStringReturnsErrDigestInvalid(t *testing.T) {
	for name, v := range map[string]any{
		"int":     42,
		"bool":    true,
		"object":  map[string]any{"x": 1},
		"float64": 3.14,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readArtifactDigest(map[string]any{"digest": v}); !errors.Is(err, ErrDigestInvalid) {
				t.Fatalf("non-string digest %T = %v, want ErrDigestInvalid", v, err)
			}
		})
	}
}

// --- readyState -----------------------------------------------------------

func TestReadyState_NoConditionsSlice(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	ok, why := readyState(obj)
	if ok || why == "" {
		t.Fatalf("absent conditions = (%v, %q), want (false, reason)", ok, why)
	}
}

func TestReadyState_EmptyConditionsSlice(t *testing.T) {
	ok, why := readyState(unstructuredWithConditions(nil))
	if ok || !strings.Contains(why, "empty") {
		t.Fatalf("empty conditions = (%v, %q), want (false, …empty…)", ok, why)
	}
}

func TestReadyState_ReadyTrue(t *testing.T) {
	ok, why := readyState(unstructuredWithConditions([]map[string]any{
		{"type": "Ready", "status": "True"},
	}))
	if !ok || why != "" {
		t.Fatalf("Ready=True = (%v, %q), want (true, \"\")", ok, why)
	}
}

func TestReadyState_ReadyFalseWithReason(t *testing.T) {
	ok, why := readyState(unstructuredWithConditions([]map[string]any{
		{"type": "Ready", "status": "False", "reason": "Fetching"},
	}))
	if ok || !strings.Contains(why, "Fetching") {
		t.Fatalf("Ready=False/Fetching = (%v, %q), want (false, …Fetching…)", ok, why)
	}
}

func TestReadyState_ReadyFalseWithoutReason(t *testing.T) {
	ok, why := readyState(unstructuredWithConditions([]map[string]any{
		{"type": "Ready", "status": "False"},
	}))
	if ok || !strings.Contains(why, "Ready=False") {
		t.Fatalf("Ready=False (no reason) = (%v, %q), want (false, …Ready=False…)", ok, why)
	}
}

func TestReadyState_NonMapEntrySkipped(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		"not-a-map",
		map[string]any{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	ok, _ := readyState(obj)
	if !ok {
		t.Fatalf("non-map entry should be skipped and Ready=True honoured")
	}
}

func TestReadyState_NoReadyEntryAmongOthers(t *testing.T) {
	ok, why := readyState(unstructuredWithConditions([]map[string]any{
		{"type": "Stalled", "status": "True"},
	}))
	if ok || !strings.Contains(why, "no Ready") {
		t.Fatalf("no Ready entry = (%v, %q), want (false, …no Ready…)", ok, why)
	}
}

// --- isPermanentHTTPStatus ------------------------------------------------

func TestIsPermanentHTTPStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusNotFound:            true,
		http.StatusForbidden:           true,
		http.StatusBadRequest:          true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
		http.StatusOK:                  false,
	}
	for code, want := range cases {
		if got := isPermanentHTTPStatus(code); got != want {
			t.Errorf("isPermanentHTTPStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// --- downloadToTemp -------------------------------------------------------

func TestDownloadToTemp_HappyPathReturnsDigest(t *testing.T) {
	body := []byte("hello world")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher()
	tmp, gotHex, err := f.downloadToTemp(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanupTemp(tmp)

	if gotHex != hexSHA256(body) {
		t.Errorf("digest = %q, want %q", gotHex, hexSHA256(body))
	}
}

func TestDownloadToTemp_PermanentStatusWrapsErrArtifactNotFound(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusBadRequest} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		f := newTestFetcher()
		_, _, err := f.downloadToTemp(context.Background(), srv.URL)
		srv.Close()
		if !errors.Is(err, ErrArtifactNotFound) {
			t.Errorf("HTTP %d = %v, want ErrArtifactNotFound", code, err)
		}
	}
}

func TestDownloadToTemp_TransientStatusDoesNotWrap(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusRequestTimeout, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		f := newTestFetcher()
		_, _, err := f.downloadToTemp(context.Background(), srv.URL)
		srv.Close()
		if err == nil {
			t.Errorf("HTTP %d returned nil error", code)
		}
		if errors.Is(err, ErrArtifactNotFound) {
			t.Errorf("HTTP %d wrapped ErrArtifactNotFound, want bare transient error", code)
		}
	}
}

func TestDownloadToTemp_OversizedBodyReturnsErrArtifactBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 64))
	}))
	defer srv.Close()

	f := newTestFetcher()
	f.MaxArchiveBytes = 16
	_, _, err := f.downloadToTemp(context.Background(), srv.URL)
	if !errors.Is(err, ErrArtifactBodyTooLarge) {
		t.Fatalf("oversized body = %v, want ErrArtifactBodyTooLarge", err)
	}
}

func TestDownloadToTemp_BodyAtCapSucceeds(t *testing.T) {
	body := make([]byte, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher()
	f.MaxArchiveBytes = 16
	tmp, _, err := f.downloadToTemp(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("body exactly at cap should succeed, got %v", err)
	}
	cleanupTemp(tmp)
}

func TestDownloadToTemp_CancelledContextReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := newTestFetcher()
	if _, _, err := f.downloadToTemp(ctx, srv.URL); err == nil {
		t.Fatalf("cancelled context should produce an error")
	}
}

func TestDownloadToTemp_BadURLReturnsBuildRequestError(t *testing.T) {
	f := newTestFetcher()
	// A control character in the URL fails http.NewRequestWithContext.
	if _, _, err := f.downloadToTemp(context.Background(), "http://exa\x7fmple"); err == nil {
		t.Fatalf("malformed URL should fail request build")
	}
}

// --- controlConn ----------------------------------------------------------

func TestControlConn_ForbiddenIPRejected(t *testing.T) {
	f := New() // default IP validator rejects loopback
	if err := f.controlConn("tcp", "127.0.0.1:443", emptyRawConn{}); err == nil {
		t.Fatalf("loopback IP should be rejected by default validator")
	}
}

func TestControlConn_AllowedIPAccepted(t *testing.T) {
	f := newTestFetcher() // permissive IP validator
	if err := f.controlConn("tcp", "127.0.0.1:443", emptyRawConn{}); err != nil {
		t.Fatalf("permissive validator should accept loopback, got %v", err)
	}
}

func TestControlConn_UnparseableAddressReturnsError(t *testing.T) {
	f := newTestFetcher()
	// Missing port → net.SplitHostPort fails.
	if err := f.controlConn("tcp", "127.0.0.1", emptyRawConn{}); err == nil {
		t.Fatalf("address without port should fail SplitHostPort")
	}
}

func TestControlConn_NonIPHostReturnsError(t *testing.T) {
	f := newTestFetcher()
	// Host is a name, not an IP literal → net.ParseIP returns nil.
	if err := f.controlConn("tcp", "example.com:443", emptyRawConn{}); err == nil {
		t.Fatalf("non-IP host should fail; controlConn requires a resolved IP literal")
	}
}

// --- helpers --------------------------------------------------------------

func cleanupTemp(tmp *os.File) {
	if tmp == nil {
		return
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())
}

// emptyRawConn satisfies syscall.RawConn for controlConn, which never
// touches it.
type emptyRawConn struct{}

func (emptyRawConn) Control(func(uintptr)) error    { return nil }
func (emptyRawConn) Read(func(uintptr) bool) error  { return nil }
func (emptyRawConn) Write(func(uintptr) bool) error { return nil }

var _ syscall.RawConn = emptyRawConn{}

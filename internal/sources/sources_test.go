/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package sources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/urlguard"
)

// buildTarGz returns a gzipped tar archive constructed from the supplied
// files map (path → content). Empty files map yields a valid empty
// archive.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// extractTarball wraps extractTarballWithLimits with the default per-entry
// and decompression caps — a test convenience for the many cases that only
// vary the aggregate cap.
func extractTarball(r io.Reader, pathPrefix string, maxBytes int64) (map[string]string, error) {
	return extractTarballWithLimits(r, pathPrefix, maxBytes, 0, 0)
}

// verifyDigest is a buffered-body digest verifier for tests; production
// hashes inline via verifyExpectedDigest in downloadToTemp.
func verifyDigest(body []byte, expected string) error {
	if expected == "" {
		return nil
	}
	sum := sha256.Sum256(body)
	return verifyExpectedDigest(hex.EncodeToString(sum[:]), expected)
}

func TestExtractTarball_HappyPath(t *testing.T) {
	body := buildTarGz(t, map[string]string{
		"main.jsonnet":     `{ ok: true }`,
		"helper.libsonnet": `{ helper: true }`,
	})
	got, err := extractTarball(bytes.NewReader(body), "", 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["main.jsonnet"] != `{ ok: true }` {
		t.Errorf("main.jsonnet = %q", got["main.jsonnet"])
	}
	if got["helper.libsonnet"] != `{ helper: true }` {
		t.Errorf("helper.libsonnet = %q", got["helper.libsonnet"])
	}
}

func TestExtractTarball_PathPrefixFiltersAndStrips(t *testing.T) {
	body := buildTarGz(t, map[string]string{
		"manifests/main.jsonnet":        `{ a: 1 }`,
		"manifests/helper.libsonnet":    `{ b: 2 }`,
		"docs/README.md":                `# ignored`,
		"manifests/sub/inner.libsonnet": `{ c: 3 }`,
	})
	got, err := extractTarball(bytes.NewReader(body), "manifests", 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["main.jsonnet"]; !ok {
		t.Errorf("missing main.jsonnet after prefix strip")
	}
	if _, ok := got["sub/inner.libsonnet"]; !ok {
		t.Errorf("missing sub/inner.libsonnet after prefix strip")
	}
	if _, ok := got["README.md"]; ok {
		t.Errorf("README.md leaked through prefix filter")
	}
}

func TestExtractTarball_RejectsTraversalEntries(t *testing.T) {
	// "../foo" stays as ".." after Clean → rejected.
	// "deeply/../foo" cleans to "foo" — safe, stays inside the archive
	// root. The filter only blocks paths that escape the root.
	body := buildTarGz(t, map[string]string{
		"../escape":           "evil",
		"a/b/../../../escape": "evil",
		"legitimate.jsonnet":  `{}`,
	})
	got, err := extractTarball(bytes.NewReader(body), "", 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["legitimate.jsonnet"]; !ok {
		t.Errorf("legitimate entry was dropped along with traversal entries")
	}
	if _, ok := got["escape"]; ok {
		t.Errorf("../escape leaked through after Clean")
	}
}

func TestExtractTarball_RejectsAbsoluteEntries(t *testing.T) {
	body := buildTarGz(t, map[string]string{
		"/etc/passwd":     "evil",
		"legit.libsonnet": "{}",
	})
	got, err := extractTarball(bytes.NewReader(body), "", 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["/etc/passwd"]; ok {
		t.Errorf("absolute path leaked through")
	}
	if _, ok := got["legit.libsonnet"]; !ok {
		t.Errorf("legitimate file dropped along with absolute path")
	}
}

func TestExtractTarball_OversizeRejected(t *testing.T) {
	body := buildTarGz(t, map[string]string{
		"big.txt": strings.Repeat("x", 10*1024),
	})
	if _, err := extractTarball(bytes.NewReader(body), "", 1024); err == nil {
		t.Errorf("expected oversize error")
	}
}

// Regression: a crafted tar entry whose hdr.Size is near math.MaxInt64
// must be rejected by the budget check, not allowed to overflow the
// int64 accumulator past zero and then trigger an unbounded
// make([]byte, hdr.Size) that crashes the pod with runtime: out of
// memory. The check has to look at hdr.Size against the REMAINING
// budget (maxBytes - total) BEFORE adding to total — otherwise the
// post-add `total > maxBytes` comparison silently misses on overflow.
func TestExtractTarball_HugeEntrySizeRejectedWithoutOverflow(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// First entry primes total > 0 so the next add wraps the int64
	// accumulator when summed with math.MaxInt64.
	if err := tw.WriteHeader(&tar.Header{
		Name: "tiny.libsonnet", Mode: 0o644, Size: 1,
		Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}); err != nil {
		t.Fatalf("write tiny header: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write tiny body: %v", err)
	}

	// Second entry: declare math.MaxInt64 bytes via PAX extended
	// headers (the ustar 12-octal-digit Size field can't hold values
	// past 8 GiB; PAX records the size in an x-attribute the tar
	// reader honors when populating Header.Size). No body is written
	// — the budget check must reject before the per-entry allocation,
	// which is the whole point of the fix.
	if err := tw.WriteHeader(&tar.Header{
		Name: "evil.libsonnet", Mode: 0o644, Size: math.MaxInt64,
		Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}); err != nil {
		t.Fatalf("write evil header: %v", err)
	}
	// tw.Close errors because we declared MaxInt64 bytes of body and
	// wrote none; the buffer already carries the malicious header so
	// we don't care about Close's error.
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}

	_, err := extractTarball(bytes.NewReader(buf.Bytes()), "", 64<<20)
	if err == nil {
		t.Fatal("expected oversize-cap error, got nil")
	}
	if !errors.Is(err, ErrTarEntryTooLarge) {
		t.Errorf("expected ErrTarEntryTooLarge sentinel, got: %v", err)
	}
}

// The decompressed-size cap must bound the gzip OUTPUT, not the
// compressed input. A skipped entry (dot-prefixed, dropped by
// normaliseEntry) is never per-entry-checked nor counted in the
// aggregate budget, yet tar.Next still decompresses its body to reach
// the next header. A highly-compressible body therefore stays tiny on
// the wire — so a cap wrapped around the compressed stream never trips
// — while expanding past the decompressed budget once inflated. The cap
// has to see the inflated bytes for this to be caught.
func TestExtractTarball_SkippedEntryDecompressionTripsCap(t *testing.T) {
	// 1 MiB of a single byte compresses to ~1 KiB. Named with a leading
	// dot so normaliseEntry drops it before any size check.
	body := buildTarGz(t, map[string]string{
		".padding": strings.Repeat("A", 1<<20),
	})
	// maxBytes / perEntryBytes large enough that only the decompressed
	// cap (64 KiB) can fire; the skipped entry isn't subject to them
	// anyway.
	_, err := extractTarballWithLimits(bytes.NewReader(body), "", 1<<30, 1<<30, 64<<10)
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge (skipped-entry body must count against the decompressed cap)", err)
	}
}

// errStuckReader yields one byte per Read forever — never EOF. It drives
// cappedReader's over-budget probe branch deterministically.
type byteAtATimeReader struct {
	data []byte
	pos  int
}

func (r *byteAtATimeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// zeroNilReader returns (0, nil) on the first call then (0, io.EOF) — the
// pathological io.Reader shape cappedReader must defend against at the
// budget boundary (an endless (0,nil) loop would hang the extractor).
type zeroNilReader struct{ calls int }

func (r *zeroNilReader) Read(_ []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		return 0, nil
	}
	return 0, io.EOF
}

func TestCappedReader_Read(t *testing.T) {
	errCap := errors.New("over cap")

	t.Run("under budget passes through to EOF", func(t *testing.T) {
		cr := newCappedReader(bytes.NewReader([]byte("hello")), 100, errCap)
		got, err := io.ReadAll(cr)
		if err != nil {
			t.Fatalf("ReadAll error = %v, want nil", err)
		}
		if string(got) != "hello" {
			t.Fatalf("got %q, want %q", got, "hello")
		}
	})

	t.Run("exact budget then clean EOF does not trip", func(t *testing.T) {
		// Five bytes, budget five: the bytes drain to the boundary, then
		// the probe sees a clean EOF rather than more data.
		cr := newCappedReader(&byteAtATimeReader{data: []byte("abcde")}, 5, errCap)
		got, err := io.ReadAll(cr)
		if err != nil {
			t.Fatalf("ReadAll error = %v, want nil", err)
		}
		if string(got) != "abcde" {
			t.Fatalf("got %q, want %q", got, "abcde")
		}
	})

	t.Run("over budget trips with cause mid-stream", func(t *testing.T) {
		// Budget three, six bytes available: after draining three the probe
		// finds a fourth byte and trips.
		cr := newCappedReader(&byteAtATimeReader{data: []byte("abcdef")}, 3, errCap)
		_, err := io.ReadAll(cr)
		if !errors.Is(err, errCap) {
			t.Fatalf("err = %v, want errCap", err)
		}
	})

	t.Run("repeated Read after trip keeps returning cause", func(t *testing.T) {
		cr := newCappedReader(&byteAtATimeReader{data: []byte("abcdef")}, 3, errCap)
		buf := make([]byte, 16)
		// Drain until the trip.
		var tripErr error
		for range 10 {
			if _, tripErr = cr.Read(buf); tripErr != nil {
				break
			}
		}
		if !errors.Is(tripErr, errCap) {
			t.Fatalf("first non-nil err = %v, want errCap", tripErr)
		}
		// A subsequent Read must still report the cause, not block or EOF.
		if _, err := cr.Read(buf); !errors.Is(err, errCap) {
			t.Fatalf("post-trip err = %v, want errCap", err)
		}
	})

	t.Run("zero-nil at boundary trips defensively", func(t *testing.T) {
		// Budget zero forces the probe immediately; the underlying answers
		// (0, nil), which cappedReader must convert to a trip.
		cr := newCappedReader(&zeroNilReader{}, 0, errCap)
		if _, err := cr.Read(make([]byte, 4)); !errors.Is(err, errCap) {
			t.Fatalf("err = %v, want errCap on (0,nil) boundary", err)
		}
	})
}

func TestExtractTarball_CorruptGzipReturnsError(t *testing.T) {
	if _, err := extractTarball(strings.NewReader("not a gzip stream"), "", 1<<20); err == nil {
		t.Errorf("corrupt gzip accepted")
	}
}

func TestExtractTarball_CorruptTarReturnsError(t *testing.T) {
	// A valid gzip stream wrapping garbage bytes that don't form a tar.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("totally not a tar archive, just bytes"))
	_ = gz.Close()
	if _, err := extractTarball(&buf, "", 1<<20); err == nil {
		t.Errorf("corrupt tar accepted")
	}
}

func TestExtractTarball_NonRegularEntriesSkipped(t *testing.T) {
	// Tar with a symlink entry alongside a regular file. The symlink must
	// be dropped silently; the regular file survives.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeSymlink,
	})
	_ = tw.WriteHeader(&tar.Header{
		Name: "main.jsonnet", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	_ = gz.Close()
	got, err := extractTarball(&buf, "", 1<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["evil-link"]; ok {
		t.Errorf("symlink leaked through")
	}
	if got["main.jsonnet"] != "{}" {
		t.Errorf("regular file dropped: %v", got)
	}
}

func TestVerifyDigest_Match(t *testing.T) {
	body := []byte("hello")
	digest := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := verifyDigest(body, digest); err != nil {
		t.Errorf("matching digest rejected: %v", err)
	}
}

func TestVerifyDigest_Mismatch(t *testing.T) {
	// Full-length valid hex that doesn't match — exercises the actual
	// mismatch branch (not the parser-rejection branch tested below).
	wrongFullLength := "sha256:" + strings.Repeat("f", 64)
	if err := verifyDigest([]byte("hello"), wrongFullLength); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("got %v, want ErrDigestMismatch", err)
	}
}

func TestVerifyDigest_InvalidDigestStrings(t *testing.T) {
	cases := []struct {
		name, digest string
	}{
		{"short hex", "sha256:deadbeef"},
		{"no algo prefix", strings.Repeat("a", 64)},
		{"unsupported algo", "md5:" + strings.Repeat("0", 32)},
		{"non-hex value", "sha256:" + strings.Repeat("z", 64)},
		{"missing value", "sha256:"},
		{"missing algo", ":" + strings.Repeat("0", 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyDigest([]byte("body"), c.digest)
			if !errors.Is(err, ErrDigestInvalid) {
				t.Errorf("got %v, want errors.Is(err, ErrDigestInvalid)", err)
			}
		})
	}
}

func TestVerifyDigest_EmptyExpectedAccepted(t *testing.T) {
	if err := verifyDigest([]byte("anything"), ""); err != nil {
		t.Errorf("empty digest rejected: %v", err)
	}
}

func TestIsReady_TrueCondition(t *testing.T) {
	obj := unstructuredWithConditions([]map[string]interface{}{
		{"type": "Ready", "status": "True"},
	})
	if ok, _ := readyState(obj); !ok {
		t.Errorf("Ready=True not recognised")
	}
}

func TestIsReady_FalseCondition(t *testing.T) {
	obj := unstructuredWithConditions([]map[string]interface{}{
		{"type": "Ready", "status": "False"},
	})
	if ok, _ := readyState(obj); ok {
		t.Errorf("Ready=False misread as ready")
	}
}

func TestIsReady_OtherConditionsOnly(t *testing.T) {
	obj := unstructuredWithConditions([]map[string]interface{}{
		{"type": "Reconciling", "status": "True"},
	})
	if ok, _ := readyState(obj); ok {
		t.Errorf("no Ready condition misread as ready")
	}
}

func TestIsReady_MissingConditionsSlice(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if ok, _ := readyState(obj); ok {
		t.Errorf("missing conditions misread as ready")
	}
}

func TestReadyState_DiagnosticByShape(t *testing.T) {
	cases := []struct {
		name      string
		obj       *unstructured.Unstructured
		wantReady bool
		wantWhy   string
	}{
		{
			name:      "no conditions at all",
			obj:       &unstructured.Unstructured{Object: map[string]interface{}{}},
			wantReady: false,
			wantWhy:   "status.conditions not yet populated",
		},
		{
			name: "empty conditions slice",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{"conditions": []interface{}{}},
				},
			},
			wantReady: false,
			wantWhy:   "status.conditions empty",
		},
		{
			name: "Ready=False with reason",
			obj: unstructuredWithConditions([]map[string]interface{}{
				{"type": "Ready", "status": "False", "reason": "BuildFailed"},
			}),
			wantReady: false,
			wantWhy:   "Ready=False/BuildFailed",
		},
		{
			name: "Ready=False without reason",
			obj: unstructuredWithConditions([]map[string]interface{}{
				{"type": "Ready", "status": "False"},
			}),
			wantReady: false,
			wantWhy:   "Ready=False",
		},
		{
			name: "no Ready condition among others",
			obj: unstructuredWithConditions([]map[string]interface{}{
				{"type": "Reconciling", "status": "True"},
			}),
			wantReady: false,
			wantWhy:   "no Ready condition",
		},
		{
			name: "Ready=True",
			obj: unstructuredWithConditions([]map[string]interface{}{
				{"type": "Ready", "status": "True"},
			}),
			wantReady: true,
			wantWhy:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := readyState(tc.obj)
			if ok != tc.wantReady {
				t.Errorf("ready = %v, want %v", ok, tc.wantReady)
			}
			if why != tc.wantWhy {
				t.Errorf("why = %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

func TestIsReady_NonMapEntryInConditionsIsIgnored(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"conditions": []interface{}{
					"not a map",
					map[string]interface{}{"type": "Ready", "status": "True"},
				},
			},
		},
	}
	if ok, _ := readyState(obj); !ok {
		t.Errorf("Ready=True ignored after a non-map entry")
	}
}

func TestReadArtifact_HappyPath(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"artifact": map[string]interface{}{
					"url":      "http://x/y.tar.gz",
					"revision": "rev1",
					"digest":   "sha256:abc",
				},
			},
		},
	}
	a, err := readArtifact(obj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.URL != "http://x/y.tar.gz" {
		t.Errorf("URL = %q", a.URL)
	}
	if a.Revision != "rev1" {
		t.Errorf("Revision = %q", a.Revision)
	}
	if a.Digest != "sha256:abc" {
		t.Errorf("Digest = %q", a.Digest)
	}
}

func TestReadArtifact_MissingArtifact(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if _, err := readArtifact(obj); !errors.Is(err, ErrArtifactMissing) {
		t.Errorf("got %v, want ErrArtifactMissing", err)
	}
}

func TestReadArtifact_MissingURL(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"artifact": map[string]interface{}{
					"revision": "rev1",
				},
			},
		},
	}
	if _, err := readArtifact(obj); err == nil {
		t.Errorf("empty URL accepted")
	}
}

func TestReadArtifact_BadShape(t *testing.T) {
	// status.artifact is a string, not a map — NestedMap returns an error.
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"artifact": "not a map",
			},
		},
	}
	if _, err := readArtifact(obj); err == nil {
		t.Errorf("malformed shape accepted")
	}
}

// --- Fetch end-to-end -------------------------------------------------------

// newTestFetcher returns a Fetcher with PermissiveHTTPURL installed so
// httptest's 127.0.0.1 listeners reach Fetch. Production callers use
// New() (which falls back to urlguard.ValidateHTTPURL).
func newTestFetcher() *Fetcher {
	f := New()
	f.URLValidator = urlguard.PermissiveHTTPURL
	// Permit loopback at dial time too so httptest's 127.0.0.1 listeners
	// reach Fetch; production leaves this nil (urlguard.ForbiddenIP).
	f.IPValidator = func(net.IP) error { return nil }
	return f
}

func TestFetch_NilRefReturnsError(t *testing.T) {
	f := newTestFetcher()
	if _, err := f.Fetch(context.Background(), fake.NewClientBuilder().Build(), nil, ""); err == nil {
		t.Errorf("nil ref accepted")
	}
}

func TestFetch_NilClientReturnsError(t *testing.T) {
	f := newTestFetcher()
	if _, err := f.Fetch(context.Background(), nil,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "x"}, "ns"); err == nil {
		t.Errorf("nil client accepted")
	}
}

func TestFetch_HappyPath_HTTPServedTarball(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"main.jsonnet": `{ from: "git" }`,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	// Build a Flux GitRepository-shaped Unstructured pointing at the
	// httptest URL. status.conditions has Ready=True and
	// status.artifact has matching url+digest.
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))

	c := fake.NewClientBuilder().
		WithScheme(schemeWithGVK(t, "GitRepository")).
		WithObjects(src).
		Build()

	f := newTestFetcher()
	got, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Files["main.jsonnet"] != `{ from: "git" }` {
		t.Errorf("main.jsonnet = %q", got.Files["main.jsonnet"])
	}
}

// TestFetch_IPv6Loopback_HTTPServedTarball exercises the whole fetch path over
// IPv6: a bracketed-IPv6 artifact URL must parse, pass the URL check, dial
// through the Control hook (which sees an IPv6 address), and download. Skips
// where IPv6 loopback is unavailable so it never flakes on IPv4-only runners.
func TestFetch_IPv6Loopback_HTTPServedTarball(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	tarball := buildTarGz(t, map[string]string{"main.jsonnet": `{ from: "ipv6" }`})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	// srv.URL is now http://[::1]:<port>.
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))
	c := fake.NewClientBuilder().
		WithScheme(schemeWithGVK(t, "GitRepository")).
		WithObjects(src).
		Build()

	f := newTestFetcher()
	got, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err != nil {
		t.Fatalf("IPv6 fetch failed: %v", err)
	}
	if got.Files["main.jsonnet"] != `{ from: "ipv6" }` {
		t.Errorf("main.jsonnet = %q", got.Files["main.jsonnet"])
	}
}

func TestFetch_NotFoundSurfacesAsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "ghost", Namespace: "team-a"}, "team-a")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("got %v, want a not-found error", err)
	}
}

func TestFetch_SourceNotReady_ReturnsErrSourceNotReady(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{"x.jsonnet": "{}"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))
	_ = unstructured.SetNestedSlice(src.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False"},
	}, "status", "conditions")

	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if !errors.Is(err, ErrSourceNotReady) {
		t.Errorf("got %v, want ErrSourceNotReady", err)
	}
}

func TestFetch_ArtifactMissing_ReturnsErrArtifactMissing(t *testing.T) {
	src := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": defaultSourceAPIVersion,
			"kind":       "GitRepository",
			"metadata": map[string]interface{}{
				"name":      "configs",
				"namespace": "team-a",
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{"type": "Ready", "status": "True"},
				},
			},
		},
	}
	src.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	})

	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if !errors.Is(err, ErrArtifactMissing) {
		t.Errorf("got %v, want ErrArtifactMissing", err)
	}
}

func TestFetch_DigestMismatchReturnsError(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{"x.jsonnet": "{}"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	// Source CR advertises a wrong digest. Must include the sha256:
	// algorithm prefix or the parser would reject it as malformed
	// (ErrDigestInvalid) before reaching the mismatch branch.
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL,
		"sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("got %v, want ErrDigestMismatch", err)
	}
}

func TestFetch_HTTPNon200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, "")
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err == nil || !strings.Contains(err.Error(), "HTTP 410") {
		t.Errorf("got %v, want an HTTP 410 error", err)
	}
}

func TestFetch_DownloadExceedsMaxBytesReturnsError(t *testing.T) {
	// Server replies with a body larger than the configured cap.
	big := bytes.Repeat([]byte("x"), 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, "")
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := &Fetcher{HTTPClient: srv.Client(), MaxArchiveBytes: 1024, URLValidator: urlguard.PermissiveHTTPURL}
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err == nil || !errors.Is(err, ErrArtifactBodyTooLarge) {
		t.Errorf("got %v, want ErrArtifactBodyTooLarge sentinel", err)
	}
}

// The compressed-download cap (MaxArchiveBytes) and the decompressed
// extracted-result cap (MaxExtractedBytes) are independent. A tarball whose
// compressed body fits comfortably under MaxArchiveBytes but whose extracted
// bodies sum past a small MaxExtractedBytes must be rejected with
// ErrTarballTooLarge — proving the extracted cap is decompressed-based and
// not tied to the on-the-wire size.
func TestFetch_ExtractedExceedsMaxExtractedBytesIndependentOfArchiveCap(t *testing.T) {
	// ~4 KiB of extractable content; highly compressible so the wire body
	// stays well under the generous MaxArchiveBytes.
	tarball := buildTarGz(t, map[string]string{
		"main.jsonnet": strings.Repeat("A", 4<<10),
	})
	if len(tarball) >= 4096 {
		t.Fatalf("compressed tarball %d bytes is not smaller than the extracted total; test premise broken", len(tarball))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	// Generous compressed-download cap, tiny extracted cap: only the
	// extracted (decompressed) budget can fire.
	f.MaxArchiveBytes = 1 << 20
	f.MaxExtractedBytes = 1024
	f.MaxPerEntryBytes = 1 << 20
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if !errors.Is(err, ErrTarballTooLarge) {
		t.Errorf("got %v, want ErrTarballTooLarge (extracted cap independent of compressed cap)", err)
	}
}

// The inverse independence direction: a generous MaxExtractedBytes with a
// tiny MaxArchiveBytes trips the compressed-download cap
// (ErrArtifactBodyTooLarge) before extraction is ever reached.
func TestFetch_GenerousExtractedCapStillTripsArchiveCapOnLargeBody(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"main.jsonnet": strings.Repeat("A", 4<<10),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	// Tiny compressed-download cap below the wire body, generous extracted
	// cap: the body trips ErrArtifactBodyTooLarge before extraction.
	f.MaxArchiveBytes = 16
	f.MaxExtractedBytes = 1 << 30
	if int64(len(tarball)) <= f.MaxArchiveBytes {
		t.Fatalf("compressed tarball %d bytes does not exceed MaxArchiveBytes %d; test premise broken", len(tarball), f.MaxArchiveBytes)
	}
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if !errors.Is(err, ErrArtifactBodyTooLarge) {
		t.Errorf("got %v, want ErrArtifactBodyTooLarge (compressed cap independent of extracted cap)", err)
	}
}

// Regression: the production validator (urlguard.ValidateHTTPURL) must
// reject loopback URLs on status.artifact.url BEFORE any HTTP dial.
// Defends against a tenant with status-write RBAC on a source CR (or a
// poisoned upstream) driving operator-side requests at apiserver / cloud
// metadata. Uses New() — the production Fetcher — not newTestFetcher.
func TestFetch_ProductionValidatorRejectsLoopbackArtifactURL(t *testing.T) {
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a",
		"http://127.0.0.1:8080/evil.tar.gz", "")
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := New() // production validator
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err == nil {
		t.Fatal("expected urlguard to reject loopback, got nil")
	}
	if !errors.Is(err, urlguard.ErrForbiddenHost) {
		t.Errorf("got %v, want errors.Is(err, ErrForbiddenHost)", err)
	}
}

func TestFetch_MalformedURLRejectedBeforeDownload(t *testing.T) {
	// A malformed URL on the source CR is now rejected at the
	// urlguard step before any HTTP I/O. (Pre-urlguard this surfaced
	// from http.NewRequestWithContext during the download.)
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a",
		"http://invalid url with space", "")
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	_, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
	if err == nil {
		t.Fatal("expected URL parse / validation error, got nil")
	}
	if !errors.Is(err, urlguard.ErrParseFailed) && !strings.Contains(err.Error(), "URL") {
		t.Errorf("got %v, want a URL parse / validation error", err)
	}
}

func TestFetch_PathFilterAppliesAtFetchLevel(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"keep/main.jsonnet": `{ keep: true }`,
		"drop/skipped.txt":  `nope`,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, sha256Hex(tarball))
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	got, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs", Namespace: "team-a",
			Path: "keep",
		}, "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Files["main.jsonnet"] != `{ keep: true }` {
		t.Errorf("expected main.jsonnet after path-strip, got %v", got.Files)
	}
	if _, ok := got.Files["drop/skipped.txt"]; ok {
		t.Errorf("drop/ leaked through the prefix filter")
	}
}

func TestFetch_DefaultsAPIVersionAndNamespace(t *testing.T) {
	// ref.APIVersion empty + ref.Namespace empty → defaults apply.
	tarball := buildTarGz(t, map[string]string{"main.jsonnet": "{}"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "owner-ns", srv.URL, sha256Hex(tarball))
	c := fake.NewClientBuilder().WithScheme(schemeWithGVK(t, "GitRepository")).WithObjects(src).Build()
	f := newTestFetcher()
	if _, err := f.Fetch(context.Background(), c,
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}, "owner-ns"); err != nil {
		t.Errorf("defaults failed: %v", err)
	}
}

func TestFetcher_FallbacksWhenZeroValuesSet(t *testing.T) {
	// HTTPClient nil + MaxArchiveBytes/MaxExtractedBytes 0 →
	// maxArchiveBytes/maxExtractedBytes/httpClient fallbacks fire.
	f := &Fetcher{}
	if f.maxArchiveBytes() != defaultMaxArchiveBytes {
		t.Errorf("maxArchiveBytes fallback = %d, want default", f.maxArchiveBytes())
	}
	if f.maxExtractedBytes() != defaultMaxExtractedBytes {
		t.Errorf("maxExtractedBytes fallback = %d, want default", f.maxExtractedBytes())
	}
	if f.httpClient() != http.DefaultClient {
		t.Errorf("httpClient fallback != http.DefaultClient")
	}
}

// --- Helpers ----------------------------------------------------------------

func sha256Hex(body []byte) string {
	return "sha256:" + hexSHA256(body)
}

func hexSHA256(body []byte) string {
	h := newSHAHasher()
	_, _ = h.Write(body)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// newSHAHasher returns a fresh SHA256 hasher; isolated as a tiny helper so
// the imports list inside individual tests stays focused.
func newSHAHasher() hash.Hash { return sha256.New() }

func unstructuredWithConditions(conds []map[string]interface{}) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		items = append(items, c)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	_ = unstructured.SetNestedSlice(obj.Object, items, "status", "conditions")
	return obj
}

func newFluxSourceUnstructured(t *testing.T, kind, name, namespace, url, digest string) *unstructured.Unstructured {
	t.Helper()
	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "source.toolkit.fluxcd.io",
		Version: "v1",
		Kind:    kind,
	})
	src.SetName(name)
	src.SetNamespace(namespace)
	artifact := map[string]interface{}{
		"url":      url,
		"revision": "rev1",
	}
	if digest != "" {
		artifact["digest"] = digest
	}
	_ = unstructured.SetNestedMap(src.Object, artifact, "status", "artifact")
	_ = unstructured.SetNestedSlice(src.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	return src
}

// TestNew_InstallsCheckRedirect_ValidatesEveryHop pins the SSRF
// defence: New() must wire CheckRedirect so a 302 to a denied host is
// rejected before the second dial. Without it, a source CR whose
// initial URL passes urlguard could 302 to cloud-metadata or an
// in-cluster service and the follow would dial it.
func TestNew_InstallsCheckRedirect_ValidatesEveryHop(t *testing.T) {
	f := New()
	if f.HTTPClient == nil || f.HTTPClient.CheckRedirect == nil {
		t.Fatal("New(): HTTPClient.CheckRedirect not installed")
	}
	// Custom validator that accepts the origin URL and rejects the
	// redirect target. The Fetcher.validateURL() helper resolves to
	// f.URLValidator when non-nil, so this drives checkRedirect.
	deny := errors.New("denied by validator")
	f.URLValidator = func(u string) error {
		if strings.Contains(u, "denied") {
			return deny
		}
		return nil
	}

	allow, _ := http.NewRequest(http.MethodGet, "http://example.test/ok", nil)
	if err := f.checkRedirect(allow, nil); err != nil {
		t.Errorf("allowed redirect rejected: %v", err)
	}

	deniedReq, _ := http.NewRequest(http.MethodGet, "http://denied.test/x", nil)
	if err := f.checkRedirect(deniedReq, nil); !errors.Is(err, deny) {
		t.Errorf("denied redirect: got %v, want denied", err)
	}

	// Loop guard: bail at 10 hops to match Go's default.
	loop, _ := http.NewRequest(http.MethodGet, "http://example.test/x", nil)
	via := make([]*http.Request, 10)
	if err := f.checkRedirect(loop, via); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Errorf("hop-limit guard: got %v, want '10 redirects' error", err)
	}
}

// TestControlConn_IPFamilies pins the connection-time half of the SSRF
// defence across BOTH address families: even if a host passes the string-level
// URL check, the dialer's Control hook must refuse the connect when the
// address it resolved to is on the denylist (loopback, link-local,
// unspecified) — and must allow routable addresses — for IPv4 and IPv6 alike.
// The hook receives the concrete IP:port the kernel is about to dial and
// ignores the RawConn, so a nil is fine here. New() installs the production
// IPValidator.
func TestControlConn_IPFamilies(t *testing.T) {
	f := New()
	cases := []struct {
		name      string
		addr      string
		forbidden bool
	}{
		{"ipv4 loopback", "127.0.0.1:80", true},
		{"ipv6 loopback", "[::1]:80", true},
		{"ipv4 link-local metadata", "169.254.169.254:80", true},
		{"ipv6 link-local", "[fe80::1]:80", true},
		{"ipv4 unspecified", "0.0.0.0:80", true},
		{"ipv6 unspecified", "[::]:80", true},
		{"ipv4 routable", "8.8.8.8:443", false},
		{"ipv6 routable", "[2001:db8::1]:443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.controlConn("tcp", tc.addr, nil)
			switch {
			case tc.forbidden && !errors.Is(err, urlguard.ErrForbiddenHost):
				t.Errorf("controlConn(%q) = %v, want ErrForbiddenHost", tc.addr, err)
			case !tc.forbidden && err != nil:
				t.Errorf("controlConn(%q) = %v, want nil (routable)", tc.addr, err)
			}
		})
	}
}

// TestFetcher_AcquireBoundsInFlightDownloads pins the concurrency
// budget: at most MaxConcurrentDownloads downloadToTemp calls can be
// in flight at once. The test runs N+2 simulated downloads against
// an HTTP server that blocks on a channel, observes the peak in-
// flight count via the test handler, and asserts it never exceeds
// MaxConcurrentDownloads.
func TestFetcher_AcquireBoundsInFlightDownloads(t *testing.T) {
	const cap = 2
	const total = cap + 3

	gate := make(chan struct{})
	var (
		inFlight int
		peak     int
		mu       sync.Mutex
	)
	tarball := buildTarGz(t, map[string]string{"main.jsonnet": "{}"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		// Block until the test signals we should drain.
		<-gate
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	f := newTestFetcher()
	f.MaxConcurrentDownloads = cap

	digest := sha256Hex(tarball)
	src := newFluxSourceUnstructured(t, "GitRepository", "configs", "team-a", srv.URL, digest)
	c := fake.NewClientBuilder().
		WithScheme(schemeWithGVK(t, "GitRepository")).
		WithObjects(src).
		Build()

	done := make(chan error, total)
	for i := 0; i < total; i++ {
		go func() {
			_, err := f.Fetch(context.Background(), c,
				&jaasv1.SourceRef{Kind: "GitRepository", Name: "configs", Namespace: "team-a"}, "team-a")
			done <- err
		}()
	}

	// Give the goroutines a moment to fan in. The peak in-flight
	// count must be exactly `cap` — the extra goroutines are stuck
	// in acquire().
	for tries := 0; tries < 100; tries++ {
		mu.Lock()
		got := inFlight
		mu.Unlock()
		if got >= cap {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if inFlight > cap {
		t.Errorf("inFlight=%d exceeded cap=%d", inFlight, cap)
	}
	mu.Unlock()

	// Drain the server one request at a time and watch the peak
	// stay at cap.
	for i := 0; i < total; i++ {
		gate <- struct{}{}
	}
	for i := 0; i < total; i++ {
		if err := <-done; err != nil {
			t.Errorf("Fetch %d: %v", i, err)
		}
	}
	if peak > cap {
		t.Errorf("peak in-flight = %d, want ≤ %d", peak, cap)
	}
}

// schemeWithGVK registers the Flux source kind on a fresh scheme so the
// fake client accepts CRUD against it via Unstructured.
func schemeWithGVK(t *testing.T, kind string) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	gv := schema.GroupVersion{Group: "source.toolkit.fluxcd.io", Version: "v1"}
	s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	return s
}

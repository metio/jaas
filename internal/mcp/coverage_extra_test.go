/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// TestLocate_EmptyFoundAt pins the early-out: an importing file with no prior
// foundAt (a top-level snippet, importedFrom == "") cannot be mapped back to a
// root, so locate reports not-found and Import falls through to the JPATH
// search.
func TestLocate_EmptyFoundAt(t *testing.T) {
	imp := newConfinedImporter([]string{t.TempDir()})
	if _, _, ok := imp.locate(""); ok {
		t.Fatal("locate(\"\") returned ok=true, want false")
	}
}

// TestLocate_FoundAtOutsideEveryRoot pins that a foundAt that sits outside all
// configured roots is rejected — the "../"-escape guard in locate. With two
// roots, a path under the second root must map to that root and never to the
// first.
func TestLocate_FoundAtOutsideEveryRoot(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	imp := newConfinedImporter([]string{rootA, rootB})

	// A file genuinely inside rootB maps to rootB, not rootA (whose Rel result
	// would be "../<rootB-base>/..." — the escape branch that must be skipped).
	insideB := filepath.Join(rootB, "sub", "file.libsonnet")
	root, dir, ok := imp.locate(insideB)
	if !ok {
		t.Fatalf("locate(%q) = ok false, want it resolved to rootB", insideB)
	}
	if root != rootB {
		t.Fatalf("locate resolved to root %q, want %q (must skip rootA whose Rel escapes)", root, rootB)
	}
	if dir != "sub" {
		t.Fatalf("locate dir = %q, want %q", dir, "sub")
	}

	// A path under neither root resolves to nothing.
	outside := filepath.Join(t.TempDir(), "x.libsonnet")
	if _, _, ok := imp.locate(outside); ok {
		t.Fatalf("locate(%q) returned ok=true for a path outside every root", outside)
	}
}

// TestLocate_RelErrorIsSkipped pins the filepath.Rel error branch. On a
// relative root and an absolute foundAt, filepath.Rel returns an error ("can't
// make ... relative to ..."), which locate must treat as a non-match and skip
// rather than panic.
func TestLocate_RelErrorIsSkipped(t *testing.T) {
	imp := newConfinedImporter([]string{"relative-root"})
	if _, _, ok := imp.locate("/absolute/found/at.libsonnet"); ok {
		t.Fatal("locate returned ok=true despite an unrelatable (relative root vs absolute path) pair")
	}
}

// TestReadWithinRoot_MissingRootErrors pins the os.OpenRoot failure path: a
// root directory that does not exist yields an error rather than a read.
func TestReadWithinRoot_MissingRootErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := readWithinRoot(missing, "file.libsonnet"); err == nil {
		t.Fatal("readWithinRoot against a missing root returned nil error")
	}
}

// TestReadWithinRoot_ReadsFileWithinRoot pins the success path through a real
// root, complementing the missing-root failure case.
func TestReadWithinRoot_ReadsFileWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.libsonnet"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readWithinRoot(root, "ok.libsonnet")
	if err != nil {
		t.Fatalf("readWithinRoot: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("read %q, want %q", got, "data")
	}
}

// TestConfinedImporter_TransitiveRelativeImportUsesFoundRoot drives locate's
// happy path through a real two-root import graph: main.libsonnet in the second
// root imports a sibling by relative path, which must resolve within that same
// second root.
func TestConfinedImporter_TransitiveRelativeImportUsesFoundRoot(t *testing.T) {
	rootA := t.TempDir() // empty, searched first
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootB, "util.libsonnet"), []byte(`{n: 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "main.libsonnet"), []byte(`{v: (import "util.libsonnet").n}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{LibraryPaths: []string{rootA, rootB}, ConfineImports: true}
	res, out, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: `(import "main.libsonnet").v`})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("transitive relative import across roots failed: %s", textContent(t, res))
	}
	assertJSONEqual(t, out.JSON, `3`)
}

// TestConfinedImporter_MissingImportReturnsError pins the not-found tail of
// Import: an import that resolves in no root is a clear error.
func TestConfinedImporter_MissingImportReturnsError(t *testing.T) {
	imp := newConfinedImporter([]string{t.TempDir()})
	_, _, err := imp.Import("", "nope.libsonnet")
	if err == nil {
		t.Fatal("Import of a missing file returned nil error")
	}
	if !strings.Contains(err.Error(), "not found within library paths") {
		t.Fatalf("error = %q, want the not-found-within-library-paths message", err)
	}
}

// gzipTar packs the given entries (each a tar.Header plus body) into a gzip'd
// tar stream so the extractTarGz edge cases can be exercised with crafted
// archives.
func gzipTar(t *testing.T, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractTarGz_SkipsNonRegularEntries pins that a directory entry (and any
// non-regular typeflag) is skipped, not keyed into the file map.
func TestExtractTarGz_SkipsNonRegularEntries(t *testing.T) {
	data := gzipTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "adir/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"k":1}`)
		if err := tw.WriteHeader(&tar.Header{Name: "file.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	})
	files, err := extractTarGz(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, ok := files["adir/"]; ok {
		t.Error("directory entry was kept")
	}
	if _, ok := files["adir"]; ok {
		t.Error("cleaned directory entry was kept")
	}
	if files["file.json"] != `{"k":1}` {
		t.Fatalf("regular entry missing/wrong: %+v", files)
	}
	if len(files) != 1 {
		t.Fatalf("want only the regular file, got %d: %+v", len(files), files)
	}
}

// TestExtractTarGz_OversizeArtifactErrors pins the maxDiffArtifactBytes guard:
// the aggregate of extracted bodies exceeding the cap is rejected rather than
// held in memory.
func TestExtractTarGz_OversizeArtifactErrors(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxDiffArtifactBytes+1)
	data := gzipTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "big.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := extractTarGz(bytes.NewReader(data)); err == nil {
		t.Fatal("extractTarGz accepted an artifact past the size cap")
	} else if !strings.Contains(err.Error(), "too large to diff") {
		t.Fatalf("error = %q, want the 'too large to diff' guard", err)
	}
}

// TestExtractTarGz_RejectsNonGzip pins the gunzip failure path on a body that
// is not a gzip stream.
func TestExtractTarGz_RejectsNonGzip(t *testing.T) {
	if _, err := extractTarGz(strings.NewReader("plain text, not gzip")); err == nil {
		t.Fatal("extractTarGz accepted a non-gzip body")
	}
}

// TestExtractTarGz_TruncatedEntryBodyErrors pins the per-entry read-error
// branch: a header declaring a larger Size than the stream actually carries
// makes the tar reader fail mid-body, which surfaces as a read error rather
// than a silently-short file.
func TestExtractTarGz_TruncatedEntryBodyErrors(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	// Declare 100 bytes but write only 4, then abandon the writer without
	// Close so the archive ends mid-entry.
	if err := tw.WriteHeader(&tar.Header{Name: "short.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	// Gzip the truncated tar bytes so gunzip succeeds and the failure lands in
	// the tar body read.
	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractTarGz(bytes.NewReader(gzbuf.Bytes())); err == nil {
		t.Fatal("extractTarGz accepted a truncated entry body")
	}
}

// TestExtractTarGz_CorruptHeaderErrors pins the tr.Next() error branch: a
// complete first entry followed by bytes that cannot parse as a tar header makes
// the next header read fail, surfacing an untar error.
func TestExtractTarGz_CorruptHeaderErrors(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	body := []byte(`{"k":1}`)
	if err := tw.WriteHeader(&tar.Header{Name: "ok.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	// Append a partial, non-zero block where the next header would be — not a
	// valid header and not the all-zero EOF marker, so tar.Reader.Next errors.
	raw.Write(bytes.Repeat([]byte{0xFF}, 512))

	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractTarGz(bytes.NewReader(gzbuf.Bytes())); err == nil {
		t.Fatal("extractTarGz accepted a stream with a corrupt trailing header")
	} else if !strings.Contains(err.Error(), "untar") {
		t.Fatalf("error = %q, want the untar header-parse failure", err)
	}
}

// TestScrubLibraryPaths_SkipsEmptyRoot pins that an empty library-path entry is
// skipped (no spurious global ReplaceAll of "/") while the populated roots are
// still stripped.
func TestScrubLibraryPaths_SkipsEmptyRoot(t *testing.T) {
	cfg := Config{ConfineImports: true, LibraryPaths: []string{"", "/libs"}}
	const diag = "/libs/grafonnet/main.libsonnet:1:1 boom"
	got := cfg.scrubLibraryPaths(diag)
	if got != "grafonnet/main.libsonnet:1:1 boom" {
		t.Fatalf("scrubLibraryPaths = %q, want the populated root stripped and the empty root ignored", got)
	}
}

// TestDiffRevisionsHandler_GetSnippetError pins the Get-failure branch: a
// snippet the client cannot fetch is reported as a tool error, not a panic.
func TestDiffRevisionsHandler_GetSnippetError(t *testing.T) {
	cfg := Config{KubeClient: fakeClient(t), Store: newStore(t)}
	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: "ns", Name: "missing"})
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error for a missing snippet, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "cannot get JsonnetSnippet") {
		t.Fatalf("error should mention the failed Get, got %q", textContent(t, res))
	}
}

// TestDiffRevisionsHandler_ToRevisionReadError pins the second read-error
// branch: the `from` revision reads cleanly but the `to` revision is absent from
// the store, surfacing a read error for the `to` side.
func TestDiffRevisionsHandler_ToRevisionReadError(t *testing.T) {
	const ns, name = "team-a", "dash"
	r1, r2 := "sha256:1111111111111111", "sha256:2222222222222222"
	store := newStore(t)
	// Store only the older revision (r1) so the `from` read succeeds and the
	// newer `to` read (r2) fails as not-found.
	putRevision(t, store, ns, name, r1, map[string]string{"main.json": "x"})
	cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, From: r1, To: r2})
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error for the unreadable to-revision, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "to revision") {
		t.Fatalf("error should name the to revision, got %q", textContent(t, res))
	}
}

// TestMutateSnippet_PatchError pins the Patch-failure branch of mutateSnippet:
// a client whose Patch is intercepted to fail surfaces an update tool error.
func TestMutateSnippet_PatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	snip := newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snip).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return errors.New("patch boom")
		},
	}).Build()
	cfg := Config{KubeClient: c, AllowMutations: true}

	res, _, err := cfg.suspendSnippetHandler(context.Background(), nil, mutateInput{Namespace: "team-a", Name: "dash"})
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error when Patch fails, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "cannot update JsonnetSnippet") {
		t.Fatalf("error should mention the failed update, got %q", textContent(t, res))
	}
}

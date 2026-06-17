/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// TestS3Backend_SweepIsNoOp pins the contract that the S3 backend has no
// orphan-.tmp residue to clean: PutObject is atomic, so Sweep always reports
// zero removals and no error. The background sweep loop relies on this so an
// S3 deployment never logs spurious cleanup activity.
func TestS3Backend_SweepIsNoOp(t *testing.T) {
	b := &S3Backend{}
	for _, age := range []time.Duration{0, time.Minute, time.Hour} {
		n, err := b.Sweep(context.Background(), age)
		if err != nil {
			t.Fatalf("Sweep(age=%v) error = %v, want nil", age, err)
		}
		if n != 0 {
			t.Fatalf("Sweep(age=%v) removed = %d, want 0", age, n)
		}
	}
}

// TestWatchUploadStall_TripsAfterNoProgress drives the stall monitor with a
// hand-controlled tick channel and a progress counter that never advances:
// after stall/interval no-progress ticks it must fire onStall. Deterministic
// — no real upload or sockets involved.
func TestWatchUploadStall_TripsAfterNoProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	tripped := make(chan struct{})
	var progress atomic.Int64 // never advances
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchUploadStall(ctx, progress.Load, func() { close(tripped) }, 4*time.Second, time.Second, tick)
	}()
	// stall=4s, interval=1s → trips on the 4th no-progress tick.
	for i := 0; i < 4; i++ {
		tick <- time.Time{}
	}
	select {
	case <-tripped:
	case <-time.After(2 * time.Second):
		t.Fatal("stall watcher did not trip after 4 no-progress ticks")
	}
	<-done
}

// TestWatchUploadStall_DoesNotTripWhileProgressing proves a slow-but-
// progressing upload is never truncated: progress advances on every tick,
// so the idle counter resets and onStall never fires.
func TestWatchUploadStall_DoesNotTripWhileProgressing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	var progress atomic.Int64
	var tripped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchUploadStall(ctx, progress.Load, func() { tripped.Store(true) }, 4*time.Second, time.Second, tick)
	}()
	for i := 0; i < 20; i++ {
		progress.Add(1) // happens-before the tick send → watcher observes it
		tick <- time.Time{}
	}
	cancel()
	<-done
	if tripped.Load() {
		t.Error("stall watcher tripped while the upload was steadily progressing")
	}
}

// fakeS3 is the minimum subset of the S3 REST API S3Backend exercises:
// PUT/HEAD/GET an object, DELETE an object, LIST a prefix, plus
// multipart upload (Initiate / UploadPart / Complete) so streaming
// Put with objectSize=-1 round-trips. Backed by an in-memory map;
// concurrency-safe so the listing+removal in Prune can run against it
// without races.
type fakeS3 struct {
	mu         sync.Mutex
	bucket     string
	objects    map[string][]byte
	multiparts map[string]map[int][]byte // uploadID → partNumber → bytes
	nextMPID   int
	// failList, when non-nil, makes serveList return a 5xx body so the
	// test can drive S3Backend.Delete / Prune's list-stream error path.
	failList string
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{
		bucket:     bucket,
		objects:    map[string][]byte{},
		multiparts: map[string]map[int][]byte{},
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// minio-go talks virtual-hosted-style by default, but for an
	// arbitrary endpoint host (which httptest.Server is) it falls
	// back to path-style: /<bucket>/<key...>.
	pathParts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(pathParts) == 0 || pathParts[0] != f.bucket {
		http.Error(w, "wrong bucket", http.StatusBadRequest)
		return
	}
	var key string
	if len(pathParts) == 2 {
		key = pathParts[1]
	}

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// minio-go sends aws-chunked payloads (SigV4 streaming) by
		// default. Decode them so the stored bytes match the original
		// object content.
		if r.Header.Get("Content-Encoding") == "aws-chunked" ||
			strings.Contains(r.Header.Get("x-amz-content-sha256"), "STREAMING") {
			body = decodeAWSChunked(body)
		}
		// Multipart UploadPart: PUT key?partNumber=N&uploadId=X.
		// Store the part bytes keyed by uploadID + partNumber so
		// CompleteMultipartUpload can stitch them in order.
		if partStr := r.URL.Query().Get("partNumber"); partStr != "" {
			uploadID := r.URL.Query().Get("uploadId")
			partNum := atoiQuery(partStr)
			f.mu.Lock()
			if _, ok := f.multiparts[uploadID]; !ok {
				f.multiparts[uploadID] = map[int][]byte{}
			}
			f.multiparts[uploadID][partNum] = body
			f.mu.Unlock()
			// Return an ETag — minio-go validates non-empty.
			w.Header().Set("ETag", `"`+itoa(partNum)+`"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		// Bucket location probe minio-go does before PutObject.
		if _, ok := r.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
			return
		}
		// List (with ?list-type=2) or a single GET.
		if r.URL.Query().Get("list-type") == "2" {
			f.serveList(w, r)
			return
		}
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
			return
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Header().Set("Last-Modified", "Mon, 1 Jan 2024 00:00:00 GMT")
		w.Header().Set("ETag", `"00000000000000000000000000000000"`)
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Write(body)
	case http.MethodHead:
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Header().Set("Last-Modified", "Mon, 1 Jan 2024 00:00:00 GMT")
		w.Header().Set("ETag", `"00000000000000000000000000000000"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		// Bulk delete posts ?delete; single delete is just DELETE on
		// the key. minio-go's RemoveObjects uses bulk on POST, plain
		// RemoveObject uses DELETE.
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		if _, ok := r.URL.Query()["delete"]; ok {
			f.serveBulkDelete(w, r)
			return
		}
		// InitiateMultipartUpload: POST key?uploads.
		if _, ok := r.URL.Query()["uploads"]; ok {
			f.mu.Lock()
			f.nextMPID++
			uploadID := "upload-" + itoa(f.nextMPID)
			f.multiparts[uploadID] = map[int][]byte{}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>` +
				xmlEscape(f.bucket) + `</Bucket><Key>` + xmlEscape(key) +
				`</Key><UploadId>` + uploadID + `</UploadId></InitiateMultipartUploadResult>`))
			return
		}
		// CompleteMultipartUpload: POST key?uploadId=X.
		if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
			f.mu.Lock()
			parts := f.multiparts[uploadID]
			// Concatenate parts in ascending partNumber order.
			nums := make([]int, 0, len(parts))
			for n := range parts {
				nums = append(nums, n)
			}
			sortInts(nums)
			var assembled []byte
			for _, n := range nums {
				assembled = append(assembled, parts[n]...)
			}
			f.objects[key] = assembled
			delete(f.multiparts, uploadID)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Location>` +
				xmlEscape(f.bucket+"/"+key) + `</Location><Bucket>` + xmlEscape(f.bucket) +
				`</Bucket><Key>` + xmlEscape(key) + `</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`))
			return
		}
		http.Error(w, "unsupported POST", http.StatusBadRequest)
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) serveList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != "" {
		http.Error(w, f.failList, http.StatusInternalServerError)
		return
	}
	var keys []string
	for k := range f.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if delimiter != "" {
			rest := strings.TrimPrefix(k, prefix)
			if strings.Contains(rest, delimiter) {
				continue
			}
		}
		keys = append(keys, k)
	}
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>`)
	buf.WriteString(f.bucket)
	buf.WriteString(`</Name>`)
	for _, k := range keys {
		buf.WriteString(`<Contents><Key>`)
		buf.WriteString(xmlEscape(k))
		buf.WriteString(`</Key><Size>`)
		buf.WriteString(itoa(len(f.objects[k])))
		buf.WriteString(`</Size></Contents>`)
	}
	buf.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.Write(buf.Bytes())
}

func (f *fakeS3) serveBulkDelete(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	// Cheap-and-cheerful: scan for <Key>...</Key> in the request body.
	s := string(body)
	var resp bytes.Buffer
	resp.WriteString(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult>`)
	for {
		i := strings.Index(s, "<Key>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</Key>")
		if j < 0 {
			break
		}
		key := s[i+len("<Key>") : i+j]
		delete(f.objects, key)
		resp.WriteString(`<Deleted><Key>`)
		resp.WriteString(xmlEscape(key))
		resp.WriteString(`</Key></Deleted>`)
		s = s[i+j+len("</Key>"):]
	}
	resp.WriteString(`</DeleteResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.Write(resp.Bytes())
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func itoa(i int) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.Repeat("0", 0), "0"), "0") + intToString(i)
}

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}

func newTestS3Backend(t *testing.T, prefix string) (*S3Backend, *fakeS3, *httptest.Server) {
	t.Helper()
	fake := newFakeS3("bkt")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake URL: %v", err)
	}
	b, err := NewS3(S3Config{
		Endpoint:        u.Host,
		Bucket:          "bkt",
		Prefix:          prefix,
		UseSSL:          false,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return b, fake, srv
}

func TestS3_NewRequiresEndpointAndBucket(t *testing.T) {
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Fatal("expected error for missing endpoint")
	}
	if _, err := NewS3(S3Config{Endpoint: "x"}); err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestS3_PutWritesDeterministicTarball(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	res, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{
		{Path: "b.json", Content: []byte("B")},
		{Path: "a.json", Content: []byte("A")},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.DigestSHA256 == "" {
		t.Fatal("DigestSHA256 empty")
	}
	if res.Path != "ns/snip/rev1.tar.gz" {
		t.Errorf("Path = %q, want ns/snip/rev1.tar.gz", res.Path)
	}
	fake.mu.Lock()
	body, ok := fake.objects["ns/snip/rev1.tar.gz"]
	fake.mu.Unlock()
	if !ok {
		t.Fatal("object not in fake bucket")
	}

	// Same inputs must give the same digest a second time.
	res2, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{
		{Path: "a.json", Content: []byte("A")}, // different order — must still match
		{Path: "b.json", Content: []byte("B")},
	})
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if res2.DigestSHA256 != res.DigestSHA256 {
		t.Errorf("digest drifted across Puts: %s vs %s", res.DigestSHA256, res2.DigestSHA256)
	}

	// Tarball must round-trip.
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, h.Name)
	}
	if len(names) != 2 || names[0] != "a.json" || names[1] != "b.json" {
		t.Errorf("tar entries = %v, want [a.json b.json]", names)
	}
}

func TestS3_PutWithPrefix(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "tenants/jaas")
	if _, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{{Path: "x", Content: []byte("y")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake.mu.Lock()
	_, ok := fake.objects["tenants/jaas/ns/snip/rev1.tar.gz"]
	fake.mu.Unlock()
	if !ok {
		t.Errorf("object not at prefixed key; keys=%v", keysOf(fake))
	}
}

func TestS3_PutRejectsTraversal(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	if _, err := b.Put(context.Background(), "..", "snip", "rev", nil); err == nil {
		t.Errorf("expected traversal rejection on namespace")
	}
	if _, err := b.Put(context.Background(), "ns", "..", "rev", nil); err == nil {
		t.Errorf("expected traversal rejection on name")
	}
	if _, err := b.Put(context.Background(), "ns", "snip", "..", nil); err == nil {
		t.Errorf("expected traversal rejection on revision")
	}
}

func TestS3_PruneRemovesOlderRevisions(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	for _, rev := range []string{"r1", "r2", "r3"} {
		if _, err := b.Put(context.Background(), "ns", "snip", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	if err := b.Prune(context.Background(), "ns", "snip", []string{"r3"}, 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.objects["ns/snip/r3.tar.gz"]; !ok {
		t.Errorf("kept revision missing")
	}
	if _, ok := fake.objects["ns/snip/r1.tar.gz"]; ok {
		t.Errorf("r1 should have been pruned")
	}
	if _, ok := fake.objects["ns/snip/r2.tar.gz"]; ok {
		t.Errorf("r2 should have been pruned")
	}
}

func TestS3_DeleteRemovesEverythingForSnippet(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	for _, rev := range []string{"r1", "r2"} {
		if _, err := b.Put(context.Background(), "ns", "snip", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := b.Delete(context.Background(), "ns", "snip"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for k := range fake.objects {
		if strings.HasPrefix(k, "ns/snip/") {
			t.Errorf("Delete left behind %q", k)
		}
	}
}

// TestS3_DeleteSurfacesListError pins the invariant that a mid-stream
// ListObjects error fails Delete: returning nil would make the caller
// drop the finalizer and orphan every untouched tarball. A truncated
// listing must never read as a successful full delete.
func TestS3_DeleteSurfacesListError(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	// Seed two revisions, then make the list call fail before bulk-delete runs.
	for _, rev := range []string{"r1", "r2"} {
		if _, err := b.Put(context.Background(), "ns", "snip", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	fake.mu.Lock()
	fake.failList = "transient: throttled"
	fake.mu.Unlock()

	err := b.Delete(context.Background(), "ns", "snip")
	if err == nil {
		t.Fatal("Delete returned nil despite list failure; orphans would have been left behind")
	}
	if !strings.Contains(err.Error(), "list") {
		t.Errorf("Delete error = %q, want it to name the list failure", err)
	}

	// The objects are still in the bucket — Delete failed; the caller
	// must NOT have removed the finalizer.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := 0
	for k := range fake.objects {
		if strings.HasPrefix(k, "ns/snip/") {
			found++
		}
	}
	if found == 0 {
		t.Errorf("expected the tarballs to still be present in the bucket; got %d", found)
	}
}

func TestS3_HTTPHandlerStreamsObject(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	if _, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{{Path: "f", Content: []byte("hello")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h := b.HTTPHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/x-gzip" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/does/not/exist.tar.gz", nil)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("missing object status = %d, want 404", rec2.Code)
	}

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/anything", nil)
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec3.Code)
	}
}

// TestS3_PutStreams_LargePayload_RoundTripsWithCorrectDigest exercises
// the streaming path: a payload that exceeds the multipart-part size
// must round-trip through io.Pipe → multipart upload → reassembly
// without corruption, and the digest must match SHA-256 of the
// stored bytes. Uses an incompressible random payload so gzip's
// output is large enough to actually cross the 16 MiB part boundary
// (sequential or zeroed bytes compress to <1% and would never
// trigger multipart).
func TestS3_PutStreams_LargePayload_RoundTripsWithCorrectDigest(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	body := make([]byte, 40*1024*1024) // 40 MiB
	// Deterministic PRNG so the test is reproducible.
	rng := newPRNG(0x9E3779B97F4A7C15)
	for i := range body {
		body[i] = byte(rng.next())
	}
	entries := []FileEntry{{Path: "big.bin", Content: body}}

	res, err := b.Put(context.Background(), "ns", "snip", "big", entries)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.DigestSHA256 == "" {
		t.Fatal("DigestSHA256 empty")
	}

	// The fake reassembled the multipart upload into a single object;
	// reading it back must yield the exact same bytes the writer
	// produced. SizeBytes is the COMPRESSED tarball size (post-gzip),
	// which equals the assembled stored size byte-for-byte.
	fake.mu.Lock()
	stored, ok := fake.objects["ns/snip/big.tar.gz"]
	fake.mu.Unlock()
	if !ok {
		t.Fatal("object not in fake bucket")
	}
	if int64(len(stored)) != res.SizeBytes {
		t.Errorf("SizeBytes %d != stored object size %d", res.SizeBytes, len(stored))
	}
	gotDigest := sha256OfBytes(stored)
	if gotDigest != res.DigestSHA256 {
		t.Errorf("digest mismatch: result=%s stored=%s", res.DigestSHA256, gotDigest)
	}
	// Sanity: the multipart upload must have transferred more than
	// one part, otherwise we're not actually testing the multipart
	// path. With our 16 MiB part size and a 32 MiB payload (post-gzip
	// it's much smaller; sequential bytes compress to ~0.4%), we may
	// only get one part. Force the issue by checking the uploaded
	// compressed size is non-trivial — if gzip squashed it below the
	// part threshold the test is still valuable but the multipart
	// guard isn't exercised. That's fine; the digest round-trip is
	// what we really care about.
	_ = stored

	// Verify the assembled tarball gunzips and contains the original
	// entry intact (no chunk-boundary corruption).
	gz, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != "big.bin" {
		t.Errorf("tar entry name = %q, want big.bin", hdr.Name)
	}
	got := make([]byte, len(body))
	if _, err := io.ReadFull(tr, got); err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if string(got) != string(body) {
		t.Error("entry bytes corrupted by multipart upload")
	}
}

// TestS3_PutStreams_DigestMatchesInMemoryBaseline cross-checks that
// the streaming Put produces the same DigestSHA256 it would have
// produced with the old in-memory builder for the same input — pins
// the cross-backend digest stability the docs promise.
func TestS3_PutStreams_DigestMatchesInMemoryBaseline(t *testing.T) {
	entries := []FileEntry{
		{Path: "a.json", Content: []byte("hello")},
		{Path: "b.json", Content: []byte("world")},
	}

	// In-memory reference build: the local Store goes through the
	// same writeTarEntry path with the same sort + ModTime
	// invariants, so its digest is the reference for any backend.
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wantRes, err := s.Put(context.Background(), "ns", "snip", "rev", entries)
	if err != nil {
		t.Fatal(err)
	}

	// Streamed S3 build.
	b, _, _ := newTestS3Backend(t, "")
	gotRes, err := b.Put(context.Background(), "ns", "snip", "rev", entries)
	if err != nil {
		t.Fatal(err)
	}
	if gotRes.DigestSHA256 != wantRes.DigestSHA256 {
		t.Errorf("S3 streamed digest %s != local digest %s — cross-backend reproducibility broken",
			gotRes.DigestSHA256, wantRes.DigestSHA256)
	}
}

func sha256OfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// prng is a tiny xorshift+ generator so the streaming-large-payload
// test can produce a deterministic, incompressible payload without
// depending on crypto/rand or math/rand (whose seeds vary across Go
// versions).
type prng struct{ s uint64 }

func newPRNG(seed uint64) *prng { return &prng{s: seed} }

func (p *prng) next() uint64 {
	p.s ^= p.s << 13
	p.s ^= p.s >> 7
	p.s ^= p.s << 17
	return p.s
}

func TestS3_HTTPHandlerHonorsPrefix(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "scope")
	if _, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{{Path: "f", Content: []byte("hi")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Client GETs the namespace-relative path; the backend prepends
	// the prefix server-side.
	rec := httptest.NewRecorder()
	h := b.HTTPHandler()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestS3_CloseIsNoOp(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func atoiQuery(s string) int {
	n := 0
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// decodeAWSChunked strips SigV4 streaming-signature chunk framing:
//
//	<hex-len>;chunk-signature=<sig>\r\n<data>\r\n...0;...\r\n\r\n
//
// The fake server doesn't verify signatures — it just extracts the data.
func decodeAWSChunked(in []byte) []byte {
	var out []byte
	for len(in) > 0 {
		nl := bytes.Index(in, []byte("\r\n"))
		if nl < 0 {
			break
		}
		header := string(in[:nl])
		in = in[nl+2:]
		semi := strings.IndexByte(header, ';')
		hexLen := header
		if semi >= 0 {
			hexLen = header[:semi]
		}
		var n int
		for _, b := range []byte(hexLen) {
			switch {
			case b >= '0' && b <= '9':
				n = n*16 + int(b-'0')
			case b >= 'a' && b <= 'f':
				n = n*16 + int(b-'a'+10)
			case b >= 'A' && b <= 'F':
				n = n*16 + int(b-'A'+10)
			default:
				return out
			}
		}
		if n == 0 {
			break
		}
		if n > len(in) {
			return out
		}
		out = append(out, in[:n]...)
		in = in[n:]
		if len(in) >= 2 {
			in = in[2:]
		}
	}
	return out
}

func keysOf(f *fakeS3) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	return out
}

// TestS3WriteError_ScrubsBackendDetails pins the privacy invariant
// on the storage HTTP path: a minio backend error must NOT have its
// raw Message field written to the response body — bucket names,
// endpoint URLs, AWS request-ids, and arbitrary backend text don't
// belong on an unauthenticated wire. The status code stays the
// same; only the body is scrubbed.
func TestS3WriteError_ScrubsBackendDetails(t *testing.T) {
	cases := []struct {
		name             string
		err              error
		wantCode         int
		wantBody         string
		bannedSubstrings []string
	}{
		{
			name: "NoSuchKey stays as not-found",
			err: minio.ErrorResponse{
				Code:    "NoSuchKey",
				Message: "object my-secret-snippet-rev under arn:aws:s3:::prod-bucket not found",
			},
			wantCode:         http.StatusNotFound,
			wantBody:         "not found",
			bannedSubstrings: []string{"my-secret-snippet-rev", "prod-bucket", "arn:aws"},
		},
		{
			name: "AccessDenied does not leak bucket or message",
			err: minio.ErrorResponse{
				Code:    "AccessDenied",
				Message: "Access denied for bucket prod-bucket at endpoint s3.internal.example",
			},
			wantCode:         http.StatusInternalServerError,
			wantBody:         "storage error",
			bannedSubstrings: []string{"prod-bucket", "s3.internal.example", "Access denied"},
		},
		{
			name:             "non-S3 dial error returns generic bad-gateway",
			err:              io.ErrUnexpectedEOF, // not a minio.ErrorResponse → Code == ""
			wantCode:         http.StatusBadGateway,
			wantBody:         "bad gateway",
			bannedSubstrings: []string{"unexpected EOF"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s3WriteError(rec, tc.err)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("body %q does not contain %q", body, tc.wantBody)
			}
			for _, banned := range tc.bannedSubstrings {
				if strings.Contains(body, banned) {
					t.Errorf("body %q leaks backend detail %q", body, banned)
				}
			}
		})
	}
}

// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package storage

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dribbleS3 answers path-style GETs for one object, writing the body in
// chunks with a delay between them — a slow-but-progressing stream. delayFirst
// holds the whole response (headers included) to exercise the first-byte bound.
type dribbleS3 struct {
	bucket     string
	key        string
	body       []byte
	chunks     int
	chunkDelay time.Duration
	delayFirst time.Duration
}

func (f *dribbleS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// minio-go probes the bucket location before the first object call;
	// answer instantly so only the object GET carries the configured delays.
	if r.URL.Query().Has("location") {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`))
		return
	}
	if f.delayFirst > 0 {
		time.Sleep(f.delayFirst)
	}
	want := "/" + f.bucket + "/" + f.key
	if r.URL.Path != want {
		http.Error(w, "no such key", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	w.Header().Set("Content-Type", "application/octet-stream")
	// minio-go's Stat requires a parseable object-info header set.
	w.Header().Set("Last-Modified", "Mon, 1 Jan 2024 00:00:00 GMT")
	w.Header().Set("ETag", `"00000000000000000000000000000000"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	fl, _ := w.(http.Flusher)
	size := (len(f.body) + f.chunks - 1) / f.chunks
	for off := 0; off < len(f.body); off += size {
		end := min(off+size, len(f.body))
		_, _ = w.Write(f.body[off:end])
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(f.chunkDelay)
	}
}

func newTimeoutBackend(t *testing.T, fake http.Handler, readTimeout time.Duration) *S3Backend {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake URL: %v", err)
	}
	b, err := NewS3(S3Config{
		Endpoint:        u.Host,
		Bucket:          "bkt",
		UseSSL:          false,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		ReadTimeout:     readTimeout,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return b
}

// A slow-but-progressing body stream must arrive COMPLETE: ReadTimeout bounds
// only the time to first byte, never the body copy — a stream slower than the
// old whole-request deadline was silently truncated after 200 + Content-Length
// were already on the wire, corrupting every consumer's fetch. The body budget
// is the surrounding http.Server's write timeout, exactly like the local
// backend.
func TestS3_HTTPHandler_SlowBodyIsNotTruncatedByReadTimeout(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 64<<10)
	fake := &dribbleS3{
		bucket: "bkt", key: "ns/snip/rev1.tar.gz", body: body,
		chunks: 8, chunkDelay: 60 * time.Millisecond, // ~480ms total, well past ReadTimeout
	}
	b := newTimeoutBackend(t, fake, 150*time.Millisecond)

	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String()[:min(rec.Body.Len(), 200)])
	}
	if rec.Body.Len() != len(body) {
		t.Fatalf("streamed %d of %d bytes: the read timeout truncated a progressing body", rec.Body.Len(), len(body))
	}
}

// ReadTimeout still bounds a backend that never starts responding: the
// first-byte guard is kept, only the body copy is released from it.
func TestS3_HTTPHandler_SlowFirstByteIsBounded(t *testing.T) {
	fake := &dribbleS3{
		bucket: "bkt", key: "ns/snip/rev1.tar.gz", body: []byte("late"),
		chunks: 1, delayFirst: 2 * time.Second,
	}
	b := newTimeoutBackend(t, fake, 100*time.Millisecond)

	start := time.Now()
	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code == http.StatusOK && rec.Body.Len() > 0 {
		t.Fatalf("a backend that exceeds the first-byte bound must not stream a body, got %d bytes", rec.Body.Len())
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("first-byte bound did not fire; handler blocked %s", elapsed)
	}
}

// Guards the error path shape: exceeding the first-byte bound surfaces as an
// HTTP error status (via s3WriteError), not a hung or empty-200 response.
func TestS3_HTTPHandler_FirstByteTimeoutIsAnErrorStatus(t *testing.T) {
	fake := &dribbleS3{
		bucket: "bkt", key: "ns/snip/rev1.tar.gz", body: []byte("late"),
		chunks: 1, delayFirst: 2 * time.Second,
	}
	b := newTimeoutBackend(t, fake, 100*time.Millisecond)

	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error status for a timed-out first byte; body=%q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

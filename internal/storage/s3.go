/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Backend stores artifacts in an S3-compatible bucket. The HTTP read
// path streams object bytes back to clients (source-controller) so the
// existing ExternalArtifact contract — a bare HTTP URL pointing at the
// tarball — keeps working without source-controller needing S3 creds.
//
// Concurrency: Put against the same (namespace,name,revision) is safe —
// the object key is derived from the revision and S3's PUT is atomic.
// Multi-writer coordination across replicas is delegated to leader
// election; in practice only the lease-holder calls Put.
type S3Backend struct {
	client *minio.Client
	bucket string
	prefix string
	// readTimeout caps how long a single object stream can take. Zero
	// keeps the http.Server's own write timeout in charge.
	readTimeout time.Duration

	// now reports the wall-clock used by Prune's grace-window
	// comparison. Nil falls back to time.Now; tests override to drive
	// expiry without time.Sleep against fake LastModified values.
	now func() time.Time

	// uploadStallTimeout aborts a Put whose upload makes no progress for
	// this long. It bounds a genuinely-stuck connection without capping a
	// large-but-progressing upload (which a fixed total-time ceiling
	// would truncate, flapping the snippet into an endless re-upload).
	// Zero falls back to defaultUploadStallTimeout. Tests set a small
	// value to exercise the abort.
	uploadStallTimeout time.Duration
}

// defaultUploadStallTimeout is the no-progress window after which a Put
// aborts. Generous enough that an ordinary network pause never trips it,
// short enough that a wedged connection doesn't pin a reconcile for long.
const defaultUploadStallTimeout = 2 * time.Minute

// S3Config captures every S3Backend-relevant setting main.go threads in
// from CLI flags. Auth modes (in order of preference):
//
//  1. AccessKeyID + SecretAccessKey (and optional SessionToken) set
//     explicitly — typically from mounted Secrets.
//  2. Empty creds + IAM/IRSA — minio-go discovers AWS_ACCESS_KEY_ID
//     and friends from the environment, including the IRSA web-identity
//     token chain on EKS.
//  3. UseAnonymous=true — public bucket, no signed requests. Test-only.
type S3Config struct {
	// Endpoint is the S3 service host (e.g. "s3.amazonaws.com",
	// "minio.minio.svc:9000"). Required.
	Endpoint string
	// Bucket the artifacts live in. The bucket must already exist;
	// S3Backend does not create it.
	Bucket string
	// Prefix is prepended to every object key. A trailing "/" is
	// inserted automatically.
	Prefix string
	// Region the bucket lives in. Optional for S3-compatible servers
	// (MinIO, Ceph RGW); required for AWS multi-region setups.
	Region string

	// UseSSL controls whether minio-go talks HTTPS. Defaults to true.
	UseSSL bool

	// AccessKeyID / SecretAccessKey / SessionToken are static creds.
	// Empty triggers the IAM/IRSA discovery chain.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// UseAnonymous skips request signing entirely. Only useful against
	// a bucket with anonymous read+write ACL (testing).
	UseAnonymous bool

	// ReadTimeout caps a single GetObject stream. Zero uses the
	// surrounding http.Server's write timeout.
	ReadTimeout time.Duration
}

// s3Credentials selects the minio credential provider from the config:
// anonymous → nil (unsigned), an explicit access key → static V4, otherwise the
// discovery chain (env → shared file → EC2/EKS IAM). AWS_* environment variables
// and IRSA web-identity tokens are honored through the env entry, so a Secret of
// AWS_* keys mounted via envFrom authenticates here.
func s3Credentials(cfg S3Config) *credentials.Credentials {
	switch {
	case cfg.UseAnonymous:
		return nil
	case cfg.AccessKeyID != "":
		return credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)
	default:
		// Discovery chain: env vars first, then EC2/EKS metadata.
		// IRSA web-identity tokens are honored via the env chain.
		return credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{Client: &http.Client{Timeout: 5 * time.Second}},
		})
	}
}

// NewS3 constructs an S3Backend against the configured endpoint. The
// bucket is NOT created — callers (or the cluster operator) provision it.
func NewS3(cfg S3Config) (*S3Backend, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("storage/s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("storage/s3: bucket is required")
	}
	opts := &minio.Options{
		Secure: cfg.UseSSL,
		Region: cfg.Region,
		Creds:  s3Credentials(cfg),
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("storage/s3: build client: %w", err)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	return &S3Backend{
		client:      client,
		bucket:      cfg.Bucket,
		prefix:      prefix,
		readTimeout: cfg.ReadTimeout,
	}, nil
}

// Close is a no-op for S3 — minio-go's client holds no resources beyond
// an *http.Client.
func (b *S3Backend) Close() error { return nil }

// Sweep is a no-op on S3: PutObject is atomic, so no `.tmp` residue
// exists to clean up.
func (b *S3Backend) Sweep(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

// objectKey resolves the canonical key for an artifact. Mirrors the
// filesystem layout: <prefix>/<namespace>/<name>/<revision>.tar.gz.
func (b *S3Backend) objectKey(namespace, name, revision string) string {
	key := path.Join(namespace, name, revision+".tar.gz")
	if b.prefix == "" {
		return key
	}
	return b.prefix + "/" + key
}

// objectDir returns the key prefix (no trailing slash) under which every
// revision of a given (namespace, name) lives.
func (b *S3Backend) objectDir(namespace, name string) string {
	dir := path.Join(namespace, name)
	if b.prefix == "" {
		return dir
	}
	return b.prefix + "/" + dir
}

// Open returns a reader over the stored <revision>.tar.gz object. The caller
// closes it. A missing object (never published or pruned) returns
// ErrRevisionNotFound, detected via an upfront Stat so callers learn of it here
// rather than only on first Read.
func (b *S3Backend) Open(ctx context.Context, namespace, name, revision string) (io.ReadCloser, error) {
	if namespace == "" || name == "" || revision == "" {
		return nil, fmt.Errorf("storage/s3: namespace/name/revision required, got (%q,%q,%q)", namespace, name, revision)
	}
	// Every other key-constructing method (Put/Prune/Delete and the filesystem
	// backend's Open) validates its components; Open must agree on what a legal
	// identifier is so a '/'-bearing revision can't address a different object
	// within the bucket prefix.
	if err := validNoTraversal(namespace, name, revision); err != nil {
		return nil, err
	}
	key := b.objectKey(namespace, name, revision)
	obj, err := b.client.GetObject(ctx, b.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage/s3: get %q: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fmt.Errorf("%w: %s/%s@%s", ErrRevisionNotFound, namespace, name, revision)
		}
		return nil, fmt.Errorf("storage/s3: stat %q: %w", key, err)
	}
	return obj, nil
}

// Put streams the deterministic tar.gz directly into PutObject via an
// io.Pipe — no full-tarball buffer in memory. Identical inputs produce
// identical bytes (sorted entries + zero ModTime + PAX format), so the
// digest stays reproducible across backends and S3's idempotency
// behaves cleanly across leader hand-overs.
//
// Streaming is essential for the multi-MB end of the
// `--max-artifact-bytes` range — a 100 MiB artifact would otherwise
// double the operator's resident set during a publish. minio-go's
// PutObject with objectSize=-1 uses multipart-upload internally; the
// per-part buffer (5 MiB minimum) is the only material allocation.
func (b *S3Backend) Put(ctx context.Context, namespace, name, revision string, entries []FileEntry) (Result, error) {
	if namespace == "" || name == "" || revision == "" {
		return Result{}, fmt.Errorf("storage/s3: namespace/name/revision required, got (%q,%q,%q)", namespace, name, revision)
	}
	if err := validNoTraversal(namespace, name, revision); err != nil {
		return Result{}, err
	}

	key := b.objectKey(namespace, name, revision)
	// Bound a stuck upload without capping a large-but-progressing one.
	// A fixed total-time ceiling truncates a legitimate big artifact on a
	// slow link, flapping the snippet into an endless re-upload-from-zero
	// loop. Instead, a stall monitor cancels only when no bytes flow for
	// uploadStallTimeout — counter.count() advances exactly as minio
	// drains the synchronous io.Pipe, so it's a faithful progress proxy.
	// The caller's ctx still aborts on SIGTERM / leader handoff.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	hasher := sha256.New()
	counter := &writeCounter{}

	stallDone := make(chan struct{})
	go func() {
		defer close(stallDone)
		stall := b.uploadStallTimeout
		if stall <= 0 {
			stall = defaultUploadStallTimeout
		}
		interval := stall / 4
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		watchUploadStall(ctx, counter.count, cancel, stall, interval, ticker.C)
	}()

	// Writer goroutine: build tar.gz into the pipe and tee through
	// the hasher + counter as we go. Any error closes the pipe with
	// the error, which surfaces to PutObject as a transport failure.
	writerDone := make(chan error, 1)
	go func() {
		err := streamTarGz(pw, hasher, counter, entries)
		// Close (with the error if any) so PutObject sees EOF / error.
		_ = pw.CloseWithError(err)
		writerDone <- err
	}()

	// PutObject reads the pipe until EOF or error. objectSize=-1 forces
	// multipart upload — fine for any sized payload.
	_, putErr := b.client.PutObject(ctx, b.bucket, key, pr, -1, minio.PutObjectOptions{
		ContentType: "application/x-gzip",
		PartSize:    16 * 1024 * 1024, // 16 MiB parts; over the AWS 5 MiB minimum
	})

	// Close the reader side before waiting on the writer goroutine.
	// If PutObject returned early without fully draining the pipe (a
	// transport error or ctx cancellation mid-upload), the writer is
	// blocked in pw.Write waiting for a reader that no longer exists.
	// Closing pr unblocks that Write with io.ErrClosedPipe so the
	// goroutine reaches its writerDone send. On the happy path the
	// pipe is already drained and this is harmless.
	_ = pr.CloseWithError(putErr)

	// Wait for the writer goroutine to finish so counter and hasher.Sum
	// are safe to read.
	writerErr := <-writerDone

	// Stop the stall monitor and wait for it to exit before returning, so
	// the goroutine can't outlive the Put.
	cancel()
	<-stallDone

	if writerErr != nil {
		return Result{}, fmt.Errorf("storage/s3: build tarball for %q: %w", key, writerErr)
	}
	if putErr != nil {
		return Result{}, fmt.Errorf("storage/s3: put %q: %w", key, putErr)
	}
	return Result{
		Path:         key,
		SizeBytes:    counter.count(),
		DigestSHA256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// watchUploadStall calls onStall once progress() has made no headway for
// `stall`. It polls tick (a ticker channel) and accumulates idle time in
// `interval` increments; reaching `stall` worth of consecutive no-progress
// ticks trips it. Returns when ctx is done. Extracted from Put so the stall
// logic is unit-testable with a hand-driven tick channel — distinguishing a
// genuinely stuck upload (abort) from a slow-but-progressing one (let it
// run) is exactly what a fixed total-time ceiling can't do.
func watchUploadStall(ctx context.Context, progress func() int64, onStall func(), stall, interval time.Duration, tick <-chan time.Time) {
	last := progress()
	var idle time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if cur := progress(); cur > last {
				last = cur
				idle = 0
				continue
			}
			if idle += interval; idle >= stall {
				onStall()
				return
			}
		}
	}
}

// streamTarGz writes the deterministic tar.gz to w, tee-ing every byte
// into hasher and counter. It writes straight into the caller's pipe so
// the S3 upload streams with no intermediate in-memory buffer of the
// whole tarball.
func streamTarGz(w io.Writer, hasher io.Writer, counter io.Writer, entries []FileEntry) error {
	multi := io.MultiWriter(w, hasher, counter)
	gz := gzip.NewWriter(multi)
	tw := tar.NewWriter(gz)

	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b FileEntry) int { return cmp.Compare(a.Path, b.Path) })

	now := time.Unix(0, 0).UTC()
	for _, e := range sorted {
		if err := writeTarEntry(tw, e, now); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("storage/s3: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("storage/s3: close gzip: %w", err)
	}
	return nil
}

// Prune lists every object under <ns>/<name>/ and removes those whose
// filename is not in keepRevisions, subject to the grace window. An
// empty keep-set is a no-op — Prune never wipes all revisions; use
// Delete for that. minio-go's ListObjects returns a channel; we collect
// into a slice first so a slow Remove doesn't block the listing.
//
// The keep-set + grace decision is delegated to selectPruneVictims, the
// same helper the filesystem backend uses, so the two implementations
// can't drift on when a superseded revision becomes eligible for deletion.
func (b *S3Backend) Prune(ctx context.Context, namespace, name string, keepRevisions []string, grace time.Duration) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("storage/s3: namespace/name required, got (%q,%q)", namespace, name)
	}
	if len(keepRevisions) == 0 {
		return nil
	}
	if err := validNoTraversal(namespace, name); err != nil {
		return err
	}
	keepSet, err := buildPruneKeepSet(keepRevisions)
	if err != nil {
		return err
	}
	dir := b.objectDir(namespace, name) + "/"

	// Bound the per-Prune call so a slow listing can't pin the
	// reconcile, but ride the caller's ctx so shutdown / leader-loss
	// propagates instead of waiting out the local deadline.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	var cands []pruneCandidate
	for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{
		Prefix:    dir,
		Recursive: false,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("storage/s3: list %q: %w", dir, obj.Err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".tar.gz") {
			continue
		}
		cands = append(cands, pruneCandidate{keepKey: base, removeKey: obj.Key, mtime: obj.LastModified})
	}

	for _, key := range selectPruneVictims(cands, keepSet, b.clock(), grace) {
		if err := b.client.RemoveObject(ctx, b.bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("storage/s3: remove %q: %w", key, err)
		}
	}
	return nil
}

// clock reports the wall-clock the grace-window comparison runs
// against. Nil now falls back to time.Now; tests override to drive
// expiry deterministically.
func (b *S3Backend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Delete removes every object under <ns>/<name>/. Used when a snippet's
// CR is being deleted.
func (b *S3Backend) Delete(ctx context.Context, namespace, name string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("storage/s3: namespace/name required, got (%q,%q)", namespace, name)
	}
	if err := validNoTraversal(namespace, name); err != nil {
		return err
	}

	dir := b.objectDir(namespace, name) + "/"
	// Bound the per-Delete call but honor caller cancellation —
	// matches Put/Prune semantics; see the rationale on Put.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	// A ListObjects stream error (auth blip, rate-limit, partial
	// listing) must fail the whole Delete. objectsCh closes on any list
	// error, which RemoveObjects would otherwise read as a clean EOF —
	// reporting success against a truncated delete set and leaving the
	// rest of the (ns, name) tarballs orphaned, which the caller's
	// finalizer drop then makes permanent. Capture the listing error
	// and return it after the remove loop drains, mirroring Prune.
	var listErr error
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{
			Prefix:    dir,
			Recursive: true,
		}) {
			if obj.Err != nil {
				listErr = obj.Err
				return
			}
			select {
			case <-ctx.Done():
				return
			case objectsCh <- obj:
			}
		}
	}()
	for err := range b.client.RemoveObjects(ctx, b.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if err.Err != nil {
			return fmt.Errorf("storage/s3: bulk remove: %w", err.Err)
		}
	}
	if listErr != nil {
		return fmt.Errorf("storage/s3: list %q during Delete: %w", dir, listErr)
	}
	return nil
}

// HTTPHandler returns a proxy handler that GETs the requested key from
// the bucket and streams it back. The path is taken verbatim (after
// stripping the leading slash) and joined with the configured prefix.
// Non-existent objects produce 404; other errors map to 500.
func (b *S3Backend) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Mirror *Store.HTTPHandler's allowlist: only `<rev>.tar.gz` requests are
		// served, so a caller reaching this port can't read arbitrary non-artifact
		// object keys from the (prefix-scoped) bucket. The allowlist matches the
		// only filename shape Backend.Put ever produces.
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			http.NotFound(w, r)
			return
		}
		key, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || key == "" {
			http.NotFound(w, r)
			return
		}
		if b.prefix != "" {
			key = b.prefix + "/" + key
		}
		ctx := r.Context()
		if b.readTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, b.readTimeout)
			defer cancel()
		}
		obj, err := b.client.GetObject(ctx, b.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			s3WriteError(w, err)
			return
		}
		defer obj.Close()
		info, err := obj.Stat()
		if err != nil {
			s3WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.Copy(w, obj); err != nil {
			// Headers are already on the wire — we can't switch to 500, so
			// the truncated body is the only signal the client gets. Log at
			// Warn with the key so a recurring mid-stream failure (backend
			// flapping, client disconnects) is diagnosable instead of silent.
			slog.Default().Warn("storage/s3: error streaming artifact body",
				slog.String("key", key), slog.Any("error", err))
			return
		}
	})
}

// s3WriteError translates a minio error into the matching HTTP status.
// NoSuchKey / NoSuchBucket map to 404; anything else is 500.
//
// Response bodies are deliberately generic — the minio error
// `Message` / `Code` fields can carry the bucket name, endpoint
// hostname, AWS request-id, and other backend identifiers that don't
// belong in an unauthenticated response body. The underlying error
// is logged at Warn so operators can still diagnose; the wire
// response stays opaque.
func s3WriteError(w http.ResponseWriter, err error) {
	errResp := minio.ToErrorResponse(err)
	switch errResp.Code {
	case "NoSuchKey", "NoSuchBucket":
		http.Error(w, "not found", http.StatusNotFound)
	case "":
		// Non-S3 error (DNS, dial, TLS). Surface as 502 so operators
		// see "upstream is sick" rather than "object missing".
		slog.Default().Warn("storage/s3: non-S3 error serving artifact",
			slog.Any("error", err))
		http.Error(w, "bad gateway", http.StatusBadGateway)
	default:
		slog.Default().Warn("storage/s3: backend error serving artifact",
			slog.String("code", errResp.Code),
			slog.Any("error", err))
		http.Error(w, "storage error", http.StatusInternalServerError)
	}
}

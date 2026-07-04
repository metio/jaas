/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package sources fetches Flux source artifacts referenced by JaaS CRs.
//
// A JsonnetSnippet (or JsonnetLibrary) may carry a spec.sourceRef pointing
// at a Flux source CR — GitRepository, OCIRepository, Bucket, or
// ExternalArtifact. Each of those exposes status.artifact.{url,digest},
// where url is a tar.gz served by Flux's source-controller and digest is
// the canonical SHA256 of the bytes. The reconciler asks Fetcher to
// resolve the SourceRef into an in-memory map of file path → content; the
// rest of the eval pipeline then treats those files identically to an
// inline spec.files map.
//
// The Fetcher is intentionally small: it accepts a controller-runtime
// client (production wires in the tenant impersonation client) and an
// http.Client (tests inject httptest servers). It does NOT know about the
// snippet's RBAC model — every source CR Get and tarball GET runs with
// whatever credentials the caller's client carries.
package sources

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/urlguard"
)

// Default Flux source group/version. Callers may override APIVersion on the
// SourceRef; an empty value resolves to this.
const defaultSourceAPIVersion = "source.toolkit.fluxcd.io/v1"

// Default cap on the compressed download body. Flux source artifacts are
// usually well under 10 MiB; 64 MiB is generous for the on-the-wire tarball.
const defaultMaxArchiveBytes int64 = 64 << 20

// Default cap on the decompressed extracted result held in memory — the
// sum of every extracted entry body. Independent of the compressed
// download cap because gzip can hide the real expanded size behind a
// small compressed body.
const defaultMaxExtractedBytes int64 = 64 << 20

// Default per-tar-entry body cap. A single 16 MiB entry is plenty for
// a Jsonnet library .libsonnet or a small JSON resource; anything
// bigger is almost certainly an attack or accidental commit.
const defaultMaxPerEntryBytes int64 = 16 << 20

// Default decompressed-stream cap. Sits above the aggregate cap to
// allow tar overhead (headers, padding) without false-positives, but
// caps total gzip output so a small compressed bomb can't expand
// indefinitely.
const defaultMaxDecompressedBytes int64 = 512 << 20

// Default cap on concurrent in-flight downloadToTemp calls. Each
// open download holds a tempfile bounded by MaxArchiveBytes (default
// 64 MiB), so the peak ephemeral-storage cost of in-flight downloads
// is bounded by this cap × MaxArchiveBytes. 4 × 64 MiB = 256 MiB
// gives headroom on a small node while letting modest parallel
// reconciles proceed without queueing.
const defaultMaxConcurrentDownloads = 4

// ErrSourceNotReady reports that the source CR exists but its
// status.conditions[Ready] is not True yet. Callers requeue rather than
// failing permanently.
var ErrSourceNotReady = errors.New("source not ready")

// ErrArtifactMissing reports that the source CR's status.artifact is missing
// or malformed. Treated as transient — Flux populates it as soon as the
// source is ready.
var ErrArtifactMissing = errors.New("source has no status.artifact yet")

// ErrDigestMismatch reports that the tarball's SHA256 didn't match
// status.artifact.digest. Treated as a transient failure that may indicate a
// network or storage glitch; the next reconcile re-fetches and re-verifies.
var ErrDigestMismatch = errors.New("tarball digest does not match source artifact digest")

// Tarball-shape sentinels. classifyFetchError routes these as
// non-transient (the upstream must shrink / sanitize / re-publish);
// without the sentinel an `errors.Is` check against the plain
// `fmt.Errorf("tarball exceeds %d bytes", ...)` wouldn't match and the
// classifier would fall through to the transient default branch,
// pinning the snippet in a retry loop forever.

// ErrArtifactBodyTooLarge reports that the compressed HTTP body of the source
// artifact exceeded the download byte cap before the tar stage. The cap comes
// from Fetcher.MaxArchiveBytes.
var ErrArtifactBodyTooLarge = errors.New("artifact body exceeded aggregate cap")

// ErrTarballTooLarge reports that the gzip-decompressed extracted result —
// the sum of every extracted entry body held in memory — exceeded
// Fetcher.MaxExtractedBytes during extraction. Distinct from
// ErrArtifactBodyTooLarge because gzip can hide the real expanded size behind
// a small compressed body (gzip-bomb shape), so the two caps are independent.
var ErrTarballTooLarge = errors.New("tarball aggregate size exceeded cap")

// ErrTarEntryTooLarge reports that a single tar entry's body (or its claimed
// header size) exceeded Fetcher.MaxPerEntryBytes. Catches malicious uploads
// that try to slip a single huge file past the aggregate cap.
var ErrTarEntryTooLarge = errors.New("tar entry exceeded per-entry cap")

// ErrDecompressedTooLarge reports that the decompressed gzip stream exceeded
// Fetcher.MaxDecompressedBytes — the gzip-bomb defence layer that catches a
// small compressed body claiming an absurd decompressed size. cappedReader
// wraps the gzip reader to enforce this.
var ErrDecompressedTooLarge = errors.New("decompressed gzip stream exceeded cap")

// ErrArtifactNotFound reports that the artifact URL returned a permanent
// 4xx (other than 408 Request Timeout / 429 Too Many Requests). A 404 /
// 403 on the artifact body won't heal by retry — the upstream must
// re-publish or fix its serving — so classifyFetchError routes this as
// non-transient. A bare "HTTP <code>" without this sentinel would fall
// into the transient default and retry the permanent failure forever.
var ErrArtifactNotFound = errors.New("artifact URL returned a permanent HTTP error")

// Result is the materialized source content: a flat file path → content
// map plus the revision string the apiserver reported on the source CR.
type Result struct {
	Files    map[string]string
	Revision string
}

// Fetcher resolves SourceRefs into Result via API + HTTP calls. The zero
// value is unusable; construct via New.
type Fetcher struct {
	// HTTPClient downloads tarballs. Defaults to http.DefaultClient with a
	// 30s timeout when constructed via New.
	HTTPClient *http.Client

	// MaxArchiveBytes bounds ONLY the compressed download body. Beyond
	// this, Fetch returns ErrArtifactBodyTooLarge rather than stream an
	// unbounded tarball off a malicious or runaway source. Zero falls
	// back to defaultMaxArchiveBytes.
	MaxArchiveBytes int64

	// MaxExtractedBytes bounds the decompressed extracted result held in
	// memory — the aggregate sum of every extracted entry body. Beyond
	// this, Fetch returns ErrTarballTooLarge. Independent of
	// MaxArchiveBytes because gzip can hide the expanded size behind a
	// small compressed body. Zero falls back to defaultMaxExtractedBytes.
	MaxExtractedBytes int64

	// MaxPerEntryBytes caps an individual tar entry's body size. A
	// lying header (small Size, gigabyte body) is also caught — the
	// body is read through io.LimitReader bounded to MaxPerEntryBytes+1.
	// Zero falls back to defaultMaxPerEntryBytes.
	MaxPerEntryBytes int64

	// MaxDecompressedBytes caps the gzip stream's decompressed output.
	// Defends against gzip-bomb attacks where a few KiB of compressed
	// bytes expand to GB. Allows some tar overhead above
	// MaxArchiveBytes (headers, padding) so a legitimately-full payload
	// doesn't false-positive. Zero falls back to
	// defaultMaxDecompressedBytes.
	MaxDecompressedBytes int64

	// URLValidator runs against status.artifact.url before any HTTP
	// dial. nil installs the production validator (urlguard.ValidateHTTPURL)
	// — rejects loopback, link-local, multicast, unspecified, and the
	// "localhost" alias, plus inet_aton-form alt-IPv4 (e.g.
	// http://2130706433/) that bypasses net.ParseIP. Tests using
	// httptest's 127.0.0.1 listeners install urlguard.PermissiveHTTPURL
	// (scheme check only) so the dial reaches them.
	URLValidator func(string) error

	// IPValidator runs against every IP the dialer resolves a host to,
	// at connection time, on the initial dial AND every redirect hop.
	// nil installs the production check (urlguard.ForbiddenIP): a host
	// that passes the string-level URLValidator but resolves (or rebinds)
	// to loopback / link-local / metadata is refused before the socket
	// opens, and the connection is pinned to the validated IP so there's
	// no second resolution to rebind. Tests that dial httptest's 127.0.0.1
	// listeners install a permissive validator alongside URLValidator.
	IPValidator func(net.IP) error

	// MaxConcurrentDownloads bounds parallel downloadToTemp calls
	// across the whole Fetcher. Without a bound, many large fetches
	// in flight at once (one per reconciling snippet when
	// MaxConcurrentReconciles is raised, or a fan-out of
	// source-CR-change events) can fill the node's tempdir before
	// the per-fetch byte cap catches the request. Zero falls back
	// to defaultMaxConcurrentDownloads.
	MaxConcurrentDownloads int

	// sem is the semaphore Fetcher.acquire uses to enforce
	// MaxConcurrentDownloads. Initialised lazily on first acquire.
	semOnce sync.Once
	sem     chan struct{}
}

// New returns a Fetcher with production defaults.
func New() *Fetcher {
	f := &Fetcher{
		MaxArchiveBytes:      defaultMaxArchiveBytes,
		MaxExtractedBytes:    defaultMaxExtractedBytes,
		MaxPerEntryBytes:     defaultMaxPerEntryBytes,
		MaxDecompressedBytes: defaultMaxDecompressedBytes,
	}
	// CheckRedirect runs URLValidator on every hop, not just the
	// initial dial. Without it, a source CR whose status.artifact.url
	// passes urlguard could 302 to a denied host (cloud metadata,
	// in-cluster service) and Go's default redirect-follower would
	// dial it. Bound at 10 hops to match Go's default.
	//
	// The dialer's Control hook adds the connection-time half: the standard
	// library resolves the host and, immediately before the kernel connects,
	// Control validates the concrete IP it chose through IPValidator. A host
	// that passes the string check but resolves (or rebinds) to a forbidden
	// address is refused before the socket opens — there is no second
	// resolution between the check and the connect. Leaving resolution to the
	// standard dialer means real cluster FQDNs (including Flux's trailing-dot
	// advertised addresses) and IPv6 resolve exactly as they do for any other
	// client; TLS still verifies against the original hostname.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Clear the inherited ProxyFromEnvironment. A configured HTTP(S)_PROXY
	// would make the standard library dial the proxy's address, so the
	// Control hook below would validate the proxy IP instead of the resolved
	// artifact host — silently defeating the dial-time rebinding defence. The
	// operator fetches in-cluster source-controller artifacts directly, so no
	// proxy is needed.
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   f.controlConn,
	}).DialContext
	f.HTTPClient = &http.Client{
		// A total-request budget generous enough to stream the largest
		// artifact the fetcher will accept: the 64 MiB decompressed cap
		// (defaultMaxArchiveBytes) needs more than a 30s wall clock over a
		// slow link. Matches the stageset fetcher's 5m budget for the same
		// ExternalArtifact contract; the byte caps, not this timeout, bound
		// what a malicious source can stream.
		Timeout:       5 * time.Minute,
		CheckRedirect: f.checkRedirect,
		Transport:     transport,
	}
	return f
}

func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return f.validateURL()(req.URL.String())
}

// ipValidator returns the configured IP validator, or the production
// urlguard-backed check when none is set.
func (f *Fetcher) ipValidator() func(net.IP) error {
	if f.IPValidator != nil {
		return f.IPValidator
	}
	return defaultIPValidator
}

func defaultIPValidator(ip net.IP) error {
	if urlguard.ForbiddenIP(ip) {
		return fmt.Errorf("%w: resolved to %s", urlguard.ErrForbiddenHost, ip)
	}
	return nil
}

// controlConn runs as the dialer's Control hook: address is the concrete
// IP:port the kernel is about to connect to (post-resolution, per attempt),
// so rejecting a forbidden IP here closes the DNS-rebinding window — there is
// no second resolution between this check and the connect. TLS still verifies
// against the original hostname; the transport derives ServerName from the URL,
// not from the dialed address.
func (f *Fetcher) controlConn(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("sources: dial address %q has no parseable IP", address)
	}
	return f.ipValidator()(ip)
}

// Fetch resolves ref against the supplied client (which should impersonate
// the tenant SA), downloads the published artifact, verifies its digest,
// and extracts .jsonnet/.libsonnet files into memory. ownerNs defaults an
// unset ref.Namespace.
func (f *Fetcher) Fetch(ctx context.Context, c client.Client, ref *jaasv1.SourceRef, ownerNs string) (*Result, error) {
	if ref == nil {
		return nil, errors.New("sources: nil SourceRef")
	}
	if c == nil {
		return nil, errors.New("sources: nil client")
	}

	apiVersion := ref.APIVersion
	if apiVersion == "" {
		apiVersion = defaultSourceAPIVersion
	}
	ns := ref.Namespace
	if ns == "" {
		ns = ownerNs
	}

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(ref.Kind)
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("source %s %s/%s not found: %w", ref.Kind, ns, ref.Name, err)
		}
		return nil, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ns, ref.Name, err)
	}

	if ok, why := readyState(obj); !ok {
		return nil, fmt.Errorf("%s %s/%s (%s): %w", ref.Kind, ns, ref.Name, why, ErrSourceNotReady)
	}

	artifact, err := readArtifact(obj)
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s: %w", ref.Kind, ns, ref.Name, err)
	}

	// SSRF guard on the URL we're about to dial. status.artifact.url
	// is usually operator-managed (source-controller writes it) but a
	// tenant with status-write RBAC on the source CR — or a poisoned
	// upstream — could redirect us at loopback / link-local / cloud
	// metadata. urlguard rejects literal denied IPs (plus inet_aton
	// alt-IPv4 forms that bypass net.ParseIP) before any HTTP I/O.
	if err := f.validateURL()(artifact.URL); err != nil {
		return nil, fmt.Errorf("%s %s/%s artifact URL %q: %w",
			ref.Kind, ns, ref.Name, artifact.URL, err)
	}

	tmp, gotDigest, err := f.downloadToTemp(ctx, artifact.URL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", artifact.URL, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if err := verifyExpectedDigest(gotDigest, artifact.Digest); err != nil {
		return nil, err
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind %s: %w", artifact.URL, err)
	}
	files, err := extractTarballWithLimits(tmp, ref.Path,
		f.maxExtractedBytes(), f.maxPerEntryBytes(), f.maxDecompressedBytes())
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", artifact.URL, err)
	}

	return &Result{Files: files, Revision: artifact.Revision}, nil
}

func (f *Fetcher) maxArchiveBytes() int64 {
	if f.MaxArchiveBytes > 0 {
		return f.MaxArchiveBytes
	}
	return defaultMaxArchiveBytes
}

func (f *Fetcher) maxExtractedBytes() int64 {
	if f.MaxExtractedBytes > 0 {
		return f.MaxExtractedBytes
	}
	return defaultMaxExtractedBytes
}

func (f *Fetcher) maxPerEntryBytes() int64 {
	if f.MaxPerEntryBytes > 0 {
		return f.MaxPerEntryBytes
	}
	return defaultMaxPerEntryBytes
}

func (f *Fetcher) maxDecompressedBytes() int64 {
	if f.MaxDecompressedBytes > 0 {
		return f.MaxDecompressedBytes
	}
	return defaultMaxDecompressedBytes
}

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return http.DefaultClient
}

func (f *Fetcher) validateURL() func(string) error {
	if f.URLValidator != nil {
		return f.URLValidator
	}
	return urlguard.ValidateHTTPURL
}

// acquire takes one slot from the concurrency semaphore, blocking
// when MaxConcurrentDownloads are already in flight. Honors ctx
// cancellation so a reconcile timeout aborts cleanly instead of
// waiting forever for an in-flight download to drain.
func (f *Fetcher) acquire(ctx context.Context) error {
	f.semOnce.Do(func() {
		n := f.MaxConcurrentDownloads
		if n <= 0 {
			n = defaultMaxConcurrentDownloads
		}
		f.sem = make(chan struct{}, n)
	})
	select {
	case f.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fetcher) release() {
	<-f.sem
}

// downloadToTemp performs an HTTP GET against url and streams the
// response body into a tempfile while computing the sha256 incrementally.
// Returns the open tempfile (rewound and ready for re-read), the hex
// digest of the bytes written, and any error.
//
// Streaming via tempfile rather than io.ReadAll keeps resident memory
// bounded by the io.Copy buffer (~32 KiB) regardless of artifact size,
// which matters when many reconciles run in parallel against large
// upstream sources. The verify-before-trust ordering is preserved:
// extractTarball doesn't see a single byte until the caller has
// verified the computed digest against status.artifact.digest.
//
// The size cap is enforced via io.LimitReader; if more than
// maxArchiveBytes+1 bytes arrive, the function returns the same error
// shape as the old io.ReadAll path so callers see no behavioural
// change.
//
// The concurrency semaphore bounds the download (on-the-wire) phase to
// MaxConcurrentDownloads, so the bytes streaming through this function at
// once can't exceed MaxConcurrentDownloads × MaxArchiveBytes. Note the
// slot is released when this function returns: the caller still holds the
// returned tempfile through extraction, so peak on-disk tempfile count is
// bounded by concurrent reconciles (controller-runtime's
// MaxConcurrentReconciles), not by this semaphore.
//
// Caller owns the tempfile lifecycle — defer Close + Remove on the
// returned *os.File.
func (f *Fetcher) downloadToTemp(ctx context.Context, url string) (*os.File, string, error) {
	if err := f.acquire(ctx); err != nil {
		return nil, "", err
	}
	defer f.release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A permanent 4xx (other than 408/429, which signal "retry later")
		// won't heal by retry — wrap it in ErrArtifactNotFound so the
		// classifier treats it as steady-state. 5xx and the two retryable
		// 4xx codes stay unwrapped and fall through to the transient default.
		if isPermanentHTTPStatus(resp.StatusCode) {
			return nil, "", fmt.Errorf("%w: HTTP %d", ErrArtifactNotFound, resp.StatusCode)
		}
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "jaas-source-*.tar.gz")
	if err != nil {
		return nil, "", fmt.Errorf("create tempfile: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	hasher := sha256.New()
	maxBytes := f.maxArchiveBytes()
	// LimitReader caps at maxBytes+1: io.Copy will read maxBytes+1 bytes
	// when the upstream is larger, and we detect that overflow via counted.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	counted, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		cleanup()
		return nil, "", err
	}
	if counted > maxBytes {
		cleanup()
		return nil, "", fmt.Errorf("%w: %d bytes", ErrArtifactBodyTooLarge, maxBytes)
	}

	return tmp, hex.EncodeToString(hasher.Sum(nil)), nil
}

// isPermanentHTTPStatus reports whether an artifact-fetch HTTP status is
// a permanent client error that retry can't fix. Every 4xx qualifies
// EXCEPT 408 (Request Timeout) and 429 (Too Many Requests), which both
// invite a later retry. 5xx is treated as transient (server-side, may
// recover) and handled by the caller's default branch.
func isPermanentHTTPStatus(code int) bool {
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}
	return code >= 400 && code < 500
}

// artifact holds the bits we care about from status.artifact.
type artifact struct {
	URL      string
	Revision string
	Digest   string
}

// readArtifact pulls status.artifact.{url,revision,digest} out of obj.
// Either ErrArtifactMissing or a "missing field" error is returned when
// the apiserver hasn't populated the artifact yet. status.artifact.digest
// is type-checked via readArtifactDigest — a buggy custom Flux source
// CRD shipping it as a non-string lands here as ErrDigestInvalid rather
// than silently slipping past verification.
func readArtifact(obj *unstructured.Unstructured) (artifact, error) {
	m, found, err := unstructured.NestedMap(obj.Object, "status", "artifact")
	if err != nil {
		return artifact{}, fmt.Errorf("read status.artifact: %w", err)
	}
	if !found {
		return artifact{}, ErrArtifactMissing
	}
	url, _ := m["url"].(string)
	if url == "" {
		return artifact{}, fmt.Errorf("status.artifact.url is empty: %w", ErrArtifactMissing)
	}
	rev, _ := m["revision"].(string)
	digest, err := readArtifactDigest(m)
	if err != nil {
		return artifact{}, err
	}
	return artifact{URL: url, Revision: rev, Digest: digest}, nil
}

// readyState inspects status.conditions[*] and reports whether the
// source has a Ready=True condition. The second return value is a
// short, human-readable reason that explains a false outcome — used
// in the ErrSourceNotReady wrapper so operators see *why* a source
// isn't ready rather than just "not ready". Defaults are
// conservative: an object with no conditions slice, a malformed
// slice, or no Ready entry surfaces a distinct reason for each case
// so a misconfigured third-party CRD doesn't appear identical to a
// source-controller that simply hasn't reconciled yet.
func readyState(obj *unstructured.Unstructured) (bool, string) {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return false, "status.conditions malformed"
	}
	if !found {
		return false, "status.conditions not yet populated"
	}
	if len(conds) == 0 {
		return false, "status.conditions empty"
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		if t != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		if s == "True" {
			return true, ""
		}
		reason, _ := m["reason"].(string)
		if reason == "" {
			return false, fmt.Sprintf("Ready=%s", s)
		}
		return false, fmt.Sprintf("Ready=%s/%s", s, reason)
	}
	return false, "no Ready condition"
}

// ErrDigestInvalid fires when status.artifact.digest is set but can't
// be parsed: missing colon, unknown algorithm, wrong hex length, or
// non-hex characters. Treated as non-transient — a buggy upstream
// shipping bad metadata won't fix itself on retry.
var ErrDigestInvalid = errors.New("artifact digest is malformed or uses an unsupported algorithm")

// supportedDigestAlgorithms enumerates accepted "<algo>:" prefixes.
// sha256 covers Flux's canonical contract; extending the list requires
// adding the matching hasher branch in verifyExpectedDigest.
var supportedDigestAlgorithms = []string{"sha256:"}

// verifyExpectedDigest compares a pre-computed hex sha256 against an
// expected "<algo>:<hex>" string. The streaming downloadToTemp hashes
// inline as bytes flow through, so the verifier doesn't re-read the
// body — this avoids a second pass over the (possibly large) tarball.
// An empty expected value is accepted (some Flux source kinds don't
// populate digest); enforcement falls back to "we trust the URL".
//
// The expected string is parsed through parseDigest, so a malformed
// declaration (wrong algorithm prefix, missing colon, non-hex value,
// wrong hex length) surfaces as ErrDigestInvalid — distinct from a
// digest *mismatch*, so operators can distinguish "buggy upstream
// metadata" from "bytes were tampered with."
func verifyExpectedDigest(gotHex, expected string) error {
	if expected == "" {
		return nil
	}
	algo, want, err := parseDigest(expected)
	if err != nil {
		return err
	}
	switch algo {
	case "sha256":
		if gotHex != want {
			return fmt.Errorf("%w: declared sha256:%s, got sha256:%s",
				ErrDigestMismatch, want, gotHex)
		}
		return nil
	default:
		// parseDigest filters unsupported algorithms; reaching here
		// would be a programming error.
		return fmt.Errorf("%w: %q (algorithm slipped past parseDigest)",
			ErrDigestInvalid, expected)
	}
}

// parseDigest splits "<algo>:<hex>" into its parts and validates the
// hex shape. Input is normalised to lower-case + whitespace-trimmed
// before parsing — sha256 hex is case-insensitive and the OCI spec
// recommends lower-case but doesn't require it. Error messages quote
// the normalised form so an operator reading the log can see exactly
// what was attempted.
func parseDigest(declared string) (algo, hexValue string, err error) {
	d := strings.ToLower(strings.TrimSpace(declared))
	if d == "" {
		return "", "", fmt.Errorf("%w: empty", ErrDigestInvalid)
	}
	colon := strings.IndexByte(d, ':')
	if colon <= 0 || colon == len(d)-1 {
		return "", "", fmt.Errorf("%w: %q: missing algorithm or value", ErrDigestInvalid, d)
	}
	algo = d[:colon]
	hexValue = d[colon+1:]
	prefix := algo + ":"
	if !slices.Contains(supportedDigestAlgorithms, prefix) {
		return "", "", fmt.Errorf("%w: %q: unsupported algorithm %q (supported: %v)",
			ErrDigestInvalid, d, algo, supportedDigestAlgorithms)
	}
	wantLen := expectedHexLength(algo)
	if wantLen > 0 && len(hexValue) != wantLen {
		return "", "", fmt.Errorf("%w: %q: hex length %d, want %d for %s",
			ErrDigestInvalid, d, len(hexValue), wantLen, algo)
	}
	if _, decodeErr := hex.DecodeString(hexValue); decodeErr != nil {
		return "", "", fmt.Errorf("%w: %q: not valid hex: %v",
			ErrDigestInvalid, d, decodeErr)
	}
	return algo, hexValue, nil
}

// expectedHexLength returns the canonical hex-string length for an
// algorithm. Zero for unknown algorithms; parseDigest already filtered
// those out by the time this is reached.
func expectedHexLength(algo string) int {
	switch algo {
	case "sha256":
		return 64
	default:
		return 0
	}
}

// readArtifactDigest reads status.artifact.digest off a source object.
// Returns ErrDigestInvalid when the field exists but isn't a string —
// a buggy custom CRD shipping it as int / object / boolean would land
// here. An absent or empty digest returns ("", nil): the caller may
// proceed without verification.
func readArtifactDigest(m map[string]any) (string, error) {
	v, ok := m["digest"]
	if !ok || v == nil {
		return "", nil
	}
	s, isString := v.(string)
	if !isString {
		return "", fmt.Errorf("%w: status.artifact.digest is %T, want string",
			ErrDigestInvalid, v)
	}
	return s, nil
}

// cappedReader returns ErrCappedExceeded once the underlying reader
// has produced more than `cap` bytes. Wraps the gzip stream so a few
// KiB of compressed bytes that expand to GB of decompressed bytes
// surface as a cap trip rather than OOM.
//
// Distinct from io.LimitReader: LimitReader returns io.EOF at the cap
// (silently truncating); cappedReader returns the configured `cause`
// error so a downstream tar.Reader propagates it as a Read failure.
type cappedReader struct {
	r         io.Reader
	remaining int64
	cause     error
	tripped   bool
	eofSeen   bool
}

func newCappedReader(r io.Reader, c int64, cause error) *cappedReader {
	return &cappedReader{r: r, remaining: c, cause: cause}
}

// Read returns cause once the underlying has produced more than the
// configured cap bytes. The (n>0, io.EOF) shape inside the budget is
// honoured — bytes count first, EOF wins. A byte produced AFTER the
// budget is exhausted always trips, even when packed with EOF in the
// same call.
func (cr *cappedReader) Read(p []byte) (int, error) {
	if cr.tripped {
		return 0, cr.cause
	}
	if cr.eofSeen {
		return 0, io.EOF
	}
	if cr.remaining <= 0 {
		// Cap exhausted; probe for one more byte to distinguish
		// "stream just ended cleanly" from "stream had more data."
		probe := make([]byte, 1)
		pn, perr := cr.r.Read(probe)
		if pn > 0 {
			cr.tripped = true
			return 0, cr.cause
		}
		if errors.Is(perr, io.EOF) {
			cr.eofSeen = true
			return 0, io.EOF
		}
		if perr != nil {
			return 0, perr
		}
		// (0, nil) at the boundary: io.Reader contract says retry
		// but in our usage that risks an infinite loop against a
		// stuck underlying. Trip defensively.
		cr.tripped = true
		return 0, cr.cause
	}
	if int64(len(p)) > cr.remaining {
		p = p[:cr.remaining]
	}
	n, err := cr.r.Read(p)
	cr.remaining -= int64(n)
	if errors.Is(err, io.EOF) {
		cr.eofSeen = true
	}
	return n, err
}

// extractTarballWithLimits reads a gzipped tar from r, returning a path →
// content map of every regular file. When pathPrefix is set, only files
// under that prefix are returned, with the prefix stripped from the keys.
// Symlinks, special files, and any entry whose path fails the safety
// inspector are rejected.
//
// Three byte caps are enforced (zero on any falls back to its package
// default):
//
//   - maxBytes: aggregate sum of extracted entry bodies (the decompressed
//     extracted-result budget, MaxExtractedBytes); defended against int64
//     overflow by checking hdr.Size against the remaining budget before
//     adding to total.
//   - perEntryBytes: an individual tar entry body. The header is
//     pre-checked AND the body read is bounded with io.LimitReader so a
//     lying header (small Size, gigabyte body) is also caught.
//   - decompressedBytes: the gzip output stream. Wraps with cappedReader
//     so a tiny compressed bomb expanding to GB of padding surfaces as
//     a Read error rather than OOM.
func extractTarballWithLimits(r io.Reader, pathPrefix string, maxBytes, perEntryBytes, decompressedBytes int64) (map[string]string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxExtractedBytes
	}
	if perEntryBytes <= 0 {
		perEntryBytes = defaultMaxPerEntryBytes
	}
	if decompressedBytes <= 0 {
		decompressedBytes = defaultMaxDecompressedBytes
	}
	// Cap the decompressed stream before the tar reader sees it: the
	// cappedReader wraps the gzip *output*, so the budget counts
	// inflated bytes — including the body of an entry the extract loop
	// skips, which tar.Next still decompresses. Wrapping the compressed
	// input instead would only bound the on-disk archive size (already
	// covered by MaxArchiveBytes) and let a tiny bomb expand unbounded.
	// Multistream stays ON (the default): a single tar gzipped across several
	// members (a producer that flushes mid-stream) decompresses transparently as
	// one stream, so every file is extracted. Two *separate* concatenated tars
	// are unreachable past archive/tar's first end-of-archive marker regardless
	// of gzip framing, and no producer emits that shape; the digest pins the
	// bytes, so there's nothing to gain by rejecting trailing members.
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	capped := newCappedReader(gz, decompressedBytes, fmt.Errorf("%w: %d bytes", ErrDecompressedTooLarge, decompressedBytes))
	tr := tar.NewReader(capped)

	files := map[string]string{}
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := normaliseEntry(hdr.Name, pathPrefix)
		if !ok {
			continue
		}
		// Reject duplicate normalised names. Keeping last-write-wins would both
		// double-count the entry against the aggregate cap (a crafted tarball
		// could pad toward ErrTarballTooLarge with repeated names while its kept
		// content stays small) and silently pick one of two contents — neither
		// is acceptable, and a real source-controller artifact never repeats a
		// path.
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("tar contains duplicate entry path %q", name)
		}
		if hdr.Size < 0 {
			return nil, fmt.Errorf("tar entry %q: negative size", hdr.Name)
		}
		// Header-claimed per-entry size precheck. Cheap rejection
		// before touching the body; the header may lie, but the
		// post-read length check below catches that too.
		if hdr.Size > perEntryBytes {
			return nil, fmt.Errorf("%w: tar entry %q header size %d > cap %d", ErrTarEntryTooLarge, hdr.Name, hdr.Size, perEntryBytes)
		}
		// Aggregate budget — check remaining BEFORE adding so a
		// header near math.MaxInt64 can't wrap the int64 accumulator.
		// maxBytes-total is non-negative (every prior iteration kept
		// total <= maxBytes) and hdr.Size is non-negative (above), so
		// the subtraction is safe.
		if hdr.Size > maxBytes-total {
			return nil, fmt.Errorf("%w: %d bytes (precheck)", ErrTarballTooLarge, maxBytes)
		}
		// Bound the body read to perEntry+1 so a lying header (small
		// Size, gigabyte body) trips at io.ReadAll time. The +1 lets
		// us detect "body produced one more byte than declared,"
		// which is the lying-header signal.
		body, err := io.ReadAll(io.LimitReader(tr, perEntryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read entry %q: %w", hdr.Name, err)
		}
		if int64(len(body)) > perEntryBytes {
			return nil, fmt.Errorf("%w: tar entry %q body > cap %d (header claimed %d)", ErrTarEntryTooLarge, hdr.Name, perEntryBytes, hdr.Size)
		}
		total += int64(len(body))
		if total > maxBytes {
			return nil, fmt.Errorf("%w: %d bytes (post-read)", ErrTarballTooLarge, maxBytes)
		}
		files[name] = string(body)
	}
	return files, nil
}

// normaliseEntry filters and rewrites a tar entry's path. Rejects
// absolute paths, paths containing "..", NUL or backslash characters,
// and any byte outside the allowlist `[A-Za-z0-9._/-]` — the latter
// stops cross-platform import confusion (\), log-line injection via
// newlines, and RTL-override scramblers from reaching the filesystem
// or the eval VM. Hidden-prefixed segments (.gitkeep, .gitignore) are
// silently dropped — they're routine in real tarballs but not
// importable as Jsonnet libraries.
//
// When pathPrefix is set, only entries under that prefix survive; the
// prefix is stripped from the returned name.
func normaliseEntry(rawName, pathPrefix string) (string, bool) {
	if rawName == "" || strings.HasPrefix(rawName, "/") {
		return "", false
	}
	if strings.ContainsRune(rawName, 0) || strings.ContainsRune(rawName, '\\') {
		return "", false
	}
	cleaned := path.Clean(rawName)
	for part := range strings.SplitSeq(cleaned, "/") {
		if part == ".." {
			return "", false
		}
		if strings.HasPrefix(part, ".") {
			// Hidden-prefixed segment — drop silently.
			return "", false
		}
	}
	for i := 0; i < len(cleaned); i++ {
		if !isSafePathByte(cleaned[i]) {
			return "", false
		}
	}
	if pathPrefix == "" {
		return cleaned, true
	}
	prefix := strings.TrimSuffix(pathPrefix, "/") + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", false
	}
	return strings.TrimPrefix(cleaned, prefix), true
}

// isSafePathByte reports whether b belongs to the materialised-path
// allowlist `[A-Za-z0-9._/-]`. Inlined byte test rather than a
// regex for allocation-free per-byte checks.
func isSafePathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '/':
		return true
	default:
		return false
	}
}

// Consumer-contract exports: the Publisher refuses to publish what these
// bounds would make every consumer refuse to fetch — a Ready=True artifact
// nobody can consume is worse than a failed publish.

// DefaultMaxPerEntryBytes is the per-entry extraction cap every fetcher
// applies (jaas's own chaining fetcher and stageset's artifact fetcher use
// the same value).
const DefaultMaxPerEntryBytes = defaultMaxPerEntryBytes

// DefaultMaxExtractedBytes is the aggregate extracted-content cap.
const DefaultMaxExtractedBytes = defaultMaxExtractedBytes

// SafeEntryName reports whether consumers will keep a tar entry of this name.
// The extractor silently DROPS entries that fail normalisation (unsafe bytes
// outside [A-Za-z0-9._/-], dot-prefixed segments, traversal, backslash), so a
// producer publishing such a name ships an artifact whose file quietly never
// arrives; producers validate here instead.
func SafeEntryName(name string) bool {
	_, ok := normaliseEntry(name, "")
	return ok
}

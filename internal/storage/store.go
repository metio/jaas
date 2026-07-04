/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package storage holds the on-disk artifact store the operator uses to
// materialize ExternalArtifact tarballs. The store is a thin wrapper around
// os.OpenRoot rooted at a single directory: every Put writes a tar.gz that
// the operator's HTTP server later serves to downstream Flux consumers.
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
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileEntry is one tarball member: a path relative to the archive root plus
// its content bytes.
type FileEntry struct {
	Path    string
	Content []byte
}

// Result describes what a Put materialized on disk.
type Result struct {
	// Path is the artifact's location relative to the store root; the HTTP
	// server resolves the URL from this.
	Path string

	// SizeBytes is the tar.gz size after compression.
	SizeBytes int64

	// DigestSHA256 is the hex SHA-256 of the compressed bytes. Flux
	// downstream uses this to confirm an artifact hasn't been tampered with
	// in transit.
	DigestSHA256 string
}

// Store materializes tarballs under a single root directory. Every Put
// produces a tar.gz at <root>/<namespace>/<name>/<revision>.tar.gz; old
// files for the same (namespace, name) are swept by Prune.
//
// Concurrent Put/Delete/Prune calls are serialized per (namespace, name)
// pair via a lazily-created per-key mutex held in keyLocks. Calls
// against distinct pairs run in parallel — a slow Put on snippet A no
// longer blocks Put on B. Sweep walks the whole tree without taking any
// key locks; it tolerates concurrent mutation through the Stat/cutoff
// filter (a .tmp file written mid-Put has a fresh modtime and is
// excluded) and Remove's tolerance of missing files.
//
// Store talks to disk via a fileSystem interface so tests can inject
// failures at the syscall boundary (writer Close errors, RemoveAll on a
// busy dir, etc.). Production wires the realFS adapter over *os.Root.
type Store struct {
	fs fileSystem

	// keyLocks holds a *sync.Mutex per "namespace/name" key, created
	// lazily on first reference. Entries accumulate for the process
	// lifetime — at thousands of unique snippets the overhead is
	// negligible (one zero-sized mutex per key) and a Forget pass on
	// snippet delete would race with re-creates carrying the same
	// (ns, name). The simpler "leak the mutex" choice is preferred.
	keyLocks sync.Map

	// now reports the wall-clock used by grace-window comparisons in
	// Prune (and tmp-residue cutoffs in Sweep). Nil falls back to
	// time.Now; tests override to drive grace expiry deterministically
	// against real on-disk mtimes.
	now func() time.Time
}

// lockFor returns the per-key mutex guarding (namespace, name)'s
// directory tree, lazily creating it. LoadOrStore guarantees that two
// goroutines racing to create the same key observe the same mutex.
func (s *Store) lockFor(namespace, name string) *sync.Mutex {
	key := namespace + "/" + name
	if v, ok := s.keyLocks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	fresh := &sync.Mutex{}
	actual, _ := s.keyLocks.LoadOrStore(key, fresh)
	return actual.(*sync.Mutex)
}

// New opens (creating if necessary) a Store at rootPath. The path must
// already point at an absolute directory the process can write to.
func New(rootPath string) (*Store, error) {
	if rootPath == "" {
		return nil, errors.New("storage: root path is empty")
	}
	if err := os.MkdirAll(rootPath, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create root %q: %w", rootPath, err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("storage: open root %q: %w", rootPath, err)
	}
	return &Store{fs: realFS{root: root}}, nil
}

// Close releases the underlying filesystem (file descriptors for the
// production *os.Root, or test fixtures for memory implementations). After
// Close, further calls behave as the filesystem decides.
func (s *Store) Close() error {
	return s.fs.Close()
}

// RootPath returns the directory the Store was opened against. Callers (the
// HTTP server, tests inspecting tarballs directly) need this to translate
// Result.Path into a filesystem location.
func (s *Store) RootPath() string {
	return s.fs.Name()
}

// Put writes a tar.gz containing entries to
// <namespace>/<name>/<revision>.tar.gz, replacing any existing file at that
// path atomically. The caller picks revision (typically a content hash);
// stable revisions let downstream consumers cache. ctx is accepted to
// satisfy the Backend interface — the filesystem write is fast enough
// that mid-write cancellation is not worth the complexity. The S3
// backend honors ctx because its upload is slow.
func (s *Store) Put(_ context.Context, namespace, name, revision string, entries []FileEntry) (Result, error) {
	if namespace == "" || name == "" || revision == "" {
		return Result{}, fmt.Errorf("storage: namespace/name/revision required, got (%q,%q,%q)", namespace, name, revision)
	}
	if err := validNoTraversal(namespace, name, revision); err != nil {
		return Result{}, err
	}

	lock := s.lockFor(namespace, name)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(namespace, name)
	if err := s.fs.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("storage: mkdir %q: %w", dir, err)
	}

	finalRel := filepath.Join(dir, RevisionFilename(revision))
	tmpRel := finalRel + ".tmp"

	// A revision is content-addressed: the same revision means byte-identical
	// tarball bytes (deterministic entry ordering + zero ModTime). If it is
	// already on disk, rewriting it would only re-stamp its mtime — which
	// keeps advancing the "newest revision" boundary Prune anchors its grace
	// window on, so a just-evicted revision's grace never elapses and it is
	// never collected. Skip the write and re-derive the Result from the
	// existing file, leaving its mtime untouched.
	if info, statErr := s.fs.Stat(finalRel); statErr == nil && !info.IsDir() {
		digest, err := s.digestOf(finalRel)
		if err != nil {
			return Result{}, err
		}
		// Reviving a revision that is OLDER than another already on disk is a
		// REVERT (the input rolled back to a still-retained revision). Leaving
		// its mtime untouched would leave the displaced newer revisions with no
		// strictly-newer sibling for selectPruneVictims to anchor their grace
		// window on — it would fall back to each displaced file's own mtime and
		// prune it immediately, opening the pin-then-fetch 404 race the grace
		// window exists to close. Bump the revived file to now so the displaced
		// revisions get a fresh supersession anchor. A steady republish (this
		// file is already the newest, no newer sibling) skips the bump, so the
		// grace boundary doesn't churn.
		if s.hasNewerSibling(dir, RevisionFilename(revision), info.ModTime()) {
			now := s.clock()
			_ = s.fs.Chtimes(finalRel, now, now)
		}
		return Result{Path: finalRel, SizeBytes: info.Size(), DigestSHA256: digest}, nil
	}

	digest, size, err := s.writeTarGz(tmpRel, entries)
	if err != nil {
		_ = s.fs.Remove(tmpRel)
		return Result{}, err
	}

	if err := s.fs.Rename(tmpRel, finalRel); err != nil {
		_ = s.fs.Remove(tmpRel)
		return Result{}, fmt.Errorf("storage: rename %q -> %q: %w", tmpRel, finalRel, err)
	}

	return Result{
		Path:         finalRel,
		SizeBytes:    size,
		DigestSHA256: digest,
	}, nil
}

// Open returns a reader over the stored <revision>.tar.gz for
// (namespace, name, revision). It reads through the same os.Root FS view as the
// HTTP handler, so the read path inherits the no-escape traversal guard: a
// planted symlink that escapes the root is refused, not followed. A revision
// that was never published or has been pruned returns ErrRevisionNotFound.
func (s *Store) Open(_ context.Context, namespace, name, revision string) (io.ReadCloser, error) {
	if namespace == "" || name == "" || revision == "" {
		return nil, fmt.Errorf("storage: namespace/name/revision required, got (%q,%q,%q)", namespace, name, revision)
	}
	if err := validNoTraversal(namespace, name, revision); err != nil {
		return nil, err
	}
	// fs.FS uses forward-slash, root-relative paths regardless of OS.
	rel := path.Join(namespace, name, RevisionFilename(revision))
	f, err := s.fs.FS().Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s/%s@%s", ErrRevisionNotFound, namespace, name, revision)
		}
		return nil, fmt.Errorf("storage: open %q: %w", rel, err)
	}
	return f, nil
}

// hasNewerSibling reports whether any OTHER revision file in dir has a strictly
// newer mtime than ref — i.e. reviving self would be reverting behind a
// still-present newer revision. Errors reading a sibling are ignored (best
// effort): a missed newer sibling only forgoes the revert bump, never a false
// positive.
func (s *Store) hasNewerSibling(dir, self string, refMtime time.Time) bool {
	names, err := s.fs.ReadDirNames(dir)
	if err != nil {
		return false
	}
	for _, n := range names {
		if n == self || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		info, statErr := s.fs.Stat(filepath.Join(dir, n))
		if statErr != nil || info.IsDir() {
			continue
		}
		if info.ModTime().After(refMtime) {
			return true
		}
	}
	return false
}

// digestOf returns the hex SHA-256 of an already-stored tarball, read through
// the same traversal-guarded FS view as the HTTP handler. Used when Put finds
// the revision already on disk and re-derives the Result without rewriting.
func (s *Store) digestOf(relPath string) (string, error) {
	f, err := s.fs.FS().Open(filepath.ToSlash(relPath))
	if err != nil {
		return "", fmt.Errorf("storage: open %q: %w", relPath, err)
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", fmt.Errorf("storage: hash %q: %w", relPath, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (s *Store) writeTarGz(relPath string, entries []FileEntry) (string, int64, error) {
	f, err := s.fs.Create(relPath, 0o644)
	if err != nil {
		return "", 0, fmt.Errorf("storage: create %q: %w", relPath, err)
	}
	hasher := sha256.New()
	counter := &writeCounter{}
	multi := io.MultiWriter(f, hasher, counter)

	gz := gzip.NewWriter(multi)
	tw := tar.NewWriter(gz)

	// Sort entries for reproducible output: the same input always yields
	// the same digest.
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b FileEntry) int { return cmp.Compare(a.Path, b.Path) })

	now := time.Unix(0, 0).UTC()
	for _, e := range sorted {
		if err := writeTarEntry(tw, e, now); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return "", 0, fmt.Errorf("storage: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return "", 0, fmt.Errorf("storage: close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("storage: close file: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), counter.count(), nil
}

func writeTarEntry(tw *tar.Writer, e FileEntry, ts time.Time) error {
	if err := validTarEntryPath(e.Path); err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:    e.Path,
		Mode:    0o644,
		Size:    int64(len(e.Content)),
		ModTime: ts,
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("storage: write header %q: %w", e.Path, err)
	}
	if _, err := tw.Write(e.Content); err != nil {
		return fmt.Errorf("storage: write body %q: %w", e.Path, err)
	}
	return nil
}

// Delete removes every artifact written under <namespace>/<name>/. Used when
// a snippet is being deleted. ctx is unused — same reasoning as Put.
func (s *Store) Delete(_ context.Context, namespace, name string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("storage: namespace/name required, got (%q,%q)", namespace, name)
	}
	if err := validNoTraversal(namespace, name); err != nil {
		return err
	}

	lock := s.lockFor(namespace, name)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(namespace, name)
	if err := s.fs.RemoveAll(dir); err != nil {
		return fmt.Errorf("storage: remove %q: %w", dir, err)
	}
	return nil
}

// Prune drops every tar.gz in <namespace>/<name>/ that does not match
// any of keepRevisions. Called after a successful Put to evict
// previous artifacts the caller no longer wants to retain. An empty
// keep-set is a no-op — Prune never wipes all revisions; use Delete
// for that. ctx is unused — same reasoning as Put.
//
// grace is the minimum time a non-keep revision is retained after it
// was superseded. Supersession time is derived from the earliest mtime
// strictly newer than the candidate's own mtime — the file that
// displaced it. The window survives operator restarts because the
// proxy is on-disk metadata, not memory. grace == 0 prunes eagerly.
func (s *Store) Prune(_ context.Context, namespace, name string, keepRevisions []string, grace time.Duration) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("storage: namespace/name required, got (%q,%q)", namespace, name)
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

	lock := s.lockFor(namespace, name)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(namespace, name)
	entries, err := s.fs.ReadDirNames(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("storage: list %q: %w", dir, err)
	}

	cands := make([]pruneCandidate, 0, len(entries))
	for _, n := range entries {
		if !looksLikeOurArtifactFilename(n) || strings.HasSuffix(n, ".tmp") {
			continue
		}
		info, statErr := s.fs.Stat(filepath.Join(dir, n))
		if statErr != nil {
			continue
		}
		cands = append(cands, pruneCandidate{keepKey: n, removeKey: filepath.Join(dir, n), mtime: info.ModTime()})
	}

	for _, key := range selectPruneVictims(cands, keepSet, s.clock(), grace) {
		if err := s.fs.Remove(key); err != nil {
			// Propagate the first Remove failure rather than swallowing it,
			// matching the S3 backend so a failing Prune surfaces on both
			// stores. A leftover revision that can't be reclaimed means the
			// keep-set isn't being honored — the caller should know.
			return fmt.Errorf("storage: prune remove %q: %w", key, err)
		}
	}
	return nil
}

// pruneCandidate is one artifact a Prune pass considers. keepKey is the
// <rev>.tar.gz filename matched against the keep-set; removeKey is the
// identity handed back for deletion — a filesystem path for the local
// store, an object key for S3; mtime drives the grace-window decision.
type pruneCandidate struct {
	keepKey   string
	removeKey string
	mtime     time.Time
}

// buildPruneKeepSet validates each keep revision against path traversal and
// returns the set of <rev>.tar.gz filenames a Prune pass must retain. Shared
// by both backends so the keep-set shape can't drift.
func buildPruneKeepSet(keepRevisions []string) (map[string]struct{}, error) {
	keepSet := make(map[string]struct{}, len(keepRevisions))
	for _, rev := range keepRevisions {
		if err := validNoTraversal(rev); err != nil {
			return nil, err
		}
		keepSet[RevisionFilename(rev)] = struct{}{}
	}
	return keepSet, nil
}

// selectPruneVictims returns the removeKeys of every candidate that is NOT
// in keepSet and — when grace > 0 — has been superseded for at least grace.
// Supersession time is the earliest strictly-newer candidate mtime, or the
// candidate's own mtime when none is newer (a successor shares its mtime at
// coarse filesystem/S3 granularity, or it's the newest-but-not-kept) so an
// orphan is never pinned forever rather than reclaimed once grace elapses.
// Shared by the filesystem and S3 backends so their prune semantics can't
// drift.
func selectPruneVictims(cands []pruneCandidate, keepSet map[string]struct{}, now time.Time, grace time.Duration) []string {
	var victims []string
	for _, c := range cands {
		if _, keep := keepSet[c.keepKey]; keep {
			continue
		}
		if grace > 0 {
			superseded := earliestNewerMtime(cands, c.mtime)
			if superseded.IsZero() {
				superseded = c.mtime
			}
			if now.Sub(superseded) < grace {
				continue
			}
		}
		victims = append(victims, c.removeKey)
	}
	return victims
}

// earliestNewerMtime returns the smallest candidate mtime strictly greater
// than ref, or the zero time when none is newer.
func earliestNewerMtime(cands []pruneCandidate, ref time.Time) time.Time {
	var out time.Time
	for _, c := range cands {
		if !c.mtime.After(ref) {
			continue
		}
		if out.IsZero() || c.mtime.Before(out) {
			out = c.mtime
		}
	}
	return out
}

// clock returns the wall-clock used by Prune's grace comparison and
// Sweep's tmp-residue cutoff. Tests override via SetNow to drive
// expiry deterministically without time.Sleep.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetNow overrides the wall-clock used by Prune's grace-window check
// and Sweep's tmp-residue cutoff. Tests drive grace expiry against
// real on-disk mtimes by stepping the clock instead of sleeping.
// Production callers leave the default of time.Now.
func (s *Store) SetNow(fn func() time.Time) { s.now = fn }

// looksLikeOurArtifactFilename reports whether name matches a filename shape
// this package writes — `<algo>-<hex>.tar.gz` (the production shape, where
// RevisionFilename mapped the digest's ':' to '-') or a bare `<hex>.tar.gz`,
// with an optional `.tmp` suffix. Used as a defence-in-depth filter in Prune /
// Sweep so non-operator files dropped in the same directory by another process
// (debug bundles, sidecar caches with foreign names) survive cleanup.
//
// The shape constraint is tighter than a length check — many tests use short
// hex stubs like "abc123" — but every conceivable foreign-file pattern (with
// unexpected characters, leading dots, or no .tar.gz extension) is still
// rejected.
func looksLikeOurArtifactFilename(name string) bool {
	var rev string
	switch {
	case strings.HasSuffix(name, ".tar.gz.tmp"):
		rev = strings.TrimSuffix(name, ".tar.gz.tmp")
	case strings.HasSuffix(name, ".tar.gz"):
		rev = strings.TrimSuffix(name, ".tar.gz")
	default:
		return false
	}
	if rev == "" {
		return false
	}
	// Production: "<algo>-<hex>" (algo lowercase-alnum, hex after the first '-').
	// Bare: "<hex>" (no '-'). A hex run never contains '-', so the Cut cleanly
	// separates the two shapes.
	if algo, hexPart, ok := strings.Cut(rev, "-"); ok {
		return isLowerAlnum(algo) && isHex(hexPart)
	}
	return isHex(rev)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isLowerAlnum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')) {
			return false
		}
	}
	return true
}

// Sweep walks the store and removes orphaned `<rev>.tar.gz.tmp` files
// older than maxTmpAge — the residue of Puts whose process died after
// the tmpfile landed but before the rename to the final name. Returns
// the number of files removed. ctx is unused — same reasoning as Put.
func (s *Store) Sweep(_ context.Context, maxTmpAge time.Duration) (int, error) {
	// Sweep acquires the same per-key mutex Put uses, scoped to one
	// (ns, name) at a time. The maxTmpAge cutoff alone is not
	// sufficient: a Put taking longer than maxTmpAge to write its
	// tempfile would otherwise see its in-flight .tmp reaped
	// mid-write, then the Rename would fail with "no such file."
	// The lock makes Sweep wait for any in-flight Put on the same
	// key to drain before stat-and-remove runs. Cross-key Puts
	// proceed concurrently with the sweep — the lock is per-key,
	// not global. Worst case: Sweep blocks for the duration of one
	// Put on the same key; Put work-per-key is bounded by
	// MaxArtifactBytes / disk throughput.
	cutoff := s.clock().Add(-maxTmpAge)
	removed := 0
	var errs []error
	namespaces, err := s.fs.ReadDirNames(".")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage: list root: %w", err)
	}
	for _, ns := range namespaces {
		nsInfo, err := s.fs.Stat(ns)
		if err != nil || !nsInfo.IsDir() {
			continue
		}
		names, err := s.fs.ReadDirNames(ns)
		if err != nil {
			continue
		}
		for _, name := range names {
			dir := filepath.Join(ns, name)
			dirInfo, err := s.fs.Stat(dir)
			if err != nil || !dirInfo.IsDir() {
				continue
			}
			n, err := s.sweepKey(ns, name, dir, cutoff)
			removed += n
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	// Aggregate per-key failures into the return so the caller's
	// jaas_storage_sweep_failures_total counter fires on the realistic
	// disk-full / permission-denied cases that strand .tmp residue. The
	// removed count still reflects the files Sweep did reap.
	return removed, errors.Join(errs...)
}

// sweepKey runs the per-(ns, name) sweep step under the per-key
// lock. Splitting this from Sweep keeps the lock scope obvious — the
// lock is acquired and released around the inner loop, not held
// across the outer ReadDirNames walks. Returns the number of files
// removed for this key plus an aggregate of any Remove failure — a
// .tmp residue that can't be reclaimed (disk full, permission denied,
// busy file) is surfaced so the caller's failure metric fires rather
// than the orphan silently accumulating.
func (s *Store) sweepKey(ns, name, dir string, cutoff time.Time) (int, error) {
	lock := s.lockFor(ns, name)
	lock.Lock()
	defer lock.Unlock()

	files, err := s.fs.ReadDirNames(dir)
	if err != nil {
		return 0, nil
	}
	removed := 0
	var errs []error
	for _, f := range files {
		if !strings.HasSuffix(f, ".tar.gz.tmp") || !looksLikeOurArtifactFilename(f) {
			continue
		}
		full := filepath.Join(dir, f)
		info, err := s.fs.Stat(full)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := s.fs.Remove(full); err != nil {
			slog.Default().Warn("storage: sweep could not remove orphaned .tmp file",
				slog.String("path", full), slog.Any("error", err))
			errs = append(errs, fmt.Errorf("storage: sweep remove %q: %w", full, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// validNoTraversal rejects path components that contain '..' or '/' so a
// malicious snippet name can't escape the store root.
func validNoTraversal(parts ...string) error {
	for _, p := range parts {
		if p == "" {
			return fmt.Errorf("storage: empty path component")
		}
		if strings.Contains(p, "/") || strings.Contains(p, `\`) {
			return fmt.Errorf("storage: path component %q contains a separator", p)
		}
		if p == "." || p == ".." {
			return fmt.Errorf("storage: path component %q is a traversal", p)
		}
	}
	return nil
}

// validTarEntryPath rejects only the shapes that escape the archive root —
// nested slashes are allowed so subdirectories materialize correctly under
// the tarball.
func validTarEntryPath(p string) error {
	if p == "" {
		return fmt.Errorf("storage: empty tar entry path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("storage: tar entry %q is absolute", p)
	}
	// A NUL truncates the name for C-based extractors, and a backslash is a
	// path separator on Windows — both let a member name that looks safe here
	// escape on extraction. The Fetcher rejects them on incoming artifacts;
	// inline spec.files keys reach the tar through this gate instead, so apply
	// the same guard or the two paths disagree on what a safe name is.
	if strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
		return fmt.Errorf("storage: tar entry %q contains a NUL or backslash", p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("storage: tar entry %q contains a traversal", p)
	}
	return nil
}

// writeCounter tees a byte count off an io.MultiWriter. n is atomic so a
// progress monitor (the S3 stall-timeout watcher) can read it concurrently
// with the writer goroutine without a data race.
type writeCounter struct{ n atomic.Int64 }

func (w *writeCounter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

func (w *writeCounter) count() int64 { return w.n.Load() }

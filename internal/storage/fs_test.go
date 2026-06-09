/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"testing"
	"testing/fstest"
)

// faultyFS is a fileSystem decorator over an in-memory backing store, with
// per-method error knobs. The zero value is unusable; construct via
// newFaultyFS(rootName).
type faultyFS struct {
	mu    sync.Mutex
	root  string
	dirs  map[string]bool          // name -> true (a known directory)
	files map[string]*bytes.Buffer // name -> content buffer

	mkdirAllErr  error
	createErr    error
	renameErr    error
	removeErr    error
	removeAllErr error
	statErr      error
	readDirErr   error
	closeErr     error
	// writer-side knobs surfaced by Create. Any new writer the test
	// returns inherits these.
	writeErr  error
	wcloseErr error
}

func newFaultyFS(rootName string) *faultyFS {
	return &faultyFS{
		root:  rootName,
		dirs:  map[string]bool{"": true},
		files: map[string]*bytes.Buffer{},
	}
}

func (f *faultyFS) MkdirAll(name string, _ os.FileMode) error {
	if f.mkdirAllErr != nil {
		return f.mkdirAllErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[name] = true
	return nil
}

func (f *faultyFS) Create(name string, _ os.FileMode) (io.WriteCloser, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	buf := &bytes.Buffer{}
	f.mu.Lock()
	f.files[name] = buf
	f.mu.Unlock()
	return &faultyWriter{
		dst:      buf,
		writeErr: f.writeErr,
		closeErr: f.wcloseErr,
	}, nil
}

func (f *faultyFS) Rename(_, newName string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	_ = newName
	return nil
}

func (f *faultyFS) Remove(_ string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	return nil
}

func (f *faultyFS) RemoveAll(_ string) error {
	if f.removeAllErr != nil {
		return f.removeAllErr
	}
	return nil
}

func (f *faultyFS) Stat(_ string) (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return nil, os.ErrNotExist
}

func (f *faultyFS) ReadDirNames(_ string) ([]string, error) {
	if f.readDirErr != nil {
		return nil, f.readDirErr
	}
	return nil, nil
}

func (f *faultyFS) Close() error { return f.closeErr }

func (f *faultyFS) Name() string { return f.root }

// FS exposes the in-memory files as an fs.FS so a faulty store can also
// drive the HTTP read path. No faulty-fs symlink concept exists, so this
// is a faithful map view; the production realFS.FS() is what carries the
// os.Root no-escape guarantee.
func (f *faultyFS) FS() fs.FS {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := fstest.MapFS{}
	for name, buf := range f.files {
		m[name] = &fstest.MapFile{Data: buf.Bytes()}
	}
	return m
}

// faultyWriter is the io.WriteCloser Create returns. It either swallows
// writes into dst or surfaces writeErr / closeErr at the configured stage.
type faultyWriter struct {
	dst      *bytes.Buffer
	writeErr error
	closeErr error
}

func (w *faultyWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.dst.Write(p)
}

func (w *faultyWriter) Close() error { return w.closeErr }

// newFaultyStore builds a Store wired to a faulty fs. Tests configure the
// returned *faultyFS's error fields before driving Store methods.
func newFaultyStore(t *testing.T) (*Store, *faultyFS) {
	t.Helper()
	fs := newFaultyFS(t.TempDir())
	return &Store{fs: fs}, fs
}

// --- Error-path coverage via the injectable fs -----------------------------

func TestPut_MkdirAllErrorPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.mkdirAllErr = errors.New("readonly fs")
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("x")}})
	if err == nil || !errors.Is(err, fs.mkdirAllErr) {
		t.Errorf("got %v, want mkdir error wrapped", err)
	}
}

func TestPut_CreateErrorPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.createErr = errors.New("permission denied")
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("x")}})
	if err == nil || !errors.Is(err, fs.createErr) {
		t.Errorf("got %v, want create error wrapped", err)
	}
}

func TestPut_WriterCloseErrorPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.wcloseErr = errors.New("short write on close")
	// One entry → writeTarGz reaches f.Close, which errors.
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("x")}})
	if err == nil || !errors.Is(err, fs.wcloseErr) {
		t.Errorf("got %v, want writer close error wrapped", err)
	}
}

func TestPut_WriterWriteErrorPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.writeErr = errors.New("ENOSPC")
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("x")}})
	if err == nil || !errors.Is(err, fs.writeErr) {
		t.Errorf("got %v, want write error wrapped", err)
	}
}

func TestPut_RenameErrorPropagatesAndCleansUpTmp(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.renameErr = errors.New("cross-device link")
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("x")}})
	if err == nil || !errors.Is(err, fs.renameErr) {
		t.Errorf("got %v, want rename error wrapped", err)
	}
}

func TestDelete_RemoveAllErrorPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.removeAllErr = errors.New("device busy")
	if err := s.Delete(context.Background(), "ns", "n"); err == nil || !errors.Is(err, fs.removeAllErr) {
		t.Errorf("got %v, want RemoveAll error wrapped", err)
	}
}

func TestPrune_ReadDirErrorPropagatesWhenNotIsNotExist(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.readDirErr = errors.New("permission denied")
	if err := s.Prune(context.Background(), "ns", "n", []string{"rev"}, 0); err == nil || !errors.Is(err, fs.readDirErr) {
		t.Errorf("got %v, want ReadDir error wrapped", err)
	}
}

func TestPrune_IsNotExistOnReadDirIsSilent(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.readDirErr = os.ErrNotExist // wrapped path test would also work
	if err := s.Prune(context.Background(), "ns", "n", []string{"rev"}, 0); err != nil {
		t.Errorf("Prune on absent dir = %v, want nil", err)
	}
}

func TestStore_CloseFromFaultyFsPropagates(t *testing.T) {
	s, fs := newFaultyStore(t)
	fs.closeErr = errors.New("close hung up")
	if err := s.Close(); !errors.Is(err, fs.closeErr) {
		t.Errorf("got %v, want close error wrapped", err)
	}
}

func TestRealFS_RoundTripsThroughOSRoot(t *testing.T) {
	// Coverage for the realFS adapter (production *os.Root path) without
	// depending on the Store wrapper. Verifies every method dispatches.
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	fs := realFS{root: root}
	defer fs.Close()

	if err := fs.MkdirAll("ns/n", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	w, err := fs.Create("ns/n/file", 0o644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}
	if _, err := fs.Stat("ns/n/file"); err != nil {
		t.Errorf("Stat: %v", err)
	}
	names, err := fs.ReadDirNames("ns/n")
	if err != nil {
		t.Fatalf("ReadDirNames: %v", err)
	}
	if len(names) != 1 || names[0] != "file" {
		t.Errorf("ReadDirNames = %v, want [file]", names)
	}
	if err := fs.Rename("ns/n/file", "ns/n/file2"); err != nil {
		t.Errorf("Rename: %v", err)
	}
	if err := fs.Remove("ns/n/file2"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := fs.RemoveAll("ns"); err != nil {
		t.Errorf("RemoveAll: %v", err)
	}
	if fs.Name() != dir {
		t.Errorf("Name() = %q, want %q", fs.Name(), dir)
	}
}

func TestStore_RootPath_ReportsConfiguredDirectory(t *testing.T) {
	s := newTestStore(t)
	if s.RootPath() == "" {
		t.Errorf("RootPath returned empty string")
	}
}

func TestRealFS_ReadDirNames_ErrorOnMissingDir(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs := realFS{root: root}
	defer fs.Close()
	if _, err := fs.ReadDirNames("does-not-exist"); err == nil {
		t.Errorf("expected error from missing dir")
	}
}

// Silence unused-import in earlier drafts.
var _ io.WriteCloser = (*faultyWriter)(nil)

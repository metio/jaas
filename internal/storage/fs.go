/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"io"
	"io/fs"
	"os"
	"sort"
)

// fileSystem is the small filesystem contract Store depends on. Production
// wraps *os.Root via realFS; tests substitute a memory- or error-injecting
// implementation to cover the standard library's error paths (writer Close
// failures, RemoveAll on a busy dir, etc.) that are otherwise unreachable
// from outside the OS.
//
// Path semantics match *os.Root: every name is relative to the root, with
// "/" separators on every platform. Tests are responsible for honoring the
// same convention; the production *os.Root rejects traversal automatically.
type fileSystem interface {
	MkdirAll(name string, perm os.FileMode) error
	Create(name string, perm os.FileMode) (io.WriteCloser, error)
	Rename(oldName, newName string) error
	Remove(name string) error
	RemoveAll(name string) error
	Stat(name string) (os.FileInfo, error)
	ReadDirNames(name string) ([]string, error)
	Close() error
	Name() string

	// FS returns a read-only fs.FS view of the tree, used by the HTTP
	// handler. The production *os.Root view rejects any symlink that
	// escapes the root, so the read path keeps the same traversal
	// guarantees the write path gets from os.Root — serving via
	// http.Dir/os.DirFS would instead follow such a symlink to an
	// out-of-root target.
	FS() fs.FS
}

// realFS adapts *os.Root to fileSystem. The only method not 1:1 is Create,
// which delegates to OpenFile with CREATE|TRUNC|WRONLY so Store.Put always
// writes a fresh file.
type realFS struct{ root *os.Root }

func (r realFS) MkdirAll(name string, perm os.FileMode) error {
	return r.root.MkdirAll(name, perm)
}

func (r realFS) Create(name string, perm os.FileMode) (io.WriteCloser, error) {
	return r.root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
}

func (r realFS) Rename(oldName, newName string) error {
	return r.root.Rename(oldName, newName)
}

func (r realFS) Remove(name string) error {
	return r.root.Remove(name)
}

func (r realFS) RemoveAll(name string) error {
	return r.root.RemoveAll(name)
}

func (r realFS) Stat(name string) (os.FileInfo, error) {
	return r.root.Stat(name)
}

func (r realFS) ReadDirNames(name string) ([]string, error) {
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func (r realFS) Close() error { return r.root.Close() }
func (r realFS) Name() string { return r.root.Name() }

// FS returns os.Root's fs.FS view, which enforces the same no-escape
// traversal guarantees as the root's other operations.
func (r realFS) FS() fs.FS { return r.root.FS() }

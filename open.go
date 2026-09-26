// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"io"

	filesystem "github.com/go-filesystems/interface"
)

// Compile-time proof that FS answers both the required contract and the
// optional random-access one.
var (
	_ filesystem.Filesystem = (*FS)(nil)
	_ filesystem.Opener     = (*FS)(nil)
	_ filesystem.File       = (*handle)(nil)
)

// OpenReader reads the cabinet in the first size bytes of r.
//
// This is the shape every driver in go-filesystems answers to, so
// github.com/go-filesystems/detect can open them all through one function type.
// It reads the header, every CFFOLDER with its block table and every CFFILE,
// and no file data at all: the returned filesystem decodes on demand.
//
// r must stay valid and readable for as long as the returned filesystem is
// used; Close does not touch it, because OpenReader never took ownership of it.
//
// A cabinet whose folders use Quantum or LZX is refused here, by name -- see
// the package documentation for why those two are out of scope.
func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error) {
	return Open(r, size)
}

// Open is OpenReader returning the concrete type, for callers that want the
// cabinet-specific accessors -- SetID, PrevCabinet, Compression, Reserved --
// that filesystem.Filesystem has no room for.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	return parse(r, size)
}

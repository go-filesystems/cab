// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"errors"
	"fmt"
	iofs "io/fs"
)

// Sentinel errors. Compare with errors.Is so wrapped errors continue to match.
//
// They are divided by what a caller can DO about the failure; see the package
// documentation, "Three sentences this package keeps apart".
var (
	// ErrNotCabinet says the bytes are not a cabinet at all: the file is
	// shorter than the four magic bytes, or those bytes are not "MSCF".
	// "I do not know what this is."
	ErrNotCabinet = errors.New("cab: not a Microsoft Cabinet file")

	// ErrUnknownCompression says a CFFOLDER named a compression method this
	// format revision does not define (typeCompress low nibble 4..15).
	// "I do not know what this is" -- about one folder rather than the file.
	ErrUnknownCompression = errors.New("cab: unknown compression method")

	// ErrCompressionUnsupported says the cabinet is well-formed and uses a
	// method this package does not implement: Quantum or LZX. The error text
	// names which one, and which folder. "I know exactly what this is and do
	// not read it yet."
	ErrCompressionUnsupported = errors.New("cab: compression method not read by this package")

	// ErrSpansCabinets says an entry is continued from, or into, another
	// cabinet of the same set. One io.ReaderAt is one file and the rest of the
	// set is in other files, so this reader cannot complete such an entry --
	// and does not pad it with zeros and call that a success.
	ErrSpansCabinets = errors.New("cab: entry continues in another cabinet of the set")

	// ErrTruncated says a structure the cabinet points at runs past the end of
	// the bytes available. "This file is broken."
	ErrTruncated = errors.New("cab: cabinet is truncated")

	// ErrCorrupt says a field, a block checksum or a DEFLATE stream
	// contradicts the rest of the file. "This file is broken."
	ErrCorrupt = errors.New("cab: cabinet is corrupt")

	// ErrReadOnly is returned by every mutating method (WriteFile, MkDir,
	// DeleteFile, DeleteDir, Rename). A cabinet is a read-only format.
	ErrReadOnly = errors.New("cab: cabinet is read-only")

	// ErrNotFound is returned when a path is not in the cabinet. It satisfies
	// errors.Is(err, io/fs.ErrNotExist), which is the error contract every
	// driver in this org owes its callers.
	ErrNotFound = fmt.Errorf("cab: path not found: %w", iofs.ErrNotExist)

	// ErrNotDirectory is returned when ListDir targets something that is not a
	// directory.
	ErrNotDirectory = errors.New("cab: not a directory")

	// ErrNotRegular is returned when ReadFile or OpenFile targets a directory.
	ErrNotRegular = errors.New("cab: not a regular file")

	// ErrNotSymlink is returned by ReadLink for every path: the cabinet format
	// records no symbolic links, so there is never one to read.
	ErrNotSymlink = errors.New("cab: not a symbolic link")

	// ErrNegativeOffset is returned by File.ReadAt for a negative offset.
	ErrNegativeOffset = errors.New("cab: negative offset")
)

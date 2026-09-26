// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package cab is a pure-Go reader for the Microsoft Cabinet (.cab) format,
// presenting a cabinet as a filesystem.Filesystem.
//
// It needs nothing outside the standard library: MSZIP is raw DEFLATE, and
// compress/flate is exactly the decoder it asks for. CGO_ENABLED=0 throughout.
//
//	fs, err := cab.OpenReader(f, size)   // f is an io.ReaderAt
//	data, err := fs.ReadFile("sub/dir/file.txt")
//
// [FS] also implements the optional filesystem.Opener, so a caller that must
// answer a byte range without materialising a whole entry can, subject to what
// the format costs (see "Random access", below).
//
// # Scope: None and MSZIP are read; Quantum and LZX are not
//
// A CFFOLDER names one of four compression methods. This package reads two of
// them:
//
//   - None (typeCompress 0): the folder's blocks are stored.
//   - MSZIP (1): each block is the two bytes "CK" followed by a raw DEFLATE
//     stream.
//
// Quantum (2) and LZX (3) are NOT read, and this is a decision rather than an
// omission. Each is a substantial algorithm in its own right -- LZX especially,
// with its own tree-delta coding, aligned-offset blocks and E8 call-offset
// translation -- and nothing in this ecosystem produces or consumes them: the
// cabinets in circulation here come from gcab and from makecab's default, both
// of which write MSZIP or store. A cabinet using either one is refused at
// [OpenReader] with [ErrCompressionUnsupported], and the error NAMES the method
// and the folder it found it in.
//
// # Three sentences this package keeps apart
//
// A reader that answers "bad file" to every question is useless to whoever has
// to decide what to do next, so the failures are divided by what the caller can
// act on:
//
//   - "I do not know what this is" -- [ErrNotCabinet] (the four magic bytes are
//     not "MSCF", or the file is too small to hold them) and
//     [ErrUnknownCompression] (a typeCompress this format revision does not
//     define). Try another reader, or another file.
//   - "I know exactly what this is and do not read it yet" --
//     [ErrCompressionUnsupported] (Quantum, LZX) and [ErrSpansCabinets] (an
//     entry continued from, or into, another cabinet of the same set, which one
//     io.ReaderAt cannot reach). The file is fine; this package is the limit.
//   - "this file is broken" -- [ErrTruncated] (a structure runs past the end)
//     and [ErrCorrupt] (a field, a checksum or a DEFLATE stream contradicts the
//     rest). Nothing will read it.
//
// # Reserve fields change the size of every later structure
//
// When the header carries CFHEADER flag cfhdrRESERVE_PRESENT (0x0004) it is
// followed by three sizes -- cbCFHeader, cbCFFolder, cbCFData -- and each one
// inserts an opaque area into EVERY instance of the structure it names: after
// the header's fixed fields, after every CFFOLDER, and, for cbCFData, between
// every CFDATA header and its compressed bytes. A reader that skips the
// header's own reserve area and forgets the other two lands one field out for
// the rest of the file, which is the classic way a cabinet reader fails. All
// three are honoured here, and testdata/reserve.cab exercises all three at
// once.
//
// That fixture also recorded a disagreement between reference readers:
// libgcab and libmspack (cabextract) both read the per-CFDATA reserve area as
// the specification describes it, and 7-Zip 26.03 reports "Data Error" on the
// same file. The specification's CFDATA layout puts abReserve between cbUncomp
// and ab[], two of three readers agree with it, and so does this package.
//
// # MSZIP history crosses block boundaries
//
// A folder's uncompressed stream is the concatenation of its CFDATA blocks'
// output, and under MSZIP the DEFLATE history carries ACROSS those blocks:
// block N may reference bytes emitted by block N-1. Each block is its own
// DEFLATE stream, so a decoder that simply starts a fresh one per block is
// correct for any file small enough to fit in one block -- which is every
// hand-made test fixture -- and silently wrong beyond that. The remedy is
// flate.NewReaderDict, seeded with exactly the preceding 32 KiB of folder
// output; this package keeps that window and testdata/mszip.cab spans eight
// blocks so that dropping it fails.
//
// # Random access costs what the format costs
//
// A folder is a STREAM. Under MSZIP, reaching byte n of a folder means
// inflating the n bytes before it, because the history window makes no block
// independently decodable; a handle therefore keeps its decoder and its
// position, so reading forwards is linear overall while reading BACKWARDS
// starts a fresh pass from the start of the folder. Under None there is no
// history, so a handle restarts at the block containing the offset and reads at
// most one block's worth it does not need.
//
// Opener is implemented anyway, rather than withheld, because the alternative
// for a caller serving 4 KiB requests is ReadFile -- which holds the WHOLE
// entry in memory. A bounded rescan is worse than nothing only if the caller
// expected a seek to be free, and this paragraph exists so that it does not.
//
// # Read-only
//
// A cabinet is read-only: every mutating method of filesystem.Filesystem
// (WriteFile, MkDir, DeleteFile, DeleteDir, Rename) returns [ErrReadOnly], as
// in the other read-only drivers of this org.
//
// # Names
//
// A CFFILE name is a NUL-terminated byte string using BACKSLASH as its path
// separator; this package converts to '/' and cleans the result, so a cabinet's
// "sub\deep\file.txt" is reached as "sub/deep/file.txt". A cabinet holds no
// directory entries of its own, so the directories a listing needs are
// synthesised from the paths that are there.
//
// The encoding is chosen by one attribute bit: with _A_NAME_IS_UTF (0x80) the
// name is UTF-8, and without it Windows-1252. The five byte values Windows-1252
// leaves undefined (0x81, 0x8D, 0x8F, 0x90, 0x9D) decode to U+FFFD.
package cab

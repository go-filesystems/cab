// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"testing"
)

// ⚠ The emulated CI lanes run a `go test -c` binary inside a container that has
// no testdata/ directory, so every fixture is EMBEDDED rather than opened. Check
// with: go test -c -o /tmp/t.test . && cd /tmp && ./t.test
//
//go:embed testdata/mszip.cab testdata/store.cab testdata/paths.cab
//go:embed testdata/reserve.cab testdata/history.cab
var fixtures embed.FS

// The five cabinets, how they were made, and which reference readers were asked
// to agree before any of them was allowed to judge this decoder.
//
// mszip.cab, store.cab and paths.cab were written by gcab 1.6 (libgcab), an
// independent implementation of the format:
//
//	gcab -c -z mszip.cab big.txt small.txt
//	gcab -c    store.cab big.txt small.txt
//	gcab -c -z paths.cab big.txt sub/deep/nested.txt sub/café.txt
//
// reserve.cab re-encodes mszip.cab with TWO folders (an MSZIP one and a stored
// one) and all three reserve areas present -- cbCFHeader 12, cbCFFolder 4,
// cbCFData 2. Two folders, because with only one the cbCFFolder area sits after
// the last structure that uses it and coffFiles is absolute: a single-folder
// cabinet CANNOT detect a reader that ignores cbCFFolder, and the first version
// of this fixture did not (see TestAblateReserveSizes). gcab 1.6 and cabextract
// 1.11 (libmspack) both extract it to identical bytes; 7-Zip 26.03 reports
// "Data Error" on any cabinet with a per-CFDATA reserve area, so it is not a
// witness for this one.
//
// history.cab holds a payload whose every 32 KiB block repeats the block before
// it, compressed with compress/flate's NewWriterDict seeded with that preceding
// block. Blocks 1..7 are 262 bytes each against block 0's 32785, so they are
// almost entirely back-references INTO THE PREVIOUS BLOCK -- and block 1 does not
// inflate at all without that dictionary. gcab 1.6, 7-Zip 26.03 and libarchive
// 3.7.4 (bsdtar) all extract it to identical bytes; cabextract 1.11 stops after
// block 0 with "decompression error", so libmspack does not carry the history
// and is not a witness here either.
//
// ⛔ gcab's own MSZIP output carries NO cross-block references, so mszip.cab
// cannot see a decoder that drops the history window. That is what history.cab
// is for, and TestAblateCrossBlockHistory asserts both halves of it.
const (
	fxMSZIP   = "testdata/mszip.cab"
	fxStore   = "testdata/store.cab"
	fxPaths   = "testdata/paths.cab"
	fxReserve = "testdata/reserve.cab"
	fxHistory = "testdata/history.cab"
)

// The payload sha256 sums, so that a fixture regenerated with a different
// payload fails loudly instead of quietly measuring something else.
const (
	sumBig    = "b82c5a7f65a27cad57743fc19cf03807fae1acf314febf4eeb2451868ea96afa"
	sumSmall  = "8019ac926eb089ccfc071739c8606af14034ddbf7d48643eb2dd40ed9dc3fe79"
	sumNested = "5d0e5bc93dbc8febbb6b3cea0503bfd7460284e544c2f689538fd55da59accb2"
	sumCafe   = "03f88f29ad1a19bc329f622300923db0a6ff2b01319be4fd0fdcf9eb8c608732"
	sumHist   = "3425527a2a92e600366aab982d58ac06a79f15b513c79f09dffecf3f34525977"
	sizeBig   = 250011
	sizeHist  = 8 * maxBlockUncomp
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fixtures.ReadFile(name)
	if err != nil {
		t.Fatalf("embedded fixture %s: %v", name, err)
	}
	return b
}

// openFixture opens an embedded cabinet, optionally after patching bytes into
// it. Every patch is an in-place overwrite of the same length, so no offset in
// the cabinet moves.
func openFixture(t *testing.T, name string, patches ...patch) *FS {
	t.Helper()
	b := apply(t, fixture(t, name), patches...)
	fs, err := Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	return fs
}

type patch struct {
	off  int
	with []byte
}

func apply(t *testing.T, b []byte, patches ...patch) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	for _, p := range patches {
		if p.off < 0 || p.off+len(p.with) > len(out) {
			t.Fatalf("patch at %d of %d bytes is outside the %d-byte fixture", p.off, len(p.with), len(out))
		}
		copy(out[p.off:], p.with)
	}
	return out
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// readFile reads a path and fails the test if it cannot.
func readFile(t *testing.T, fs *FS, p string) []byte {
	t.Helper()
	b, err := fs.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", p, err)
	}
	return b
}

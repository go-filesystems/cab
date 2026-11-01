// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Bombs: small cabinets that made the reader allocate, or spin, without bound.
// Every number was measured on the code before the fix (runtime.MemStats
// TotalAlloc, or wall time, around the call):
//
//	2 000 folders sharing one chain of 2 000 blocks, 32 054 bytes    130 457 800 bytes
//	  (65 535 of 65 535, from a 1 MiB file, is 103 GB)
//	ReadFile of a 65 MB claim over a 16 KB cabinet                    65 549 624 bytes
//	65 535 files in one directory, 1.6 MB                             3.08 s
//	20 000 files 124 directories deep each, 5.4 MB                    968 732 496 bytes

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

type rawFile struct {
	size, off uint32
	name      string
}

// rawCab builds a cabinet without craft's honesty: nFolders folders each saying it
// has `blocks` blocks, which start at the one chain that exists (shared) or at a
// chain of their own, every block (cbData cbData, cbUncomp unc) with no bytes
// behind it -- so the only honest cbData is 0.
func rawCab(typeC uint16, nFolders, blocks int, cbData, unc uint16, files []rawFile, shared bool) []byte {
	var tbl bytes.Buffer
	w := func(b *bytes.Buffer, v any) { _ = binary.Write(b, binary.LittleEndian, v) }
	for _, f := range files {
		w(&tbl, f.size)
		w(&tbl, f.off)
		w(&tbl, uint16(0))    // iFolder
		w(&tbl, uint16(0x21)) // date
		w(&tbl, uint16(0))    // time
		w(&tbl, uint16(0x20)) // attribs
		tbl.WriteString(f.name)
		tbl.WriteByte(0)
	}
	coffFiles := hdrFixedSize + nFolders*folderFixedSize
	dataStart := coffFiles + tbl.Len()
	chains := nFolders
	if shared {
		chains = 1
	}
	blockLen := dataFixedSize + int(cbData)
	total := dataStart + chains*blocks*blockLen
	var out bytes.Buffer
	out.WriteString("MSCF")
	w(&out, uint32(0))
	w(&out, uint32(total))
	w(&out, uint32(0))
	w(&out, uint32(coffFiles))
	w(&out, uint32(0))
	out.WriteByte(1) // verMin
	out.WriteByte(3) // verMaj
	w(&out, uint16(nFolders))
	w(&out, uint16(len(files)))
	w(&out, uint16(0)) // flags
	w(&out, uint16(1)) // setID
	w(&out, uint16(0)) // iCabinet
	for i := range nFolders {
		start := dataStart
		if !shared {
			start += i * blocks * blockLen
		}
		w(&out, uint32(start))
		w(&out, uint16(blocks))
		w(&out, typeC)
	}
	out.Write(tbl.Bytes())
	for range chains {
		for range blocks {
			w(&out, uint32(0))
			w(&out, cbData)
			w(&out, unc)
			out.Write(make([]byte, cbData))
		}
	}
	return out.Bytes()
}

func open(b []byte) (*FS, error) { return Open(bytes.NewReader(b), int64(len(b))) }

func TestFoldersAreProvedToFitBeforeTheyAreAllocated(t *testing.T) {
	c := craft{
		folders:      []cFolder{{blocks: []cBlock{{data: []byte("x")}}}},
		files:        []cFile{{size: 1, name: []byte("a")}},
		cFoldersSaid: u16(65535),
	}
	b := c.build()
	var err error
	got := allocated(func() { _, err = open(b) })
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("Open = %v, want ErrTruncated", err)
	}
	// 65 535 folders were 5 MB of folder{} before anything checked the file held them.
	if got > 64<<10 {
		t.Errorf("%d bytes allocated for a %d-byte cabinet", got, len(b))
	}
}

func TestFoldersCannotShareOneChainOfBlocks(t *testing.T) {
	b := rawCab(0, 2000, 2000, 0, 0, []rawFile{{0, 0, "a"}}, true)
	var err error
	got := allocated(func() { _, err = open(b) })
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt", err)
	}
	if got > 1<<20 {
		t.Errorf("%d bytes allocated to refuse a %d-byte cabinet", got, len(b))
	}
}

// TestTheBlockBudgetIsExact: two folders over one chain of n blocks declare 2n; the
// cabinet holds floor(len/8) blocks' worth of bytes. Passing exactly when 2n fits
// shows the budget is len/8, not some looser number.
func TestTheBlockBudgetIsExact(t *testing.T) {
	var passed, refused int
	for n := 1; n <= 24; n++ {
		b := rawCab(0, 2, n, 0, 0, []rawFile{{0, 0, "a"}}, true)
		_, err := open(b)
		fits := int64(2*n) <= int64(len(b))/(dataFixedSize)
		switch {
		case fits && err != nil:
			t.Errorf("n=%d: 2n=%d blocks in %d bytes should fit: %v", n, 2*n, len(b), err)
		case !fits && !errors.Is(err, ErrCorrupt):
			t.Errorf("n=%d: 2n=%d blocks in %d bytes should be refused: %v", n, 2*n, len(b), err)
		}
		if fits {
			passed++
		} else {
			refused++
		}
	}
	if passed == 0 || refused == 0 {
		t.Fatalf("the range does not cross the boundary: %d pass, %d refuse", passed, refused)
	}
}

// TestABlockCannotYieldMoreThanDeflateCanPack pins maxInflateRatio from both sides:
// 1032 bytes a byte of DEFLATE (the bytes after "CK") passes, one more is refused.
func TestABlockCannotYieldMoreThanDeflateCanPack(t *testing.T) {
	for _, tc := range []struct {
		cbData, cbUncomp uint16
		ok               bool
	}{
		{0, 0, true},
		{1, 0, true},
		{2, 0, true},
		{2, 1, false},
		{0, 1, false},
		{3, 1032, true},
		{3, 1033, false},
		{34, 32768, true},  // 32 x 1032 = 33 024
		{33, 32768, false}, // 31 x 1032 = 31 992
		{66, 32768, true},
	} {
		c := craft{
			folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{
				{data: make([]byte, tc.cbData), cbUncomp: u16(tc.cbUncomp)}}}},
			files: []cFile{{size: 0, name: []byte("a")}},
		}
		_, err := open(c.build())
		if tc.ok && err != nil {
			t.Errorf("cbData %d, cbUncomp %d: refused: %v", tc.cbData, tc.cbUncomp, err)
		}
		if !tc.ok && !errors.Is(err, ErrCorrupt) {
			t.Errorf("cbData %d, cbUncomp %d: %v, want ErrCorrupt", tc.cbData, tc.cbUncomp, err)
		}
	}
	// The stored method has its own, stricter rule and is not affected.
	c := craft{
		folders: []cFolder{{blocks: []cBlock{{data: nil, cbUncomp: u16(0)}}}},
		files:   []cFile{{size: 0, name: []byte("a")}},
	}
	if _, err := open(c.build()); err != nil {
		t.Errorf("an empty stored block: %v", err)
	}
}

func TestReadFileHoldsOnlyWhatComesOut(t *testing.T) {
	// 65 MB claimed, no DEFLATE behind it: parse now refuses the claim itself.
	b := rawCab(uint16(CompressionMSZIP), 1, 2000, 0, 32768, []rawFile{{2000 * 32768, 0, "big"}}, false)
	if _, err := open(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt", err)
	}
	// 65 MB claimed with bytes that satisfy the ratio but are not DEFLATE: the claim
	// is accepted and ReadFile allocates one step, not the claim.
	const blocks = 2000
	b = rawCab(uint16(CompressionMSZIP), 1, blocks, 34, 32768, []rawFile{{blocks * 32768, 0, "big"}}, false)
	fsys, err := open(b)
	if err != nil {
		t.Fatal(err)
	}
	got := allocated(func() { _, err = fsys.ReadFile("big") })
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadFile = %v, want ErrCorrupt (the bytes are not DEFLATE)", err)
	}
	// Measured before: 65 549 624. The step is 16 MiB, so this is a bound, not a
	// zero -- what it must not be is the 65 MB claim.
	if got > 20<<20 {
		t.Errorf("ReadFile allocated %d bytes over a %d-byte cabinet", got, len(b))
	}
}

func TestReadFileHasACeilingAndOpenFileHasNone(t *testing.T) {
	const blocks = 32769 // x 32 768 = 1 GiB + 32 KiB
	mk := func(size uint32) *FS {
		b := rawCab(uint16(CompressionMSZIP), 1, blocks, 34, 32768, []rawFile{{size, 0, "huge"}}, false)
		fsys, err := open(b)
		if err != nil {
			t.Fatal(err)
		}
		return fsys
	}
	over := mk(MaxReadFileSize + 1)
	var err error
	got := allocated(func() { _, err = over.ReadFile("huge") })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadFile one over the ceiling: %v, want ErrTooLarge", err)
	}
	if got > 1<<20 {
		t.Errorf("refusing it allocated %d bytes", got)
	}
	h, err := over.OpenFile("huge")
	if err != nil || h.Size() != MaxReadFileSize+1 {
		t.Errorf("OpenFile is not subject to the ceiling: %v, %v", h, err)
	}
	// At the ceiling the claim passes the check and fails later, on the bytes.
	at := mk(MaxReadFileSize)
	if _, err = at.ReadFile("huge"); errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadFile at the ceiling: %v, want ErrCorrupt from the first block", err)
	}
}

// TestReadFileGrowsAcrossSteps reads a stored entry bigger than one step, so the
// buffer grows as the data arrives and the result is still exact.
func TestReadFileGrowsAcrossSteps(t *testing.T) {
	const blockLen = 32768
	n := (readStep+(1<<20))/blockLen + 1 // a little over one step
	var blocks []cBlock
	var want []byte
	for i := range n {
		d := bytes.Repeat([]byte{byte(i % 251)}, blockLen)
		blocks = append(blocks, cBlock{data: d})
		want = append(want, d...)
	}
	c := craft{
		folders: []cFolder{{blocks: blocks}},
		files:   []cFile{{size: uint32(len(want)), name: []byte("a")}},
	}
	fsys := mustOpen(t, c)
	got, err := fsys.ReadFile("a")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadFile: %d bytes, %v; want %d bytes", len(got), err, len(want))
	}
}

// TestAWideDirectoryIsLinear: filing N files under one directory used to cost N²/2
// comparisons. The test compares N with 4N and not a number of seconds, so a slow
// machine does not fail it: linear is about 4x, quadratic about 16x.
func TestAWideDirectoryIsLinear(t *testing.T) {
	timeIt := func(n int) time.Duration {
		best := time.Hour
		for range 5 {
			f := &FS{byPath: map[string]*entry{}, kids: map[string][]string{}}
			d := time.Now()
			for i := range n {
				if err := f.add(&entry{path: fmt.Sprintf("d/f%d", i), ord: i}); err != nil {
					t.Fatal(err)
				}
			}
			best = min(best, time.Since(d))
		}
		return best
	}
	small, large := timeIt(20000), timeIt(80000)
	ratio := float64(large) / float64(small)
	t.Logf("20 000 files: %v, 80 000: %v, ratio %.1f", small, large, ratio)
	if ratio > 10 {
		t.Errorf("4x the files took %.1fx the time: the index is not linear", ratio)
	}
}

func TestEachChildIsFiledOnceUnderEachParent(t *testing.T) {
	f := &FS{byPath: map[string]*entry{}, kids: map[string][]string{}}
	for i, p := range []string{"a/b/c", "a/b/d", "a/e", "f"} {
		if err := f.add(&entry{path: p, ord: i}); err != nil {
			t.Fatal(err)
		}
	}
	for dir, want := range map[string][]string{
		".":   {"a", "f"},
		"a":   {"a/b", "a/e"},
		"a/b": {"a/b/c", "a/b/d"},
	} {
		if got := f.kids[dir]; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("kids[%q] = %v, want %v", dir, got, want)
		}
	}
}

func TestTheEntryCeilingCountsDirectories(t *testing.T) {
	defer func(old int) { maxEntries = old }(maxEntries)
	maxEntries = 10
	files := func(n int, dirs bool) craft {
		var fs []cFile
		for i := range n {
			name := fmt.Sprintf("f%d", i)
			if dirs {
				name = fmt.Sprintf(`d%d\f`, i) // each in a directory nothing declared
			}
			fs = append(fs, cFile{size: 0, name: []byte(name)})
		}
		return craft{folders: []cFolder{{blocks: []cBlock{{data: []byte("x")}}}}, files: fs}
	}
	for _, tc := range []struct {
		name string
		c    craft
		ok   bool
	}{
		{"10 files", files(10, false), true},
		{"11 files", files(11, false), false},
		{"5 files and their 5 directories", files(5, true), true},
		{"6 files and their 6 directories", files(6, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := open(tc.c.build())
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Open = %v, want ErrTooLarge", err)
			}
		})
	}
}

func TestDeepDirectoriesAreBoundedByTheEntryCeiling(t *testing.T) {
	// 2 200 files 124 directories deep each, none shared: 275 000 entries, over the
	// ceiling (256 Ki = 262 144). The 20 000-file cabinet measured at 968 MB would
	// need 2.5 million.
	var files []rawFile
	for i := range 2200 {
		files = append(files, rawFile{0, 0, fmt.Sprintf("%05d", i) + strings.Repeat(`\a`, 124)})
	}
	b := rawCab(0, 1, 1, 0, 0, files, false)
	var err error
	got := allocated(func() { _, err = open(b) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Open = %v, want ErrTooLarge", err)
	}
	// MaxEntries entries at about 430 bytes each; measured before: 968 MB for 20 000.
	t.Logf("%d-byte cabinet refused after %d bytes allocated", len(b), got)
	if got > 200<<20 {
		t.Errorf("%d bytes allocated before refusing", got)
	}
}

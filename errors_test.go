// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// TestThreeSentences is the distinction the package documentation makes, held as
// a test: "I do not know what this is", "I know exactly what this is and do not
// read it yet", "this file is broken". A reader that collapses them leaves its
// caller nothing to decide with.
func TestThreeSentences(t *testing.T) {
	big := bytes.Repeat([]byte("payload."), 8)

	cases := []struct {
		name string
		c    craft
		raw  []byte // used instead of c when non-nil
		want error
		says string // a fragment the message must carry
	}{
		// --- "I do not know what this is" -----------------------------------
		{name: "too short for a signature", raw: []byte{'M', 'S'}, want: ErrNotCabinet},
		{name: "wrong signature", raw: []byte("NOPEnothingatallhereofanyusewhatsoever"), want: ErrNotCabinet},
		{
			name: "compression the format does not define",
			c:    craft{folders: []cFolder{{typeC: 7, blocks: stored(big)}}},
			want: ErrUnknownCompression, says: "typeCompress(7)",
		},

		// --- "I know what this is and do not read it yet" -------------------
		{
			name: "Quantum",
			c:    craft{folders: []cFolder{{typeC: uint16(CompressionQuantum), blocks: stored(big)}}},
			want: ErrCompressionUnsupported, says: "Quantum",
		},
		{
			name: "LZX with a window size in the high bits",
			c:    craft{folders: []cFolder{{typeC: 0x1503, blocks: stored(big)}}},
			want: ErrCompressionUnsupported, says: "LZX",
		},

		// --- "this file is broken" -----------------------------------------
		{name: "header cut short", c: craft{folders: []cFolder{{blocks: stored(big)}}, truncateTo: 20}, want: ErrTruncated},
		{name: "cbCabinet below the header size", c: craft{folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(10)}, want: ErrTruncated, says: "cbCabinet is 10"},
		{name: "cbCabinet beyond the bytes held", c: craft{folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(1 << 20)}, want: ErrTruncated},
		{name: "no folders at all", c: craft{}, want: ErrCorrupt, says: "cFolders is 0"},
		{
			name: "reserve area cut short",
			c:    craft{resHdr: bytes.Repeat([]byte{1}, 64), folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(44), truncateTo: 44},
			want: ErrTruncated,
		},
		{
			name: "a neighbour's name begins at the very end",
			c:    craft{prev: &[2]string{"a.cab", "d"}, folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(36), truncateTo: 36},
			want: ErrTruncated, says: "at the end of the cabinet",
		},
		{
			name: "folder table runs past the end",
			c:    craft{folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(40), truncateTo: 40},
			want: ErrTruncated,
		},
		{
			// The reserve SIZES themselves are cut off, so every field after
			// them is read against an error that is already set: the cursor
			// must keep the first failure and not compound it.
			name: "the reserve sizes are cut off",
			c:    craft{resHdr: bytes.Repeat([]byte{1}, 8), folders: []cFolder{{blocks: stored(big)}}, cbCabinet: u32(38), truncateTo: 38},
			want: ErrTruncated,
		},
		{
			name: "coffFiles points past the end",
			c:    craft{folders: []cFolder{{blocks: stored(big)}}, files: []cFile{{size: 1, name: []byte("a")}}, coffFiles: u32(1 << 20)},
			want: ErrTruncated,
		},
		{
			name: "block table runs past the end",
			c:    craft{folders: []cFolder{{blocks: stored(big), coffSaid: u32(1 << 20)}}},
			want: ErrTruncated,
		},
		{
			name: "a block claims more bytes than the cabinet holds",
			c:    craft{folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{{data: big, cbDataSaid: u16(40000)}}}}},
			want: ErrTruncated, says: "past the end",
		},
		{
			name: "a block yields more than the 32768 ceiling",
			c:    craft{folders: []cFolder{{blocks: []cBlock{{data: big, cbUncomp: u16(40000)}}}}},
			want: ErrCorrupt, says: "over the 32768 ceiling",
		},
		{
			name: "a stored block that is not stored",
			c:    craft{folders: []cFolder{{blocks: []cBlock{{data: big, cbUncomp: u16(99)}}}}},
			want: ErrCorrupt, says: "is stored yet holds",
		},
		{
			name: "an entry names a folder that is not there",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files:   []cFile{{size: 1, iFolder: 5, name: []byte("a")}},
			},
			want: ErrCorrupt, says: "names folder 5 of 1",
		},
		{
			name: "an entry wants more of its folder than the folder yields",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files:   []cFile{{size: 9999, name: []byte("a")}},
			},
			want: ErrCorrupt, says: "wants bytes [0, 9999)",
		},
		{
			name: "a name flagged UTF-8 that is not UTF-8",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files:   []cFile{{size: 1, attribs: attrNameIsUTF8, name: []byte{0xff, 0xfe}}},
			},
			want: ErrCorrupt, says: "flagged UTF-8",
		},
		{
			name: "a name that resolves to no path",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files:   []cFile{{size: 1, name: []byte(`\`)}},
			},
			want: ErrCorrupt, says: "no path at all",
		},
		{
			name: "a name with no NUL within 256 bytes",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files:   []cFile{{size: 1, name: bytes.Repeat([]byte("a"), 300), noNUL: true}},
			},
			want: ErrCorrupt, says: "not NUL-terminated",
		},
		{
			name: "two entries naming one path",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files: []cFile{
					{size: 1, name: []byte("a.txt")},
					{size: 1, name: []byte("a.txt")},
				},
			},
			want: ErrCorrupt, says: "entries 0 and 1 both name",
		},
		{
			name: "an entry naming a directory another entry implies",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files: []cFile{
					{size: 1, name: []byte(`d\a.txt`)},
					{size: 1, name: []byte("d")},
				},
			},
			want: ErrCorrupt, says: "already implies as a directory",
		},
		{
			name: "a file that is also a parent directory",
			c: craft{
				folders: []cFolder{{blocks: stored(big)}},
				files: []cFile{
					{size: 1, name: []byte("d")},
					{size: 1, name: []byte(`d\a.txt`)},
				},
			},
			want: ErrCorrupt, says: "both a file and the parent directory",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.raw
			if b == nil {
				b = tc.c.build()
			}
			_, err := Open(bytes.NewReader(b), int64(len(b)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("message %q does not say %q", err, tc.says)
			}
		})
	}
}

// TestSpanningEntryIsRefusedByName: an entry continued across cabinets is not
// padded with zeros and called a success.
func TestSpanningEntryIsRefusedByName(t *testing.T) {
	for _, marker := range []uint16{folderContinuedFromPrev, folderContinuedToNext, folderContinuedPrevAndNext} {
		c := craft{
			setID:   42,
			next:    &[2]string{"two.cab", "Disk 2"},
			folders: []cFolder{{typeC: uint16(CompressionNone), blocks: stored([]byte("abc"))}},
			files:   []cFile{{size: 1 << 20, iFolder: marker, name: []byte("huge.bin")}},
		}
		fs := mustOpen(t, c)
		// The entry is INDEXED -- a listing shows it -- and only reading refuses.
		if st, err := fs.Stat("huge.bin"); err != nil || st.Size() != 1<<20 {
			t.Errorf("marker %#x: Stat = %v, %v", marker, st, err)
		}
		if _, err := fs.OpenFile("huge.bin"); !errors.Is(err, ErrSpansCabinets) {
			t.Errorf("marker %#x: OpenFile = %v, want ErrSpansCabinets", marker, err)
		}
		if _, err := fs.ReadFile("huge.bin"); !errors.Is(err, ErrSpansCabinets) {
			t.Errorf("marker %#x: ReadFile = %v, want ErrSpansCabinets", marker, err)
		}
	}
}

// TestBlockLevelCorruption covers the failures a block raises while being read,
// rather than while being indexed.
func TestBlockLevelCorruption(t *testing.T) {
	cases := []struct {
		name string
		c    craft
		want error
		says string
	}{
		{
			name: "checksum disagrees with the block",
			c: craft{
				folders: []cFolder{{blocks: []cBlock{{data: []byte("hello"), csum: u32(0xDEADBEEF)}}}},
				files:   []cFile{{size: 5, name: []byte("a")}},
			},
			want: ErrCorrupt, says: "recorded 0xdeadbeef",
		},
		{
			name: "MSZIP block with no CK signature",
			c: craft{
				folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{{data: []byte("XKrubbish"), cbUncomp: u16(5)}}}},
				files:   []cFile{{size: 5, name: []byte("a")}},
			},
			want: ErrCorrupt, says: `"CK" signature`,
		},
		{
			name: "MSZIP block too short to hold CK",
			c: craft{
				folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{{data: []byte("C"), cbUncomp: u16(5)}}}},
				files:   []cFile{{size: 5, name: []byte("a")}},
			},
			want: ErrCorrupt, says: `"CK" signature`,
		},
		{
			name: "MSZIP stream that will not inflate",
			c: craft{
				folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{{data: []byte("CK\xff\xff\xff\xff"), cbUncomp: u16(64)}}}},
				files:   []cFile{{size: 64, name: []byte("a")}},
			},
			want: ErrCorrupt, says: "inflating the 64 bytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := mustOpen(t, tc.c)
			_, err := fs.ReadFile("a")
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadFile = %v, want %v", err, tc.want)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("message %q does not say %q", err, tc.says)
			}
		})
	}
}

// TestChecksumZeroMeansNotComputed: a recorded 0 is "not computed" and must not
// be compared, or every cabinet from a writer that skips the checksum breaks.
func TestChecksumZeroMeansNotComputed(t *testing.T) {
	body := []byte("the checksum of this is certainly not zero")
	if checksum(body, uint32(len(body))|uint32(len(body))<<16) == 0 {
		t.Fatal("pick another body: its real checksum is 0")
	}
	fs := mustOpen(t, craft{
		folders: []cFolder{{blocks: []cBlock{{data: body, csum: u32(0)}}}},
		files:   []cFile{{size: uint32(len(body)), name: []byte("a")}},
	})
	if got := readFile(t, fs, "a"); !bytes.Equal(got, body) {
		t.Errorf("a = %q", got)
	}
}

// TestIOFailureIsReportedAsItself: a read that fails underneath is not
// relabelled "corrupt". A caller told the cabinet is broken when the disk is
// what failed goes looking in the wrong place.
func TestIOFailureIsReportedAsItself(t *testing.T) {
	b := fixture(t, fxMSZIP)
	// Locate the second CFDATA block's compressed bytes, and fail from there.
	fs0 := openFixture(t, fxMSZIP)
	from := fs0.folders[0].blocks[1].dataOff
	boom := errors.New("the disk said no")
	var armed atomic.Bool
	r := failAt{r: bytes.NewReader(b), from: from, err: boom, armed: &armed}

	fs, err := Open(r, int64(len(b)))
	if err != nil {
		t.Fatalf("Open must succeed while the reader is unarmed: %v", err)
	}
	armed.Store(true)
	// ReadFull's path: a read that reaches into the failing block.
	if _, err := fs.ReadFile("big.txt"); !errors.Is(err, boom) {
		t.Errorf("ReadFile = %v, want the underlying error", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Error("an I/O failure was relabelled as corruption")
	}
	// CopyN's path: the discard that walks to uoffFolderStart fails.
	if _, err := fs.ReadFile("small.txt"); !errors.Is(err, boom) {
		t.Errorf("ReadFile(small.txt) = %v, want the underlying error", err)
	}
	// And a failure while the header itself is being read.
	r2 := failAt{r: bytes.NewReader(b), from: 8, err: boom}
	if _, err := Open(r2, int64(len(b))); !errors.Is(err, boom) {
		t.Errorf("Open with a failing header read = %v", err)
	}
	// And a failure while a NAME is being read.
	pb := fixture(t, fxPaths)
	r3 := failAt{r: bytes.NewReader(pb), whenLen: maxNameLen + 1, err: boom}
	if _, err := Open(r3, int64(len(pb))); !errors.Is(err, boom) {
		t.Errorf("Open with a failing name read = %v", err)
	}
}

// TestEmptyBlockIsSkipped: a CFDATA that yields nothing must not end the
// stream, which is why the block loop is a loop.
func TestEmptyBlockIsSkipped(t *testing.T) {
	body := []byte("a folder whose first block yields nothing at all")
	fs := mustOpen(t, craft{
		folders: []cFolder{{typeC: uint16(CompressionMSZIP), blocks: []cBlock{
			{data: []byte("CK"), cbUncomp: u16(0)},
			mszipBlock(t, body, nil),
		}}},
		files: []cFile{{size: uint32(len(body)), name: []byte("a")}},
	})
	if got := readFile(t, fs, "a"); !bytes.Equal(got, body) {
		t.Errorf("a = %q, want %q", got, body)
	}
}

// TestHistoryWindowFromEmpty walks window()'s small-output path from an empty
// history, which is the first block of any folder whose blocks are short.
func TestHistoryWindowFromEmpty(t *testing.T) {
	fs := mustOpen(t, craft{
		folders: []cFolder{{blocks: []cBlock{{data: []byte("0123456789")}, {data: []byte("abcdefghij")}}}},
		files:   []cFile{{size: 20, name: []byte("a")}},
	})
	if got := readFile(t, fs, "a"); string(got) != "0123456789abcdefghij" {
		t.Errorf("a = %q", got)
	}
	if got := window(nil, []byte("xy")); string(got) != "xy" {
		t.Errorf("window(nil, xy) = %q", got)
	}
	long := bytes.Repeat([]byte{7}, maxBlockUncomp+5)
	if got := window([]byte("prior"), long); len(got) != maxBlockUncomp || !bytes.Equal(got, long[5:]) {
		t.Errorf("window with an over-long block kept %d bytes", len(got))
	}
}

// TestChecksumRemainder pins the trailing-byte order, which is the one part of
// the cabinet checksum that is not a plain little-endian XOR.
//
// ⛔ Measured against gcab's own cabinets: folding the leftover bytes
// little-endian agrees for every cbData that is a multiple of four -- 12 of 29
// blocks in the first fixture tried -- and disagrees for all the rest. A test
// whose inputs all happened to be multiples of four would have passed.
func TestChecksumRemainder(t *testing.T) {
	data := []byte{0x11, 0x22, 0x33, 0x44, 0xAA, 0xBB, 0xCC}
	for _, tc := range []struct {
		n    int
		want uint32
	}{
		{0, 0},
		{1, 0x00000011},
		{2, 0x00001122},
		{3, 0x00112233},
		{4, 0x44332211},
		{5, 0x443322BB}, // 0x44332211 ^ 0xAA
		{6, 0x443388AA}, // 0x44332211 ^ 0xAABB
		{7, 0x449999DD}, // 0x44332211 ^ 0xAABBCC
	} {
		if got := checksum(data[:tc.n], 0); got != tc.want {
			t.Errorf("checksum(%#x) = %#08x, want %#08x", data[:tc.n], got, tc.want)
		}
	}
	// And the seed is folded in, which is how the CFDATA header's own two
	// fields reach the sum.
	if got := checksum(nil, 0x1234); got != 0x1234 {
		t.Errorf("checksum(nil, seed) = %#x", got)
	}
	// Every recorded checksum in every fixture agrees. That is the real
	// assertion: the algorithm is not ours to choose.
	for _, f := range []string{fxMSZIP, fxStore, fxPaths, fxReserve} {
		b := fixture(t, f)
		fs := openFixture(t, f)
		for i, blk := range fs.folders[0].blocks {
			raw := b[blk.dataOff : blk.dataOff+int64(blk.cbData)]
			got := checksum(raw, uint32(blk.cbData)|uint32(blk.cbUncomp)<<16)
			if got != blk.csum {
				t.Errorf("%s block %d (cbData %d, %d mod 4): checksum %#08x, recorded %#08x",
					f, i, blk.cbData, blk.cbData%4, got, blk.csum)
			}
		}
	}
}

// TestWindows1252Names covers the non-UTF-8 half of the name decoder, including
// the five byte values Windows-1252 leaves undefined.
func TestWindows1252Names(t *testing.T) {
	// 0x80 EURO SIGN, 0x92 RIGHT SINGLE QUOTE, 0x81 undefined, 0xE9 e-acute.
	name := []byte{'c', 'a', 'f', 0xE9, 0x80, 0x92, 0x81, '.', 't', 'x', 't'}
	fs := mustOpen(t, craft{
		folders: []cFolder{{blocks: []cBlock{{data: []byte("x")}}}},
		files:   []cFile{{size: 1, name: name}},
	})
	want := "caf\u00e9\u20ac\u2019\ufffd.txt"
	if _, err := fs.Stat(want); err != nil {
		var have []string
		for p := range fs.byPath {
			have = append(have, p)
		}
		t.Errorf("Stat(%q) = %v; the cabinet holds %q", want, err, have)
	}
	// The whole 0xA0..0xFF range passes through as the code point of the same
	// value, which is where Windows-1252 and Latin-1 agree.
	if got := decodeWindows1252([]byte{0xA0, 0xFF}); got != " ÿ" {
		t.Errorf("decodeWindows1252 = %q", got)
	}
}

// TestReadFileTolerance: ReadFile stops at io.EOF rather than reporting it, so a
// whole-file read of the last entry in a folder is not an error.
func TestReadFileTolerance(t *testing.T) {
	fs := openFixture(t, fxStore)
	h, err := fs.OpenFile("small.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	buf := make([]byte, 100)
	n, err := h.ReadAt(buf, 0)
	if n != 14 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt over-long = %d, %v; want 14, io.EOF", n, err)
	}
	if got := readFile(t, fs, "small.txt"); string(got) != "hello cabinet\n" {
		t.Errorf("ReadFile = %q", got)
	}
}

// TestFolderStreamEndsAtItsLastBlock reads a folder's whole stream directly, so
// that the end of the block chain is observed as io.EOF rather than inferred.
//
// Every public read is bounded by an entry's declared size, which parse checked
// against the folder's own block table -- so the end of the chain is not
// reachable through ReadFile or ReadAt, and would otherwise go untested.
func TestFolderStreamEndsAtItsLastBlock(t *testing.T) {
	for _, f := range []string{fxMSZIP, fxStore} {
		fs := openFixture(t, f)
		fr := &folderReader{fs: fs, idx: 0}
		fr.reset(0)
		n, err := io.Copy(io.Discard, fr)
		if !errors.Is(err, nil) {
			t.Fatalf("%s: copying the folder stream: %v", f, err)
		}
		if n != fs.folders[0].uncomp {
			t.Errorf("%s: folder 0 yielded %d bytes, its block table says %d", f, n, fs.folders[0].uncomp)
		}
		if _, err := fr.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("%s: a read past the last block = %v, want io.EOF", f, err)
		}
	}
}

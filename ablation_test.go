// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// The ablation suite: for each decision this reader makes, UNDO it and check
// that the corpus notices.
//
// An ablation that PASSES is the finding, not the failure -- it says the fixture
// cannot see the mistake, so the test guarding it is decoration. Every case here
// reports which way it went, and TestAblationTable prints the lot.
//
// Two shapes of ablation appear below, and they are not interchangeable:
//
//   - PATCH the input so the cabinet describes itself the way a forgetful reader
//     would read it (reserve sizes set to 0, an attribute bit cleared, a
//     signature byte changed). Same length, so no offset in the file moves.
//   - REIMPLEMENT the step wrongly here in the test, and compare against the
//     reference bytes. Nothing in the package is switchable for a test's
//     benefit; the wrong version lives in the test that needs it.

type ablation struct {
	name     string
	undoes   string
	detected bool
	expected bool
	how      string
}

var ablations []ablation

// record files one ablation's outcome.
//
// It takes what the outcome is EXPECTED to be, and fails either way round. An
// ablation that was meant to be caught and was not says the guard is decoration;
// an ablation documented as invisible to a fixture that suddenly becomes visible
// says the note above it is stale, which is the same defect a year later.
func record(t *testing.T, name, undoes string, detected bool, how string) {
	t.Helper()
	recordExpecting(t, name, undoes, detected, true, how)
}

func recordExpecting(t *testing.T, name, undoes string, detected, expected bool, how string) {
	t.Helper()
	ablations = append(ablations, ablation{name, undoes, detected, expected, how})
	switch {
	case expected && !detected:
		t.Errorf("ABLATION PASSES: %s -- %s. The corpus cannot see this mistake.", name, how)
	case !expected && detected:
		t.Errorf("ABLATION NOW CAUGHT: %s -- %s. It was documented as invisible to this fixture; fix the note.", name, how)
	}
}

// layout finds the fields an in-place patch has to reach.
type cabLayout struct {
	reserveSizes int // offset of cbCFHeader; -1 when the header has no reserve area
	folders      []int
	files        []int // offset of each CFFILE's fixed part
	names        []int // offset of each CFFILE's name bytes
	blocks       []int // offset of each CFDATA header of folder 0
	cbCFData     int
}

func layout(t *testing.T, b []byte) cabLayout {
	t.Helper()
	l := cabLayout{reserveSizes: -1}
	cFolders := int(le16(b[26:]))
	cFiles := int(le16(b[28:]))
	flags := le16(b[30:])
	off := hdrFixedSize
	resFolder, resData := 0, 0
	if flags&flagReservePresent != 0 {
		l.reserveSizes = off
		resFolder, resData = int(b[off+2]), int(b[off+3])
		l.cbCFData = resData
		off += 4 + int(le16(b[off:]))
	}
	for i := 0; i < 2; i++ {
		if flags&(1<<i) != 0 {
			for j := 0; j < 2; j++ {
				off += bytes.IndexByte(b[off:], 0) + 1
			}
		}
	}
	var coffFolder0 int
	for i := 0; i < cFolders; i++ {
		l.folders = append(l.folders, off)
		if i == 0 {
			coffFolder0 = int(le32(b[off:]))
		}
		off += folderFixedSize + resFolder
	}
	off = int(le32(b[16:]))
	for i := 0; i < cFiles; i++ {
		l.files = append(l.files, off)
		l.names = append(l.names, off+fileFixedSize)
		off += fileFixedSize + bytes.IndexByte(b[off+fileFixedSize:], 0) + 1
	}
	o := coffFolder0
	for i := 0; i < int(le16(b[l.folders[0]+4:])); i++ {
		l.blocks = append(l.blocks, o)
		o += dataFixedSize + resData + int(le16(b[o+4:]))
	}
	return l
}

// readOrErr reads a path from patched bytes, returning either the bytes or the
// error, so an ablation can say which of the two ways it was detected.
func readOrErr(b []byte, p string) ([]byte, error) {
	fs, err := Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(p)
}

// --- 1. the three reserve sizes --------------------------------------------

// TestAblateReserveSizes sets each reserve size to zero in turn, which is
// exactly what a reader that forgot that field would do with the rest of the
// file.
func TestAblateReserveSizes(t *testing.T) {
	b := fixture(t, fxReserve)
	l := layout(t, b)
	if l.reserveSizes < 0 {
		t.Fatal("the reserve fixture has no reserve area: the premise is gone")
	}
	want := readFile(t, openFixture(t, fxReserve), "big.txt")
	if sum(want) != sumBig {
		t.Fatalf("the reserve fixture does not hold the expected payload")
	}
	cases := []struct {
		name  string
		patch patch
	}{
		{"cbCFHeader ignored", patch{l.reserveSizes, []byte{0, 0}}},
		{"cbCFFolder ignored", patch{l.reserveSizes + 2, []byte{0}}},
		{"cbCFData ignored", patch{l.reserveSizes + 3, []byte{0}}},
		{"all three ignored", patch{l.reserveSizes, []byte{0, 0, 0, 0}}},
	}
	for _, tc := range cases {
		got, err := readOrErr(apply(t, b, tc.patch), "big.txt")
		detected, how := judge(want, got, err)
		record(t, "reserve/"+tc.name, "the reserve area each size inserts into every later structure", detected, how)
	}
}

// judge says whether an ablation was caught, and by what.
func judge(want, got []byte, err error) (bool, string) {
	switch {
	case err != nil:
		return true, "refused: " + firstLine(err.Error())
	case !bytes.Equal(want, got):
		return true, fmt.Sprintf("wrong bytes: %d returned, first difference at %d", len(got), firstDiff(want, got))
	default:
		return false, "the same bytes came back"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// --- 2. the backslash separator --------------------------------------------

// TestAblateBackslashConversion leaves the separator alone, which is what a
// reader that takes a CFFILE name at face value does.
func TestAblateBackslashConversion(t *testing.T) {
	b := fixture(t, fxPaths)
	l := layout(t, b)
	raw := b[l.names[1] : l.names[1]+bytes.IndexByte(b[l.names[1]:], 0)]
	if !bytes.Contains(raw, []byte(`\`)) {
		t.Fatalf("entry 1 is named %q and holds no backslash: the ablation has nothing to undo", raw)
	}
	fs := openFixture(t, fxPaths)

	// The ablated name: no separator conversion.
	ablated := string(raw)
	_, err := fs.Stat(ablated)
	detected := err != nil
	how := "the unconverted name is not a path this filesystem answers to"
	if !detected {
		how = fmt.Sprintf("%q resolves anyway", ablated)
	}
	record(t, "names/backslash left alone", "converting the CFFILE separator to '/'", detected, how)

	// And the other half: no path in the filesystem carries a backslash, and
	// the converted one is reachable.
	for p := range fs.byPath {
		if strings.Contains(p, `\`) {
			t.Errorf("the filesystem holds %q, which still carries a backslash", p)
		}
	}
	if _, err := fs.Stat("sub/deep/nested.txt"); err != nil {
		t.Errorf("the converted path does not resolve: %v", err)
	}
}

// --- 3. the MSZIP history window -------------------------------------------

// TestAblateCrossBlockHistory decodes every block with an EMPTY dictionary,
// which is what flate.NewReader (rather than NewReaderDict) gives you.
//
// ⛔ It runs over BOTH MSZIP fixtures, and they answer differently. gcab's own
// compressor never emits a back-reference into the preceding block, so
// mszip.cab -- eight blocks, 250 KB, a folder by every measure large enough --
// decodes perfectly with the window thrown away. Size was not the property that
// mattered; the WRITER was. history.cab exists because of that, and its blocks
// 1..7 are 262 bytes each precisely because they are almost nothing but
// references into the block before.
func TestAblateCrossBlockHistory(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		ref     func(*testing.T) []byte
		sees    bool
		note    string
	}{
		{fxMSZIP, func(t *testing.T) []byte { return readFile(t, openFixture(t, fxStore), "big.txt") }, false,
			"gcab emits no cross-block back-references, so this fixture cannot see it"},
		{fxHistory, func(t *testing.T) []byte { return histPayload(t) }, true, ""},
	} {
		b := fixture(t, tc.fixture)
		fs := openFixture(t, tc.fixture)
		fol := &fs.folders[0]
		if len(fol.blocks) < 2 {
			t.Fatalf("%s spans one block: cross-block history cannot be ablated", tc.fixture)
		}
		want := tc.ref(t)

		// The ablated decoder: a fresh DEFLATE stream per block, no dictionary.
		var out bytes.Buffer
		var failed error
		for i, blk := range fol.blocks {
			raw := b[blk.dataOff : blk.dataOff+int64(blk.cbData)]
			dec, err := inflateMSZIP(raw, nil, int(blk.cbUncomp))
			if err != nil {
				failed = fmt.Errorf("block %d: %w", i, err)
				break
			}
			out.Write(dec)
		}
		got := out.Bytes()
		if len(got) > len(want) {
			got = got[:len(want)]
		}
		detected, how := judge(want, got, failed)
		if tc.note != "" {
			how += " -- " + tc.note
		}
		recordExpecting(t, "mszip/history dropped per block ("+tc.fixture+")",
			"seeding each block with the preceding 32 KiB", detected, tc.sees, how)

		// The positive control: with the window, the folder matches the
		// reference byte for byte.
		var hist []byte
		var good bytes.Buffer
		for i, blk := range fol.blocks {
			raw := b[blk.dataOff : blk.dataOff+int64(blk.cbData)]
			dec, err := inflateMSZIP(raw, hist, int(blk.cbUncomp))
			if err != nil {
				t.Fatalf("%s block %d with the window: %v", tc.fixture, i, err)
			}
			good.Write(dec)
			hist = window(hist, dec)
		}
		if !bytes.Equal(good.Bytes()[:len(want)], want) {
			t.Fatalf("%s: the control decode does not match the reference", tc.fixture)
		}
	}
}

// histPayload is history.cab's payload, taken from the package's own reader --
// which is circular on its own, so its sha256 is pinned and three outside
// readers were made to agree on the same bytes before it was committed.
func histPayload(t *testing.T) []byte {
	t.Helper()
	b := readFile(t, openFixture(t, fxHistory), "hist.bin")
	if len(b) != sizeHist || sum(b) != sumHist {
		t.Fatalf("history.cab yields %d bytes sum %s, want %d / %s", len(b), sum(b), sizeHist, sumHist)
	}
	return b
}

// TestAblateHistorySizes narrows it: how much of the window does the payload
// actually need? A reader that keeps too little fails in the same silent way as
// one that keeps none.
func TestAblateHistorySizes(t *testing.T) {
	b := fixture(t, fxHistory)
	fs := openFixture(t, fxHistory)
	fol := &fs.folders[0]
	want := histPayload(t)
	for _, keep := range []int{0, 1024, 8192, 16384, maxBlockUncomp - 1, maxBlockUncomp} {
		var hist, out []byte
		var failed error
		for i, blk := range fol.blocks {
			raw := b[blk.dataOff : blk.dataOff+int64(blk.cbData)]
			d := hist
			if keep < len(d) {
				d = d[len(d)-keep:]
			}
			dec, err := inflateMSZIP(raw, d, int(blk.cbUncomp))
			if err != nil {
				failed = fmt.Errorf("block %d: %w", i, err)
				break
			}
			out = append(out, dec...)
			hist = window(hist, dec)
		}
		if keep == maxBlockUncomp {
			if failed != nil || !bytes.Equal(out, want) {
				t.Fatalf("the full window must reproduce the payload: %v", failed)
			}
			continue
		}
		if len(out) > len(want) {
			out = out[:len(want)]
		}
		detected, how := judge(want, out, failed)
		record(t, fmt.Sprintf("mszip/window truncated to %d bytes", keep), "keeping the whole 32 KiB window", detected, how)
	}
}

// --- 4. uoffFolderStart ----------------------------------------------------

// TestAblateUoffFolderStart reads the folder stream from its beginning instead
// of from the entry's own offset.
func TestAblateUoffFolderStart(t *testing.T) {
	for _, f := range []string{fxMSZIP, fxStore} {
		fs := openFixture(t, f)
		e := fs.byPath["small.txt"]
		if e.off == 0 {
			t.Fatalf("%s: small.txt starts at folder offset 0: nothing to ablate", f)
		}
		want := readFile(t, fs, "small.txt")

		fr := &folderReader{fs: fs, idx: e.folder}
		fr.reset(0)
		got := make([]byte, e.size)
		if _, err := io.ReadFull(fr, got); err != nil {
			t.Fatalf("%s: reading from folder offset 0: %v", f, err)
		}
		detected, how := judge(want, got, nil)
		record(t, "entries/uoffFolderStart ignored ("+f+")", "seeking to the entry's offset inside its folder", detected, how)
	}
}

// --- 5. the CK prefix ------------------------------------------------------

// TestAblateCKPrefix does it both ways: a cabinet whose signature is wrong must
// be refused, and a decoder that feeds those two bytes to DEFLATE must fail.
func TestAblateCKPrefix(t *testing.T) {
	b := fixture(t, fxMSZIP)
	l := layout(t, b)
	dataOff := l.blocks[0] + dataFixedSize + l.cbCFData
	want := readFile(t, openFixture(t, fxMSZIP), "big.txt")

	// (a) the signature is not there: the reader must say so.
	got, err := readOrErr(apply(t, b, patch{dataOff, []byte("X")}), "big.txt")
	detected, how := judge(want, got, err)
	record(t, "mszip/CK signature corrupted", "checking the two-byte block signature", detected, how)

	// (a2) the same, with the block's checksum zeroed so that the CHECKSUM
	// cannot be what notices. Case (a) is caught by the checksum first, which
	// makes it no test of the signature check at all.
	got, err = readOrErr(apply(t, b,
		patch{l.blocks[0], []byte{0, 0, 0, 0}},
		patch{dataOff, []byte("X")},
	), "big.txt")
	detected, how = judge(want, got, err)
	record(t, "mszip/CK corrupted, checksum zeroed", "checking the block signature independently of the checksum", detected, how)

	// (b) the two bytes are left in the DEFLATE stream.
	fs := openFixture(t, fxMSZIP)
	blk := fs.folders[0].blocks[0]
	raw := b[blk.dataOff : blk.dataOff+int64(blk.cbData)]
	zr := flate.NewReaderDict(bytes.NewReader(raw), nil) // no raw[2:]
	defer zr.Close()
	buf := make([]byte, blk.cbUncomp)
	n, zerr := io.ReadFull(zr, buf)
	ablated := buf[:n]
	detected, how = judge(want[:min(len(want), int(blk.cbUncomp))], ablated, zerr)
	record(t, "mszip/CK fed to the inflater", "skipping the two signature bytes", detected, how)
}

// --- 6. UTF-8 versus Windows-1252 ------------------------------------------

// TestAblateNameEncoding flips the _A_NAME_IS_UTF bit both ways on names that
// are not plain ASCII.
func TestAblateNameEncoding(t *testing.T) {
	// (a) A UTF-8 name read as Windows-1252: paths.cab's third entry.
	b := fixture(t, fxPaths)
	l := layout(t, b)
	attrOff := l.files[2] + 14
	if le16(b[attrOff:])&attrNameIsUTF8 == 0 {
		t.Fatal("entry 2 is not flagged UTF-8: nothing to ablate")
	}
	cleared := le16(b[attrOff:]) & ^uint16(attrNameIsUTF8)
	var p [2]byte
	binary.LittleEndian.PutUint16(p[:], cleared)
	fs, err := Open(bytes.NewReader(apply(t, b, patch{attrOff, p[:]})), int64(len(b)))
	if err != nil {
		record(t, "names/UTF-8 read as Windows-1252", "the _A_NAME_IS_UTF bit choosing the encoding", true, "refused: "+firstLine(err.Error()))
	} else {
		_, missErr := fs.Stat("sub/café.txt")
		detected := missErr != nil
		how := "the name no longer resolves: it decodes as " + mojibake(fs)
		if !detected {
			how = "sub/café.txt resolves under either encoding"
		}
		record(t, "names/UTF-8 read as Windows-1252", "the _A_NAME_IS_UTF bit choosing the encoding", detected, how)
	}

	// (b) A Windows-1252 name read as UTF-8. mszip.cab's "small.txt" is nine
	// bytes, and so is "caf\xe9.txt", so the name is replaced in place.
	mb := fixture(t, fxMSZIP)
	ml := layout(t, mb)
	nameOff := ml.names[1]
	if got := mb[nameOff : nameOff+9]; string(got) != "small.txt" {
		t.Fatalf("entry 1 is named %q, not small.txt", got)
	}
	latin := apply(t, mb, patch{nameOff, []byte("caf\xe9.txt\x00")})
	fs1252, err := Open(bytes.NewReader(latin), int64(len(latin)))
	if err != nil {
		t.Fatalf("a Windows-1252 name must be readable: %v", err)
	}
	if _, err := fs1252.Stat("café.txt"); err != nil {
		t.Errorf("the Windows-1252 name did not decode to café.txt: %v", err)
	}
	// Now claim it is UTF-8. 0xE9 alone is not valid UTF-8, so the reader must
	// refuse rather than invent a name.
	utfAttr := le16(mb[ml.files[1]+14:]) | attrNameIsUTF8
	binary.LittleEndian.PutUint16(p[:], utfAttr)
	_, err = Open(bytes.NewReader(apply(t, latin, patch{ml.files[1] + 14, p[:]})), int64(len(latin)))
	detected := errors.Is(err, ErrCorrupt)
	how := "refused as invalid UTF-8"
	if !detected {
		how = fmt.Sprintf("accepted a name that is not UTF-8 (err %v)", err)
	}
	record(t, "names/Windows-1252 read as UTF-8", "validating a name flagged UTF-8", detected, how)
}

func mojibake(fs *FS) string {
	var names []string
	for p := range fs.byPath {
		if strings.HasPrefix(p, "sub/") {
			names = append(names, p)
		}
	}
	return strings.Join(names, ", ")
}

// --- 7. the block checksum -------------------------------------------------

// TestAblateChecksum flips a byte inside a block, and separately folds the
// checksum's trailing bytes the wrong way round.
func TestAblateChecksum(t *testing.T) {
	b := fixture(t, fxStore)
	l := layout(t, b)
	dataOff := l.blocks[0] + dataFixedSize + l.cbCFData
	want := readFile(t, openFixture(t, fxStore), "big.txt")
	flipped := apply(t, b, patch{dataOff + 100, []byte{b[dataOff+100] ^ 0xFF}})
	got, err := readOrErr(flipped, "big.txt")
	detected, how := judge(want, got, err)
	record(t, "blocks/one byte flipped", "verifying the CFDATA checksum", detected, how)

	// ⛔ The remainder ablation. Folding the trailing 1..3 bytes little-endian
	// is the obvious reading of the specification's reference code and it is
	// wrong; it agrees for every cbData that is a multiple of four.
	var agree, disagree [4]int
	for _, f := range []string{fxMSZIP, fxStore, fxPaths, fxReserve, fxHistory} {
		fb := fixture(t, f)
		fs := openFixture(t, f)
		for i := range fs.folders {
			for _, blk := range fs.folders[i].blocks {
				raw := fb[blk.dataOff : blk.dataOff+int64(blk.cbData)]
				seed := uint32(blk.cbData) | uint32(blk.cbUncomp)<<16
				if checksum(raw, seed) != blk.csum {
					t.Fatalf("%s: the correct fold disagrees with a recorded checksum", f)
				}
				if checksumLittleEndianRemainder(raw, seed) == blk.csum {
					agree[len(raw)%4]++
				} else {
					disagree[len(raw)%4]++
				}
			}
		}
	}
	total := 0
	for i := 0; i < 4; i++ {
		total += agree[i] + disagree[i]
	}
	detected = disagree[0]+disagree[1]+disagree[2]+disagree[3] > 0
	// Broken down by cbData mod 4, because that is the property that decides
	// it: remainders of 0 and 1 bytes fold the same way under both rules, so a
	// corpus made only of those cannot tell the two apart however large it is.
	how = fmt.Sprintf("of %d blocks, by cbData mod 4: {0: %d agree/%d differ} {1: %d/%d} {2: %d/%d} {3: %d/%d}",
		total, agree[0], disagree[0], agree[1], disagree[1], agree[2], disagree[2], agree[3], disagree[3])
	if !detected {
		how = fmt.Sprintf("every one of %d blocks agrees under both folds: %s", total, how)
	}
	record(t, "checksum/trailing bytes folded little-endian", "folding the trailing bytes high-byte-first", detected, how)
}

// checksumLittleEndianRemainder is checksum with the remainder folded the wrong
// way round. It exists only to be shown to disagree.
func checksumLittleEndianRemainder(b []byte, seed uint32) uint32 {
	sum := seed
	n := len(b) / 4 * 4
	for i := 0; i < n; i += 4 {
		sum ^= binary.LittleEndian.Uint32(b[i:])
	}
	var ul uint32
	for i, c := range b[n:] {
		ul |= uint32(c) << (8 * i)
	}
	return sum ^ ul
}

// --- the table -------------------------------------------------------------

// TestAblationTable prints what every ablation above did. It runs last because
// Go runs tests in source order within a file, and the ablations live in the
// files above and below it by name -- so it re-reports rather than measures.
func TestZZZAblationTable(t *testing.T) {
	if len(ablations) == 0 {
		t.Skip("no ablation ran (a -run filter?)")
	}
	t.Log("ablation                                                  | detected           | how")
	for _, a := range ablations {
		mark := "yes"
		switch {
		case !a.detected && a.expected:
			mark = "NO"
		case !a.detected:
			mark = "no (as documented)"
		}
		t.Logf("%-57s | %-18s | %s", a.name, mark, a.how)
	}
}

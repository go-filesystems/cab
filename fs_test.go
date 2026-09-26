// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestPremise asserts what the fixtures ARE before anything judges what the
// decoder makes of them.
//
// ⛔ A writer asked for MSZIP stores a small entry anyway, and a corpus of small
// entries passes every method while only None ever runs. So the method recorded
// in the CFFOLDER is read back from the cabinet, and the payload is large enough
// that its folder spans several CFDATA blocks -- without which the cross-block
// DEFLATE history is never exercised at all.
func TestPremise(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		method  Compression
	}{
		{fxMSZIP, CompressionMSZIP},
		{fxStore, CompressionNone},
		{fxPaths, CompressionMSZIP},
		{fxReserve, CompressionMSZIP},
	} {
		fs := openFixture(t, tc.fixture)
		got, err := fs.Compression("big.txt")
		if err != nil {
			t.Fatalf("%s: Compression: %v", tc.fixture, err)
		}
		if got != tc.method {
			t.Errorf("%s: CFFOLDER records %s, the fixture is meant to use %s", tc.fixture, got, tc.method)
		}
		if n := len(fs.folders[0].blocks); n < 2 {
			t.Errorf("%s: folder 0 has %d CFDATA block(s); a single block cannot exercise cross-block history", tc.fixture, n)
		}
		st, err := fs.Stat("big.txt")
		if err != nil {
			t.Fatalf("%s: Stat: %v", tc.fixture, err)
		}
		if st.Size() != sizeBig {
			t.Errorf("%s: big.txt is %d bytes, the fixture is meant to hold %d", tc.fixture, st.Size(), sizeBig)
		}
	}
	// paths.cab must actually contain a backslash-separated name and a name
	// flagged UTF-8, or the conversions they test are never reached.
	b := fixture(t, fxPaths)
	if !bytes.Contains(b, []byte(`sub\deep\nested.txt`)) {
		t.Error("paths.cab holds no backslash-separated name; the separator conversion is untested")
	}
	fs := openFixture(t, fxPaths)
	at, err := fs.Attributes("sub/café.txt")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if at&attrNameIsUTF8 == 0 {
		t.Errorf("sub/café.txt attribs %#x has no _A_NAME_IS_UTF bit; the UTF-8 path is untested", at)
	}
}

// TestBytes asserts BYTES per file, never counts.
func TestBytes(t *testing.T) {
	for _, f := range []string{fxMSZIP, fxStore, fxReserve} {
		fs := openFixture(t, f)
		if got := sum(readFile(t, fs, "big.txt")); got != sumBig {
			t.Errorf("%s big.txt sha256 %s, want %s", f, got, sumBig)
		}
		if got := readFile(t, fs, "small.txt"); string(got) != "hello cabinet\n" {
			t.Errorf("%s small.txt = %q", f, got)
		}
		if got := sum(readFile(t, fs, "small.txt")); got != sumSmall {
			t.Errorf("%s small.txt sha256 %s, want %s", f, got, sumSmall)
		}
	}
	fs := openFixture(t, fxPaths)
	if got := readFile(t, fs, "sub/deep/nested.txt"); string(got) != "nested content here\n" {
		t.Errorf("nested.txt = %q", got)
	} else if sum(got) != sumNested {
		t.Errorf("nested.txt sha256 mismatch")
	}
	if got := readFile(t, fs, "sub/café.txt"); string(got) != "café naïve\n" {
		t.Errorf("café.txt = %q", got)
	} else if sum(got) != sumCafe {
		t.Errorf("café.txt sha256 mismatch")
	}
}

// TestStoredAndInflatedAgree is the witness for MSZIP.
//
// The same payload is in two cabinets, one STORED and one MSZIP-compressed, and
// the stored copy is a reference this package did not produce: if the inflated
// bytes match it byte for byte -- across eight blocks, with the history window
// carrying between them -- then the decoder agrees with gcab's compressor.
func TestStoredAndInflatedAgree(t *testing.T) {
	stored := openFixture(t, fxStore)
	zipped := openFixture(t, fxMSZIP)
	if m, _ := stored.Compression("big.txt"); m != CompressionNone {
		t.Fatalf("the stored fixture is %s", m)
	}
	if m, _ := zipped.Compression("big.txt"); m != CompressionMSZIP {
		t.Fatalf("the compressed fixture is %s", m)
	}
	a := readFile(t, stored, "big.txt")
	b := readFile(t, zipped, "big.txt")
	if !bytes.Equal(a, b) {
		t.Fatalf("inflated bytes differ from the stored copy: first difference at %d", firstDiff(a, b))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestOpenerRandomAccess exercises File.ReadAt across block boundaries, in both
// directions, and at the edges io.ReaderAt's contract talks about.
func TestOpenerRandomAccess(t *testing.T) {
	for _, f := range []string{fxMSZIP, fxStore} {
		fs := openFixture(t, f)
		want := readFile(t, fs, "big.txt")
		h, err := fs.OpenFile("big.txt")
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer h.Close()
		if h.Size() != sizeBig {
			t.Errorf("%s: Size() = %d, want %d", f, h.Size(), sizeBig)
		}
		// Offsets chosen around the 32768-byte block boundary, plus the tail.
		for _, off := range []int64{0, 1, 32766, 32767, 32768, 32769, 65535, 65536, 65537, 250010} {
			for _, n := range []int{1, 3, 4096, 40000} {
				buf := make([]byte, n)
				got, err := h.ReadAt(buf, off)
				end := min(off+int64(n), int64(len(want)))
				if int64(got) != end-off {
					t.Fatalf("%s: ReadAt(%d bytes, %d) = %d, want %d (err %v)", f, n, off, got, end-off, err)
				}
				if int64(got) < int64(n) && err == nil {
					t.Errorf("%s: ReadAt(%d, %d) short read with a nil error", f, n, off)
				}
				if !bytes.Equal(buf[:got], want[off:end]) {
					t.Errorf("%s: ReadAt(%d, %d) returned the wrong bytes", f, n, off)
				}
			}
		}
		// At and past the end: 0, io.EOF.
		if n, err := h.ReadAt(make([]byte, 8), sizeBig); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt at size = %d, %v; want 0, io.EOF", f, n, err)
		}
		if n, err := h.ReadAt(make([]byte, 8), sizeBig+99); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt past size = %d, %v; want 0, io.EOF", f, n, err)
		}
		if _, err := h.ReadAt(make([]byte, 4), -1); !errors.Is(err, ErrNegativeOffset) {
			t.Errorf("%s: ReadAt(-1) = %v, want ErrNegativeOffset", f, err)
		}
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		// A handle reopens its decoder after Close, because ReadAt is
		// positional and holds no promise about what came before it.
		if n, err := h.ReadAt(make([]byte, 4), 0); n != 4 || err != nil {
			t.Errorf("%s: ReadAt after Close = %d, %v", f, n, err)
		}
	}
}

// TestOpenerSeeksToUoffFolderStart reads the SECOND entry of a folder, which
// lives 250011 bytes into the stream: a reader that ignores uoffFolderStart
// hands back the first entry's opening bytes instead.
func TestOpenerSeeksToUoffFolderStart(t *testing.T) {
	for _, f := range []string{fxMSZIP, fxStore} {
		fs := openFixture(t, f)
		h, err := fs.OpenFile("small.txt")
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer h.Close()
		buf := make([]byte, 14)
		if _, err := h.ReadAt(buf, 0); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if string(buf) != "hello cabinet\n" {
			t.Errorf("%s: small.txt at folder offset %d = %q", f, fs.byPath["small.txt"].off, buf)
		}
	}
}

// TestConcurrentReadAt holds io.ReaderAt's concurrency clause: several ReadAt
// calls on one handle at once, checked under -race.
func TestConcurrentReadAt(t *testing.T) {
	fs := openFixture(t, fxMSZIP)
	want := readFile(t, fs, "big.txt")
	h, err := fs.OpenFile("big.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 4096
			buf := make([]byte, 1024)
			if n, err := h.ReadAt(buf, off); err != nil || n != 1024 {
				t.Errorf("ReadAt(%d) = %d, %v", off, n, err)
			} else if !bytes.Equal(buf, want[off:off+1024]) {
				t.Errorf("ReadAt(%d) returned the wrong bytes", off)
			}
		}(i)
	}
	wg.Wait()
}

func TestListDirAndStat(t *testing.T) {
	fs := openFixture(t, fxPaths)
	want := map[string][]string{
		".":        {"big.txt", "sub"},
		"sub":      {"café.txt", "deep"},
		"sub/deep": {"nested.txt"},
	}
	for dir, names := range want {
		got, err := fs.ListDir(dir)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		if len(got) != len(names) {
			t.Fatalf("ListDir(%q) has %d entries, want %v", dir, len(got), names)
		}
		for i, e := range got {
			if e.Name() != names[i] {
				t.Errorf("ListDir(%q)[%d] = %q, want %q", dir, i, e.Name(), names[i])
			}
			if e.Inode() == 0 {
				t.Errorf("ListDir(%q)[%d] inode is 0", dir, i)
			}
		}
	}
	// "" and "/" and "." all name the root.
	for _, root := range []string{"", "/", ".", "/./"} {
		if got, err := fs.ListDir(root); err != nil || len(got) != 2 {
			t.Errorf("ListDir(%q) = %v, %v", root, got, err)
		}
	}
	// A synthesised directory stats as one, with the type bits a uint16 can
	// carry.
	st, err := fs.Stat("sub/deep")
	if err != nil {
		t.Fatalf("Stat(sub/deep): %v", err)
	}
	if st.Mode() != modeDir|0o555 {
		t.Errorf("Stat(sub/deep).Mode() = %#o, want %#o", st.Mode(), modeDir|0o555)
	}
	if st.Size() != 0 {
		t.Errorf("Stat(sub/deep).Size() = %d", st.Size())
	}
	st, err = fs.Stat("big.txt")
	if err != nil {
		t.Fatalf("Stat(big.txt): %v", err)
	}
	if st.Mode() != modeRegular|0o444 {
		t.Errorf("Stat(big.txt).Mode() = %#o, want %#o", st.Mode(), modeRegular|0o444)
	}
	// A leading slash and a redundant component reach the same entry.
	if _, err := fs.Stat("/sub/./deep/../deep/nested.txt"); err != nil {
		t.Errorf("Stat through a dirty path: %v", err)
	}
}

// TestExecBitReachesTheMode covers the one attribute that changes st_mode.
func TestExecBitReachesTheMode(t *testing.T) {
	fs := mustOpen(t, craft{
		folders: []cFolder{{typeC: uint16(CompressionNone), blocks: stored([]byte("x"))}},
		files:   []cFile{{size: 1, name: []byte("run"), attribs: attrExec}},
	})
	st, err := fs.Stat("run")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode() != modeRegular|0o555 {
		t.Errorf("Mode() = %#o, want %#o", st.Mode(), modeRegular|0o555)
	}
}

func TestMetadataAccessors(t *testing.T) {
	fs := openFixture(t, fxReserve)
	if got := string(fs.Reserved()); got != "RESERVEHDR!!" {
		t.Errorf("Reserved() = %q", got)
	}
	if maj, minr := fs.Version(); maj != 1 || minr != 3 {
		t.Errorf("Version() = %d.%d, want 1.3", maj, minr)
	}
	if fs.FolderCount() != 2 {
		t.Errorf("FolderCount() = %d, want 2", fs.FolderCount())
	}
	if fs.SetID() != 0x1234 || fs.CabinetIndex() != 0 {
		t.Errorf("SetID/CabinetIndex = %#x/%d", fs.SetID(), fs.CabinetIndex())
	}
	// The second folder is stored where the first is compressed: one cabinet,
	// two methods, and the entry decides which decoder runs.
	if m, err := fs.Compression("second.txt"); err != nil || m != CompressionNone {
		t.Errorf("Compression(second.txt) = %s, %v", m, err)
	}
	if got := readFile(t, fs, "second.txt"); string(got) != "the second folder of this cabinet\n" {
		t.Errorf("second.txt = %q", got)
	}
	if fs.PrevCabinet() != "" || fs.NextCabinet() != "" || fs.PrevDisk() != "" || fs.NextDisk() != "" {
		t.Error("a single cabinet names neighbours")
	}
	mt, err := fs.ModTime("big.txt")
	if err != nil {
		t.Fatalf("ModTime: %v", err)
	}
	if got := mt.Format("2006-01-02 15:04:05 MST"); got != "2026-09-26 06:31:14 UTC" {
		t.Errorf("ModTime = %s", got)
	}
	// The set fields, and the neighbour names, from a crafted header.
	c := oneStoredFile("a.txt", []byte("a"))
	c.setID, c.index = 0x1234, 7
	c.prev = &[2]string{"first.cab", "Disk 1"}
	c.next = &[2]string{"third.cab", "Disk 3"}
	fs2 := mustOpen(t, c)
	if fs2.SetID() != 0x1234 || fs2.CabinetIndex() != 7 {
		t.Errorf("SetID/CabinetIndex = %#x/%d", fs2.SetID(), fs2.CabinetIndex())
	}
	if fs2.PrevCabinet() != "first.cab" || fs2.PrevDisk() != "Disk 1" {
		t.Errorf("prev = %q/%q", fs2.PrevCabinet(), fs2.PrevDisk())
	}
	if fs2.NextCabinet() != "third.cab" || fs2.NextDisk() != "Disk 3" {
		t.Errorf("next = %q/%q", fs2.NextCabinet(), fs2.NextDisk())
	}
	if fs2.Reserved() != nil {
		t.Errorf("Reserved() = %q with no reserve area", fs2.Reserved())
	}
}

// TestReadOnly: every mutating method refuses, and Close releases nothing.
func TestReadOnly(t *testing.T) {
	fs := openFixture(t, fxMSZIP)
	if err := fs.WriteFile("x", nil, 0o644); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteFile = %v", err)
	}
	if err := fs.MkDir("x", os.FileMode(0o755)); !errors.Is(err, ErrReadOnly) {
		t.Errorf("MkDir = %v", err)
	}
	if err := fs.DeleteFile("x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteFile = %v", err)
	}
	if err := fs.DeleteDir("x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteDir = %v", err)
	}
	if err := fs.Rename("x", "y"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Rename = %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// TestErrNotExistContract is the error contract this org owes its callers: a
// path that is not there returns something errors.Is can classify.
func TestErrNotExistContract(t *testing.T) {
	fs := openFixture(t, fxPaths)
	miss := "no/such/file"
	if _, err := fs.ReadFile(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("ReadFile: %v", err)
	}
	if _, err := fs.OpenFile(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("OpenFile: %v", err)
	}
	if _, err := fs.Stat(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("Stat: %v", err)
	}
	if _, err := fs.ListDir(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("ListDir: %v", err)
	}
	if _, err := fs.ReadLink(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("ReadLink: %v", err)
	}
	if _, err := fs.Compression(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("Compression: %v", err)
	}
	if _, err := fs.Attributes(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("Attributes: %v", err)
	}
	if _, err := fs.ModTime(miss); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("ModTime: %v", err)
	}
}

// TestKindRefusals: a directory is not a regular file, a regular file is not a
// directory, and nothing in a cabinet is a symbolic link.
func TestKindRefusals(t *testing.T) {
	fs := openFixture(t, fxPaths)
	if _, err := fs.ReadFile("sub"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(dir) = %v", err)
	}
	if _, err := fs.OpenFile("sub"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenFile(dir) = %v", err)
	}
	if _, err := fs.Compression("sub"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Compression(dir) = %v", err)
	}
	if _, err := fs.ListDir("big.txt"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("ListDir(file) = %v", err)
	}
	if _, err := fs.ReadLink("big.txt"); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("ReadLink = %v", err)
	}
	if _, err := fs.ReadLink("sub"); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("ReadLink(dir) = %v", err)
	}
}

// TestOpenReaderReturnsTheContract keeps the wiring the ecosystem opens every
// driver through.
func TestOpenReaderReturnsTheContract(t *testing.T) {
	b := fixture(t, fxMSZIP)
	var got filesystem.Filesystem
	got, err := OpenReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	o, ok := got.(filesystem.Opener)
	if !ok {
		t.Fatal("OpenReader's result does not implement filesystem.Opener")
	}
	h, err := o.OpenFile("small.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	buf := make([]byte, 14)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "hello cabinet\n" {
		t.Errorf("small.txt = %q", buf)
	}
	if _, err := OpenReader(bytes.NewReader([]byte("nope")), 4); !errors.Is(err, ErrNotCabinet) {
		t.Errorf("OpenReader on rubbish = %v", err)
	}
}

func TestCompressionString(t *testing.T) {
	for _, tc := range []struct {
		c    Compression
		want string
	}{
		{CompressionNone, "None"},
		{CompressionMSZIP, "MSZIP"},
		{CompressionQuantum, "Quantum"},
		{CompressionLZX, "LZX"},
		{Compression(9), "typeCompress(9)"},
	} {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Compression(%d).String() = %q, want %q", uint16(tc.c), got, tc.want)
		}
	}
}

// TestHistoryFixture reads the cross-block fixture through the public API, which
// is the path a caller takes: everything else about it is measured by driving
// inflateMSZIP directly.
func TestHistoryFixture(t *testing.T) {
	fs := openFixture(t, fxHistory)
	if m, err := fs.Compression("hist.bin"); err != nil || m != CompressionMSZIP {
		t.Fatalf("Compression = %s, %v", m, err)
	}
	if n := len(fs.folders[0].blocks); n != 8 {
		t.Errorf("folder 0 has %d blocks, want 8", n)
	}
	got := readFile(t, fs, "hist.bin")
	if len(got) != sizeHist || sum(got) != sumHist {
		t.Fatalf("hist.bin = %d bytes sum %s, want %d / %s", len(got), sum(got), sizeHist, sumHist)
	}
	// Every 32 KiB block of the payload is the same bytes, which is what makes
	// the back-references cross the boundary in the first place.
	for i := maxBlockUncomp; i < len(got); i += maxBlockUncomp {
		if !bytes.Equal(got[:maxBlockUncomp], got[i:i+maxBlockUncomp]) {
			t.Fatalf("block at %d differs from block 0: the fixture's premise is gone", i)
		}
	}
	// And a random-access read lands in the right place across a boundary.
	h, err := fs.OpenFile("hist.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	buf := make([]byte, 64)
	if _, err := h.ReadAt(buf, maxBlockUncomp*3+17); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, got[maxBlockUncomp*3+17:maxBlockUncomp*3+17+64]) {
		t.Error("ReadAt across a block boundary returned the wrong bytes")
	}
}

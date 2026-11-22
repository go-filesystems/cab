package cab

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// auditQuadraticCab builds a cabinet with ONE MSZIP folder of nBlocks blocks,
// each inflating to 32768 zero bytes, and nFiles one-byte files that all sit in
// the LAST block of the folder. It returns the bytes and the file offset the
// CFDATA area starts at.
func auditQuadraticCab(t testing.TB, nBlocks, nFiles int) ([]byte, int64) {
	t.Helper()
	var z bytes.Buffer
	z.WriteString("CK")
	fw, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(make([]byte, 32768)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	for z.Len() < 34 { // 32768 <= 1032*(cbData-2)
		z.WriteByte(0)
	}
	blk := z.Bytes()

	var files bytes.Buffer
	last := uint32(nBlocks*32768 - 1)
	for i := 0; i < nFiles; i++ {
		var f [16]byte
		binary.LittleEndian.PutUint32(f[0:], 1)     // cbFile
		binary.LittleEndian.PutUint32(f[4:], last)  // uoffFolderStart
		binary.LittleEndian.PutUint16(f[8:], 0)     // iFolder
		binary.LittleEndian.PutUint16(f[10:], 0x21) // date 1980-01-01
		files.Write(f[:])
		fmt.Fprintf(&files, "f%05d\x00", i)
	}

	const hdr, fol = 36, 8
	coffFiles := hdr + fol
	dataStart := coffFiles + files.Len()
	total := dataStart + nBlocks*(8+len(blk))

	out := make([]byte, 0, total)
	h := make([]byte, hdr)
	copy(h, "MSCF")
	binary.LittleEndian.PutUint32(h[8:], uint32(total))
	binary.LittleEndian.PutUint32(h[16:], uint32(coffFiles))
	h[24], h[25] = 3, 1
	binary.LittleEndian.PutUint16(h[26:], 1)
	binary.LittleEndian.PutUint16(h[28:], uint16(nFiles))
	out = append(out, h...)
	var fo [fol]byte
	binary.LittleEndian.PutUint32(fo[0:], uint32(dataStart))
	binary.LittleEndian.PutUint16(fo[4:], uint16(nBlocks))
	binary.LittleEndian.PutUint16(fo[6:], 1) // MSZIP
	out = append(out, fo[:]...)
	out = append(out, files.Bytes()...)
	for i := 0; i < nBlocks; i++ {
		var d [8]byte // csum 0 = not computed
		binary.LittleEndian.PutUint16(d[4:], uint16(len(blk)))
		binary.LittleEndian.PutUint16(d[6:], 32768)
		out = append(out, d[:]...)
		out = append(out, blk...)
	}
	return out, int64(dataStart)
}

// countingAt counts the ReadAt calls that land in the CFDATA area with more
// than a header's worth of bytes: one per block inflated.
type countingAt struct {
	r         io.ReaderAt
	dataStart int64
	blocks    atomic.Int64
}

func (c *countingAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.dataStart && len(p) > 8 {
		c.blocks.Add(1)
	}
	return c.r.ReadAt(p, off)
}

// TestAuditQuadraticFolderRestart reads every member of the cabinet once, the
// way an extractor does (OpenFile, read, Close). A linear reader inflates each
// block once: nBlocks inflations. HEAD inflates nBlocks*nFiles.
func TestAuditQuadraticFolderRestart(t *testing.T) {
	const nBlocks, nFiles = 400, 400
	data, dataStart := auditQuadraticCab(t, nBlocks, nFiles)
	ca := &countingAt{r: bytes.NewReader(data), dataStart: dataStart}
	fsys, err := OpenReader(ca, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	afterOpen := ca.blocks.Load()

	entries, err := fsys.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != nFiles {
		t.Fatalf("listed %d entries, want %d", len(entries), nFiles)
	}
	start := time.Now()
	var written int64
	for _, e := range entries {
		h, err := fsys.(filesystem.Opener).OpenFile("/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, io.NewSectionReader(h, 0, h.Size()))
		if err != nil {
			t.Fatal(err)
		}
		written += n
		h.Close()
	}
	elapsed := time.Since(start)
	inflated := ca.blocks.Load() - afterOpen
	t.Logf("cabinet %d bytes, %d blocks, %d files: %d bytes extracted, %d block inflations (%d bytes inflated), %v",
		len(data), nBlocks, nFiles, written, inflated, inflated*32768, elapsed)
	t.Logf("extrapolated to the format maximum (65535 blocks x 65535 files, a ~%d MiB cabinet): %.0f hours at this rate",
		(65535*(8+len(data)/nBlocks)+65535*23)>>20,
		elapsed.Hours()*(65535.0*65535.0)/float64(nBlocks*nFiles))
	if inflated > int64(4*(nBlocks+nFiles)) {
		t.Fatalf("extracting %d bytes cost %d block inflations; a single pass over the folder is %d",
			written, inflated, nBlocks)
	}
}

// TestCheckpointCacheIsBounded asserts the cache never exceeds its documented
// cap, however many states are offered.
func TestCheckpointCacheIsBounded(t *testing.T) {
	var c checkpoints
	for i := 0; i < 10*maxCheckpoints; i++ {
		c.put(i%7, i, int64(i), nil)
	}
	if len(c.items) != maxCheckpoints {
		t.Fatalf("cache holds %d states, cap is %d", len(c.items), maxCheckpoints)
	}
}

func TestCheckpointNearestAndDedup(t *testing.T) {
	var c checkpoints
	if _, ok := c.nearest(0, 1<<40); ok {
		t.Fatal("empty cache returned a state")
	}
	c.put(0, 4, 100, []byte("a"))
	c.put(0, 4, 100, []byte("a"))
	c.put(0, 8, 200, []byte("b"))
	c.put(1, 12, 300, []byte("c"))
	if len(c.items) != 3 {
		t.Fatalf("duplicate put grew the cache to %d states, want 3", len(c.items))
	}
	if ck, ok := c.nearest(0, 250); !ok || ck.blk != 8 {
		t.Fatalf("nearest(0,250) = %+v %v, want block 8", ck, ok)
	}
	if ck, ok := c.nearest(0, 199); !ok || ck.blk != 4 {
		t.Fatalf("nearest(0,199) = %+v %v, want block 4", ck, ok)
	}
	if _, ok := c.nearest(0, 99); ok {
		t.Fatal("state past the target returned")
	}
	if _, ok := c.nearest(2, 1000); ok {
		t.Fatal("state of another folder returned")
	}
}

// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// On-disk sizes and limits, from the Microsoft Cabinet File Format
// specification. Each of the three "reserve" sizes ADDS to one of these.
const (
	hdrFixedSize    = 36    // CFHEADER up to and including iCabinet
	folderFixedSize = 8     // CFFOLDER: coffCabStart, cCFData, typeCompress
	fileFixedSize   = 16    // CFFILE up to and including attribs
	dataFixedSize   = 8     // CFDATA: csum, cbData, cbUncomp
	maxBlockUncomp  = 32768 // cbUncomp ceiling, and the MSZIP history size
	maxNameLen      = 255   // longest NUL-terminated name this reader accepts
)

// CFHEADER flags.
const (
	flagPrevCabinet    = 0x0001
	flagNextCabinet    = 0x0002
	flagReservePresent = 0x0004
)

// CFFILE attribute bits. Only the one that changes how the name is DECODED
// matters to this reader; the rest are reported through Attributes.
const (
	attrExec       = 0x40 // _A_EXEC
	attrNameIsUTF8 = 0x80 // _A_NAME_IS_UTF
)

// iFolder values above every real folder index mean the entry is continued
// from, or into, an adjacent cabinet of the same set.
const (
	folderContinuedFromPrev     = 0xFFFD
	folderContinuedToNext       = 0xFFFE
	folderContinuedPrevAndNext  = 0xFFFF
	folderFirstContinuationMark = folderContinuedFromPrev
)

// Compression is the method a CFFOLDER records in the low nibble of its
// typeCompress field.
type Compression uint16

// The four methods the format defines. This package reads the first two; see
// the package documentation for why it refuses the other two by name.
const (
	CompressionNone    Compression = 0
	CompressionMSZIP   Compression = 1
	CompressionQuantum Compression = 2
	CompressionLZX     Compression = 3
)

// String names the method, so an error can say which one it met.
func (c Compression) String() string {
	switch c {
	case CompressionNone:
		return "None"
	case CompressionMSZIP:
		return "MSZIP"
	case CompressionQuantum:
		return "Quantum"
	case CompressionLZX:
		return "LZX"
	default:
		return fmt.Sprintf("typeCompress(%d)", uint16(c))
	}
}

// cursor reads little-endian structures forwards from an io.ReaderAt, keeping
// the FIRST error it met and returning zeroed buffers afterwards. Every parse
// step can then be written straight through and checked once at the end.
type cursor struct {
	r     io.ReaderAt
	off   int64
	limit int64 // one past the last byte that belongs to the cabinet
	err   error
}

// at reads n bytes from off without moving the cursor, refusing to read past
// the cabinet's own declared end.
func (c *cursor) at(off int64, n int) []byte {
	b := make([]byte, n)
	if off+int64(n) > c.limit {
		c.err = fmt.Errorf("cab: a %d-byte structure at offset %d runs past the end of the %d-byte cabinet: %w", n, off, c.limit, ErrTruncated)
		return b
	}
	if _, err := c.r.ReadAt(b, off); err != nil {
		c.err = fmt.Errorf("cab: reading %d bytes at offset %d: %w", n, off, err)
		return b
	}
	return b
}

// read returns the next n bytes and advances.
func (c *cursor) read(n int) []byte {
	if c.err != nil {
		return make([]byte, n)
	}
	b := c.at(c.off, n)
	c.off += int64(n)
	return b
}

// cstring returns the next NUL-terminated byte string, without its NUL.
func (c *cursor) cstring() []byte {
	if c.err != nil {
		return nil
	}
	n := int64(maxNameLen + 1)
	if c.off+n > c.limit {
		n = c.limit - c.off
	}
	if n <= 0 {
		c.err = fmt.Errorf("cab: a name begins at offset %d, at the end of the cabinet: %w", c.off, ErrTruncated)
		return nil
	}
	b := c.at(c.off, int(n))
	if c.err != nil {
		return nil
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		c.err = fmt.Errorf("cab: the name at offset %d is not NUL-terminated within %d bytes: %w", c.off, n, ErrCorrupt)
		return nil
	}
	c.off += int64(i) + 1
	return b[:i]
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

// block is one CFDATA: where its compressed bytes are, how long they are, and
// where its output begins within the folder's uncompressed stream.
type block struct {
	dataOff  int64
	cbData   uint16
	cbUncomp uint16
	csum     uint32
	start    int64
}

// folder is one CFFOLDER plus the block table scanned from it.
type folder struct {
	compress Compression
	raw      uint16 // typeCompress as recorded, window bits and all
	reserved []byte
	blocks   []block
	uncomp   int64 // total uncompressed length of the folder's stream
}

// entry is one path the cabinet holds. A directory is synthesised -- the format
// records no directory entries -- and carries ordinal -1.
type entry struct {
	path    string
	name    string
	dir     bool
	size    int64
	folder  int
	off     int64 // uoffFolderStart
	attribs uint16
	span    bool
	mod     time.Time
	ord     int
}

// parse reads a cabinet's metadata: the header, every CFFOLDER with its block
// table, and every CFFILE. It reads no file DATA.
func parse(r io.ReaderAt, size int64) (*FS, error) {
	c := &cursor{r: r, limit: size}
	if size < 4 {
		return nil, fmt.Errorf("cab: %d bytes cannot hold a cabinet signature: %w", size, ErrNotCabinet)
	}
	if magic := c.read(4); !bytes.Equal(magic, []byte("MSCF")) {
		return nil, fmt.Errorf("cab: signature is %#x, want \"MSCF\": %w", magic, ErrNotCabinet)
	}

	h := c.read(hdrFixedSize - 4)
	if c.err != nil {
		return nil, c.err
	}
	cbCabinet := int64(le32(h[4:]))
	coffFiles := int64(le32(h[12:]))
	cFolders := int(le16(h[22:]))
	cFiles := int(le16(h[24:]))
	flags := le16(h[26:])

	// cbCabinet is the cabinet's own idea of its length. Believing a larger
	// value than we hold would turn a truncation into a decode of whatever
	// follows; a SMALLER one is legitimate (a cabinet appended to a stub),
	// and becomes the limit for every offset below.
	if cbCabinet < hdrFixedSize || cbCabinet > size {
		return nil, fmt.Errorf("cab: cbCabinet is %d, outside [%d, %d]: %w", cbCabinet, hdrFixedSize, size, ErrTruncated)
	}
	c.limit = cbCabinet

	f := &FS{
		r:      r,
		size:   cbCabinet,
		verMin: h[20],
		verMaj: h[21],
		setID:  le16(h[28:]),
		index:  le16(h[30:]),
		byPath: map[string]*entry{},
		kids:   map[string][]string{},
	}

	// ⛔ These three sizes change the size of EVERY later structure. The
	// header's own reserve area is skipped here; cbCFFolder and cbCFData are
	// carried down into the folder and CFDATA loops below.
	var cbCFFolder, cbCFData int
	if flags&flagReservePresent != 0 {
		res := c.read(4)
		cbCFHeader := int(le16(res[0:]))
		cbCFFolder = int(res[2])
		cbCFData = int(res[3])
		f.reserved = c.read(cbCFHeader)
	}
	if flags&flagPrevCabinet != 0 {
		f.prev = string(c.cstring())
		f.prevDisk = string(c.cstring())
	}
	if flags&flagNextCabinet != 0 {
		f.next = string(c.cstring())
		f.nextDisk = string(c.cstring())
	}
	if c.err != nil {
		return nil, c.err
	}

	if cFolders == 0 {
		return nil, fmt.Errorf("cab: cFolders is 0, so no file can be reached: %w", ErrCorrupt)
	}
	f.folders = make([]folder, cFolders)
	for i := range f.folders {
		b := c.read(folderFixedSize + cbCFFolder)
		if c.err != nil {
			return nil, c.err
		}
		fol := &f.folders[i]
		fol.raw = le16(b[6:])
		fol.compress = Compression(fol.raw & 0x0f)
		fol.reserved = b[folderFixedSize:]
		// The method is judged BEFORE the block table is walked, so that a
		// Quantum or LZX cabinet is refused by name rather than by whatever
		// its blocks happen to look like to a decoder that will not run.
		if err := checkCompression(fol.compress, i); err != nil {
			return nil, err
		}
		if err := scanBlocks(c, fol, i, int64(le32(b[0:])), int(le16(b[4:])), cbCFData); err != nil {
			return nil, err
		}
	}

	c.off = coffFiles
	for i := 0; i < cFiles; i++ {
		b := c.read(fileFixedSize)
		name := c.cstring()
		if c.err != nil {
			return nil, c.err
		}
		e, err := makeEntry(b, name, i, len(f.folders))
		if err != nil {
			return nil, err
		}
		fol := &f.folders[e.folder]
		if !e.span && e.off+e.size > fol.uncomp {
			return nil, fmt.Errorf("cab: %q wants bytes [%d, %d) of folder %d, which yields %d: %w", e.path, e.off, e.off+e.size, e.folder, fol.uncomp, ErrCorrupt)
		}
		if err := f.add(e); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// scanBlocks walks a folder's CFDATA chain, recording where each block's
// compressed bytes are and where its output lands in the folder's stream.
func scanBlocks(c *cursor, fol *folder, idx int, off int64, count, cbCFData int) error {
	fol.blocks = make([]block, count)
	for j := range fol.blocks {
		b := c.at(off, dataFixedSize+cbCFData)
		if c.err != nil {
			return c.err
		}
		blk := block{
			csum:     le32(b[0:]),
			cbData:   le16(b[4:]),
			cbUncomp: le16(b[6:]),
			dataOff:  off + int64(dataFixedSize+cbCFData),
			start:    fol.uncomp,
		}
		if blk.cbUncomp > maxBlockUncomp {
			return fmt.Errorf("cab: folder %d block %d yields %d bytes, over the %d ceiling: %w", idx, j, blk.cbUncomp, maxBlockUncomp, ErrCorrupt)
		}
		if fol.compress == CompressionNone && blk.cbData != blk.cbUncomp {
			return fmt.Errorf("cab: folder %d block %d is stored yet holds %d bytes for %d: %w", idx, j, blk.cbData, blk.cbUncomp, ErrCorrupt)
		}
		if blk.dataOff+int64(blk.cbData) > c.limit {
			return fmt.Errorf("cab: folder %d block %d holds %d bytes at offset %d, past the end of the %d-byte cabinet: %w", idx, j, blk.cbData, blk.dataOff, c.limit, ErrTruncated)
		}
		fol.blocks[j] = blk
		fol.uncomp += int64(blk.cbUncomp)
		off = blk.dataOff + int64(blk.cbData)
	}
	return nil
}

// checkCompression separates "I do not know what this is" from "I know exactly
// what this is and do not read it yet".
func checkCompression(m Compression, idx int) error {
	switch m {
	case CompressionNone, CompressionMSZIP:
		return nil
	case CompressionQuantum, CompressionLZX:
		return fmt.Errorf("cab: folder %d uses %s, which is a complete algorithm this package does not implement: %w", idx, m, ErrCompressionUnsupported)
	default:
		return fmt.Errorf("cab: folder %d records %s, which the format does not define: %w", idx, m, ErrUnknownCompression)
	}
}

// makeEntry turns one CFFILE into an entry, converting its name to this
// driver's spelling of a path.
func makeEntry(b, rawName []byte, ord, nFolders int) (*entry, error) {
	e := &entry{
		size:    int64(le32(b[0:])),
		off:     int64(le32(b[4:])),
		attribs: le16(b[14:]),
		mod:     dosTime(le16(b[10:]), le16(b[12:])),
		ord:     ord,
	}
	iFolder := le16(b[8:])
	switch {
	case iFolder >= folderFirstContinuationMark:
		// The entry belongs to a folder that is continued across cabinets. The
		// folder INDEX is then implied (first or last), so point at one that
		// exists and let the read refuse.
		e.span = true
		if iFolder == folderContinuedFromPrev || iFolder == folderContinuedPrevAndNext {
			e.folder = 0
		} else {
			e.folder = nFolders - 1
		}
	case int(iFolder) < nFolders:
		e.folder = int(iFolder)
	default:
		return nil, fmt.Errorf("cab: an entry names folder %d of %d: %w", iFolder, nFolders, ErrCorrupt)
	}

	name, err := decodeName(rawName, e.attribs&attrNameIsUTF8 != 0)
	if err != nil {
		return nil, err
	}
	e.path = name
	e.name = path.Base(name)
	return e, nil
}

// decodeName converts a CFFILE name to a cleaned, slash-separated path.
//
// ⛔ The separator is a BACKSLASH. A reader that leaves it alone produces one
// entry called `sub\deep\file.txt` at the root, which every path-based caller
// then fails to find while the listing looks plausible.
func decodeName(raw []byte, isUTF8 bool) (string, error) {
	var s string
	if isUTF8 {
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("cab: a name is flagged UTF-8 and is not valid UTF-8 (%q): %w", raw, ErrCorrupt)
		}
		s = string(raw)
	} else {
		s = decodeWindows1252(raw)
	}
	s = strings.ReplaceAll(s, `\`, "/")
	s = path.Clean("/" + s)[1:]
	if s == "" {
		return "", fmt.Errorf("cab: a name (%q) resolves to no path at all: %w", raw, ErrCorrupt)
	}
	return s, nil
}

// cp1252High maps the 0x80..0x9F range, which is where Windows-1252 differs
// from ISO 8859-1; 0xA0..0xFF agree with Unicode code points of the same value.
// The five positions Windows-1252 leaves undefined map to U+FFFD.
var cp1252High = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡',
	'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—',
	'˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

func decodeWindows1252(raw []byte) string {
	var sb strings.Builder
	sb.Grow(len(raw))
	for _, b := range raw {
		switch {
		case b < 0x80:
			sb.WriteByte(b)
		case b < 0xA0:
			sb.WriteRune(cp1252High[b-0x80])
		default:
			sb.WriteRune(rune(b))
		}
	}
	return sb.String()
}

// dosTime decodes the MS-DOS packed date and time a CFFILE carries. The format
// has two-second resolution and no timezone, so the result is in UTC.
func dosTime(date, t uint16) time.Time {
	return time.Date(
		1980+int(date>>9), time.Month((date>>5)&0x0f), int(date&0x1f),
		int(t>>11), int((t>>5)&0x3f), int(t&0x1f)*2, 0, time.UTC)
}

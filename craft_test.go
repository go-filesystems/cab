// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"sync/atomic"
	"testing"
)

// A cabinet builder, for the structural failures no real writer will produce.
//
// It is deliberately NOT used as a witness for decoding: a fixture made of the
// reader's own idea of the format cannot disagree with it. Every assertion about
// CONTENT is made against testdata/, written by gcab. What is built here are
// broken and exotic cabinets -- reserve areas, continuation markers, a folder
// claiming LZX -- and the field OVERRIDES that make a header lie.
type craft struct {
	magic     []byte // nil means "MSCF"
	setID     uint16
	index     uint16
	verMaj    uint8
	verMin    uint8
	resHdr    []byte
	resFolder int
	resData   int
	prev      *[2]string
	next      *[2]string
	folders   []cFolder
	files     []cFile

	// Overrides. Each is applied AFTER the honest value is computed, so a
	// cabinet can be made to lie about exactly one thing.
	cbCabinet    *uint32
	coffFiles    *uint32
	cFoldersSaid *uint16
	cFilesSaid   *uint16
	truncateTo   int // >0: keep only this many bytes
}

type cFolder struct {
	typeC      uint16
	coffSaid   *uint32
	cCFDataSay *uint16
	blocks     []cBlock
}

type cBlock struct {
	data       []byte
	cbUncomp   *uint16 // nil: len(data)
	cbDataSaid *uint16
	csum       *uint32 // nil: computed; point at 0 for "not computed"
}

type cFile struct {
	size    uint32
	off     uint32
	iFolder uint16
	date    uint16
	time    uint16
	attribs uint16
	name    []byte
	noNUL   bool
}

func u16(v uint16) *uint16 { return &v }
func u32(v uint32) *uint32 { return &v }

func (c craft) build() []byte {
	var hdr bytes.Buffer
	magic := c.magic
	if magic == nil {
		magic = []byte("MSCF")
	}
	verMaj, verMin := c.verMaj, c.verMin
	if verMaj == 0 && verMin == 0 {
		verMaj, verMin = 3, 1
	}
	var flags uint16
	if len(c.resHdr) > 0 || c.resFolder > 0 || c.resData > 0 {
		flags |= flagReservePresent
	}
	if c.prev != nil {
		flags |= flagPrevCabinet
	}
	if c.next != nil {
		flags |= flagNextCabinet
	}
	cFoldersSaid := uint16(len(c.folders))
	if c.cFoldersSaid != nil {
		cFoldersSaid = *c.cFoldersSaid
	}
	cFilesSaid := uint16(len(c.files))
	if c.cFilesSaid != nil {
		cFilesSaid = *c.cFilesSaid
	}
	hdr.Write(magic)
	putU32(&hdr, 0)       // reserved1
	putU32(&hdr, 0)       // cbCabinet, rewritten below
	putU32(&hdr, 0)       // reserved2
	putU32(&hdr, 0)       // coffFiles, rewritten below
	putU32(&hdr, 0)       // reserved3
	hdr.WriteByte(verMin) //
	hdr.WriteByte(verMaj) //
	putU16(&hdr, cFoldersSaid)
	putU16(&hdr, cFilesSaid)
	putU16(&hdr, flags)
	putU16(&hdr, c.setID)
	putU16(&hdr, c.index)
	if flags&flagReservePresent != 0 {
		putU16(&hdr, uint16(len(c.resHdr)))
		hdr.WriteByte(byte(c.resFolder))
		hdr.WriteByte(byte(c.resData))
		hdr.Write(c.resHdr)
	}
	if c.prev != nil {
		hdr.WriteString(c.prev[0])
		hdr.WriteByte(0)
		hdr.WriteString(c.prev[1])
		hdr.WriteByte(0)
	}
	if c.next != nil {
		hdr.WriteString(c.next[0])
		hdr.WriteByte(0)
		hdr.WriteString(c.next[1])
		hdr.WriteByte(0)
	}

	var table bytes.Buffer
	for _, f := range c.files {
		putU32(&table, f.size)
		putU32(&table, f.off)
		putU16(&table, f.iFolder)
		putU16(&table, f.date)
		putU16(&table, f.time)
		putU16(&table, f.attribs)
		table.Write(f.name)
		if !f.noNUL {
			table.WriteByte(0)
		}
	}

	coffFiles := uint32(hdr.Len() + len(c.folders)*(folderFixedSize+c.resFolder))
	dataStart := coffFiles + uint32(table.Len())

	var fols, data bytes.Buffer
	at := dataStart
	for _, f := range c.folders {
		coff := at
		if f.coffSaid != nil {
			coff = *f.coffSaid
		}
		said := uint16(len(f.blocks))
		if f.cCFDataSay != nil {
			said = *f.cCFDataSay
		}
		putU32(&fols, coff)
		putU16(&fols, said)
		putU16(&fols, f.typeC)
		fols.Write(bytes.Repeat([]byte{0xAB}, c.resFolder))
		for _, b := range f.blocks {
			cbUncomp := uint16(len(b.data))
			if b.cbUncomp != nil {
				cbUncomp = *b.cbUncomp
			}
			cbData := uint16(len(b.data))
			if b.cbDataSaid != nil {
				cbData = *b.cbDataSaid
			}
			var csum uint32
			if b.csum != nil {
				csum = *b.csum
			} else {
				csum = checksum(b.data, uint32(cbData)|uint32(cbUncomp)<<16)
			}
			putU32(&data, csum)
			putU16(&data, cbData)
			putU16(&data, cbUncomp)
			data.Write(bytes.Repeat([]byte{0xCD}, c.resData))
			data.Write(b.data)
			at += uint32(dataFixedSize + c.resData + len(b.data))
		}
	}

	out := append(hdr.Bytes(), fols.Bytes()...)
	out = append(out, table.Bytes()...)
	out = append(out, data.Bytes()...)
	cb := uint32(len(out))
	if c.cbCabinet != nil {
		cb = *c.cbCabinet
	}
	binary.LittleEndian.PutUint32(out[8:], cb)
	cf := coffFiles
	if c.coffFiles != nil {
		cf = *c.coffFiles
	}
	binary.LittleEndian.PutUint32(out[16:], cf)
	if c.truncateTo > 0 && c.truncateTo < len(out) {
		out = out[:c.truncateTo]
	}
	return out
}

func putU16(b *bytes.Buffer, v uint16) {
	var p [2]byte
	binary.LittleEndian.PutUint16(p[:], v)
	b.Write(p[:])
}

func putU32(b *bytes.Buffer, v uint32) {
	var p [4]byte
	binary.LittleEndian.PutUint32(p[:], v)
	b.Write(p[:])
}

// stored splits data into stored CFDATA blocks of at most maxBlockUncomp bytes.
func stored(data []byte) []cBlock {
	var out []cBlock
	for len(data) > maxBlockUncomp {
		out = append(out, cBlock{data: data[:maxBlockUncomp]})
		data = data[maxBlockUncomp:]
	}
	return append(out, cBlock{data: data})
}

// oneStoredFile is the smallest honest cabinet: one None folder, one entry.
func oneStoredFile(name string, body []byte) craft {
	return craft{
		folders: []cFolder{{typeC: uint16(CompressionNone), blocks: stored(body)}},
		files:   []cFile{{size: uint32(len(body)), name: []byte(name), attribs: 0x20}},
	}
}

func openCraft(t *testing.T, c craft) (*FS, error) {
	t.Helper()
	b := c.build()
	return Open(bytes.NewReader(b), int64(len(b)))
}

// mustOpen builds a cabinet the test expects to be well-formed.
func mustOpen(t *testing.T, c craft) *FS {
	t.Helper()
	fs, err := openCraft(t, c)
	if err != nil {
		t.Fatalf("Open(crafted): %v", err)
	}
	return fs
}

// mszipBlock wraps data as an MSZIP CFDATA: "CK" then a raw DEFLATE stream
// whose back-references may reach into dict.
//
// It builds cabinets for the STRUCTURAL paths -- an empty block, a folder that
// must be refused -- and never stands in as a witness for decoding, which is
// measured against gcab's output in testdata/.
func mszipBlock(t *testing.T, data, dict []byte) cBlock {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("CK")
	zw, err := flate.NewWriterDict(&buf, flate.BestCompression, dict)
	if err != nil {
		t.Fatalf("NewWriterDict: %v", err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("deflate write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("deflate close: %v", err)
	}
	return cBlock{data: buf.Bytes(), cbUncomp: u16(uint16(len(data)))}
}

// failAt is an io.ReaderAt that refuses every read touching from or beyond,
// which is how an I/O failure partway through a cabinet is reached without a
// real disk that is failing.
// armed exists because indexing a cabinet walks the WHOLE block table, whose
// last CFDATA header sits past every block's data: a reader that failed from the
// start of block 1 could never get through parse. A test therefore opens the
// cabinet unarmed and arms the reader before reading.
type failAt struct {
	r    io.ReaderAt
	from int64 // fail every read touching this offset or beyond
	// whenLen, when set, fails reads of exactly this length instead of using
	// from. It is how a failure is aimed at a NAME: parse walks the folder
	// table -- whose last CFDATA header sits past the file table -- before it
	// reads a single name, so no offset threshold can reach one first, and only
	// a name is read maxNameLen+1 bytes at a time.
	whenLen int
	err     error
	armed   *atomic.Bool
}

func (f failAt) ReadAt(p []byte, off int64) (int, error) {
	switch {
	case f.armed != nil && !f.armed.Load():
	case f.whenLen > 0 && len(p) == f.whenLen:
		return 0, f.err
	case f.whenLen == 0 && off+int64(len(p)) > f.from:
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

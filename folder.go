// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// folderReader produces one folder's uncompressed stream, block by block.
//
// ⛔ Under MSZIP the DEFLATE history carries ACROSS CFDATA blocks: block N may
// reference bytes emitted by block N-1. hist holds exactly the preceding
// maxBlockUncomp bytes of output and is handed to flate.NewReaderDict for every
// block. A decoder that drops it is correct for every file that fits in one
// block and silently wrong for every file that does not.
type folderReader struct {
	fs   *FS
	idx  int
	blk  int    // index of the next block to decode
	buf  []byte // the decoded bytes of the current block not yet returned
	hist []byte // the preceding maxBlockUncomp bytes of folder output
	off  int64  // uncompressed offset of the next byte Read will return
}

// reset positions the reader so that it can reach target without reading
// backwards, and returns the offset it actually starts from.
//
// Under None there is no history, so it starts at the block CONTAINING target.
// Under MSZIP it must start at the beginning of the folder, because no block is
// independently decodable.
func (fr *folderReader) reset(target int64) int64 {
	fol := &fr.fs.folders[fr.idx]
	start := 0
	if fol.compress == CompressionNone {
		start = sort.Search(len(fol.blocks), func(i int) bool {
			return fol.blocks[i].start+int64(fol.blocks[i].cbUncomp) > target
		})
	}
	// target always lies inside the folder's stream: a handle refuses an offset
	// at or past its entry's size, and parse refused an entry that does not fit
	// in its folder. So the search always lands on a block that exists.
	fr.blk, fr.buf, fr.hist = start, nil, nil
	fr.off = fol.blocks[start].start
	return fr.off
}

// Read returns the next bytes of the folder's stream.
func (fr *folderReader) Read(p []byte) (int, error) {
	for len(fr.buf) == 0 {
		if err := fr.nextBlock(); err != nil {
			return 0, err
		}
	}
	n := copy(p, fr.buf)
	fr.buf = fr.buf[n:]
	fr.off += int64(n)
	return n, nil
}

// nextBlock reads, verifies and decodes one CFDATA.
func (fr *folderReader) nextBlock() error {
	fol := &fr.fs.folders[fr.idx]
	if fr.blk >= len(fol.blocks) {
		return io.EOF
	}
	b := fol.blocks[fr.blk]
	raw := make([]byte, b.cbData)
	if _, err := fr.fs.r.ReadAt(raw, b.dataOff); err != nil {
		return fmt.Errorf("cab: folder %d block %d: reading %d bytes at offset %d: %w", fr.idx, fr.blk, b.cbData, b.dataOff, err)
	}
	// csum 0 means "not computed", which many writers leave it at; a non-zero
	// one is checked, because a block that decodes to plausible rubbish is
	// worse than a block that refuses.
	if b.csum != 0 {
		if got := checksum(raw, uint32(b.cbData)|uint32(b.cbUncomp)<<16); got != b.csum {
			return fmt.Errorf("cab: folder %d block %d: checksum is %#08x, recorded %#08x: %w", fr.idx, fr.blk, got, b.csum, ErrCorrupt)
		}
	}
	out := raw
	if fol.compress == CompressionMSZIP {
		var err error
		if out, err = inflateMSZIP(raw, fr.hist, int(b.cbUncomp)); err != nil {
			return fmt.Errorf("cab: folder %d block %d: %w", fr.idx, fr.blk, err)
		}
	}
	fr.buf = out
	fr.blk++
	fr.hist = window(fr.hist, out)
	return nil
}

// inflateMSZIP decodes one MSZIP block: the two bytes "CK", then a raw DEFLATE
// stream whose back-references may reach into dict, the preceding block output.
func inflateMSZIP(raw, dict []byte, want int) ([]byte, error) {
	if len(raw) < 2 || raw[0] != 'C' || raw[1] != 'K' {
		return nil, fmt.Errorf("cab: MSZIP block does not begin with the \"CK\" signature: %w", ErrCorrupt)
	}
	zr := flate.NewReaderDict(bytes.NewReader(raw[2:]), dict)
	defer zr.Close()
	out := make([]byte, want)
	// The block declares its own output length, so the stream is read to that
	// length rather than to its end: MSZIP writers differ over whether they set
	// DEFLATE's final-block bit, and cbUncomp is the authority either way.
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, fmt.Errorf("cab: inflating the %d bytes an MSZIP block declares: %w: %w", want, err, ErrCorrupt)
	}
	return out, nil
}

// window returns the last maxBlockUncomp bytes of hist followed by out, which
// is the dictionary the NEXT block inflates against.
func window(hist, out []byte) []byte {
	if len(out) >= maxBlockUncomp {
		return out[len(out)-maxBlockUncomp:]
	}
	if keep := maxBlockUncomp - len(out); len(hist) > keep {
		hist = hist[len(hist)-keep:]
	}
	w := make([]byte, 0, len(hist)+len(out))
	return append(append(w, hist...), out...)
}

// checksum is the cabinet format's own block checksum: a 32-bit XOR of the
// little-endian words of b, seeded with seed.
//
// ⛔ The trailing 1..3 bytes are NOT folded in little-endian order. The
// specification's reference code walks them with a pointer, so the FIRST
// leftover byte takes the HIGHEST position of the three. Folding them
// little-endian instead agrees with the recorded value whenever cbData is a
// multiple of four -- 12 of 29 blocks in the first fixture measured -- and
// disagrees on the rest.
func checksum(b []byte, seed uint32) uint32 {
	sum := seed
	n := len(b) / 4 * 4
	for i := 0; i < n; i += 4 {
		sum ^= binary.LittleEndian.Uint32(b[i:])
	}
	var ul uint32
	switch len(b) - n {
	case 3:
		ul = uint32(b[n])<<16 | uint32(b[n+1])<<8 | uint32(b[n+2])
	case 2:
		ul = uint32(b[n])<<8 | uint32(b[n+1])
	case 1:
		ul = uint32(b[n])
	}
	return sum ^ ul
}

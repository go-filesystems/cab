// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// FS reads a Microsoft Cabinet as a filesystem.
//
// It satisfies filesystem.Filesystem and the optional filesystem.Opener. See
// the package documentation, "Random access costs what the format costs", for
// what a seek inside a compressed folder actually does.
type FS struct {
	r    io.ReaderAt
	size int64

	verMaj, verMin uint8
	setID, index   uint16
	prev, prevDisk string
	next, nextDisk string
	reserved       []byte
	folders        []folder
	byPath         map[string]*entry
	kids           map[string][]string
}

// SetID is the cabinet set this file belongs to: every cabinet of one set
// carries the same value.
func (f *FS) SetID() uint16 { return f.setID }

// CabinetIndex is this cabinet's position within its set, counting from zero.
func (f *FS) CabinetIndex() uint16 { return f.index }

// Version is the format version the header records, major first. The
// specification describes 1.3.
func (f *FS) Version() (major, minor uint8) { return f.verMaj, f.verMin }

// PrevCabinet is the file name of the cabinet before this one in its set, and
// "" when the header does not name one. PrevDisk is the accompanying media
// name, which is prompt text rather than a path.
func (f *FS) PrevCabinet() string { return f.prev }

// PrevDisk is the media name recorded beside PrevCabinet.
func (f *FS) PrevDisk() string { return f.prevDisk }

// NextCabinet is the file name of the cabinet after this one in its set, and ""
// when the header does not name one.
func (f *FS) NextCabinet() string { return f.next }

// NextDisk is the media name recorded beside NextCabinet.
func (f *FS) NextDisk() string { return f.nextDisk }

// Reserved is the opaque area the header's cbCFHeader reserved, or nil. The
// bytes belong to whatever wrote the cabinet; this package only skips them
// correctly.
func (f *FS) Reserved() []byte { return f.reserved }

// FolderCount is how many CFFOLDER streams the cabinet holds.
func (f *FS) FolderCount() int { return len(f.folders) }

// Compression reports the method recorded in the CFFOLDER that holds path.
//
// It exists so that a test can assert the PREMISE of a fixture rather than
// assume it: a writer asked for MSZIP may store a small entry anyway, and a
// corpus of such entries passes every method while only None ever runs.
func (f *FS) Compression(p string) (Compression, error) {
	e, err := f.lookup(p)
	if err != nil {
		return 0, err
	}
	if e.dir {
		return 0, ErrNotRegular
	}
	return f.folders[e.folder].compress, nil
}

// Attributes is the CFFILE attribute word recorded for path.
func (f *FS) Attributes(p string) (uint16, error) {
	e, err := f.lookup(p)
	if err != nil {
		return 0, err
	}
	return e.attribs, nil
}

// ModTime is the modification time recorded for path. The format's packed
// MS-DOS date and time have two-second resolution and no timezone, so the
// result is in UTC.
func (f *FS) ModTime(p string) (time.Time, error) {
	e, err := f.lookup(p)
	if err != nil {
		return time.Time{}, err
	}
	return e.mod, nil
}

// Close releases nothing: FS never took ownership of the io.ReaderAt it was
// given, and each read decodes into memory it drops on the way out.
func (f *FS) Close() error { return nil }

func (f *FS) lookup(p string) (*entry, error) {
	e, ok := f.byPath[clean(p)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, p)
	}
	return e, nil
}

// ListDir names what is directly inside p, sorted.
func (f *FS) ListDir(p string) ([]filesystem.DirEntry, error) {
	cp := clean(p)
	if cp == "" {
		cp = "."
	}
	if cp != "." {
		e, err := f.lookup(cp)
		if err != nil {
			return nil, err
		}
		if !e.dir {
			return nil, fmt.Errorf("%w: %q", ErrNotDirectory, p)
		}
	}
	kids := append([]string(nil), f.kids[cp]...)
	sort.Strings(kids)
	out := make([]filesystem.DirEntry, 0, len(kids))
	for _, k := range kids {
		e := f.byPath[k]
		var ftype uint8
		if e.dir {
			ftype = 2 // DT_DIR
		}
		out = append(out, filesystem.NewDirEntry(uint64(e.ord+2), e.name, ftype))
	}
	return out, nil
}

// POSIX st_mode type bits. os.FileMode is NOT the shape a filesystem.Stat
// wants: it is a uint32 whose type bits sit at the TOP (os.ModeDir is 1<<31),
// so narrowing one to a uint16 throws every one of them away and leaves a
// directory unable to say it is one.
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
)

func (e *entry) mode() uint16 {
	if e.dir {
		return modeDir | 0o555
	}
	if e.attribs&attrExec != 0 {
		return modeRegular | 0o555
	}
	return modeRegular | 0o444
}

// Stat reports what the cabinet recorded about p.
func (f *FS) Stat(p string) (filesystem.Stat, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(e.mode(), uint64(e.size), uint64(e.ord+2)), nil
}

// ReadLink always refuses: the cabinet format records no symbolic links, so
// there is never one to read. The method exists because filesystem.Filesystem
// requires it.
func (f *FS) ReadLink(p string) (string, error) {
	if _, err := f.lookup(p); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w: %q", ErrNotSymlink, p)
}

// ReadFile returns the whole entry.
//
// It holds the entry in memory, which for a large member of a compressed folder
// is the whole member. Use OpenFile for anything whose size you did not choose.
func (f *FS) ReadFile(p string) ([]byte, error) {
	h, err := f.OpenFile(p)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	size := h.Size()
	// ⛔ The size is the cabinet's claim, and the block table that supports it is
	// only as true as the DEFLATE underneath. So it is capped, and the buffer
	// grows with the bytes that actually come out, a step at a time: a claim of
	// 65 MB over a 16 KB cabinet used to allocate all 65 MB before the first
	// block failed to inflate. A step is large (16 MiB) so that the files people
	// really read are one allocation and one read, as before.
	if size > MaxReadFileSize {
		return nil, fmt.Errorf("cab: %q is %d bytes, over the %d ceiling of ReadFile (OpenFile has none): %w",
			p, size, MaxReadFileSize, ErrTooLarge)
	}
	var buf []byte
	for int64(len(buf)) < size {
		step := int(min(size-int64(len(buf)), readStep))
		if need := len(buf) + step; need > cap(buf) {
			// One allocation, doubling but never past the entry's size. (slices.Grow
			// allocates the step twice under -race.)
			grown := make([]byte, len(buf), max(need, int(min(2*int64(cap(buf)), size))))
			copy(grown, buf)
			buf = grown
		}
		n, err := h.ReadAt(buf[len(buf):len(buf)+step], int64(len(buf)))
		buf = buf[:len(buf)+n]
		// handle.ReadAt answers io.EOF only for a read that comes up short, and
		// this one is clipped to the entry, so any error here is a real one.
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// readStep is how much of an entry ReadFile asks the decoder for at a time.
const readStep = 16 << 20

// OpenFile hands back a random-access handle on one entry, reading none of it.
func (f *FS) OpenFile(p string) (filesystem.File, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if e.dir {
		return nil, fmt.Errorf("%w: %q", ErrNotRegular, p)
	}
	if e.span {
		return nil, fmt.Errorf("cab: %q is continued from or into another cabinet of set %d, which one reader cannot reach: %w", p, f.setID, ErrSpansCabinets)
	}
	return &handle{fs: f, e: e}, nil
}

// The mutating half of filesystem.Filesystem. A cabinet is read-only, as in the
// other read-only drivers of this org.
func (f *FS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (f *FS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (f *FS) DeleteFile(string) error                     { return ErrReadOnly }
func (f *FS) DeleteDir(string) error                      { return ErrReadOnly }
func (f *FS) Rename(string, string) error                 { return ErrReadOnly }

// add records one entry, plus every directory above it. A cabinet is a list of
// paths and not a tree -- it holds no directory entries at all -- so a reader
// that registers only what it was told cannot list a single directory.
//
// ⛔ Two costs here were unbounded, both measured on the old code:
//
//   - appendOnce scanned a directory's children before appending, so N files in one
//     directory cost N²/2 comparisons: 65 535 files (a 1.6 MB cabinet) took 3.08 s.
//     A child is new at every level it is appended at -- the entry itself, or a
//     directory created a moment ago -- and the walk stops at the first directory
//     that already existed, whose own ancestors are filed already. So nothing is
//     ever appended twice and nothing needs looking up.
//   - every path implies its own directories, up to 127 a path, none shared when the
//     first components differ: 20 000 such files (a 5.4 MB cabinet) allocated 968 MB.
//     Entries, files and directories together, are capped at MaxEntries.
func (f *FS) add(e *entry) error {
	if prev, seen := f.byPath[e.path]; seen {
		if prev.ord < 0 {
			return fmt.Errorf("cab: entry %d names %q, which another entry already implies as a directory: %w", e.ord, e.path, ErrCorrupt)
		}
		return fmt.Errorf("cab: entries %d and %d both name %q: %w", prev.ord, e.ord, e.path, ErrCorrupt)
	}
	f.byPath[e.path] = e
	child := e.path
	for {
		if len(f.byPath) > maxEntries {
			return fmt.Errorf("cab: more than %d paths, counting the directories they imply: %w", maxEntries, ErrTooLarge)
		}
		parent := path.Dir(child)
		got, seen := f.byPath[parent]
		if seen && !got.dir {
			return fmt.Errorf("cab: %q is both a file and the parent directory of %q: %w", parent, child, ErrCorrupt)
		}
		f.kids[parent] = append(f.kids[parent], child)
		if seen || parent == "." {
			return nil
		}
		f.byPath[parent] = &entry{path: parent, name: path.Base(parent), dir: true, ord: -1}
		child = parent
	}
}

// clean turns a caller's spelling of a path into the one this driver answers
// to: '/' separators, no leading slash, no "." or ".." components. "" and "/"
// and "." all mean the root.
func clean(p string) string {
	return path.Clean("/" + p)[1:]
}

// handle is one open entry: a folder decoder parked at some offset.
//
// ReadAt is safe to call concurrently, as io.ReaderAt requires, by serialising
// on mu -- the decoder underneath is a single position and cannot be shared any
// other way.
type handle struct {
	fs *FS
	e  *entry
	mu sync.Mutex
	fr *folderReader
}

func (h *handle) Size() int64 { return h.e.size }

// Close drops the decoder. The handle is not usable afterwards.
func (h *handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fr = nil
	return nil
}

// ReadAt fills p from off within the entry.
//
// A short read always carries a non-nil error, which is io.ReaderAt's contract
// and the one part of it a caller cannot work around: a consumer that sees
// n < len(p) with a nil error treats it as "keep going" and loses data.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: %d", ErrNegativeOffset, off)
	}
	if off >= h.e.size {
		return 0, io.EOF
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	want := len(p)
	if int64(want) > h.e.size-off {
		want = int(h.e.size - off)
	}
	target := h.e.off + off
	if h.fr == nil || h.fr.off > target {
		h.fr = &folderReader{fs: h.fs, idx: h.e.folder}
		h.fr.reset(target)
	}
	if _, err := io.CopyN(io.Discard, h.fr, target-h.fr.off); err != nil {
		return 0, err
	}
	// The folder cannot end early: parse refused an entry whose declared range
	// does not fit the folder's own block table, so any error here comes from
	// the bytes underneath -- an I/O failure, a bad checksum, a stream that
	// will not inflate -- and is reported as it was raised rather than
	// relabelled. An error relabelled "corrupt" sends a caller looking at the
	// wrong thing when the disk is what failed.
	n, err := io.ReadFull(h.fr, p[:want])
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

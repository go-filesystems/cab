// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// FuzzOpen feeds the whole reader arbitrary bytes: the header, the folder and block
// tables, the file table, the index, and then a read of what the index promises.
// The invariants are the ones a hostile cabinet could break: no panic, no hang, and
// an entry Open accepts either reads back to exactly its declared size or fails
// with an error -- never a short read reported as success.
//
// Crashers go to testdata/fuzz AND get a named unit test, because the emulated CI
// lanes run a compiled test binary away from testdata/.
func FuzzOpen(f *testing.F) {
	for _, name := range []string{fxMSZIP, fxStore, fxPaths, fxReserve, fxHistory} {
		b, err := fixtures.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add(rawCab(0, 3, 2, 0, 0, []rawFile{{0, 0, `a\b`}}, true))
	f.Fuzz(func(t *testing.T, b []byte) {
		start := time.Now()
		fsys, err := Open(bytes.NewReader(b), int64(len(b)))
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("Open took %v over %d bytes", took, len(b))
		}
		if err != nil {
			if len(err.Error()) > 4096 {
				t.Fatalf("an error of %d bytes", len(err.Error()))
			}
			return
		}
		reads := 0
		walk(t, fsys, ".", 0, &reads)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("Open and %d reads took %v over %d bytes", reads, took, len(b))
		}
	})
}

// walk lists every directory and reads (at most 1 MiB of) the first few files: the
// cost of a read is the caller's to choose -- a backwards read of an MSZIP entry
// restarts its folder -- so reading them all would measure the harness.
func walk(t *testing.T, fsys *FS, dir string, depth int, reads *int) {
	t.Helper()
	if depth > 300 {
		t.Fatalf("a directory tree %d deep from a name of at most %d bytes", depth, maxNameLen)
	}
	kids, err := fsys.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	for _, k := range kids {
		p := k.Name()
		if dir != "." {
			p = dir + "/" + p
		}
		if _, err := fsys.Stat(p); err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if fsys.byPath[p].dir {
			walk(t, fsys, p, depth+1, reads)
			continue
		}
		h, err := fsys.OpenFile(p)
		if err != nil || *reads >= 4 {
			continue // a spanning entry, or similar: refused by name
		}
		*reads++
		n := min(h.Size(), 1<<20)
		buf := make([]byte, n)
		got, err := h.ReadAt(buf, 0)
		if err == nil && int64(got) != n {
			t.Fatalf("%q: a short read (%d of %d) reported as success", p, got, n)
		}
		if err != nil && err != io.EOF && len(err.Error()) > 4096 {
			t.Fatalf("an error of %d bytes", len(err.Error()))
		}
	}
}

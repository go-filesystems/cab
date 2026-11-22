// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package cab

import "sync"

const (
	// checkpointStride is the spacing, in blocks, of the MSZIP decoder states
	// kept for reuse: a handle opened at byte n resumes at most this many
	// blocks (checkpointStride * 32 KiB of output) before the block holding n.
	checkpointStride = 4
	// maxCheckpoints caps the cache for the whole FS. Each entry pins one
	// 32 KiB history window, so the cache never holds more than
	// maxCheckpoints * 32 KiB = 4 MiB, whatever the cabinet declares.
	maxCheckpoints = 128
)

// checkpoint is the state of a folder decoder just before block blk: the
// history window every later block needs. hist is never mutated after it is
// built (window always allocates), so it is shared, not copied.
type checkpoint struct {
	folder, blk int
	off         int64
	hist        []byte
	used        uint64
}

// checkpoints is a bounded cache of MSZIP decoder states, shared by every
// handle of one FS. Without it each OpenFile restarts the folder from its
// first block, which is quadratic over a cabinet's members.
type checkpoints struct {
	mu    sync.Mutex
	tick  uint64
	items []checkpoint
}

// nearest returns the cached state of folder with the greatest offset not
// above target, if any.
func (c *checkpoints) nearest(folder int, target int64) (checkpoint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	best := -1
	for i := range c.items {
		it := &c.items[i]
		if it.folder == folder && it.off <= target && (best < 0 || it.off > c.items[best].off) {
			best = i
		}
	}
	if best < 0 {
		return checkpoint{}, false
	}
	c.tick++
	c.items[best].used = c.tick
	return c.items[best], true
}

// put records the state before block blk, evicting the least recently used
// entry when the cache is full.
func (c *checkpoints) put(folder, blk int, off int64, hist []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	oldest := 0
	for i := range c.items {
		it := &c.items[i]
		if it.folder == folder && it.blk == blk {
			it.used = c.tick
			return
		}
		if it.used < c.items[oldest].used {
			oldest = i
		}
	}
	ck := checkpoint{folder: folder, blk: blk, off: off, hist: hist, used: c.tick}
	if len(c.items) < maxCheckpoints {
		c.items = append(c.items, ck)
		return
	}
	c.items[oldest] = ck
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"io"
)

// mp4Stats is what a walk of an MP4's box headers counted.
type mp4Stats struct {
	boxes int // every box met, containers included
	depth int // the deepest nesting met; a top-level box is at depth 1
}

// The boxes whose children the walk reads, and the bytes each holds before them,
// are mp4Containers (mp4_containers.go), generated from mp4ff by
// internal/mp4boxes: a list kept by hand missed the sample entries, dref, stsd,
// trgr, ludt and vexu, and 500,000 empty boxes inside any of them went past the
// bound. A test fails when mp4ff and the table differ. Any other box is kept as
// bytes by mp4ff and costs what it holds.
//
// ⛔ What the table cannot say is a header whose length the bytes choose: a box
// with several forms (a QuickTime audio entry has one per version, a meta box is
// ISO or QuickTime) lists them all and the walk takes the first whose children
// tile the box; and stpp, stpc, which hold three NUL-terminated strings of any
// length before their children, are known at the length they have when the
// strings are empty.

// ⛔ The table boxes (stts, stsc, stsz, stco, trun...) are NOT bounded by a count
// of their own. A count is a claim, but mp4ff checks it against the bytes of the
// box before it sizes anything (a 16-byte stts stating 2^32-1 entries is refused
// with 'expected size 34359738376, got 16' after 4 KB), and a uniform stsz, which
// states a count in 20 bytes, is checked against the size of the file when the
// samples are listed (progressiveSamples). What a table costs is therefore
// bounded by the bytes that hold it, and the number of boxes is what was left.

// checkMP4 walks the box headers of an MP4 of the given size, reading nothing
// but the headers (at most 16 bytes each), and refuses the file with
// ErrReaderLimit as soon as it holds more boxes, or nests them deeper, than
// limits allow: before mp4ff allocates a single box for it. A header the walk
// cannot read or make sense of ends that level of the walk without an error, for
// the decode that follows judges malformed boxes on its own. limits are applied
// as given: callers go through bounded.
func checkMP4(at io.ReaderAt, size int64, limits ReaderLimits) error {
	_, err := walkMP4(at, size, limits)
	return err
}

// walkMP4 is checkMP4 returning what it counted; it stops at the first refusal.
func walkMP4(at io.ReaderAt, size int64, limits ReaderLimits) (mp4Stats, error) {
	w := mp4Walk{at: at, limits: limits}
	err := w.level(0, size, 1)
	return w.stats, err
}

type mp4Walk struct {
	at     io.ReaderAt
	limits ReaderLimits
	stats  mp4Stats
	// window is the last 4 KiB read, so that a flood of 8-byte boxes costs one
	// read per 512 of them: a file read at every header costs a system call
	// each, and on Windows an allocation too (12.5 MB for a million boxes).
	window    [4 << 10]byte
	windowOff int64
	windowLen int
}

// peek returns the n bytes at off, nil when the source does not hold them. The
// slice is valid until the next peek.
func (w *mp4Walk) peek(off int64, n int) []byte {
	if off < w.windowOff || off+int64(n) > w.windowOff+int64(w.windowLen) {
		w.windowLen, _ = w.at.ReadAt(w.window[:], off)
		w.windowOff = off
	}
	if off+int64(n) > w.windowOff+int64(w.windowLen) {
		return nil
	}
	return w.window[off-w.windowOff:][:n]
}

// level walks the boxes that fill [pos, end) at the given depth.
func (w *mp4Walk) level(pos, end int64, depth int) error {
	for end-pos >= 8 {
		head := w.peek(pos, 8)
		if head == nil {
			return nil
		}
		boxSize, hdr := int64(binary.BigEndian.Uint32(head[:4])), int64(8)
		typ := [4]byte(head[4:8])
		w.stats.boxes++
		if w.stats.boxes > w.limits.MaxBoxes {
			return limitError("the file holds more than %d boxes", w.limits.MaxBoxes)
		}
		if depth > w.limits.MaxBoxDepth {
			return limitError("boxes nested more than %d deep", w.limits.MaxBoxDepth)
		}
		w.stats.depth = max(w.stats.depth, depth)
		switch boxSize {
		case 0:
			boxSize = end - pos // to the end of the enclosing box
		case 1:
			if end-pos < 16 {
				return nil
			}
			wide := w.peek(pos+8, 8)
			if wide == nil {
				return nil
			}
			large := binary.BigEndian.Uint64(wide)
			if large < 16 || large > uint64(end-pos) {
				return nil
			}
			boxSize, hdr = int64(large), 16
		}
		if boxSize < hdr || boxSize > end-pos {
			return nil
		}
		if skips, ok := mp4Containers[string(typ[:])]; ok {
			start, to := pos+hdr, pos+boxSize
			skip := w.formOf(skips, start, to)
			if to-start >= int64(skip) {
				if err := w.level(start+int64(skip), to, depth+1); err != nil {
					return err
				}
			}
		}
		pos += boxSize
	}
	return nil
}

// formOf chooses among the header lengths a box may have the first that leaves
// children that tile the box exactly, as mp4ff's own tmcd decoder does, and the
// first when none does: the walk then ends at the first header it cannot read.
func (w *mp4Walk) formOf(skips []int, start, end int64) int {
	if len(skips) > 1 {
		for _, skip := range skips {
			if w.tiles(start+int64(skip), end) {
				return skip
			}
		}
	}
	return skips[0]
}

// tiles says whether boxes of a stated size, one after the other, fill
// [pos, end) exactly. It reads their headers only.
func (w *mp4Walk) tiles(pos, end int64) bool {
	for pos < end {
		head := w.peek(pos, 8)
		if head == nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(head))
		if size < 8 || size > end-pos {
			return false
		}
		pos += size
	}
	return pos == end
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"io"
	"slices"
)

// mp4Stats is what a walk of an MP4's box headers counted.
type mp4Stats struct {
	boxes int // every box met, containers included
	depth int // the deepest nesting met; a top-level box is at depth 1
}

// The boxes whose children the walk reads, and the bytes each may hold before
// them, are mp4Containers and mp4StringForms (mp4_containers.go), generated from
// mp4ff by internal/mp4boxes: a list kept by hand missed the sample entries, dref,
// stsd, trgr, ludt and vexu. A test fails when mp4ff and the table differ. Any
// other box is kept as bytes by mp4ff and costs what it holds.
//
// ⛔ The walk does NOT decide which of a box's offsets mp4ff reads its children
// from. Where mp4ff picks one by the box's own bytes (a QuickTime audio entry at
// 28, 44 or 64, a meta box at 0 or 4, three NUL-terminated strings of any length
// in stpp), every candidate offset is walked, the children found there are counted
// even when they stop short of the end of the box, and the box is charged the
// largest count and the deepest nesting: whichever offset mp4ff takes, what it
// decodes is no more than what was counted. A rule copied from mp4ff would drift
// from it, and two earlier versions of this walk, which chose, were bypassed by
// a file built to make them choose differently.
//
// ⛔ And it fails closed: a box whose offset cannot be bounded (strings that run
// past the window) is refused, and so is a walk that takes more steps than any
// file within the bounds needs.

// ⛔ The table boxes (stts, stsc, stsz, stco, trun...) are NOT bounded by a count
// of their own. A count is a claim, but mp4ff checks it against the bytes of the
// box before it sizes anything (a 16-byte stts stating 2^32-1 entries is refused
// with 'expected size 34359738376, got 16' after 4 KB), and a uniform stsz, which
// states a count in 20 bytes, is checked against the size of the file when the
// samples are listed (progressiveSamples). What a table costs is therefore
// bounded by the bytes that hold it, and the number of boxes is what was left.

// mp4StringWindow is how far into a box the walk looks for the end of its strings.
const mp4StringWindow = 4 << 10

// mp4StepsPerBox is the steps (headers read, bytes scanned for a NUL) a walk may
// take for each box it may count, plus a constant. A file within the bounds takes
// a few per box; one that makes every candidate offset of nested boxes parse is
// what this stops.
const mp4StepsPerBox = 16

// stepBudget is how many steps a walk may take under these limits.
func (l ReaderLimits) stepBudget() int64 {
	return mp4StepsPerBox*min(int64(l.MaxBoxes), 1<<40) + 1<<16
}

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
	w := mp4Walk{at: at, limits: limits, steps: limits.stepBudget()}
	err := w.level(0, size, 1, false)
	return w.stats, err
}

type mp4Walk struct {
	at     io.ReaderAt
	limits ReaderLimits
	stats  mp4Stats
	steps  int64 // what is left of the steps the walk may take
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

// step spends one step of the walk.
func (w *mp4Walk) step() error {
	if w.steps--; w.steps < 0 {
		return limitError("the box structure takes more than %d steps to walk", w.limits.stepBudget())
	}
	return nil
}

// level walks the boxes that fill [pos, end) at the given depth.
func (w *mp4Walk) level(pos, end int64, depth int, anyKind bool) error {
	for end-pos >= 8 {
		if err := w.step(); err != nil {
			return err
		}
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
		// A box of a type the table does not list is still a container when its
		// parent holds containers of any type (the items of an ilst).
		_, listed := mp4Containers[string(typ[:])]
		if listed || anyKind {
			if err := w.children(string(typ[:]), listed, pos+hdr, pos+boxSize, depth+1); err != nil {
				return err
			}
		}
		pos += boxSize
	}
	return nil
}

// children walks the children of a box whose payload is [start, end), from every
// offset mp4ff may read them at, and keeps the largest count.
func (w *mp4Walk) children(typ string, listed bool, start, end int64, depth int) error {
	offsets, err := w.offsets(typ, listed, start, end)
	if err != nil {
		return err
	}
	base := w.stats
	best := base
	for _, off := range offsets {
		w.stats = base
		if err := w.level(off, end, depth, mp4AnyKind[typ]); err != nil {
			return err
		}
		best.boxes = max(best.boxes, w.stats.boxes)
		best.depth = max(best.depth, w.stats.depth)
	}
	w.stats = best
	return nil
}

// offsets are the places a box of type typ, whose payload is [start, end), may
// have its children: after each header length the table lists, and, in a box
// that holds strings first, after each of the first three NUL bytes past the
// bytes before them, wherever those are. Strings that do not end within the
// window leave the offset unbounded, and the box is refused.
func (w *mp4Walk) offsets(typ string, listed bool, start, end int64) ([]int64, error) {
	if !listed {
		return []int64{start}, nil
	}
	var out []int64
	for _, skip := range mp4Containers[typ] {
		if start+int64(skip) <= end {
			out = append(out, start+int64(skip))
		}
	}
	lead, ok := mp4StringForms[typ]
	if !ok {
		return out, nil
	}
	pos, nuls := start+int64(lead), 0
	for pos < end && pos < start+int64(lead)+mp4StringWindow && nuls < 3 {
		if err := w.step(); err != nil {
			return nil, err
		}
		b := w.peek(pos, 1)
		if b == nil {
			return out, nil
		}
		pos++
		if b[0] == 0 {
			nuls++
			if !slices.Contains(out, pos) {
				out = append(out, pos)
			}
		}
	}
	if nuls < 3 && pos < end {
		return nil, limitError("a %s box holds strings longer than %d bytes, and its children cannot be placed", typ, mp4StringWindow)
	}
	return out, nil
}

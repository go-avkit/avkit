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

// The boxes whose children the walk reads are mp4Containers (mp4_containers.go),
// generated from mp4ff by internal/mp4boxes, which follows the calls of every
// decoder in mp4ff's source to the lookup of another decoder: a list kept by hand
// missed the sample entries, dref, stsd, trgr, ludt and vexu. A test fails when
// mp4ff and the table differ. Any other box is kept as bytes by mp4ff and costs
// what it holds, and the walk jumps over it.
//
// ⛔ The walk models NO offset. Where in a box mp4ff reads its children from can
// depend on the box's own bytes (a QuickTime text entry after a font name of
// 0 to 255 bytes, an audio entry at 28, 44 or 64, a meta box at 0 or 4, stpp
// after three strings): four versions of this walk that modelled it were each
// bypassed by a file built to make them differ from mp4ff. It takes as the
// possible start of the children every offset in the first 4 KiB of the box where
// a box header could be, walks the chain of boxes from each (a chain from the
// true start is what mp4ff decodes), counts what it finds even when the chain
// stops short of the end of the box, and charges the box the largest count and
// the deepest nesting. Whichever offset mp4ff takes, what it decodes is no more
// than what was counted.
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

// printableType says whether four bytes can be a box type as files write them:
// printable ASCII, or the (c) sign of the iTunes tags (0xA9) first. A candidate
// offset that holds anything else is not taken for the start of children, and a
// chain from a guessed start ends at it: numbers in a table that happen to be a
// plausible size would otherwise each start a chain through the table.
//
// ⛔ This is the one assumption the walk makes about a box it cannot place: that
// its type is printable. mp4ff decodes any four bytes as the type of an unknown
// box, so a flood of boxes of a non-printable type, inside a box whose children
// start at a content-dependent offset (a QuickTime text entry, an audio entry),
// is not counted; where the start is known (every other container) it is.
func printableType(t [4]byte) bool {
	for i, b := range t {
		if (b < 0x20 || b > 0x7e) && !(i == 0 && b == 0xa9) {
			return false
		}
	}
	return true
}

// mp4OffsetWindow is how far into a box the walk looks for where its children
// start: every offset in it that opens a plausible box is a candidate, and in the
// boxes that hold strings first so is the end of the strings, or the box is
// refused. The longest content-dependent header known is a QuickTime text entry's,
// 51 bytes and a font name of at most 255.
const mp4OffsetWindow = 4 << 10

// A walk may take mp4StepsPerBox steps (headers read, offsets and bytes looked at)
// for each box it may count and mp4StepsPerByte for each byte of the file, plus a
// constant, and is refused past that: its CPU is O(bytes), whatever the file does
// to make candidate offsets of nested boxes parse. The library of 3,466 files
// takes at most 4.4 steps per byte (more for small files, which the constant
// covers), a quarter of the allowance.
const (
	mp4StepsPerBox  = 16
	mp4StepsPerByte = 16
)

// stepBudget is how many steps a walk of a file of the given size may take.
func (l ReaderLimits) stepBudget(size int64) int64 {
	return mp4StepsPerBox*min(int64(l.MaxBoxes), 1<<40) + mp4StepsPerByte*min(size, 1<<50) + 1<<20
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
	budget := limits.stepBudget(size)
	w := mp4Walk{at: at, limits: limits, steps: budget, budget: budget}
	err := w.level(0, size, 1, false, nil, nil)
	return w.stats, err
}

type mp4Walk struct {
	at     io.ReaderAt
	limits ReaderLimits
	stats  mp4Stats
	steps  int64 // what is left of the steps the walk may take
	budget int64 // what it was
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
		return limitError("the box structure takes more than %d steps to walk", w.budget)
	}
	return nil
}

// level walks the boxes that fill [pos, end) at the given depth.
//
// cands are the offsets the chain of boxes being walked may start at, and visited
// the ones an earlier chain passed through; both are nil below the first level.
func (w *mp4Walk) level(pos, end int64, depth int, anyKind bool, cands []int64, visited []bool) error {
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
		if cands != nil && !printableType(typ) {
			return nil // where the start is a guess, bytes that are not a box end the chain
		}
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
		listed := mp4Containers[string(typ[:])]
		if listed || anyKind {
			if err := w.children(string(typ[:]), listed, pos+hdr, pos+boxSize, depth+1); err != nil {
				return err
			}
		}
		pos += boxSize
		if i, ok := slices.BinarySearch(cands, pos); ok {
			visited[i] = true
		}
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
	if len(offsets) == 1 {
		return w.level(offsets[0], end, depth, mp4AnyKind[typ], nil, nil)
	}
	// A chain that passes through another candidate offset contains the chain
	// that starts there, which can count no more: it is not walked again.
	slices.Sort(offsets)
	visited := make([]bool, len(offsets))
	base := w.stats
	best := base
	for i, off := range offsets {
		if visited[i] {
			continue
		}
		w.stats = base
		if err := w.level(off, end, depth, mp4AnyKind[typ], offsets, visited); err != nil {
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
	out := make([]int64, 0, 2)
	for s := start; s+8 <= end && s < start+mp4OffsetWindow; s++ {
		if err := w.step(); err != nil {
			return nil, err
		}
		head := w.peek(s, 8)
		if head == nil {
			break
		}
		size := int64(binary.BigEndian.Uint32(head))
		if (size == 1 || (size >= 8 && size <= end-s)) && printableType([4]byte(head[4:8])) {
			out = append(out, s)
		}
	}
	lead, ok := mp4StringForms[typ]
	if !ok {
		return out, nil
	}
	pos, nuls := start+int64(lead), 0
	for pos < end && pos < start+int64(lead)+mp4OffsetWindow && nuls < 3 {
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
		return nil, limitError("a %s box holds strings longer than %d bytes, and its children cannot be placed", typ, mp4OffsetWindow)
	}
	return out, nil
}

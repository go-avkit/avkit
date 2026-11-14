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

// mp4Containers are the boxes whose children the walk reads, by the payload
// offset their children start at: plain containers at 0, meta (a full box) at 4.
// They are the boxes mp4ff decodes into a tree; any other box is kept as bytes
// and costs what it holds.
var mp4Containers = map[string]int{
	"moov": 0, "trak": 0, "mdia": 0, "minf": 0, "stbl": 0, "dinf": 0, "edts": 0,
	"udta": 0, "mvex": 0, "moof": 0, "traf": 0, "mfra": 0, "sinf": 0, "schi": 0,
	"ilst": 0, "meta": 4,
}

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
	header [16]byte
}

// level walks the boxes that fill [pos, end) at the given depth.
func (w *mp4Walk) level(pos, end int64, depth int) error {
	// One buffer for the whole walk: it is handed to an interface, so a local
	// would be allocated at every level. Nothing of it is read after a descent.
	header := &w.header
	for end-pos >= 8 {
		if _, err := w.at.ReadAt(header[:8], pos); err != nil {
			return nil
		}
		w.stats.boxes++
		if w.stats.boxes > w.limits.MaxBoxes {
			return limitError("the file holds more than %d boxes", w.limits.MaxBoxes)
		}
		if depth > w.limits.MaxBoxDepth {
			return limitError("boxes nested more than %d deep", w.limits.MaxBoxDepth)
		}
		w.stats.depth = max(w.stats.depth, depth)
		boxSize, hdr := int64(binary.BigEndian.Uint32(header[:4])), int64(8)
		switch boxSize {
		case 0:
			boxSize = end - pos // to the end of the enclosing box
		case 1:
			if end-pos < 16 {
				return nil
			}
			if _, err := w.at.ReadAt(header[8:16], pos+8); err != nil {
				return nil
			}
			large := binary.BigEndian.Uint64(header[8:16])
			if large < 16 || large > uint64(end-pos) {
				return nil
			}
			boxSize, hdr = int64(large), 16
		}
		if boxSize < hdr || boxSize > end-pos {
			return nil
		}
		if skip, ok := mp4Containers[string(header[4:8])]; ok && boxSize-hdr >= int64(skip) {
			if err := w.level(pos+hdr+int64(skip), pos+boxSize, depth+1); err != nil {
				return err
			}
		}
		pos += boxSize
	}
	return nil
}

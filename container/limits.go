// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"fmt"
)

// The default bounds on what a reader will unpack from one file. They are
// ceilings: a caller states ReaderLimits to go lower, and a value above one of
// them is the ceiling again, so a caller can never be talked into more than the
// reader was written to hold.
const (
	// DefaultMaxTrackFrames is how many frames one track may hold: 1<<24, about
	// 1.85 times the largest track met in a library of 30 Matroska files,
	// a 4K remux whose TrueHD track holds 9,064,256 frames (one access unit
	// of 0.83 ms each). A lower default would refuse a real file.
	DefaultMaxTrackFrames = 1 << 24
	// DefaultMaxFrames is how many frames a whole file may hold, all tracks
	// together: 1<<25, about 3.5 times the largest file of that library
	// (9,493,297 frames).
	DefaultMaxFrames = 1 << 25
	// DefaultMaxBoxes is how many boxes an MP4 may hold, all levels together:
	// 1<<20, about 1,900 times the largest of a library of 3,466 MP4, MOV and
	// M4A files (557 boxes), and room for a day of one-second fragments of two
	// tracks (about 10 boxes each).
	DefaultMaxBoxes = 1 << 20
	// DefaultMaxBoxDepth is how deep MP4 boxes may nest: 64, mp4ff's own bound,
	// seven times the deepest file of that library (9).
	DefaultMaxBoxDepth = 64
)

// The worst case at these defaults is a file of one-byte laced frames: about 380
// bytes are allocated per input byte (367 to 381 measured), so a file of 34 MB
// can make a reader allocate 13 GB. For an MP4 it is a flood of empty 8-byte
// boxes: about 33.5 bytes per input byte, so a file of 8 MiB makes a reader
// allocate 280 MB. A caller with less memory than that, a wasm worker for one,
// states ReaderLimits that fit it.

// ErrReaderLimit means a file holds more than the reader is willing to unpack:
// the refusal is made from the file's own counts, before anything is allocated
// for what they state. It wraps a description of which bound was crossed.
var ErrReaderLimit = errors.New("container: the file exceeds a reader limit")

// ReaderLimits lowers the bounds a reader applies while it unpacks a file. The
// zero value, and any field left at zero or set above its default, means the
// default.
type ReaderLimits struct {
	// MaxTrackFrames bounds the frames of one track; see DefaultMaxTrackFrames.
	MaxTrackFrames int
	// MaxFrames bounds the frames of the whole file; see DefaultMaxFrames.
	MaxFrames int
	// MaxBoxes bounds the boxes of an MP4 file, all levels together; see
	// DefaultMaxBoxes.
	MaxBoxes int
	// MaxBoxDepth bounds how deep MP4 boxes nest; see DefaultMaxBoxDepth.
	MaxBoxDepth int
}

// bounded returns the limits a reader applies: each field is the caller's when
// that is positive and no higher than its default, and the default otherwise.
func (l ReaderLimits) bounded() ReaderLimits {
	if l.MaxTrackFrames <= 0 || l.MaxTrackFrames > DefaultMaxTrackFrames {
		l.MaxTrackFrames = DefaultMaxTrackFrames
	}
	if l.MaxFrames <= 0 || l.MaxFrames > DefaultMaxFrames {
		l.MaxFrames = DefaultMaxFrames
	}
	if l.MaxBoxes <= 0 || l.MaxBoxes > DefaultMaxBoxes {
		l.MaxBoxes = DefaultMaxBoxes
	}
	if l.MaxBoxDepth <= 0 || l.MaxBoxDepth > DefaultMaxBoxDepth {
		l.MaxBoxDepth = DefaultMaxBoxDepth
	}
	return l
}

// NewReaderWithLimits is NewReader with the caller's bounds, which can only be
// lower than the defaults. A file past a bound is refused with ErrReaderLimit.
func NewReaderWithLimits(data []byte, limits ReaderLimits) (*Reader, error) {
	return newReader(data, limits.bounded())
}

func limitError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrReaderLimit, fmt.Sprintf(format, args...))
}

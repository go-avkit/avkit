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
	// DefaultMaxTrackFrames is how many frames one track may hold. A million is
	// about four and a half hours of 60 fps video, or five and a half hours of
	// 20 ms Opus, in one track.
	DefaultMaxTrackFrames = 1 << 20
	// DefaultMaxFrames is how many frames a whole file may hold, all tracks
	// together.
	DefaultMaxFrames = 1 << 21
)

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

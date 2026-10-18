// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"fmt"
	"io"
	"math"

	"github.com/Eyevinn/mp4ff/mp4"
)

// SegmentIndex makes the Muxer index the file it writes: one segment index box
// (sidx, ISO/IEC 14496-12 §8.16.3) per track, after the initialisation segment
// and before the first fragment, naming every fragment's size, start and
// duration. That is what turns one fragmented MP4 into a DASH on-demand
// representation (ISO/IEC 23009-1 §8.3, an Indexed Self-Initializing Media
// Segment): a client reads the index with one range request, and fetches any
// fragment by its byte range from then on.
//
// The index states what follows it, so it cannot be written before the last
// fragment is. What the muxer does about that depends on the writer, the way
// ProgressiveMuxer's mdat does:
//
//   - A writer that can also read and seek — an [os.File] — is handed every
//     fragment as it is made. At Close the fragments are moved forward by the
//     size of the index, a block at a time, and the index is written in the
//     room that leaves: ffmpeg's global_sidx does the same. Nothing but one
//     block is held, and the cost is the media written twice.
//   - Any other writer has the fragments held in memory until Close, and the
//     media held may not exceed IndexMemoryLimit.
//
// One index box can reference 65535 fragments, which at the default two
// seconds a fragment is some 36 hours; a file needing more is refused rather
// than indexed in part.
func SegmentIndex() MuxOption {
	return func(s *muxSettings) { s.index = true }
}

// IndexMemoryLimit sets how much media a Muxer writing a segment index to a
// writer that cannot be read back holds before giving up with
// ErrMediaTooLarge. Zero restores the default, DefaultMediaMemoryLimit.
func IndexMemoryLimit(n uint64) MuxOption {
	return func(s *muxSettings) {
		if n == 0 {
			n = DefaultMediaMemoryLimit
		}
		s.indexMemoryLimit = n
	}
}

// maxIndexReferences is how many references one sidx can state: its
// reference_count is sixteen bits wide.
//
// It and maxReferencedSize are variables so a test can lower them, the way
// maxChunkOffset32 is: reaching either for real takes 65535 fragments or one of
// two gigabytes.
var maxIndexReferences = math.MaxUint16

// maxReferencedSize is the largest fragment a reference can state:
// referenced_size is thirty-one bits wide, the top bit saying whether the
// reference is to media or to another index.
var maxReferencedSize uint64 = 1<<31 - 1

// shiftBlock is how much of the file is moved at a time when room is made for
// the index. It is a variable so that a test can move a small file in several
// blocks and reach the loop's every branch.
var shiftBlock = 1 << 20

// segmentIndex is what a Muxer writing a sidx keeps: either the fragments
// themselves, or where in the file they start.
type segmentIndex struct {
	// file is the writer when it can be read back and seeked; nil otherwise.
	file io.ReadWriteSeeker
	// start is where the first fragment begins in file.
	start int64
	// held is the fragments of a writer that cannot be read back.
	held  bytes.Buffer
	limit uint64

	fragments []indexedFragment
}

// indexedFragment is what one reference of each track's index states about one
// fragment.
type indexedFragment struct {
	size  uint64
	spans []trackSpan // one per track, in the order the tracks were added
}

// trackSpan is one track's part of a fragment: when its media starts and ends
// on the presentation timeline, and whether a player can start decoding there.
type trackSpan struct {
	present bool
	// earliest is the earliest presentation time of the track's samples in
	// the fragment and end the latest time one of them presents until, both
	// in the track's timescale.
	earliest, end uint64
	// startsWithSAP says the first sample in decoding order is a sync sample,
	// and sapType is 1 when nothing in the fragment presents before it either,
	// which is ISO/IEC 14496-12 Annex I's type 1. A sync sample with frames
	// presenting before it may be type 2 or 3, which only the bitstream can
	// tell, so it is stated as 0, "unknown", as ffmpeg states every one.
	startsWithSAP bool
	sapType       uint8
}

// newSegmentIndex prepares the index of a Muxer writing to w.
func newSegmentIndex(w io.Writer, limit uint64) *segmentIndex {
	x := &segmentIndex{limit: limit}
	if f, ok := w.(io.ReadWriteSeeker); ok {
		x.file = f
	}
	return x
}

// begin notes where the fragments start, once the initialisation segment is
// out. Only a writer the fragments go straight to needs it.
func (x *segmentIndex) begin() error {
	if x.file == nil {
		return nil
	}
	pos, err := x.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("container: find where the fragments start: %w", err)
	}
	x.start = pos
	return nil
}

// spans describes every track's part of the fragment about to be written, from
// the samples it is made of. A presentation time before zero, which a negative
// composition offset can produce, is counted from zero, as ffmpeg does: the
// index cannot state one.
func spans(tracks []*muxTrack) []trackSpan {
	out := make([]trackSpan, len(tracks))
	for i, t := range tracks {
		if len(t.pending) == 0 {
			continue
		}
		span := &out[i]
		span.present = true
		var firstPTS uint64
		for k, s := range t.pending {
			pts := max(int64(s.DecodeTime)+int64(s.CompositionTimeOffset), 0)
			start, end := uint64(pts), uint64(pts)+uint64(s.Dur)
			if k == 0 {
				span.earliest, span.end, firstPTS = start, end, start
				span.startsWithSAP = s.Flags == mp4.SyncSampleFlags
				continue
			}
			span.earliest = min(span.earliest, start)
			span.end = max(span.end, end)
		}
		if span.startsWithSAP && firstPTS == span.earliest {
			span.sapType = 1
		}
	}
	return out
}

// write puts one fragment out, or holds it, and records it in the index. It is
// refused before anything is written when the index could not reference it.
//
// The size recorded is what was written rather than what the fragment said it
// would be: an encoder is free to tighten its track fragment headers on the
// way out, and a reference one byte off sends a player into the middle of a
// box. The checks use the size stated beforehand, which tightening can only
// lower.
func (x *segmentIndex) write(w io.Writer, frag *mp4.Fragment, spans []trackSpan) error {
	size := frag.Size()
	switch {
	case len(x.fragments) == maxIndexReferences:
		return fmt.Errorf("%w: a segment index references at most %d fragments",
			ErrMediaTooLarge, maxIndexReferences)
	case size > maxReferencedSize:
		return fmt.Errorf("%w: a fragment of %d bytes is larger than a segment index can reference",
			ErrMediaTooLarge, size)
	}
	if x.file == nil {
		if held := uint64(x.held.Len()) + size; held > x.limit {
			return fmt.Errorf("%w: %d bytes of fragments held for the segment index, limit %d",
				ErrMediaTooLarge, held, x.limit)
		}
		w = &x.held
	}
	counted := &webmCounter{w: w}
	if err := frag.Encode(counted); err != nil {
		return err
	}
	x.fragments = append(x.fragments, indexedFragment{size: counted.n, spans: spans})
	return nil
}

// boxes builds the index: one sidx per track that has samples, each referencing
// every fragment, in the order the tracks were added. A track absent from a
// fragment states a reference of no duration there, so that every box names
// the same byte ranges.
//
// Version 1 is written whatever the values, as ffmpeg does: its 64-bit fields
// cost eight bytes a track and leave no time or offset that could fail to fit.
func (x *segmentIndex) boxes(tracks []*muxTrack) ([]*mp4.SidxBox, error) {
	var out []*mp4.SidxBox
	for i, t := range tracks {
		sidx := &mp4.SidxBox{Version: 1, ReferenceID: t.id, Timescale: t.timescale}
		// The duration of a subsegment runs from its earliest presentation
		// time to the next one's, and the last one's to the end of the track
		// (§8.16.3.3), so the references are filled from the last backwards.
		sidx.SidxRefs = make([]mp4.SidxRef, len(x.fragments))
		next, seen := uint64(0), false
		for k := len(x.fragments) - 1; k >= 0; k-- {
			fragment := x.fragments[k]
			span := fragment.spans[i]
			ref := &sidx.SidxRefs[k]
			ref.ReferencedSize = uint32(fragment.size)
			if !span.present {
				continue
			}
			if !seen {
				next, seen = trackEnd(x.fragments, i), true
			}
			// Frames presenting out of order can make a fragment start
			// later than the next one does; that fragment then adds nothing
			// the next does not cover.
			duration := next - min(span.earliest, next)
			if duration > math.MaxUint32 {
				return nil, fmt.Errorf("%w: track %d has a fragment of %d ticks, more than a segment index can state",
					ErrMediaTooLarge, t.id, duration)
			}
			ref.SubSegmentDuration = uint32(duration)
			if span.startsWithSAP {
				ref.StartsWithSAP = 1
			}
			ref.SAPType = span.sapType
			sidx.EarliestPresentationTime = span.earliest
			next = min(next, span.earliest)
		}
		if seen {
			out = append(out, sidx)
		}
	}
	// Each box's first_offset is the distance from its end to the first
	// fragment, which is the boxes written after it.
	var after uint64
	for k := len(out) - 1; k >= 0; k-- {
		out[k].FirstOffset = after
		after += out[k].Size()
	}
	return out, nil
}

// trackEnd is the latest time a track's media presents until, over every
// fragment.
func trackEnd(fragments []indexedFragment, track int) uint64 {
	var end uint64
	for _, f := range fragments {
		end = max(end, f.spans[track].end)
	}
	return end
}

// finish writes the index in front of the fragments, and the fragments after
// it when they were held. A file holding no fragment has nothing to index and
// gets no index: an index of nothing is worse than none.
func (x *segmentIndex) finish(w io.Writer, tracks []*muxTrack) error {
	if len(x.fragments) == 0 {
		return nil
	}
	boxes, err := x.boxes(tracks)
	if err != nil {
		return err
	}
	var index bytes.Buffer
	for _, sidx := range boxes {
		// Encoding into memory, of boxes built from values already checked.
		_ = sidx.Encode(&index)
	}
	if x.file == nil {
		if _, err := w.Write(index.Bytes()); err != nil {
			return fmt.Errorf("container: write segment index: %w", err)
		}
		if _, err := x.held.WriteTo(w); err != nil {
			return fmt.Errorf("container: write fragments: %w", err)
		}
		return nil
	}
	end, err := x.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("container: find where the fragments end: %w", err)
	}
	room := int64(index.Len())
	if err := shiftForward(x.file, x.start, end, room); err != nil {
		return err
	}
	if _, err := x.file.Seek(x.start, io.SeekStart); err != nil {
		return fmt.Errorf("container: seek back to the segment index: %w", err)
	}
	if _, err := x.file.Write(index.Bytes()); err != nil {
		return fmt.Errorf("container: write segment index: %w", err)
	}
	if _, err := x.file.Seek(end+room, io.SeekStart); err != nil {
		return fmt.Errorf("container: seek past the fragments: %w", err)
	}
	return nil
}

// shiftForward moves the bytes of f between start and end forward by room,
// starting from the end so that nothing is overwritten before it is read.
func shiftForward(f io.ReadWriteSeeker, start, end, room int64) error {
	buf := make([]byte, shiftBlock)
	for at := end; at > start; {
		n := min(int64(len(buf)), at-start)
		at -= n
		if _, err := f.Seek(at, io.SeekStart); err != nil {
			return fmt.Errorf("container: make room for the segment index: %w", err)
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return fmt.Errorf("container: make room for the segment index: read at %d: %w", at, err)
		}
		if _, err := f.Seek(at+room, io.SeekStart); err != nil {
			return fmt.Errorf("container: make room for the segment index: %w", err)
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return fmt.Errorf("container: make room for the segment index: write at %d: %w", at+room, err)
		}
	}
	return nil
}

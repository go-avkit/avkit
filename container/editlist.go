// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"math"
	"slices"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Where a track's presentation begins.
//
// A Sample states its duration and how far its presentation runs behind its
// decoding; the muxers add the durations up from zero. A track that reorders
// its frames -- H.264 or HEVC with B-frames -- therefore decodes its first
// frame at zero and presents it at its composition offset: two frames later
// for x264's default of two B-frames. Nothing in the samples says that the
// presentation should start there rather than at zero, and a file that does not
// say it either shows the first picture that much late: audio and subtitles,
// whose samples present when they decode, run ahead of it by that much.
//
// ISO/IEC 14496-12 §8.6.6.1 names the remedy: an edit "used to establish a
// presentation time of 0 for the first presented sample, when composition
// offsets are used". Its media_time is in composition time (§8.6.6.3), so an
// edit starting at the first sample's composition offset presents that sample
// at zero, and a priming stated as StartDelay is skipped after it. It is what
// ffmpeg's movenc writes (mov_write_edts_tag, from start_dts + start_cts), and
// its own fragmented output only gets it right with -movflags delay_moov, which
// holds the header back until the first fragment for the same reason Muxer now
// does.
//
// Negative composition offsets (a version 1 ctts, §8.6.1.3, with a cslg,
// §8.6.1.4) would avoid the edit list instead: decode times move, presentation
// times stay. It is not used here. It changes every sample's offset rather than
// adding one box, it needs a brand that permits a version 1 ctts, and ffmpeg's
// own reading of it in a fragmented file starts the track at 0.040 s where the
// edit list gives 0.000, measured on the same source.
//
// The composition time of the FIRST sample is the one used, as movenc's
// start_cts is, rather than the smallest of all of them. The first sample is a
// sync sample, the one a decoder starts at; a leading picture presenting before
// it belongs to a group of pictures that began before the stream did, and a
// decoder starting here discards it.

// primingTicks is a StartDelay in a track's own timescale, rounded to the
// NEAREST tick. A timescale too coarse to hold it exactly is common -- Matroska
// counts in milliseconds by default, so a 21.333 ms AAC priming becomes 21 --
// and rounding down would leave a sliver of the priming audible where rounding
// to nearest does not. A delay shorter than half a tick is none at all: a whole
// tick would move the track by more than the delay it describes.
func primingTicks(delay time.Duration, timescale uint32) int64 {
	if delay <= 0 {
		return 0
	}
	return (int64(delay)*int64(timescale) + int64(time.Second)/2) / int64(time.Second)
}

// presentationStart is the media time, in the track's timescale and in
// composition time, at which a track's presentation begins: the composition
// time of its first sample -- its offset, since it decodes at zero -- and then
// the priming after it. A first sample presenting before it decodes, which a
// version 1 ctts can state, starts at zero: an edit cannot reach before the
// media does.
func presentationStart(firstOffset int32, priming int64) int64 {
	return max(int64(firstOffset), 0) + priming
}

// presentationEdits is the edit box starting a track's presentation at start,
// for segment, or nil when it starts at zero and no edit list is needed. A
// segment of zero is what a fragmented file writes: its header goes out before
// the length of the track is known, and §8.6.6.1 allows an edit of duration
// zero there, reaching to the end of the media in every fragment that follows.
func presentationEdits(start int64, segment uint64) *mp4.EdtsBox {
	if start <= 0 {
		return nil
	}
	return newEdts(mp4.ElstEntry{SegmentDuration: segment, MediaTime: start, MediaRateInteger: 1})
}

// newEdts builds an edit box holding one edit list, in the 32-bit form of
// §8.6.6.2 unless one of the entries only fits the 64-bit one.
func newEdts(entries ...mp4.ElstEntry) *mp4.EdtsBox {
	elst := &mp4.ElstBox{Entries: entries}
	if slices.ContainsFunc(entries, func(e mp4.ElstEntry) bool {
		return e.SegmentDuration > math.MaxUint32 ||
			e.MediaTime > math.MaxInt32 || e.MediaTime < math.MinInt32
	}) {
		elst.Version = 1
	}
	edts := &mp4.EdtsBox{}
	// AddChild appends to Children, which is what the encoder writes; Elst is
	// filled only when a file is DECODED. Setting both keeps the tree in memory
	// saying the same thing as the bytes it will become.
	edts.AddChild(elst)
	edts.Elst = append(edts.Elst, elst)
	return edts
}

// setEdits puts an edit box in a track box where ISO/IEC 14496-12 §8.3.1 and
// the box order of §6.2.3 place it: after the track header, before the media
// box. A reader finds it anywhere, but a file written in the order the standard
// lists is one less thing for a strict one to object to.
//
// Both muxers build their track boxes with mp4ff, which always gives them a
// media box, so there is always one to be placed before.
func setEdits(trak *mp4.TrakBox, edts *mp4.EdtsBox) {
	trak.Edts = edts
	at := slices.IndexFunc(trak.Children, func(b mp4.Box) bool { return b.Type() == "mdia" })
	trak.Children = slices.Insert(trak.Children, at, mp4.Box(edts))
}

// scaleDurationUp converts a duration between two timescales, rounding UP: an
// edit a fraction of a tick short would cut the end of the last sample off,
// which is why movenc rounds the same way (av_rescale_rnd, AV_ROUND_UP).
func scaleDurationUp(d uint64, from, to uint32) uint64 {
	down := scaleDuration(d, from, to)
	if scaleDuration(down, to, from) < d {
		down++
	}
	return down
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// The clock an audio track is counted in.
//
// A Matroska track counts in the segment's ticks, a millisecond by default, and
// a track copied from one keeps that timescale. In an MP4 it costs the priming.
// ffmpeg reads an edit's media_time for an audio track as a number of SAMPLES:
// the edit-list pass of mov.c (mov_fix_index) drops the frames that end before
// media_time, and for the frame it lands inside it adds
// "packet_skip_samples = edit_list_media_time - curr_cts" to the stream's
// skip_samples -- a difference of timescale ticks, used as a count of samples.
// With the track at the sample rate the two are the same number; at 1000 Hz an
// edit of 25 ms at media time 25 skips 25 samples where 1105 were meant, and the
// decoder presents the encoder's priming that the edit list says to leave out.
// Measured with ffmpeg 9.0.2: avkit's MP4 copy of its own 1 s, 44 100-sample MP3
// Matroska decoded 45 168 samples.
//
// movenc avoids it by construction: "track->timescale = st->codecpar->sample_rate"
// for every audio track (mov_init), so a priming is a count of samples in the
// edit it writes (mov_write_edts_tag). Muxer and ProgressiveMuxer do the same
// for a track whose timescale is coarser than its sample rate, converting what
// is counted in it -- sample durations, composition offsets, and the media times
// of an edit list -- when the track is added and its samples written. A caller
// keeps counting in the timescale the track came in.

// audioTimescale is the media timescale an audio track is written to an MP4
// with: its sample rate, when the timescale it came in is too coarse to count
// samples in; otherwise the one it came in. An Opus decoder outputs 48 kHz
// whatever rate the stream was recorded at, which is what movenc's codecpar
// says as well. Anything that is not audio, or does not state a rate, keeps its
// timescale.
func audioTimescale(cfg TrackConfig) uint32 {
	rate := cfg.SampleRate
	if cfg.Codec == "Opus" || cfg.Codec == "opus" {
		rate = opusOutputRate
	}
	if cfg.Kind != Audio || rate <= 0 || uint64(rate) > maxUint32 || uint32(rate) <= cfg.Timescale {
		return cfg.Timescale
	}
	return uint32(rate)
}

// audioClock converts the samples of one track from the timescale they are
// counted in to the one the track is written with.
type audioClock struct {
	from, to uint32
	// in and out are the track's total so far in each: a duration is converted
	// as the difference between two converted totals, so the rounding of one
	// sample is made up by the next and the track keeps its length.
	in, out uint64
}

// withAudioClock returns the configuration a muxer writes for cfg, and the
// clock that converts its samples; a nil clock when the track is written as it
// came.
func withAudioClock(cfg TrackConfig) (TrackConfig, *audioClock) {
	to := audioTimescale(cfg)
	if to == cfg.Timescale {
		return cfg, nil
	}
	c := &audioClock{from: cfg.Timescale, to: to}
	if cfg.EditList != nil {
		el := *cfg.EditList
		el.Edits = slices.Clone(el.Edits)
		// A priming the source could only state to the tick -- 25 ms for 25.057 --
		// is stated in full when the edit is the one the priming made.
		priming := primingTicks(cfg.StartDelay, cfg.Timescale)
		for i, e := range el.Edits {
			switch {
			case e.MediaTime == EmptyEdit:
			case e.MediaTime == priming && priming > 0:
				el.Edits[i].MediaTime = primingTicks(cfg.StartDelay, to)
			default:
				el.Edits[i].MediaTime = int64(scaleDurationNearest(uint64(e.MediaTime), cfg.Timescale, to))
			}
		}
		cfg.EditList = &el
	}
	cfg.Timescale = to
	return cfg, c
}

// sample converts one sample's durations. A nil clock leaves it alone.
func (c *audioClock) sample(s Sample) (Sample, error) {
	if c == nil {
		return s, nil
	}
	in := c.in + uint64(s.Duration)
	out, ok := scaleNearest(in, c.from, c.to)
	offset, fits := scaleNearest(uint64(max(s.CompositionOffset, -s.CompositionOffset)), c.from, c.to)
	if !ok || !fits || out-c.out > math.MaxUint32 || offset > math.MaxInt32 {
		return s, fmt.Errorf("%w: a time of %d (offset %d) in a timescale of %d does not fit one of %d",
			ErrSample, in, s.CompositionOffset, c.from, c.to)
	}
	s.Duration = uint32(out - c.out)
	c.in, c.out = in, out
	if s.CompositionOffset < 0 {
		s.CompositionOffset = -int32(offset)
	} else {
		s.CompositionOffset = int32(offset)
	}
	return s, nil
}

// scaleNearest is d converted between two timescales, rounded to the nearest
// unit, and whether it fits 64 bits.
func scaleNearest(d uint64, from, to uint32) (uint64, bool) {
	hi, lo := bits.Mul64(d, uint64(to))
	lo, carry := bits.Add64(lo, uint64(from)/2, 0)
	hi += carry
	if hi >= uint64(from) {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, uint64(from))
	return q, true
}

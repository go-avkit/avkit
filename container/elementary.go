// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"
)

// The formats this file registers readers for -- ADTS, native FLAC, Ogg, WAV --
// have no sample table and no index a reader could address
// into: a demuxer walks them from the first byte to the last, the way it walks
// a transport stream or a Matroska file. Each is read whole, in one pass, into
// the same per-track record the Matroska reader keeps, so Reader.Samples and
// Reader.TrackConfig serve them without knowing which one they came from.
//
// They are registered rather than listed in Sniff, so that each format stays in
// a file of its own and adding one does not mean editing the others.

// ErrMalformed means an elementary stream or a legacy container breaks its own
// syntax at a point where reading on would mean guessing.
var ErrMalformed = errors.New("container: malformed stream")

// elementaryFormat is one format read in a single pass.
type elementaryFormat struct {
	format Format
	// name is what File.Format reports and what an error calls it.
	name string
	// order decides which sniffer looks first. Formats whose signatures
	// cannot collide may share an order; the ones that are recognised by
	// their frames rather than by a magic number come after those that are.
	order int
	sniff func(data []byte) bool
	// read returns the tracks, and ErrTruncated beside them when the data
	// stopped part way through a frame and only what came before was kept.
	read func(data []byte) ([]*mkvReadTrack, error)
}

// The orders Sniff tries the registered formats in: those that open with a
// magic number first, then those recognised by their frames, because a sync
// word is weaker evidence than four letters.
const (
	orderMagic      = 10
	orderSyncFrames = 20
)

// elementaryFormats are the registered formats, in the order Sniff tries them.
var elementaryFormats []elementaryFormat

// registerElementary adds a format to the ones Sniff, Demux and NewReader know.
func registerElementary(f elementaryFormat) {
	elementaryFormats = append(elementaryFormats, f)
	sort.SliceStable(elementaryFormats, func(i, j int) bool {
		return elementaryFormats[i].order < elementaryFormats[j].order
	})
}

// sniffElementary is the first registered format whose signature data carries.
func sniffElementary(data []byte) Format {
	for _, f := range elementaryFormats {
		if f.sniff(data) {
			return f.format
		}
	}
	return FormatUnknown
}

// elementaryFor is the registered format, if format is one.
func elementaryFor(format Format) (elementaryFormat, bool) {
	for _, f := range elementaryFormats {
		if f.format == format {
			return f, true
		}
	}
	return elementaryFormat{}, false
}

// newElementaryReader reads a registered format whole.
//
// A stream that stopped part way through a frame is returned WITH ErrTruncated,
// as a Matroska file is: a caller writing the ordinary `if err != nil` keeps the
// refusal, and one that asks for the frames before the cut gets them.
func newElementaryReader(f elementaryFormat, data []byte) (*Reader, error) {
	tracks, err := f.read(data)
	if err != nil && !errors.Is(err, ErrTruncated) {
		return nil, err
	}
	r := &Reader{data: data, mkv: map[uint32]*mkvReadTrack{}, truncated: err != nil}
	file := &File{Format: f.name}
	for _, t := range tracks {
		for _, s := range t.samples {
			t.track.Duration += uint64(s.Duration)
		}
		if len(t.samples) == 0 && t.samplesErr == nil {
			t.samplesErr = errorf(ErrNoSamples, "%s track %d holds no frame", f.name, t.track.ID)
		}
		r.mkv[t.track.ID] = t
		file.Tracks = append(file.Tracks, t.track)
	}
	file.Timescale, file.Duration = fileTiming(file.Tracks)
	r.file = file
	return r, err
}

// microsecond is the timescale a file whose tracks run on different clocks
// states its overall duration in.
const microsecond = 1_000_000

// fileTiming is the clock a file's overall duration is stated in, and that
// duration: the track's own clock when there is one track, or every clock the
// tracks share, and microseconds when they share none.
func fileTiming(tracks []Track) (uint32, uint64) {
	if len(tracks) == 0 {
		return 0, 0
	}
	scale := tracks[0].Timescale
	for _, t := range tracks[1:] {
		if t.Timescale != scale {
			scale = microsecond
			break
		}
	}
	var longest uint64
	for _, t := range tracks {
		if d := rescale(t.Duration, t.Timescale, scale); d > longest {
			longest = d
		}
	}
	return scale, longest
}

// rescale converts a count of ticks from one clock to another, rounding down.
//
// It multiplies before it divides, in 128 bits, because a length field of a
// hostile file can make the product overflow 64 and a wrapped duration is a
// wrong one that says nothing; and it divides in integers, because a
// float-to-integer conversion is what a known loong64 code-generation defect
// mistranslates (see the CI workflow).
func rescale(ticks uint64, from, to uint32) uint64 {
	if from == 0 || from == to {
		return ticks
	}
	hi, lo := bits.Mul64(ticks, uint64(to))
	if hi >= uint64(from) {
		return ^uint64(0) // more than 64 bits can state; saturate
	}
	q, _ := bits.Div64(hi, lo, uint64(from))
	return q
}

// errorf wraps a sentinel with a message, the form every refusal here takes.
func errorf(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

// oneTrack is the result of a format that carries a single track, so the
// formats with one elementary stream report it the same way.
func oneTrack(t *mkvReadTrack, err error) ([]*mkvReadTrack, error) {
	return []*mkvReadTrack{t}, err
}

// audioTrack is the record of a single audio track, numbered 1.
func audioTrack(codec string, timescale uint32, channels, rate int) *mkvReadTrack {
	return &mkvReadTrack{
		track: Track{ID: 1, Kind: Audio, Codec: codec, Channels: channels,
			SampleRate: rate, Timescale: timescale, Language: "und"},
		config: TrackConfig{Kind: Audio, Codec: codec, Channels: channels,
			SampleRate: rate, Timescale: timescale},
	}
}

// truncatedAt says where a stream stopped being whole.
func truncatedAt(format string, offset, size int) error {
	return fmt.Errorf("%w: %s stops at byte %d of a frame that needs %d more",
		ErrTruncated, format, offset, size)
}

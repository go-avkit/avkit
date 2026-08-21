// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"slices"
	"time"
)

// Errors reported by the remux helpers.
var (
	// ErrTrackMismatch means two inputs cannot be joined as they are,
	// because a decoder would have to be reconfigured between them.
	ErrTrackMismatch = errors.New("container: tracks do not match")
	// ErrTimeRange means the range asked for cannot be cut.
	ErrTimeRange = errors.New("container: invalid time range")
	// ErrNoSyncSample means a track offers no sample a decoder may start at,
	// so a cut cannot begin there.
	ErrNoSyncSample = errors.New("container: no sync sample to cut at")
)

// nsScale is the timescale a time.Duration counts in.
const nsScale = uint32(time.Second / time.Nanosecond)

// RemuxOption configures a remux operation.
type RemuxOption func(*remuxSettings)

type remuxSettings struct {
	mux        []MuxOption
	drop       map[uint32]bool
	strictSync bool
}

// MuxOptions passes options on to the Muxer writing the output.
func MuxOptions(opts ...MuxOption) RemuxOption {
	return func(s *remuxSettings) { s.mux = append(s.mux, opts...) }
}

// DropTracks leaves the named input tracks out of the output — keeping the
// video and discarding a commentary track is this and nothing else. The
// identifiers are the input's own, as Reader.TrackIDs reports them, and one
// that names no track is an error rather than a silent no-op.
//
// It is an option rather than an operation of its own so that it composes:
// tracks can be dropped while cutting or while concatenating.
func DropTracks(trackIDs ...uint32) RemuxOption {
	return func(s *remuxSettings) {
		if s.drop == nil {
			s.drop = map[uint32]bool{}
		}
		for _, id := range trackIDs {
			s.drop[id] = true
		}
	}
}

// RequireSyncStart makes Cut refuse a start that does not land exactly on a
// sync sample, instead of snapping back to the one before it. It is for a
// caller who would rather be told than handed a clip that begins earlier than
// asked.
func RequireSyncStart() RemuxOption {
	return func(s *remuxSettings) { s.strictSync = true }
}

func settingsFor(opts []RemuxOption) *remuxSettings {
	set := &remuxSettings{}
	for _, opt := range opts {
		opt(set)
	}
	return set
}

// sourceTrack is one input track selected for copying: how it is declared, and
// the samples queued behind it.
type sourceTrack struct {
	id      uint32
	cfg     TrackConfig
	samples []Sample
}

// Remux copies every track of src into a fragmented MP4 on w: it reads each
// track's configuration and samples, declares them all, and writes them
// interleaved by decode time so a player can read the result in one pass. It
// is the loop a caller otherwise writes by hand, and the foundation of Cut and
// Concat.
//
// Nothing is re-encoded and no sample's bytes are touched. What the output
// cannot carry over is timing the muxer does not express: each track's timeline
// restarts at zero and advances by its own sample durations, so edit lists and
// gaps in the input are not reproduced.
//
// A track whose samples cannot be read fails the whole copy, because the reader
// cannot tell a deliberately empty track from an unreadable sample table;
// DropTracks is how a caller who knows better leaves it out.
func Remux(w io.Writer, src *Reader, opts ...RemuxOption) error {
	return Concat(w, []*Reader{src}, opts...)
}

// Cut copies the samples of src that fall between start and end, as Remux
// otherwise would. An end of zero or less means "to the end of the input".
//
// A cut can only begin at a sync sample: a decoder handed anything else cannot
// reconstruct the first pictures. So start is snapped *back* to the last sync
// sample at or before it, which means the output may begin earlier than asked —
// by up to one group of pictures. RequireSyncStart turns that snap into an
// error instead. When the cut falls before the first sync sample of all, the
// output begins at that one, later than asked.
//
// The sync grid of the first video track decides where the cut lands; every
// other track then starts at its own last sync sample at or before that point.
// Since each track's output timeline restarts at zero, a track whose sync grid
// is coarser than the video's is advanced against it by that difference — up to
// one audio frame in practice. Sample-accurate alignment would need an edit
// list, which the muxer does not write.
//
// Samples are kept whole: end excludes the first sample that starts at or after
// it rather than truncating one, and a range that selects nothing still writes
// a file naming its tracks.
func Cut(w io.Writer, src *Reader, start, end time.Duration, opts ...RemuxOption) error {
	if start < 0 || (end > 0 && end <= start) {
		return fmt.Errorf("%w: %v to %v", ErrTimeRange, start, end)
	}
	set := settingsFor(opts)
	tracks, err := readSource(src, set)
	if err != nil {
		return err
	}
	if err := trim(tracks, start, end, set.strictSync); err != nil {
		return err
	}
	return writeCopy(w, set, [][]sourceTrack{tracks})
}

// Concat writes srcs one after another into a single fragmented MP4, tracks
// paired by position in Reader.TrackIDs order — the first input's tracks are
// the ones the output declares.
//
// Only inputs whose paired tracks would not force a decoder to be reconfigured
// can be joined this way; anything else is refused with an error naming the
// track and what differs about it. Language is not part of that comparison: it
// says nothing about how a sample decodes, and the first input's wins.
//
// Timestamps need no shifting: each track's clock continues from the durations
// already written, so the second input follows the first instead of overlapping
// it. The consequence is that a track drifts against its siblings by whatever
// their durations differ by inside each input — go-avkit inserts no silence and
// duplicates no frame to hide it.
func Concat(w io.Writer, srcs []*Reader, opts ...RemuxOption) error {
	if len(srcs) == 0 {
		return fmt.Errorf("%w: no input", ErrNoTracks)
	}
	set := settingsFor(opts)
	inputs := make([][]sourceTrack, 0, len(srcs))
	for i, src := range srcs {
		tracks, err := readSource(src, set)
		if err != nil {
			// One input among several is worth naming; on its own it is
			// not, and Remux delegates here.
			if len(srcs) > 1 {
				return fmt.Errorf("input %d: %w", i+1, err)
			}
			return err
		}
		inputs = append(inputs, tracks)
	}
	if err := checkJoinable(inputs); err != nil {
		return err
	}
	return writeCopy(w, set, inputs)
}

// trackConfig exists so the failure it guards can be tested: every identifier
// handed to it comes from TrackIDs, so no reader can fail to describe one
// today — but swallowing the error would leave a later one unreported.
var trackConfig = (*Reader).TrackConfig

// readSource picks the tracks to copy, in input order, and reads each one whole.
func readSource(src *Reader, set *remuxSettings) ([]sourceTrack, error) {
	if src == nil {
		return nil, fmt.Errorf("%w: no reader", ErrNoTracks)
	}
	ids := src.TrackIDs()
	for id := range set.drop {
		if !slices.Contains(ids, id) {
			return nil, fmt.Errorf("%w: %d cannot be dropped", ErrUnknownTrack, id)
		}
	}
	out := make([]sourceTrack, 0, len(ids))
	for _, id := range ids {
		if set.drop[id] {
			continue
		}
		cfg, err := trackConfig(src, id)
		if err != nil {
			return nil, err
		}
		samples, err := src.Samples(id)
		if err != nil {
			return nil, err
		}
		out = append(out, sourceTrack{id: id, cfg: cfg, samples: samples})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: every track was dropped", ErrNoTracks)
	}
	return out, nil
}

// writeCopy declares the first input's tracks, then writes every input's
// samples behind one another.
func writeCopy(w io.Writer, set *remuxSettings, inputs [][]sourceTrack) error {
	m := NewMuxer(w, set.mux...)
	// Every track is declared before the first sample: the initialisation
	// segment names them all.
	outIDs := make([]uint32, len(inputs[0]))
	for i, t := range inputs[0] {
		id, err := m.AddTrack(t.cfg)
		if err != nil {
			return err
		}
		outIDs[i] = id
	}
	for _, in := range inputs {
		if err := interleave(m, outIDs, in); err != nil {
			return err
		}
	}
	return m.Close()
}

// interleave writes the tracks of one input in decode-time order, so a fragment
// holds the same stretch of media for every track and a player can read the
// output without seeking back. Tracks at the same time are written in input
// order.
func interleave(m *Muxer, outIDs []uint32, tracks []sourceTrack) error {
	next := make([]int, len(tracks))
	at := make([]uint64, len(tracks))
	for {
		pick := -1
		for i := range tracks {
			if next[i] >= len(tracks[i].samples) {
				continue
			}
			if pick < 0 || compareTimes(at[i], tracks[i].cfg.Timescale,
				at[pick], tracks[pick].cfg.Timescale) < 0 {
				pick = i
			}
		}
		if pick < 0 {
			return nil
		}
		s := tracks[pick].samples[next[pick]]
		if err := m.WriteSample(outIDs[pick], s); err != nil {
			return err
		}
		at[pick] += uint64(s.Duration)
		next[pick]++
	}
}

// compareTimes orders a/aScale against b/bScale the way cmp.Compare does. The
// two times are compared as the rationals they are, cross-multiplied in 128
// bits: tracks with unrelated timescales then need neither a common unit that
// could overflow nor a division a timescale of zero would make unsafe.
func compareTimes(a uint64, aScale uint32, b uint64, bScale uint32) int {
	ahi, alo := bits.Mul64(a, uint64(bScale))
	bhi, blo := bits.Mul64(b, uint64(aScale))
	if c := cmp.Compare(ahi, bhi); c != 0 {
		return c
	}
	return cmp.Compare(alo, blo)
}

// checkJoinable refuses inputs a concatenation would have to re-encode.
func checkJoinable(inputs [][]sourceTrack) error {
	first := inputs[0]
	for i, in := range inputs[1:] {
		if len(in) != len(first) {
			return fmt.Errorf("%w: input %d copies %d track(s), input 1 copies %d",
				ErrTrackMismatch, i+2, len(in), len(first))
		}
		for j := range in {
			if what := mismatch(first[j].cfg, in[j].cfg); what != "" {
				return fmt.Errorf("%w: input %d track %d against input 1 track %d: %s",
					ErrTrackMismatch, i+2, in[j].id, first[j].id, what)
			}
		}
	}
	return nil
}

// mismatch says what would stop two tracks being joined, or nothing when they
// can be. The track kind is not compared: it follows from the codec, which is.
func mismatch(a, b TrackConfig) string {
	switch {
	case a.Codec != b.Codec:
		return fmt.Sprintf("codec %q against %q", b.Codec, a.Codec)
	case a.Timescale != b.Timescale:
		return fmt.Sprintf("timescale %d against %d", b.Timescale, a.Timescale)
	case a.Width != b.Width || a.Height != b.Height:
		return fmt.Sprintf("frame size %dx%d against %dx%d", b.Width, b.Height, a.Width, a.Height)
	case a.SampleRate != b.SampleRate || a.Channels != b.Channels:
		return fmt.Sprintf("audio %dch/%dHz against %dch/%dHz",
			b.Channels, b.SampleRate, a.Channels, a.SampleRate)
	case a.AudioObjectType != b.AudioObjectType:
		return fmt.Sprintf("AAC profile %d against %d", b.AudioObjectType, a.AudioObjectType)
	case !sameParameters(a, b):
		return "the parameter sets differ"
	}
	return ""
}

// sameParameters reports whether two tracks carry the same codec parameters —
// what a decoder is set up from, and what therefore may not change mid-file.
func sameParameters(a, b TrackConfig) bool {
	return slices.EqualFunc(a.SPS, b.SPS, bytes.Equal) &&
		slices.EqualFunc(a.PPS, b.PPS, bytes.Equal) &&
		slices.EqualFunc(a.VPS, b.VPS, bytes.Equal) &&
		bytes.Equal(a.CodecConfig, b.CodecConfig)
}

// trim keeps in every track the samples the cut asks for.
func trim(tracks []sourceTrack, start, end time.Duration, strict bool) error {
	primary := primaryIndex(tracks)
	_, cutAt, err := syncStart(tracks[primary], uint64(start), nsScale)
	if err != nil {
		return err
	}
	scale := tracks[primary].cfg.Timescale
	if strict && compareTimes(cutAt, scale, uint64(start), nsScale) != 0 {
		return fmt.Errorf("%w: track %d has none at %v, the nearest usable one is at %.3fs",
			ErrNoSyncSample, tracks[primary].id, start, float64(cutAt)/float64(scale))
	}
	for i := range tracks {
		// The primary track is asked again, now about the point it chose
		// itself: the answer is the same sample, and the loop stays one.
		from, _, err := syncStart(tracks[i], cutAt, scale)
		if err != nil {
			return err
		}
		tracks[i].samples = cutRange(tracks[i], from, end)
	}
	return nil
}

// primaryIndex is the track whose sync samples decide where a cut lands: the
// first video track, since video is what a cut in the wrong place breaks.
func primaryIndex(tracks []sourceTrack) int {
	for i, t := range tracks {
		if t.cfg.Kind == Video {
			return i
		}
	}
	return 0
}

// syncStart is the sample a track has to begin at for a cut placed at
// at/atScale to decode: the last sync sample at or before that point, or the
// very first sync sample when the cut falls before any of them. It returns the
// sample's index and its decode time in the track's own units.
func syncStart(t sourceTrack, at uint64, atScale uint32) (int, uint64, error) {
	index, when, dt := -1, uint64(0), uint64(0)
	for i, s := range t.samples {
		if s.Sync && (index < 0 || compareTimes(dt, t.cfg.Timescale, at, atScale) <= 0) {
			index, when = i, dt
		}
		dt += uint64(s.Duration)
	}
	if index < 0 {
		return 0, 0, fmt.Errorf("%w: track %d has none at all", ErrNoSyncSample, t.id)
	}
	return index, when, nil
}

// cutRange returns the samples from index from up to the first one starting at
// or after end, which a zero or negative end leaves unbounded.
func cutRange(t sourceTrack, from int, end time.Duration) []Sample {
	if end <= 0 {
		return t.samples[from:]
	}
	dt := uint64(0)
	for _, s := range t.samples[:from] {
		dt += uint64(s.Duration)
	}
	for i := from; i < len(t.samples); i++ {
		if compareTimes(dt, t.cfg.Timescale, uint64(end), nsScale) >= 0 {
			return t.samples[from:i]
		}
		dt += uint64(t.samples[i].Duration)
	}
	return t.samples[from:]
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
)

// Errors reported when writing a container.
var (
	// ErrNoTracks means Close or a write was reached before any track was
	// declared.
	ErrNoTracks = errors.New("container: no track to write")
	// ErrUnknownTrack means the sample names a track this muxer does not
	// have.
	ErrUnknownTrack = errors.New("container: unknown track")
	// ErrTrackConfig means a track configuration is incomplete or does not
	// describe what its codec needs.
	ErrTrackConfig = errors.New("container: invalid track configuration")
	// ErrUnsupportedCodec means the codec cannot be described in a sample
	// entry by this muxer yet.
	ErrUnsupportedCodec = errors.New("container: unsupported codec")
	// ErrClosed means the muxer was used after Close.
	ErrClosed = errors.New("container: muxer is closed")
	// ErrSample means the sample cannot be written as given.
	ErrSample = errors.New("container: invalid sample")
)

// DefaultFragmentDuration is how much media a fragment holds when the caller
// states nothing: long enough to keep the overhead low, short enough that a
// player can start on the first one.
const DefaultFragmentDuration = 2 * time.Second

// DefaultBrand is the major brand written in ftyp.
const DefaultBrand = "iso5"

// Sample is one coded frame, in its track's own timescale.
type Sample struct {
	// Data is the coded frame, in the length-prefixed form the container
	// expects for AVC and HEVC.
	Data []byte
	// Duration is how long the frame lasts, in the track's timescale.
	Duration uint32
	// CompositionOffset shifts presentation against decoding, for streams
	// that reorder frames.
	CompositionOffset int32
	// Sync marks a frame a player can start decoding at.
	Sync bool
}

// TrackConfig describes a track to write. What a codec needs to be described
// differs, so the fields a codec ignores may be left at zero.
type TrackConfig struct {
	Kind Kind
	// Codec is the sample entry to write: "avc1", "avc3", "hvc1", "hev1",
	// "av01", "mp4a".
	Codec string
	// Timescale is the unit of every duration of this track, per second.
	Timescale uint32
	// Width and Height are the frame size, for a video track.
	Width, Height int
	// Channels and SampleRate describe an audio track.
	Channels, SampleRate int
	// Language is an ISO-639-2 code; "und" when empty.
	Language string
	// SPS, PPS and VPS are the parameter sets of an AVC or HEVC track, as
	// raw NAL units without a start code.
	SPS, PPS, VPS [][]byte
	// CodecConfig is the codec configuration record of a track whose
	// parameters are not NAL units, such as the av1C payload of AV1.
	CodecConfig []byte
	// AudioObjectType selects the AAC profile; 0 means AAC-LC.
	AudioObjectType byte
}

// MuxOption configures a Muxer.
type MuxOption func(*muxSettings)

type muxSettings struct {
	fragmentDuration time.Duration
	brand            string
}

// FragmentDuration sets how much media a fragment holds. A value of zero or
// less restores the default.
func FragmentDuration(d time.Duration) MuxOption {
	return func(s *muxSettings) {
		if d <= 0 {
			d = DefaultFragmentDuration
		}
		s.fragmentDuration = d
	}
}

// Brand sets the major brand written in ftyp.
func Brand(b string) MuxOption {
	return func(s *muxSettings) {
		if b != "" {
			s.brand = b
		}
	}
}

// Muxer writes a fragmented MP4: an initialisation segment naming every track,
// then one fragment after another.
//
// It is what joining separately delivered streams needs — a DASH presentation
// keeps video and audio apart — and it never re-encodes: samples are written as
// handed over.
type Muxer struct {
	w        io.Writer
	settings muxSettings

	init     *mp4.InitSegment
	tracks   []*muxTrack
	byID     map[uint32]*muxTrack
	started  bool
	closed   bool
	sequence uint32
}

// muxTrack is one track's writing state.
type muxTrack struct {
	id        uint32
	timescale uint32
	nextTime  uint64
	pending   []mp4.FullSample
	buffered  uint64 // duration of pending samples, in timescale units
}

// NewMuxer returns a Muxer writing to w.
func NewMuxer(w io.Writer, opts ...MuxOption) *Muxer {
	settings := muxSettings{fragmentDuration: DefaultFragmentDuration, brand: DefaultBrand}
	for _, opt := range opts {
		opt(&settings)
	}
	return &Muxer{w: w, settings: settings, byID: map[uint32]*muxTrack{}}
}

// AddTrack declares a track and returns its identifier. Every track must be
// added before the first sample is written, because the initialisation segment
// names them all.
func (m *Muxer) AddTrack(cfg TrackConfig) (uint32, error) {
	switch {
	case m.closed:
		return 0, ErrClosed
	case m.started:
		return 0, fmt.Errorf("%w: tracks cannot be added once writing has begun", ErrTrackConfig)
	case cfg.Timescale == 0:
		return 0, fmt.Errorf("%w: %s track has no timescale", ErrTrackConfig, cfg.Codec)
	}
	if m.init == nil {
		m.init = mp4.CreateEmptyInit()
		m.init.Moov.Mvhd.NextTrackID = 1
		m.init.Ftyp = mp4.NewFtyp(m.settings.brand, 0x200, []string{"isom", m.settings.brand, "dash"})
	}
	lang := cfg.Language
	if lang == "" {
		lang = "und"
	}
	trak := m.init.AddEmptyTrack(cfg.Timescale, handlerFor(cfg.Kind), lang)
	if err := describe(trak, cfg); err != nil {
		return 0, err
	}
	t := &muxTrack{id: trak.Tkhd.TrackID, timescale: cfg.Timescale}
	m.tracks = append(m.tracks, t)
	m.byID[t.id] = t
	return t.id, nil
}

// handlerFor is the handler name an ISO-BMFF track of this kind carries.
func handlerFor(k Kind) string {
	switch k {
	case Video:
		return "video"
	case Audio:
		return "audio"
	case Subtitle:
		return "subtitle"
	default:
		return "video"
	}
}

// describe writes the sample entry that tells a player how to decode the
// track.
func describe(trak *mp4.TrakBox, cfg TrackConfig) error {
	codec := strings.ToLower(strings.TrimSpace(cfg.Codec))
	switch codec {
	case "avc1", "avc3":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
			return fmt.Errorf("%w: %s needs both SPS and PPS", ErrTrackConfig, codec)
		}
		trak.SetAVCDescriptor(codec, cfg.SPS, cfg.PPS, true)
	case "hvc1", "hev1":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 || len(cfg.VPS) == 0 {
			return fmt.Errorf("%w: %s needs VPS, SPS and PPS", ErrTrackConfig, codec)
		}
		trak.SetHEVCDescriptor(codec, cfg.VPS, cfg.SPS, cfg.PPS, nil, true)
	case "av01":
		av1C, err := decodeAv1C(cfg.CodecConfig)
		if err != nil {
			return err
		}
		trak.SetAV1Descriptor(codec, av1C, uint16(cfg.Width), uint16(cfg.Height))
	case "mp4a":
		if cfg.SampleRate <= 0 {
			return fmt.Errorf("%w: mp4a needs a sample rate", ErrTrackConfig)
		}
		objType := cfg.AudioObjectType
		if objType == 0 {
			objType = aac.AAClc
		}
		trak.SetAACDescriptor(objType, cfg.SampleRate)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedCodec, cfg.Codec)
	}
	return nil
}

// These two indirections exist so the failure they guard can be tested: both
// are impossible to reach through the library as it stands, and code that
// cannot be exercised is code nobody knows the behaviour of.
var (
	decodeAv1CBox = mp4.DecodeAv1C
	newFragment   = mp4.CreateMultiTrackFragment
)

// decodeAv1C reads an av1C configuration record from its payload.
func decodeAv1C(payload []byte) (*mp4.Av1CBox, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: av01 needs its av1C configuration", ErrTrackConfig)
	}
	hdr := mp4.BoxHeader{Name: "av1C", Size: uint64(8 + len(payload)), Hdrlen: 8}
	box, err := decodeAv1CBox(hdr, 0, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: av1C: %v", ErrTrackConfig, err)
	}
	av1C, ok := box.(*mp4.Av1CBox)
	if !ok {
		return nil, fmt.Errorf("%w: av1C decoded as %T", ErrTrackConfig, box)
	}
	return av1C, nil
}

// WriteSample appends one frame to a track. The initialisation segment is
// written before the first one, and a fragment is flushed once the first track
// holds a fragment's worth of media and reaches a sync sample.
func (m *Muxer) WriteSample(trackID uint32, s Sample) error {
	switch {
	case m.closed:
		return ErrClosed
	case len(m.tracks) == 0:
		return ErrNoTracks
	case len(s.Data) == 0:
		return fmt.Errorf("%w: no data", ErrSample)
	case s.Duration == 0:
		return fmt.Errorf("%w: no duration", ErrSample)
	}
	t, ok := m.byID[trackID]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
	}
	if !m.started {
		if err := m.init.Encode(m.w); err != nil {
			return fmt.Errorf("container: write init segment: %w", err)
		}
		m.started = true
	}
	if m.shouldFlush(t, s) {
		if err := m.Flush(); err != nil {
			return err
		}
	}
	flags := uint32(mp4.NonSyncSampleFlags)
	if s.Sync {
		flags = mp4.SyncSampleFlags
	}
	t.pending = append(t.pending, mp4.FullSample{
		Sample: mp4.Sample{
			Flags:                 flags,
			Dur:                   s.Duration,
			Size:                  uint32(len(s.Data)),
			CompositionTimeOffset: s.CompositionOffset,
		},
		DecodeTime: t.nextTime,
		Data:       s.Data,
	})
	t.nextTime += uint64(s.Duration)
	t.buffered += uint64(s.Duration)
	return nil
}

// shouldFlush reports whether the fragment is long enough to be closed. Cutting
// on a sync sample is what lets a player start at a fragment boundary.
func (m *Muxer) shouldFlush(t *muxTrack, s Sample) bool {
	if t != m.tracks[0] || !s.Sync || len(t.pending) == 0 {
		return false
	}
	limit := uint64(m.settings.fragmentDuration.Seconds() * float64(t.timescale))
	return limit > 0 && t.buffered >= limit
}

// Flush writes the buffered samples as one fragment. It does nothing when
// nothing is buffered.
func (m *Muxer) Flush() error {
	if m.closed {
		return ErrClosed
	}
	if !m.buffering() {
		return nil
	}
	ids := make([]uint32, 0, len(m.tracks))
	for _, t := range m.tracks {
		ids = append(ids, t.id)
	}
	m.sequence++
	frag, err := newFragment(m.sequence, ids)
	if err != nil {
		return fmt.Errorf("container: create fragment %d: %w", m.sequence, err)
	}
	for _, t := range m.tracks {
		for _, s := range t.pending {
			frag.AddFullSampleToTrack(s, t.id)
		}
		t.pending, t.buffered = nil, 0
	}
	if err := frag.Encode(m.w); err != nil {
		return fmt.Errorf("container: write fragment %d: %w", m.sequence, err)
	}
	return nil
}

// buffering reports whether any track holds a sample not yet written.
func (m *Muxer) buffering() bool {
	for _, t := range m.tracks {
		if len(t.pending) > 0 {
			return true
		}
	}
	return false
}

// Close writes what is left and refuses any further use. It reports an error
// when no track was ever declared, because that file would name nothing.
func (m *Muxer) Close() error {
	if m.closed {
		return ErrClosed
	}
	if len(m.tracks) == 0 {
		m.closed = true
		return ErrNoTracks
	}
	if !m.started {
		if err := m.init.Encode(m.w); err != nil {
			m.closed = true
			return fmt.Errorf("container: write init segment: %w", err)
		}
		m.started = true
	}
	err := m.Flush()
	m.closed = true
	return err
}

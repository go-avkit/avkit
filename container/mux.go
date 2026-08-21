// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
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
	// "av01", "vp08", "vp09", "mp4a", "Opus", "ac-3", "ec-3".
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
	// PreSkip is how many samples an Opus decoder discards at the start of
	// the track, counted at 48 kHz whatever the input rate. A track written
	// without it starts a few milliseconds early. It is stated by the
	// identification header a Matroska or an Ogg file carries, so a caller
	// that passes CodecConfig does not need to fill this in.
	PreSkip uint16
	// VPx describes a VP8 or VP9 track. ISO-BMFF cannot carry one without
	// this record, and unlike AVC or HEVC the bitstream keeps it out of the
	// sample data, so a caller remuxing VP9 has to state it.
	VPx *VPxConfig
}

// VPxConfig is what the vpcC record says about a VP8 or VP9 track. The colour
// fields take their values from ISO/IEC 23001-8; leaving them at 2, the value
// that means "unspecified", is what a caller that does not know them should do.
type VPxConfig struct {
	// Profile is 0 to 3, and Level a value of the VP9 level table (10 for
	// level 1, 11 for 1.1, and so on). Level 0 is not a level: a record that
	// states it is refused rather than written as a guess.
	Profile, Level byte
	// BitDepth is 8, 10 or 12.
	BitDepth byte
	// ChromaSubsampling is 0 (4:2:0 vertically colocated), 1 (4:2:0
	// colocated), 2 (4:2:2) or 3 (4:4:4).
	ChromaSubsampling byte
	// FullRange tells a player the samples use the full range rather than
	// the studio swing of 16 to 235.
	FullRange bool
	// ColourPrimaries, TransferCharacteristics and MatrixCoefficients are
	// the ISO/IEC 23001-8 code points; 2 means unspecified.
	ColourPrimaries, TransferCharacteristics, MatrixCoefficients byte
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
	case "vp08", "vp09":
		vpcC, err := vpxConfig(codec, cfg)
		if err != nil {
			return err
		}
		if err := setVPxEntry(trak, codec, vpcC, uint16(cfg.Width), uint16(cfg.Height)); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrTrackConfig, codec, err)
		}
	case "opus":
		dops, err := opusConfig(cfg)
		if err != nil {
			return err
		}
		// The decoder always runs at 48 kHz whatever the track was recorded
		// at, and the Opus-in-ISOBMFF mapping says the sample entry states
		// that rate; the rate the track came from lives in dOps.
		entry := mp4.CreateAudioSampleEntryBox("Opus",
			uint16(dops.OutputChannelCount), 16, opusOutputRate, dops)
		trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	case "ac-3":
		dac3, err := decodeDac3(cfg.CodecConfig)
		if err != nil {
			return err
		}
		if err := setAC3Entry(trak, dac3); err != nil {
			return fmt.Errorf("%w: ac-3: %v", ErrTrackConfig, err)
		}
	case "ec-3":
		dec3, err := decodeDec3(cfg.CodecConfig)
		if err != nil {
			return err
		}
		if err := setEC3Entry(trak, dec3); err != nil {
			return fmt.Errorf("%w: ec-3: %v", ErrTrackConfig, err)
		}
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
	// A descriptor is written from the parameter sets it is given, and sets
	// nothing at all when it cannot read them. Writing a file whose sample
	// entry is empty would produce something no player can open, so say so
	// here instead.
	if _, err := trak.Mdia.Minf.Stbl.Stsd.GetSampleDescription(0); err != nil {
		return fmt.Errorf("%w: %s parameter sets are not usable: %v", ErrTrackConfig, codec, err)
	}
	return nil
}

// These two indirections exist so the failure they guard can be tested: both
// are impossible to reach through the library as it stands, and code that
// cannot be exercised is code nobody knows the behaviour of.
var (
	decodeAv1CBox = mp4.DecodeAv1C
	decodeDac3Box = mp4.DecodeDac3
	setVPxEntry   = (*mp4.TrakBox).SetVPxDescriptor
	setAC3Entry   = (*mp4.TrakBox).SetAC3Descriptor
	setEC3Entry   = (*mp4.TrakBox).SetEC3Descriptor
	decodeDec3Box = mp4.DecodeDec3
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

// opusOutputRate is the rate an Opus decoder always outputs, and so the rate
// the sample entry of an Opus track states.
const opusOutputRate = 48000

// opusHeadMagic labels the Opus identification header, which is what Matroska
// and Ogg carry as the codec's private data. Its fields are little-endian,
// where the dOps box of an MP4 holds the same information big-endian and
// without the label: the two are converted, never copied.
const opusHeadMagic = "OpusHead"

// opusHeadSize is the length of an identification header that maps its channels
// implicitly (mapping family 0).
const opusHeadSize = 19

// opusConfig builds the Opus configuration to write, from the identification
// header when the caller has one and from the track's own fields otherwise.
func opusConfig(cfg TrackConfig) (*mp4.DopsBox, error) {
	if len(cfg.CodecConfig) > 0 {
		return opusHead(cfg.CodecConfig)
	}
	switch {
	case cfg.Channels <= 0:
		return nil, fmt.Errorf("%w: Opus needs a channel count", ErrTrackConfig)
	case cfg.Channels > 2:
		// Mapping family 0 covers mono and stereo only, so more channels
		// cannot be described without the header that states their mapping.
		return nil, fmt.Errorf("%w: %d-channel Opus needs its identification header",
			ErrTrackConfig, cfg.Channels)
	case cfg.SampleRate <= 0:
		return nil, fmt.Errorf("%w: Opus needs the sample rate it was recorded at", ErrTrackConfig)
	}
	return &mp4.DopsBox{
		OutputChannelCount: byte(cfg.Channels),
		PreSkip:            cfg.PreSkip,
		InputSampleRate:    uint32(cfg.SampleRate),
	}, nil
}

// opusHead converts an Opus identification header into the box an MP4 carries.
func opusHead(head []byte) (*mp4.DopsBox, error) {
	if len(head) < opusHeadSize || string(head[:len(opusHeadMagic)]) != opusHeadMagic {
		return nil, fmt.Errorf("%w: Opus needs an %s identification header", ErrTrackConfig, opusHeadMagic)
	}
	if version := head[8]; version != 1 {
		return nil, fmt.Errorf("%w: %s version %d is not one this can read",
			ErrTrackConfig, opusHeadMagic, version)
	}
	channels := head[9]
	if channels == 0 {
		return nil, fmt.Errorf("%w: %s states no channel", ErrTrackConfig, opusHeadMagic)
	}
	dops := &mp4.DopsBox{
		OutputChannelCount:   channels,
		PreSkip:              binary.LittleEndian.Uint16(head[10:12]),
		InputSampleRate:      binary.LittleEndian.Uint32(head[12:16]),
		OutputGain:           int16(binary.LittleEndian.Uint16(head[16:18])),
		ChannelMappingFamily: head[18],
	}
	if dops.ChannelMappingFamily == 0 {
		if channels > 2 {
			return nil, fmt.Errorf("%w: %s maps %d channels implicitly, which family 0 cannot",
				ErrTrackConfig, opusHeadMagic, channels)
		}
		return dops, nil
	}
	// A stated mapping needs a stream count, a coupled count and one index
	// per channel; a header that stops short would otherwise be written as a
	// mapping of silence.
	if len(head) < opusHeadSize+2+int(channels) {
		return nil, fmt.Errorf("%w: %s stops before its channel mapping", ErrTrackConfig, opusHeadMagic)
	}
	dops.StreamCount = head[19]
	dops.CoupledCount = head[20]
	dops.ChannelMapping = append([]byte(nil), head[21:21+int(channels)]...)
	return dops, nil
}

// OpusHead renders an Opus configuration as the identification header Matroska
// and Ogg carry, which is what a caller writing one of those needs back.
func OpusHead(cfg TrackConfig) ([]byte, error) {
	dops, err := opusConfig(cfg)
	if err != nil {
		return nil, err
	}
	return opusHeadBytes(dops), nil
}

// opusHeadBytes is the identification header of this configuration.
func opusHeadBytes(dops *mp4.DopsBox) []byte {
	head := make([]byte, opusHeadSize, opusHeadSize+2+len(dops.ChannelMapping))
	copy(head, opusHeadMagic)
	head[8] = 1
	head[9] = dops.OutputChannelCount
	binary.LittleEndian.PutUint16(head[10:12], dops.PreSkip)
	binary.LittleEndian.PutUint32(head[12:16], dops.InputSampleRate)
	binary.LittleEndian.PutUint16(head[16:18], uint16(dops.OutputGain))
	head[18] = dops.ChannelMappingFamily
	if dops.ChannelMappingFamily != 0 {
		head = append(head, dops.StreamCount, dops.CoupledCount)
		head = append(head, dops.ChannelMapping...)
	}
	return head
}

// vpxConfig builds the vpcC record of a VP8 or VP9 track, refusing a
// description a player could not use rather than writing a guess.
func vpxConfig(codec string, cfg TrackConfig) (*mp4.VppCBox, error) {
	v := cfg.VPx
	if v == nil {
		return nil, fmt.Errorf("%w: %s needs its vpcC configuration", ErrTrackConfig, codec)
	}
	switch {
	case cfg.Width <= 0 || cfg.Height <= 0:
		return nil, fmt.Errorf("%w: %s needs its frame size", ErrTrackConfig, codec)
	case cfg.Width > maxFrameSide || cfg.Height > maxFrameSide:
		return nil, fmt.Errorf("%w: %s frame of %dx%d does not fit a sample entry",
			ErrTrackConfig, codec, cfg.Width, cfg.Height)
	case v.Profile > 3:
		return nil, fmt.Errorf("%w: %s profile %d is not one of 0 to 3", ErrTrackConfig, codec, v.Profile)
	case v.Level == 0:
		return nil, fmt.Errorf("%w: %s needs a level; 0 is not one", ErrTrackConfig, codec)
	case v.BitDepth != 8 && v.BitDepth != 10 && v.BitDepth != 12:
		return nil, fmt.Errorf("%w: %s bit depth %d is not 8, 10 or 12", ErrTrackConfig, codec, v.BitDepth)
	case v.ChromaSubsampling > 3:
		return nil, fmt.Errorf("%w: %s chroma subsampling %d is not one of 0 to 3",
			ErrTrackConfig, codec, v.ChromaSubsampling)
	}
	var fullRange byte
	if v.FullRange {
		fullRange = 1
	}
	return &mp4.VppCBox{
		Version:                 1,
		Profile:                 v.Profile,
		Level:                   v.Level,
		BitDepth:                v.BitDepth,
		ChromaSubsampling:       v.ChromaSubsampling,
		VideoFullRangeFlag:      fullRange,
		ColourPrimaries:         v.ColourPrimaries,
		TransferCharacteristics: v.TransferCharacteristics,
		MatrixCoefficients:      v.MatrixCoefficients,
	}, nil
}

// maxFrameSide is the largest frame side a sample entry can state.
const maxFrameSide = 0xFFFF

// dac3Size is the length of an AC-3 configuration record: two bits of sample
// rate code, five of bit stream identification, three of bit stream mode, three
// of audio coding mode, one of LFE, five of bit rate code and five reserved.
const dac3Size = 3

// dec3MinSize is the shortest Enhanced AC-3 record that describes anything:
// thirteen bits of data rate and three of substream count, then one
// three-byte substream.
const dec3MinSize = 5

// maxAC3SampleRateCode is the largest sample rate code AC-3 defines. A record
// stating more is refused here, because the sample rate table of the library
// that builds the sample entry is indexed by it and would run off its end.
const maxAC3SampleRateCode = 2

// decodeDac3 rebuilds the AC-3 configuration from the record's content, which
// is what a container carrying it without its box header hands over.
func decodeDac3(payload []byte) (*mp4.Dac3Box, error) {
	if len(payload) < dac3Size {
		return nil, fmt.Errorf("%w: a dac3 record is %d bytes, not %d",
			ErrTrackConfig, dac3Size, len(payload))
	}
	box, err := decodeConfigBox("dac3", payload, decodeDac3Box)
	if err != nil {
		return nil, err
	}
	dac3, ok := box.(*mp4.Dac3Box)
	if !ok {
		return nil, fmt.Errorf("%w: dac3 decoded as %T", ErrTrackConfig, box)
	}
	if dac3.FSCod > maxAC3SampleRateCode {
		return nil, fmt.Errorf("%w: dac3 states sample rate code %d, which AC-3 does not define",
			ErrTrackConfig, dac3.FSCod)
	}
	return dac3, nil
}

// decodeDec3 rebuilds the Enhanced AC-3 configuration from the record's
// content.
func decodeDec3(payload []byte) (*mp4.Dec3Box, error) {
	if len(payload) < dec3MinSize {
		return nil, fmt.Errorf("%w: a dec3 record is at least %d bytes, not %d",
			ErrTrackConfig, dec3MinSize, len(payload))
	}
	box, err := decodeConfigBox("dec3", payload, decodeDec3Box)
	if err != nil {
		return nil, err
	}
	dec3, ok := box.(*mp4.Dec3Box)
	if !ok {
		return nil, fmt.Errorf("%w: dec3 decoded as %T", ErrTrackConfig, box)
	}
	// The sample entry is built from the first substream, so a record naming
	// none, or naming a sample rate code that does not exist, must not reach
	// the library: it indexes both without looking.
	if len(dec3.EC3Subs) == 0 {
		return nil, fmt.Errorf("%w: dec3 names no substream", ErrTrackConfig)
	}
	for i, sub := range dec3.EC3Subs {
		if sub.FSCod > maxAC3SampleRateCode {
			return nil, fmt.Errorf("%w: dec3 substream %d states sample rate code %d, which AC-3 does not define",
				ErrTrackConfig, i, sub.FSCod)
		}
	}
	return dec3, nil
}

// decodeConfigBox reads a configuration record given only its content, by
// handing the decoder the header that content would have had.
func decodeConfigBox(name string, payload []byte,
	decode func(mp4.BoxHeader, uint64, io.Reader) (mp4.Box, error)) (mp4.Box, error) {
	hdr := mp4.BoxHeader{Name: name, Size: uint64(8 + len(payload)), Hdrlen: 8}
	box, err := decode(hdr, 0, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTrackConfig, name, err)
	}
	return box, nil
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

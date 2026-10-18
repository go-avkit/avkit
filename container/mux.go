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

	// ErrNotInSamples means the codec is known, and its configuration is not
	// something its samples can state.
	//
	// ⛔ It WRAPS ErrUnsupportedCodec, so a caller written before it existed
	// still matches. The distinction is worth making because the two refusals
	// call for opposite answers: this one says the configuration is in the
	// CONTAINER — an Opus track's identification header, an AC-3 track's bit
	// stream information, a VP8 track's level — and should be taken from there,
	// while a plain ErrUnsupportedCodec says there is nothing to take.
	ErrNotInSamples = fmt.Errorf("%w: not stated by the samples", ErrUnsupportedCodec)

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
	// that reorder frames. A track is presented from its FIRST sample's
	// composition time: every muxer shows that frame at zero, the MP4 ones
	// with an edit list and WebMMuxer by taking the offset off every block's
	// time, so a reordering delay does not make the pictures late against
	// their sound.
	CompositionOffset int32
	// Sync marks a frame a player can start decoding at.
	Sync bool
}

// TrackConfig describes a track to write. What a codec needs to be described
// differs, so the fields a codec ignores may be left at zero.
type TrackConfig struct {
	Kind Kind
	// Codec is the sample entry to write: "avc1", "avc3", "hvc1", "hev1",
	// "av01", "vp08", "vp09", "mjpg", "mp4a", "Opus", "ac-3", "ec-3", "fLaC",
	// and "mp1", "mp2" or "mp3" for MPEG audio, which an MP4 writes as mp4a;
	// and for timed text "wvtt" (WebVTT, ISO/IEC 14496-30) and "stpp" (TTML).
	//
	// A WebVTT track's samples are what ISO/IEC 14496-30 makes them, whatever
	// container the track came from: each covers a stretch of time and holds
	// the cues shown during it as vttc boxes, or one vtte box when none is, so
	// the samples follow one another without a gap. Reader hands a Matroska
	// WebVTT or SRT track back in that form, and WebMMuxer takes it.
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
	// parameters are not NAL units, such as the av1C payload of AV1. For AAC
	// it is the AudioSpecificConfig (ISO/IEC 14496-3), which the MP4 muxers
	// write verbatim and take the channel count from; without one they build a
	// record from AudioObjectType, Channels and SampleRate. For FLAC it is the
	// "fLaC" signature and every metadata block, STREAMINFO first, as
	// Matroska's CodecPrivate holds them. For WebVTT it is the header of the
	// WebVTT file, the text of the vttC box (left empty, the bare "WEBVTT"
	// signature is written); for TTML it is the content of the stpp sample
	// entry, which is carried as it stands.
	CodecConfig []byte
	// AudioObjectType selects the AAC profile when no AudioSpecificConfig is
	// given; 0 means AAC-LC.
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
	// StartDelay is media at the head of the track that is decoded but not
	// presented: an encoder's priming, which an AAC encoder emits as one
	// frame of about 21 ms at 48 kHz.
	//
	// Each container states it in its own way -- Matroska as CodecDelay, an
	// MP4 as an edit list -- and a remux that dropped it would hand the
	// output a frame the input never presented, so the two tracks would part
	// company by that much. Readers fill it in from whichever form the source
	// used, Muxer and ProgressiveMuxer write an edit list for it, and
	// WebMMuxer the CodecDelay of an audio track whose codec states none of
	// its own; a caller that knows better can overwrite it.
	//
	// It is counted from the track's first PRESENTED frame, not from its first
	// decoded one: a track that reorders its frames starts its presentation
	// at its first sample's CompositionOffset, which the muxers state on their
	// own, and the priming comes after that. An MP4 edit list starting at that
	// offset therefore reads back as no StartDelay at all.
	//
	// It is not PreSkip. PreSkip is Opus stating its own priming inside its
	// identification header, in samples at 48 kHz, and it travels with the
	// codec configuration wherever the track goes. StartDelay is what the
	// CONTAINER says, for any codec, and it has to be rewritten into the
	// form the output container uses.
	//
	// When EditList is set, the MP4 muxers write that instead and this is not
	// added to it: the list already says where the presentation starts.
	StartDelay time.Duration
	// EditList is the track's MP4 edit list, as the MP4 reader found it, or
	// nil. Muxer and ProgressiveMuxer write it as it stands, in place of the
	// one they would build from the first composition offset and StartDelay,
	// so a track copied from one MP4 into another keeps its presentation edit
	// for edit. A caller who wants the muxers' own edit list sets it to nil.
	//
	// Matroska has no edit lists: WebMMuxer takes the one shape it can state,
	// an optional empty edit followed by one edit at the normal rate, as a
	// shift of the track's times, and refuses any other. TSMuxer ignores it.
	// Cut and Concat refuse a list they cannot carry; see ErrEditList.
	EditList *EditList
}

// VPxConfig is what the vpcC record says about a VP8 or VP9 track. The colour
// fields take their values from ISO/IEC 23001-8; leaving them at 2, the value
// that means "unspecified", is what a caller that does not know them should do.
type VPxConfig struct {
	// Profile is 0 to 3, and Level a value of the VP9 level table (10 for
	// level 1, 11 for 1.1, and so on). A VP9 record stating level 0 is
	// refused rather than written as a guess: the frames state the level.
	//
	// VP8 defines no level at all, and the VP Codec ISO Media File Format
	// Binding's table gives 0 as "Undefined": a vp08 record states level 0,
	// which is what a VP8 track read out of Matroska carries, and profile 0,
	// the only one the binding allows for VP8.
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
	index            bool
	indexMemoryLimit uint64
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
// handed over. Asked for a SegmentIndex, it also writes the sidx boxes that make
// the file a DASH on-demand representation a client can fetch by byte range.
type Muxer struct {
	w        io.Writer
	settings muxSettings

	init    *mp4.InitSegment
	tracks  []*muxTrack
	byID    map[uint32]*muxTrack
	started bool
	// headerOut is set once the initialisation segment is written, which is
	// with the first fragment rather than the first sample: see writeHeader.
	headerOut bool
	closed    bool
	sequence  uint32
	// index is what a Muxer asked for a SegmentIndex keeps until Close;
	// nil otherwise.
	index    *segmentIndex
	metadata *Metadata // what SetMetadata was given, nil for nothing
}

// muxTrack is one track's writing state.
type muxTrack struct {
	id        uint32
	timescale uint32
	nextTime  uint64
	pending   []mp4.FullSample
	buffered  uint64 // duration of pending samples, in timescale units

	// trak is the track's box in the initialisation segment, which receives
	// its edit list when the header is written.
	trak *mp4.TrakBox
	// priming is the track's StartDelay in its own timescale.
	priming int64
	// firstOffset is the composition offset of the track's first sample, and
	// written says there was one.
	firstOffset int32
	written     bool
}

// writeHeader writes the initialisation segment, once, stating in each track's
// edit list where its presentation begins: at its first sample's composition
// time, then past its priming (see presentationStart).
//
// It is written with the FIRST FRAGMENT rather than the first sample, because
// the composition offset it states is a sample's: a track that reorders its
// frames is only known to once its first sample has arrived, and a header
// written before then cannot say it. Holding it back costs nothing a reader can
// see -- the first fragment is written in the same call -- and it is what
// ffmpeg's movenc does under -movflags delay_moov for the same reason. A track
// whose first sample arrives only after the first fragment has gone out cannot
// be stated, and its edit list says its priming alone.
//
// segment_duration is left at zero, which §8.6.6.1 allows in the initial movie
// of a fragmented file and which reads as "to the end of the media". The
// alternative is the track's duration in the MOVIE timescale, which a muxer
// writing fragments does not know when the header goes out: writing a wrong
// one would cut the track short, where zero cannot.
func (m *Muxer) writeHeader() error {
	if m.headerOut {
		return nil
	}
	for _, t := range m.tracks {
		// A header that failed to go out is written again on the next call,
		// and must not gain a second edit box on the way.
		if t.trak.Edts != nil {
			continue
		}
		var first int32
		if t.written {
			first = t.firstOffset
		}
		if edts := presentationEdits(presentationStart(first, t.priming), 0); edts != nil {
			setEdits(t.trak, edts)
		}
	}
	if err := m.encodeInit(); err != nil {
		return fmt.Errorf("container: write init segment: %w", err)
	}
	m.headerOut = true
	// A segment index goes between the initialisation segment and the first
	// fragment, so it notes where that is now that the header is out.
	if m.index != nil {
		return m.index.begin()
	}
	return nil
}

// setFtyp makes ftyp the file type box the initialisation segment is written
// with.
//
// mp4ff's CreateEmptyInit already holds one -- major brand cmfc, compatible
// cmfc and iso6 -- and an InitSegment is ENCODED from its Children, not from
// its Ftyp field, which is filled only when a file is decoded. Setting the field
// alone left that default in the bytes: every fragmented file claimed CMAF
// (ISO/IEC 23000-19), which nothing here promises, and none carried the brand
// the caller asked for. ISO/IEC 14496-12 §4.3 allows one ftyp, first in the
// file, so the child is replaced where it stands.
func setFtyp(init *mp4.InitSegment, ftyp *mp4.FtypBox) {
	init.Ftyp = ftyp
	for i, child := range init.Children {
		if _, ok := child.(*mp4.FtypBox); ok {
			init.Children[i] = ftyp
		}
	}
}

// NewMuxer returns a Muxer writing to w.
func NewMuxer(w io.Writer, opts ...MuxOption) *Muxer {
	settings := muxSettings{
		fragmentDuration: DefaultFragmentDuration, brand: DefaultBrand,
		indexMemoryLimit: DefaultMediaMemoryLimit,
	}
	for _, opt := range opts {
		opt(&settings)
	}
	m := &Muxer{w: w, settings: settings, byID: map[uint32]*muxTrack{}}
	if settings.index {
		m.index = newSegmentIndex(w, settings.indexMemoryLimit)
	}
	return m
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
	if cfg.EditList != nil {
		if err := fragmentedEditList(cfg.EditList); err != nil {
			return 0, err
		}
	}
	if m.init == nil {
		m.init = mp4.CreateEmptyInit()
		m.init.Moov.Mvhd.NextTrackID = 1
		setFtyp(m.init, mp4.NewFtyp(m.settings.brand, 0x200, []string{"isom", m.settings.brand, "dash"}))
	}
	lang := cfg.Language
	if lang == "" {
		lang = "und"
	}
	trak := m.init.AddEmptyTrack(cfg.Timescale, trackHandler(cfg), lang)
	if err := describe(trak, cfg); err != nil {
		return 0, err
	}
	if cfg.EditList != nil {
		// Known now, so written now: writeHeader leaves a track that already
		// has its edit box alone.
		edts, _ := cfg.EditList.mp4Edits(m.init.Moov.Mvhd.Timescale, cfg.Timescale, 0)
		setEdits(trak, edts)
	}
	t := &muxTrack{id: trak.Tkhd.TrackID, timescale: cfg.Timescale, trak: trak,
		priming: primingTicks(cfg.StartDelay, cfg.Timescale)}
	m.tracks = append(m.tracks, t)
	m.byID[t.id] = t
	return t.id, nil
}

// trackHandler is the handler a track is written with. Timed text is the one
// case where the codec decides rather than the kind: ISO/IEC 14496-30 gives a
// WebVTT track the 'text' handler and a null media header, and a TTML track the
// 'subt' handler and a subtitle media header, and a player looking for one does
// not find the other. mp4ff's handler table knows both by their sample entry.
func trackHandler(cfg TrackConfig) string {
	if codec := strings.ToLower(strings.TrimSpace(cfg.Codec)); isTimedText(codec) {
		return codec
	}
	return handlerFor(cfg.Kind)
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
		fixHvcCConstraints(trak, cfg.SPS)
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
	case "mjpg":
		if err := describeMJPEG(trak, cfg); err != nil {
			return err
		}
	case "mp1", "mp2", "mp3":
		// What an MPEG audio track is named when it is read. In an MP4 it
		// is mp4a with object type indication 0x6B or 0x69 (MP4RA).
		if err := describeMPEGAudio(trak, codec, cfg); err != nil {
			return err
		}
	case "flac":
		if err := describeFLAC(trak, cfg); err != nil {
			return err
		}
	case "vorb":
		return fmt.Errorf("%w: vorb: MP4RA registers no ISO-BMFF sample entry for Vorbis, so no player "+
			"would know the track; Matroska carries it (WebMMuxer)", ErrUnsupportedCodec)
	case "mp4a":
		if err := describeAAC(trak, cfg); err != nil {
			return err
		}
	case codecWebVTT:
		if err := describeWebVTT(trak, cfg); err != nil {
			return err
		}
	case codecTTML:
		if err := describeTTML(trak, cfg); err != nil {
			return err
		}
	case "ipcm", "fpcm", "raw":
		// ISO/IEC 23003-5 gives PCM its sample entries and a pcmC record,
		// and mp4ff writes neither yet: refused by name rather than written
		// as an entry a player would read as some other codec.
		return fmt.Errorf("%w: %q: PCM sample entries (ISO/IEC 23003-5 ipcm/fpcm with pcmC) are not written yet",
			ErrUnsupportedCodec, cfg.Codec)
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
	setMJpegEntry = (*mp4.TrakBox).SetMJpegDescriptor
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

// opusHeadMajor is the major version, the upper four bits of the version
// octet, of the identification header RFC 7845 defines.
const opusHeadMajor = 0

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
	// RFC 7845 §5.1 splits the version into a major nibble and a minor one: a
	// change of the minor nibble is a compatible one, so a reader of version 1
	// SHOULD accept anything up to 15 and SHOULD refuse 16 and above. A later
	// minor version may append fields after the ones read here, and the RFC
	// says those MUST NOT make the header invalid; they are ignored, and the
	// header written back is a version 1 one, which is all this states.
	if version := head[8]; version>>4 != opusHeadMajor {
		return nil, fmt.Errorf("%w: %s version %d is major version %d, and only %d is defined",
			ErrTrackConfig, opusHeadMagic, version, version>>4, opusHeadMajor)
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
	case v.Level == 0 && codec != "vp08":
		// The binding's level table lists 0 as "Undefined", and a VP9 track
		// has a level its frames state, which Remux reads when the container
		// did not (describeFromSamples): a VP9 record stating none is refused
		// rather than written as one.
		return nil, fmt.Errorf("%w: %s needs a level; 0 is not one", ErrTrackConfig, codec)
	case codec == "vp08" && v.Profile != 0:
		// "VP8 only supports a profile value of 0." (VP Codec ISO Media File
		// Format Binding v1.0, semantics of the VPCodecConfigurationRecord).
		return nil, fmt.Errorf("%w: vp08 profile %d; VP8 only supports profile 0", ErrTrackConfig, v.Profile)
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

// describeMJPEG writes the sample entry of a motion JPEG track, as ISO/IEC
// 23008-12 Annex H defines it for JPEG image sequences: every sample is a
// complete JPEG image, which is what an encoder built on the standard library's
// image/jpeg produces.
//
// A caller whose samples are not complete images — the same tables in front of
// every one of them, the entropy-coded data alone in each — states that shared
// prefix in CodecConfig, and it is written as the jpgC box a decoder prepends.
func describeMJPEG(trak *mp4.TrakBox, cfg TrackConfig) error {
	switch {
	case cfg.Width <= 0 || cfg.Height <= 0:
		return fmt.Errorf("%w: mjpg needs its frame size", ErrTrackConfig)
	case cfg.Width > maxFrameSide || cfg.Height > maxFrameSide:
		return fmt.Errorf("%w: mjpg frame of %dx%d does not fit a sample entry",
			ErrTrackConfig, cfg.Width, cfg.Height)
	}
	// A nil prefix and an empty one mean different things to the library: nil
	// writes no jpgC at all, which is what a stream of whole images wants.
	var prefix []byte
	if len(cfg.CodecConfig) > 0 {
		prefix = cfg.CodecConfig
	}
	if err := setMJpegEntry(trak, uint16(cfg.Width), uint16(cfg.Height), prefix); err != nil {
		return fmt.Errorf("%w: mjpg: %v", ErrTrackConfig, err)
	}
	return nil
}

// describeAAC writes the mp4a sample entry of an AAC track.
//
// Two fields say how many channels the track has, and they must agree: the
// AudioSampleEntry's channelcount (ISO/IEC 14496-12 §12.2.3, "the number of
// channels such as 1 (mono) or 2 (stereo)") and the channelConfiguration of the
// AudioSpecificConfig (ISO/IEC 14496-3 §1.6.2.1, Table 1.19) that the esds
// carries as its DecoderSpecificInfo. A decoder is set up from the second, so a
// mono track whose record says stereo is decoded as stereo: ffprobe reports
// channels=2 for it.
//
// When the caller has the track's AudioSpecificConfig -- every reader of this
// package hands it over as CodecConfig -- it is written verbatim, as ffmpeg's
// movenc.c writes the extradata it was given. Rebuilding it would lose what this
// package does not model: the SBR and PS signalling of HE-AAC, a program
// config element, the GASpecificConfig flags. The channel count of the sample
// entry is then the one the record states, and the caller's only where the
// record leaves it to the bitstream (configuration 0).
//
// Without one, the record is built from the track's fields; a channel count no
// configuration of Table 1.19 states is refused rather than written as stereo.
func describeAAC(trak *mp4.TrakBox, cfg TrackConfig) error {
	if cfg.SampleRate <= 0 {
		return fmt.Errorf("%w: mp4a needs a sample rate", ErrTrackConfig)
	}
	if len(cfg.CodecConfig) == 0 {
		return buildAACEntry(trak, cfg)
	}
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(cfg.CodecConfig))
	if err != nil {
		return fmt.Errorf("%w: mp4a AudioSpecificConfig: %v", ErrTrackConfig, err)
	}
	channels := aacChannels(asc.ChannelConfiguration)
	if channels == 0 {
		channels = cfg.Channels
	}
	if channels <= 0 || channels > 0xFFFF {
		return fmt.Errorf("%w: mp4a AudioSpecificConfig leaves the channel count to the bitstream, and the track states none",
			ErrTrackConfig)
	}
	esds := mp4.CreateEsdsBox(append([]byte(nil), cfg.CodecConfig...))
	trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateAudioSampleEntryBox("mp4a",
		uint16(channels), 16, uint16(cfg.SampleRate), esds))
	return nil
}

// buildAACEntry writes an mp4a sample entry whose AudioSpecificConfig is built
// from the track's fields.
func buildAACEntry(trak *mp4.TrakBox, cfg TrackConfig) error {
	objType := cfg.AudioObjectType
	if objType == 0 {
		objType = aac.AAClc
	}
	var err error
	switch {
	case cfg.Channels == 0 || objType == aac.HEAACv2:
		// Nothing stated, which has always meant stereo; and HE-AACv2 is a
		// mono core that parametric stereo makes two of, so its record says 1
		// whatever the output is.
		err = trak.SetAACDescriptor(objType, cfg.SampleRate)
	default:
		configuration, ok := aacConfiguration(cfg.Channels)
		if !ok {
			return fmt.Errorf("%w: no AAC channel configuration states %d channels; pass the AudioSpecificConfig",
				ErrTrackConfig, cfg.Channels)
		}
		err = trak.SetAACDescriptorWithChannelConfig(objType, configuration, cfg.SampleRate)
	}
	if err != nil {
		return fmt.Errorf("%w: mp4a: %v", ErrTrackConfig, err)
	}
	return nil
}

// aacConfiguration is the channelConfiguration of ISO/IEC 14496-3 Table 1.19
// that states this many channels: the inverse of aacChannels.
func aacConfiguration(channels int) (byte, bool) {
	switch channels {
	case 1, 2, 3, 4, 5, 6:
		return byte(channels), true
	case 8:
		return 7, true
	}
	return 0, false
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

// WriteSample appends one frame to a track. A fragment is flushed once the
// first track holds a fragment's worth of media and reaches a sync sample, and
// the initialisation segment is written just before the first fragment, when
// every track's first sample says where its presentation begins.
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
	m.started = true
	if !t.written {
		// Recorded before a flush the sample may trigger, so a header written
		// by that flush states it even though the sample lands in the next
		// fragment.
		t.firstOffset, t.written = s.CompositionOffset, true
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
	if err := m.writeHeader(); err != nil {
		return err
	}
	// Only the tracks that have something to write. A track fragment declared
	// for a track with no samples in it carries nothing, and readers in the wild
	// -- including this package's own, until it was fixed -- refuse the file for
	// it.
	ids := make([]uint32, 0, len(m.tracks))
	for _, t := range m.tracks {
		if len(t.pending) > 0 {
			ids = append(ids, t.id)
		}
	}
	m.sequence++
	frag, err := newFragment(m.sequence, ids)
	if err != nil {
		return fmt.Errorf("container: create fragment %d: %w", m.sequence, err)
	}
	var indexed []trackSpan
	if m.index != nil {
		indexed = spans(m.tracks)
	}
	for _, t := range m.tracks {
		for _, s := range t.pending {
			frag.AddFullSampleToTrack(s, t.id)
		}
		t.pending, t.buffered = nil, 0
	}
	if m.index != nil {
		err = m.index.write(m.w, frag, indexed)
	} else {
		err = frag.Encode(m.w)
	}
	if err != nil {
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
	if err := m.writeHeader(); err != nil {
		m.closed = true
		return err
	}
	err := m.Flush()
	m.closed = true
	if err == nil && m.index != nil {
		// The index states every fragment, so it is written once the last
		// one is.
		err = m.index.finish(m.w, m.tracks)
	}
	return err
}

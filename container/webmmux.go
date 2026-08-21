// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
)

// DefaultClusterDuration is how much media a cluster holds when the caller
// states nothing. A second is what the browsers' own muxers settle on: short
// enough that a player scanning for a start point does not read far, long
// enough that the per-cluster overhead stays negligible.
const DefaultClusterDuration = time.Second

// DefaultTimestampScale is how long one segment tick lasts when the caller
// states nothing: a millisecond, the Matroska default, and the only scale the
// WebM guidelines expect a browser to meet.
const DefaultTimestampScale = time.Millisecond

// webmMuxingApp names this library in the MuxingApp and WritingApp of every
// segment it writes. It carries no version: the output has to be reproducible
// byte for byte, and a version string would change it without changing the
// media.
const webmMuxingApp = "github.com/go-avkit/avkit container"

// A SimpleBlock states its timestamp relative to its cluster in a signed
// 16-bit field, so a block further from the cluster's own timestamp than this
// cannot be written and needs a cluster of its own.
const (
	maxBlockTimecode = math.MaxInt16
	minBlockTimecode = math.MinInt16
)

// opusSeekPreRoll is how much audio an Opus decoder needs to have fed before
// its output is correct: 80 ms, as the WebM Opus mapping states, in the
// nanoseconds SeekPreRoll is counted in.
const opusSeekPreRoll = uint64(80 * time.Millisecond / time.Nanosecond)

// vorbisSetupPackets is how many packets the Vorbis private data holds: the
// identification, comment and setup headers, Xiph-laced. A decoder cannot start
// without all three.
const vorbisSetupPackets = 3

// vorbisIdentificationPacket is the type byte of the first of those packets.
const vorbisIdentificationPacket = 0x01

// aacMaxChannelConfiguration is the largest channel configuration of ISO/IEC
// 14496-3 table 1.19; 8 to 15 are reserved, and the field is four bits wide, so
// a larger value would be written as something else entirely.
const aacMaxChannelConfiguration = 7

// WebMOption configures a WebMMuxer.
type WebMOption func(*webmSettings)

type webmSettings struct {
	clusterDuration time.Duration
	tick            time.Duration
	buffered        bool
}

// ClusterDuration sets how much media a cluster holds before the next sync
// sample of the first track starts a new one. A value of zero or less restores
// the default.
func ClusterDuration(d time.Duration) WebMOption {
	return func(s *webmSettings) {
		if d <= 0 {
			d = DefaultClusterDuration
		}
		s.clusterDuration = d
	}
}

// TimestampScale sets how long one segment tick lasts, which is the resolution
// every timestamp in the file is stated at. The default millisecond cannot state
// a 90 kHz or a 48 kHz time exactly, so a caller who needs the timestamps it
// hands over to survive unrounded asks for a finer tick here.
//
// It is not free: a block states its timestamp relative to its cluster in a
// signed 16-bit field, so the finer the tick the shorter a cluster can be, and
// the more clusters the file holds.
//
// A tick that is not a whole divisor of a second, or is longer than one,
// restores the default: the scale has to divide a second exactly or the ticks
// per second this converts with would themselves be rounded.
func TimestampScale(tick time.Duration) WebMOption {
	return func(s *webmSettings) {
		if tick <= 0 || tick > time.Second || time.Second%tick != 0 {
			tick = DefaultTimestampScale
		}
		s.tick = tick
	}
}

// BufferedSegment holds the whole segment in memory until Close, so it can be
// written with every size known and with the duration stated in Info. That is
// what a caller writing to a file wants; a caller writing to an HTTP response
// cannot afford it and should not ask for it.
func BufferedSegment() WebMOption {
	return func(s *webmSettings) { s.buffered = true }
}

// WebMMuxer writes a Matroska file: the EBML header, a segment naming every
// track, then one cluster after another.
//
// It is the counterpart of Muxer — the same tracks, the same samples, the same
// refusals — for the container a browser plays without a plugin, and like Muxer
// it never re-encodes: samples are written as handed over.
//
// By default nothing is buffered and nothing is seeked: the segment and every
// cluster are written with their size left unknown, so the file is playable as
// it is produced and can be streamed straight into an HTTP response. A player
// reading such a file loses three things, and a caller who can hold the file in
// memory gets them back with BufferedSegment:
//
//   - the duration, which Info cannot state before the last sample is known, so
//     a player shows no total time and no seek bar until it has read the file;
//   - the segment's size, which a player has to reach the end of the file to
//     learn;
//   - a cue point index, which this muxer never writes in either mode, so
//     seeking means scanning cluster by cluster.
//
// The document type follows the tracks: "webm" while every track holds a codec
// WebM allows, "matroska" as soon as one does not, because a file declaring
// itself WebM while carrying AVC or AAC is one a strict player is right to
// refuse.
type WebMMuxer struct {
	w        io.Writer
	settings webmSettings

	entries []webmTrackEntry
	tracks  []*webmTrack
	byID    map[uint32]*webmTrack

	started bool
	closed  bool

	clusterOpen  bool
	clusterStart uint64
	clusters     []webmCluster // buffered mode only
}

// webmTrack is one track's writing state.
type webmTrack struct {
	id        uint32
	timescale uint32
	// nextTime is the track's decoding time, in its own timescale, and end
	// the largest presentation time reached. Both stay in track units: every
	// timestamp written is converted from one of them, never from the tick
	// last written, which is what keeps the rounding from accumulating.
	nextTime uint64
	end      uint64
	// webmCodec is what the codec this track carries is allowed in.
	inWebM bool
}

// The EBML tree this muxer writes. The elements come from ebml-go's own table
// and every byte is encoded by ebml-go's marshaller; only the shape is stated
// here, because the shape is what the two modes differ in.
type webmSizedDoc struct {
	Header  webm.EBMLHeader `ebml:"EBML"`
	Segment webmSegment     `ebml:"Segment"`
}

// webmStreamDoc is the same file with the segment's size left unknown, so the
// clusters that follow can be written one at a time to a writer that cannot
// seek. webm.SegmentStream would say the same, but it holds its clusters in a
// slice, and a slice is what a streaming muxer does not have.
type webmStreamDoc struct {
	Header  webm.EBMLHeader `ebml:"EBML"`
	Segment webmOpenSegment `ebml:"Segment,size=unknown"`
}

type webmSegment struct {
	Info    webmInfo      `ebml:"Info"`
	Tracks  webmTracks    `ebml:"Tracks"`
	Cluster []webmCluster `ebml:"Cluster"`
}

type webmOpenSegment struct {
	Info   webmInfo   `ebml:"Info"`
	Tracks webmTracks `ebml:"Tracks"`
}

type webmInfo struct {
	TimecodeScale uint64  `ebml:"TimecodeScale"`
	MuxingApp     string  `ebml:"MuxingApp"`
	WritingApp    string  `ebml:"WritingApp"`
	Duration      float64 `ebml:"Duration,omitempty"`
}

type webmTracks struct {
	TrackEntry []webmTrackEntry `ebml:"TrackEntry"`
}

// webmTrackEntry is webm.TrackEntry plus the Language every demuxer reads,
// which that struct leaves out.
type webmTrackEntry struct {
	TrackNumber  uint64     `ebml:"TrackNumber"`
	TrackUID     uint64     `ebml:"TrackUID"`
	TrackType    uint64     `ebml:"TrackType"`
	CodecID      string     `ebml:"CodecID"`
	CodecPrivate []byte     `ebml:"CodecPrivate,omitempty"`
	CodecDelay   uint64     `ebml:"CodecDelay,omitempty"`
	SeekPreRoll  uint64     `ebml:"SeekPreRoll,omitempty"`
	Language     string     `ebml:"Language,omitempty"`
	Video        *webmVideo `ebml:"Video"`
	Audio        *webmAudio `ebml:"Audio"`
}

type webmVideo struct {
	PixelWidth  uint64 `ebml:"PixelWidth"`
	PixelHeight uint64 `ebml:"PixelHeight"`
}

type webmAudio struct {
	SamplingFrequency float64 `ebml:"SamplingFrequency"`
	Channels          uint64  `ebml:"Channels"`
}

type webmCluster struct {
	Timecode    uint64       `ebml:"Timecode"`
	SimpleBlock []ebml.Block `ebml:"SimpleBlock"`
}

// webmOpenCluster is a cluster whose size is left unknown, so the blocks
// marshalled after it land inside it.
type webmOpenCluster struct {
	Cluster webmClusterHead `ebml:"Cluster,size=unknown"`
}

type webmClusterHead struct {
	Timecode uint64 `ebml:"Timecode"`
}

type webmBlockElement struct {
	Block ebml.Block `ebml:"SimpleBlock"`
}

// NewWebMMuxer returns a WebMMuxer writing to w. It writes to w as samples
// arrive and never closes it.
func NewWebMMuxer(w io.Writer, opts ...WebMOption) *WebMMuxer {
	settings := webmSettings{clusterDuration: DefaultClusterDuration, tick: DefaultTimestampScale}
	for _, opt := range opts {
		opt(&settings)
	}
	return &WebMMuxer{w: w, settings: settings, byID: map[uint32]*webmTrack{}}
}

// AddTrack declares a track and returns its identifier, which is the Matroska
// track number. Every track must be added before the first sample is written,
// because the Tracks element names them all and precedes the first cluster.
//
// A configuration this cannot describe is refused here rather than written as a
// guess: a track entry a player cannot decode from is worse than no file.
func (m *WebMMuxer) AddTrack(cfg TrackConfig) (uint32, error) {
	switch {
	case m.closed:
		return 0, ErrClosed
	case m.started:
		return 0, fmt.Errorf("%w: tracks cannot be added once writing has begun", ErrTrackConfig)
	case cfg.Timescale == 0:
		return 0, fmt.Errorf("%w: %s track has no timescale", ErrTrackConfig, cfg.Codec)
	}
	number := uint64(len(m.tracks)) + 1
	entry, codec, err := describeWebMTrack(cfg, number)
	if err != nil {
		return 0, err
	}
	t := &webmTrack{id: uint32(number), timescale: cfg.Timescale, inWebM: codec.inWebM}
	m.entries = append(m.entries, entry)
	m.tracks = append(m.tracks, t)
	m.byID[t.id] = t
	return t.id, nil
}

// webmCodec is what this muxer knows about one codec: the Matroska CodecID it
// writes, the kind of track that ID belongs to, whether WebM allows it, and how
// to state the codec's own configuration.
type webmCodec struct {
	id       string
	kind     Kind
	inWebM   bool
	describe func(TrackConfig) (webmCodecPrivate, error)
}

// webmCodecPrivate is what a codec adds to its track entry.
type webmCodecPrivate struct {
	private     []byte
	codecDelay  uint64 // in nanoseconds
	seekPreRoll uint64 // in nanoseconds
	audio       *webmAudio
}

// webmCodecs maps the ISO-BMFF sample entry name of a codec — "vp09", "avc1",
// "mp4a" and the rest — onto the Matroska CodecID this muxer writes for it.
//
// A caller stating a Matroska id instead reaches the same entry through
// mkvCodec, the correspondence the Matroska reader already keeps, so a track
// read out of a Matroska file and the same track read out of an MP4 are one
// thing here and Muxer and WebMMuxer can be swapped for one another.
var webmCodecs = map[string]webmCodec{
	"vp08": {id: "V_VP8", kind: Video, inWebM: true, describe: webmNoPrivate},
	"vp09": {id: "V_VP9", kind: Video, inWebM: true, describe: webmNoPrivate},
	"av01": {id: "V_AV1", kind: Video, inWebM: true, describe: webmAV1Private},
	"avc1": {id: "V_MPEG4/ISO/AVC", kind: Video, describe: webmAVCPrivate},
	"avc3": {id: "V_MPEG4/ISO/AVC", kind: Video, describe: webmAVCPrivate},
	"hvc1": {id: "V_MPEGH/ISO/HEVC", kind: Video, describe: webmHEVCPrivate},
	"hev1": {id: "V_MPEGH/ISO/HEVC", kind: Video, describe: webmHEVCPrivate},
	"opus": {id: "A_OPUS", kind: Audio, inWebM: true, describe: webmOpusPrivate},
	"vorb": {id: "A_VORBIS", kind: Audio, inWebM: true, describe: webmVorbisPrivate},
	"mp4a": {id: "A_AAC", kind: Audio, describe: webmAACPrivate},
}

// webmCodecFor is the codec a caller's spelling names, whether it spells the
// sample entry an MP4 states or the id a Matroska file does — including the
// suffixed ids Matroska allows, such as A_AAC/MPEG4/LC.
func webmCodecFor(spelling string) (webmCodec, bool) {
	name := strings.TrimSpace(spelling)
	if entry, ok := mkvCodec(strings.ToUpper(name)); ok {
		name = entry
	}
	codec, ok := webmCodecs[strings.ToLower(name)]
	return codec, ok
}

// describeWebMTrack builds one track entry, or refuses the configuration.
func describeWebMTrack(cfg TrackConfig, number uint64) (webmTrackEntry, webmCodec, error) {
	codec, ok := webmCodecFor(cfg.Codec)
	if !ok {
		return webmTrackEntry{}, codec, fmt.Errorf("%w: %q", ErrUnsupportedCodec, cfg.Codec)
	}
	// A track's kind follows its codec, so a configuration that states another
	// one is a mistake worth reporting rather than overruling: the caller and
	// this muxer disagree about what the samples are.
	if cfg.Kind != Other && cfg.Kind != codec.kind {
		return webmTrackEntry{}, codec, fmt.Errorf("%w: %s is a %s codec, not a %s one",
			ErrTrackConfig, codec.id, codec.kind, cfg.Kind)
	}
	desc, err := codec.describe(cfg)
	if err != nil {
		return webmTrackEntry{}, codec, err
	}
	lang := cfg.Language
	if lang == "" {
		lang = "und"
	}
	entry := webmTrackEntry{
		TrackNumber: number,
		// The UID is the track number rather than something random, so two
		// runs of the same input produce the same bytes.
		TrackUID:     number,
		CodecID:      codec.id,
		CodecPrivate: desc.private,
		CodecDelay:   desc.codecDelay,
		SeekPreRoll:  desc.seekPreRoll,
		Language:     lang,
		Audio:        desc.audio,
	}
	// Every codec in the table is a video or an audio one: subtitles are a
	// boundary this muxer does not cross yet, and are refused above as a codec
	// it has no id for.
	switch codec.kind {
	case Video:
		entry.TrackType = mkvTrackVideo
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return webmTrackEntry{}, codec, fmt.Errorf("%w: %s needs its frame size",
				ErrTrackConfig, codec.id)
		}
		entry.Video = &webmVideo{PixelWidth: uint64(cfg.Width), PixelHeight: uint64(cfg.Height)}
	default:
		entry.TrackType = mkvTrackAudio
	}
	return entry, codec, nil
}

// webmNoPrivate describes a codec that states everything in its own bitstream.
// VP8 and VP9 are those: Matroska defines no configuration record for either,
// and a VPxConfig, which the ISO-BMFF sample entry cannot do without, is
// accepted and left unwritten here.
func webmNoPrivate(TrackConfig) (webmCodecPrivate, error) { return webmCodecPrivate{}, nil }

// webmAV1Private states the AV1 configuration record, which the AV1-in-Matroska
// mapping requires as the private data, and which is the payload of the av1C box
// of an MP4 — so a track remuxed from an MP4 hands over exactly what it read.
func webmAV1Private(cfg TrackConfig) (webmCodecPrivate, error) {
	// Decoded only to be checked: what is written is the caller's own record,
	// byte for byte, never a re-encoding of it.
	if _, err := decodeAv1C(cfg.CodecConfig); err != nil {
		return webmCodecPrivate{}, err
	}
	return webmCodecPrivate{private: cfg.CodecConfig}, nil
}

// These indirections exist so the failures they guard can be tested. Both
// records are encoded into a buffer that cannot fail, and both encoders only
// report the writer's failures and their own inconsistency, so no configuration
// reaching them through this library makes them fail — and code whose behaviour
// nobody can observe is code nobody knows.
var (
	encodeAVCRecord  = (*avc.DecConfRec).Encode
	encodeHEVCRecord = (*hevc.DecConfRec).Encode
)

// webmAVCPrivate states the AVC decoder configuration record, which is what
// both Matroska's private data and the avcC box of an MP4 carry.
func webmAVCPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
		return webmCodecPrivate{}, fmt.Errorf("%w: V_MPEG4/ISO/AVC needs both SPS and PPS",
			ErrTrackConfig)
	}
	record, err := avc.CreateAVCDecConfRec(cfg.SPS, cfg.PPS, true)
	if err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: AVC parameter sets are not usable: %v",
			ErrTrackConfig, err)
	}
	var buf bytes.Buffer
	if err := encodeAVCRecord(record, &buf); err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: AVC configuration record: %v", ErrTrackConfig, err)
	}
	return webmCodecPrivate{private: buf.Bytes()}, nil
}

// webmHEVCPrivate states the HEVC decoder configuration record, the payload of
// an MP4's hvcC box.
func webmHEVCPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	if len(cfg.VPS) == 0 || len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
		return webmCodecPrivate{}, fmt.Errorf("%w: V_MPEGH/ISO/HEVC needs VPS, SPS and PPS",
			ErrTrackConfig)
	}
	record, err := hevc.CreateHEVCDecConfRec(cfg.VPS, cfg.SPS, cfg.PPS, true, true, true, true)
	if err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: HEVC parameter sets are not usable: %v",
			ErrTrackConfig, err)
	}
	var buf bytes.Buffer
	if err := encodeHEVCRecord(&record, &buf); err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: HEVC configuration record: %v", ErrTrackConfig, err)
	}
	return webmCodecPrivate{private: buf.Bytes()}, nil
}

// webmOpusPrivate states the Opus identification header, which is what Matroska
// carries as the private data, together with the two timings the WebM Opus
// mapping requires: the pre-skip as a CodecDelay in nanoseconds, and the 80 ms
// of audio a decoder must be fed before its output is correct.
func webmOpusPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	dops, err := opusConfig(cfg)
	if err != nil {
		return webmCodecPrivate{}, err
	}
	return webmCodecPrivate{
		private: opusHeadBytes(dops),
		// The pre-skip is counted at 48 kHz whatever the track was recorded
		// at, so this is exact for every Opus track there is.
		codecDelay:  uint64(dops.PreSkip) * uint64(time.Second/time.Nanosecond) / opusOutputRate,
		seekPreRoll: opusSeekPreRoll,
		// An Opus decoder always outputs 48 kHz, and the mapping says the
		// track states that rate; the rate it was recorded at is in the
		// identification header.
		audio: &webmAudio{
			SamplingFrequency: opusOutputRate,
			Channels:          uint64(dops.OutputChannelCount),
		},
	}, nil
}

// webmVorbisPrivate states the three Vorbis setup headers, Xiph-laced, which is
// the only form Matroska defines and without which no decoder can start. They
// are unlaced with ebml-go's own unlacer to check they are all there, because a
// truncated set would be written as a track that opens and plays nothing.
func webmVorbisPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	audio, err := webmAudioFields(cfg, "A_VORBIS")
	if err != nil {
		return webmCodecPrivate{}, err
	}
	if len(cfg.CodecConfig) == 0 {
		return webmCodecPrivate{}, fmt.Errorf(
			"%w: A_VORBIS needs its three setup headers as CodecConfig", ErrTrackConfig)
	}
	unlacer, err := unlaceVorbisHeaders(bytes.NewReader(cfg.CodecConfig), int64(len(cfg.CodecConfig)))
	if err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: A_VORBIS setup headers are not Xiph-laced: %v",
			ErrTrackConfig, err)
	}
	var packets [][]byte
	for {
		packet, err := unlacer.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return webmCodecPrivate{}, fmt.Errorf("%w: A_VORBIS setup headers stop short: %v",
				ErrTrackConfig, err)
		}
		packets = append(packets, packet)
	}
	switch {
	case len(packets) != vorbisSetupPackets:
		return webmCodecPrivate{}, fmt.Errorf("%w: A_VORBIS needs %d setup headers, not %d",
			ErrTrackConfig, vorbisSetupPackets, len(packets))
	case len(packets[0]) == 0:
		return webmCodecPrivate{}, fmt.Errorf("%w: the first A_VORBIS setup header is empty",
			ErrTrackConfig)
	case packets[0][0] != vorbisIdentificationPacket:
		return webmCodecPrivate{}, fmt.Errorf(
			"%w: the first A_VORBIS setup header is of type %d, not the identification header",
			ErrTrackConfig, packets[0][0])
	}
	return webmCodecPrivate{private: cfg.CodecConfig, audio: audio}, nil
}

// unlaceVorbisHeaders takes the Vorbis private data apart. It is a variable so
// an unlacer that stops part way can be staged: ebml-go's own hands back frame
// sizes that always add up to the data it was given, so nothing this library can
// pass it makes a packet come up short.
var unlaceVorbisHeaders = func(r io.Reader, size int64) (ebml.Unlacer, error) {
	return ebml.NewXiphUnlacer(r, size)
}

// webmAACPrivate states the AudioSpecificConfig, which is what Matroska carries
// as the private data of an AAC track and what the esds of an MP4 holds.
func webmAACPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	audio, err := webmAudioFields(cfg, "A_AAC")
	if err != nil {
		return webmCodecPrivate{}, err
	}
	if cfg.Channels > aacMaxChannelConfiguration {
		return webmCodecPrivate{}, fmt.Errorf(
			"%w: A_AAC cannot state %d channels; %d is the largest configuration",
			ErrTrackConfig, cfg.Channels, aacMaxChannelConfiguration)
	}
	objectType := cfg.AudioObjectType
	if objectType == 0 {
		objectType = aac.AAClc
	}
	asc := aac.AudioSpecificConfig{
		ObjectType:           objectType,
		ChannelConfiguration: byte(cfg.Channels),
		SamplingFrequency:    cfg.SampleRate,
		ExtensionFrequency:   cfg.SampleRate,
	}
	var buf bytes.Buffer
	if err := asc.Encode(&buf); err != nil {
		return webmCodecPrivate{}, fmt.Errorf("%w: A_AAC configuration: %v", ErrTrackConfig, err)
	}
	return webmCodecPrivate{private: buf.Bytes(), audio: audio}, nil
}

// webmAudioFields is the Audio element of a track whose rate and channel count
// come from the configuration rather than from a header of its own.
func webmAudioFields(cfg TrackConfig, id string) (*webmAudio, error) {
	switch {
	case cfg.SampleRate <= 0:
		return nil, fmt.Errorf("%w: %s needs its sample rate", ErrTrackConfig, id)
	case cfg.Channels <= 0:
		return nil, fmt.Errorf("%w: %s needs a channel count", ErrTrackConfig, id)
	}
	return &webmAudio{
		SamplingFrequency: float64(cfg.SampleRate),
		Channels:          uint64(cfg.Channels),
	}, nil
}

// WriteSample appends one frame to a track. The header and the track list are
// written before the first one in streaming mode, and every mode starts a new
// cluster when the open one has held a cluster's worth of media and the first
// track reaches a sync sample — which is what lets a player start there — or
// when the frame's timestamp no longer fits the 16 bits a block states it in.
//
// Matroska blocks carry presentation times, not decoding times, so a sample's
// CompositionOffset is added to its time rather than written beside it. Samples
// are written in the order handed over: a caller interleaving several tracks
// keeps them in step, as it must for the fragmented MP4 muxer too.
func (m *WebMMuxer) WriteSample(trackID uint32, s Sample) error {
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
	presentation := int64(t.nextTime) + int64(s.CompositionOffset)
	if presentation < 0 {
		return fmt.Errorf("%w: a composition offset of %d puts the frame at %d, before the track starts",
			ErrSample, s.CompositionOffset, presentation)
	}
	tick, err := m.ticksFor(uint64(presentation), t.timescale)
	if err != nil {
		return err
	}
	if !m.started {
		if err := m.begin(); err != nil {
			return err
		}
	}
	if m.needNewCluster(tick, t, s.Sync) {
		if err := m.openCluster(tick); err != nil {
			return err
		}
	}
	if err := m.writeBlock(ebml.Block{
		TrackNumber: uint64(t.id),
		Timecode:    int16(int64(tick) - int64(m.clusterStart)),
		Keyframe:    s.Sync,
		Data:        [][]byte{s.Data},
	}); err != nil {
		return err
	}
	t.nextTime += uint64(s.Duration)
	if end := uint64(presentation) + uint64(s.Duration); end > t.end {
		t.end = end
	}
	return nil
}

// begin writes what precedes the first cluster, in the mode that writes it
// early. The buffered mode cannot: its Info states a duration only the last
// sample knows.
func (m *WebMMuxer) begin() error {
	if !m.settings.buffered {
		doc := webmStreamDoc{
			Header: m.ebmlHeader(),
			Segment: webmOpenSegment{
				Info:   m.info(0),
				Tracks: webmTracks{TrackEntry: m.entries},
			},
		}
		if err := ebml.Marshal(&doc, m.w); err != nil {
			return fmt.Errorf("container: write webm header: %w", err)
		}
	}
	m.started = true
	return nil
}

// needNewCluster reports whether this frame has to open a cluster of its own.
func (m *WebMMuxer) needNewCluster(tick uint64, t *webmTrack, sync bool) bool {
	if !m.clusterOpen {
		return true
	}
	relative := int64(tick) - int64(m.clusterStart)
	if relative > maxBlockTimecode || relative < minBlockTimecode {
		return true
	}
	// Cutting on a sync sample of the first track added is what makes a
	// cluster a place a player can start at.
	return sync && t == m.tracks[0] && relative >= int64(m.clusterTicks())
}

// openCluster starts a cluster at this tick.
func (m *WebMMuxer) openCluster(tick uint64) error {
	if m.settings.buffered {
		m.clusters = append(m.clusters, webmCluster{Timecode: tick})
	} else if err := ebml.Marshal(&webmOpenCluster{Cluster: webmClusterHead{Timecode: tick}}, m.w); err != nil {
		return fmt.Errorf("container: write cluster at %d: %w", tick, err)
	}
	m.clusterStart, m.clusterOpen = tick, true
	return nil
}

// writeBlock adds one block to the open cluster.
func (m *WebMMuxer) writeBlock(b ebml.Block) error {
	if m.settings.buffered {
		cluster := &m.clusters[len(m.clusters)-1]
		cluster.SimpleBlock = append(cluster.SimpleBlock, b)
		return nil
	}
	if err := ebml.Marshal(&webmBlockElement{Block: b}, m.w); err != nil {
		return fmt.Errorf("container: write block on track %d: %w", b.TrackNumber, err)
	}
	return nil
}

// Close finishes the file and refuses any further use. It reports an error when
// no track was ever declared, because that file would name nothing.
func (m *WebMMuxer) Close() error {
	if m.closed {
		return ErrClosed
	}
	if len(m.tracks) == 0 {
		m.closed = true
		return ErrNoTracks
	}
	m.closed = true
	if !m.settings.buffered {
		// Every cluster is already out; a track declared and never fed still
		// deserves the header that names it.
		if m.started {
			return nil
		}
		doc := webmStreamDoc{
			Header: m.ebmlHeader(),
			Segment: webmOpenSegment{
				Info:   m.info(0),
				Tracks: webmTracks{TrackEntry: m.entries},
			},
		}
		if err := ebml.Marshal(&doc, m.w); err != nil {
			return fmt.Errorf("container: write webm header: %w", err)
		}
		return nil
	}
	duration, err := m.duration()
	if err != nil {
		return err
	}
	doc := webmSizedDoc{
		Header: m.ebmlHeader(),
		Segment: webmSegment{
			Info:    m.info(duration),
			Tracks:  webmTracks{TrackEntry: m.entries},
			Cluster: m.clusters,
		},
	}
	if err := ebml.Marshal(&doc, m.w); err != nil {
		return fmt.Errorf("container: write webm segment: %w", err)
	}
	return nil
}

// duration is how long the longest track lasts, in segment ticks.
func (m *WebMMuxer) duration() (float64, error) {
	var longest uint64
	for _, t := range m.tracks {
		end, err := m.ticksFor(t.end, t.timescale)
		if err != nil {
			return 0, err
		}
		if end > longest {
			longest = end
		}
	}
	return float64(longest), nil
}

// ebmlHeader is the EBML header of the document, whose type follows the tracks:
// a file carrying a codec WebM does not allow says so, instead of claiming to be
// a WebM a strict player would then refuse.
func (m *WebMMuxer) ebmlHeader() webm.EBMLHeader {
	header := *webm.DefaultEBMLHeader
	for _, t := range m.tracks {
		if !t.inWebM {
			header.DocType = "matroska"
			break
		}
	}
	return header
}

// info is the segment's Info element. A duration of zero is left unwritten,
// which is what a streaming segment, whose duration nothing knows yet, states.
func (m *WebMMuxer) info(duration float64) webmInfo {
	return webmInfo{
		TimecodeScale: uint64(m.settings.tick / time.Nanosecond),
		MuxingApp:     webmMuxingApp,
		WritingApp:    webmMuxingApp,
		Duration:      duration,
	}
}

// ticksPerSecond is how many segment ticks a second holds. TimestampScale keeps
// the tick a whole divisor of a second, so this is exact.
func (m *WebMMuxer) ticksPerSecond() uint64 { return uint64(time.Second / m.settings.tick) }

// clusterTicks is the cluster bound in segment ticks, never less than one: a
// bound of zero ticks would put every block in a cluster of its own.
func (m *WebMMuxer) clusterTicks() uint64 {
	if ticks := uint64(m.settings.clusterDuration / m.settings.tick); ticks > 0 {
		return ticks
	}
	return 1
}

// ticksFor converts a time counted in a track's own timescale into segment
// ticks, rounded to the nearest tick.
//
// Every timestamp is converted from the track's cumulative time in its own
// units, never from the tick written before it, so the rounding error stays
// below half a tick however long the file runs instead of accumulating one
// rounding per sample. The multiplication is carried out in 128 bits, so a
// timescale as fine as a nanosecond and a track time as long as the type allows
// do not silently wrap; a time this segment's resolution cannot state is refused
// instead.
func (m *WebMMuxer) ticksFor(units uint64, timescale uint32) (uint64, error) {
	scale := uint64(timescale)
	hi, lo := bits.Mul64(units, m.ticksPerSecond())
	// Rounding half up, by adding half the divisor before dividing. The carry
	// cannot make hi wrap: a tick is at least a nanosecond, so there are at
	// most a thousand million of them per second and hi stays far below the
	// type's ceiling.
	lo, carry := bits.Add64(lo, scale/2, 0)
	hi += carry
	// Ticks are subtracted from one another to state a block against its
	// cluster, so the largest time this can state is the one whose tick count
	// still fits a signed 64-bit number. Comparing against that product rather
	// than dividing first is also what keeps the division below in range: it
	// leaves hi under half of scale, and a 128-by-64 division needs hi below
	// scale not to overflow its own quotient.
	limitHi, limitLo := bits.Mul64(math.MaxInt64, scale)
	if hi > limitHi || (hi == limitHi && lo > limitLo) {
		return 0, fmt.Errorf(
			"%w: a time of %d in a timescale of %d is beyond what %d ticks per second can state",
			ErrSample, units, timescale, m.ticksPerSecond())
	}
	ticks, _ := bits.Div64(hi, lo, scale)
	return ticks, nil
}

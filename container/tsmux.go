// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/asticode/go-astits"
)

// Writing a transport stream undoes what ts.go does when reading one, so the
// two directions meet on the same TrackConfig and Sample values: what Reader
// hands back, TSMuxer takes, which is what remuxing an MP4 into an HLS segment
// needs.
//
// A transport stream is meant to be joined part-way through, so it states in
// band what an MP4 states once: the parameter sets go in front of every sync
// sample, and every AAC frame carries the header describing its own format.
//
// Packet and table writing is delegated to asticode/go-astits, as parsing is
// when reading; this file converts the payloads and keeps the clock.

// tsFirstPID is the packet identifier given to the first elementary stream.
// Identifiers below it are reserved for the tables.
const tsFirstPID = 0x100

// tsClockWrap is one past the largest timestamp a transport stream can state:
// its clock fields are 33 bits wide, so a long stream counts around.
const tsClockWrap = int64(1) << 33

// tsADTSPayloadMax is the largest AAC frame an ADTS header can announce: the
// length field is 13 bits wide and counts the seven header bytes as well.
const tsADTSPayloadMax = 1<<13 - 1 - 7

// tsMaxChannelConfig is the highest channel configuration an ADTS header can
// state, its field being three bits wide. Nothing smaller than one is a
// configuration either: zero means "read it from the codec configuration
// instead", which a transport stream does not carry.
const tsMaxChannelConfig = 7

// addElementaryStream exists so the failure it guards can be tested: this
// muxer allocates every packet identifier itself, from a counter, so the
// collision astits reports there cannot be provoked through the library.
var addElementaryStream = (*astits.Muxer).AddElementaryStream

// TSMuxer writes an MPEG-2 transport stream: a program map naming every
// elementary stream, repeated often enough that a player joining part-way
// through can still make sense of what follows.
//
// It is the counterpart of the Reader's transport stream side, and never
// re-encodes: samples are converted to the form a transport stream states them
// in — Annex-B for AVC and HEVC, the AOM start-code form for AV1, ADTS for AAC
// — and written as handed over. AC-3, Enhanced AC-3 and MPEG audio (Layers I
// to III, named "mp1", "mp2" and "mp3") need no conversion: their frames are
// the same in both containers. Opus is refused, no published specification
// carrying it in a transport stream.
//
// Every track is presented from its first presented frame: its first
// composition offset and its StartDelay are taken off its timestamps, and the
// program starts 1.4 s into the clock (see tsTimestampOffset).
type TSMuxer struct {
	ts      *astits.Muxer
	tracks  []*tsMuxTrack
	byID    map[uint32]*tsMuxTrack
	nextPID uint16
	pcrPID  uint16
	started bool
	closed  bool
}

// tsMuxTrack is one elementary stream's writing state.
type tsMuxTrack struct {
	pid       uint16
	kind      Kind
	timescale uint32
	// form is what a sample is converted with, codec the name it was
	// declared under, and streamID the PES stream identifier, which is
	// stated rather than left to astits: see tsaudio.go.
	form     tsMuxForm
	codec    string
	streamID uint8
	// params are the sets a decoder needs to start, repeated in front of
	// every sync sample.
	params parameterSets
	// adts is the header every frame of an audio track repeats, its length
	// field aside. Building it once is also what checks the configuration.
	adts *aac.ADTSHeader
	// nextTime is the decode time of the next sample, in the track's own
	// timescale.
	nextTime int64
	// priming is the track's StartDelay in its own timescale, and start the
	// media time its presentation begins at: the first sample's composition
	// offset, then the priming. started says the first sample has fixed it.
	priming int64
	start   int64
	started bool
}

// The program clock, ISO/IEC 13818-1 §2.4.3.7 and §2.4.2.
//
// A transport stream has no edit list: what is presented when is the PTS of
// every unit and nothing else. A track that reorders its frames (H.264 with
// B-frames) presents its first frame at its composition offset, and a track
// with an encoder priming presents media it should not; both must be taken off
// the timestamps, so every track's first PRESENTED frame lands on the same
// instant. The units decoded before that instant -- the reordered pictures'
// first decode, the priming frame -- then need a DTS (and the priming a PTS)
// before it, which the 33-bit unsigned fields cannot state below zero.
//
// ffmpeg's mpegtsenc.c solves it with a program-wide offset: write_packet adds
// 2 × max_delay to every PTS and DTS, and the PCR is stated max_delay before
// the DTS of the unit carrying it (mpegts_write_pes), which is also the time
// the T-STD buffer is given to receive a unit before it is decoded. The ffmpeg
// command line's -muxdelay default is 0.7 s, and the same values are used here.
const (
	// tsMuxDelay is how far the clock reference runs ahead of the decode time
	// of the unit carrying it: 0.7 s of the 90 kHz clock.
	tsMuxDelay = int64(63000)
	// tsTimestampOffset is where the program's presentation begins on the
	// 90 kHz clock, and so how much media may be decoded or presented ahead
	// of it: twice tsMuxDelay, 1.4 s, of which a unit may use tsMuxDelay.
	tsTimestampOffset = 2 * tsMuxDelay
)

// tsMuxForm is the conversion a track's samples go through on their way in.
type tsMuxForm uint8

const (
	tsMuxAnnexB    tsMuxForm = iota // AVC and HEVC: start codes, sets in band
	tsMuxAV1                        // AV1: the AOM start-code form
	tsMuxADTS                       // AAC: an ADTS header in front
	tsMuxAC3                        // AC-3: the sync frames as they are
	tsMuxEAC3                       // Enhanced AC-3, likewise
	tsMuxMPEGAudio                  // MPEG audio: the frames as they are
)

// NewTSMuxer returns a TSMuxer writing to w.
func NewTSMuxer(w io.Writer) *TSMuxer {
	return &TSMuxer{
		// The context is what astits cancels a write with; this muxer writes
		// only when told to, so nothing here ever cancels.
		ts:      astits.NewMuxer(context.Background(), w),
		byID:    map[uint32]*tsMuxTrack{},
		nextPID: tsFirstPID,
	}
}

// AddTrack declares an elementary stream and returns its packet identifier,
// which is what a transport stream names a track by. Every track must be added
// before the first sample, because the program map describing them all is
// written in front of it.
func (m *TSMuxer) AddTrack(cfg TrackConfig) (uint32, error) {
	switch {
	case m.closed:
		return 0, ErrClosed
	case m.started:
		return 0, fmt.Errorf("%w: tracks cannot be added once writing has begun", ErrTrackConfig)
	case cfg.Timescale == 0:
		return 0, fmt.Errorf("%w: %s track has no timescale", ErrTrackConfig, cfg.Codec)
	}
	t := &tsMuxTrack{pid: m.nextPID, timescale: cfg.Timescale,
		priming: tsPrimingTicks(cfg.StartDelay, cfg.Timescale)}
	streamType, descriptors, err := t.describe(cfg)
	if err != nil {
		return 0, err
	}
	if err := addElementaryStream(m.ts, astits.PMTElementaryStream{
		ElementaryPID: t.pid, StreamType: streamType,
		ElementaryStreamDescriptors: descriptors,
	}); err != nil {
		return 0, fmt.Errorf("container: declare stream %d: %w", t.pid, err)
	}
	m.nextPID++
	m.tracks = append(m.tracks, t)
	m.byID[uint32(t.pid)] = t
	m.setPCR()
	return uint32(t.pid), nil
}

// setPCR states which stream carries the clock a player locks its own onto:
// the video one when there is one, because that is the timing a viewer
// notices, and the first stream otherwise.
func (m *TSMuxer) setPCR() {
	carrier := m.tracks[0]
	for _, t := range m.tracks {
		if t.kind == Video {
			carrier = t
			break
		}
	}
	m.pcrPID = carrier.pid
	m.ts.SetPCRPID(carrier.pid)
}

// describe reads a codec's configuration into what every sample of the track
// is converted with, and returns the stream type and the descriptors the
// program map states.
func (t *tsMuxTrack) describe(cfg TrackConfig) (astits.StreamType, []*astits.Descriptor, error) {
	codec := strings.ToLower(strings.TrimSpace(cfg.Codec))
	t.codec = codec
	switch codec {
	case "avc1", "avc3":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
			return 0, nil, fmt.Errorf("%w: %s needs both SPS and PPS", ErrTrackConfig, codec)
		}
		t.kind = Video
		t.params = parameterSets{sps: cfg.SPS, pps: cfg.PPS}
		return astits.StreamTypeH264Video, nil, nil
	case "hvc1", "hev1":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 || len(cfg.VPS) == 0 {
			return 0, nil, fmt.Errorf("%w: %s needs VPS, SPS and PPS", ErrTrackConfig, codec)
		}
		t.kind = Video
		t.params = parameterSets{sps: cfg.SPS, pps: cfg.PPS, vps: cfg.VPS}
		return astits.StreamTypeH265Video, nil, nil
	case "av01":
		// The av1C record is read through the same decoder the MP4 muxer
		// uses, so a record one refuses the other refuses too; the AV1 video
		// descriptor is built from its first four bytes.
		if _, err := decodeAv1C(cfg.CodecConfig); err != nil {
			return 0, nil, err
		}
		t.kind, t.form, t.streamID = Video, tsMuxAV1, tsPrivateStream1 // 3.4
		// The registration descriptor comes first, then the AV1 video
		// descriptor (2.1).
		return astits.StreamTypePrivateData, []*astits.Descriptor{
			registrationDescriptor(av1RegistrationID),
			{
				Tag: av1VideoDescriptorTag, Length: 4,
				UserDefined: av1VideoDescriptor(cfg.CodecConfig),
			},
		}, nil
	case "ac-3", "ec-3":
		// ATSC A/52 System A, which is what FFmpeg's mpegtsenc.c writes
		// unless told "-mpegts_flags system_b": the stream type identifies
		// the codec (Annex A 4.1, Annex G 3.1), and the AC-3 registration
		// descriptor goes with BOTH — Annex G 2 extends it to Enhanced AC-3
		// "in combination with the E-AC-3 stream_type value", keeping "EAC3"
		// for systems that have no stream type of their own.
		//
		// ⛔ FFmpeg writes "EAC3" next to 0x87 instead. Its reader, and this
		// package's, decide by the stream type first, so both forms read;
		// the one written here is the one the standard states.
		t.kind, t.form, t.streamID = Audio, tsMuxAC3, tsPrivateStream1 // A4.2, G3.2
		streamType := astits.StreamTypeAC3Audio
		if codec == "ec-3" {
			t.form, streamType = tsMuxEAC3, astits.StreamTypeEAC3Audio
		}
		return streamType, []*astits.Descriptor{registrationDescriptor(ac3RegistrationID)}, nil
	case "mp1", "mp2", "mp3":
		streamType, ok := mpaStreamType(cfg.SampleRate)
		if !ok {
			return 0, nil, fmt.Errorf("%w: %s at %d Hz: MPEG audio is carried at 32, 44.1 and 48 kHz "+
				"(stream type 0x03) or 16, 22.05 and 24 kHz (0x04), and the rate is what decides",
				ErrTrackConfig, codec, cfg.SampleRate)
		}
		t.kind, t.form, t.streamID = Audio, tsMuxMPEGAudio, tsAudioStream0
		return streamType, nil, nil
	case "opus":
		// ⛔ Not an omission. The only text that maps Opus onto a transport
		// stream is a draft ETSI specification, "TS 1xx xxx V0.1.3
		// (2015-10)", which was never published. FFmpeg writes it anyway;
		// this package writes what a published specification defines.
		return 0, nil, fmt.Errorf("%w: opus: no published specification carries Opus in an MPEG-TS "+
			"(the only one, ETSI TS opus V0.1.3, is a 2015 draft that was never adopted)", ErrUnsupportedCodec)
	case "mp4a":
		if cfg.Channels < 1 || cfg.Channels > tsMaxChannelConfig {
			return 0, nil, fmt.Errorf("%w: mp4a needs between 1 and %d channels, not %d",
				ErrTrackConfig, tsMaxChannelConfig, cfg.Channels)
		}
		objectType := cfg.AudioObjectType
		if objectType == 0 {
			objectType = aac.AAClc
		}
		// The header carries the sample rate as an index into a fixed table
		// and states the profile in two bits, so a rate or a profile it has
		// no room for is a configuration no transport stream can state.
		header, err := aac.NewADTSHeader(cfg.SampleRate, byte(cfg.Channels), objectType, 0)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: mp4a: %v", ErrTrackConfig, err)
		}
		t.kind, t.form, t.adts = Audio, tsMuxADTS, header
		return astits.StreamTypeAACAudio, nil, nil
	case codecWebVTT, codecTTML, codecTx3g:
		// Said apart from an unknown codec because the codec is known and
		// carried elsewhere: a transport stream's subtitles are DVB bitmaps
		// or teletext pages, and an HLS player takes WebVTT as segments of
		// its own, beside the stream rather than inside it.
		return 0, nil, fmt.Errorf("%w: %q: subtitles are not written into an MPEG-TS by this package",
			ErrUnsupportedCodec, cfg.Codec)
	case codecPGS:
		return 0, nil, errPGSNotInTS()
	default:
		return 0, nil, fmt.Errorf("%w: %q", ErrUnsupportedCodec, cfg.Codec)
	}
}

// registrationDescriptor is the ISO/IEC 13818-1 2.6.8 descriptor that names
// the format of a stream whose stream type does not.
func registrationDescriptor(formatIdentifier uint32) *astits.Descriptor {
	return &astits.Descriptor{
		Tag: astits.DescriptorTagRegistration, Length: 4,
		Registration: &astits.DescriptorRegistration{FormatIdentifier: formatIdentifier},
	}
}

// WriteSample writes one frame as a packetised unit of its track. Each sample
// is a unit of its own, so the reader of the stream recovers exactly the
// samples that were written.
func (m *TSMuxer) WriteSample(trackID uint32, s Sample) error {
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
	payload, err := t.packetise(s)
	if err != nil {
		return err
	}
	// Presentation is shifted against decoding by a stream that reorders its
	// frames, and every time is counted from the track's first presented frame
	// (see tsTimestampOffset). Kept aside until the sample is accepted: a
	// refused first sample must not decide where the next one is shown.
	start := t.start
	if !t.started {
		start = max(int64(s.CompositionOffset), 0) + t.priming
	}
	dts := t.clock(t.nextTime - start)
	pts := t.clock(t.nextTime + int64(s.CompositionOffset) - start)
	if dts < tsTimestampOffset-tsMuxDelay || pts < tsTimestampOffset-tsMuxDelay {
		return fmt.Errorf("%w: a composition offset of %d and a start delay of %d ticks put the frame "+
			"more than %d ms before the start of the program, which its clock cannot state",
			ErrSample, s.CompositionOffset, t.priming, tsMuxDelay*1000/TSTimescale)
	}
	data := &astits.MuxerData{
		PID: t.pid,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				// Left at zero, the stream identifier is left to astits,
				// which takes the one the standard fixes for the type of
				// the stream; the codecs it gets wrong state their own.
				StreamID:       t.streamID,
				OptionalHeader: t.timing(dts, pts),
			},
			Data: payload,
		},
	}
	if t.pid == m.pcrPID {
		// The clock a player recovers its own timing from, and the flag
		// saying a decoder may start here — which is also what makes astits
		// repeat the tables, so joining at this point describes itself.
		data.AdaptationField = &astits.PacketAdaptationField{
			HasPCR:                true,
			PCR:                   &astits.ClockReference{Base: (dts - tsMuxDelay) % tsClockWrap},
			RandomAccessIndicator: s.Sync,
		}
	}
	// The tables describing the program go out with this unit, so the program
	// is settled from here on whether the write succeeds or not.
	m.started = true
	if _, err := m.ts.WriteData(data); err != nil {
		return fmt.Errorf("container: write unit of stream %d: %w", t.pid, err)
	}
	t.nextTime += int64(s.Duration)
	t.start, t.started = start, true
	return nil
}

// tsPrimingTicks is a StartDelay in a track's own timescale, rounded to the
// nearest tick, as the MP4 muxers round the edit list stating it.
func tsPrimingTicks(delay time.Duration, timescale uint32) int64 {
	if delay <= 0 {
		return 0
	}
	return (int64(delay)*int64(timescale) + int64(time.Second)/2) / int64(time.Second)
}

// timing states when a unit is decoded and shown. astits refuses to write a
// unit with no timestamp at all, and a decode time is only worth stating when
// it differs from the presentation time.
func (t *tsMuxTrack) timing(dts, pts int64) *astits.PESOptionalHeader {
	h := &astits.PESOptionalHeader{
		MarkerBits: 2,
		// Every unit this muxer writes starts on a start code or a sync
		// word, which is what the flag states: AOM's AV1 carriage (3.4)
		// and ATSC A/52 Annex G (3.3) require it.
		DataAlignmentIndicator: true,
		PTSDTSIndicator:        astits.PTSDTSIndicatorOnlyPTS,
		PTS:                    &astits.ClockReference{Base: pts % tsClockWrap},
	}
	if dts != pts {
		h.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
		h.DTS = &astits.ClockReference{Base: dts % tsClockWrap}
	}
	return h
}

// clock converts a time in the track's own timescale, counted from the
// track's first presented frame, into the 90 kHz clock every timestamp in a
// transport stream is counted in, on which the program starts at
// tsTimestampOffset. It is not wrapped yet: the fields it is written to count
// around at 33 bits, and timing does that.
func (t *tsMuxTrack) clock(when int64) int64 {
	return when*TSTimescale/int64(t.timescale) + tsTimestampOffset
}

// packetise converts one sample into the bytes its elementary stream carries.
func (t *tsMuxTrack) packetise(s Sample) ([]byte, error) {
	switch t.form {
	case tsMuxAnnexB:
		payload, ok := toAnnexB(s.Data, t.params, s.Sync)
		if !ok {
			return nil, fmt.Errorf("%w: %d bytes are not length-prefixed NAL units",
				ErrSample, len(s.Data))
		}
		return payload, nil
	case tsMuxAV1:
		return av1ToStartCodes(s.Data)
	case tsMuxAC3, tsMuxEAC3:
		// A sample is one access unit, which ATSC A/52 Annex G 3.3 forbids
		// splitting across units: written as one, it stays whole.
		return s.Data, checkAC3Sample(s.Data, t.form == tsMuxEAC3)
	case tsMuxMPEGAudio:
		return s.Data, checkMPASample(s.Data, t.codec)
	default:
		return toADTS(s.Data, *t.adts)
	}
}

// toAnnexB turns the length-prefixed NAL units of a sample back into the
// start-code separated form a transport stream carries, and puts the parameter
// sets in front of a sync sample: a player may join the stream there, and it
// cannot start decoding without them.
func toAnnexB(sample []byte, params parameterSets, sync bool) ([]byte, bool) {
	nalus, ok := splitLengthPrefixed(sample)
	if !ok {
		return nil, false
	}
	var out bytes.Buffer
	if sync {
		// In the order a decoder reads them: a sequence set refers to the
		// video set, and a picture set to the sequence set.
		for _, set := range [][][]byte{params.vps, params.sps, params.pps} {
			for _, nalu := range set {
				writeStartCode(&out)
				out.Write(nalu)
			}
		}
	}
	for _, nalu := range nalus {
		writeStartCode(&out)
		out.Write(nalu)
	}
	return out.Bytes(), true
}

// writeStartCode writes the four-byte code a transport stream separates its
// NAL units with.
func writeStartCode(w *bytes.Buffer) {
	w.Write([]byte{0, 0, 0, 1})
}

// toADTS puts back the header a transport stream states an AAC frame's format
// in: an MP4 states it once in its sample entry, so the frames it hands over
// are raw.
func toADTS(frame []byte, header aac.ADTSHeader) ([]byte, error) {
	if len(frame) > tsADTSPayloadMax {
		return nil, fmt.Errorf("%w: an ADTS frame holds at most %d bytes, not %d",
			ErrSample, tsADTSPayloadMax, len(frame))
	}
	// The field counts the frame alone; the encoder adds the header's own
	// seven bytes back.
	header.PayloadLength = uint16(len(frame))
	return append(header.Encode(), frame...), nil
}

// Close writes what a stream carrying nothing still owes — the tables naming
// its tracks — and refuses any further use. It reports an error when no track
// was ever declared, because that stream would describe nothing.
func (m *TSMuxer) Close() error {
	if m.closed {
		return ErrClosed
	}
	m.closed = true
	switch {
	case len(m.tracks) == 0:
		return ErrNoTracks
	case m.started:
		// Every unit is written as it is handed over, tables included, so
		// there is nothing held back to write here.
		return nil
	}
	if _, err := m.ts.WriteTables(); err != nil {
		return fmt.Errorf("container: write tables: %w", err)
	}
	return nil
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

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
// in — Annex-B for AVC and HEVC, ADTS for AAC — and written as handed over.
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
	// params are the sets a decoder needs to start, repeated in front of
	// every sync sample.
	params parameterSets
	// adts is the header every frame of an audio track repeats, its length
	// field aside. Building it once is also what checks the configuration.
	adts *aac.ADTSHeader
	// nextTime is the decode time of the next sample, in the track's own
	// timescale.
	nextTime int64
}

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
	t := &tsMuxTrack{pid: m.nextPID, timescale: cfg.Timescale}
	streamType, err := t.describe(cfg)
	if err != nil {
		return 0, err
	}
	if err := addElementaryStream(m.ts, astits.PMTElementaryStream{
		ElementaryPID: t.pid, StreamType: streamType,
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
// is converted with, and returns the stream type the program map states.
func (t *tsMuxTrack) describe(cfg TrackConfig) (astits.StreamType, error) {
	codec := strings.ToLower(strings.TrimSpace(cfg.Codec))
	switch codec {
	case "avc1", "avc3":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
			return 0, fmt.Errorf("%w: %s needs both SPS and PPS", ErrTrackConfig, codec)
		}
		t.kind = Video
		t.params = parameterSets{sps: cfg.SPS, pps: cfg.PPS}
		return astits.StreamTypeH264Video, nil
	case "hvc1", "hev1":
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 || len(cfg.VPS) == 0 {
			return 0, fmt.Errorf("%w: %s needs VPS, SPS and PPS", ErrTrackConfig, codec)
		}
		t.kind = Video
		t.params = parameterSets{sps: cfg.SPS, pps: cfg.PPS, vps: cfg.VPS}
		return astits.StreamTypeH265Video, nil
	case "mp4a":
		if cfg.Channels < 1 || cfg.Channels > tsMaxChannelConfig {
			return 0, fmt.Errorf("%w: mp4a needs between 1 and %d channels, not %d",
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
			return 0, fmt.Errorf("%w: mp4a: %v", ErrTrackConfig, err)
		}
		t.kind, t.adts = Audio, header
		return astits.StreamTypeAACAudio, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnsupportedCodec, cfg.Codec)
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
	// frames. Before the start of the stream it cannot be stated at all.
	dts := t.nextTime
	pts := dts + int64(s.CompositionOffset)
	if pts < 0 {
		return fmt.Errorf("%w: a composition offset of %d puts the frame before the start of the stream",
			ErrSample, s.CompositionOffset)
	}
	data := &astits.MuxerData{
		PID: t.pid,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				// The stream identifier is left to astits, which takes the
				// one the standard fixes for the type of the stream.
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
			PCR:                   &astits.ClockReference{Base: t.rescale(dts)},
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
	return nil
}

// timing states when a unit is decoded and shown. astits refuses to write a
// unit with no timestamp at all, and a decode time is only worth stating when
// it differs from the presentation time.
func (t *tsMuxTrack) timing(dts, pts int64) *astits.PESOptionalHeader {
	h := &astits.PESOptionalHeader{
		MarkerBits: 2,
		// Every unit this muxer writes starts on a start code or an ADTS
		// syncword, which is what the flag states.
		DataAlignmentIndicator: true,
		PTSDTSIndicator:        astits.PTSDTSIndicatorOnlyPTS,
		PTS:                    &astits.ClockReference{Base: t.rescale(pts)},
	}
	if dts != pts {
		h.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
		h.DTS = &astits.ClockReference{Base: t.rescale(dts)}
	}
	return h
}

// rescale converts a time in the track's own timescale into the 90 kHz clock
// every timestamp in a transport stream is counted in, counting around at the
// width of the field it is written to.
func (t *tsMuxTrack) rescale(when int64) int64 {
	return (when * TSTimescale / int64(t.timescale)) % tsClockWrap
}

// packetise converts one sample into the bytes its elementary stream carries.
func (t *tsMuxTrack) packetise(s Sample) ([]byte, error) {
	if t.kind == Video {
		payload, ok := toAnnexB(s.Data, t.params, s.Sync)
		if !ok {
			return nil, fmt.Errorf("%w: %d bytes are not length-prefixed NAL units",
				ErrSample, len(s.Data))
		}
		return payload, nil
	}
	return toADTS(s.Data, *t.adts)
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

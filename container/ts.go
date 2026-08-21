// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/asticode/go-astits"
)

// MPEG-TS carries its elementary streams in 188-byte packets, and says what
// they are in a table it repeats. Reading one means reassembling the packetised
// units, then handing over what the tracks hold in the form an MP4 wants:
// length-prefixed NAL units for video, raw frames for AAC.
//
// Packet and table parsing is delegated to asticode/go-astits, as MP4 parsing
// is to mp4ff; this file projects its structures onto the same File/Track model
// and converts the payloads.

// TSTimescale is the clock every timestamp in a transport stream is counted in.
const TSTimescale = 90000

// tsPacketSize is the length of a transport packet.
const tsPacketSize = 188

// ErrNoProgram means the stream never described what it carries.
var ErrNoProgram = errors.New("container: transport stream declares no program")

// sniffTS reports whether data looks like a transport stream: the sync byte at
// the start of consecutive packets is what distinguishes it.
func sniffTS(data []byte) bool {
	if len(data) < tsPacketSize+1 || data[0] != 0x47 {
		return false
	}
	for offset := tsPacketSize; offset < len(data) && offset <= 3*tsPacketSize; offset += tsPacketSize {
		if data[offset] != 0x47 {
			return false
		}
	}
	return true
}

// tsTrack is one elementary stream, read and converted.
type tsTrack struct {
	track  Track
	config TrackConfig
	// hevc says which codec's NAL headers the units carry, which the bytes
	// themselves do not.
	hevc    bool
	samples []Sample
	// pending holds each unit with the time it is shown at, so durations can
	// be worked out once the following one is known.
	pending []tsUnit
}

// tsUnit is one access unit before its duration is known.
type tsUnit struct {
	data []byte
	time int64 // decode time in TSTimescale units
}

// readTS reads a transport stream once, and returns its tracks with their
// samples already in the form a muxer takes.
func readTS(data []byte) ([]*tsTrack, error) {
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(data))
	tracks := map[uint16]*tsTrack{}
	var order []uint16

	for {
		d, err := dmx.NextData()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, astits.ErrNoMorePackets) {
				break
			}
			return nil, fmt.Errorf("container: read transport stream: %w", err)
		}
		switch {
		case d.PMT != nil:
			for _, es := range d.PMT.ElementaryStreams {
				if _, seen := tracks[es.ElementaryPID]; seen {
					continue
				}
				t := newTSTrack(es)
				if t == nil {
					continue // a stream this package cannot hand over
				}
				tracks[es.ElementaryPID] = t
				order = append(order, es.ElementaryPID)
			}
		case d.PES != nil:
			t, ok := tracks[d.PID]
			if !ok {
				continue
			}
			t.append(d.PES)
		}
	}
	if len(order) == 0 {
		return nil, ErrNoProgram
	}
	out := make([]*tsTrack, 0, len(order))
	for _, pid := range order {
		t := tracks[pid]
		t.finish()
		out = append(out, t)
	}
	return out, nil
}

// newTSTrack describes an elementary stream, or reports nothing for one this
// package cannot convert.
func newTSTrack(es *astits.PMTElementaryStream) *tsTrack {
	kind, codec := Other, ""
	switch es.StreamType {
	case astits.StreamTypeH264Video:
		kind, codec = Video, "avc1"
	case astits.StreamTypeH265Video:
		kind, codec = Video, "hvc1"
	case astits.StreamTypeAACAudio:
		kind, codec = Audio, "mp4a"
	default:
		return nil
	}
	id := uint32(es.ElementaryPID)
	return &tsTrack{
		track:  Track{ID: id, Kind: kind, Codec: codec, Timescale: TSTimescale},
		config: TrackConfig{Kind: kind, Codec: codec, Timescale: TSTimescale},
		hevc:   es.StreamType == astits.StreamTypeH265Video,
	}
}

// append converts one packetised unit and keeps it until its duration is known.
func (t *tsTrack) append(pes *astits.PESData) {
	when := pesTime(pes)
	switch t.track.Kind {
	case Video:
		payload, params := convertAnnexB(pes.Data, t.hevc)
		t.adoptParameterSets(params)
		if len(payload) > 0 {
			t.pending = append(t.pending, tsUnit{data: payload, time: when})
		}
	case Audio:
		frames, cfg := splitADTS(pes.Data)
		if cfg.SampleRate > 0 && t.config.SampleRate == 0 {
			t.config.SampleRate, t.config.Channels = cfg.SampleRate, cfg.Channels
			t.config.AudioObjectType = cfg.ObjectType
			t.track.SampleRate, t.track.Channels = cfg.SampleRate, cfg.Channels
		}
		// Every frame is its own access unit, evenly spread over the unit.
		for i, frame := range frames {
			t.pending = append(t.pending, tsUnit{
				data: frame,
				time: when + int64(i)*aacFrameDuration(cfg.SampleRate),
			})
		}
	}
}

// adoptParameterSets keeps the first parameter sets the stream states: an MP4
// names them once, in its sample entry.
func (t *tsTrack) adoptParameterSets(p parameterSets) {
	if len(p.sps) > 0 && len(t.config.SPS) == 0 {
		t.config.SPS = p.sps
	}
	if len(p.pps) > 0 && len(t.config.PPS) == 0 {
		t.config.PPS = p.pps
	}
	if len(p.vps) > 0 && len(t.config.VPS) == 0 {
		t.config.VPS = p.vps
	}
}

// finish turns the units kept aside into samples, the duration of each being
// the distance to the next one.
func (t *tsTrack) finish() {
	if len(t.pending) == 0 {
		return
	}
	for i, u := range t.pending {
		dur := int64(0)
		switch {
		case i+1 < len(t.pending):
			dur = t.pending[i+1].time - u.time
		case len(t.pending) > 1:
			dur = t.pending[i].time - t.pending[i-1].time
		}
		if dur <= 0 {
			dur = defaultUnitDuration(t.track.Kind, t.config.SampleRate)
		}
		t.samples = append(t.samples, Sample{
			Data:     u.data,
			Duration: uint32(dur),
			Sync:     t.track.Kind == Audio || isSyncUnit(u.data, t.hevc),
		})
		t.track.Duration += uint64(dur)
	}
	t.pending = nil
}

// defaultUnitDuration is what a single unit lasts when the stream gives no
// second timestamp to measure against.
func defaultUnitDuration(kind Kind, sampleRate int) int64 {
	if kind == Audio {
		return aacFrameDuration(sampleRate)
	}
	return TSTimescale / 25 // a frame at 25 fps
}

// aacFrameDuration is how long one AAC frame lasts, in transport clock units.
func aacFrameDuration(sampleRate int) int64 {
	if sampleRate <= 0 {
		return TSTimescale / 43 // a frame of 1024 samples at 44.1 kHz
	}
	return int64(1024 * TSTimescale / sampleRate)
}

// pesTime is the decode time of a unit, its presentation time when it states
// no other.
func pesTime(pes *astits.PESData) int64 {
	if pes.Header == nil || pes.Header.OptionalHeader == nil {
		return 0
	}
	if dts := pes.Header.OptionalHeader.DTS; dts != nil {
		return dts.Base
	}
	if pts := pes.Header.OptionalHeader.PTS; pts != nil {
		return pts.Base
	}
	return 0
}

// parameterSets are the NAL units that describe a stream rather than carry a
// picture.
type parameterSets struct {
	sps, pps, vps [][]byte
}

// convertAnnexB turns the start-code separated NAL units of a transport stream
// into the length-prefixed form an MP4 sample holds, and lifts out the
// parameter sets, which belong in the sample entry instead.
func convertAnnexB(data []byte, hevc bool) ([]byte, parameterSets) {
	var (
		out    bytes.Buffer
		params parameterSets
	)
	for _, nalu := range splitAnnexB(data) {
		if len(nalu) == 0 {
			continue
		}
		switch naluKind(nalu, hevc) {
		case naluSPS:
			params.sps = append(params.sps, nalu)
			continue
		case naluPPS:
			params.pps = append(params.pps, nalu)
			continue
		case naluVPS:
			params.vps = append(params.vps, nalu)
			continue
		case naluDrop:
			continue
		}
		writeLength(&out, len(nalu))
		out.Write(nalu)
	}
	return out.Bytes(), params
}

// splitAnnexB cuts a byte stream on its start codes.
func splitAnnexB(data []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(data); {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				out = append(out, trimTrailingZeros(data[start:i]))
			}
			i += 3
			start = i
			continue
		}
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 {
			i++
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		out = append(out, trimTrailingZeros(data[start:]))
	}
	return out
}

// trimTrailingZeros drops the padding a stream may put between units.
func trimTrailingZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// The kinds of NAL unit this file has to tell apart.
const (
	naluPicture = iota
	naluSPS
	naluPPS
	naluVPS
	naluDrop
)

// naluKind classifies a NAL unit. Which of the two codecs the stream carries
// has to be stated, because the header does not say: the two spell their type
// in different bits of it, and the meanings overlap. An AVC slice header of
// 0x41 reads as an HEVC video parameter set, and dropping a picture as if it
// described the stream loses the picture.
func naluKind(nalu []byte, hevc bool) int {
	if len(nalu) == 0 {
		return naluDrop
	}
	if hevc {
		// An HEVC unit has a two-byte header whose type sits in six bits.
		switch (nalu[0] >> 1) & 0x3f {
		case 32:
			return naluVPS
		case 33:
			return naluSPS
		case 34:
			return naluPPS
		case 35, 38:
			return naluDrop // an access unit delimiter or filler carries nothing
		}
		return naluPicture
	}
	// An AVC unit states its type in the low five bits of one byte.
	switch nalu[0] & 0x1f {
	case 7:
		return naluSPS
	case 8:
		return naluPPS
	case 9, 12:
		return naluDrop
	}
	return naluPicture
}

// splitLengthPrefixed cuts a sample into the NAL units its four-byte prefixes
// separate, and reports whether they described the sample exactly: a prefix
// reaching past the end, or bytes left over after the last unit, mean the data
// is not in the form this package writes. The units read before that are still
// returned, because deciding what a partly readable unit holds is worth more
// than refusing to look at it.
func splitLengthPrefixed(data []byte) ([][]byte, bool) {
	var nalus [][]byte
	i := 0
	for i+4 <= len(data) {
		size := int(uint32(data[i])<<24 | uint32(data[i+1])<<16 |
			uint32(data[i+2])<<8 | uint32(data[i+3]))
		if size <= 0 || i+4+size > len(data) {
			return nalus, false
		}
		nalus = append(nalus, data[i+4:i+4+size])
		i += 4 + size
	}
	return nalus, i == len(data)
}

// isSyncUnit reports whether an access unit can be decoded on its own. As with
// naluKind, only the stream's codec says which bits of the header to read.
func isSyncUnit(data []byte, hevc bool) bool {
	nalus, _ := splitLengthPrefixed(data)
	for _, nalu := range nalus {
		if hevc {
			if t := (nalu[0] >> 1) & 0x3f; t >= 16 && t <= 21 {
				return true // an HEVC picture starting a decodable segment
			}
			continue
		}
		if nalu[0]&0x1f == 5 {
			return true // an AVC picture coded without reference to another
		}
	}
	return false
}

// writeLength writes the four-byte prefix an MP4 sample separates its NAL
// units with.
func writeLength(w *bytes.Buffer, n int) {
	w.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// adtsConfig is what an ADTS header says about the stream carrying it.
type adtsConfig struct {
	SampleRate int
	Channels   int
	ObjectType byte
}

// splitADTS separates the frames of an ADTS stream from their headers: an MP4
// keeps the configuration in its sample entry and the frames raw.
func splitADTS(data []byte) ([][]byte, adtsConfig) {
	var (
		frames [][]byte
		cfg    adtsConfig
	)
	for offset := 0; offset < len(data); {
		// The offset a decode returns is what it skipped before the sync
		// word; the header's own length is what separates it from the frame.
		header, skipped, err := aac.DecodeADTSHeader(bytes.NewReader(data[offset:]))
		if err != nil || header.PayloadLength == 0 {
			break
		}
		start := offset + skipped + int(header.HeaderLength)
		end := start + int(header.PayloadLength)
		if end > len(data) {
			break
		}
		if cfg.SampleRate == 0 {
			cfg = adtsConfig{
				SampleRate: adtsFrequency(header.SamplingFrequencyIndex),
				Channels:   int(header.ChannelConfig),
				ObjectType: header.ObjectType,
			}
		}
		frames = append(frames, data[start:end])
		offset = end
	}
	return frames, cfg
}

// adtsFrequency turns the index an ADTS header carries into a sample rate.
func adtsFrequency(index byte) int {
	rates := []int{96000, 88200, 64000, 48000, 44100, 32000,
		24000, 22050, 16000, 12000, 11025, 8000, 7350}
	if int(index) >= len(rates) {
		return 0
	}
	return rates[index]
}

// demuxTS reads a transport stream's structure.
func demuxTS(data []byte) (*File, error) {
	tracks, err := readTS(data)
	if err != nil {
		return nil, err
	}
	f := &File{Format: "mpegts", Timescale: TSTimescale}
	for _, t := range tracks {
		f.Tracks = append(f.Tracks, t.track)
		if t.track.Duration > f.Duration {
			f.Duration = t.track.Duration
		}
	}
	return f, nil
}

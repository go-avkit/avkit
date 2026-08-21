// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/asticode/go-astits"
)

// The NAL units the fixtures below are built from. None of them ends in a zero
// byte, nor holds a start code, so what is read back must be byte for byte what
// was written.
var (
	tsIDR   = []byte{0x65, 0x88, 0x84, 0x21} // an AVC picture coded on its own
	tsSlice = []byte{0x41, 0x9a, 0x24, 0x6c} // one coded against another
)

// lengthPrefixed joins NAL units the way an MP4 sample holds them, which is the
// form both Reader and Muxer hand over.
func lengthPrefixed(nalus ...[]byte) []byte {
	var out bytes.Buffer
	for _, nalu := range nalus {
		writeLength(&out, len(nalu))
		out.Write(nalu)
	}
	return out.Bytes()
}

// tsVideoConfig describes an AVC track already counted in the transport
// clock, so a round trip compares durations without any rescaling in the way.
func tsVideoConfig(t *testing.T) TrackConfig {
	t.Helper()
	sps, pps, w, h := avcParameterSets(t)
	return TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: TSTimescale,
		Width: w, Height: h, SPS: sps, PPS: pps,
	}
}

// tsAudioConfig describes an AAC track in the transport clock as well.
func tsAudioConfig() TrackConfig {
	return TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: TSTimescale,
		Channels: 2, SampleRate: 48000,
	}
}

// tsUnitsOfStream reads back the packetised units of one stream, with the
// packet each began in, so what the muxer really wrote can be looked at rather
// than assumed.
func tsUnitsOfStream(t *testing.T, data []byte, pid uint16) []*astits.DemuxerData {
	t.Helper()
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(data))
	var out []*astits.DemuxerData
	for {
		d, err := dmx.NextData()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, astits.ErrNoMorePackets) {
				return out
			}
			t.Fatalf("reading back the stream: %v", err)
		}
		if d.PES != nil && d.PID == pid {
			out = append(out, d)
		}
	}
}

// tsProgramMap is the first program map the stream states.
func tsProgramMap(t *testing.T, data []byte) *astits.PMTData {
	t.Helper()
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(data))
	for {
		d, err := dmx.NextData()
		if err != nil {
			t.Fatalf("the stream states no program map: %v", err)
		}
		if d.PMT != nil {
			return d.PMT
		}
	}
}

// TestTSMuxRoundTripsThroughReader is what writing a transport stream is for:
// every sample handed to the muxer comes back out of the package's own reader,
// byte for byte, with its timing and its sync flag.
func TestTSMuxRoundTripsThroughReader(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	video, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	audio, err := m.AddTrack(tsAudioConfig())
	if err != nil {
		t.Fatalf("AddTrack(audio): %v", err)
	}
	// The identifiers are the ones a transport stream names its streams by.
	if video != tsFirstPID || audio != tsFirstPID+1 {
		t.Fatalf("packet identifiers = %d and %d", video, audio)
	}

	// A constant cadence on purpose: a reader works a unit's duration out
	// from the distance to the next one, so the last unit of a stream can only
	// be given the distance before it.
	const videoDuration, audioDuration = 3600, 1920
	var wroteVideo, wroteAudio []Sample
	for i := 0; i < 6; i++ {
		v := Sample{
			// Two NAL units, to prove more than one survives the crossing.
			Data:     lengthPrefixed(tsIDR, tsSlice),
			Duration: videoDuration,
			Sync:     i%3 == 0,
		}
		if !v.Sync {
			v.Data = lengthPrefixed(tsSlice)
		}
		a := Sample{
			Data:     []byte{0x21, byte(i), 0x23, 0x24},
			Duration: audioDuration,
			Sync:     true,
		}
		if err := m.WriteSample(video, v); err != nil {
			t.Fatalf("WriteSample(video, %d): %v", i, err)
		}
		if err := m.WriteSample(audio, a); err != nil {
			t.Fatalf("WriteSample(audio, %d): %v", i, err)
		}
		wroteVideo, wroteAudio = append(wroteVideo, v), append(wroteAudio, a)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// What was written must be a transport stream, and be seen as one.
	if got := Sniff(buf.Bytes()); got != FormatMPEGTS {
		t.Fatalf("Sniff = %d, want FormatMPEGTS", got)
	}
	if buf.Len()%tsPacketSize != 0 {
		t.Errorf("%d bytes written, which is not a whole number of packets", buf.Len())
	}
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Format != "mpegts" || file.Timescale != TSTimescale {
		t.Fatalf("file = %+v", file)
	}
	if v := file.VideoTracks(); len(v) != 1 || v[0].Codec != "avc1" || v[0].ID != video {
		t.Fatalf("video tracks = %+v", v)
	}
	if a := file.AudioTracks(); len(a) != 1 || a[0].Codec != "mp4a" ||
		a[0].SampleRate != 48000 || a[0].Channels != 2 {
		t.Fatalf("audio tracks = %+v", file.AudioTracks())
	}

	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// The precondition every comparison below rests on: this was read as a
	// transport stream, not by some other path.
	if r.ts == nil {
		t.Fatal("the output was not read as a transport stream")
	}
	// The parameter sets were written in band and are found again there.
	cfg, err := r.TrackConfig(video)
	if err != nil {
		t.Fatalf("TrackConfig(video): %v", err)
	}
	want := tsVideoConfig(t)
	if len(cfg.SPS) != 1 || !bytes.Equal(cfg.SPS[0], want.SPS[0]) {
		t.Errorf("SPS = %x, want %x", cfg.SPS, want.SPS)
	}
	if len(cfg.PPS) != 1 || !bytes.Equal(cfg.PPS[0], want.PPS[0]) {
		t.Errorf("PPS = %x, want %x", cfg.PPS, want.PPS)
	}
	audioCfg, err := r.TrackConfig(audio)
	if err != nil {
		t.Fatalf("TrackConfig(audio): %v", err)
	}
	if audioCfg.SampleRate != 48000 || audioCfg.Channels != 2 ||
		audioCfg.AudioObjectType != aac.AAClc {
		t.Errorf("audio config = %+v", audioCfg)
	}

	for _, track := range []struct {
		name  string
		id    uint32
		wrote []Sample
	}{{"video", video, wroteVideo}, {"audio", audio, wroteAudio}} {
		read, err := r.Samples(track.id)
		if err != nil {
			t.Fatalf("Samples(%s): %v", track.name, err)
		}
		if len(read) != len(track.wrote) {
			t.Fatalf("%s: read %d samples, wrote %d", track.name, len(read), len(track.wrote))
		}
		for i, got := range read {
			w := track.wrote[i]
			if !bytes.Equal(got.Data, w.Data) {
				t.Errorf("%s sample %d = %x, want %x", track.name, i, got.Data, w.Data)
			}
			if got.Duration != w.Duration {
				t.Errorf("%s sample %d lasts %d, want %d",
					track.name, i, got.Duration, w.Duration)
			}
			if got.Sync != w.Sync {
				t.Errorf("%s sample %d sync = %v, want %v", track.name, i, got.Sync, w.Sync)
			}
		}
	}
}

// TestTSMuxRepeatsParameterSetsAtEverySyncSample is what makes a transport
// stream joinable part-way through: a player starting at any sync sample finds
// what it needs to decode it in the stream itself.
func TestTSMuxRepeatsParameterSetsAtEverySyncSample(t *testing.T) {
	cfg := tsVideoConfig(t)
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sync := []bool{true, false, true, false}
	for _, isSync := range sync {
		nalu := tsSlice
		if isSync {
			nalu = tsIDR
		}
		if err := m.WriteSample(id, Sample{
			Data: lengthPrefixed(nalu), Duration: 3600, Sync: isSync,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	units := tsUnitsOfStream(t, buf.Bytes(), uint16(id))
	if len(units) != len(sync) {
		t.Fatalf("the stream carries %d units, want %d", len(units), len(sync))
	}
	var lastPCR int64 = -1
	for i, unit := range units {
		payload := unit.PES.Data
		// Each unit is start-code separated, not length-prefixed.
		if !bytes.HasPrefix(payload, []byte{0, 0, 0, 1}) {
			t.Fatalf("unit %d does not start on a start code: %x", i, payload)
		}
		carries := bytes.Contains(payload, cfg.SPS[0]) && bytes.Contains(payload, cfg.PPS[0])
		if carries != sync[i] {
			t.Errorf("unit %d carries its parameter sets = %v, want %v", i, carries, sync[i])
		}
		if sync[i] {
			// And in front of the picture, where a decoder reads them before
			// the data they describe.
			if got := bytes.Index(payload, cfg.SPS[0]); got > bytes.Index(payload, tsIDR) {
				t.Errorf("unit %d states its parameter sets after the picture", i)
			}
		}
		// Every unit states when it is shown; astits refuses one that does not.
		if unit.PES.Header.OptionalHeader.PTS == nil {
			t.Errorf("unit %d states no presentation time", i)
		}
		// This stream carries the clock, so every unit of it advances that
		// clock and says whether a decoder may start here.
		af := unit.FirstPacket.AdaptationField
		if af == nil || !af.HasPCR {
			t.Fatalf("unit %d carries no clock reference", i)
		}
		if af.PCR.Base <= lastPCR {
			t.Errorf("unit %d puts the clock at %d, after %d", i, af.PCR.Base, lastPCR)
		}
		lastPCR = af.PCR.Base
		if af.RandomAccessIndicator != sync[i] {
			t.Errorf("unit %d says a decoder may start here = %v, want %v",
				i, af.RandomAccessIndicator, sync[i])
		}
	}
	// The tables are repeated, not stated once: the stream describes itself
	// wherever a player joins it.
	if got := bytes.Count(buf.Bytes(), []byte{0x47, 0x50, 0x00}); got < 2 {
		t.Errorf("the program map appears %d times, want it repeated", got)
	}
}

// TestTSMuxRemuxesAFragmentedMP4 is the direction this muxer was missing: the
// samples of an MP4, in the MP4's own timescale, become a transport stream
// counted in the transport clock.
func TestTSMuxRemuxesAFragmentedMP4(t *testing.T) {
	// An MP4 first, whose timescale is not the transport one.
	const mp4Timescale, mp4Duration = 12800, 512 // 40 ms a frame
	cfg := tsVideoConfig(t)
	cfg.Timescale = mp4Timescale
	var mp4Bytes bytes.Buffer
	src := NewMuxer(&mp4Bytes)
	srcID, err := src.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := src.WriteSample(srcID, Sample{
			Data: lengthPrefixed(tsIDR), Duration: mp4Duration, Sync: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(mp4Bytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The precondition of the crossing: this really is a fragmented MP4, read
	// through its fragments rather than a sample table.
	if !r.mp4.IsFragmented() {
		t.Fatal("the source was not read as a fragmented MP4")
	}
	inCfg, err := r.TrackConfig(srcID)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(srcID)
	if err != nil {
		t.Fatal(err)
	}

	var tsBytes bytes.Buffer
	m := NewTSMuxer(&tsBytes)
	outID, err := m.AddTrack(inCfg)
	if err != nil {
		t.Fatalf("AddTrack from the MP4's own configuration: %v", err)
	}
	for _, s := range samples {
		if err := m.WriteSample(outID, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	// 512 units of a 12800 clock is 3600 of a 90 kHz one.
	units := tsUnitsOfStream(t, tsBytes.Bytes(), uint16(outID))
	if len(units) != len(samples) {
		t.Fatalf("the stream carries %d units, want %d", len(units), len(samples))
	}
	for i, unit := range units {
		want := int64(i) * mp4Duration * TSTimescale / mp4Timescale
		if got := unit.PES.Header.OptionalHeader.PTS.Base; got != want {
			t.Errorf("unit %d is shown at %d, want %d", i, got, want)
		}
	}
	back, err := NewReader(tsBytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	read, err := back.Samples(outID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != len(samples) {
		t.Fatalf("read %d samples back, want %d", len(read), len(samples))
	}
	// Rescaled, so stated in the transport clock rather than the MP4's.
	if read[0].Duration != mp4Duration*TSTimescale/mp4Timescale {
		t.Errorf("duration = %d, want it rescaled to the transport clock", read[0].Duration)
	}
	if !bytes.Equal(read[0].Data, samples[0].Data) {
		t.Errorf("sample = %x, want %x", read[0].Data, samples[0].Data)
	}
}

// TestTSMuxWritesHEVC covers the codec whose parameter sets are three rather
// than two, and whose NAL header is not the same shape.
func TestTSMuxWritesHEVC(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0c}
	sps := []byte{0x42, 0x01, 0x01}
	pps := []byte{0x44, 0x01, 0xc0}
	picture := []byte{0x26, 0x01, 0xaf} // an IRAP picture
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "hvc1", Timescale: TSTimescale,
		VPS: [][]byte{vps}, SPS: [][]byte{sps}, PPS: [][]byte{pps},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.WriteSample(id, Sample{
			Data: lengthPrefixed(picture), Duration: 3000, Sync: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The video set comes first, then the sequence set, then the picture set:
	// each refers to the one before it.
	unit := tsUnitsOfStream(t, buf.Bytes(), uint16(id))[0].PES
	if got := unit.Data; bytes.Index(got, vps) > bytes.Index(got, sps) ||
		bytes.Index(got, sps) > bytes.Index(got, pps) {
		t.Errorf("the parameter sets are out of order: %x", got)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codec != "hvc1" || len(cfg.VPS) != 1 || len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("config = %+v", cfg)
	}
	samples, err := r.Samples(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 || !samples[0].Sync {
		t.Fatalf("samples = %+v", samples)
	}
	if !bytes.Equal(samples[0].Data, lengthPrefixed(picture)) {
		t.Errorf("sample = %x", samples[0].Data)
	}
}

// TestTSMuxStatesADecodeTimeOnlyWhenItDiffers checks the timing of a stream
// that reorders its frames: presentation is shifted against decoding, and both
// times are then stated.
func TestTSMuxStatesADecodeTimeOnlyWhenItDiffers(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	id, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	offsets := []int32{0, 3600}
	for _, offset := range offsets {
		if err := m.WriteSample(id, Sample{
			Data: lengthPrefixed(tsIDR), Duration: 3600,
			CompositionOffset: offset, Sync: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	units := tsUnitsOfStream(t, buf.Bytes(), uint16(id))
	if len(units) != 2 {
		t.Fatalf("the stream carries %d units, want 2", len(units))
	}
	if units[0].PES.Header.OptionalHeader.DTS != nil {
		t.Error("a frame shown when it is decoded needs no decode time of its own")
	}
	dts := units[1].PES.Header.OptionalHeader.DTS
	if dts == nil {
		t.Fatal("a reordered frame must state when it is decoded")
	}
	if got := units[1].PES.Header.OptionalHeader.PTS.Base - dts.Base; got != 3600 {
		t.Errorf("presentation is shifted by %d, want 3600", got)
	}

	// A frame shown before the stream starts cannot be stated at all.
	m2 := NewTSMuxer(io.Discard)
	id2, err := m2.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	err = m2.WriteSample(id2, Sample{
		Data: lengthPrefixed(tsIDR), Duration: 3600, CompositionOffset: -1, Sync: true,
	})
	if !errors.Is(err, ErrSample) {
		t.Errorf("a negative presentation time: %v", err)
	}
}

// TestTSMuxPutsTheClockOnTheVideoStream checks which stream a player recovers
// its timing from, whichever order the tracks were declared in.
func TestTSMuxPutsTheClockOnTheVideoStream(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	// Audio first, so the choice cannot be the first track by accident.
	audio, err := m.AddTrack(tsAudioConfig())
	if err != nil {
		t.Fatal(err)
	}
	video, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{
		Data: lengthPrefixed(tsIDR), Duration: 3600, Sync: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(audio, Sample{
		Data: []byte{1, 2, 3}, Duration: 1920, Sync: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tsProgramMap(t, buf.Bytes()).PCRPID; got != uint16(video) {
		t.Errorf("the clock is carried on stream %d, want the video one %d", got, video)
	}

	// With no video stream it goes on the first one, because a stream with no
	// clock at all is one no player can pace.
	var audioOnly bytes.Buffer
	m2 := NewTSMuxer(&audioOnly)
	only, err := m2.AddTrack(tsAudioConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.WriteSample(only, Sample{
		Data: []byte{1, 2, 3}, Duration: 1920, Sync: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tsProgramMap(t, audioOnly.Bytes()).PCRPID; got != uint16(only) {
		t.Errorf("the clock is carried on stream %d, want %d", got, only)
	}
}

// TestTSMuxADTSHeaders checks the header a transport stream states an AAC
// frame's format in, which an MP4 keeps in its sample entry instead.
func TestTSMuxADTSHeaders(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	id, err := m.AddTrack(tsAudioConfig())
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0x21, 0x22, 0x23, 0x24, 0x25}
	if err := m.WriteSample(id, Sample{Data: frame, Duration: 1920, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	unit := tsUnitsOfStream(t, buf.Bytes(), uint16(id))[0].PES
	header, skipped, err := aac.DecodeADTSHeader(bytes.NewReader(unit.Data))
	if err != nil {
		t.Fatalf("the unit does not start with an ADTS header: %v", err)
	}
	if skipped != 0 {
		t.Errorf("the header sits %d bytes into the unit, want it first", skipped)
	}
	// The length field counts the header as well, so what it announces is the
	// frame alone once decoded.
	if int(header.PayloadLength) != len(frame) {
		t.Errorf("the header announces %d bytes, want %d", header.PayloadLength, len(frame))
	}
	if header.ChannelConfig != 2 || header.ObjectType != aac.AAClc ||
		adtsFrequency(header.SamplingFrequencyIndex) != 48000 {
		t.Errorf("header = %+v", header)
	}

	// A frame longer than the length field can count is one no ADTS stream can
	// state.
	m2 := NewTSMuxer(io.Discard)
	id2, err := m2.AddTrack(tsAudioConfig())
	if err != nil {
		t.Fatal(err)
	}
	err = m2.WriteSample(id2, Sample{
		Data: make([]byte, tsADTSPayloadMax+1), Duration: 1920, Sync: true,
	})
	if !errors.Is(err, ErrSample) {
		t.Errorf("an oversized frame: %v", err)
	}
}

func TestTSMuxAddTrackErrors(t *testing.T) {
	video := tsVideoConfig(t)
	cases := []struct {
		name string
		cfg  TrackConfig
		want error
	}{
		{"no timescale", TrackConfig{Kind: Video, Codec: "avc1", SPS: video.SPS, PPS: video.PPS}, ErrTrackConfig},
		{"unknown codec", TrackConfig{Kind: Video, Codec: "vp09", Timescale: TSTimescale}, ErrUnsupportedCodec},
		{"avc without pps", TrackConfig{Kind: Video, Codec: "avc1", Timescale: TSTimescale, SPS: video.SPS}, ErrTrackConfig},
		{"hevc without vps", TrackConfig{Kind: Video, Codec: "hvc1", Timescale: TSTimescale, SPS: video.SPS, PPS: video.PPS}, ErrTrackConfig},
		{"aac without channels", TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: TSTimescale, SampleRate: 48000}, ErrTrackConfig},
		{"aac with too many channels", TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: TSTimescale, SampleRate: 48000, Channels: tsMaxChannelConfig + 1}, ErrTrackConfig},
		{"aac at a rate no header can state", TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: TSTimescale, SampleRate: 44000, Channels: 2}, ErrTrackConfig},
		{"aac in a profile no header can state", TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: TSTimescale, SampleRate: 48000, Channels: 2, AudioObjectType: aac.HEAACv1}, ErrTrackConfig},
	}
	for _, c := range cases {
		m := NewTSMuxer(io.Discard)
		if _, err := m.AddTrack(c.cfg); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}

	// A track cannot be declared once the program map describing them all has
	// been written.
	m := NewTSMuxer(io.Discard)
	id, err := m.AddTrack(video)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{
		Data: lengthPrefixed(tsIDR), Duration: 3600, Sync: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(tsAudioConfig()); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("adding a track once writing has begun: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(tsAudioConfig()); !errors.Is(err, ErrClosed) {
		t.Errorf("adding a track after Close: %v", err)
	}
}

func TestTSMuxWriteSampleErrors(t *testing.T) {
	// Nothing declared yet.
	empty := NewTSMuxer(io.Discard)
	if err := empty.WriteSample(tsFirstPID, Sample{Data: []byte{1}, Duration: 1}); !errors.Is(err, ErrNoTracks) {
		t.Errorf("writing to a muxer with no track: %v", err)
	}

	m := NewTSMuxer(io.Discard)
	id, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		id     uint32
		sample Sample
		want   error
	}{
		{"no data", id, Sample{Duration: 3600}, ErrSample},
		{"no duration", id, Sample{Data: lengthPrefixed(tsIDR)}, ErrSample},
		{"unknown track", 999, Sample{Data: lengthPrefixed(tsIDR), Duration: 3600}, ErrUnknownTrack},
		{"a length reaching past the end", id, Sample{Data: []byte{0, 0, 0, 9, 1}, Duration: 3600}, ErrSample},
		{"bytes left over after the last unit", id, Sample{Data: append(lengthPrefixed(tsIDR), 0xff), Duration: 3600}, ErrSample},
		{"a unit of nothing", id, Sample{Data: []byte{0, 0, 0, 0}, Duration: 3600}, ErrSample},
	}
	for _, c := range cases {
		if err := m.WriteSample(c.id, c.sample); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: lengthPrefixed(tsIDR), Duration: 3600}); !errors.Is(err, ErrClosed) {
		t.Errorf("writing after Close: %v", err)
	}
}

func TestTSMuxCloseStatesTheProgramOfAnEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	video, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A stream naming its tracks and carrying nothing is still a stream: what
	// it declares reads back, and asking for its samples says there are none.
	if got := Sniff(buf.Bytes()); got != FormatMPEGTS {
		t.Fatalf("Sniff = %d, want FormatMPEGTS", got)
	}
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if len(file.Tracks) != 1 || file.Tracks[0].ID != video {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(video); !errors.Is(err, ErrNoSamples) {
		t.Errorf("the samples of an empty stream: %v", err)
	}
	if err := m.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("Close twice: %v", err)
	}
}

func TestTSMuxCloseWithoutTracks(t *testing.T) {
	m := NewTSMuxer(io.Discard)
	if err := m.Close(); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Close with nothing declared: %v", err)
	}
}

func TestTSMuxWriteFailures(t *testing.T) {
	// The tables in front of the first unit cannot be written.
	m := NewTSMuxer(&failWriter{})
	id, err := m.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{
		Data: lengthPrefixed(tsIDR), Duration: 3600, Sync: true,
	}); err == nil {
		t.Fatal("a failing writer was ignored")
	}

	// And on Close, where a stream that carried nothing still states what it
	// would have carried.
	m2 := NewTSMuxer(&failWriter{})
	if _, err := m2.AddTrack(tsVideoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m2.Close(); err == nil {
		t.Fatal("a failing table write was ignored on Close")
	}
}

func TestTSMuxReportsAStreamItCannotDeclare(t *testing.T) {
	original := addElementaryStream
	defer func() { addElementaryStream = original }()
	addElementaryStream = func(*astits.Muxer, astits.PMTElementaryStream) error {
		return astits.ErrPIDAlreadyExists
	}
	m := NewTSMuxer(io.Discard)
	if _, err := m.AddTrack(tsVideoConfig(t)); !errors.Is(err, astits.ErrPIDAlreadyExists) {
		t.Fatalf("err = %v, want the collision astits reports", err)
	}
}

func TestSplitLengthPrefixed(t *testing.T) {
	nalus, ok := splitLengthPrefixed(lengthPrefixed(tsIDR, tsSlice))
	if !ok || len(nalus) != 2 ||
		!bytes.Equal(nalus[0], tsIDR) || !bytes.Equal(nalus[1], tsSlice) {
		t.Fatalf("nalus = %x, ok = %v", nalus, ok)
	}
	if _, ok := splitLengthPrefixed(nil); !ok {
		t.Error("nothing at all is prefixed correctly, there being nothing to prefix")
	}
	// A prefix reaching past the end, a unit of nothing, and bytes left over
	// after the last unit are each data this package did not write.
	for _, data := range [][]byte{
		{0, 0, 0, 9, 1},
		{0, 0, 0, 0},
		append(lengthPrefixed(tsIDR), 0xff),
		lengthPrefixed(tsIDR)[:5],
	} {
		if _, ok := splitLengthPrefixed(data); ok {
			t.Errorf("%x was taken for length-prefixed units", data)
		}
	}
}

func TestTSClockCountsAround(t *testing.T) {
	// The timestamp fields of a transport stream are 33 bits wide, so a stream
	// long enough counts around rather than writing something that would be
	// silently truncated.
	track := &tsMuxTrack{timescale: TSTimescale}
	if got := track.rescale(tsClockWrap + 7); got != 7 {
		t.Errorf("rescale = %d, want 7", got)
	}
	// And a track counted in its own clock is converted to the transport one.
	half := &tsMuxTrack{timescale: TSTimescale / 2}
	if got := half.rescale(1000); got != 2000 {
		t.Errorf("rescale = %d, want 2000", got)
	}
}

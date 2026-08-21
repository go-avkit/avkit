// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/asticode/go-astits"
)

// annexB joins NAL units the way a transport stream carries them: each one
// preceded by a start code.
func annexB(nalus ...[]byte) []byte {
	var out bytes.Buffer
	for _, nalu := range nalus {
		out.Write([]byte{0, 0, 0, 1})
		out.Write(nalu)
	}
	return out.Bytes()
}

// adtsFrame wraps a payload in the header an ADTS stream puts before it.
func adtsFrame(t *testing.T, payload []byte) []byte {
	t.Helper()
	header, err := aac.NewADTSHeader(48000, 2, aac.AAClc, uint16(len(payload)))
	if err != nil {
		t.Fatalf("ADTS header: %v", err)
	}
	return append(header.Encode(), payload...)
}

// tsFixture builds a transport stream carrying one AVC and one AAC stream, as
// an HLS segment does.
func tsFixture(t *testing.T, units int) []byte {
	t.Helper()
	sps, pps, _, _ := avcParameterSets(t)
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: astits.StreamTypeH264Video,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 257, StreamType: astits.StreamTypeAACAudio,
	}); err != nil {
		t.Fatal(err)
	}
	// A stream type this package cannot hand over must simply be ignored.
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 258, StreamType: astits.StreamTypePrivateData,
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)

	idr := []byte{0x65, 0x88, 0x84, 0x21}
	delimiter := []byte{0x09, 0x10} // an access unit delimiter, which carries nothing
	for i := 0; i < units; i++ {
		payload := annexB(delimiter, sps[0], pps[0], idr)
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: int64(i) * 3600},
			}},
			Data: payload,
		}}); err != nil {
			t.Fatalf("write video: %v", err)
		}
		if _, err := m.WriteData(&astits.MuxerData{PID: 258, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xBD, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: int64(i) * 3600},
			}},
			Data: []byte{0xaa, 0xbb},
		}}); err != nil {
			t.Fatalf("write private data: %v", err)
		}
		if _, err := m.WriteData(&astits.MuxerData{PID: 257, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xC0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: int64(i) * 1920},
			}},
			Data: adtsFrame(t, []byte{0x21, 0x22, 0x23, 0x24}),
		}}); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	return buf.Bytes()
}

func TestSniffRecognisesATransportStream(t *testing.T) {
	if got := Sniff(tsFixture(t, 2)); got != FormatMPEGTS {
		t.Fatalf("Sniff = %d, want FormatMPEGTS", got)
	}
	if got := describeFormat(FormatMPEGTS); got != "mpegts" {
		t.Errorf("describeFormat = %q", got)
	}
	// A lone sync byte, or one not repeating a packet apart, is not a stream.
	if sniffTS([]byte{0x47}) {
		t.Error("one byte was taken for a transport stream")
	}
	short := make([]byte, 2*tsPacketSize)
	short[0] = 0x47
	if sniffTS(short) {
		t.Error("a stream whose second packet has no sync byte was accepted")
	}
}

func TestDemuxTransportStream(t *testing.T) {
	file, err := Demux(tsFixture(t, 3))
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if file.Format != "mpegts" || file.Timescale != TSTimescale {
		t.Fatalf("file = %+v", file)
	}
	// The private stream is not something this package can hand over.
	if len(file.Tracks) != 2 {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	video, audio := file.VideoTracks(), file.AudioTracks()
	if len(video) != 1 || video[0].Codec != "avc1" || video[0].ID != 256 {
		t.Fatalf("video = %+v", video)
	}
	if len(audio) != 1 || audio[0].Codec != "mp4a" || audio[0].SampleRate != 48000 {
		t.Fatalf("audio = %+v", audio)
	}
	if file.DurationSeconds() <= 0 {
		t.Errorf("duration = %v", file.DurationSeconds())
	}
}

func TestReadTransportStreamSamples(t *testing.T) {
	r, err := NewReader(tsFixture(t, 3))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg, err := r.TrackConfig(256)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	// The parameter sets belong in the sample entry, not in the samples.
	if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 || cfg.Timescale != TSTimescale {
		t.Fatalf("video config = %+v", cfg)
	}
	samples, err := r.Samples(256)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("read %d access units, want 3", len(samples))
	}
	for i, s := range samples {
		if s.Duration == 0 {
			t.Errorf("sample %d has no duration", i)
		}
		if !s.Sync {
			t.Errorf("sample %d should be a sync sample", i)
		}
		// Length-prefixed, and holding neither the parameter sets nor the
		// access unit delimiter.
		if len(s.Data) < 5 || !bytes.Equal(s.Data[:4], []byte{0, 0, 0, 4}) {
			t.Fatalf("sample %d is not length-prefixed: %x", i, s.Data)
		}
		if bytes.Contains(s.Data, cfg.SPS[0]) {
			t.Errorf("sample %d still carries its parameter sets", i)
		}
	}
	// The distance between two units is what a duration is.
	if samples[0].Duration != 3600 {
		t.Errorf("duration = %d, want the 3600 between the two timestamps", samples[0].Duration)
	}

	audioCfg, err := r.TrackConfig(257)
	if err != nil {
		t.Fatalf("TrackConfig(audio): %v", err)
	}
	if audioCfg.SampleRate != 48000 || audioCfg.Channels != 2 || audioCfg.AudioObjectType != aac.AAClc {
		t.Fatalf("audio config = %+v", audioCfg)
	}
	audio, err := r.Samples(257)
	if err != nil {
		t.Fatalf("Samples(audio): %v", err)
	}
	if len(audio) != 3 {
		t.Fatalf("read %d audio frames, want 3", len(audio))
	}
	// The header is the container's business, not the sample's.
	if !bytes.Equal(audio[0].Data, []byte{0x21, 0x22, 0x23, 0x24}) {
		t.Fatalf("audio sample = %x, want the frame without its ADTS header", audio[0].Data)
	}
}

// TestRemuxTransportStreamIntoMP4 is what reading a transport stream is for:
// an HLS segment becomes an MP4 without anything being re-encoded.
func TestRemuxTransportStreamIntoMP4(t *testing.T) {
	src, err := NewReader(tsFixture(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m := NewMuxer(&out)
	// Every track is declared before the first sample: the initialisation
	// segment names them all.
	type copied struct {
		id      uint32
		samples []Sample
	}
	var tracks []copied
	for _, id := range src.TrackIDs() {
		cfg, err := src.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		samples, err := src.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		outID, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatalf("AddTrack(%d): %v", id, err)
		}
		tracks = append(tracks, copied{outID, samples})
	}
	for _, tr := range tracks {
		for _, s := range tr.samples {
			if err := m.WriteSample(tr.id, s); err != nil {
				t.Fatalf("WriteSample: %v", err)
			}
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	file, err := Demux(out.Bytes())
	if err != nil {
		t.Fatalf("the remuxed file does not read back: %v", err)
	}
	if len(file.VideoTracks()) != 1 || len(file.AudioTracks()) != 1 {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range back.TrackIDs() {
		samples, err := back.Samples(id)
		if err != nil {
			t.Fatalf("track %d: %v", id, err)
		}
		if len(samples) != 4 {
			t.Errorf("track %d holds %d samples, want 4", id, len(samples))
		}
	}
}

func TestReadTransportStreamErrors(t *testing.T) {
	// A stream that never says what it carries.
	empty := make([]byte, 4*tsPacketSize)
	for i := 0; i < len(empty); i += tsPacketSize {
		empty[i] = 0x47
		empty[i+1] = 0x1f // a null packet
		empty[i+2] = 0xff
		empty[i+3] = 0x10
	}
	if _, err := readTS(empty); !errors.Is(err, ErrNoProgram) {
		t.Errorf("without a program: %v", err)
	}
	if _, err := NewReader(empty); err == nil {
		t.Error("a stream declaring nothing was accepted")
	}
	r, err := NewReader(tsFixture(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(999); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("Samples of an unknown track: %v", err)
	}
	if _, err := r.TrackConfig(999); !errors.Is(err, ErrUnknownTrack) {
		t.Errorf("TrackConfig of an unknown track: %v", err)
	}
}

func TestAnnexBConversion(t *testing.T) {
	sps := []byte{0x67, 0x42}
	pps := []byte{0x68, 0xce}
	picture := []byte{0x65, 0x88}
	payload, params := convertAnnexB(annexB([]byte{0x09, 0x10}, sps, pps, picture))
	if len(params.sps) != 1 || len(params.pps) != 1 {
		t.Fatalf("parameter sets = %+v", params)
	}
	want := append([]byte{0, 0, 0, 2}, picture...)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %x, want %x", payload, want)
	}
	// A three-byte start code is as valid as a four-byte one, and trailing
	// padding belongs to neither unit.
	three := append([]byte{0, 0, 1}, picture...)
	three = append(three, 0, 0)
	payload, _ = convertAnnexB(three)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %x, want %x", payload, want)
	}
	if got, _ := convertAnnexB(nil); len(got) != 0 {
		t.Errorf("an empty stream yielded %x", got)
	}
}

func TestNALUClassification(t *testing.T) {
	cases := []struct {
		nalu []byte
		want int
	}{
		{[]byte{0x67, 0x00}, naluSPS},
		{[]byte{0x68, 0x00}, naluPPS},
		{[]byte{0x09, 0x10}, naluDrop},
		{[]byte{0x65, 0x00}, naluPicture},
		{[]byte{0x40, 0x01}, naluVPS}, // HEVC video parameter set
		{[]byte{0x42, 0x01}, naluSPS}, // HEVC sequence parameter set
		{[]byte{0x44, 0x01}, naluPPS}, // HEVC picture parameter set
		{nil, naluDrop},
	}
	for _, c := range cases {
		if got := naluKind(c.nalu); got != c.want {
			t.Errorf("naluKind(%x) = %d, want %d", c.nalu, got, c.want)
		}
	}
}

func TestSyncUnitDetection(t *testing.T) {
	avcIDR := append([]byte{0, 0, 0, 2}, 0x65, 0x88)
	if !isSyncUnit(avcIDR) {
		t.Error("an AVC picture coded on its own is a sync sample")
	}
	avcSlice := append([]byte{0, 0, 0, 2}, 0x41, 0x88)
	if isSyncUnit(avcSlice) {
		t.Error("a picture referring to another is not")
	}
	hevcIRAP := append([]byte{0, 0, 0, 2}, 0x26, 0x01)
	if !isSyncUnit(hevcIRAP) {
		t.Error("an HEVC picture starting a decodable segment is a sync sample")
	}
	if isSyncUnit([]byte{0, 0, 0, 99, 1}) {
		t.Error("a length past the end must not be trusted")
	}
	if isSyncUnit(nil) {
		t.Error("nothing is not a sync sample")
	}
}

func TestADTSSplitting(t *testing.T) {
	first := adtsFrame(t, []byte{1, 2, 3})
	second := adtsFrame(t, []byte{4, 5})
	frames, cfg := splitADTS(append(first, second...))
	if len(frames) != 2 {
		t.Fatalf("frames = %d", len(frames))
	}
	if !bytes.Equal(frames[0], []byte{1, 2, 3}) || !bytes.Equal(frames[1], []byte{4, 5}) {
		t.Fatalf("frames = %x", frames)
	}
	if cfg.SampleRate != 48000 || cfg.Channels != 2 {
		t.Fatalf("config = %+v", cfg)
	}
	if frames, _ := splitADTS([]byte{0xff}); frames != nil {
		t.Errorf("a truncated header yielded %x", frames)
	}
	// A header promising more than what follows must not be read.
	truncated := adtsFrame(t, []byte{1, 2, 3, 4})[:8]
	if frames, _ := splitADTS(truncated); frames != nil {
		t.Errorf("a truncated frame yielded %x", frames)
	}
}

func TestTimingHelpers(t *testing.T) {
	if got := adtsFrequency(3); got != 48000 {
		t.Errorf("adtsFrequency(3) = %d", got)
	}
	if got := adtsFrequency(99); got != 0 {
		t.Errorf("adtsFrequency(99) = %d", got)
	}
	if got := aacFrameDuration(48000); got != 1920 {
		t.Errorf("aacFrameDuration = %d", got)
	}
	if got := aacFrameDuration(0); got == 0 {
		t.Error("an unknown sample rate must still yield a duration")
	}
	if got := defaultUnitDuration(Video, 0); got != TSTimescale/25 {
		t.Errorf("defaultUnitDuration(video) = %d", got)
	}
	if got := defaultUnitDuration(Audio, 48000); got != 1920 {
		t.Errorf("defaultUnitDuration(audio) = %d", got)
	}
	if got := pesTime(&astits.PESData{}); got != 0 {
		t.Errorf("pesTime without a header = %d", got)
	}
	both := &astits.PESData{Header: &astits.PESHeader{OptionalHeader: &astits.PESOptionalHeader{
		DTS: &astits.ClockReference{Base: 100}, PTS: &astits.ClockReference{Base: 200},
	}}}
	if got := pesTime(both); got != 100 {
		t.Errorf("pesTime = %d, want the decode time", got)
	}
	onlyPTS := &astits.PESData{Header: &astits.PESHeader{OptionalHeader: &astits.PESOptionalHeader{
		PTS: &astits.ClockReference{Base: 200},
	}}}
	if got := pesTime(onlyPTS); got != 200 {
		t.Errorf("pesTime = %d, want the presentation time", got)
	}
}

// hevcTSFixture carries an HEVC stream, whose parameter sets are three rather
// than two and whose NAL header is not the same shape.
func hevcTSFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 300, StreamType: astits.StreamTypeH265Video,
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(300)
	vps := []byte{0x40, 0x01, 0x0c}
	sps := []byte{0x42, 0x01, 0x01}
	pps := []byte{0x44, 0x01, 0xc0}
	picture := []byte{0x26, 0x01, 0xaf} // an IRAP picture
	for i := 0; i < 2; i++ {
		if _, err := m.WriteData(&astits.MuxerData{PID: 300, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorBothPresent,
				PTS:             &astits.ClockReference{Base: int64(i)*3000 + 100},
				DTS:             &astits.ClockReference{Base: int64(i) * 3000},
			}},
			Data: annexB(vps, sps, pps, picture),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestReadHEVCTransportStream(t *testing.T) {
	r, err := NewReader(hevcTSFixture(t))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg, err := r.TrackConfig(300)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if cfg.Codec != "hvc1" {
		t.Fatalf("codec = %q", cfg.Codec)
	}
	if len(cfg.VPS) != 1 || len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("parameter sets = %+v", cfg)
	}
	samples, err := r.Samples(300)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 2 || !samples[0].Sync {
		t.Fatalf("samples = %+v", samples)
	}
	// The decode time, not the presentation time, spaces the samples.
	if samples[0].Duration != 3000 {
		t.Errorf("duration = %d, want 3000", samples[0].Duration)
	}
}

func TestReadTransportStreamOfASingleUnit(t *testing.T) {
	// With one unit there is no second timestamp to measure against, so the
	// duration falls back to a frame's worth of time.
	r, err := NewReader(tsFixture(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(256)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Duration != TSTimescale/25 {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestTracksWithoutSamplesAreReported(t *testing.T) {
	// A stream that declares a track and then carries nothing for it.
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: astits.StreamTypeH264Video,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 257, StreamType: astits.StreamTypeAACAudio,
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)
	if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
		Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
			MarkerBits:      2,
			PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
			PTS:             &astits.ClockReference{Base: 900},
		}},
		Data: annexB([]byte{0x65, 0x88}),
	}}); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(257); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("the silent track: %v", err)
	}
	// The one that does carry something still reads, its lone unit lasting a
	// frame for want of a second timestamp.
	samples, err := r.Samples(256)
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples = %+v, %v", samples, err)
	}
}

func TestDemuxTransportStreamTakesTheLongestTrack(t *testing.T) {
	file, err := demuxTS(tsFixture(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	var longest uint64
	for _, tr := range file.Tracks {
		if tr.Duration > longest {
			longest = tr.Duration
		}
	}
	if file.Duration != longest {
		t.Fatalf("file duration = %d, want the longest track's %d", file.Duration, longest)
	}
}

func TestReadTSPropagatesAReadFailure(t *testing.T) {
	// Packets whose sync bytes are right but whose content is not, which the
	// packet reader refuses rather than skips.
	data := make([]byte, 4*tsPacketSize)
	for i := 0; i < len(data); i += tsPacketSize {
		data[i] = 0x47
		for j := 1; j < tsPacketSize; j++ {
			data[i+j] = 0xff
		}
	}
	if _, err := readTS(data); err == nil {
		t.Fatal("a stream of nonsense was accepted")
	}
}

func TestConvertAnnexBSkipsEmptyUnits(t *testing.T) {
	// Two start codes in a row name a unit with nothing in it.
	payload, _ := convertAnnexB([]byte{0, 0, 1, 0, 0, 1, 0x65, 0x88})
	want := append([]byte{0, 0, 0, 2}, 0x65, 0x88)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %x, want %x", payload, want)
	}
}

func TestNewTSTrackIgnoresWhatItCannotConvert(t *testing.T) {
	if got := newTSTrack(&astits.PMTElementaryStream{
		ElementaryPID: 1, StreamType: astits.StreamTypeMPEG2Video,
	}); got != nil {
		t.Fatalf("a stream type this package cannot convert yielded %+v", got.track)
	}
}

func TestParameterSetsAreKeptOnlyOnce(t *testing.T) {
	tr := &tsTrack{track: Track{Kind: Video}}
	tr.adoptParameterSets(parameterSets{
		sps: [][]byte{{1}}, pps: [][]byte{{2}}, vps: [][]byte{{3}},
	})
	tr.adoptParameterSets(parameterSets{
		sps: [][]byte{{9}}, pps: [][]byte{{9}}, vps: [][]byte{{9}},
	})
	if tr.config.SPS[0][0] != 1 || tr.config.PPS[0][0] != 2 || tr.config.VPS[0][0] != 3 {
		t.Fatalf("the first sets must be the ones kept: %+v", tr.config)
	}
}

func TestTablesRepeatedInALongerStream(t *testing.T) {
	// A long enough stream repeats the table describing it, and a track must
	// not be declared twice because of that.
	r, err := NewReader(tsFixture(t, 60))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := len(r.TrackIDs()); got != 2 {
		t.Fatalf("tracks = %d, want 2 however often the table is repeated", got)
	}
	samples, err := r.Samples(256)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 60 {
		t.Fatalf("read %d access units, want 60", len(samples))
	}
}

func TestPESTimeWithoutAnyTimestamp(t *testing.T) {
	pes := &astits.PESData{Header: &astits.PESHeader{OptionalHeader: &astits.PESOptionalHeader{}}}
	if got := pesTime(pes); got != 0 {
		t.Fatalf("pesTime = %d, want 0", got)
	}
}

func TestDemuxTSPropagatesAReadFailure(t *testing.T) {
	if _, err := demuxTS([]byte("not a transport stream at all")); err == nil {
		t.Fatal("nonsense was accepted")
	}
}

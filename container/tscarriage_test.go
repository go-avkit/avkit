// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asticode/go-astits"
)

// The testdata/ffmpeg-*.ts fixtures were written by FFmpeg 9.0.2, which is the
// oracle here: a stream it wrote is one a player meets, and its own reader
// (ffprobe) counted the frames each holds. They were made from a 0.2 s sine
// tone with
//
//	ffmpeg -f lavfi -i sine=frequency=440:sample_rate=R:duration=0.2 \
//	       [-ac 2] -c:a C -b:a B [-mpegts_flags system_b] -f mpegts OUT
//
// System A (ATSC: stream type 0x81 or 0x87) is FFmpeg's default and System B
// (DVB: private data and a descriptor) what "-mpegts_flags system_b" asks for.
var ffmpegTSFixtures = []struct {
	file     string
	codec    string
	rate     int
	channels int
	sizes    []int // what ffprobe -show_packets reported, in order
	duration uint32
}{
	{"ffmpeg-ac3.ts", "ac-3", 48000, 2, repeat(384, 7), 2880},
	{"ffmpeg-eac3.ts", "ec-3", 48000, 2, repeat(384, 7), 2880},
	{"ffmpeg-ac3_dvb.ts", "ac-3", 48000, 1, repeat(256, 7), 2880},
	{"ffmpeg-eac3_dvb.ts", "ec-3", 48000, 1, repeat(256, 7), 2880},
	// 1152 samples at 44.1 kHz do not divide the transport clock: 2351.02.
	{"ffmpeg-mp3.ts", "mp3", 44100, 1, append([]int{208}, repeat(209, 8)...), 2351},
	// Half as many samples at half the rate: ISO/IEC 13818-3 Layer III.
	{"ffmpeg-mp3_lsf.ts", "mp3", 22050, 1, []int{104, 105, 104, 105, 104, 105, 104, 105, 104, 105}, 2351},
	{"ffmpeg-mp2.ts", "mp2", 48000, 1, repeat(192, 9), 2160},
}

func repeat(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestFFmpegsTransportStreamsReadFrameByFrame is the reading half of the oracle:
// every frame ffprobe counts in a stream FFmpeg wrote comes back as one sample
// of the size ffprobe reports, timed by the frame's own length, on a track
// named after what the frames say they are.
func TestFFmpegsTransportStreamsReadFrameByFrame(t *testing.T) {
	for _, fx := range ffmpegTSFixtures {
		t.Run(fx.file, func(t *testing.T) {
			r, err := NewReader(readFixture(t, fx.file))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			ids := r.TrackIDs()
			if len(ids) != 1 {
				t.Fatalf("tracks = %+v", r.File().Tracks)
			}
			track := r.File().Tracks[0]
			if track.Kind != Audio || track.Codec != fx.codec ||
				track.SampleRate != fx.rate || track.Channels != fx.channels {
				t.Errorf("track = %+v", track)
			}
			cfg, err := r.TrackConfig(ids[0])
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if cfg.Codec != fx.codec || cfg.SampleRate != fx.rate || cfg.Channels != fx.channels {
				t.Errorf("config = %+v", cfg)
			}
			samples, err := r.Samples(ids[0])
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(samples) != len(fx.sizes) {
				t.Fatalf("%d samples, ffprobe counts %d", len(samples), len(fx.sizes))
			}
			for i, s := range samples {
				if len(s.Data) != fx.sizes[i] || s.Duration != fx.duration || !s.Sync {
					t.Errorf("sample %d: %d bytes, duration %d, sync %v; want %d bytes, duration %d",
						i, len(s.Data), s.Duration, s.Sync, fx.sizes[i], fx.duration)
				}
			}
		})
	}
}

// TestAC3OutOfATransportStreamGoesIntoAnMP4 is the point of building a dac3 or
// dec3 record while reading: a transport stream states none, and an MP4 sample
// entry cannot be written without one.
func TestAC3OutOfATransportStreamGoesIntoAnMP4(t *testing.T) {
	for _, file := range []string{"ffmpeg-ac3.ts", "ffmpeg-eac3_dvb.ts"} {
		t.Run(file, func(t *testing.T) {
			r, err := NewReader(readFixture(t, file))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			id := r.TrackIDs()[0]
			cfg, err := r.TrackConfig(id)
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if len(cfg.CodecConfig) == 0 {
				t.Fatalf("%s config states no record", cfg.Codec)
			}
			samples, err := r.Samples(id)
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			var out bytes.Buffer
			m := NewProgressiveMuxer(&out)
			track, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
			}
			for i, s := range samples {
				if err := m.WriteSample(track, s); err != nil {
					t.Fatalf("WriteSample %d: %v", i, err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			back, err := NewReader(out.Bytes())
			if err != nil {
				t.Fatalf("reading the MP4 back: %v", err)
			}
			got, err := back.Samples(back.TrackIDs()[0])
			if err != nil {
				t.Fatalf("Samples of the MP4: %v", err)
			}
			if len(got) != len(samples) {
				t.Fatalf("the MP4 holds %d samples, the stream %d", len(got), len(samples))
			}
			for i := range got {
				if !bytes.Equal(got[i].Data, samples[i].Data) {
					t.Errorf("sample %d differs", i)
				}
			}
		})
	}
}

// tsRemux copies every track of a transport stream through Reader and
// TSMuxer, which is what a caller converting one would write.
func tsRemux(t *testing.T, data []byte) ([]byte, map[uint32][]Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var out bytes.Buffer
	m := NewTSMuxer(&out)
	wrote := map[uint32][]Sample{}
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig: %v", err)
		}
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		pid, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
		}
		for i, s := range samples {
			if err := m.WriteSample(pid, s); err != nil {
				t.Fatalf("WriteSample(%s, %d): %v", cfg.Codec, i, err)
			}
		}
		wrote[pid] = samples
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes(), wrote
}

// TestTSMuxerWritesEachAudioCarriageAsItsStandardSays is the writing half:
// what the program map states and what each unit is identified by, read back
// with astits, for every audio carriage this package writes, and the samples
// returned byte for byte by the package's own reader.
func TestTSMuxerWritesEachAudioCarriageAsItsStandardSays(t *testing.T) {
	for _, c := range []struct {
		file         string
		streamType   astits.StreamType
		streamID     uint8
		registration uint32 // zero: none
	}{
		// ATSC A/52 Annex A 4.1, 4.2 and 3 (Table A3.1).
		{"ffmpeg-ac3.ts", 0x81, 0xBD, ac3RegistrationID},
		// Annex G 3.1 and 3.2, with the AC-3 registration descriptor that
		// Annex G 2 extends to Enhanced AC-3 in System A.
		{"ffmpeg-eac3.ts", 0x87, 0xBD, ac3RegistrationID},
		// DVB in, ATSC out: this package writes one convention.
		{"ffmpeg-ac3_dvb.ts", 0x81, 0xBD, ac3RegistrationID},
		{"ffmpeg-eac3_dvb.ts", 0x87, 0xBD, ac3RegistrationID},
		// ISO/IEC 13818-1 Table 2-34 and Table 2-22: an audio stream number.
		{"ffmpeg-mp3.ts", 0x03, 0xC0, 0},
		{"ffmpeg-mp3_lsf.ts", 0x04, 0xC0, 0},
		{"ffmpeg-mp2.ts", 0x03, 0xC0, 0},
	} {
		t.Run(c.file, func(t *testing.T) {
			out, wrote := tsRemux(t, readFixture(t, c.file))
			pmt := tsProgramMap(t, out)
			if len(pmt.ElementaryStreams) != 1 {
				t.Fatalf("program map = %+v", pmt)
			}
			es := pmt.ElementaryStreams[0]
			if es.StreamType != c.streamType {
				t.Errorf("stream type = %#x, want %#x", es.StreamType, c.streamType)
			}
			var registrations []uint32
			for _, d := range es.ElementaryStreamDescriptors {
				if d.Registration != nil {
					registrations = append(registrations, d.Registration.FormatIdentifier)
				}
			}
			switch {
			case c.registration == 0 && len(registrations) != 0:
				t.Errorf("registration descriptors %#x on a stream whose type says it all", registrations)
			case c.registration != 0 && (len(registrations) != 1 || registrations[0] != c.registration):
				t.Errorf("registration descriptors = %#x, want %#x", registrations, c.registration)
			}
			units := tsUnitsOfStream(t, out, es.ElementaryPID)
			for i, u := range units {
				if u.PES.Header.StreamID != c.streamID {
					t.Fatalf("unit %d: stream_id = %#x, want %#x", i, u.PES.Header.StreamID, c.streamID)
				}
			}

			r, err := NewReader(out)
			if err != nil {
				t.Fatalf("reading the copy: %v", err)
			}
			id := r.TrackIDs()[0]
			got, err := r.Samples(id)
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			want := wrote[id]
			if len(got) != len(want) {
				t.Fatalf("read back %d samples, wrote %d", len(got), len(want))
			}
			for i := range got {
				if !bytes.Equal(got[i].Data, want[i].Data) || got[i].Duration != want[i].Duration {
					t.Errorf("sample %d differs: %d bytes over %d, wrote %d over %d", i,
						len(got[i].Data), got[i].Duration, len(want[i].Data), want[i].Duration)
				}
			}
		})
	}
}

// tsAudioFixture writes one elementary stream of the given type and
// descriptors, one unit per payload, each unit timed when at[i] is not
// negative and untimed otherwise.
func tsAudioFixture(t *testing.T, streamType astits.StreamType, descriptors []*astits.Descriptor,
	payloads [][]byte, at []int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: streamType, ElementaryStreamDescriptors: descriptors,
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)
	for i, p := range payloads {
		opt := &astits.PESOptionalHeader{MarkerBits: 2}
		if at[i] >= 0 {
			opt.PTSDTSIndicator = astits.PTSDTSIndicatorOnlyPTS
			opt.PTS = &astits.ClockReference{Base: at[i]}
		}
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xBD, OptionalHeader: opt},
			Data:   p,
		}}); err != nil {
			t.Fatalf("write unit %d: %v", i, err)
		}
	}
	return buf.Bytes()
}

// readOnlyTrack reads a stream of one track and returns that track's samples.
func readOnlyTrack(t *testing.T, data []byte) (Track, []Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if len(r.TrackIDs()) != 1 {
		t.Fatalf("tracks = %+v", r.File().Tracks)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	return r.File().Tracks[0], samples
}

// TestAFrameThatStraddlesTwoUnitsIsReadWhole: ISO/IEC 13818-1 lets a unit end
// part-way through a frame, and FFmpeg never does it, so this is the one test
// of it. The frame is carried over and timed from the unit it began in; the
// unit's own timestamp belongs to the first frame that begins in IT.
func TestAFrameThatStraddlesTwoUnitsIsReadWhole(t *testing.T) {
	// Three AC-3 frames of 1536 samples at 48 kHz: 2880 ticks each.
	f := ac3ClassicFrame(t, 2, false, 0, 0) // 128 bytes
	f = append(f, make([]byte, 128-len(f))...)
	frames := [][]byte{marked3(f, 1), marked3(f, 2), marked3(f, 3)}
	stream := bytes.Join(frames, nil)
	// The first unit ends 40 bytes into the second frame. The second unit
	// says when the third frame begins: later than the first two would
	// make it, as a stream with a gap in it would.
	payloads := [][]byte{stream[:168], stream[168:]}
	data := tsAudioFixture(t, astits.StreamTypeAC3Audio, nil, payloads, []int64{9000, 9000 + 2*2880 + 90})

	track, samples := readOnlyTrack(t, data)
	if track.Codec != "ac-3" {
		t.Fatalf("track = %+v", track)
	}
	if len(samples) != 3 {
		t.Fatalf("%d samples, want 3", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, frames[i]) {
			t.Errorf("sample %d is not frame %d", i, i)
		}
	}
	// The second frame lasts until the third, which the second unit timed.
	if samples[0].Duration != 2880 || samples[1].Duration != 2880+90 {
		t.Errorf("durations = %d, %d; want 2880, 2970", samples[0].Duration, samples[1].Duration)
	}
}

// marked3 is a copy of an AC-3 frame whose last byte says which one it is.
func marked3(frame []byte, n byte) []byte {
	out := append([]byte(nil), frame...)
	out[len(out)-1] = n
	return out
}

// TestAUnitWithoutTimestampContinuesTheClock: a unit need not state a
// timestamp, and the frames in it follow on from the frames before.
func TestAUnitWithoutTimestampContinuesTheClock(t *testing.T) {
	f := ac3ClassicFrame(t, 2, false, 0, 0)
	f = append(f, make([]byte, 128-len(f))...)
	data := tsAudioFixture(t, astits.StreamTypeAC3Audio, nil,
		[][]byte{f, f, f}, []int64{900, -1, -1})
	_, samples := readOnlyTrack(t, data)
	if len(samples) != 3 {
		t.Fatalf("%d samples, want 3", len(samples))
	}
	for i, s := range samples {
		if s.Duration != 2880 {
			t.Errorf("sample %d lasts %d, want 2880", i, s.Duration)
		}
	}
}

// TestBytesThatAreNotAFrameArePassedOver: a reader looks for the next sync
// word rather than giving up on a stream, and a few bytes left over at the
// end, too few to be a header, are not a frame.
func TestBytesThatAreNotAFrameArePassedOver(t *testing.T) {
	f := ac3ClassicFrame(t, 2, false, 0, 0)
	f = append(f, make([]byte, 128-len(f))...)
	garbage := bytes.Repeat([]byte{0x55}, 40)
	payload := append(append(append([]byte(nil), garbage...), f...), 0x0B, 0x77, 0x01)
	data := tsAudioFixture(t, astits.StreamTypeAC3Audio, nil, [][]byte{payload}, []int64{0})
	_, samples := readOnlyTrack(t, data)
	if len(samples) != 1 || !bytes.Equal(samples[0].Data, f) {
		t.Fatalf("samples = %d, want the one frame", len(samples))
	}
	// The same for MPEG audio, whose sync word is another.
	mp3 := append(mpaHeader(1, 3, 5, 0, 0, 3), make([]byte, 204)...)
	payload = append(append([]byte(nil), garbage...), mp3...)
	data = tsAudioFixture(t, tsMPEG1Audio, nil, [][]byte{payload}, []int64{0})
	if track, samples := readOnlyTrack(t, data); len(samples) != 1 || track.Codec != "mp3" ||
		!bytes.Equal(samples[0].Data, mp3) {
		t.Errorf("track = %+v with %d samples, want the one frame", track, len(samples))
	}

	// A unit that holds nothing that reads gives a track with nothing in it:
	// what the stream does not say is not invented, and there is no sample
	// to hand over.
	data = tsAudioFixture(t, astits.StreamTypeAC3Audio, nil, [][]byte{garbage}, []int64{0})
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if track := r.File().Tracks[0]; track.Codec != "ac-3" || track.SampleRate != 0 {
		t.Errorf("track = %+v", track)
	}
	if _, err := r.Samples(r.TrackIDs()[0]); err == nil {
		t.Error("an empty track handed over samples")
	}
}

// TestEnhancedAC3SubstreamsShareAnAccessUnit: ATSC A/52 Annex G 3.3 makes the
// access unit every substream of one period, and an MP4 sample is that unit,
// so a dependent substream joins the frame before it rather than becoming a
// sample of its own.
func TestEnhancedAC3SubstreamsShareAnAccessUnit(t *testing.T) {
	independent := eac3Frame(t, 2, false, 0, 3, 64)
	dependent := eac3Frame(t, 2, false, 0, 3, 32)
	dependent[2] |= 0x40 // strmtyp 1: dependent
	// A stream may even begin with one; with nothing before it to join, it
	// stands as a unit, and does not describe the track.
	payload := bytes.Join([][]byte{dependent, independent, dependent, independent}, nil)
	data := tsAudioFixture(t, astits.StreamTypeEAC3Audio, nil, [][]byte{payload}, []int64{0})
	track, samples := readOnlyTrack(t, data)
	if track.Codec != "ec-3" || track.Channels != 2 {
		t.Errorf("track = %+v", track)
	}
	if len(samples) != 3 {
		t.Fatalf("%d samples, want 3", len(samples))
	}
	if want := append(append([]byte(nil), independent...), dependent...); !bytes.Equal(samples[1].Data, want) {
		t.Errorf("sample 1 holds %d bytes, want the independent frame and its dependent one", len(samples[1].Data))
	}
	if !bytes.Equal(samples[2].Data, independent) {
		t.Errorf("sample 2 holds %d bytes", len(samples[2].Data))
	}
}

// TestPrivateDataIsAudioOnlyWhenADescriptorSaysSo covers each way a stream of
// private data names AC-3 or Enhanced AC-3, and the one it does not.
func TestPrivateDataIsAudioOnlyWhenADescriptorSaysSo(t *testing.T) {
	reg := func(id uint32) *astits.Descriptor { return registrationDescriptor(id) }
	for _, c := range []struct {
		name  string
		descs []*astits.Descriptor
		want  string
	}{
		// ETSI EN 300 468 Annex D.2 and D.4: the tag alone identifies it.
		{"AC-3_descriptor", []*astits.Descriptor{{Tag: astits.DescriptorTagAC3, Length: 1,
			AC3: &astits.DescriptorAC3{}}}, "ac-3"},
		{"enhanced_AC-3_descriptor", []*astits.Descriptor{{Tag: astits.DescriptorTagEnhancedAC3, Length: 1,
			EnhancedAC3: &astits.DescriptorEnhancedAC3{}}}, "ec-3"},
		// ATSC A/52 Annex A 3 and Annex G 2, for systems without a type.
		{"registration AC-3", []*astits.Descriptor{reg(ac3RegistrationID)}, "ac-3"},
		{"registration EAC3", []*astits.Descriptor{reg(eac3RegistrationID)}, "ec-3"},
		{"nil, then registration AC-3", []*astits.Descriptor{nil, reg(ac3RegistrationID)}, "ac-3"},
		{"registration Opus", []*astits.Descriptor{reg(0x4F707573)}, ""},
		{"no descriptor", nil, ""},
	} {
		got := privateStreamCodec(&astits.PMTElementaryStream{ElementaryStreamDescriptors: c.descs})
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// And through the reader: a DVB stream marked by its descriptor alone.
	f := eac3Frame(t, 2, false, 0, 3, 64)
	data := tsAudioFixture(t, astits.StreamTypePrivateData, []*astits.Descriptor{{
		Tag: astits.DescriptorTagEnhancedAC3, Length: 1, EnhancedAC3: &astits.DescriptorEnhancedAC3{},
	}}, [][]byte{f}, []int64{0})
	if track, samples := readOnlyTrack(t, data); track.Codec != "ec-3" || len(samples) != 1 {
		t.Errorf("track = %+v with %d samples", track, len(samples))
	}
	// Private data no descriptor names is not a track at all.
	data = tsAudioFixture(t, astits.StreamTypePrivateData, nil, [][]byte{f}, []int64{0})
	if _, err := NewReader(data); !errors.Is(err, ErrNoProgram) {
		t.Errorf("NewReader(unnamed private data) = %v, want ErrNoProgram", err)
	}
}

// TestTheFramesNotTheProgramMapNameTheCodec: a stream type says which family
// the frames belong to; the first frame says which member. A Layer II stream
// under the type MP3 also uses, and an E-AC-3 stream under AC-3's type, are
// named for what they hold.
func TestTheFramesNotTheProgramMapNameTheCodec(t *testing.T) {
	f := eac3Frame(t, 2, false, 0, 3, 64)
	data := tsAudioFixture(t, astits.StreamTypeAC3Audio, nil, [][]byte{f}, []int64{0})
	if track, _ := readOnlyTrack(t, data); track.Codec != "ec-3" {
		t.Errorf("E-AC-3 frames under stream type 0x81 read as %q", track.Codec)
	}
}

// FuzzTSAudioFrames: whatever the payloads and wherever they are cut, the
// splitter returns, hands over no more bytes than it was given, and every
// sample it hands over begins with a frame header that reads.
func FuzzTSAudioFrames(f *testing.F) {
	ac3 := make([]byte, 128)
	ac3[0], ac3[1], ac3[5] = 0x0B, 0x77, 0x40 // fscod 0, frmsizecod 0, bsid 8
	f.Add(append(ac3, ac3...), uint8(100), false)
	f.Add(append(mpaHeader(1, 3, 5, 0, 0, 3), make([]byte, 300)...), uint8(50), true)
	f.Fuzz(func(t *testing.T, data []byte, cut uint8, mpa bool) {
		tr := &tsTrack{track: Track{Kind: Audio}, audio: tsAC3}
		if mpa {
			tr.audio = tsMPA
		}
		step := int(cut) + 1
		for start := 0; start < len(data); start += step {
			end := min(start+step, len(data))
			tr.append(&astits.PESData{Data: data[start:end]})
		}
		tr.finish()
		total := 0
		for i, s := range tr.samples {
			total += len(s.Data)
			_, errAC3 := parseAC3Frame(s.Data)
			_, errMPA := parseMPAFrame(s.Data)
			if (mpa && errMPA != nil) || (!mpa && errAC3 != nil) {
				t.Fatalf("sample %d does not begin with a frame: %x", i, s.Data)
			}
		}
		if total > len(data) {
			t.Fatalf("%d bytes handed over from %d", total, len(data))
		}
	})
}

// TestTSMuxerRefusesWhatItCannotCarry: each refusal names its reason.
func TestTSMuxerRefusesWhatItCannotCarry(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  TrackConfig
		want error
		says string
	}{
		{"opus", TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 48000, Channels: 2, SampleRate: 48000},
			ErrUnsupportedCodec, "never adopted"},
		{"mp3 at 8 kHz", TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 8000, Channels: 1, SampleRate: 8000},
			ErrTrackConfig, "0x04"},
		{"mp2 without a rate", TrackConfig{Kind: Audio, Codec: "mp2", Timescale: 90000},
			ErrTrackConfig, "decides"},
		{"av01 without av1C", TrackConfig{Kind: Video, Codec: "av01", Timescale: 90000},
			ErrTrackConfig, "av1C"},
	} {
		_, err := NewTSMuxer(&bytes.Buffer{}).AddTrack(c.cfg)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: AddTrack = %v, want %v saying %q", c.name, err, c.want, c.says)
		}
	}
}

// TestTSMuxerRefusesASampleItsTrackDoesNotHold: a frame of another codec or
// layer, or no frame at all, written anyway, would be a stream whose program
// map says one thing and whose frames another.
func TestTSMuxerRefusesASampleItsTrackDoesNotHold(t *testing.T) {
	ac3 := ac3ClassicFrame(t, 2, false, 0, 0)
	eac3 := eac3Frame(t, 2, false, 0, 3, 64)
	mp2 := mpaHeader(1, 2, 4, 1, 0, 3) // MPEG-1 Layer II
	for _, c := range []struct {
		name   string
		cfg    TrackConfig
		sample []byte
		says   string
	}{
		{"E-AC-3 in an AC-3 track", TrackConfig{Codec: "ac-3", Timescale: 48000}, eac3, "declared as AC-3"},
		{"AC-3 in an E-AC-3 track", TrackConfig{Codec: "ec-3", Timescale: 48000}, ac3, "declared as Enhanced AC-3"},
		{"no sync frame", TrackConfig{Codec: "ac-3", Timescale: 48000}, []byte{1, 2, 3, 4, 5, 6, 7, 8}, "sync word"},
		{"Layer II in an mp3 track", TrackConfig{Codec: "mp3", Timescale: 48000, SampleRate: 48000}, mp2, "Layer 2"},
		{"no MPEG audio frame", TrackConfig{Codec: "mp3", Timescale: 48000, SampleRate: 48000}, ac3, "sync word"},
		{"an OBU with its forbidden bit set", av1Track(90000, 352, 288), []byte{0x80, 0}, "forbidden"},
	} {
		m := NewTSMuxer(&bytes.Buffer{})
		id, err := m.AddTrack(c.cfg)
		if err != nil {
			t.Fatalf("%s: AddTrack: %v", c.name, err)
		}
		err = m.WriteSample(id, Sample{Data: c.sample, Duration: 1})
		if !errors.Is(err, ErrSample) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: WriteSample = %v, want ErrSample saying %q", c.name, err, c.says)
		}
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// --- helpers -----------------------------------------------------------------

// awTrack is the first audio track of a fixture: its configuration and samples.
func awTrack(t *testing.T, name string) (TrackConfig, []Sample) {
	t.Helper()
	r, err := NewReader(readFixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, id := range r.TrackIDs() {
		cfg, err := r.TrackConfig(id)
		if err != nil || cfg.Kind != Audio {
			continue
		}
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return cfg, samples
	}
	t.Fatalf("%s: no audio track", name)
	return TrackConfig{}, nil
}

// awWrite writes a track in a format and returns the file.
func awWrite(t *testing.T, format Format, cfg TrackConfig, samples []Sample, opts ...AudioOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	a := NewAudioWriter(format, &buf, opts...)
	id, err := a.AddTrack(cfg)
	if err != nil {
		t.Fatalf("%s AddTrack: %v", describeFormat(format), err)
	}
	for i, s := range samples {
		if err := a.WriteSample(id, s); err != nil {
			t.Fatalf("%s sample %d: %v", describeFormat(format), i, err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatalf("%s Close: %v", describeFormat(format), err)
	}
	return buf.Bytes()
}

// awSameSamples fails unless two tracks hold the same frames.
func awSameSamples(t *testing.T, what string, got, want []Sample) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d samples, want %d", what, len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i].Data, want[i].Data) {
			t.Fatalf("%s: sample %d differs", what, i)
		}
	}
}

// awReadBack reads a written file with this package's own demuxer.
func awReadBack(t *testing.T, data []byte) (TrackConfig, []Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("read back: %d tracks", len(ids))
	}
	cfg, err := r.TrackConfig(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	return cfg, samples
}

// awFailWriter fails once it has taken limit bytes.
type awFailWriter struct{ limit int }

var errAWFull = errors.New("the writer is full")

func (w *awFailWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		n := w.limit
		w.limit = 0
		return n, errAWFull
	}
	w.limit -= len(p)
	return len(p), nil
}

// awBits packs fields of (value, width) most significant bit first.
func awBits(fields ...[2]uint32) []byte {
	var out []byte
	var acc uint64
	n := 0
	for _, f := range fields {
		acc = acc<<f[1] | uint64(f[0])
		n += int(f[1])
	}
	for n%8 != 0 {
		acc <<= 1
		n++
	}
	for i := n - 8; i >= 0; i -= 8 {
		out = append(out, byte(acc>>i))
	}
	return out
}

// --- the writer's API --------------------------------------------------------

func TestAudioWriterRefusesWhatItCannotWrite(t *testing.T) {
	aac := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 44100, Channels: 2, SampleRate: 44100}
	t.Run("not an audio-only format", func(t *testing.T) {
		for _, f := range []Format{FormatUnknown, FormatMP4, FormatMatroska, FormatMPEGTS} {
			_, err := NewAudioWriter(f, io.Discard).AddTrack(aac)
			if !errors.Is(err, ErrAudioFormat) {
				t.Errorf("%v: error = %v", f, err)
			}
		}
	})
	t.Run("not an audio track", func(t *testing.T) {
		_, err := NewAudioWriter(FormatADTS, io.Discard).AddTrack(TrackConfig{Kind: Video, Codec: "avc1"})
		if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "adts") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("one track", func(t *testing.T) {
		a := NewAudioWriter(FormatADTS, io.Discard)
		if _, err := a.AddTrack(aac); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AddTrack(aac); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("second track: %v", err)
		}
	})
	t.Run("order", func(t *testing.T) {
		a := NewAudioWriter(FormatADTS, io.Discard)
		if err := a.WriteSample(1, Sample{}); !errors.Is(err, ErrNoTracks) {
			t.Errorf("sample before track: %v", err)
		}
		if err := a.Close(); !errors.Is(err, ErrNoTracks) {
			t.Errorf("close without track: %v", err)
		}
		if err := a.Close(); !errors.Is(err, ErrClosed) {
			t.Errorf("second close: %v", err)
		}
		if _, err := a.AddTrack(aac); !errors.Is(err, ErrClosed) {
			t.Errorf("track after close: %v", err)
		}
		if err := a.WriteSample(1, Sample{}); !errors.Is(err, ErrClosed) {
			t.Errorf("sample after close: %v", err)
		}
		b := NewAudioWriter(FormatADTS, io.Discard)
		if _, err := b.AddTrack(aac); err != nil {
			t.Fatal(err)
		}
		if err := b.WriteSample(7, Sample{}); !errors.Is(err, ErrUnknownTrack) {
			t.Errorf("unknown track: %v", err)
		}
	})
	t.Run("the codec the format does not carry", func(t *testing.T) {
		for _, c := range []struct {
			format Format
			codec  string
		}{
			{FormatADTS, "Opus"}, {FormatADTS, "mp3"}, {FormatADTS, "fLaC"},
			{FormatMPEGAudio, "mp4a"}, {FormatMPEGAudio, "Opus"},
			{FormatFLAC, "Opus"}, {FormatFLAC, "mp4a"},
			{FormatOgg, "mp4a"}, {FormatOgg, "mp3"}, {FormatOgg, "ipcm"},
			{FormatWAV, "fLaC"}, {FormatWAV, "mp4a"}, {FormatWAV, "ac-3"},
		} {
			_, err := NewAudioWriter(c.format, io.Discard).AddTrack(TrackConfig{Kind: Audio, Codec: c.codec,
				Timescale: 48000, Channels: 2, SampleRate: 48000})
			if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), describeFormat(c.format)) {
				t.Errorf("%s into %s: error = %v", c.codec, describeFormat(c.format), err)
			}
		}
	})
}

func TestAudioWriterReportsTheOutputFailing(t *testing.T) {
	cfg, samples := awTrack(t, "sine.flac")
	// The header, a sample and Close each have a point at which the output fails.
	if _, err := NewAudioWriter(FormatFLAC, &awFailWriter{limit: 3}).AddTrack(cfg); !errors.Is(err, errAWFull) {
		t.Errorf("AddTrack: %v", err)
	}
	a := NewAudioWriter(FormatFLAC, &awFailWriter{limit: 60})
	if _, err := a.AddTrack(cfg); err != nil {
		t.Fatal(err)
	}
	var err error
	for _, s := range samples {
		if err = a.WriteSample(1, s); err != nil {
			break
		}
	}
	if !errors.Is(err, errAWFull) {
		t.Errorf("WriteSample: %v", err)
	}
	// Ogg holds its last page back, and an odd-sized WAVE adds a pad byte: their
	// output fails in Close.
	ocfg, osamples := awTrack(t, "sine.opus")
	o := NewAudioWriter(FormatOgg, &awFailWriter{limit: 200})
	if _, err := o.AddTrack(ocfg); err != nil {
		t.Fatal(err)
	}
	for _, s := range osamples[:1] {
		if err := o.WriteSample(1, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Close(); !errors.Is(err, errAWFull) {
		t.Errorf("Close: %v", err)
	}
}

// --- ADTS --------------------------------------------------------------------

func TestADTSRoundTrip(t *testing.T) {
	for _, name := range []string{"sine.aac", "ffmpeg-aaclc.mp4", "ffmpeg-aaclc.mkv", "ffmpeg-heaac.mp4", "heaacv2.m4a"} {
		t.Run(name, func(t *testing.T) {
			cfg, samples := awTrack(t, name)
			out := awWrite(t, FormatADTS, cfg, samples)
			got, back := awReadBack(t, out)
			awSameSamples(t, name, back, samples)
			if got.Codec != "mp4a" {
				t.Errorf("codec = %q", got.Codec)
			}
			// What the header states is the core's, per adtsParameters.
			_, rate, configuration, _ := adtsParameters(cfg.CodecConfig)
			if got.SampleRate != rate || got.Channels != aacChannels(configuration) {
				t.Errorf("read back %d Hz, %d channels; core is %d Hz, configuration %d",
					got.SampleRate, got.Channels, rate, configuration)
			}
		})
	}
}

func TestADTSHeaderIsFFmpegs(t *testing.T) {
	cfg, samples := awTrack(t, "sine.aac")
	out := awWrite(t, FormatADTS, cfg, samples[:1])
	h := out[:adtsHeaderSize]
	// adts_fixed_header: syncword, ID 0 (MPEG-4), layer 0, protection_absent 1;
	// then adts_variable_header with a buffer fullness of 0x7FF.
	if h[0] != 0xFF || h[1]&0xF9 != 0xF1 {
		t.Errorf("sync, ID, layer, protection: % x", h[:2])
	}
	if fullness := int(h[5]&0x1F)<<6 | int(h[6]>>2); fullness != 0x7FF || h[6]&3 != 0 {
		t.Errorf("buffer fullness %#x, blocks %d", fullness, h[6]&3)
	}
	if frame := int(h[3]&3)<<11 | int(h[4])<<3 | int(h[5]>>5); frame != len(out) {
		t.Errorf("frame length %d, frame is %d bytes", frame, len(out))
	}
}

func TestADTSConfiguration(t *testing.T) {
	lc := func(ga uint32) []byte {
		return awBits([2]uint32{2, 5}, [2]uint32{4, 4}, [2]uint32{2, 4}, [2]uint32{ga, 3})
	}
	sbr := func(ga uint32) []byte {
		return awBits([2]uint32{5, 5}, [2]uint32{7, 4}, [2]uint32{1, 4}, [2]uint32{4, 4}, [2]uint32{2, 5}, [2]uint32{ga, 3})
	}
	base := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 44100, Channels: 2, SampleRate: 44100}
	with := func(f func(*TrackConfig)) error {
		cfg := base
		f(&cfg)
		_, err := NewAudioWriter(FormatADTS, io.Discard).AddTrack(cfg)
		return err
	}
	for name, c := range map[string]struct {
		change func(*TrackConfig)
		want   string // empty: accepted
	}{
		"built from the fields": {func(*TrackConfig) {}, ""},
		"eight channels":        {func(c *TrackConfig) { c.Channels = 8 }, ""},
		"a record":              {func(c *TrackConfig) { c.CodecConfig = lc(0) }, ""},
		"hierarchical SBR":      {func(c *TrackConfig) { c.CodecConfig = sbr(0) }, ""},
		"seven channels":        {func(c *TrackConfig) { c.Channels = 7 }, "channel configuration"},
		"no channels":           {func(c *TrackConfig) { c.Channels = 0 }, "channel configuration"},
		"a profile ADTS cannot": {func(c *TrackConfig) { c.AudioObjectType = 29 }, "mp4a"},
		"a rate with no index":  {func(c *TrackConfig) { c.SampleRate = 12345 }, "mp4a"},
		"960 window":            {func(c *TrackConfig) { c.CodecConfig = lc(4) }, "GASpecificConfig"},
		"extension flag":        {func(c *TrackConfig) { c.CodecConfig = lc(1) }, "GASpecificConfig"},
		"flags behind SBR":      {func(c *TrackConfig) { c.CodecConfig = sbr(2) }, "GASpecificConfig"},
		"a layout in the frames": {func(c *TrackConfig) {
			c.CodecConfig = awBits([2]uint32{2, 5}, [2]uint32{4, 4}, [2]uint32{0, 4}, [2]uint32{0, 3})
		}, "channel configuration"},
		"a reserved rate index": {func(c *TrackConfig) {
			c.CodecConfig = awBits([2]uint32{2, 5}, [2]uint32{13, 4}, [2]uint32{2, 4}, [2]uint32{0, 3})
		}, "core no ADTS header"},
		"a core that is not AAC-LC": {func(c *TrackConfig) {
			c.CodecConfig = awBits([2]uint32{17, 5}, [2]uint32{4, 4}, [2]uint32{2, 4}, [2]uint32{0, 3})
		}, "core no ADTS header"},
	} {
		err := with(c.change)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (!errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error = %v, want one naming %q", name, err, c.want)
		}
	}
}

func TestADTSSamples(t *testing.T) {
	a := NewAudioWriter(FormatADTS, io.Discard)
	id, err := a.AddTrack(TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 44100, Channels: 2, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSample(id, Sample{}); err != nil {
		t.Errorf("an empty sample is skipped, as adtsenc.c does: %v", err)
	}
	if err := a.WriteSample(id, Sample{Data: make([]byte, tsADTSPayloadMax+1)}); !errors.Is(err, ErrSample) {
		t.Errorf("a frame ADTS cannot hold: %v", err)
	}
}

// --- MPEG audio --------------------------------------------------------------

func TestMPEGAudioRoundTrip(t *testing.T) {
	for _, name := range []string{"sine.mp3", "sine.mp2", "ffmpeg-mp3.mp4", "ffmpeg-mp3.mkv", "ffmpeg-mp2.mkv", "ffmpeg-mp3_lsf.mp4"} {
		t.Run(name, func(t *testing.T) {
			cfg, samples := awTrack(t, name)
			out := awWrite(t, FormatMPEGAudio, cfg, samples)
			// The file is the frames and nothing else: no ID3v2, no Xing.
			var frames []byte
			for _, s := range samples {
				frames = append(frames, s.Data...)
			}
			if !bytes.Equal(out, frames) {
				t.Errorf("%d bytes written, the frames are %d", len(out), len(frames))
			}
			got, back := awReadBack(t, out)
			awSameSamples(t, name, back, samples)
			if got.Codec != cfg.Codec || got.SampleRate != cfg.SampleRate || got.Channels != cfg.Channels {
				t.Errorf("read back %s %d Hz %d ch, wrote %s %d Hz %d ch", got.Codec, got.SampleRate, got.Channels,
					cfg.Codec, cfg.SampleRate, cfg.Channels)
			}
		})
	}
}

func TestMPEGAudioRefusesSamplesThatAreNotItsFrames(t *testing.T) {
	cfg, samples := awTrack(t, "sine.mp3")
	a := NewAudioWriter(FormatMPEGAudio, io.Discard)
	id, err := a.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSample(id, Sample{Data: []byte("not a frame")}); !errors.Is(err, ErrSample) {
		t.Errorf("garbage: %v", err)
	}
	two := cfg
	two.Codec = "mp2"
	b := NewAudioWriter(FormatMPEGAudio, io.Discard)
	id, _ = b.AddTrack(two)
	if err := b.WriteSample(id, samples[0]); !errors.Is(err, ErrSample) {
		t.Errorf("a Layer III frame in an mp2 track: %v", err)
	}
}

// --- native FLAC -------------------------------------------------------------

func TestFLACRoundTrip(t *testing.T) {
	for _, name := range []string{"sine.flac", "ffmpeg-flac.mp4", "ffmpeg-flac.mkv"} {
		t.Run(name, func(t *testing.T) {
			cfg, samples := awTrack(t, name)
			out := awWrite(t, FormatFLAC, cfg, samples)
			if !bytes.HasPrefix(out, []byte("fLaC")) {
				t.Fatal("no marker")
			}
			got, back := awReadBack(t, out)
			awSameSamples(t, name, back, samples)
			// STREAMINFO is the source's, byte for byte: the sample count and the
			// MD5 signature are not computed.
			if want := cfg.CodecConfig[:4+4+flacStreamInfoSize]; !bytes.Equal(got.CodecConfig[:len(want)], want[:4]) &&
				!bytes.Equal(got.CodecConfig[8:8+flacStreamInfoSize], want[8:]) {
				t.Errorf("STREAMINFO changed")
			}
		})
	}
}

func TestFLACKeepsTheBlocksButNotTheSeekTable(t *testing.T) {
	cfg, samples := awTrack(t, "sine.flac")
	info := cfg.CodecConfig[8 : 8+flacStreamInfoSize]
	block := func(kind byte, last bool, body ...byte) []byte {
		if last {
			kind |= 0x80
		}
		return append([]byte{kind, 0, 0, byte(len(body))}, body...)
	}
	config := append([]byte(flacSignature), block(0, false, info...)...)
	config = append(config, block(3, false, make([]byte, 18)...)...)
	config = append(config, block(4, true, 1, 2, 3)...)
	cfg.CodecConfig = config
	out := awWrite(t, FormatFLAC, cfg, samples[:1])
	want := append([]byte(flacSignature), block(0, false, info...)...)
	want = append(want, block(4, true, 1, 2, 3)...)
	if !bytes.HasPrefix(out, want) || !bytes.Equal(out[len(want):], samples[0].Data) {
		t.Errorf("blocks written: % x", out[:len(want)+4])
	}
	// Dropping the SEEKTABLE that was last still leaves a last block.
	config = append([]byte(flacSignature), block(0, false, info...)...)
	config = append(config, block(3, true, make([]byte, 18)...)...)
	cfg.CodecConfig = config
	out = awWrite(t, FormatFLAC, cfg, nil)
	if want := append([]byte(flacSignature), block(0, true, info...)...); !bytes.Equal(out, want) {
		t.Errorf("only STREAMINFO: % x", out)
	}
}

func TestFLACRefusals(t *testing.T) {
	cfg, samples := awTrack(t, "sine.flac")
	bad := cfg
	bad.CodecConfig = nil
	if _, err := NewAudioWriter(FormatFLAC, io.Discard).AddTrack(bad); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("no configuration: %v", err)
	}
	a := NewAudioWriter(FormatFLAC, io.Discard)
	id, _ := a.AddTrack(cfg)
	if err := a.WriteSample(id, Sample{Data: samples[0].Data[:2]}); !errors.Is(err, ErrSample) {
		t.Errorf("a truncated frame: %v", err)
	}
}

// --- Remux -------------------------------------------------------------------

func TestRemuxWritesAudioOnlyFiles(t *testing.T) {
	for _, c := range []struct {
		src    string
		format Format
	}{
		{"ffmpeg-aaclc.mp4", FormatADTS}, {"ffmpeg-mp3.mp4", FormatMPEGAudio}, {"ffmpeg-flac.mkv", FormatFLAC},
		{"ffmpeg-opus.mp4", FormatOgg}, {"sine_f32.wav", FormatWAV},
	} {
		t.Run(c.src, func(t *testing.T) {
			r, err := NewReader(readFixture(t, c.src))
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := Remux(&buf, r, AudioOutput(c.format)); err != nil {
				t.Fatal(err)
			}
			if got := Sniff(buf.Bytes()); got != c.format {
				t.Errorf("sniffed as %v, want %v", describeFormat(got), describeFormat(c.format))
			}
		})
	}
}

func TestRemuxAudioOutputNeedsOneTrackOfTheRightCodec(t *testing.T) {
	aac := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 44100, Channels: 2, SampleRate: 44100}
	two := fixtureTrack{cfg: aac, samples: []Sample{{Data: []byte{1, 2, 3}, Duration: 1024}}}
	if err := Remux(io.Discard, fixtureReader(t, two, two), AudioOutput(FormatADTS)); !errors.Is(err, ErrTrackConfig) ||
		!strings.Contains(err.Error(), "DropTracks") {
		t.Errorf("two tracks: %v", err)
	}
	r, err := NewReader(readFixture(t, "tiny.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Remux(io.Discard, r, AudioOutput(FormatADTS)); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("a video track: %v", err)
	}
	ro, err := NewReader(readFixture(t, "ffmpeg-opus.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Remux(io.Discard, ro, AudioOutput(FormatADTS)); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("Opus into ADTS: %v", err)
	}
	rm, _ := NewReader(readFixture(t, "ffmpeg-aaclc.mp4"))
	if err := Remux(io.Discard, rm, AudioOutput(FormatMP4)); !errors.Is(err, ErrAudioFormat) {
		t.Errorf("a video container as audio output: %v", err)
	}
}

func TestConcatAndCutWriteAudioOnlyFiles(t *testing.T) {
	data := readFixture(t, "ffmpeg-aaclc.mp4")
	r1, _ := NewReader(data)
	r2, _ := NewReader(data)
	_, samples := awTrack(t, "ffmpeg-aaclc.mp4")
	var buf bytes.Buffer
	if err := Concat(&buf, []*Reader{r1, r2}, AudioOutput(FormatADTS)); err != nil {
		t.Fatal(err)
	}
	_, back := awReadBack(t, buf.Bytes())
	awSameSamples(t, "concat", back, append(append([]Sample(nil), samples...), samples...))

	buf.Reset()
	r3, _ := NewReader(data)
	if err := Cut(&buf, r3, 0, 0, AudioOutput(FormatADTS)); err != nil {
		t.Fatal(err)
	}
	_, back = awReadBack(t, buf.Bytes())
	awSameSamples(t, "cut", back, samples)

	// A writer that fails ends the copy: the declaration and the samples.
	r4, _ := NewReader(data)
	if err := Remux(&awFailWriter{limit: 10}, r4, AudioOutput(FormatADTS)); !errors.Is(err, errAWFull) {
		t.Errorf("failing output: %v", err)
	}
}

func TestRemuxAudioOutputStopsOnASampleTheFormatRefuses(t *testing.T) {
	// A frame ADTS cannot hold comes out of the writer as the error of the copy.
	src := fixtureReader(t, fixtureTrack{cfg: TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 44100,
		Channels: 2, SampleRate: 44100}, samples: []Sample{{Data: make([]byte, tsADTSPayloadMax+1), Duration: 1024}}})
	if err := Remux(io.Discard, src, AudioOutput(FormatADTS)); !errors.Is(err, ErrSample) {
		t.Errorf("error = %v", err)
	}
}

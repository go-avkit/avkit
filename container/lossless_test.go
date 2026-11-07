// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The fixtures were written by FFmpeg 9.0.2:
//
//	ffmpeg -fflags +bitexact -f lavfi -i sine=frequency=440:sample_rate=R:duration=D \
//	       -ac C -c:a alac -flags:a +bitexact OUT.m4a      (R 48000 stereo, 96000 stereo, 48000 6 channels)
//	ffmpeg -i ffmpeg-alac.m4a -c copy ffmpeg-alac.mkv, and -f mov for ffmpeg-alac.mov
//	ffmpeg -f lavfi -i sine=...:duration=0.5 -ac 2 -c:a truehd -strict -2 -f truehd T.thd, then
//	ffmpeg -i T.thd -c copy -strict -2 ffmpeg-truehd.mp4 (and .mkv, without -strict)
var losslessFixtures = []struct {
	file     string
	codec    string
	rate     int
	channels int
	packets  int
	depth    int // ALAC bit depth; 0 for TrueHD
}{
	{"ffmpeg-alac.m4a", "alac", 48000, 2, 6, 16},
	{"ffmpeg-alac.mkv", "alac", 48000, 2, 6, 16},
	{"ffmpeg-alac.mov", "alac", 48000, 2, 6, 16},
	{"ffmpeg-alac-96k.m4a", "alac", 96000, 2, 8, 16},
	{"ffmpeg-alac-6ch.m4a", "alac", 48000, 6, 4, 16},
	{"ffmpeg-truehd.mp4", "mlpa", 48000, 2, 600, 0},
	{"ffmpeg-truehd.mkv", "mlpa", 48000, 2, 600, 0},
}

func TestLosslessAudioGoesIntoEveryMuxer(t *testing.T) {
	for _, fx := range losslessFixtures {
		t.Run(fx.file, func(t *testing.T) {
			r, err := NewReader(readFixture(t, fx.file))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			f := r.File()
			if tr := f.Tracks[0]; tr.Channels != fx.channels || tr.SampleRate != fx.rate {
				t.Errorf("Demux says %d ch %d Hz, want %d ch %d Hz", tr.Channels, tr.SampleRate, fx.channels, fx.rate)
			}
			cfg, err := r.TrackConfig(r.TrackIDs()[0])
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if cfg.Codec != fx.codec || cfg.SampleRate != fx.rate || cfg.Channels != fx.channels || len(cfg.CodecConfig) == 0 {
				t.Fatalf("read %s %d Hz %d ch, config %x; want %s %d Hz %d ch", cfg.Codec, cfg.SampleRate,
					cfg.Channels, cfg.CodecConfig, fx.codec, fx.rate, fx.channels)
			}
			samples, err := r.Samples(r.TrackIDs()[0])
			if err != nil || len(samples) != fx.packets {
				t.Fatalf("Samples = %d, %v; want %d", len(samples), err, fx.packets)
			}
			for _, kind := range []string{"fragmented", "progressive", "matroska"} {
				data := writeAudio(t, kind, cfg, samples)
				checkLosslessReadsBack(t, kind, data, cfg, samples)
				if kind == "matroska" {
					checkLosslessMatroskaEntry(t, data, fx.codec, cfg)
				} else {
					checkLosslessMP4Entry(t, data, fx.codec, cfg, fx.depth)
				}
			}
		})
	}
}

func checkLosslessReadsBack(t *testing.T, kind string, data []byte, cfg TrackConfig, samples []Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("%s: NewReader: %v", kind, err)
	}
	got, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("%s: TrackConfig: %v", kind, err)
	}
	if got.Codec != cfg.Codec || got.SampleRate != cfg.SampleRate || got.Channels != cfg.Channels ||
		!bytes.Equal(got.CodecConfig, cfg.CodecConfig) {
		t.Errorf("%s: read back %s %d Hz %d ch %x, want %s %d Hz %d ch %x", kind, got.Codec, got.SampleRate,
			got.Channels, got.CodecConfig, cfg.Codec, cfg.SampleRate, cfg.Channels, cfg.CodecConfig)
	}
	back, err := r.Samples(r.TrackIDs()[0])
	if err != nil || len(back) != len(samples) {
		t.Fatalf("%s: Samples = %d, %v; want %d", kind, len(back), err, len(samples))
	}
	for i := range back {
		if !bytes.Equal(back[i].Data, samples[i].Data) {
			t.Fatalf("%s: sample %d differs", kind, i)
		}
	}
}

func checkLosslessMP4Entry(t *testing.T, data []byte, codec string, cfg TrackConfig, depth int) {
	t.Helper()
	stsd := sampleEntryOf(t, data)
	if codec == "alac" {
		entry, ok := stsd.Children[0].(*mp4.AudioSampleEntryBox)
		if !ok || entry.Type() != "alac" || entry.Alac == nil {
			t.Fatalf("entry %T, want an alac one holding an alac box", stsd.Children[0])
		}
		if !bytes.Equal(alacCookie(entry.Alac), cfg.CodecConfig) {
			t.Errorf("cookie %x, want %x", alacCookie(entry.Alac), cfg.CodecConfig)
		}
		want := cfg.SampleRate
		if want > alacMaxEntry {
			want = 0
		}
		if int(entry.SampleRate) != want || int(entry.ChannelCount) != cfg.Channels || int(entry.SampleSize) != depth {
			t.Errorf("entry states %d Hz, %d ch, %d bits; want %d, %d, %d", entry.SampleRate, entry.ChannelCount,
				entry.SampleSize, want, cfg.Channels, depth)
		}
		return
	}
	entry, ok := stsd.Children[0].(*mp4.UnknownBox)
	if !ok || entry.Type() != "mlpa" {
		t.Fatalf("entry %T, want mlpa", stsd.Children[0])
	}
	p := entry.Payload()
	if binary.BigEndian.Uint16(p[16:]) != uint16(cfg.Channels) || binary.BigEndian.Uint16(p[18:]) != 16 ||
		int(binary.BigEndian.Uint32(p[24:])) != cfg.SampleRate {
		t.Errorf("mlpa fixed fields % x", p[:28])
	}
	if want := append([]byte{0, 0, 0, 18, 'd', 'm', 'l', 'p'}, cfg.CodecConfig...); !bytes.Equal(p[28:], want) {
		t.Errorf("mlpa boxes % x, want % x", p[28:], want)
	}
}

func checkLosslessMatroskaEntry(t *testing.T, data []byte, codec string, cfg TrackConfig) {
	t.Helper()
	f := readWebM(t, data)
	if f.Header.DocType != "matroska" {
		t.Errorf("DocType %q: WebM allows neither codec", f.Header.DocType)
	}
	entry := f.Segment.Tracks.TrackEntry[0]
	id, want := "A_ALAC", cfg.CodecConfig
	if codec == "mlpa" {
		id, want = "A_TRUEHD", nil
	}
	if entry.CodecID != id || !bytes.Equal(entry.CodecPrivate, want) {
		t.Errorf("CodecID %q private %x, want %q %x", entry.CodecID, entry.CodecPrivate, id, want)
	}
	if entry.Audio == nil || int(entry.Audio.SamplingFrequency) != cfg.SampleRate ||
		int(entry.Audio.Channels) != cfg.Channels {
		t.Errorf("Audio %+v, want %d Hz %d ch", entry.Audio, cfg.SampleRate, cfg.Channels)
	}
}

// TestRemuxCarriesLosslessAudioBetweenContainers is the equality FFmpeg is the
// judge of: what it reads out of the output is what it reads out of the source,
// packet for packet, and the stream parameters and extradata agree.
func TestRemuxCarriesLosslessAudioBetweenContainers(t *testing.T) {
	for _, fx := range losslessFixtures {
		for _, kind := range []string{"fragmented", "progressive", "matroska"} {
			t.Run(fx.file+" to "+kind, func(t *testing.T) {
				r, err := NewReader(readFixture(t, fx.file))
				if err != nil {
					t.Fatal(err)
				}
				cfg, _ := r.TrackConfig(r.TrackIDs()[0])
				samples, _ := r.Samples(r.TrackIDs()[0])
				ext := ".mp4"
				if kind == "matroska" {
					ext = ".mkv"
				}
				checkOracle(t, fx.file+" to "+kind, ext, writeAudio(t, kind, cfg, samples), oracleStreams|oracleSums|oracleDuration,
					func(string) string { return "testdata/" + fx.file })
			})
		}
		t.Run(fx.file+" through Remux", func(t *testing.T) {
			r, err := NewReader(readFixture(t, fx.file))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := Remux(&out, r); err != nil {
				t.Fatalf("Remux: %v", err)
			}
			checkOracle(t, fx.file+" through Remux", ".mp4", out.Bytes(), oracleStreams|oracleSums|oracleDuration,
				func(string) string { return "testdata/" + fx.file })
		})
	}
}

// ffmpeg's A_ALAC CodecPrivate is the atom this package writes, byte for byte.
func TestAlacCodecPrivateIsTheOneFFmpegWrites(t *testing.T) {
	r, err := NewReader(readFixture(t, "ffmpeg-alac.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.TrackConfig(r.TrackIDs()[0])
	want := readWebM(t, readFixture(t, "ffmpeg-alac.mkv")).Segment.Tracks.TrackEntry[0].CodecPrivate
	if !bytes.Equal(cfg.CodecConfig, want) {
		t.Errorf("cookie % x, FFmpeg writes % x", cfg.CodecConfig, want)
	}
	// The reader takes the bare cookie and the atom of the cookie description
	// alike: the size, "alac", version and flags before it.
	atom := append([]byte{0, 0, 0, 36, 'a', 'l', 'a', 'c', 0, 0, 0, 0}, want...)
	if !bytes.Equal(alacMatroskaCookie(atom), want) || !bytes.Equal(alacMatroskaCookie(want), want) {
		t.Errorf("the cookie is not read back from either form")
	}
	// A size that is not the length is not an atom.
	atom[3] = 40
	if got := alacMatroskaCookie(atom); !bytes.Equal(got, atom) {
		t.Errorf("a wrong size was read as an atom")
	}
}

// goodCookie is a valid ALACSpecificConfig: 4096 frames, 16 bits, 2 channels, 48 kHz.
func goodCookie() []byte {
	return []byte{0, 0, 16, 0, 0, 16, 40, 10, 14, 2, 0, 0, 0, 0, 64, 4, 0, 23, 112, 0, 0, 0, 187, 128}
}

func TestAlacCookieIsCheckedBeforeItIsWritten(t *testing.T) {
	with := func(f func(c []byte) []byte) []byte { return f(goodCookie()) }
	bad := map[string][]byte{
		"short":         goodCookie()[:23],
		"odd layout":    append(goodCookie(), 1, 2, 3),
		"frame length":  with(func(c []byte) []byte { clear(c[0:4]); return c }),
		"version":       with(func(c []byte) []byte { c[4] = 1; return c }),
		"bit depth":     with(func(c []byte) []byte { c[5] = 12; return c }),
		"no channels":   with(func(c []byte) []byte { c[9] = 0; return c }),
		"nine channels": with(func(c []byte) []byte { c[9] = 9; return c }),
		"no rate":       with(func(c []byte) []byte { clear(c[20:24]); return c }),
		"huge rate":     with(func(c []byte) []byte { c[20] = 0x80; return c }),
	}
	for name, cookie := range bad {
		if _, err := parseALACCookie(cookie); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("%s: parseALACCookie = %v, want ErrTrackConfig", name, err)
		}
		cfg := TrackConfig{Kind: Audio, Codec: "alac", Timescale: 48000, CodecConfig: cookie, Channels: 2, SampleRate: 48000}
		for _, kind := range []string{"fragmented", "matroska"} {
			var m audioMuxer = NewMuxer(&bytes.Buffer{})
			if kind == "matroska" {
				m = NewWebMMuxer(&bytes.Buffer{})
			}
			if _, err := m.AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("%s in %s: AddTrack = %v, want ErrTrackConfig", name, kind, err)
			}
		}
	}
	// An ALAC track states its cookie.
	cfg := TrackConfig{Kind: Audio, Codec: "alac", Timescale: 48000, Channels: 2, SampleRate: 48000}
	if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("no cookie: AddTrack = %v, want ErrTrackConfig", err)
	}
	// A cookie with its channel layout is carried whole.
	layout := append(goodCookie(), make([]byte, 24)...)
	cfg.CodecConfig = layout
	data := writeAudio(t, "fragmented", cfg, []Sample{{Data: []byte{1}, Duration: 4096, Sync: true}})
	got, _ := mustReader(t, data).TrackConfig(1)
	if !bytes.Equal(got.CodecConfig, layout) {
		t.Errorf("cookie with layout read back as %x", got.CodecConfig)
	}
}

func mustReader(t *testing.T, data []byte) *Reader {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAlacCookieOfTheEntryWinsOverItsRateField(t *testing.T) {
	// A sample entry with no alac box describes nothing.
	stsd := mp4.NewStsdBox()
	if alacSampleEntry(stsd) != nil {
		t.Errorf("an empty stsd has a cookie")
	}
	stsd.AddChild(mp4.CreateAudioSampleEntryBox("alac", 2, 16, 48000, nil))
	if alacSampleEntry(stsd) != nil {
		t.Errorf("an alac entry without its box has a cookie")
	}
	stsd = mp4.NewStsdBox()
	stsd.AddChild(mp4.CreateAudioSampleEntryBox("mp4a", 2, 16, 48000, nil))
	if alacSampleEntry(stsd) != nil {
		t.Errorf("an mp4a entry has a cookie")
	}
	// A cookie that cannot be read leaves the entry's own fields.
	var cfg TrackConfig
	alacTrackConfig(&cfg, []byte{1, 2, 3})
	if cfg.Channels != 0 || len(cfg.CodecConfig) != 3 {
		t.Errorf("alacTrackConfig took %+v from a broken cookie", cfg)
	}
	var tr Track
	stsd = mp4.NewStsdBox()
	stsd.AddChild(mp4.CreateAudioSampleEntryBox("alac", 2, 16, 48000, &mp4.AlacBox{NumChannels: 1}))
	applySampleEntry(&tr, stsd)
	if tr.Channels != 2 || tr.SampleRate != 48000 {
		t.Errorf("a broken cookie changed the track to %d ch %d Hz", tr.Channels, tr.SampleRate)
	}
}

// goodDMLP is the dmlp payload of the fixtures: 48 kHz, 2 channels.
func goodDMLP() []byte { return []byte{0, 0x50, 0xa0, 0x01, 0x18, 0xfe, 0, 0, 0, 0} }

func TestTrueHDRates(t *testing.T) {
	for bits, want := range map[uint32]int{0: 48000, 1: 96000, 2: 192000, 8: 44100, 9: 88200, 10: 176400} {
		got, ok := mlpSampleRate(bits << 28)
		if !ok || got != want {
			t.Errorf("ratebits %d: %d, %v; want %d", bits, got, ok, want)
		}
		if n := mlpAccessUnitSamples(bits << 28); n != 40<<(bits&7) {
			t.Errorf("ratebits %d: an access unit is %d samples", bits, n)
		}
	}
	for _, bits := range []uint32{3, 7, 11, 15} {
		if _, ok := mlpSampleRate(bits << 28); ok {
			t.Errorf("ratebits %d is reserved and was given a rate", bits)
		}
	}
}

func TestTrueHDConfigurationIsCheckedBeforeItIsWritten(t *testing.T) {
	reserved := goodDMLP()
	reserved[0] = 0xF0
	for name, cfg := range map[string]TrackConfig{
		"no dmlp":        {Channels: 2},
		"short dmlp":     {Channels: 2, CodecConfig: goodDMLP()[:9]},
		"reserved rate":  {Channels: 2, CodecConfig: reserved},
		"no channels":    {CodecConfig: goodDMLP()},
		"many channels":  {Channels: 17, CodecConfig: goodDMLP()},
		"rate against":   {Channels: 2, SampleRate: 44100, CodecConfig: goodDMLP()},
		"no channels mk": {CodecConfig: goodDMLP(), SampleRate: 48000},
	} {
		cfg.Kind, cfg.Codec, cfg.Timescale = Audio, "mlpa", 48000
		if name != "no channels mk" {
			if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("%s: Muxer.AddTrack = %v, want ErrTrackConfig", name, err)
			}
		}
		if name == "no dmlp" || name == "no channels" || name == "no channels mk" || name == "rate against" {
			if _, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("%s: WebMMuxer.AddTrack = %v, want ErrTrackConfig", name, err)
			}
		}
	}
	// Matroska has no CodecPrivate for TrueHD, but a dmlp it is handed is checked.
	cfg := TrackConfig{Kind: Audio, Codec: "mlpa", Timescale: 48000, Channels: 2, CodecConfig: reserved}
	if _, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("reserved rate in Matroska: AddTrack = %v, want ErrTrackConfig", err)
	}
	// And one without a rate of its own takes it from the format.
	cfg = TrackConfig{Kind: Audio, Codec: "mlpa", Timescale: 48000, Channels: 2, CodecConfig: goodDMLP()}
	if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); err != nil {
		t.Errorf("a dmlp states the rate: %v", err)
	}
}

// mlpaEntryBytes builds the payload of an mlpa entry around the given boxes.
func mlpaEntryBytes(channels uint16, boxes ...[]byte) []byte {
	p := make([]byte, 28)
	p[7] = 1
	binary.BigEndian.PutUint16(p[16:], channels)
	for _, b := range boxes {
		p = append(p, b...)
	}
	return p
}

func isoBox(name string, payload []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(8+len(payload))), append([]byte(name), payload...)...)
}

func stsdWith(entry mp4.Box) *mp4.StsdBox {
	stsd := mp4.NewStsdBox()
	stsd.AddChild(entry)
	return stsd
}

func TestAnMlpaEntryIsReadBoxByBox(t *testing.T) {
	unknown := func(p []byte) *mp4.StsdBox { return stsdWith(mp4.CreateUnknownBox("mlpa", uint64(8+len(p)), p)) }
	if mlpaSampleEntry(mp4.NewStsdBox()) != nil {
		t.Errorf("an empty stsd has an mlpa entry")
	}
	if mlpaSampleEntry(stsdWith(mp4.CreateAudioSampleEntryBox("mp4a", 2, 16, 48000, nil))) != nil ||
		mlpaSampleEntry(stsdWith(mp4.CreateUnknownBox("abcd", 8, nil))) != nil {
		t.Errorf("another entry was read as mlpa")
	}
	reserved := goodDMLP()
	reserved[0] = 0xF0
	for name, p := range map[string][]byte{
		"fixed fields cut": make([]byte, 20),
		"no boxes":         mlpaEntryBytes(2),
		"box too small":    mlpaEntryBytes(2, []byte{0, 0, 0, 4, 'b', 't', 'r', 't'}),
		"box too long":     mlpaEntryBytes(2, []byte{0, 0, 1, 0, 'b', 't', 'r', 't'}),
		"no dmlp":          mlpaEntryBytes(2, isoBox("btrt", make([]byte, 12))),
		"dmlp too short":   mlpaEntryBytes(2, isoBox("dmlp", goodDMLP()[:6])),
		"dmlp reserved":    mlpaEntryBytes(2, isoBox("dmlp", reserved)),
	} {
		if mlpaSampleEntry(unknown(p)) != nil {
			t.Errorf("%s: an entry was read", name)
		}
	}
	// A box ahead of the dmlp is stepped over, and one after it is ignored.
	f := mlpaSampleEntry(unknown(mlpaEntryBytes(6, isoBox("btrt", make([]byte, 12)), isoBox("dmlp", goodDMLP()), isoBox("free", nil))))
	if f == nil || f.channels != 6 || f.rate != 48000 || !bytes.Equal(f.dmlp, goodDMLP()) {
		t.Fatalf("the entry was read as %+v", f)
	}
	var tr Track
	applySampleEntry(&tr, unknown(mlpaEntryBytes(6, isoBox("dmlp", goodDMLP()))))
	if tr.Channels != 6 || tr.SampleRate != 48000 {
		t.Errorf("Demux says %d ch %d Hz", tr.Channels, tr.SampleRate)
	}
	tr = Track{}
	applySampleEntry(&tr, unknown(mlpaEntryBytes(6)))
	if tr.Channels != 0 || tr.Codec != "mlpa" {
		t.Errorf("an entry with no dmlp stated %d channels, codec %q", tr.Channels, tr.Codec)
	}
}

// aMajorSync is a TrueHD access unit's head: its length and timing, the major
// sync, format_info, signature, flags, reserved bytes and the peak data rate.
func aMajorSync() []byte {
	au := []byte{0x00, 0x20, 0x00, 0x00, 0xF8, 0x72, 0x6F, 0xBA, 0x00, 0x50, 0xA0, 0x01, 0xB7, 0x52, 0x00, 0x00, 0x00, 0x00, 0x8C, 0x7F}
	return append(au, make([]byte, 12)...)
}

func TestTrueHDConfigurationIsTakenFromTheMajorSync(t *testing.T) {
	dmlp, ok := mlpMajorSyncOf(aMajorSync())
	// The peak data rate is 15 bits, and the variable-rate bit above it is not
	// part of it: 0x8C7F is a variable rate of 0x0C7F, which a dmlp states as
	// 0x18FE.
	if !ok || !bytes.Equal(dmlp, goodDMLP()) {
		t.Errorf("the major sync gave % x, %v; want % x", dmlp, ok, goodDMLP())
	}
	short := aMajorSync()[:19]
	wrong := aMajorSync()
	wrong[7] = 0xBB // an MLP stream, not TrueHD
	for name, au := range map[string][]byte{"short": short, "mlp": wrong, "none": {1, 2, 3}} {
		if _, ok := mlpMajorSyncOf(au); ok {
			t.Errorf("%s: a major sync was found", name)
		}
	}
	cfg := TrackConfig{Kind: Audio, Codec: "mlpa", Timescale: 1000, Channels: 2}
	samples := []Sample{{Data: []byte{0, 0x10, 0, 0, 1, 2, 3, 4}}, {Data: aMajorSync(), Duration: 1}, {Data: aMajorSync()}}
	got, err := trueHDFromSamples(cfg, samples)
	if err != nil || !bytes.Equal(got.CodecConfig, goodDMLP()) || got.SampleRate != 48000 {
		t.Errorf("trueHDFromSamples = %+v, %v", got, err)
	}
	if _, err := trueHDFromSamples(cfg, samples[:1]); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("no major sync: %v, want ErrTrackConfig", err)
	}
	reserved := aMajorSync()
	reserved[8] = 0xF0
	if _, err := trueHDFromSamples(cfg, []Sample{{Data: reserved}}); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("reserved rate: %v, want ErrTrackConfig", err)
	}
	// describeFromSamples does the same for a track Remux copies.
	cfg2, err := describeFromSamples(cfg, samples)
	if err != nil || !bytes.Equal(cfg2.CodecConfig, goodDMLP()) {
		t.Errorf("describeFromSamples = %+v, %v", cfg2, err)
	}
	// And the access units count in samples, at the format's rate.
	timed := cfg
	timed.CodecConfig = goodDMLP()
	trueHDTiming(&timed, samples)
	if timed.Timescale != 48000 || samples[0].Duration != 40 || samples[2].Duration != 40 {
		t.Errorf("trueHDTiming: timescale %d, durations %d %d", timed.Timescale, samples[0].Duration, samples[2].Duration)
	}
	broken := cfg
	trueHDTiming(&broken, samples)
	if broken.Timescale != 1000 {
		t.Errorf("a track with no dmlp was retimed to %d", broken.Timescale)
	}
}

func TestLosslessCodecsAreNotInSamplesAndNotInATransportStream(t *testing.T) {
	for _, codec := range []string{"alac", "mlpa"} {
		if _, err := ConfigFromSamples(codec, []Sample{{Data: []byte{1}}}); !errors.Is(err, ErrNotInSamples) {
			t.Errorf("ConfigFromSamples(%s) = %v, want ErrNotInSamples", codec, err)
		}
		_, err := NewTSMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{Kind: Audio, Codec: codec, Timescale: 90000,
			Channels: 2, SampleRate: 48000, CodecConfig: goodCookie()})
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "no published specification") {
			t.Errorf("TSMuxer.AddTrack(%s) = %v, want ErrUnsupportedCodec", codec, err)
		}
	}
}

func TestMatroskaAlacWithoutACookieIsRefused(t *testing.T) {
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_ALAC", CodecPrivate: []byte{1, 2, 3},
		Audio: &mkvAudio{SamplingFrequency: 48000, Channels: 2}}
	if _, err := mkvTrackConfig(te, 1000); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "A_ALAC") {
		t.Errorf("mkvTrackConfig = %v, want ErrTrackConfig naming A_ALAC", err)
	}
	te.CodecID, te.CodecPrivate = "A_TRUEHD", []byte{9}
	cfg, err := mkvTrackConfig(te, 1000)
	if err != nil || cfg.Codec != "mlpa" || len(cfg.CodecConfig) != 0 {
		t.Errorf("A_TRUEHD read as %+v, %v; its CodecPrivate is not a dmlp", cfg, err)
	}
}

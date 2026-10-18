// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The audio fixtures were written by FFmpeg 9.0.2 from a 0.15 s sine tone, with
//
//	ffmpeg -fflags +bitexact -f lavfi -i sine=frequency=440:sample_rate=R:duration=0.15 \
//	       [-ac 2] -c:a C [-b:a B] -flags:a +bitexact -map_metadata -1 OUT
//
// for C = flac (R 44100, stereo), libmp3lame (44100 at 64k; 22050 at 32k for
// the _lsf file), mp2, ac3 and eac3 (48000 at 64k), into .mkv and .mp4. The
// codec, rate, channel count and packet count are what ffprobe reports for each.
var ffmpegAudioFixtures = []struct {
	file     string
	codec    string
	rate     int
	channels int
	packets  int
	// oti is the object type indication an MP4 states MPEG audio with, and
	// mkvID the Matroska CodecID the track is written under.
	oti   byte
	mkvID string
}{
	{"ffmpeg-flac.mkv", "fLaC", 44100, 2, 7, 0, "A_FLAC"},
	{"ffmpeg-flac.mp4", "fLaC", 44100, 2, 7, 0, "A_FLAC"},
	{"ffmpeg-mp3.mkv", "mp3", 44100, 1, 7, 0x6B, "A_MPEG/L3"},
	{"ffmpeg-mp2.mkv", "mp2", 48000, 1, 7, 0x6B, "A_MPEG/L2"},
	{"ffmpeg-ac3.mkv", "ac-3", 48000, 1, 5, 0, "A_AC3"},
	{"ffmpeg-eac3.mkv", "ec-3", 48000, 1, 5, 0, "A_EAC3"},
	{"ffmpeg-mp3.mp4", "mp3", 44100, 1, 7, 0x6B, "A_MPEG/L3"},
	{"ffmpeg-mp3_lsf.mp4", "mp3", 22050, 1, 8, 0x69, "A_MPEG/L3"},
	{"ffmpeg-mp2.mp4", "mp2", 48000, 1, 7, 0x6B, "A_MPEG/L2"},
	// The transport streams of the MPEG-TS carriage, which an MP4 used to
	// refuse.
	{"ffmpeg-mp3.ts", "mp3", 44100, 1, 9, 0x6B, "A_MPEG/L3"},
	{"ffmpeg-mp3_lsf.ts", "mp3", 22050, 1, 10, 0x69, "A_MPEG/L3"},
	{"ffmpeg-mp2.ts", "mp2", 48000, 1, 9, 0x6B, "A_MPEG/L2"},
}

// audioMuxer is what the three muxers have in common.
type audioMuxer interface {
	AddTrack(TrackConfig) (uint32, error)
	WriteSample(uint32, Sample) error
	Close() error
}

// writeAudio writes one track into a muxer and returns the file.
func writeAudio(t *testing.T, kind string, cfg TrackConfig, samples []Sample) []byte {
	t.Helper()
	var buf bytes.Buffer
	var m audioMuxer
	switch kind {
	case "fragmented":
		m = NewMuxer(&buf)
	case "progressive":
		m = NewProgressiveMuxer(&buf)
	default:
		m = NewWebMMuxer(&buf, BufferedSegment())
	}
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("%s AddTrack(%s): %v", kind, cfg.Codec, err)
	}
	for i, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("%s WriteSample(%d): %v", kind, i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("%s Close: %v", kind, err)
	}
	return buf.Bytes()
}

// TestFFmpegsAudioGoesIntoEveryMuxer reads each fixture, writes it into the
// fragmented MP4, the progressive MP4 and the Matroska muxer, and reads every
// result back: the same codec, rate and channel count, the same samples byte
// for byte, and the sample entry or track entry each mapping defines. FFmpeg
// itself decoded every one of these outputs without an error message, and
// found the same packets in them (framemd5) as in the input.
func TestFFmpegsAudioGoesIntoEveryMuxer(t *testing.T) {
	for _, fx := range ffmpegAudioFixtures {
		t.Run(fx.file, func(t *testing.T) {
			r, err := NewReader(readFixture(t, fx.file))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			cfg, err := r.TrackConfig(r.TrackIDs()[0])
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if cfg.Codec != fx.codec || cfg.SampleRate != fx.rate || cfg.Channels != fx.channels {
				t.Fatalf("read %s %d Hz %d ch, want %s %d Hz %d ch",
					cfg.Codec, cfg.SampleRate, cfg.Channels, fx.codec, fx.rate, fx.channels)
			}
			samples, err := r.Samples(r.TrackIDs()[0])
			if err != nil || len(samples) != fx.packets {
				t.Fatalf("Samples = %d, %v; want %d", len(samples), err, fx.packets)
			}
			for _, kind := range []string{"fragmented", "progressive", "matroska"} {
				out := cfg
				if (cfg.Codec == "ac-3" || cfg.Codec == "ec-3") && len(cfg.CodecConfig) == 0 && kind != "matroska" {
					// Matroska states no dac3 or dec3; the frames do.
					got, err := ConfigFromSamples(cfg.Codec, samples)
					if err != nil {
						t.Fatalf("ConfigFromSamples: %v", err)
					}
					out.CodecConfig = got.CodecConfig
				}
				data := writeAudio(t, kind, out, samples)
				checkAudioReadsBack(t, kind, data, cfg, samples)
				if kind == "matroska" {
					checkMatroskaAudioEntry(t, data, fx.mkvID, cfg)
				} else {
					checkMP4AudioEntry(t, data, fx.codec, fx.oti, cfg)
				}
			}
		})
	}
}

// checkAudioReadsBack reads a written file with this package's own reader.
func checkAudioReadsBack(t *testing.T, kind string, data []byte, cfg TrackConfig, samples []Sample) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("%s: NewReader: %v", kind, err)
	}
	got, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("%s: TrackConfig: %v", kind, err)
	}
	if got.Codec != cfg.Codec || got.SampleRate != cfg.SampleRate || got.Channels != cfg.Channels {
		t.Errorf("%s: read back %s %d Hz %d ch, want %s %d Hz %d ch", kind,
			got.Codec, got.SampleRate, got.Channels, cfg.Codec, cfg.SampleRate, cfg.Channels)
	}
	if cfg.Codec == "fLaC" && !bytes.Equal(got.CodecConfig, cfg.CodecConfig) {
		t.Errorf("%s: FLAC configuration\n got %x\nwant %x", kind, got.CodecConfig, cfg.CodecConfig)
	}
	back, err := r.Samples(r.TrackIDs()[0])
	if err != nil || len(back) != len(samples) {
		t.Fatalf("%s: Samples = %d, %v; want %d", kind, len(back), err, len(samples))
	}
	for i := range back {
		if !bytes.Equal(back[i].Data, samples[i].Data) || !back[i].Sync {
			t.Fatalf("%s: sample %d differs or is not a sync sample", kind, i)
		}
	}
}

// checkMP4AudioEntry decodes the sample entry with mp4ff and checks it against
// the mapping: mp4a with the object type and no decoder specific info for MPEG
// audio, fLaC with a dfLa whose first block is STREAMINFO for FLAC.
func checkMP4AudioEntry(t *testing.T, data []byte, codec string, oti byte, cfg TrackConfig) {
	t.Helper()
	entry, ok := sampleEntryOf(t, data).Children[0].(*mp4.AudioSampleEntryBox)
	if !ok {
		t.Fatalf("the sample entry is not an audio one")
	}
	switch codec {
	case "mp1", "mp2", "mp3":
		if entry.Type() != "mp4a" || entry.Esds == nil || entry.Esds.DecConfigDescriptor == nil {
			t.Fatalf("entry %s with esds %v, want mp4a with one", entry.Type(), entry.Esds)
		}
		dec := entry.Esds.DecConfigDescriptor
		if dec.ObjectType != oti || dec.StreamType>>2 != mp4.StreamTypeAudio || dec.DecSpecificInfo != nil {
			t.Errorf("object type 0x%02X, stream type %d, specific info %v; want 0x%02X, audio, none",
				dec.ObjectType, dec.StreamType>>2, dec.DecSpecificInfo, oti)
		}
		if int(entry.SampleRate) != cfg.SampleRate || int(entry.ChannelCount) != cfg.Channels {
			t.Errorf("entry states %d Hz %d ch", entry.SampleRate, entry.ChannelCount)
		}
	case "fLaC":
		if entry.Type() != "fLaC" || entry.DfLa == nil || len(entry.DfLa.MetadataBlocks) == 0 {
			t.Fatalf("entry %s, want fLaC with a dfLa", entry.Type())
		}
		first := entry.DfLa.MetadataBlocks[0]
		if first.BlockType != flacStreamInfo || first.Length != flacStreamInfoSize ||
			entry.DfLa.Version != 0 || entry.DfLa.Flags != 0 {
			t.Errorf("dfLa v%d flags %d begins with block %d of %d bytes",
				entry.DfLa.Version, entry.DfLa.Flags, first.BlockType, first.Length)
		}
		if entry.SampleSize != 16 || int(entry.ChannelCount) != cfg.Channels ||
			int(entry.SampleRate) != cfg.SampleRate {
			t.Errorf("entry states %d bits, %d ch, %d Hz", entry.SampleSize, entry.ChannelCount, entry.SampleRate)
		}
	default:
		if entry.Type() != codec {
			t.Errorf("entry %s, want %s", entry.Type(), codec)
		}
	}
}

// checkMatroskaAudioEntry reads the track entry back with ebml-go: the CodecID
// of the Matroska codec specifications, and a CodecPrivate only for FLAC.
func checkMatroskaAudioEntry(t *testing.T, data []byte, id string, cfg TrackConfig) {
	t.Helper()
	f := readWebM(t, data)
	if f.Header.DocType != "matroska" {
		t.Errorf("DocType %q: WebM allows none of these codecs", f.Header.DocType)
	}
	entry := f.Segment.Tracks.TrackEntry[0]
	if entry.CodecID != id {
		t.Errorf("CodecID %q, want %q", entry.CodecID, id)
	}
	want := []byte(nil)
	if id == "A_FLAC" {
		want = cfg.CodecConfig
	}
	if !bytes.Equal(entry.CodecPrivate, want) {
		t.Errorf("CodecPrivate %x, want %x", entry.CodecPrivate, want)
	}
	if entry.Audio == nil || int(entry.Audio.SamplingFrequency) != cfg.SampleRate ||
		int(entry.Audio.Channels) != cfg.Channels {
		t.Errorf("Audio %+v, want %d Hz %d ch", entry.Audio, cfg.SampleRate, cfg.Channels)
	}
}

// --- MPEG audio -------------------------------------------------------------

// mpaFrameOf is one whole frame of silence behind a header.
func mpaFrameOf(t *testing.T, header []byte) []byte {
	t.Helper()
	f, err := parseMPAFrame(header)
	if err != nil {
		t.Fatalf("parseMPAFrame: %v", err)
	}
	frame := make([]byte, f.FrameSize)
	copy(frame, header)
	return frame
}

func TestTheSampleRateChoosesTheObjectType(t *testing.T) {
	for rate, want := range map[int]byte{
		32000: 0x6B, 44100: 0x6B, 48000: 0x6B,
		16000: 0x69, 22050: 0x69, 24000: 0x69,
	} {
		if got, ok := mpaObjectType(rate); !ok || got != want {
			t.Errorf("mpaObjectType(%d) = 0x%02X, %v; want 0x%02X", rate, got, ok, want)
		}
	}
	// The "MPEG 2.5" rates belong to neither standard, and zero is the
	// reserved slot of the table, not a rate.
	for _, rate := range []int{8000, 11025, 12000, 96000, 0} {
		if _, ok := mpaObjectType(rate); ok {
			t.Errorf("mpaObjectType(%d) was given an object type", rate)
		}
	}
}

// TestLayerIGoesIntoAnMP4AndComesBackNamedByItsFrames: the esds states the
// standard and not the layer, so the reader names the track from its first
// frame, and a Layer I track written as mp4a comes back as mp1.
func TestLayerIGoesIntoAnMP4AndComesBackNamedByItsFrames(t *testing.T) {
	frame := mpaFrameOf(t, mpaHeader(1, 1, 1, 1, 0, 0)) // 32 kbit/s at 48 kHz, stereo
	samples := []Sample{{Data: frame, Duration: 384, Sync: true}, {Data: frame, Duration: 384, Sync: true}}
	cfg := TrackConfig{Kind: Audio, Codec: "mp1", Timescale: 48000, SampleRate: 48000, Channels: 2}
	for _, kind := range []string{"fragmented", "progressive", "matroska"} {
		data := writeAudio(t, kind, cfg, samples)
		checkAudioReadsBack(t, kind, data, cfg, samples)
		if kind == "matroska" {
			checkMatroskaAudioEntry(t, data, "A_MPEG/L1", cfg)
		} else {
			checkMP4AudioEntry(t, data, "mp1", 0x6B, cfg)
		}
	}
}

func TestAnMPEGAudioSampleEntryRefusesWhatItCannotState(t *testing.T) {
	for name, cfg := range map[string]TrackConfig{
		"an MPEG 2.5 rate": {Kind: Audio, Codec: "mp3", Timescale: 8000, SampleRate: 8000, Channels: 1},
		"no channel":       {Kind: Audio, Codec: "mp3", Timescale: 44100, SampleRate: 44100},
		"three channels":   {Kind: Audio, Codec: "mp2", Timescale: 44100, SampleRate: 44100, Channels: 3},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("Muxer: %v, want ErrTrackConfig", err)
			}
			if _, err := NewProgressiveMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("ProgressiveMuxer: %v, want ErrTrackConfig", err)
			}
		})
	}
}

// TestAnMP4WhoseMPEGAudioIsNotAFrameIsRefused: the reader names the track by
// its first frame, and a track whose first sample is not one -- or that has
// no sample at all -- cannot be named, which is said rather than reading it as
// AAC.
func TestAnMP4WhoseMPEGAudioIsNotAFrameIsRefused(t *testing.T) {
	cfg := TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 44100, SampleRate: 44100, Channels: 1}
	notAFrame := writeAudio(t, "progressive", cfg, []Sample{{Data: []byte("not a frame"), Duration: 1152}})
	r, err := NewReader(notAFrame)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.TrackConfig(1); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "not a frame") {
		t.Errorf("TrackConfig = %v, want ErrTrackConfig saying the sample is not a frame", err)
	}

	var empty bytes.Buffer
	m := NewMuxer(&empty)
	if _, err := m.AddTrack(cfg); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err = NewReader(empty.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.TrackConfig(1); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("TrackConfig = %v, want ErrTrackConfig saying the frame cannot be read", err)
	}
}

func TestConfigFromMPEGAudioSamples(t *testing.T) {
	for _, c := range []struct {
		file, codec, codecString string
		rate, channels           int
	}{
		{"ffmpeg-mp3.mkv", "mp3", "mp4a.6B", 44100, 1},
		{"ffmpeg-mp2.mkv", "mp2", "mp4a.6B", 48000, 1},
		{"ffmpeg-mp3_lsf.mp4", "mp3", "mp4a.69", 22050, 1},
	} {
		r, err := NewReader(readFixture(t, c.file))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		samples, err := r.Samples(r.TrackIDs()[0])
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		got, err := ConfigFromSamples(c.codec, samples, SampleTimescale(uint32(c.rate)))
		if err != nil {
			t.Fatalf("%s: ConfigFromSamples: %v", c.file, err)
		}
		if got.Kind != Audio || got.Codec != c.codec || got.SampleRate != c.rate || got.Channels != c.channels ||
			got.CodecString != c.codecString || got.Timescale != uint32(c.rate) {
			t.Errorf("%s: %+v", c.file, got)
		}
		// What it derived is what the muxer takes.
		writeAudio(t, "fragmented", got.TrackConfig, samples)
	}
}

func TestConfigFromMPEGAudioSamplesRefusesAContradiction(t *testing.T) {
	mp3 := mpaFrameOf(t, mpaHeader(1, 3, 5, 0, 0, 3))
	mp3At48 := mpaFrameOf(t, mpaHeader(1, 3, 5, 1, 0, 3))
	for name, c := range map[string]struct {
		codec   string
		samples [][]byte
		says    string
	}{
		"Layer III frames named mp2": {"mp2", [][]byte{mp3}, "Layer 3"},
		"a sample that is no frame":  {"mp3", [][]byte{mp3, []byte("ID3")}, "sample 1"},
		"a change of rate":           {"mp3", [][]byte{mp3, mp3At48}, "48000 Hz"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples(c.codec, videoSamples(c.samples...))
			if !errors.Is(err, ErrCodecMismatch) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("error = %v, want ErrCodecMismatch saying %q", err, c.says)
			}
		})
	}
}

// --- AC-3 -------------------------------------------------------------------

func TestConfigFromAC3SamplesReadsTheSyncFrame(t *testing.T) {
	for _, c := range []struct {
		file, codec string
		record      int
	}{
		{"ffmpeg-ac3.mkv", "ac-3", dac3Size},
		{"ffmpeg-eac3.mkv", "ec-3", dec3MinSize},
	} {
		r, err := NewReader(readFixture(t, c.file))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		samples, err := r.Samples(r.TrackIDs()[0])
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		// A sample that is not a sync frame is passed over, as the
		// transport stream reader passes one over.
		samples = append([]Sample{{Data: []byte{0, 1, 2}, Duration: 1}}, samples...)
		got, err := ConfigFromSamples(c.codec, samples)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if got.Codec != c.codec || got.CodecString != c.codec || len(got.CodecConfig) != c.record ||
			got.SampleRate != 48000 || got.Channels != 1 {
			t.Errorf("%s: %+v", c.file, got)
		}
	}
}

func TestConfigFromAC3SamplesRefusesTheOtherCodec(t *testing.T) {
	ac3 := ac3ClassicFrame(t, 2, false, 0, 8)
	eac3 := eac3Frame(t, 2, false, 0, 3, 256)
	for codec, frame := range map[string][]byte{"ec-3": ac3, "ac-3": eac3} {
		_, err := ConfigFromSamples(codec, videoSamples(frame))
		if !errors.Is(err, ErrCodecMismatch) || !strings.Contains(err.Error(), "bsid") {
			t.Errorf("%s: error = %v, want ErrCodecMismatch naming the bsid", codec, err)
		}
	}
}

// --- Vorbis -----------------------------------------------------------------

// TestVorbisIntoAnMP4IsRefusedWithTheReason: MP4RA registers no sample entry
// for Vorbis (data/sample-entries.csv has none), so there is no box to write,
// and the refusal says so rather than calling the codec unknown.
func TestVorbisIntoAnMP4IsRefusedWithTheReason(t *testing.T) {
	cfg := TrackConfig{Kind: Audio, Codec: "vorb", Timescale: 48000, SampleRate: 48000, Channels: 2,
		CodecConfig: webmVorbisHeaders()}
	for kind, m := range map[string]audioMuxer{
		"fragmented":  NewMuxer(&bytes.Buffer{}),
		"progressive": NewProgressiveMuxer(&bytes.Buffer{}),
	} {
		_, err := m.AddTrack(cfg)
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "MP4RA") {
			t.Errorf("%s: %v, want ErrUnsupportedCodec naming MP4RA", kind, err)
		}
	}
}

// --- FLAC -------------------------------------------------------------------

// flacBlock and streamInfo are the helpers of flac_test.go (#58), which build
// the same bytes: 4096-sample blocks, unknown frame sizes and MD5, and the
// sample count given (0 here).

// flacConfig is a whole configuration: the signature, STREAMINFO and the
// blocks given, the last of everything marked last.
func flacConfig(rate, channels, bitDepth int, more ...[]byte) []byte {
	config := append([]byte(flacSignature), flacBlock(len(more) == 0, flacStreamInfo, streamInfo(rate, channels, bitDepth, 0))...)
	for i, block := range more {
		config = append(config, flacBlock(i == len(more)-1, 4, block)...)
	}
	return config
}

func TestTheFLACConfigurationIsReadAsRFC9639StatesIt(t *testing.T) {
	blocks, format, err := parseFLACConfig(flacConfig(96000, 6, 24, []byte("comment"), nil))
	if err != nil {
		t.Fatalf("parseFLACConfig: %v", err)
	}
	if format != (flacStreamFormat{SampleRate: 96000, Channels: 6, BitDepth: 24}) || len(blocks) != 3 {
		t.Errorf("format %+v from %d blocks", format, len(blocks))
	}
	if !blocks[2].LastMetadataBlockFlag || blocks[1].LastMetadataBlockFlag || string(blocks[1].BlockData) != "comment" {
		t.Errorf("blocks %+v", blocks)
	}
	// The largest rate twenty bits state, and the deepest samples.
	if _, format, err := parseFLACConfig(flacConfig(1<<20-1, 8, 32)); err != nil ||
		format != (flacStreamFormat{SampleRate: 1<<20 - 1, Channels: 8, BitDepth: 32}) {
		t.Errorf("%+v, %v", format, err)
	}
}

func TestAFLACConfigurationThatIsNotOneIsRefused(t *testing.T) {
	good := flacConfig(44100, 2, 16)
	notLast := append([]byte(flacSignature), flacBlock(false, flacStreamInfo, streamInfo(44100, 2, 16, 0))...)
	for name, c := range map[string]struct {
		config []byte
		says   string
	}{
		"no signature":                  {good[4:], "fLaC"},
		"a bare signature":              {[]byte(flacSignature), "marked last"},
		"a header cut short":            {append(append([]byte{}, notLast...), 0x84, 0), "inside its header"},
		"a block the bytes do not hold": {good[:len(good)-1], "states 34 bytes and 33 follow"},
		"the forbidden type":            {append([]byte(flacSignature), flacBlock(true, 127, nil)...), "forbidden"},
		"something before STREAMINFO":   {append([]byte(flacSignature), flacBlock(true, 4, nil)...), "not STREAMINFO"},
		"two STREAMINFO":                {append(append([]byte{}, notLast...), flacBlock(true, 0, streamInfo(44100, 2, 16, 0))...), "second STREAMINFO"},
		"no block marked last":          {notLast, "marked last"},
		"a frame after the header":      {append(append([]byte{}, good...), 0xFF, 0xF8), "2 bytes follow"},
		"a short STREAMINFO":            {append([]byte(flacSignature), flacBlock(true, 0, make([]byte, 18))...), "is 18 bytes"},
		"a rate of zero":                {flacConfig(0, 2, 16), "not audio"},
		"three bits per sample":         {flacConfig(44100, 2, 3), "3 bits"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseFLACConfig(c.config)
			if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("error = %v, want ErrTrackConfig saying %q", err, c.says)
			}
		})
	}
}

func TestAFLACSampleEntryStatesARateItCanHold(t *testing.T) {
	for rate, want := range map[int]int{
		44100: 44100, 65535: 65535, 96000: 48000, 192000: 48000, 88200: 44100, 176400: 44100,
		// Halving leaves a fraction: the mapping says 65535.
		65537: math.MaxUint16, 131074: math.MaxUint16, 1<<20 - 1: math.MaxUint16,
	} {
		if got := flacEntryRate(rate); got != want {
			t.Errorf("flacEntryRate(%d) = %d, want %d", rate, got, want)
		}
	}
}

// TestFLACKeepsEveryMetadataBlockAndItsRate: the mapping asks for the native
// metadata to be preserved, and for the rate to be read from STREAMINFO when
// the sample entry cannot hold it -- which this package's reader does.
func TestFLACKeepsEveryMetadataBlockAndItsRate(t *testing.T) {
	config := flacConfig(96000, 2, 24, []byte("vorbis comment"), make([]byte, 8))
	cfg := TrackConfig{Kind: Audio, Codec: "fLaC", Timescale: 96000, CodecConfig: config,
		SampleRate: 96000, Channels: 2}
	samples := []Sample{{Data: []byte{0xFF, 0xF8, 1}, Duration: 4096, Sync: true}}
	for _, kind := range []string{"fragmented", "progressive", "matroska"} {
		data := writeAudio(t, kind, cfg, samples)
		checkAudioReadsBack(t, kind, data, cfg, samples)
		if kind == "matroska" {
			checkMatroskaAudioEntry(t, data, "A_FLAC", cfg)
			continue
		}
		entry := sampleEntryOf(t, data).Children[0].(*mp4.AudioSampleEntryBox)
		if entry.SampleRate != 48000 || entry.SampleSize != 24 || len(entry.DfLa.MetadataBlocks) != 3 {
			t.Errorf("%s: entry %d Hz, %d bits, %d blocks; want 48000, 24, 3", kind,
				entry.SampleRate, entry.SampleSize, len(entry.DfLa.MetadataBlocks))
		}
	}
}

func TestAFLACTrackWithoutItsConfigurationIsRefused(t *testing.T) {
	for _, config := range [][]byte{nil, []byte("fLaC")} {
		cfg := TrackConfig{Kind: Audio, Codec: "fLaC", Timescale: 44100, CodecConfig: config}
		if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("Muxer(%q): %v, want ErrTrackConfig", config, err)
		}
	}
}

// TestAMatroskaFLACTrackWhosePrivateIsNotAConfigurationIsRefused: STREAMINFO
// is what the Audio element only repeats, and a track without it is reported.
func TestAMatroskaFLACTrackWhosePrivateIsNotAConfigurationIsRefused(t *testing.T) {
	doc := webmDoc()
	doc.Segment.Tracks.TrackEntry[1].CodecID = "A_FLAC"
	doc.Segment.Tracks.TrackEntry[1].CodecPrivate = []byte("OggS")
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.TrackConfig(2); !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "A_FLAC") {
		t.Errorf("TrackConfig = %v, want ErrTrackConfig naming A_FLAC", err)
	}
}

func TestOnlyAFLACEntryWithADfLaIsReadAsOne(t *testing.T) {
	if flacSampleEntry(&mp4.StsdBox{}) != nil {
		t.Error("an empty sample description table holds a FLAC entry")
	}
	video := &mp4.StsdBox{}
	video.AddChild(mp4.CreateVisualSampleEntryBox("avc1", 16, 16, nil))
	if flacSampleEntry(video) != nil {
		t.Error("a video entry was read as FLAC")
	}
	bare := &mp4.StsdBox{}
	bare.AddChild(mp4.CreateAudioSampleEntryBox("fLaC", 2, 16, 44100, nil))
	if flacSampleEntry(bare) != nil {
		t.Error("a fLaC entry without dfLa was read as one")
	}
}

// TestADfLaThatIsNotOneTravelsAsItIs: a dfLa whose first block is not
// STREAMINFO is carried for the muxer to refuse, and the rate the sample
// entry stated is left alone, having nothing better to replace it.
func TestADfLaThatIsNotOneTravelsAsItIs(t *testing.T) {
	cfg := TrackConfig{SampleRate: 44100, Channels: 2}
	flacTrackConfig(&cfg, &mp4.DfLaBox{MetadataBlocks: []mp4.FLACMetadataBlock{
		{BlockType: 4, Length: 3, BlockData: []byte("abc")},
	}})
	want := append([]byte(flacSignature), flacBlock(true, 4, []byte("abc"))...)
	if !bytes.Equal(cfg.CodecConfig, want) || cfg.SampleRate != 44100 || cfg.Channels != 2 {
		t.Errorf("config %x at %d Hz %d ch", cfg.CodecConfig, cfg.SampleRate, cfg.Channels)
	}
	if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{Kind: Audio, Codec: "fLaC",
		Timescale: 44100, CodecConfig: cfg.CodecConfig}); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("AddTrack: %v, want ErrTrackConfig", err)
	}
}

// FuzzParseFLACConfig: whatever the bytes, the parser returns rather than
// panics, and a configuration it accepts is one it can write back exactly.
func FuzzParseFLACConfig(f *testing.F) {
	f.Add(flacConfig(44100, 2, 16))
	f.Add(flacConfig(96000, 6, 24, []byte("comment"), nil))
	f.Add([]byte("fLaC\x80\x00\x00\x22"))
	f.Fuzz(func(t *testing.T, config []byte) {
		blocks, format, err := parseFLACConfig(config)
		if err != nil {
			return
		}
		var cfg TrackConfig
		flacTrackConfig(&cfg, &mp4.DfLaBox{MetadataBlocks: blocks})
		if !bytes.Equal(cfg.CodecConfig, config) {
			t.Fatalf("written back as %x, read from %x", cfg.CodecConfig, config)
		}
		if format.SampleRate <= 0 || format.Channels < 1 || format.Channels > 8 || format.BitDepth < 4 {
			t.Fatalf("format %+v", format)
		}
	})
}

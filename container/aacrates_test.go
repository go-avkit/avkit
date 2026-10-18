// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/at-wat/ebml-go"
)

// audioFile is a written file's audio elements read back with ebml-go.
type audioFile struct {
	Segment struct {
		Tracks struct {
			TrackEntry []struct {
				Audio *mkvAudio `ebml:"Audio"`
			} `ebml:"TrackEntry"`
		} `ebml:"Tracks"`
	} `ebml:"Segment"`
}

// audioElement is the first track's Audio element of a Matroska file.
func audioElement(t *testing.T, data []byte) mkvAudio {
	t.Helper()
	var f audioFile
	if err := ebml.Unmarshal(bytes.NewReader(data), &f, ebml.WithIgnoreUnknown(true)); err != nil {
		t.Fatal(err)
	}
	return *f.Segment.Tracks.TrackEntry[0].Audio
}

// hierarchicalSBR is an HE-AAC record signalled the hierarchical way: object
// type 5, the core at 24 kHz and the output at 48 kHz.
func hierarchicalSBR(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	asc := aac.AudioSpecificConfig{ObjectType: aac.HEAACv1, ChannelConfiguration: 2,
		SamplingFrequency: 24000, ExtensionFrequency: 48000}
	if err := asc.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// aacRates reads the rates ffmpeg's matroskaenc writes, the way its
// ff_mpeg4audio_get_config_gb reads them.
func TestAACRates(t *testing.T) {
	for name, tc := range map[string]struct {
		asc          []byte
		core, output int
	}{
		// ffmpeg's own AAC-LC encoder: a sync extension stating no SBR.
		"ffmpeg's AAC-LC": {[]byte{0x11, 0x90, 0x56, 0xe5, 0x00}, 48000, 0},
		// Apple's HE-AAC, as aac_at writes it: AAC-LC at 22050, then a sync
		// extension stating SBR at 44100.
		"backward-compatible SBR": {[]byte{0x13, 0x90, 0x56, 0xe5, 0xa0}, 22050, 44100},
		"hierarchical SBR":        {hierarchicalSBR(t), 24000, 48000},
		"plain AAC-LC":            {[]byte{0x12, 0x10}, 44100, 0},
		// A sync extension stating the core's own rate still states it.
		"an extension at the core rate": {[]byte{0x13, 0x90, 0x56, 0xe5, 0xb8}, 22050, 22050},
		// 15 is an explicit 24-bit frequency: 0x003E80 is 16000 Hz.
		"an explicit frequency": {[]byte{0x17, 0x80, 0x1f, 0x40, 0x10}, 16000, 0},
		// A reserved extension index states nothing.
		"a reserved extension index": {[]byte{0x13, 0x90, 0x56, 0xe5, 0xe8}, 22050, 0},
		// An escaped extension object type is not SBR.
		"an escaped extension": {[]byte{0x13, 0x90, 0x56, 0xff, 0xe0}, 22050, 0},
		// A core with no frameLengthFlag -- object type 6 -- is searched from
		// the bit after channelConfiguration, and finds the same extension.
		"a core with no frameLengthFlag": {[]byte{0x33, 0x90, 0x56, 0xe5, 0xa0}, 22050, 44100},
		// A record ending before the sbrPresentFlag reads it as zero.
		"a record ending before the SBR flag": {[]byte{0x13, 0x90, 0x56, 0xe5}, 22050, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if core, output := aacRates(tc.asc); core != tc.core || output != tc.output {
				t.Errorf("aacRates(% x) = %d, %d; want %d, %d", tc.asc, core, output, tc.core, tc.output)
			}
		})
	}
}

// What L11 found: WebMMuxer wrote an AAC track's SamplingFrequency as the
// track's rate, so an HE-AAC MP4 -- 44.1 kHz audio coded as 22.05 kHz AAC and
// spectral band replication -- went to Matroska as 44100 with no output rate,
// where ffmpeg's copy states the record's own: 22050, and 44100 as
// OutputSamplingFrequency. The fixtures are aac_at's HE-AAC and ffmpeg 9.0.2's
// -c copy of it.
func TestAnHEAACTrackStatesBothRatesInMatroska(t *testing.T) {
	src, err := os.ReadFile("testdata/ffmpeg-heaac.mp4")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile("testdata/ffmpeg-heaac.mkv")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	copyInto(t, NewWebMMuxer(&out, BufferedSegment()), r)
	got, want := audioElement(t, out.Bytes()), audioElement(t, ref)
	if want.SamplingFrequency != 22050 || want.OutputSamplingFrequency != 44100 {
		t.Fatalf("ffmpeg's copy states %v and %v; the fixture is not what the test needs",
			want.SamplingFrequency, want.OutputSamplingFrequency)
	}
	if got.SamplingFrequency != want.SamplingFrequency || got.OutputSamplingFrequency != want.OutputSamplingFrequency {
		t.Errorf("wrote %v and %v; ffmpeg writes %v and %v", got.SamplingFrequency, got.OutputSamplingFrequency,
			want.SamplingFrequency, want.OutputSamplingFrequency)
	}
	// And read back, both files are 44.1 kHz audio, as the MP4 said.
	for name, data := range map[string][]byte{"ours": out.Bytes(), "ffmpeg's": ref} {
		r, err := NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := r.TrackConfig(r.TrackIDs()[0])
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SampleRate != 44100 || r.File().Tracks[0].SampleRate != 44100 {
			t.Errorf("%s read back at %d Hz (demuxed %d); want 44100", name, cfg.SampleRate, r.File().Tracks[0].SampleRate)
		}
	}
}

// An AAC-LC track states one rate, its own, and no output rate; so does a
// track whose record is built from its fields.
func TestAnAACLCTrackStatesOneRate(t *testing.T) {
	for name, cfg := range map[string]TrackConfig{
		"its record":   {Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2, CodecConfig: []byte{0x11, 0x90, 0x56, 0xe5, 0x00}},
		"built":        {Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2},
		"built HE-AAC": {Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2, AudioObjectType: aac.HEAACv1},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			m := NewWebMMuxer(&out, BufferedSegment())
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.WriteSample(id, Sample{Data: []byte{1, 2}, Duration: 1024, Sync: true}); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			a := audioElement(t, out.Bytes())
			// A built HE-AAC record states its extension as the track's own
			// rate, hierarchically, and matroskaenc writes that as the output.
			wantOut := 0.0
			if cfg.AudioObjectType == aac.HEAACv1 {
				wantOut = 48000
			}
			if a.SamplingFrequency != 48000 || a.OutputSamplingFrequency != wantOut {
				t.Errorf("wrote %v and %v; want 48000 and %v", a.SamplingFrequency, a.OutputSamplingFrequency, wantOut)
			}
		})
	}
}

// The Matroska reader takes the rate a track decodes to, OutputSamplingFrequency
// where it is stated, as matroskadec does.
func TestMkvAudioRate(t *testing.T) {
	for _, tc := range []struct {
		a    mkvAudio
		want int
	}{
		{mkvAudio{SamplingFrequency: 22050, OutputSamplingFrequency: 44100}, 44100},
		{mkvAudio{SamplingFrequency: 48000}, 48000},
	} {
		if got := tc.a.rate(); got != tc.want {
			t.Errorf("%+v rate = %d; want %d", tc.a, got, tc.want)
		}
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"testing"
)

// The tests in this file hold what the lots of #54 do TOGETHER: each lot is
// tested on its own branch, and these are the paths where one lot's reader
// feeds another lot's writer.

// sameSamples reports the first sample whose bytes differ, or whose start
// differs by more than one tick of the output's timescale (Matroska counts
// milliseconds, so 1152/44100 s comes back as 26 or 27 of them), or -1.
func sameSamples(t *testing.T, in, out []Sample, inScale, outScale uint32) int {
	t.Helper()
	if len(in) != len(out) {
		t.Errorf("%d samples in, %d out", len(in), len(out))
		return 0
	}
	var inAt, outAt uint64 // in units of 1/(inScale*outScale) s
	for i := range in {
		d := int64(inAt) - int64(outAt)
		if !bytes.Equal(in[i].Data, out[i].Data) || d >= int64(inScale) || -d >= int64(inScale) {
			return i
		}
		inAt += uint64(in[i].Duration) * uint64(outScale)
		outAt += uint64(out[i].Duration) * uint64(inScale)
	}
	return -1
}

// TestElementaryAudioRemuxesToMP4AndMatroska: the elementary demuxers (#58,
// #59) and AVI (#64) read FLAC, MPEG audio and AAC; the audio codecs lot (#61)
// writes FLAC and MPEG audio into an MP4 and into Matroska. Every track comes
// back with the same codec, format and sample bytes.
func TestElementaryAudioRemuxesToMP4AndMatroska(t *testing.T) {
	for _, file := range []string{"sine.flac", "sine.oga", "sine.mp3", "sine.mp2"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + file)
			if err != nil {
				t.Fatal(err)
			}
			r, err := NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := r.TrackConfig(1)
			if err != nil {
				t.Fatal(err)
			}
			in, err := r.Samples(1)
			if err != nil {
				t.Fatal(err)
			}

			var mp4Out bytes.Buffer
			if err := Remux(&mp4Out, r); err != nil {
				t.Fatalf("into MP4: %v", err)
			}
			checkBack(t, "MP4", mp4Out.Bytes(), cfg, in)

			var mkv bytes.Buffer
			m := NewWebMMuxer(&mkv)
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatalf("into Matroska: %v", err)
			}
			for _, s := range in {
				if err := m.WriteSample(id, s); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			checkBack(t, "Matroska", mkv.Bytes(), cfg, in)
		})
	}
}

// TestASubtitleTrackWithAnEditListConcatenates: subtitles (#56) pad a WebVTT
// track to the end of its input before the next one follows, and general edit
// lists (#62) drop a list that only says where the presentation starts, after
// checking that it covers the whole media. The list is checked against the
// samples the source holds, before padding: checked after, the padding would
// look like media the list leaves out, and the concatenation would be refused.
func TestASubtitleTrackWithAnEditListConcatenates(t *testing.T) {
	hi := cueSample(t, vttc("", "", "hi"))
	r := fixtureReader(t,
		fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 10, 100, 5)},
		fixtureTrack{cfg: TrackConfig{Kind: Subtitle, Codec: "wvtt", Timescale: 1000,
			EditList: &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 300, MediaTime: 0, Rate: NormalRate}}}},
			samples: []Sample{{Data: hi, Duration: 300, Sync: true}}})
	if cfg, _ := r.TrackConfig(2); cfg.EditList == nil {
		t.Fatal("the fixture's subtitle track lost its edit list; the test would test nothing")
	}
	var buf bytes.Buffer
	if err := Concat(&buf, []*Reader{r, r}); err != nil {
		t.Fatalf("Concat: %v", err)
	}
	_, samples := readBack(t, buf.Bytes())
	assertShown(t, "concat", shown(t, samples[1]), []shownSample{
		{duration: 300, cues: []shownCue{{text: "hi"}}}, {duration: 700},
		{duration: 300, cues: []shownCue{{text: "hi"}}},
	})
}

// checkBack reads a written file back and compares its one track with the
// source's.
func checkBack(t *testing.T, what string, data []byte, cfg TrackConfig, in []Sample) {
	t.Helper()
	back, err := NewReader(data)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	ids := back.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("%s: %d tracks", what, len(ids))
	}
	got, err := back.TrackConfig(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Codec != cfg.Codec || got.Channels != cfg.Channels || got.SampleRate != cfg.SampleRate {
		t.Errorf("%s: %s %d ch %d Hz, the source %s %d ch %d Hz", what,
			got.Codec, got.Channels, got.SampleRate, cfg.Codec, cfg.Channels, cfg.SampleRate)
	}
	if cfg.Codec == "fLaC" && !bytes.Equal(got.CodecConfig, cfg.CodecConfig) {
		t.Errorf("%s: FLAC configuration % x, the source % x", what, got.CodecConfig, cfg.CodecConfig)
	}
	out, err := back.Samples(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if i := sameSamples(t, in, out, cfg.Timescale, got.Timescale); i >= 0 {
		t.Errorf("%s: sample %d differs: %d bytes lasting %d/%d, the source's %d bytes lasting %d/%d",
			what, i, len(out[i].Data), out[i].Duration, got.Timescale,
			len(in[i].Data), in[i].Duration, cfg.Timescale)
	}
}

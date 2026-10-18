// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests in this file hold what the lots of #54 do TOGETHER: each lot is
// tested on its own branch, and these are the paths where one lot's reader
// feeds another lot's writer.

// keepForOracle writes an output where ffprobe can be pointed at it, when
// AVKIT_ORACLE_DIR names a directory: the reference tool is not on every CI
// runner, so the comparison is made by hand and reported in the pull request.
func keepForOracle(t *testing.T, name string, data []byte) {
	t.Helper()
	if dir := os.Getenv("AVKIT_ORACLE_DIR"); dir != "" {
		if err := os.WriteFile(dir+"/"+name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// firstDifferent reports the first sample whose bytes differ, or whose start
// differs by more than one tick of the output's timescale (Matroska counts
// milliseconds, so 1152/44100 s comes back as 26 or 27 of them), or -1.
func firstDifferent(t *testing.T, in, out []Sample, inScale, outScale uint32) int {
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

// TestAnIndexedFileCarriesBFramesAndSubtitles: the B-frame edit list of #60,
// the general edit lists of #62, the WebVTT track of #56 and the segment index
// of #63, all in one fragmented file. The index has one sidx per track, the
// text track's included; every box tiles the same fragments; the samples read
// back as they do without the index; and the pictures are still presented
// from zero.
func TestAnIndexedFileCarriesBFramesAndSubtitles(t *testing.T) {
	video, _ := openFixture(t, "bframes.mp4")
	text := subtitleFixture(t, "subtitles-webvtt.mkv")
	var buf bytes.Buffer
	if err := Join(&buf, []*Reader{video, text}, MuxOptions(SegmentIndex(), FragmentDuration(time.Second))); err != nil {
		t.Fatalf("Join: %v", err)
	}
	data := buf.Bytes()
	keepForOracle(t, "indexed-bframes-subtitles.mp4", data)
	cfgs, samples := readBack(t, data)
	if len(cfgs) != 3 || cfgs[2].Codec != "wvtt" {
		t.Fatalf("tracks = %+v, want the source's two and a wvtt", cfgs)
	}
	assertShown(t, "indexed", shown(t, samples[2]), threeCues)
	if got := firstPresented(t, data, 0); got != 0 {
		t.Errorf("the first picture is shown at %d ticks; want 0", got)
	}
	if samples[0][0].CompositionOffset <= 0 {
		t.Fatal("the first picture is not reordered; the test would test nothing")
	}

	boxes := topBoxes(t, data)
	starts, sizes := fragmentRanges(t, boxes)
	index, ends := sidxBoxes(boxes)
	if len(index) != 3 {
		t.Fatalf("%d sidx boxes, want one per track", len(index))
	}
	for k, sidx := range index {
		if sidx.ReferenceID != uint32(k+1) || len(sidx.SidxRefs) != len(starts) {
			t.Fatalf("sidx %d indexes track %d with %d references for %d fragments",
				k, sidx.ReferenceID, len(sidx.SidxRefs), len(starts))
		}
		at := ends[k] + sidx.FirstOffset
		for i, ref := range sidx.SidxRefs {
			if at != starts[i] || uint64(ref.ReferencedSize) != sizes[i] {
				t.Errorf("sidx %d reference %d: %d bytes at %d, the fragment is %d at %d",
					k, i, ref.ReferencedSize, at, sizes[i], starts[i])
			}
			at += uint64(ref.ReferencedSize)
		}
	}
	// The text track's index adds up to the 4 s its cues cover.
	var total uint64
	for _, ref := range index[2].SidxRefs {
		total += uint64(ref.SubSegmentDuration)
	}
	if total != 4000 {
		t.Errorf("the WebVTT index lasts %d ms, the cues 4000", total)
	}
	sameSamples(t, readAllSamples(t, data), readAllSamples(t, withoutIndex(t, data)))
}

// TestTheSeekHeadNamesTheTagsAndTheCover: metadata (#67) writes Tags and an
// Attachments element before the first cluster; the SeekHead (#66) names
// every Top-Level Element written there. Both modes that carry a SeekHead --
// buffered, and streamed to a writer that seeks -- name them at the position
// they really have, and the metadata reads back.
func TestTheSeekHeadNamesTheTagsAndTheCover(t *testing.T) {
	want := fullMetadata(t)
	avc, err := NewReader(readFixture(t, "metadata-ffmpeg.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	buffered := copyToWebM(t, avc, want, BufferedSegment())

	sink := newOffsetSink(100)
	m := NewWebMMuxer(sink)
	if err := m.SetMetadata(want); err != nil {
		t.Fatal(err)
	}
	copyTracks(t, avc, m)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	for what, data := range map[string][]byte{"buffered": buffered, "streamed to a seeker": sink.data} {
		keepForOracle(t, "seekhead-metadata-"+strings.ReplaceAll(what, " ", "-")+".mkv", data)
		checkSeekHead(t, data, true)
		seeks := seekHeadOf(t, data)
		for name, id := range map[string]uint64{"Tags": 0x1254C367, "Attachments": 0x1941A469, "Cues": 0x1C53BB6B} {
			if _, ok := seeks[id]; !ok {
				t.Errorf("%s: the SeekHead does not name the %s", what, name)
			}
		}
		f, err := Demux(data)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		assertMetadata(t, what, f.Metadata, want)
	}
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
	if i := firstDifferent(t, in, out, cfg.Timescale, got.Timescale); i >= 0 {
		t.Errorf("%s: sample %d differs: %d bytes lasting %d/%d, the source's %d bytes lasting %d/%d",
			what, i, len(out[i].Data), out[i].Duration, got.Timescale,
			len(in[i].Data), in[i].Duration, cfg.Timescale)
	}
}

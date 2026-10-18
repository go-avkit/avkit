// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"slices"
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
	for _, file := range []string{"sine.flac", "sine.oga", "sine.mp3", "sine.mp2", "sine.aac"} {
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
			keepForOracle(t, "elementary-"+file+".mp4", mp4Out.Bytes())
			checkBack(t, "MP4", mp4Out.Bytes(), cfg, in)

			var progressive bytes.Buffer
			if err := JoinProgressive(&progressive, []*Reader{r}); err != nil {
				t.Fatalf("into a progressive MP4: %v", err)
			}
			keepForOracle(t, "elementary-"+file+".progressive.mp4", progressive.Bytes())
			checkBack(t, "MP4 progressive", progressive.Bytes(), cfg, in)

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
			keepForOracle(t, "elementary-"+file+".mkv", mkv.Bytes())
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
	// #76: the stream to a seeker is completed at Close with its Duration and
	// its sizes, Info rewritten in place behind a SeekHead and before the
	// Tags and the cover -- and comes out stating what the buffered file does.
	streamed, held := readWebM(t, sink.data).Segment.Info, readWebM(t, buffered).Segment.Info
	if streamed.Duration == 0 || streamed.Duration != held.Duration {
		t.Errorf("streamed Info: duration %v; buffered %v", streamed.Duration, held.Duration)
	}
	// (The title rewritten with the Info is checked by the metadata read
	// above, from Info/Title.)
	if got := segmentSizeBytes(t, sink.data); bytes.Equal(got, unknownSize) {
		t.Error("the segment streamed to a seeker still states an unknown size")
	}
}

// TestASubtitleStreamToASeekerIsCompletedAtClose: WebVTT cues (#56) are
// written as block groups once their end is known, the last of them at
// Close; #76 then rewrites every cluster's size from where the Cues say the
// clusters start, the Duration and the segment's size, and #66 the SeekHead.
// The completed file reads back, cue for cue, with ebml-go's strict reader,
// and lasts as long as its cues.
func TestASubtitleStreamToASeekerIsCompletedAtClose(t *testing.T) {
	text, err := subtitleFixture(t, "subtitles-webvtt.mkv").Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	r := subtitleClip(t, text...)
	sink := newOffsetSink(0)
	copyInto(t, NewWebMMuxer(sink, ClusterDuration(200*time.Millisecond)), r)
	keepForOracle(t, "subtitles-seekable.mkv", sink.data)
	checkSeekHead(t, sink.data, true)
	if got := segmentSizeBytes(t, sink.data); bytes.Equal(got, unknownSize) {
		t.Error("the segment still states an unknown size")
	}
	if d := readWebM(t, sink.data).Segment.Info.Duration; d != 4000 {
		t.Errorf("Duration %v ms, the cues last 4000", d)
	}
	back, err := NewReader(sink.data)
	if err != nil {
		t.Fatal(err)
	}
	cues, err := back.Samples(back.TrackIDs()[1])
	if err != nil {
		t.Fatal(err)
	}
	assertShown(t, "seekable stream", shown(t, cues), threeCues)
}

// TestAMatroskaCopyStatesThePrimingOnceBesideTheBFrameShift: #60 takes the
// first composition offset off a reordering track's blocks and turns an MP4
// edit list into a shift (#62); #71 writes an audio track's StartDelay as
// CodecDelay. From ffmpeg's bframes.mp4 -- whose AAC priming is a one-edit
// list, read as StartDelay -- the copy must present the pictures from zero and
// state the priming once: as CodecDelay, with the audio blocks left where they
// were, not shifted as well.
func TestAMatroskaCopyStatesThePrimingOnceBesideTheBFrameShift(t *testing.T) {
	r, _ := openFixture(t, "bframes.mp4")
	audioCfg, err := r.TrackConfig(r.TrackIDs()[1])
	if err != nil {
		t.Fatal(err)
	}
	if audioCfg.StartDelay <= 0 || audioCfg.EditList == nil {
		t.Fatalf("the fixture's audio states StartDelay %v, edit list %v; the test would test nothing",
			audioCfg.StartDelay, audioCfg.EditList)
	}
	var out bytes.Buffer
	copyInto(t, NewWebMMuxer(&out, BufferedSegment()), r)
	keepForOracle(t, "bframes-codecdelay.mkv", out.Bytes())
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.TrackConfig(back.TrackIDs()[1])
	if err != nil {
		t.Fatal(err)
	}
	// CodecDelay counts nanoseconds and the source's edit list ticks of
	// 1/48000 s: the same priming, to the nanosecond it is rounded to.
	if d := got.StartDelay - audioCfg.StartDelay; d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("the copy states a priming of %v, the source %v", got.StartDelay, audioCfg.StartDelay)
	}
	f := readWebM(t, out.Bytes())
	ticks := blockTicks(f, 2)
	if len(ticks) == 0 || ticks[0] != 0 {
		t.Errorf("the first audio block is at %v ticks; want 0, the priming frame where it was", ticks[:min(1, len(ticks))])
	}
	if video := blockTicks(f, 1); len(video) == 0 || slices.Min(video) != 0 {
		t.Errorf("the first picture is shown at %v; want 0", video)
	}
}

// TestATransportStreamCopyOfFFmpegsBFrameFilesStartsTogether: #72 takes each
// track's first composition offset and its StartDelay off every timestamp. An
// MP4 source needs #60 for that: before it, the reader took ffmpeg's B-frame
// edit list for an 80 ms video priming, which #72 would have taken off as
// well. From both of ffmpeg's B-frame files the first picture and the first
// sound after the AAC priming are shown at the program's start.
func TestATransportStreamCopyOfFFmpegsBFrameFilesStartsTogether(t *testing.T) {
	for _, source := range []string{"bframes.mp4", "bframes.mkv"} {
		t.Run(source, func(t *testing.T) {
			r, _ := openFixture(t, source)
			vcfg, _ := r.TrackConfig(r.TrackIDs()[0])
			acfg, _ := r.TrackConfig(r.TrackIDs()[1])
			if vcfg.StartDelay != 0 || acfg.StartDelay <= 0 {
				t.Fatalf("video StartDelay %v, audio %v; want none and the AAC priming", vcfg.StartDelay, acfg.StartDelay)
			}
			var out bytes.Buffer
			copyInto(t, NewTSMuxer(&out), r)
			keepForOracle(t, source+".ts", out.Bytes())
			back, err := NewReader(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			pids := back.TrackIDs() // a transport stream's tracks are its PIDs
			video := tsUnitsOfStream(t, out.Bytes(), uint16(pids[0]))
			audio := tsUnitsOfStream(t, out.Bytes(), uint16(pids[1]))
			if len(video) == 0 || len(audio) == 0 {
				t.Fatalf("%d video and %d audio units", len(video), len(audio))
			}
			earliest := video[0].PES.Header.OptionalHeader.PTS.Base
			for _, u := range video {
				earliest = min(earliest, u.PES.Header.OptionalHeader.PTS.Base)
			}
			if earliest != tsTimestampOffset {
				t.Errorf("the first picture is shown at %d, want the program's start %d", earliest, tsTimestampOffset)
			}
			// The priming frame first, then the media proper at the start. The
			// priming is counted in the track's own ticks (a Matroska track's
			// are milliseconds, so 21.333 ms is 21 of them), as #72 counts it.
			priming := tsPrimingTicks(acfg.StartDelay, acfg.Timescale) * 90000 / int64(acfg.Timescale)
			if got := audio[0].PES.Header.OptionalHeader.PTS.Base; got < tsTimestampOffset-priming-1 || got > tsTimestampOffset-priming+1 {
				t.Errorf("the priming frame is shown at %d, want %d", got, tsTimestampOffset-priming)
			}
		})
	}
}

// TestAVIAudioRemuxesToMP4AndMatroska: the AVI reader (#64) reads AC-3 and MP3
// out of an AVI; AC-3's configuration is derived from its frames
// (describeFromSamples, #61's ac3StreamConfig) and MP3 goes into an MP4 as
// mp4a (#61). H.264 + AC-3 copies whole into both; Xvid has no MP4 muxer
// here, so its MP3 is copied alone.
func TestAVIAudioRemuxesToMP4AndMatroska(t *testing.T) {
	for _, c := range []struct {
		file   string
		codecs []string
		drop   bool // the video, which no muxer writes
	}{
		{"testsrc_h264_ac3.avi", []string{"avc1", "ac-3"}, false},
		{"testsrc_xvid_mp3.avi", []string{"mp3"}, true},
	} {
		t.Run(c.file, func(t *testing.T) {
			r, err := NewReader(readFixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			var opts []RemuxOption
			if c.drop {
				opts = append(opts, DropTracks(r.TrackIDs()[0]))
			}
			var mp4Out bytes.Buffer
			if err := Remux(&mp4Out, r, opts...); err != nil {
				t.Fatalf("into MP4: %v", err)
			}
			keepForOracle(t, c.file+".mp4", mp4Out.Bytes())
			cfgs, _ := readBack(t, mp4Out.Bytes())
			if len(cfgs) != len(c.codecs) {
				t.Fatalf("%d tracks, want %v", len(cfgs), c.codecs)
			}
			for i, cfg := range cfgs {
				if cfg.Codec != c.codecs[i] {
					t.Errorf("MP4 track %d is %s, want %s", i+1, cfg.Codec, c.codecs[i])
				}
			}
			if c.drop {
				return
			}
			var mkv bytes.Buffer
			copyInto(t, NewWebMMuxer(&mkv), r)
			keepForOracle(t, c.file+".mkv", mkv.Bytes())
			cfgs, _ = readBack(t, mkv.Bytes())
			for i, cfg := range cfgs {
				if cfg.Codec != c.codecs[i] {
					t.Errorf("Matroska track %d is %s, want %s", i+1, cfg.Codec, c.codecs[i])
				}
			}
		})
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
	// FLAC's STREAMINFO and AAC's AudioSpecificConfig travel as they are:
	// #68 writes the AAC one verbatim into an MP4, #79 into Matroska.
	if (cfg.Codec == "fLaC" || cfg.Codec == "mp4a") && !bytes.Equal(got.CodecConfig, cfg.CodecConfig) {
		t.Errorf("%s: %s configuration % x, the source % x", what, cfg.Codec, got.CodecConfig, cfg.CodecConfig)
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

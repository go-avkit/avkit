// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The oracle is ffmpeg (hls muxer, -hls_segment_type fmp4; dash muxer), run once
// on testdata/segment/source.mp4; its output is committed under
// testdata/segment/{hls,dash} so CI does not need ffmpeg.

func segFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "segment", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func segmentSource(t *testing.T, name string) *Reader {
	t.Helper()
	r, err := NewReader(segFile(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// fragSpan is where a media segment starts and how long it lasts, as its moof says.
func fragSpan(t *testing.T, init, seg []byte) (start, dur uint64) {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(append(bytes.Clone(init), seg...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Segments) != 1 || len(f.Segments[0].Fragments) != 1 {
		t.Fatalf("want one fragment, got %d segments", len(f.Segments))
	}
	traf := f.Segments[0].Fragments[0].Moof.Traf
	for _, s := range traf.Trun.Samples {
		d := uint64(s.Dur)
		if d == 0 {
			d = uint64(traf.Tfhd.DefaultSampleDuration)
		}
		dur += d
	}
	return traf.Tfdt.BaseMediaDecodeTime(), dur
}

func segTrak(t *testing.T, init []byte) *mp4.TrakBox {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Moov.Traks) != 1 {
		t.Fatalf("init has %d tracks", len(f.Moov.Traks))
	}
	return f.Moov.Traks[0]
}

func segBoxBytes(t *testing.T, b mp4.Box) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSegmentMatchesFFmpegHLSVideoSegmentForSegment(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	video, audio := &seg.Tracks[0], &seg.Tracks[1]
	if video.Name != "video1" || audio.Name != "audio1" {
		t.Fatalf("names %q %q", video.Name, audio.Name)
	}
	ref := segFile(t, "hls/init_video.mp4")
	if len(video.Segments) != 4 {
		t.Fatalf("%d video segments, ffmpeg wrote 4", len(video.Segments))
	}
	for i, s := range video.Segments {
		wantStart, wantDur := fragSpan(t, ref, segFile(t, "hls/seg_video_00"+string(rune('0'+i))+".m4s"))
		gotStart, gotDur := fragSpan(t, video.Init, s.Data)
		if gotStart != wantStart || gotDur != wantDur || s.Start != wantStart || s.Duration != wantDur {
			t.Errorf("video segment %d: start %d dur %d (struct %d, %d), ffmpeg %d, %d",
				i, gotStart, gotDur, s.Start, s.Duration, wantStart, wantDur)
		}
	}
	// The audio is cut at the first frame at or after each instant, as ffmpeg
	// cuts its first three; ffmpeg ends with a one-frame segment of its own.
	aref := segFile(t, "hls/init_audio.mp4")
	for i := 0; i < 3; i++ {
		wantStart, wantDur := fragSpan(t, aref, segFile(t, "hls/seg_audio_00"+string(rune('0'+i))+".m4s"))
		gotStart, gotDur := fragSpan(t, audio.Init, audio.Segments[i].Data)
		if gotStart != wantStart || gotDur != wantDur {
			t.Errorf("audio segment %d: start %d dur %d, ffmpeg %d, %d", i, gotStart, gotDur, wantStart, wantDur)
		}
	}
	var total uint64
	for _, s := range audio.Segments {
		total += s.Duration
	}
	var want uint64
	for i := 0; i < 5; i++ {
		_, d := fragSpan(t, aref, segFile(t, "hls/seg_audio_00"+string(rune('0'+i))+".m4s"))
		want += d
	}
	if total != want {
		t.Errorf("audio covers %d ticks, ffmpeg's covers %d", total, want)
	}
}

// fragSamples is how many samples a media segment holds, as its trun says.
func fragSamples(t *testing.T, init, seg []byte) int {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(append(bytes.Clone(init), seg...)))
	if err != nil {
		t.Fatal(err)
	}
	return len(f.Segments[0].Fragments[0].Moof.Traf.Trun.Samples)
}

func TestSegmentKeepsEverySampleOfEveryTrack(t *testing.T) {
	r := segmentSource(t, "source.mp4")
	seg, err := Segment(r, SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range r.TrackIDs() {
		src, err := r.Samples(id)
		if err != nil {
			t.Fatal(err)
		}
		var wantDur uint64
		for _, s := range src {
			wantDur += uint64(s.Duration)
		}
		var gotDur uint64
		var gotN int
		tr := seg.Tracks[i]
		for _, s := range tr.Segments {
			gotDur += s.Duration
			gotN += fragSamples(t, tr.Init, s.Data)
		}
		if gotN != len(src) || gotDur != wantDur {
			t.Errorf("%s: %d samples / %d ticks in the segments, %d / %d in the source", tr.Name, gotN, gotDur, len(src), wantDur)
		}
	}
	// ffmpeg's five audio segments cover the same ticks as the source (the
	// last, one frame long, is its flush of a frame it had held back).
	aref := segFile(t, "hls/init_audio.mp4")
	var ffTotal, ffN uint64
	for i := 0; i < 5; i++ {
		s := segFile(t, "hls/seg_audio_00"+string(rune('0'+i))+".m4s")
		_, d := fragSpan(t, aref, s)
		ffTotal += d
		ffN += uint64(fragSamples(t, aref, s))
	}
	var ours uint64
	var oursN int
	for _, s := range seg.Tracks[1].Segments {
		ours += s.Duration
		oursN += fragSamples(t, seg.Tracks[1].Init, s.Data)
	}
	if ours != ffTotal || uint64(oursN) != ffN {
		t.Errorf("audio: %d samples / %d ticks, ffmpeg %d / %d", oursN, ours, ffN, ffTotal)
	}
}

func TestSegmentInitSegmentsNameTheSameTrackBoxesAsFFmpeg(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range []string{"hls/init_video.mp4", "hls/init_audio.mp4"} {
		got, want := segTrak(t, seg.Tracks[i].Init), segTrak(t, segFile(t, ref))
		if got.Mdia.Hdlr.HandlerType != want.Mdia.Hdlr.HandlerType ||
			got.Mdia.Mdhd.Timescale != want.Mdia.Mdhd.Timescale {
			t.Errorf("track %d: handler/timescale %s/%d, ffmpeg %s/%d", i, got.Mdia.Hdlr.HandlerType,
				got.Mdia.Mdhd.Timescale, want.Mdia.Hdlr.HandlerType, want.Mdia.Mdhd.Timescale)
		}
		ge, we := got.Mdia.Minf.Stbl.Stsd.Children[0], want.Mdia.Minf.Stbl.Stsd.Children[0]
		if ge.Type() != we.Type() {
			t.Errorf("track %d: sample entry %s, ffmpeg %s", i, ge.Type(), we.Type())
		}
		switch e := ge.(type) {
		case *mp4.VisualSampleEntryBox:
			w := we.(*mp4.VisualSampleEntryBox)
			if e.Width != w.Width || e.Height != w.Height || !bytes.Equal(segBoxBytes(t, e.AvcC), segBoxBytes(t, w.AvcC)) {
				t.Errorf("video entry differs from ffmpeg's: %dx%d vs %dx%d", e.Width, e.Height, w.Width, w.Height)
			}
		case *mp4.AudioSampleEntryBox:
			w := we.(*mp4.AudioSampleEntryBox)
			// The rest of the esds (buffer size, bit rates) is the encoder's
			// statistics, which ffmpeg fills in and a remux cannot.
			if e.ChannelCount != w.ChannelCount || e.SampleRate != w.SampleRate ||
				!bytes.Equal(e.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig,
					w.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig) {
				t.Errorf("audio entry differs from ffmpeg's: %d ch %d Hz vs %d ch %d Hz",
					e.ChannelCount, e.SampleRate, w.ChannelCount, w.SampleRate)
			}
		}
	}
	if seg.Tracks[0].Codecs != "avc1.4d400b" || seg.Tracks[1].Codecs != "mp4a.40.2" {
		t.Errorf("codecs %q %q, ffmpeg's master playlist says avc1.4d400b and mp4a.40.2",
			seg.Tracks[0].Codecs, seg.Tracks[1].Codecs)
	}
}

func TestSegmentAlignsTracksAndCountsMatchTheMPD(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// manifest.mpd: video SegmentTimeline S t=0 d=25600 r=3 at timescale 12800.
	v := seg.Tracks[0]
	if v.Timescale != 12800 {
		t.Fatalf("video timescale %d", v.Timescale)
	}
	for i, s := range v.Segments {
		if s.Start != uint64(i)*25600 || s.Duration != 25600 {
			t.Errorf("video segment %d: %d+%d, MPD says %d+25600", i, s.Start, s.Duration, i*25600)
		}
	}
	// Every audio segment starts at the first frame at or after a video start.
	a := seg.Tracks[1]
	for i, s := range a.Segments {
		if i == 0 {
			continue
		}
		vt := float64(v.Segments[i].Start) / float64(v.Timescale)
		at := float64(s.Start) / float64(a.Timescale)
		if at < vt || at-vt > 1024.0/48000 {
			t.Errorf("audio segment %d starts at %.4f s, video at %.4f s", i, at, vt)
		}
	}
	if v.Bandwidth == 0 || v.AverageBandwidth == 0 || v.Bandwidth < v.AverageBandwidth {
		t.Errorf("bandwidth %d / average %d", v.Bandwidth, v.AverageBandwidth)
	}
	if d := v.Duration(); d < 7.99 || d > 8.01 {
		t.Errorf("duration %v", d)
	}
}

func TestSegmentWritesWebVTTSegmentsThatRepeatCuesCrossingAnEdge(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mkv"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Tracks) != 3 {
		t.Fatalf("%d tracks", len(seg.Tracks))
	}
	text := seg.Tracks[2]
	if text.Name != "text1" || text.MIME != "text/vtt" || text.Codecs != "wvtt" || text.Init != nil ||
		text.SegmentName(0) != "text1_00001.vtt" {
		t.Fatalf("text track %+v", text.Name)
	}
	if len(text.Segments) != len(seg.Tracks[0].Segments) {
		t.Fatalf("%d text segments for %d video segments", len(text.Segments), len(seg.Tracks[0].Segments))
	}
	const head = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n"
	cue1 := "00:00:00.500 --> 00:00:02.500\nFirst cue\n\n"
	// (ffmpeg's Matroska muxer drops the cue's "line:10%" setting; so does
	// its own hls output, which is why none is expected here.)
	cue2 := "00:00:03.000 --> 00:00:05.000\nSecond cue\nspans two lines\n\n"
	cue3 := "00:00:05.500 --> 00:00:07.500\nThird cue\n\n"
	want := []string{head + cue1, head + cue1 + cue2, head + cue2 + cue3, head + cue3}
	for i, w := range want {
		if got := string(text.Segments[i].Data); got != w {
			t.Errorf("text segment %d:\n%q\nwant\n%q", i, got, w)
		}
		if text.Segments[i].Start != uint64(i)*2000 || text.Segments[i].Duration != 2000 {
			t.Errorf("text segment %d spans %d+%d ms", i, text.Segments[i].Start, text.Segments[i].Duration)
		}
	}
	// ffmpeg's hls muxer, on the same cues, writes each cue once, in the segment
	// where it starts, and shifts them by the video's start (80 ms); the cue
	// texts and the segment each starts in are the oracle's, the repetition ours
	// (draft-pantos-hls-rfc8216bis 3.1.4: a segment holds every cue displayed in
	// its period).
	for i, first := range []string{"First cue", "Second cue\nspans two lines", "Third cue", ""} {
		o := string(segFile(t, "oracle/subs/media_0"+string(rune('0'+i))+".vtt"))
		if (first == "") != !strings.Contains(o, "-->") || (first != "" && !strings.Contains(o, first)) {
			t.Errorf("oracle segment %d does not hold %q: %q", i, first, o)
		}
		if first != "" && !strings.Contains(string(text.Segments[i].Data), first) {
			t.Errorf("segment %d lacks the cue ffmpeg starts there: %q", i, first)
		}
	}
}

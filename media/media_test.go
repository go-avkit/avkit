// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package media

import (
	"errors"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	empd "github.com/Eyevinn/dash-mpd/mpd"

	"github.com/go-avkit/avkit/container"
	"github.com/go-streamkit/streamkit/dash"
	"github.com/go-streamkit/streamkit/hls"
)

// The oracle is ffmpeg's hls (fmp4) and dash muxers on container/testdata/
// segment/source.mp4, whose output go-avkit's container tests committed.

func oraclePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "container", "testdata", "segment"}, parts...)...)
}

func oracleLines(t *testing.T, parts ...string) []string {
	t.Helper()
	b, err := os.ReadFile(oraclePath(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func segmentFile(t *testing.T, name string) *container.Segmentation {
	t.Helper()
	b, err := os.ReadFile(oraclePath(name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := container.NewReader(b)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := container.Segment(r, container.SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

// attr reads one attribute of a tag line.
func attr(line, key string) string {
	_, rest, _ := strings.Cut(line, ":")
	var field strings.Builder
	quoted := false
	check := func() string {
		k, v, ok := strings.Cut(field.String(), "=")
		field.Reset()
		if ok && k == key {
			return strings.Trim(v, `"`)
		}
		return ""
	}
	for _, r := range rest {
		switch {
		case r == '"':
			quoted = !quoted
			field.WriteRune(r)
		case r == ',' && !quoted:
			if v := check(); v != "" {
				return v
			}
		default:
			field.WriteRune(r)
		}
	}
	return check()
}

func TestHLSMatchesFFmpegsMediaPlaylists(t *testing.T) {
	files, err := HLS(segmentFile(t, "source.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	// Video: ffmpeg's four 2 s segments, the same tags and durations; only the
	// file names are ours.
	got := strings.Split(strings.TrimSpace(string(files.Media["video1.m3u8"])), "\n")
	want := oracleLines(t, "hls", "media_video.m3u8")
	if len(got) != len(want) {
		t.Fatalf("%d lines, ffmpeg %d:\n%s", len(got), len(want), files.Media["video1.m3u8"])
	}
	for i := range want {
		switch {
		case strings.HasPrefix(want[i], "#EXT-X-MAP"):
			if got[i] != `#EXT-X-MAP:URI="video1_init.mp4"` {
				t.Errorf("map %q", got[i])
			}
		case strings.HasPrefix(want[i], "#"):
			// header tags may come in another order; compare as sets below
		default:
			// ffmpeg numbers its segments from 0, this from 1.
			if !strings.HasPrefix(got[i], "video1_0000") || !strings.HasSuffix(got[i], ".m4s") {
				t.Errorf("segment line %q, ffmpeg %q", got[i], want[i])
			}
		}
	}
	for _, tag := range []string{"#EXT-X-VERSION:7", "#EXT-X-TARGETDURATION:2", "#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-INDEPENDENT-SEGMENTS", "#EXTINF:2.000000,", "#EXT-X-ENDLIST"} {
		if !strings.Contains(string(files.Media["video1.m3u8"]), tag) {
			t.Errorf("missing %s", tag)
		}
	}
	// Audio: ffmpeg's first three EXTINF are ours; ffmpeg then splits the last
	// 2.005333 s into 1.984 and 0.021333 (avkit#104), so the totals agree.
	audio := string(files.Media["audio1.m3u8"])
	var ffTotal, total float64
	for _, l := range oracleLines(t, "hls", "media_audio.m3u8") {
		if v, ok := strings.CutPrefix(l, "#EXTINF:"); ok {
			d, _ := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
			ffTotal += d
		}
	}
	for _, l := range strings.Split(audio, "\n") {
		if v, ok := strings.CutPrefix(l, "#EXTINF:"); ok {
			d, _ := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
			total += d
		}
	}
	if math.Abs(total-ffTotal) > 1e-5 {
		t.Errorf("audio totals %v s, ffmpeg's %v s", total, ffTotal)
	}
	if strings.Count(audio, "#EXTINF:2.005333,") != 4 || !strings.Contains(audio, "#EXT-X-TARGETDURATION:2") ||
		!strings.Contains(audio, `#EXT-X-MAP:URI="audio1_init.mp4"`) || strings.Contains(audio, "INDEPENDENT") {
		t.Errorf("audio playlist:\n%s", audio)
	}
}

func TestHLSMasterPlaylistStatesWhatFFmpegsDoes(t *testing.T) {
	files, err := HLS(segmentFile(t, "source.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	var wantInf, wantMedia string
	for _, l := range oracleLines(t, "hls", "master.m3u8") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"):
			wantInf = l
		case strings.HasPrefix(l, "#EXT-X-MEDIA"):
			wantMedia = l
		}
	}
	var gotInf, gotMedia string
	for _, l := range strings.Split(string(files.Master), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"):
			gotInf = l
		case strings.HasPrefix(l, "#EXT-X-MEDIA"):
			gotMedia = l
		}
	}
	for _, k := range []string{"RESOLUTION", "CODECS"} {
		if attr(gotInf, k) != attr(wantInf, k) || attr(gotInf, k) == "" {
			t.Errorf("%s: %q, ffmpeg %q\n%s", k, attr(gotInf, k), attr(wantInf, k), files.Master)
		}
	}
	for _, k := range []string{"TYPE", "DEFAULT", "CHANNELS"} {
		if attr(gotMedia, k) != attr(wantMedia, k) || attr(gotMedia, k) == "" {
			t.Errorf("%s: %q, ffmpeg %q", k, attr(gotMedia, k), attr(wantMedia, k))
		}
	}
	// ffmpeg's BANDWIDTH (67208) counts the video stream alone, although the
	// variant plays the audio rendition too; this adds the audio. The oracle
	// for the sum is ffprobe's bit_rate of the two streams of source.mp4, 64265
	// and 64278 bit/s: the segments' rates, which count the moof and mdat
	// overhead, must be above it and within 25% of it.
	const ffprobeSum = 64265 + 64278
	for _, k := range []string{"BANDWIDTH", "AVERAGE-BANDWIDTH"} {
		g, err := strconv.ParseFloat(attr(gotInf, k), 64)
		if err != nil {
			t.Fatal(err)
		}
		if g < ffprobeSum || g > ffprobeSum*1.25 {
			t.Errorf("%s %v, ffprobe's streams sum to %d", k, g, ffprobeSum)
		}
		if w, _ := strconv.ParseFloat(attr(wantInf, k), 64); g <= w {
			t.Errorf("%s %v is not above ffmpeg's video-only %v", k, g, w)
		}
	}
	if attr(gotInf, "AUDIO") != "audio" || attr(gotMedia, "URI") != "audio1.m3u8" {
		t.Errorf("%s", files.Master)
	}
}

func TestDASHMatchesFFmpegsManifest(t *testing.T) {
	b, err := DASH(segmentFile(t, "source.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := empd.ReadFromString(string(b))
	if err != nil {
		t.Fatal(err)
	}
	want, err := empd.ReadFromFile(oraclePath("dash", "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	// The presentation lasts as long as its longest track: the audio, 8.021 s
	// with its priming frame, where ffmpeg's dash muxer states 8 s and drops one
	// frame (1024 ticks) from its audio timeline that its hls muxer keeps.
	near := func(a, b empd.Duration) bool { return math.Abs(float64(a-b)) <= float64(25*time.Millisecond) }
	if got.Profiles != want.Profiles || got.GetType() != want.GetType() ||
		!near(*got.MediaPresentationDuration, *want.MediaPresentationDuration) ||
		!near(*got.MaxSegmentDuration, *want.MaxSegmentDuration) {
		t.Errorf("MPD: %q %q %v %v, ffmpeg %q %q %v %v", got.Profiles, got.GetType(), *got.MediaPresentationDuration,
			*got.MaxSegmentDuration, want.Profiles, want.GetType(), *want.MediaPresentationDuration, *want.MaxSegmentDuration)
	}
	for i, ws := range want.Periods[0].AdaptationSets {
		gs := got.Periods[0].AdaptationSets[i]
		wr, gr := ws.Representations[0], gs.Representations[0]
		if gs.ContentType != ws.ContentType || gs.SegmentAlignment != ws.SegmentAlignment ||
			gr.MimeType != wr.MimeType || gr.Codecs != wr.Codecs || gr.Width != wr.Width || gr.Height != wr.Height ||
			*gr.SegmentTemplate.Timescale != *wr.SegmentTemplate.Timescale {
			t.Errorf("set %d differs from ffmpeg's: %+v / %+v", i, gr.RepresentationBaseType, wr.RepresentationBaseType)
		}
		wt, gt := wr.SegmentTemplate.SegmentTimeline.S, gr.SegmentTemplate.SegmentTimeline.S
		var wTotal, gTotal uint64
		for _, s := range wt {
			wTotal += s.D * uint64(1+s.R)
		}
		for _, s := range gt {
			gTotal += s.D * uint64(1+s.R)
		}
		// Video: exactly ffmpeg's. Audio: 385024 ticks, which is what ffmpeg's
		// hls playlist covers (8.021333 s), one frame more than its MPD.
		wantTotal := wTotal
		if ws.ContentType == "audio" {
			wantTotal = 385024
		}
		if gTotal != wantTotal {
			t.Errorf("set %d: timeline covers %d ticks, want %d (ffmpeg's MPD %d)", i, gTotal, wantTotal, wTotal)
		}
		if ws.ContentType == "video" {
			if len(wt) != len(gt) || gt[0].D != wt[0].D || gt[0].R != wt[0].R {
				t.Errorf("video timeline %+v, ffmpeg %+v", gt[0], wt[0])
			}
		}
	}
}

func TestAPresentationIsWrittenAndEveryFileItNamesExists(t *testing.T) {
	seg := segmentFile(t, "source.mkv") // video, audio, WebVTT
	dir := filepath.Join(t.TempDir(), "out")
	if err := WriteDir(dir, seg); err != nil {
		t.Fatal(err)
	}
	exists := func(rel string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("a file the presentation names is missing: %v", err)
		}
	}
	base, _ := url.Parse("https://cdn.example/vod/")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	local := func(u string) string { return strings.TrimPrefix(u, base.String()) }
	master, err := hls.Parse(base.JoinPath("master.m3u8"), read(MasterName))
	if err != nil || !master.Master || len(master.Variants) != 1 {
		t.Fatalf("master: %+v %v", master, err)
	}
	for _, name := range []string{"video1.m3u8", "audio1.m3u8", "text1.m3u8"} {
		pl, err := hls.Parse(base.JoinPath(name), read(name))
		if err != nil || pl.Live {
			t.Fatalf("%s: %+v %v", name, pl, err)
		}
		if pl.Init != nil {
			exists(local(pl.Init.URL))
		}
		for _, s := range pl.Segments {
			exists(local(s.URL))
		}
		if len(pl.Segments) != 4 {
			t.Errorf("%s: %d segments", name, len(pl.Segments))
		}
	}
	for _, line := range strings.Split(string(read(MasterName)), "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA") {
			exists(attr(line, "URI"))
		}
	}
	exists(master.Variants[0].URL[strings.LastIndex(master.Variants[0].URL, "/")+1:])
	m, err := dash.Parse(base.JoinPath("manifest.mpd"), read(ManifestName))
	if err != nil || m.Dynamic || len(m.Streams) != 3 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	for _, s := range m.Streams {
		if s.Init != "" {
			exists(local(s.Init))
		}
		for _, u := range s.Segments {
			exists(local(u))
		}
		if len(s.Segments) != 4 {
			t.Errorf("%s: %d segments", s.ID, len(s.Segments))
		}
	}
	if path.Base(m.Streams[2].Segments[0]) != "text1_00001.vtt" || m.Streams[2].Kind != dash.KindText {
		t.Errorf("text stream %+v", m.Streams[2])
	}
	if !strings.Contains(string(read(MasterName)), `TYPE=SUBTITLES`) {
		t.Errorf("no subtitles rendition:\n%s", read(MasterName))
	}
}

func TestWhatCannotBeDescribedIsRefusedAndNothingIsWritten(t *testing.T) {
	good := segmentFile(t, "source.mkv")
	clone := func(mod func(*container.Segmentation)) *container.Segmentation {
		c := &container.Segmentation{Target: good.Target, Tracks: append([]container.SegmentedTrack(nil), good.Tracks...)}
		mod(c)
		return c
	}
	cases := map[string]*container.Segmentation{
		"no segment":   clone(func(s *container.Segmentation) { s.Tracks[0].Segments = nil }),
		"no timescale": clone(func(s *container.Segmentation) { s.Tracks[1].Timescale = 0 }),
		"two videos": clone(func(s *container.Segmentation) {
			s.Tracks = append(s.Tracks, s.Tracks[0])
		}),
		"audio only":  clone(func(s *container.Segmentation) { s.Tracks = s.Tracks[1:2] }),
		"unsafe name": clone(func(s *container.Segmentation) { s.Tracks[1].Name = "../audio" }),
	}
	for name, seg := range cases {
		dir := filepath.Join(t.TempDir(), "out")
		if err := WriteDir(dir, seg); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: wrote into the directory", name)
		}
	}
	for name, seg := range map[string]*container.Segmentation{
		"no segment": cases["no segment"], "two videos": cases["two videos"],
	} {
		if _, err := HLS(seg); !errors.Is(err, ErrPresentation) {
			t.Errorf("%s: HLS %v", name, err)
		}
		if _, err := DASH(seg); !errors.Is(err, ErrPresentation) {
			t.Errorf("%s: DASH %v", name, err)
		}
	}
}

func TestBandwidthsThatCannotBeStatedAreRefused(t *testing.T) {
	good := segmentFile(t, "source.mkv")
	with := func(mod func(*container.Segmentation)) *container.Segmentation {
		c := &container.Segmentation{Target: good.Target, Tracks: append([]container.SegmentedTrack(nil), good.Tracks...)}
		mod(c)
		return c
	}
	zero := with(func(s *container.Segmentation) { s.Tracks[0].Bandwidth, s.Tracks[1].Bandwidth = 0, 0 })
	if _, err := HLS(zero); !errors.Is(err, ErrPresentation) {
		t.Errorf("HLS zero: %v", err)
	}
	if _, err := DASH(zero); !errors.Is(err, ErrPresentation) {
		t.Errorf("DASH zero: %v", err)
	}
	huge := with(func(s *container.Segmentation) {
		s.Tracks[0].Bandwidth = math.MaxUint32
		s.Tracks[1].Bandwidth = math.MaxUint32
	})
	if _, err := HLS(huge); !errors.Is(err, ErrPresentation) {
		t.Errorf("HLS huge: %v", err)
	}
	over := with(func(s *container.Segmentation) { s.Tracks[0].Bandwidth = math.MaxUint32 + 1 })
	if _, err := DASH(over); !errors.Is(err, ErrPresentation) {
		t.Errorf("DASH huge: %v", err)
	}
	bigAvg := with(func(s *container.Segmentation) { s.Tracks[0].AverageBandwidth = math.MaxUint32 + 1 })
	if _, err := HLS(bigAvg); !errors.Is(err, ErrPresentation) {
		t.Errorf("HLS average: %v", err)
	}
	// An audio track that states no bandwidth passes HLS (the variant has the
	// video's) and stops DASH, which needs one per representation.
	audioZero := with(func(s *container.Segmentation) { s.Tracks[1].Bandwidth = 0 })
	if _, err := HLS(audioZero); err != nil {
		t.Errorf("HLS: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if err := WriteDir(dir, audioZero); !errors.Is(err, ErrPresentation) {
		t.Errorf("WriteDir: %v", err)
	}
}

func TestAWriterRefusesWhatTheTrackNamesWouldBreak(t *testing.T) {
	good := segmentFile(t, "source.mkv")
	with := func(mod func(*container.Segmentation)) *container.Segmentation {
		c := &container.Segmentation{Target: good.Target, Tracks: append([]container.SegmentedTrack(nil), good.Tracks...)}
		mod(c)
		return c
	}
	if _, err := HLS(with(func(s *container.Segmentation) { s.Tracks[0].Name = "a/../b" })); err == nil {
		t.Error("HLS accepted a traversing name")
	}
	if _, err := HLS(with(func(s *container.Segmentation) { s.Tracks[1].Config.Language = `e"` })); err == nil {
		t.Error("HLS accepted a quote in a language")
	}
	if _, err := DASH(with(func(s *container.Segmentation) { s.Tracks[1].Name = "a b" })); err == nil {
		t.Error("DASH accepted a bad identifier")
	}
}

func TestLanguagesAndNamesOfRenditions(t *testing.T) {
	seg := segmentFile(t, "source.mkv")
	for i := range seg.Tracks {
		seg.Tracks[i].Config.Language = "fra"
	}
	seg.Tracks[0].Config.SARWidth, seg.Tracks[0].Config.SARHeight = 4, 3
	files, err := HLS(seg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files.Master), `NAME="audio1 (fra)"`) || !strings.Contains(string(files.Master), `LANGUAGE="fra"`) {
		t.Errorf("%s", files.Master)
	}
	b, err := DASH(seg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `lang="fra"`) || !strings.Contains(string(b), `sar="4:3"`) {
		t.Errorf("%s", b)
	}
	// An undetermined subtitle language is stated as und, which HLS requires.
	for i := range seg.Tracks {
		seg.Tracks[i].Config.Language = "und"
	}
	files, err = HLS(seg)
	if err != nil || !strings.Contains(string(files.Master), `LANGUAGE="und"`) {
		t.Errorf("%v\n%s", err, files.Master)
	}
}

func TestWriteDirReportsFilesystemFailures(t *testing.T) {
	seg := segmentFile(t, "source.mkv")
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDir(filepath.Join(file, "sub"), seg); err == nil {
		t.Error("a directory under a file")
	}
	for _, blocker := range []string{MasterName, ManifestName, "video1.m3u8"} {
		dir := filepath.Join(root, "b-"+blocker)
		if err := os.MkdirAll(filepath.Join(dir, blocker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteDir(dir, seg); err == nil {
			t.Errorf("a directory named %s", blocker)
		}
	}
}

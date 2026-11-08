// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// fakeSeg is a Reader whose answers a test can spoil.
type fakeSeg struct {
	*Reader
	truncated  bool
	configErr  error
	samplesErr error
	noTracks   bool
}

func (f fakeSeg) Truncated() bool { return f.truncated }
func (f fakeSeg) File() *File {
	if f.noTracks {
		return &File{}
	}
	return f.Reader.File()
}
func (f fakeSeg) TrackIDs() []uint32 {
	if f.noTracks {
		return nil
	}
	return f.Reader.TrackIDs()
}
func (f fakeSeg) TrackConfig(id uint32) (TrackConfig, error) {
	if f.configErr != nil {
		return TrackConfig{}, f.configErr
	}
	return f.Reader.TrackConfig(id)
}
func (f fakeSeg) Samples(id uint32) ([]Sample, error) {
	if f.samplesErr != nil {
		return nil, f.samplesErr
	}
	return f.Reader.Samples(id)
}

func TestSegmentRefusesWhatItCannotCutAndSaysWhy(t *testing.T) {
	r := segmentSource(t, "source.mp4")
	boom := errors.New("boom")
	cases := []struct {
		name string
		r    segReader
		opts []SegmentOption
		want error
	}{
		{"target too short", r, []SegmentOption{SegmentDuration(time.Millisecond)}, ErrSegmentOption},
		{"target too long", r, []SegmentOption{SegmentDuration(2 * time.Hour)}, ErrSegmentOption},
		{"truncated", fakeSeg{Reader: r, truncated: true}, nil, ErrTruncated},
		{"unknown track", r, []SegmentOption{SegmentTracks(99)}, ErrUnknownTrack},
		{"no track", fakeSeg{Reader: r, noTracks: true}, nil, ErrNoTracks},
		{"config error", fakeSeg{Reader: r, configErr: boom}, nil, boom},
		{"samples error", fakeSeg{Reader: r, samplesErr: boom}, nil, boom},
	}
	for _, c := range cases {
		if _, err := segment(c.r, c.opts); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func TestANewSegSourceRefusesTracksNoPackagingCarries(t *testing.T) {
	one := []Sample{{Data: []byte{1}, Duration: 1, Sync: true}}
	cases := []struct {
		name    string
		cfg     TrackConfig
		samples []Sample
		want    error
	}{
		{"tx3g", TrackConfig{Kind: Subtitle, Codec: "tx3g", Timescale: 1000}, one, ErrSegmentTrack},
		{"other", TrackConfig{Kind: Other, Timescale: 1000}, one, ErrSegmentTrack},
		{"no timescale", TrackConfig{Kind: Video}, one, ErrSegmentTrack},
		{"no samples", TrackConfig{Kind: Video, Timescale: 1000}, nil, ErrNoSamples},
	}
	for _, c := range cases {
		if _, err := newSegSource(Track{ID: 7}, c.cfg, c.samples); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func syntheticSource(kind Kind, ts uint32, codec string, durations ...uint32) *segSource {
	var samples []Sample
	for _, d := range durations {
		samples = append(samples, Sample{Data: []byte{1, 2, 3}, Duration: d, Sync: true})
	}
	src, err := newSegSource(Track{ID: 1}, TrackConfig{Kind: kind, Codec: codec, Timescale: ts}, samples)
	if err != nil {
		panic(err)
	}
	return src
}

func TestTheReferenceTrackIsVideoThenAudioThenWhateverThereIs(t *testing.T) {
	v := syntheticSource(Video, 10, "avc1", 1)
	a := syntheticSource(Audio, 10, "mp4a", 1)
	s := syntheticSource(Subtitle, 10, "wvtt", 1)
	if referenceTrack([]*segSource{s, a, v}) != v || referenceTrack([]*segSource{s, a}) != a ||
		referenceTrack([]*segSource{s}) != s {
		t.Error("wrong reference")
	}
}

func TestReferenceCutsWaitForAKeyframeAndAreBounded(t *testing.T) {
	// 1 s samples, timescale 1; the sample at 2 s is not a keyframe, so the cut
	// moves to the one at 3 s.
	v := syntheticSource(Video, 1, "avc1", 1, 1, 1, 1, 1)
	v.samples[2].Sync = false
	c, err := referenceCuts(v, 2*time.Second)
	if err != nil || len(c.at) != 1 || c.at[0] != 3 || c.idx[0] != 3 || c.end != 5 {
		t.Fatalf("%+v %v", c, err)
	}
	// An audio reference needs no keyframe.
	a := syntheticSource(Audio, 1, "mp4a", 1, 1, 1, 1, 1)
	a.samples[2].Sync = false
	if c, _ = referenceCuts(a, 2*time.Second); len(c.at) != 2 || c.at[0] != 2 || c.at[1] != 4 {
		t.Fatalf("%+v", c)
	}
	// One segment per 100 ms sample would be more than MaxSegments.
	dur := make([]uint32, MaxSegments+2)
	for i := range dur {
		dur[i] = 1
	}
	big := syntheticSource(Video, 10, "avc1", dur...)
	if _, err = referenceCuts(big, MinSegmentDuration); !errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("%v", err)
	}
}

func TestOtherTracksAreCutAtTheFirstSampleAtOrAfterTheReferenceInstant(t *testing.T) {
	ref := syntheticSource(Video, 1, "avc1", 2, 2, 2, 2)
	cuts, _ := referenceCuts(ref, 2*time.Second) // at 2, 4, 6 s
	if got := cutIndexes(ref, cuts, ref); len(got) != 3 {
		t.Fatalf("ref %v", got)
	}
	// 1500 ms audio frames: 3 s is the frame at 3 s, 4.5 s the next after 4 s.
	a := syntheticSource(Audio, 1000, "mp4a", 1500, 1500, 1500, 1500, 1500, 1500)
	if got := cutIndexes(a, cuts, ref); len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Errorf("audio %v", got)
	}
	// A frame longer than the segment makes two instants land on one sample: the
	// second is not cut again.
	long := syntheticSource(Audio, 1000, "mp4a", 5000, 5000, 5000)
	if got := cutIndexes(long, cuts, ref); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("long %v", got)
	}
	// A track that ends before the last instant stops being cut.
	short := syntheticSource(Audio, 1000, "mp4a", 1000, 1000)
	if got := cutIndexes(short, cuts, ref); len(got) != 0 {
		t.Errorf("short %v", got)
	}
	// A second video track waits for a keyframe too.
	v2 := syntheticSource(Video, 1, "avc1", 1, 1, 1, 1, 1, 1, 1, 1)
	v2.samples[2].Sync = false
	if got := cutIndexes(v2, cuts, ref); len(got) == 0 || got[0] != 3 {
		t.Errorf("video %v", got)
	}
}

func TestSegmentSurfacesTheFailuresOfWritingATrack(t *testing.T) {
	r := segmentSource(t, "source.mp4")
	// A codec with no codecs string.
	bad := syntheticSource(Video, 1000, "xxxx", 1000)
	if _, err := cutMedia(bad, bad, refCuts{ts: 1000}, false); !errors.Is(err, ErrSegmentTrack) {
		t.Errorf("codecs: %v", err)
	}
	// A track the muxer cannot describe: FLAC without its STREAMINFO.
	opus, err := newSegSource(Track{ID: 1}, TrackConfig{Kind: Audio, Codec: "fLaC", Timescale: 48000, Channels: 2, SampleRate: 48000},
		[]Sample{{Data: []byte{1}, Duration: 960}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cutMedia(opus, opus, refCuts{ts: 48000}, false); !errors.Is(err, ErrNotInSamples) && !errors.Is(err, ErrTrackConfig) {
		t.Errorf("add track: %v", err)
	}
	// A sample with no data.
	good, err := loadSegSource(r, r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	empty := *good
	empty.samples = []Sample{{Duration: 1, Sync: true}}
	empty.starts, empty.end = []uint64{0}, 1
	if _, err := cutMedia(&empty, &empty, refCuts{ts: good.cfg.Timescale}, false); !errors.Is(err, ErrSample) {
		t.Errorf("write sample: %v", err)
	}
	// A write that fails.
	segmentSink = func(*bytes.Buffer) io.Writer { return failingWriter{} }
	defer func() { segmentSink = func(b *bytes.Buffer) io.Writer { return b } }()
	if _, err := cutMedia(good, good, refCuts{ts: good.cfg.Timescale}, false); err == nil {
		t.Error("flush error not reported")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestASegmentTooLongToIndexIsRefused(t *testing.T) {
	r := segmentSource(t, "source.mp4")
	good, _ := loadSegSource(r, r.TrackIDs()[0])
	// Two samples of 3e9 ticks make one segment of 6e9, which a 32-bit
	// subsegment_duration cannot state.
	src := *good
	src.samples = []Sample{
		{Data: good.samples[0].Data, Duration: 3_000_000_000, Sync: true},
		{Data: good.samples[0].Data, Duration: 3_000_000_000},
	}
	src.starts, src.end = []uint64{0, 3_000_000_000}, 6_000_000_000
	if _, err := cutMedia(&src, &src, refCuts{ts: good.cfg.Timescale}, true); !errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("%v", err)
	}
	if _, err := segmentIndexBox(good, 0, 1, maxReferencedSize+1, 1); !errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("size: %v", err)
	}
}

func TestIndexedSegmentsCarryAStypAndASidxThatReferenceTheFragment(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second), IndexedSegments())
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range seg.Tracks {
		for i, s := range tr.Segments {
			boxes := segmentBoxes(s.Data)
			if len(boxes) != 4 || boxes[0].typ != "styp" || boxes[1].typ != "sidx" ||
				boxes[2].typ != "moof" || boxes[3].typ != "mdat" {
				t.Fatalf("%s segment %d: %v", tr.Name, i, boxes)
			}
			// The sidx's one reference spans the moof and mdat after it, and its
			// first_offset is 0: the fragment follows it directly.
			sidx := s.Data[boxes[1].start:boxes[1].end]
			if got := uint32(sidx[len(sidx)-12])<<24 | uint32(sidx[len(sidx)-11])<<16 |
				uint32(sidx[len(sidx)-10])<<8 | uint32(sidx[len(sidx)-9]); got&0x7fffffff != uint32(len(s.Data)-boxes[2].start) {
				t.Errorf("%s segment %d: referenced size %d, fragment %d", tr.Name, i, got&0x7fffffff, len(s.Data)-boxes[2].start)
			}
		}
	}
}

func TestSegmentBoxesWalksBoxesAndStopsAtAMalformedOne(t *testing.T) {
	box := func(typ string, size uint32, body int) []byte {
		b := []byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
		b = append(b, typ...)
		return append(b, make([]byte, body)...)
	}
	large := append([]byte{0, 0, 0, 1}, "free"...)
	large = append(large, 0, 0, 0, 0, 0, 0, 0, 20, 1, 2, 3, 4)
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"two boxes", append(box("ftyp", 12, 4), box("moof", 8, 0)...), 2},
		{"to the end", box("mdat", 0, 5), 1},
		{"large", large, 1},
		{"truncated large", large[:12], 0},
		{"too big", box("free", 99, 0), 0},
		{"too small", box("free", 4, 0), 0},
		{"short", []byte{0, 0, 0}, 0},
	}
	for _, c := range cases {
		if got := len(segmentBoxes(c.in)); got != c.want {
			t.Errorf("%s: %d boxes, want %d", c.name, got, c.want)
		}
	}
}

func TestBitrateSaturatesAndTheHelpersAgree(t *testing.T) {
	if bitrate(1, 0, 1) != 0 || bitrate(125, 1, 1) != 1000 || bitrate(math.MaxUint64/8, 1, math.MaxUint32) != math.MaxUint64 {
		t.Error("bitrate")
	}
	if ticksOf(2500*time.Millisecond, 1000) != 2500 || !before(1, 3, 1, 2) || before(1, 2, 1, 3) || before(1, 2, 1, 2) {
		t.Error("ticksOf or before")
	}
	if vttClock(3_723_004) != "01:02:03.004" {
		t.Error(vttClock(3_723_004))
	}
}

func TestCodecsOfNamesEachCodecAsFFmpegDoes(t *testing.T) {
	for _, c := range []struct{ file, v, a string }{
		{"codecs/av1_opus.mp4", "av01.0.00M.08", "Opus"},
		{"codecs/vp9_flac.mp4", "vp09.00.10.08", "fLaC"},
		{"codecs/hevc_ac3.mp4", "hvc1.1.6.L", "ac-3"},
		{"source.mp4", "avc1.4d400b", "mp4a.40.2"},
	} {
		seg, err := Segment(segmentSource(t, c.file), SegmentDuration(time.Second))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		// ffmpeg's dash muxer states av01.0.00M.08, vp09.00.10.08, ac-3, and
		// flac and opus in lower case; it states no codecs at all for hvc1, so
		// that one is checked by prefix. Opus and fLaC are the registered
		// spellings (RFC 6381 and Apple's HLS Authoring Specification).
		if got := seg.Tracks[0].Codecs; !strings.HasPrefix(got, c.v) {
			t.Errorf("%s video codecs %q, want %q", c.file, got, c.v)
		}
		if got := seg.Tracks[1].Codecs; got != c.a {
			t.Errorf("%s audio codecs %q, want %q", c.file, got, c.a)
		}
	}
}

func TestCodecsOfRefusesWhatItCannotName(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  TrackConfig
		want string
	}{
		{"avc no sps", TrackConfig{Codec: "avc1"}, ""},
		{"avc bad sps", TrackConfig{Codec: "avc1", SPS: [][]byte{{1}}}, ""},
		{"hevc no sps", TrackConfig{Codec: "hvc1"}, ""},
		{"hevc bad sps", TrackConfig{Codec: "hev1", SPS: [][]byte{{1}}}, ""},
		{"av1 bad", TrackConfig{Codec: "av01"}, ""},
		{"vp9 none", TrackConfig{Codec: "vp09"}, ""},
		{"aac bad", TrackConfig{Codec: "mp4a", CodecConfig: []byte{0xff}}, ""},
		{"unknown", TrackConfig{Codec: "zzzz"}, ""},
		{"vp8", TrackConfig{Codec: "vp08", VPx: &VPxConfig{BitDepth: 8}}, "vp08.00.00.08"},
		{"mp3", TrackConfig{Codec: "mp3"}, "mp4a.6B"},
		{"mp2", TrackConfig{Codec: "mp2"}, "mp4a.69"},
		{"aac no config", TrackConfig{Codec: "mp4a"}, "mp4a.40.2"},
		{"aac profile", TrackConfig{Codec: "mp4a", AudioObjectType: 5}, "mp4a.40.5"},
		{"he-aac", TrackConfig{Codec: "mp4a", CodecConfig: []byte{0x2b, 0x92, 0x08, 0x00}}, "mp4a.40.5"},
		{"he-aacv2", TrackConfig{Codec: "mp4a", CodecConfig: []byte{0xeb, 0x0a, 0x08, 0x00}}, ""},
	} {
		got, err := codecsOf(c.cfg)
		switch {
		case c.want == "" && c.name != "he-aacv2" && err == nil:
			t.Errorf("%s: no error, got %q", c.name, got)
		case c.want == "" && c.name != "he-aacv2" && !errors.Is(err, ErrSegmentTrack):
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err != nil || got != c.want):
			t.Errorf("%s: %q, %v, want %q", c.name, got, err, c.want)
		}
	}
}

func TestAWebVTTCueThatWouldBreakTheDocumentIsRefused(t *testing.T) {
	var w strings.Builder
	for _, c := range []webvttCue{
		{id: "a\nb"}, {settings: "x --> y"}, {text: "a\n\nb"}, {text: "a --> b"}, {text: "\nlead"}, {text: "trail\n"},
		{comments: []string{"x\ny"}}, {comments: []string{"x --> y"}},
	} {
		if err := writeCue(&w, c, 0, 1); !errors.Is(err, ErrSample) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	w.Reset()
	c := webvttCue{id: "7", settings: "line:0", text: "a\r\nb", comments: []string{"note"}}
	if err := writeCue(&w, c, 1500, 2000); err != nil {
		t.Fatal(err)
	}
	if want := "NOTE note\n\n7\n00:00:01.500 --> 00:00:02.000 line:0\na\nb\n\n"; w.String() != want {
		t.Errorf("%q", w.String())
	}
}

func wvttSource(t *testing.T, data ...[]byte) *segSource {
	t.Helper()
	var samples []Sample
	for _, d := range data {
		samples = append(samples, Sample{Data: d, Duration: 1000, Sync: true})
	}
	src, err := newSegSource(Track{ID: 3}, TrackConfig{Kind: Subtitle, Codec: "wvtt", Timescale: 1000}, samples)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestCutTextRefusesBadSamplesAndBadCues(t *testing.T) {
	cuts := refCuts{ts: 1000, end: 2000}
	if _, err := cutText(wvttSource(t, []byte("not boxes")), nil, cuts); !errors.Is(err, ErrSample) {
		t.Errorf("sample: %v", err)
	}
	var enc bytes.Buffer
	if err := vttc("", "", "a --> b").Encode(&enc); err != nil {
		t.Fatal(err)
	}
	if _, err := cutText(wvttSource(t, enc.Bytes()), nil, cuts); !errors.Is(err, ErrSample) {
		t.Errorf("cue: %v", err)
	}
	// A track that outlasts the reference gets a last window reaching its end.
	enc.Reset()
	_ = vttc("", "", "late").Encode(&enc)
	st, err := cutText(wvttSource(t, enc.Bytes(), enc.Bytes(), enc.Bytes()), nil, cuts)
	if err != nil || len(st.Segments) != 1 || st.Segments[0].Duration != 3000 {
		t.Errorf("%+v %v", st.Segments, err)
	}
}

// synthSeg is a reader of invented tracks.
type synthSeg struct {
	tracks []*segSource
}

func (s synthSeg) File() *File {
	f := &File{}
	for _, t := range s.tracks {
		f.Tracks = append(f.Tracks, t.track)
	}
	return f
}
func (synthSeg) Truncated() bool { return false }
func (s synthSeg) TrackIDs() []uint32 {
	var ids []uint32
	for _, t := range s.tracks {
		ids = append(ids, t.track.ID)
	}
	return ids
}
func (s synthSeg) TrackConfig(id uint32) (TrackConfig, error) { return s.tracks[id-1].cfg, nil }
func (s synthSeg) Samples(id uint32) ([]Sample, error)        { return s.tracks[id-1].samples, nil }

func TestSegmentNamesTheTrackWhoseCuttingFailed(t *testing.T) {
	bad := syntheticSource(Video, 1000, "xxxx", 1000)
	if _, err := segment(synthSeg{[]*segSource{bad}}, nil); !errors.Is(err, ErrSegmentTrack) ||
		!strings.Contains(err.Error(), "track 1") {
		t.Errorf("%v", err)
	}
	dur := make([]uint32, MaxSegments+2)
	for i := range dur {
		dur[i] = 1
	}
	big := syntheticSource(Video, 10, "avc1", dur...)
	if _, err := segment(synthSeg{[]*segSource{big}}, []SegmentOption{SegmentDuration(MinSegmentDuration)}); !errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("%v", err)
	}
}

func TestATrackConfigOfAMovieWithoutAnMvhdIsRefusedNotDereferenced(t *testing.T) {
	// Found by FuzzSegment: Reader.TrackConfig read the movie timescale of an
	// mvhd the file did not have. The input is the committed corpus entry
	// segment-nil-mvhd; this one removes the box from a real file.
	f, err := mp4.DecodeFile(bytes.NewReader(segFile(t, "../tiny.mp4")))
	if err != nil {
		t.Fatal(err)
	}
	kept := f.Moov.Children[:0]
	for _, c := range f.Moov.Children {
		if c.Type() != "mvhd" {
			kept = append(kept, c)
		}
	}
	f.Moov.Children, f.Moov.Mvhd = kept, nil
	var buf bytes.Buffer
	if err := f.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Skipf("the reader already refuses this file: %v", err)
	}
	if _, err := r.TrackConfig(r.TrackIDs()[0]); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("%v", err)
	}
}

func TestSegmentOfASubtitleFileAloneIsCutOnTheTargetGrid(t *testing.T) {
	seg, err := Segment(segmentSource(t, "../subtitles-webvtt.mkv"), SegmentDuration(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Tracks) != 1 || seg.Tracks[0].Kind != Subtitle || len(seg.Tracks[0].Segments) == 0 {
		t.Fatalf("%+v", seg.Tracks)
	}
}

func TestWriteDirWritesGeneratedNamesAndNothingElse(t *testing.T) {
	seg, err := Segment(segmentSource(t, "source.mkv"), SegmentDuration(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out", "nested")
	names, err := seg.WriteDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := 2 + 4*3 // two initialisation segments and four segments of each track
	if len(names) != want {
		t.Fatalf("%d files, want %d", len(names), want)
	}
	got, err := os.ReadFile(filepath.Join(dir, "video1_init.mp4"))
	if err != nil || !bytes.Equal(got, seg.Tracks[0].Init) {
		t.Errorf("init differs: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "text1_00002.vtt"))
	if err != nil || !bytes.Equal(got, seg.Tracks[2].Segments[1].Data) {
		t.Errorf("text segment differs: %v", err)
	}
	// Written again over what is there.
	if _, err := seg.WriteDir(dir); err != nil {
		t.Errorf("rewrite: %v", err)
	}
}

func TestWriteDirNeverLeavesItsDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "out")
	for _, name := range []string{"../escape", "a/b", ".hidden", "sp ace", strings.Repeat("a", 300)} {
		s := &Segmentation{Tracks: []SegmentedTrack{{Name: name, Init: []byte("x")}}}
		if _, err := s.WriteDir(dir); err == nil {
			t.Errorf("%q written", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape_init.mp4")); err == nil {
		t.Error("a file escaped")
	}
	if !plainFileName("video1_00001.m4s") {
		t.Error("a plain name is refused")
	}
}

func TestWriteDirReportsFilesystemFailures(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Segmentation{Tracks: []SegmentedTrack{{Name: "v", Init: []byte("x"), Segments: []MediaSegment{{Data: []byte("y")}}}}}
	if _, err := s.WriteDir(filepath.Join(file, "sub")); err == nil {
		t.Error("a directory under a file")
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	if os.Geteuid() != 0 {
		if _, err := s.WriteDir(locked); err == nil {
			t.Error("an unreadable directory")
		}
	}
	// A directory where the init segment goes, then where a segment goes.
	for _, blocker := range []string{"v_init.mp4", "v_00001.m4s"} {
		dir := filepath.Join(root, "b-"+blocker)
		if err := os.MkdirAll(filepath.Join(dir, blocker), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WriteDir(dir); err == nil {
			t.Errorf("a directory named %s", blocker)
		}
	}
}

func TestSegmentedTrackNamesAndDuration(t *testing.T) {
	tr := SegmentedTrack{Name: "audio2", Kind: Audio, Timescale: 1000,
		Segments: []MediaSegment{{Duration: 1500}, {Duration: 500}}}
	if tr.InitName() != "audio2_init.mp4" || tr.SegmentName(2) != "audio2_00003.m4s" || tr.Duration() != 2 {
		t.Errorf("%s %s %v", tr.InitName(), tr.SegmentName(2), tr.Duration())
	}
}

func FuzzSegmentBoxes(f *testing.F) {
	f.Add([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'})
	f.Add([]byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 0, 0, 0, 0, 0, 0, 0, 16})
	f.Fuzz(func(t *testing.T, b []byte) {
		prev := 0
		for _, box := range segmentBoxes(b) {
			if box.start != prev || box.end <= box.start || box.end > len(b) {
				t.Fatalf("box %+v after %d in %d bytes", box, prev, len(b))
			}
			prev = box.end
		}
	})
}

func FuzzSegment(f *testing.F) {
	for _, name := range []string{"tiny.mp4", "ffmpeg-aaclc.mp4", "subtitles-webvtt.mkv", "tiny.mkv"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := NewReader(b)
		if err != nil {
			return
		}
		// Whatever the input, segmenting returns an error or a result whose
		// segments tile each track without overlapping.
		seg, err := Segment(r, SegmentDuration(time.Second), IndexedSegments())
		if err != nil {
			return
		}
		for _, tr := range seg.Tracks {
			var end uint64
			for _, s := range tr.Segments {
				if s.Start < end && tr.Kind != Subtitle {
					t.Fatalf("%s: segment starts at %d before the last ended at %d", tr.Name, s.Start, end)
				}
				end = s.Start + s.Duration
			}
		}
	})
}

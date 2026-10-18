// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// The fixtures under testdata/subtitles-* were written by ffmpeg 9.0.2 from the
// cue files below, and are what the tests hold this package against: ffmpeg is
// the reference both for what a Matroska text track looks like and, through
// its own reading of what this package writes, for whether a round trip kept
// every cue (see the pull request for that half, which needs ffmpeg to run).
//
//	ffmpeg -i cues.vtt -c:s copy subtitles-webvtt.mkv   (and .webm)
//	ffmpeg -i cues.srt -c:s copy subtitles-srt.mkv
//	ffmpeg -i overlap.vtt -c:s copy subtitles-overlap.mkv
//	ffmpeg -i cues.srt -c:s ttml -time_base:s 1:1000 subtitles-ttml.mp4
//
// cues.vtt holds three cues: "first" at 0.5-1.5 s with settings, an anonymous
// two-line one at 2-3.25 s, and "third" at 3.25-4 s, which starts where the
// second ends. overlap.vtt holds two cues that overlap and a third inside both.

// shownCue is one cue as a test states it: what it says and when.
type shownCue struct {
	id, settings, text string
	comments           []string
}

// shownSample is one wvtt sample: how long it lasts and the cues it shows,
// none for a vtte.
type shownSample struct {
	duration uint32
	cues     []shownCue
}

func subtitleFixture(t *testing.T, name string) *Reader {
	t.Helper()
	r, err := NewReader(readFixture(t, name))
	if err != nil {
		t.Fatalf("NewReader(%s): %v", name, err)
	}
	return r
}

// shown takes wvtt samples apart with mp4ff's own box decoders, through the
// same reading every muxer uses.
func shown(t *testing.T, samples []Sample) []shownSample {
	t.Helper()
	var out []shownSample
	for i, s := range samples {
		cues, err := readWebVTTSample(s.Data)
		if err != nil {
			t.Fatalf("sample %d: %v", i+1, err)
		}
		if !s.Sync {
			t.Errorf("sample %d is not a sync sample; every cue sample is one", i+1)
		}
		got := shownSample{duration: s.Duration}
		for _, c := range cues {
			got.cues = append(got.cues, shownCue{id: c.id, settings: c.settings, text: c.text, comments: c.comments})
		}
		out = append(out, got)
	}
	return out
}

func assertShown(t *testing.T, what string, got, want []shownSample) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d samples, want %d:\n got %+v\nwant %+v", what, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].duration != want[i].duration || len(got[i].cues) != len(want[i].cues) {
			t.Fatalf("%s sample %d = %+v, want %+v", what, i+1, got[i], want[i])
		}
		for j := range want[i].cues {
			g, w := got[i].cues[j], want[i].cues[j]
			if g.id != w.id || g.settings != w.settings || g.text != w.text ||
				strings.Join(g.comments, "|") != strings.Join(w.comments, "|") {
				t.Fatalf("%s sample %d cue %d = %+v, want %+v", what, i+1, j+1, g, w)
			}
		}
	}
}

var (
	cueFirst  = shownCue{id: "first", settings: "align:start line:10%", text: "Hello, <b>world</b>."}
	cueSecond = shownCue{text: "Second cue\non two lines."}
	cueThird  = shownCue{id: "third", settings: "position:20%", text: "Ünïcödé — third."}
	// threeCues is the timeline of cues.vtt as ISO/IEC 14496-30 lays it out:
	// nothing until 0.5 s, the first cue, nothing until 2 s, then the second
	// and the third back to back, with no empty sample between them.
	threeCues = []shownSample{
		{duration: 500},
		{duration: 1000, cues: []shownCue{cueFirst}},
		{duration: 500},
		{duration: 1250, cues: []shownCue{cueSecond}},
		{duration: 750, cues: []shownCue{cueThird}},
	}
)

func TestAWebVTTTrackOfMatroskaReadsAsTheSamplesOfAWvttTrack(t *testing.T) {
	// Both of the forms ffmpeg writes: .mkv and .webm hold the same WebM
	// mapping, D_WEBVTT/SUBTITLES.
	for _, name := range []string{"subtitles-webvtt.mkv", "subtitles-webvtt.webm"} {
		r := subtitleFixture(t, name)
		cfg, err := r.TrackConfig(1)
		if err != nil {
			t.Fatalf("%s TrackConfig: %v", name, err)
		}
		if cfg.Kind != Subtitle || cfg.Codec != "wvtt" || cfg.Timescale != 1000 || len(cfg.CodecConfig) != 0 {
			t.Fatalf("%s config = %+v, want a wvtt subtitle track in milliseconds with no header", name, cfg)
		}
		if tr := r.File().Tracks[0]; tr.Codec != "wvtt" || tr.Kind != Subtitle || tr.Duration != 4000 {
			t.Fatalf("%s track = %+v, want wvtt lasting 4000 ms", name, tr)
		}
		samples, err := r.Samples(1)
		if err != nil {
			t.Fatalf("%s Samples: %v", name, err)
		}
		assertShown(t, name, shown(t, samples), threeCues)
	}
}

func TestAnSRTTrackOfMatroskaReadsAsWebVTTCuesWithItsTextAsItStands(t *testing.T) {
	r := subtitleFixture(t, "subtitles-srt.mkv")
	cfg, err := r.TrackConfig(1)
	if err != nil || cfg.Codec != "wvtt" || cfg.Kind != Subtitle {
		t.Fatalf("TrackConfig = %+v, %v, want wvtt", cfg, err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	// SRT has no identifier and no settings; its <b> is WebVTT's own.
	assertShown(t, "srt", shown(t, samples), []shownSample{
		{duration: 500},
		{duration: 1000, cues: []shownCue{{text: "Hello, <b>world</b>."}}},
		{duration: 500},
		{duration: 1250, cues: []shownCue{{text: "Second cue\non two lines."}}},
		{duration: 750, cues: []shownCue{{text: "Ünïcödé — third."}}},
	})
}

func TestOverlappingCuesAreCutIntoSamplesThatEachShowEveryCueOnScreen(t *testing.T) {
	r := subtitleFixture(t, "subtitles-overlap.mkv")
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	roll1 := shownCue{id: "roll1", settings: "line:80%", text: "First line stays."}
	roll2 := shownCue{id: "roll2", settings: "line:90%", text: "Second line overlaps."}
	third := shownCue{text: "Third, inside both."}
	assertShown(t, "overlap", shown(t, samples), []shownSample{
		{duration: 1000},
		{duration: 1000, cues: []shownCue{roll1}},
		{duration: 1000, cues: []shownCue{roll1, roll2}},
		{duration: 500, cues: []shownCue{roll1, roll2, third}},
		{duration: 500, cues: []shownCue{roll1, roll2}},
		{duration: 1000, cues: []shownCue{roll2}},
		{duration: 2250},
		{duration: 750, cues: []shownCue{{text: "After a gap."}}},
	})
	// The two cues shown in several samples say they are one cue each; the
	// two shown once need no source id.
	sources := map[string]map[uint32]bool{}
	for _, s := range samples {
		cues, _ := readWebVTTSample(s.Data)
		for _, c := range cues {
			if sources[c.text] == nil {
				sources[c.text] = map[uint32]bool{}
			}
			sources[c.text][c.source] = true
		}
	}
	for text, ids := range sources {
		split := text == roll1.text || text == roll2.text
		if len(ids) != 1 || (split && ids[0]) || (!split && !ids[0]) {
			t.Errorf("cue %q carries source ids %v", text, ids)
		}
	}
}

// mkvCueBlock is one block of a Matroska text track, as ebml-go reads it.
type mkvCueBlock struct {
	time, duration int64
	data           string
	additional     []byte
	codecID        string
	private        []byte
	trackType      uint64
	docType        string
}

// mkvCueBlocks reads every block of a file's first track, in file order.
func mkvCueBlocks(t *testing.T, data []byte) []mkvCueBlock {
	t.Helper()
	doc, groups, err := unmarshalMatroska(data)
	if err != nil {
		t.Fatalf("read back with ebml-go: %v", err)
	}
	refs, err := blockOrder(groups, doc.Segment.Cluster)
	if err != nil {
		t.Fatalf("blockOrder: %v", err)
	}
	te := doc.Segment.Tracks.TrackEntry[0]
	var out []mkvCueBlock
	for _, ref := range refs {
		if ref.group == nil {
			t.Fatalf("a cue was written as a simple block, which cannot state its duration")
		}
		b := mkvBlockOf(ref)
		out = append(out, mkvCueBlock{time: b.time, duration: b.stated, data: string(b.frames[0]),
			additional: b.additional, codecID: te.CodecID, private: te.CodecPrivate,
			trackType: te.TrackType, docType: doc.Header.DocType})
	}
	return out
}

// webmOf writes every track of a reader with WebMMuxer.
func webmOf(t *testing.T, r *Reader, opts ...WebMOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeAll(t, NewWebMMuxer(&buf, opts...), r); err != nil {
		t.Fatalf("write webm: %v", err)
	}
	return buf.Bytes()
}

// closer is a sampleWriter that can be closed, which every muxer is.
type closer interface {
	sampleWriter
	Close() error
}

// writeAll copies a reader into a muxer in decoding order, as Join does.
func writeAll(t *testing.T, m closer, r *Reader) error {
	t.Helper()
	if err := joinInto(m, []*Reader{r}); err != nil {
		return err
	}
	return m.Close()
}

func TestWebMMuxerWritesWebVTTCuesExactlyAsFFmpegDid(t *testing.T) {
	// The blocks ffmpeg wrote and the blocks written from what was read out
	// of them must be the same: same times, same durations, same bytes.
	for _, name := range []string{"subtitles-webvtt.mkv", "subtitles-webvtt.webm", "subtitles-overlap.mkv"} {
		original, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		want := mkvCueBlocks(t, original)
		for _, mode := range []struct {
			name string
			opts []WebMOption
		}{{"buffered", []WebMOption{BufferedSegment()}}, {"streamed", nil}} {
			got := mkvCueBlocks(t, webmOf(t, subtitleFixture(t, name), mode.opts...))
			if len(got) != len(want) {
				t.Fatalf("%s %s: %d blocks, want %d", name, mode.name, len(got), len(want))
			}
			for i := range want {
				g, w := got[i], want[i]
				if g.time != w.time || g.duration != w.duration || g.data != w.data || g.additional != nil {
					t.Errorf("%s %s block %d = %+v, want %+v", name, mode.name, i+1, g, w)
				}
				if g.codecID != "D_WEBVTT/SUBTITLES" || g.private != nil || g.trackType != mkvTrackSubtitle ||
					g.docType != "webm" {
					t.Errorf("%s %s track = %s %v type %d in %s, want D_WEBVTT/SUBTITLES with no private data, type 17, in webm",
						name, mode.name, g.codecID, g.private, g.trackType, g.docType)
				}
			}
		}
	}
}

func TestAWebVTTTrackGoesThroughBothMP4MuxersAndComesBackTheSame(t *testing.T) {
	r := subtitleFixture(t, "subtitles-overlap.mkv")
	want, _ := r.Samples(1)
	for _, mux := range []struct {
		name string
		new  func(io.Writer) closer
	}{
		{"fragmented", func(w io.Writer) closer { return NewMuxer(w) }},
		{"progressive", func(w io.Writer) closer { return NewProgressiveMuxer(w) }},
	} {
		var buf bytes.Buffer
		if err := writeAll(t, mux.new(&buf), r); err != nil {
			t.Fatalf("%s: %v", mux.name, err)
		}
		cfgs, samples := readBack(t, buf.Bytes())
		if cfgs[0].Codec != "wvtt" || cfgs[0].Kind != Subtitle || string(cfgs[0].CodecConfig) != "WEBVTT" {
			t.Fatalf("%s config = %+v", mux.name, cfgs[0])
		}
		assertSamples(t, mux.name, samples[0], want)
		// ISO/IEC 14496-30: a WebVTT track has the 'text' handler and a
		// null media header, and its entry carries the header in vttC.
		parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		trak := parsed.Moov.Traks[0]
		nmhd := false
		for _, child := range trak.Mdia.Minf.Children {
			_, isNull := child.(*mp4.NmhdBox)
			nmhd = nmhd || isNull
		}
		if trak.Mdia.Hdlr.HandlerType != "text" || !nmhd {
			t.Errorf("%s handler %q, nmhd %v: want text and a null media header",
				mux.name, trak.Mdia.Hdlr.HandlerType, nmhd)
		}
		if w := trak.Mdia.Minf.Stbl.Stsd.Wvtt; w == nil || w.VttC == nil || w.VttC.Config != "WEBVTT" {
			t.Errorf("%s sample entry = %+v, want wvtt with vttC WEBVTT", mux.name, w)
		}
	}
}

// styledHeader is a WebVTT header that says more than the signature, which the
// WebM mapping has nowhere to put.
const styledHeader = "WEBVTT\n\nSTYLE\n::cue(b) {\n  color: peachpuff;\n}"

// cueSample encodes the boxes of one wvtt sample.
func cueSample(t *testing.T, boxes ...mp4.Box) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, b := range boxes {
		if err := b.Encode(&buf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func vttc(id, settings, text string) *mp4.VttcBox {
	return cueBox(&webvttCue{id: id, settings: settings, text: text})
}

func TestAStyledHeaderIsWrittenInTheMatroskaMappingWithEverythingBesideTheText(t *testing.T) {
	cfg := TrackConfig{Kind: Subtitle, Codec: "wvtt", Timescale: 1000, CodecConfig: []byte(styledHeader)}
	samples := []Sample{
		{Data: vtteSample, Duration: 1000, Sync: true},
		// A NOTE before the cue, settings, an identifier, and a timestamp
		// inside the text, which an MP4 counts from the presentation.
		{Data: cueSample(t, &mp4.VttaBox{CueAdditionalText: "NOTE one"},
			&mp4.VttaBox{CueAdditionalText: "NOTE two\nlines"},
			vttc("karaoke", "align:end", "Hi <00:00:01.500>there")), Duration: 2000, Sync: true},
		{Data: cueSample(t, vttc("", "", "plain")), Duration: 500, Sync: true},
	}
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for _, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("WriteSample: %v", err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	blocks := mkvCueBlocks(t, buf.Bytes())
	if len(blocks) != 2 {
		t.Fatalf("%d blocks, want 2", len(blocks))
	}
	b := blocks[0]
	if b.codecID != "S_TEXT/WEBVTT" || string(b.private) != styledHeader || b.docType != "matroska" {
		t.Fatalf("track = %s %q in %s, want S_TEXT/WEBVTT carrying the header, in matroska",
			b.codecID, b.private, b.docType)
	}
	// The Matroska mapping: the text alone in the block, its timestamps made
	// relative to the block; settings, identifier and comments beside it.
	if b.time != 1000 || b.duration != 2000 || b.data != "Hi <00:00:00.500>there" ||
		string(b.additional) != "align:end\nkaraoke\nNOTE one\n\nNOTE two\nlines" {
		t.Fatalf("first block = %+v (addition %q)", b, b.additional)
	}
	// A cue stating none of the three has no addition at all.
	if blocks[1].data != "plain" || blocks[1].additional != nil {
		t.Fatalf("second block = %+v", blocks[1])
	}
	// And it reads back as it was written.
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, _ := r.TrackConfig(1)
	if string(got.CodecConfig) != styledHeader {
		t.Fatalf("header read back = %q", got.CodecConfig)
	}
	back, err := r.Samples(1)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	assertShown(t, "styled", shown(t, back), []shownSample{
		{duration: 1000},
		{duration: 2000, cues: []shownCue{{id: "karaoke", settings: "align:end", text: "Hi <00:00:01.500>there",
			comments: []string{"NOTE one", "NOTE two\nlines"}}}},
		{duration: 500, cues: []shownCue{{text: "plain"}}},
	})
}

func TestTheWebMMappingKeepsTimestampsInsideTheTextAsTheyStand(t *testing.T) {
	// ffmpeg writes them unchanged in D_WEBVTT, and reads them back so.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment())
	id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
	m.WriteSample(id, Sample{Data: vtteSample, Duration: 1000, Sync: true})
	m.WriteSample(id, Sample{Data: cueSample(t, &mp4.VttaBox{CueAdditionalText: "NOTE dropped"},
		vttc("", "", "a <00:01.500>b")), Duration: 1000, Sync: true})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := mkvCueBlocks(t, buf.Bytes())
	if len(blocks) != 1 || blocks[0].data != "\n\na <00:01.500>b" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestACueShownAcrossSeveralSamplesIsWrittenAsOneBlock(t *testing.T) {
	// The pieces of one cue are told apart from two cues by what they say,
	// and by their source id when they carry one.
	a := vttc("a", "", "same")
	split := cueBox(&webvttCue{text: "split", source: 7})
	samples := []Sample{
		{Data: cueSample(t, a), Duration: 100, Sync: true},
		{Data: cueSample(t, a, split), Duration: 100, Sync: true},
		{Data: cueSample(t, split), Duration: 100, Sync: true},
		{Data: cueSample(t, cueBox(&webvttCue{text: "split", source: 8})), Duration: 100, Sync: true},
	}
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, _ := m.AddTrack(TrackConfig{Codec: "D_WEBVTT/SUBTITLES", Timescale: 1000})
	for _, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := mkvCueBlocks(t, buf.Bytes())
	want := []struct {
		time, duration int64
		data           string
	}{{0, 200, "a\n\nsame"}, {100, 200, "\n\nsplit"}, {300, 100, "\n\nsplit"}}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i, w := range want {
		if blocks[i].time != w.time || blocks[i].duration != w.duration || blocks[i].data != w.data {
			t.Errorf("block %d = %+v, want %+v", i+1, blocks[i], w)
		}
	}
}

func TestACueThatEndsAfterTheNextOneStartsWaitsForItInStartOrder(t *testing.T) {
	// A long cue and a short one inside it: the short one ends first, and is
	// still written after the long one, which started before it.
	a := &cueAssembler{}
	long, short := webvttCue{text: "long"}, webvttCue{text: "short"}
	if done := a.add(0, 10, []webvttCue{long}); len(done) != 0 {
		t.Fatalf("finished %v before anything ended", done)
	}
	if done := a.add(10, 20, []webvttCue{long, short}); len(done) != 0 {
		t.Fatalf("finished %v", done)
	}
	if done := a.add(20, 30, []webvttCue{long}); len(done) != 0 {
		t.Fatalf("short was handed over before long, which started earlier: %v", done)
	}
	done := a.add(30, 40, nil)
	if len(done) != 2 || done[0].text != "long" || done[0].start != 0 || done[0].end != 30 ||
		done[1].text != "short" || done[1].start != 10 || done[1].end != 20 {
		t.Fatalf("finished %+v", done)
	}
	if left := a.drain(); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

func TestAStreamedFileWritesTheCuesStillOnScreenWhenItCloses(t *testing.T) {
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
	if err := m.WriteSample(id, Sample{Data: cueSample(t, vttc("", "", "last")), Duration: 700, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := mkvCueBlocks(t, buf.Bytes())
	if len(blocks) != 1 || blocks[0].duration != 700 || blocks[0].data != "\n\nlast" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestACueBlockBeforeItsClusterIsStatedRelativeToIt(t *testing.T) {
	// The video track decides where clusters start; a cue is written when it
	// ends, by which time a cluster may have opened after it began.
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, BufferedSegment(), ClusterDuration(100*time.Millisecond))
	video, _ := m.AddTrack(webmVideoConfig())
	text, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
	vcfg := webmVideoConfig()
	frame := uint32(vcfg.Timescale / 10) // 100 ms
	m.WriteSample(text, Sample{Data: cueSample(t, vttc("", "", "long")), Duration: 300, Sync: true})
	for i := 0; i < 4; i++ {
		if err := m.WriteSample(video, Sample{Data: []byte{byte(i)}, Duration: frame, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	m.WriteSample(text, Sample{Data: vtteSample, Duration: 100, Sync: true})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	var cues []webmBlock
	for _, b := range webmBlocks(readWebM(t, buf.Bytes())) {
		if b.track == uint64(text) {
			cues = append(cues, b)
		}
	}
	if len(cues) != 1 || cues[0].tick != 0 || cues[0].relative >= 0 || cues[0].duration != 300 {
		t.Fatalf("cue blocks = %+v, want one at 0 stated before its cluster", cues)
	}
}

func TestTTMLGoesFromMP4ToMP4Unchanged(t *testing.T) {
	original, err := os.ReadFile("testdata/subtitles-ttml.mp4")
	if err != nil {
		t.Fatal(err)
	}
	r := subtitleFixture(t, "subtitles-ttml.mp4")
	cfg, err := r.TrackConfig(1)
	if err != nil || cfg.Codec != "stpp" || cfg.Kind != Subtitle {
		t.Fatalf("TrackConfig = %+v, %v", cfg, err)
	}
	want, _ := r.Samples(1)
	src, _ := mp4.DecodeFile(bytes.NewReader(original))
	wantEntry, _ := boxPayload(src.Moov.Traks[0].Mdia.Minf.Stbl.Stsd.Stpp)
	for _, out := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{"Remux", func(w io.Writer) error { return Remux(w, r) }},
		{"JoinProgressive", func(w io.Writer) error { return JoinProgressive(w, []*Reader{r}) }},
	} {
		var buf bytes.Buffer
		if err := out.write(&buf); err != nil {
			t.Fatalf("%s: %v", out.name, err)
		}
		_, samples := readBack(t, buf.Bytes())
		assertSamples(t, out.name, samples[0], want)
		parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		trak := parsed.Moov.Traks[0]
		gotEntry, _ := boxPayload(trak.Mdia.Minf.Stbl.Stsd.Stpp)
		if !bytes.Equal(gotEntry, wantEntry) {
			t.Errorf("%s stpp entry = %q, want %q", out.name, gotEntry, wantEntry)
		}
		if trak.Mdia.Hdlr.HandlerType != "subt" || trak.Mdia.Minf.Sthd == nil {
			t.Errorf("%s: handler %q, want subt with a subtitle media header", out.name, trak.Mdia.Hdlr.HandlerType)
		}
	}
}

func TestATTMLSampleEntryThatDescribesNothingIsRefused(t *testing.T) {
	noNamespace := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0}
	for _, c := range []struct {
		name   string
		config []byte
	}{
		{"absent", nil},
		{"too short to decode", []byte{0, 0, 0}},
		{"no namespace", noNamespace},
	} {
		_, err := NewMuxer(io.Discard).AddTrack(TrackConfig{Codec: "stpp", Timescale: 1000, CodecConfig: c.config})
		if !errors.Is(err, ErrTrackConfig) {
			t.Errorf("%s: AddTrack = %v, want ErrTrackConfig", c.name, err)
		}
	}
}

func TestAWebVTTHeaderThatIsNotOneIsRefusedEverywhere(t *testing.T) {
	cfg := TrackConfig{Codec: "wvtt", Timescale: 1000, CodecConfig: []byte("STYLE\nnot a header")}
	if _, err := NewMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("Muxer: %v", err)
	}
	if _, err := NewWebMMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("WebMMuxer: %v", err)
	}
	// A very long first line is shortened in the message.
	_, err := webvttConfig(TrackConfig{CodecConfig: []byte(strings.Repeat("x", 100))})
	if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 33)) {
		t.Errorf("error = %v", err)
	}
	// And a Matroska file stating one fails that track only.
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackSubtitle, CodecID: "S_TEXT/WEBVTT",
		CodecPrivate: []byte("NOTE no signature")}
	if _, err := mkvTrackConfig(te, 1000); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("mkvTrackConfig = %v", err)
	}
}

func TestSubtitlesAreRefusedClearlyByTheTransportStreamMuxer(t *testing.T) {
	for _, codec := range []string{"wvtt", "stpp"} {
		_, err := NewTSMuxer(io.Discard).AddTrack(TrackConfig{Kind: Subtitle, Codec: codec, Timescale: 1000})
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "subtitles") {
			t.Errorf("%s: %v", codec, err)
		}
	}
	// TTML has no Matroska mapping this package writes.
	if _, err := NewWebMMuxer(io.Discard).AddTrack(TrackConfig{Codec: "stpp", Timescale: 1000}); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("WebMMuxer stpp: %v", err)
	}
}

func TestAWebVTTSampleThatIsNotOneIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	cases := map[string][]byte{
		"undecodable":      {0, 0, 0, 9, 'v', 't', 't'},
		"a foreign box":    cueSample(t, &mp4.FreeBox{}),
		"vtte beside cues": cueSample(t, &mp4.VtteBox{}, vttc("", "", "x")),
		"vtte after notes": cueSample(t, &mp4.VttaBox{CueAdditionalText: "NOTE"}, &mp4.VtteBox{}),
	}
	for name, data := range cases {
		if _, err := readWebVTTSample(data); !errors.Is(err, ErrSample) {
			t.Errorf("%s: %v, want ErrSample", name, err)
		}
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf)
		id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
		if err := m.WriteSample(id, Sample{Data: data, Duration: 10}); !errors.Is(err, ErrSample) {
			t.Errorf("%s: WriteSample = %v", name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: %d bytes written for a refused sample", name, buf.Len())
		}
	}
	// A NOTE that precedes no cue has nowhere to go, and is not an error.
	if cues, err := readWebVTTSample(cueSample(t, &mp4.VttaBox{CueAdditionalText: "NOTE"})); err != nil || len(cues) != 0 {
		t.Errorf("a lone NOTE = %v, %v", cues, err)
	}
}

// cueDoc is a Matroska document with one text track and the given blocks.
func cueDoc(codecID string, private []byte, defaultDuration uint64, groups ...mkvBlockGroup) *mkvReadDoc {
	return &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: 1_000_000},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{TrackNumber: 1, TrackType: mkvTrackSubtitle,
				CodecID: codecID, CodecPrivate: private, DefaultDuration: defaultDuration}}},
			Cluster: []mkvCluster{{Timecode: 1000, BlockGroup: groups}},
		},
	}
}

func cueGroup(timecode int16, duration uint64, data string, addition []byte) mkvBlockGroup {
	g := mkvBlockGroup{BlockDuration: duration,
		Block: ebml.Block{TrackNumber: 1, Timecode: timecode, Keyframe: true, Data: [][]byte{[]byte(data)}}}
	if addition != nil {
		g.BlockAdditions = &mkvBlockAdditions{BlockMore: []mkvBlockMore{
			{BlockAddID: 2, BlockAdditional: []byte("another addition")},
			{BlockAdditional: addition},
		}}
	}
	return g
}

func TestMatroskaCuesThatCannotBeTimedOrReadAreRefusedForTheirTrack(t *testing.T) {
	laced := cueGroup(0, 100, "x", nil)
	laced.Block.Data = [][]byte{[]byte("a"), []byte("b")}
	laced.Block.Lacing = ebml.LacingXiph
	for _, c := range []struct {
		name string
		doc  *mkvReadDoc
	}{
		{"no duration", cueDoc("S_TEXT/UTF8", nil, 0, cueGroup(0, 0, "x", nil))},
		{"laced", cueDoc("S_TEXT/UTF8", nil, 0, laced)},
		{"before the segment", cueDoc("S_TEXT/UTF8", nil, 0, cueGroup(-1001, 100, "x", nil))},
		{"WebM block with no line", cueDoc("D_WEBVTT/SUBTITLES", nil, 0, cueGroup(0, 100, "no newline", nil))},
		{"WebM block with one line", cueDoc("D_WEBVTT/SUBTITLES", nil, 0, cueGroup(0, 100, "id\nno settings line", nil))},
		{"addition with no line", cueDoc("S_TEXT/WEBVTT", nil, 0, cueGroup(0, 100, "x", []byte("settings")))},
		{"addition with one line", cueDoc("S_TEXT/WEBVTT", nil, 0, cueGroup(0, 100, "x", []byte("settings\nid")))},
	} {
		r, err := NewReader(marshalDoc(t, c.doc))
		if err != nil {
			t.Fatalf("%s: NewReader: %v", c.name, err)
		}
		if _, err := r.Samples(1); !errors.Is(err, ErrMatroska) {
			t.Errorf("%s: Samples = %v, want ErrMatroska", c.name, err)
		}
	}
	// A text track with no block holds no sample.
	r, _ := NewReader(marshalDoc(t, cueDoc("S_TEXT/UTF8", nil, 0)))
	if _, err := r.Samples(1); !errors.Is(err, ErrNoSamples) {
		t.Errorf("empty track: %v", err)
	}
}

func TestAMatroskaCueTakesItsDurationFromTheTrackWhenItStatesNone(t *testing.T) {
	r, err := NewReader(marshalDoc(t, cueDoc("S_TEXT/UTF8", nil, 250_000_000, cueGroup(0, 0, "x\r\n", nil))))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	assertShown(t, "default duration", shown(t, samples), []shownSample{
		{duration: 1000}, {duration: 250, cues: []shownCue{{text: "x"}}},
	})
}

func TestAnSTextWebVTTCueReadsItsAdditionAndCountsItsTimestampsFromThePresentation(t *testing.T) {
	doc := cueDoc("S_TEXT/WEBVTT", []byte(styledHeader), 0,
		cueGroup(500, 1000, "sing <00:00.250>now", []byte("line:0\nid1\nNOTE a\n\nNOTE b\n")),
		cueGroup(2000, 100, "bare", nil))
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	assertShown(t, "S_TEXT/WEBVTT", shown(t, samples), []shownSample{
		{duration: 1500},
		{duration: 1000, cues: []shownCue{{id: "id1", settings: "line:0", text: "sing <00:01.750>now",
			comments: []string{"NOTE a", "NOTE b"}}}},
		{duration: 500},
		{duration: 100, cues: []shownCue{{text: "bare"}}},
	})
}

func TestTheWebMMappingAcceptsCarriageReturnsLikeFFmpeg(t *testing.T) {
	c, err := readWebMCueBlock([]byte("id\r\nalign:end\r\ntext\r\n\r\n"))
	if err != nil || c.id != "id" || c.settings != "align:end" || c.text != "text" {
		t.Fatalf("cue = %+v, %v", c, err)
	}
}

func TestDescriptionsAndMetadataTracksOfTheWebMMappingReadAsSubtitles(t *testing.T) {
	doc := cueDoc("D_WEBVTT/METADATA", nil, 0, cueGroup(0, 100, "\n\n{\"k\":1}", nil))
	doc.Segment.Tracks.TrackEntry[0].TrackType = 0x21
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if tr := r.File().Tracks[0]; tr.Kind != Subtitle || tr.Codec != "wvtt" {
		t.Fatalf("track = %+v", tr)
	}
}

func TestAStretchLongerThanASampleCanLastIsRefused(t *testing.T) {
	_, err := webvttSamples([]webvttCue{{text: "late", start: math.MaxUint32 + 1, end: math.MaxUint32 + 2}})
	if !errors.Is(err, ErrMatroska) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnEncoderFailureIsReportedNotWritten(t *testing.T) {
	// encodeBox writes into a buffer and cannot fail; staged, it says where.
	saved := encodeBox
	defer func() { encodeBox = saved }()
	boom := errors.New("boom")
	for _, failOn := range []string{"vtta", "vttc"} {
		encodeBox = func(b mp4.Box, w io.Writer) error {
			if b.Type() == failOn {
				return boom
			}
			return b.Encode(w)
		}
		_, err := webvttSamples([]webvttCue{{text: "x", comments: []string{"NOTE"}, start: 0, end: 10}})
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), failOn) {
			t.Errorf("failing on %s: %v", failOn, err)
		}
	}
}

func TestTimestampsInsideACueMoveAndNothingElseDoes(t *testing.T) {
	for _, c := range []struct {
		in    string
		delta time.Duration
		want  string
	}{
		{"no tags", time.Hour, "no tags"},
		{"a <b>bold</b> <00:01.000>x", time.Second, "a <b>bold</b> <00:02.000>x"},
		{"<01:00:01.000>", -time.Hour, "<00:00:01.000>"},
		{"<59:59.999>", time.Millisecond, "<01:00:00.000>"},
		{"<123:00:00.000>", time.Second, "<123:00:01.000>"},
		{"<00:01.000>", 0, "<00:01.000>"},
		// Not timestamps: left exactly as they were.
		{"<1:00.000> <00:60.000> <00:01.00> <0:00:01.000> <a:bc.def> <00.000> <1:2:3:4.000> <c.x> unclosed <00:01.000",
			time.Second,
			"<1:00.000> <00:60.000> <00:01.00> <0:00:01.000> <a:bc.def> <00.000> <1:2:3:4.000> <c.x> unclosed <00:01.000"},
		{"<00:aa.000>", time.Second, "<00:aa.000>"},
		{"<99999999999:00:00.000>", time.Second, "<99999999999:00:00.000>"},
	} {
		got, err := shiftCueTimestamps(c.in, c.delta)
		if err != nil || got != c.want {
			t.Errorf("shift(%q, %v) = %q, %v, want %q", c.in, c.delta, got, err, c.want)
		}
	}
	if _, err := shiftCueTimestamps("<00:01.000>", -2*time.Second); !errors.Is(err, ErrSample) {
		t.Errorf("a timestamp moved before zero: %v", err)
	}
	if !hasCueTimestamp("x <00:00.000> y") || hasCueTimestamp("x <b>y</b>") {
		t.Error("hasCueTimestamp does not tell a timestamp from a tag")
	}
}

func TestATimestampBeforeItsCueCannotBeWrittenInTheMatroskaMapping(t *testing.T) {
	// The mapping counts it from the block, where it would be negative. It
	// is refused when the cue is written, and the cue at Close as well.
	for _, closing := range []bool{false, true} {
		m := NewWebMMuxer(io.Discard)
		id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000, CodecConfig: []byte(styledHeader)})
		m.WriteSample(id, Sample{Data: vtteSample, Duration: 5000, Sync: true})
		m.WriteSample(id, Sample{Data: cueSample(t, vttc("", "", "<00:01.000>early")), Duration: 100, Sync: true})
		var err error
		if closing {
			err = m.Close()
		} else {
			err = m.WriteSample(id, Sample{Data: vtteSample, Duration: 100, Sync: true})
		}
		if !errors.Is(err, ErrSample) {
			t.Errorf("closing=%v: %v", closing, err)
		}
	}
}

func TestACueBlockThatCannotBeWrittenIsReported(t *testing.T) {
	// Every write of a streamed file goes out as it is made, so a writer that
	// stops part way fails the cluster or the block group of a cue.
	sample := Sample{Data: cueSample(t, vttc("", "", "x")), Duration: 100, Sync: true}
	var full bytes.Buffer
	m := NewWebMMuxer(&full)
	id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
	m.WriteSample(id, sample)
	m.Close()
	failures := 0
	for limit := 0; limit < full.Len(); limit++ {
		w := &webmShortWriter{limit: limit}
		m := NewWebMMuxer(w)
		id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
		err := m.WriteSample(id, sample)
		if err == nil {
			err = m.Close()
		}
		if err != nil {
			failures++
			if !errors.Is(err, errWebMShortWriter) {
				t.Fatalf("limit %d: %v", limit, err)
			}
		}
	}
	if failures == 0 {
		t.Fatal("no write failed")
	}
}

// subtitleClip is a video track of 10 frames of 100 ms, a sync sample every
// fifth, beside a WebVTT track whose cues are given.
func subtitleClip(t *testing.T, cues ...Sample) *Reader {
	t.Helper()
	return fixtureReader(t,
		fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 10, 100, 5)},
		fixtureTrack{cfg: TrackConfig{Kind: Subtitle, Codec: "wvtt", Timescale: 1000}, samples: cues})
}

func TestACutStartsTheCuesExactlyWhereThePicturesStart(t *testing.T) {
	hello := cueSample(t, vttc("", "", "hello"))
	r := subtitleClip(t,
		Sample{Data: vtteSample, Duration: 300, Sync: true},
		Sample{Data: hello, Duration: 400, Sync: true},
		Sample{Data: vtteSample, Duration: 300, Sync: true})
	for _, c := range []struct {
		start time.Duration
		want  []shownSample
	}{
		// At 500 ms, inside the cue: the rest of it is shown from the start.
		{500 * time.Millisecond, []shownSample{{duration: 200, cues: []shownCue{{text: "hello"}}}, {duration: 300}}},
		// At zero nothing moves.
		{0, []shownSample{{duration: 300}, {duration: 400, cues: []shownCue{{text: "hello"}}}, {duration: 300}}},
	} {
		var buf bytes.Buffer
		if err := Cut(&buf, r, c.start, 0); err != nil {
			t.Fatalf("Cut at %v: %v", c.start, err)
		}
		_, samples := readBack(t, buf.Bytes())
		if len(samples[0]) != 10-int(c.start/(100*time.Millisecond)) {
			t.Fatalf("cut at %v kept %d video samples", c.start, len(samples[0]))
		}
		assertShown(t, "cut", shown(t, samples[1]), c.want)
	}
}

func TestACutPastTheLastCueLeavesTheTextTrackEmpty(t *testing.T) {
	r := subtitleClip(t, Sample{Data: cueSample(t, vttc("", "", "x")), Duration: 200, Sync: true})
	tracks, err := readSource(r, &remuxSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if err := trim(tracks, 500*time.Millisecond, 0, false); err != nil {
		t.Fatal(err)
	}
	if len(tracks[1].samples) != 0 {
		t.Fatalf("kept %d cue samples past the end of the track", len(tracks[1].samples))
	}
	// And an empty kept range moves nothing.
	if kept, err := cutCues(tracks[1], nil, 0, 1, 1); err != nil || len(kept) != 0 {
		t.Fatalf("cutCues(nothing) = %v, %v", kept, err)
	}
}

func TestTextWhoseTimesWouldBeWrongAfterACutOrAJoinIsRefused(t *testing.T) {
	karaoke := cueSample(t, vttc("", "", "a <00:00.900>b"))
	ttml := TrackConfig{Kind: Subtitle, Codec: "stpp", Timescale: 1000,
		CodecConfig: append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, []byte("http://www.w3.org/ns/ttml\x00\x00\x00")...)}
	stamped := subtitleClip(t, Sample{Data: karaoke, Duration: 1000, Sync: true})
	ttmlClip := fixtureReader(t,
		fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 10, 100, 5)},
		fixtureTrack{cfg: ttml, samples: []Sample{{Data: []byte("<tt/>"), Duration: 1000, Sync: true}}})
	for _, r := range []*Reader{stamped, ttmlClip} {
		if err := Cut(io.Discard, r, 500*time.Millisecond, 0); !errors.Is(err, ErrTimeRange) {
			t.Errorf("Cut: %v, want ErrTimeRange", err)
		}
		if err := Concat(io.Discard, []*Reader{r, r}); !errors.Is(err, ErrTrackMismatch) {
			t.Errorf("Concat: %v, want ErrTrackMismatch", err)
		}
		// Copying them where they are is fine, and so is leaving them out.
		if err := Remux(io.Discard, r); err != nil {
			t.Errorf("Remux: %v", err)
		}
		if err := Concat(io.Discard, []*Reader{r, r}, DropTracks(2)); err != nil {
			t.Errorf("Concat without the text track: %v", err)
		}
	}
	// A sample that is not a WebVTT sample is reported, not passed over.
	broken := subtitleClip(t, Sample{Data: []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, Duration: 1000, Sync: true})
	if err := Concat(io.Discard, []*Reader{broken, broken}); !errors.Is(err, ErrSample) {
		t.Errorf("Concat of a broken sample: %v", err)
	}
}

func TestConcatenatedCuesFollowThePicturesNotTheLastCue(t *testing.T) {
	// The cue ends at 300 ms and the pictures at 1 s: the second input's cue
	// must start at 1 s, not at 300 ms.
	hi := cueSample(t, vttc("", "", "hi"))
	r := subtitleClip(t, Sample{Data: hi, Duration: 300, Sync: true})
	var buf bytes.Buffer
	if err := Concat(&buf, []*Reader{r, r}); err != nil {
		t.Fatal(err)
	}
	_, samples := readBack(t, buf.Bytes())
	assertShown(t, "concat", shown(t, samples[1]), []shownSample{
		{duration: 300, cues: []shownCue{{text: "hi"}}}, {duration: 700},
		{duration: 300, cues: []shownCue{{text: "hi"}}},
	})
}

func TestPaddingLongerThanOneSampleIsSplit(t *testing.T) {
	tracks := []sourceTrack{
		{cfg: TrackConfig{Codec: "av01", Timescale: 1}, samples: []Sample{{Duration: math.MaxUint32}, {Duration: 10}}},
		{cfg: TrackConfig{Codec: "wvtt", Timescale: 1}, samples: []Sample{{Data: vtteSample, Duration: 1}}},
	}
	padTimedText(tracks)
	got := tracks[1].samples
	// 1 + MaxUint32 + 9 = MaxUint32 + 10, the length of the video.
	if len(got) != 3 || got[1].Duration != math.MaxUint32 || got[2].Duration != 9 {
		t.Fatalf("padding = %+v", got)
	}
}

func TestJoinPutsASubtitleTrackBesideThePictures(t *testing.T) {
	video := fixtureReader(t, fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 10, 100, 5)})
	text := subtitleFixture(t, "subtitles-webvtt.mkv")
	var buf bytes.Buffer
	if err := Join(&buf, []*Reader{video, text}); err != nil {
		t.Fatal(err)
	}
	cfgs, samples := readBack(t, buf.Bytes())
	if len(cfgs) != 2 || cfgs[1].Codec != "wvtt" {
		t.Fatalf("configs = %+v", cfgs)
	}
	assertShown(t, "join", shown(t, samples[1]), threeCues)
}

func TestAnMP4WebVTTEntryWithoutAHeaderStatesNone(t *testing.T) {
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	id, _ := m.AddTrack(TrackConfig{Kind: Subtitle, Codec: "wvtt", Timescale: 1000})
	m.WriteSample(id, Sample{Data: vtteSample, Duration: 10, Sync: true})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// Drop the vttC, as a writer that states no header does.
	entry := parsed.Moov.Traks[0].Mdia.Minf.Stbl.Stsd.Wvtt
	entry.Children, entry.VttC = nil, nil
	var out bytes.Buffer
	if err := parsed.Encode(&out); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Codec != "wvtt" || cfg.CodecConfig != nil {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestTheHandlerOfATimedTextTrackFollowsItsCodec(t *testing.T) {
	cases := []struct {
		cfg  TrackConfig
		want string
	}{
		{TrackConfig{Kind: Subtitle, Codec: "wvtt"}, "wvtt"},
		{TrackConfig{Kind: Other, Codec: " STPP "}, "stpp"},
		{TrackConfig{Kind: Subtitle, Codec: "tx3g"}, "subtitle"},
		{TrackConfig{Kind: Audio, Codec: "mp4a"}, "audio"},
	}
	for _, c := range cases {
		if got := trackHandler(c.cfg); got != c.want {
			t.Errorf("trackHandler(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

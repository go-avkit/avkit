// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Eyevinn/mp4ff/mp4"
)

// subtitles-tx3g.mp4 was written by ffmpeg 9.0.2 from three SRT cues, and
// subtitles-tx3g.vtt is what the same ffmpeg decodes it to:
//
//	ffmpeg -f lavfi -i color=c=black:s=64x48:r=5:d=6 -i in.srt -map 0:v -map 1:s \
//	  -c:v libx264 -pix_fmt yuv420p -c:s mov_text subtitles-tx3g.mp4
//	ffmpeg -i subtitles-tx3g.mp4 -map 0:s -c:s webvtt subtitles-tx3g.vtt
//
// The cues are "Hello world" at 1-2 s, a two-line cue with bold, italic and
// underlined words at 3-4.5 s, and "Café + 日本語 x" with a bold x at 4.5-5 s.

// tx3gSampleOf builds a text sample: the string, then each box as given.
func tx3gSampleOf(text string, boxes ...[]byte) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(len(text)))
	out = append(out, text...)
	for _, b := range boxes {
		out = append(out, b...)
	}
	return out
}

// stylBox is a styl box holding records of (start, end, flags).
func stylBox(records ...[3]int) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(10+12*len(records)))
	out = append(out, "styl"...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(records)))
	for _, r := range records {
		out = append(out, tx3gStyleRecord(uint16(r[0]), uint16(r[1]), byte(r[2]))...)
	}
	return out
}

// box is any box with a four-character name and a payload.
func plainBox(name string, payload ...byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	return append(append(out, name...), payload...)
}

// vttCue is one cue of the oracle's WebVTT file.
type vttCue struct {
	start, end time.Duration
	text       string
}

// ffmpegCues reads the WebVTT file ffmpeg wrote.
func ffmpegCues(t *testing.T) []vttCue {
	t.Helper()
	data, err := os.ReadFile("testdata/subtitles-tx3g.vtt")
	if err != nil {
		t.Fatal(err)
	}
	var cues []vttCue
	for _, block := range strings.Split(strings.TrimSpace(string(data)), "\n\n")[1:] {
		timing, text, _ := strings.Cut(block, "\n")
		from, to, _ := strings.Cut(timing, " --> ")
		cues = append(cues, vttCue{vttTime(t, from), vttTime(t, to), text})
	}
	return cues
}

// vttTime reads mm:ss.mmm.
func vttTime(t *testing.T, s string) time.Duration {
	t.Helper()
	minutes, rest, _ := strings.Cut(s, ":")
	seconds, millis, _ := strings.Cut(rest, ".")
	m, e1 := strconv.Atoi(minutes)
	sec, e2 := strconv.Atoi(seconds)
	ms, e3 := strconv.Atoi(millis)
	if e1 != nil || e2 != nil || e3 != nil {
		t.Fatalf("bad WebVTT time %q", s)
	}
	return time.Duration(m)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond
}

// cuesOf is the cues a track of wvtt samples shows, with their times.
func cuesOf(t *testing.T, samples []Sample, timescale uint32) []vttCue {
	t.Helper()
	var out []vttCue
	var at uint64
	for i, s := range samples {
		cues, err := readWebVTTSample(s.Data)
		if err != nil {
			t.Fatalf("sample %d: %v", i+1, err)
		}
		for _, c := range cues {
			out = append(out, vttCue{unitsToDuration(at, timescale), unitsToDuration(at+uint64(s.Duration), timescale), c.text})
		}
		at += uint64(s.Duration)
	}
	return out
}

func assertCues(t *testing.T, what string, got, want []vttCue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d cues, want %d:\n got %+v\nwant %+v", what, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s cue %d = %+v, want %+v", what, i+1, got[i], want[i])
		}
	}
}

func TestATx3gTrackOfAnMP4IsReadWithItsEntryAndItsSamples(t *testing.T) {
	r := subtitleFixture(t, "subtitles-tx3g.mp4")
	ids := r.TrackIDs()
	if len(ids) != 2 {
		t.Fatalf("tracks = %v", ids)
	}
	cfg, err := r.TrackConfig(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != Subtitle || cfg.Codec != "tx3g" || cfg.Timescale != 1_000_000 {
		t.Fatalf("config = %+v, want a tx3g subtitle track in microseconds", cfg)
	}
	if err := checkTx3gEntry(cfg.CodecConfig); err != nil {
		t.Fatalf("the sample entry read back is not a tx3g one: %v", err)
	}
	// The entry is what ffmpeg wrote: the 8 reserved and reference bytes, then
	// the fields of TS 26.245 clause 5.16, then the font table box.
	if !bytes.Contains(cfg.CodecConfig, []byte("ftab")) {
		t.Errorf("entry %x holds no font table", cfg.CodecConfig)
	}
	samples, err := r.Samples(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	// ffmpeg wrote six: the five that last, and the zero-length marker that
	// ends every mov_text track, which Samples leaves behind.
	if len(samples) != 5 {
		t.Fatalf("%d samples, want 5", len(samples))
	}
	if n := r.traks[ids[1]].Mdia.Minf.Stbl.Stsz.SampleNumber; n != 6 {
		t.Fatalf("the file holds %d samples, want 6: the fixture no longer ends in a marker", n)
	}
	// ⛔ The oracle: every sample converted to WebVTT must show what ffmpeg's
	// own decoder made of it, at the same times.
	var converted []Sample
	for i, s := range samples {
		data, err := webvttSampleOfTx3g(s.Data)
		if err != nil {
			t.Fatalf("sample %d: %v", i+1, err)
		}
		s.Data = data
		converted = append(converted, s)
	}
	assertCues(t, "against ffmpeg", cuesOf(t, converted, 1_000_000), ffmpegCues(t))
}

func TestATx3gTrackGoesFromMP4ToMP4WithItsEntryAndItsSamplesUnchanged(t *testing.T) {
	r := subtitleFixture(t, "subtitles-tx3g.mp4")
	srcCfg, _ := r.TrackConfig(2)
	want, _ := r.Samples(2)
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
		if cfgs[1].Codec != "tx3g" || cfgs[1].Kind != Subtitle || !bytes.Equal(cfgs[1].CodecConfig, srcCfg.CodecConfig) {
			t.Fatalf("%s config = %+v, want the entry it came with", mux.name, cfgs[1])
		}
		assertSamples(t, mux.name, samples[1], want)
		parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		trak := parsed.Moov.Traks[1]
		nmhd := false
		for _, child := range trak.Mdia.Minf.Children {
			_, isNull := child.(*mp4.NmhdBox)
			nmhd = nmhd || isNull
		}
		if trak.Mdia.Hdlr.HandlerType != "text" || !nmhd {
			t.Errorf("%s handler %q, nmhd %v: want text and a null media header", mux.name, trak.Mdia.Hdlr.HandlerType, nmhd)
		}
	}
	// Remux, which is what most callers use, says the same.
	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		t.Fatal(err)
	}
	cfgs, samples := readBack(t, out.Bytes())
	if !bytes.Equal(cfgs[1].CodecConfig, srcCfg.CodecConfig) {
		t.Errorf("Remux changed the sample entry")
	}
	assertSamples(t, "Remux", samples[1], want)
}

func TestATx3gTrackBecomesWebVTTInMatroskaAndWebM(t *testing.T) {
	r := subtitleFixture(t, "subtitles-tx3g.mp4")
	oracle := ffmpegCues(t)
	for _, mode := range []struct {
		name string
		opts []WebMOption
	}{{"buffered", []WebMOption{BufferedSegment()}}, {"streamed", nil}} {
		var buf bytes.Buffer
		// Only the text track: the fixture's video is H.264, which WebM refuses.
		m := NewWebMMuxer(&buf, mode.opts...)
		id, err := m.AddTrack(trackConfigOf(t, r, 2))
		if err != nil {
			t.Fatal(err)
		}
		samples, _ := r.Samples(2)
		for _, s := range samples {
			if err := m.WriteSample(id, s); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		blocks := mkvCueBlocks(t, buf.Bytes())
		if len(blocks) != len(oracle) {
			t.Fatalf("%s: %d blocks, want %d", mode.name, len(blocks), len(oracle))
		}
		for i, b := range blocks {
			text := strings.TrimPrefix(b.data, "\n\n")
			start := time.Duration(b.time) * time.Millisecond
			end := start + time.Duration(b.duration)*time.Millisecond
			if (vttCue{start, end, text}) != oracle[i] {
				t.Errorf("%s block %d = %q %v-%v, want %+v", mode.name, i+1, text, start, end, oracle[i])
			}
			if b.codecID != "D_WEBVTT/SUBTITLES" || b.docType != "webm" || b.private != nil {
				t.Errorf("%s block %d in %s as %s with private %v", mode.name, i+1, b.docType, b.codecID, b.private)
			}
		}
	}
}

func trackConfigOf(t *testing.T, r *Reader, id uint32) TrackConfig {
	t.Helper()
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSubtitlesAsConvertsBetweenTx3gAndWebVTTKeepingTheTimeline(t *testing.T) {
	// tx3g to wvtt, in an MP4.
	r := subtitleFixture(t, "subtitles-tx3g.mp4")
	original, _ := r.Samples(2)
	var buf bytes.Buffer
	if err := Remux(&buf, r, SubtitlesAs(" WVTT ")); err != nil {
		t.Fatal(err)
	}
	cfgs, samples := readBack(t, buf.Bytes())
	if cfgs[1].Codec != "wvtt" || string(cfgs[1].CodecConfig) != "WEBVTT" || len(samples[1]) != len(original) {
		t.Fatalf("config %+v with %d samples", cfgs[1], len(samples[1]))
	}
	for i := range original {
		if samples[1][i].Duration != original[i].Duration {
			t.Errorf("sample %d lasts %d, was %d", i+1, samples[1][i].Duration, original[i].Duration)
		}
	}
	assertCues(t, "tx3g as wvtt", cuesOf(t, samples[1], cfgs[1].Timescale), ffmpegCues(t))

	// And back: wvtt to tx3g, from the file that holds the cues of threeCues.
	w := subtitleFixture(t, "subtitles-webvtt.mkv")
	buf.Reset()
	if err := Remux(&buf, w, SubtitlesAs("tx3g")); err != nil {
		t.Fatal(err)
	}
	cfgs, samples = readBack(t, buf.Bytes())
	if cfgs[0].Codec != "tx3g" || !bytes.Equal(cfgs[0].CodecConfig, tx3gDefaultEntry) {
		t.Fatalf("config = %+v, want tx3g with the default entry", cfgs[0])
	}
	var texts []string
	var durations []uint32
	for _, s := range samples[0] {
		parsed, err := parseTx3gSample(s.Data)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, parsed.text)
		durations = append(durations, s.Duration)
	}
	// threeCues' timeline, with the bold <b>world</b> of the first cue kept as a
	// style and the identifier and settings of the cues gone.
	wantTexts := []string{"", "Hello, world.", "", "Second cue\non two lines.", "Ünïcödé — third."}
	wantDurations := []uint32{500, 1000, 500, 1250, 750}
	if strings.Join(texts, "|") != strings.Join(wantTexts, "|") {
		t.Errorf("texts = %q, want %q", texts, wantTexts)
	}
	for i := range wantDurations {
		if durations[i] != wantDurations[i] {
			t.Errorf("durations = %v, want %v", durations, wantDurations)
			break
		}
	}
	first, _ := parseTx3gSample(samples[0][1].Data)
	if string(first.faces) != "\x00\x00\x00\x00\x00\x00\x00\x01\x01\x01\x01\x01\x00" {
		t.Errorf("faces of the first cue = %v, want the five letters of world in bold", first.faces)
	}

	// The same samples are what a copy of the converted file holds, and the
	// other way round gives the text back.
	var again bytes.Buffer
	if err := Remux(&again, subtitleFromBytes(t, buf.Bytes()), SubtitlesAs("wvtt")); err != nil {
		t.Fatal(err)
	}
	_, back := readBack(t, again.Bytes())
	assertCues(t, "round trip", cuesOf(t, back[0], 1000), cuesOf(t, samplesOfTrack(t, w, 1), 1000))
}

func subtitleFromBytes(t *testing.T, data []byte) *Reader {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func samplesOfTrack(t *testing.T, r *Reader, id uint32) []Sample {
	t.Helper()
	s, err := r.Samples(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSubtitlesAsRefusesWhatItCannotRewrite(t *testing.T) {
	r := subtitleFixture(t, "subtitles-tx3g.mp4")
	if err := Remux(io.Discard, r, SubtitlesAs("stpp")); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("SubtitlesAs(stpp) = %v", err)
	}
	ttml := subtitleFixture(t, "subtitles-ttml.mp4")
	if err := Remux(io.Discard, ttml, SubtitlesAs("tx3g")); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("TTML as tx3g = %v", err)
	}
	// A track already in the form asked for, and one that is not text, stay.
	if err := Remux(io.Discard, r, SubtitlesAs("tx3g")); err != nil {
		t.Errorf("tx3g as tx3g = %v", err)
	}
	// A sample that is not what its codec says is reported with its place.
	for _, c := range []struct {
		from, to string
		data     []byte
	}{
		{"tx3g", "wvtt", []byte{0}},
		{"wvtt", "tx3g", []byte{0, 0, 0, 9, 'v'}},
		{"wvtt", "tx3g", cueSample(t, vttc("", "", strings.Repeat("x", 70000)))},
	} {
		clip := fixtureReader(t,
			fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 2, 100, 1)},
			fixtureTrack{cfg: TrackConfig{Kind: Subtitle, Codec: c.from, Timescale: 1000}, samples: []Sample{{Data: c.data, Duration: 200, Sync: true}}})
		if err := Remux(io.Discard, clip, SubtitlesAs(c.to)); !errors.Is(err, ErrSample) {
			t.Errorf("%s as %s of %d bytes = %v, want ErrSample", c.from, c.to, len(c.data), err)
		}
	}
}

func TestTheTextOfATx3gSampleBecomesWebVTT(t *testing.T) {
	bold, italic, under := tx3gBold, tx3gItalic, tx3gUnderline
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"plain", tx3gSampleOf("hi"), "hi"},
		{"nothing", tx3gEmptySample, ""},
		{"escapes", tx3gSampleOf("a & b <c> d"), "a &amp; b &lt;c&gt; d"},
		{"line ends", tx3gSampleOf("one\r\ntwo\u2028three\u0085four\u2029five"), "one\ntwo\nthree\nfour\nfive"},
		{"blank lines and the edges", tx3gSampleOf("\n\nx\n\n\ny\n\n"), "x\ny"},
		{"only a line end", tx3gSampleOf("\n"), ""},
		{"a nul", tx3gSampleOf("a\x00b"), "a\uFFFDb"},
		{"bold italic nested", tx3gSampleOf("abcd", stylBox([3]int{1, 3, bold | italic})), "a<b><i>bc</i></b>d"},
		{"all three", tx3gSampleOf("xyz", stylBox([3]int{0, 3, bold | italic | under})), "<b><i><u>xyz</u></i></b>"},
		{"successive runs", tx3gSampleOf("abc", stylBox([3]int{0, 1, bold}, [3]int{1, 2, italic}, [3]int{2, 3, under})),
			"<b>a</b><i>b</i><u>c</u>"},
		{"a run across a line end", tx3gSampleOf("a\nb", stylBox([3]int{0, 3, bold})), "<b>a</b>\n<b>b</b>"},
		{"a run past the end", tx3gSampleOf("ab", stylBox([3]int{1, 99, italic})), "a<i>b</i>"},
		{"a run of no character", tx3gSampleOf("ab", stylBox([3]int{1, 1, bold})), "ab"},
		{"later records paint over", tx3gSampleOf("ab", stylBox([3]int{0, 2, bold}, [3]int{1, 2, italic})), "<b>a</b><i>b</i>"},
		{"colours and fonts are dropped", tx3gSampleOf("ab", stylBox([3]int{0, 2, 0})), "ab"},
		{"characters, not bytes", tx3gSampleOf("é日x", stylBox([3]int{2, 3, bold})), "é日<b>x</b>"},
		{"unknown boxes are skipped", tx3gSampleOf("x", plainBox("hlit", 0, 0, 0, 1), plainBox("hclr", 1, 2, 3, 4), plainBox("twrp", 1)), "x"},
		{"a 64-bit box size", tx3gSampleOf("x", append(binary.BigEndian.AppendUint32(nil, 1), append([]byte("junk"), 0, 0, 0, 0, 0, 0, 0, 17, 9)...)), "x"},
		{"a box to the end", tx3gSampleOf("x", append(binary.BigEndian.AppendUint32(nil, 0), "junkjunk"...)), "x"},
	}
	for _, c := range cases {
		s, err := parseTx3gSample(c.data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := webvttOfTx3g(s); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// UTF-16, with either byte order mark.
	for _, order := range []binary.AppendByteOrder{binary.BigEndian, binary.LittleEndian} {
		bom := []byte{0xFE, 0xFF}
		if order == binary.AppendByteOrder(binary.LittleEndian) {
			bom = []byte{0xFF, 0xFE}
		}
		for _, u := range utf16.Encode([]rune("日本\U0001F600")) {
			bom = order.AppendUint16(bom, u)
		}
		sample := append(binary.BigEndian.AppendUint16(nil, uint16(len(bom))), bom...)
		s, err := parseTx3gSample(sample)
		if err != nil || s.text != "日本\U0001F600" {
			t.Errorf("UTF-16 %v: %q, %v", order, s.text, err)
		}
	}
	// A karaoke box is noticed.
	if s, _ := parseTx3gSample(tx3gSampleOf("x", plainBox("krok", make([]byte, 6)...))); !s.karaoke {
		t.Error("a krok box was not noticed")
	}
}

func TestATx3gSampleThatIsNotOneIsRefused(t *testing.T) {
	cases := map[string][]byte{
		"under two bytes":         {0},
		"text past the sample":    {0, 5, 'a'},
		"invalid UTF-8":           tx3gSampleOf("\xc3\x28"),
		"an odd UTF-16 string":    {0, 3, 0xFE, 0xFF, 0},
		"a short trailer":         tx3gSampleOf("x", []byte{0, 0, 0}),
		"a box too small":         tx3gSampleOf("x", []byte{0, 0, 0, 4, 'a', 'b', 'c', 'd'}),
		"a box past the sample":   tx3gSampleOf("x", []byte{0, 0, 0, 20, 'a', 'b', 'c', 'd'}),
		"a short 64-bit header":   tx3gSampleOf("x", []byte{0, 0, 0, 1, 'a', 'b', 'c', 'd', 0, 0}),
		"a styl without count":    tx3gSampleOf("x", plainBox("styl", 0)),
		"a styl missing records":  tx3gSampleOf("x", plainBox("styl", 0, 2, 0, 0)),
		"a style that ends first": tx3gSampleOf("xy", stylBox([3]int{2, 1, 1})),
	}
	for name, data := range cases {
		if _, err := parseTx3gSample(data); !errors.Is(err, ErrSample) {
			t.Errorf("%s: %v, want ErrSample", name, err)
		}
		// And every muxer that reads it says so before it writes anything.
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf)
		id, _ := m.AddTrack(TrackConfig{Codec: "tx3g", Timescale: 1000})
		if err := m.WriteSample(id, Sample{Data: data, Duration: 10}); !errors.Is(err, ErrSample) {
			t.Errorf("%s: WebMMuxer.WriteSample = %v", name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: %d bytes written for a refused sample", name, buf.Len())
		}
	}
}

func TestTheTextOfACueBecomesATx3gSample(t *testing.T) {
	cases := []struct {
		name  string
		cues  []webvttCue
		text  string
		faces string // one flag byte per character; empty when not checked
	}{
		{"plain", []webvttCue{{text: "hi"}}, "hi", "\x00\x00"},
		{"nothing", nil, "", ""},
		{"faces", []webvttCue{{text: "a<b>b<i>c</i></b><u>d</u>"}}, "abcd", "\x00\x01\x03\x04"},
		{"references", []webvttCue{{text: "&amp;&lt;&gt;&lrm;&rlm;&nbsp;&bogus;&"}}, "&<>\u200e\u200f\u00a0&bogus;&", ""},
		{"other tags are dropped", []webvttCue{{text: "<c.red>a</c><v Bob>b</v><00:01.000>c<lang en>d</lang>"}}, "abcd", "\x00\x00\x00\x00"},
		{"a stray close and a bare <", []webvttCue{{text: "</b>a < b<b"}}, "a < b<b", ""},
		{"close more than open", []webvttCue{{text: "<b>a</b></b>b"}}, "ab", "\x01\x00"},
		{"carriage returns", []webvttCue{{text: "a\r\nb\rc"}}, "a\nb\nc", ""},
		{"two cues one under the other", []webvttCue{{text: "one"}, {text: "<i>two</i>"}}, "one\ntwo", "\x00\x00\x00\x00\x02\x02\x02"},
		{"an empty cue among others", []webvttCue{{text: ""}, {text: "x"}}, "x", "\x00"},
	}
	for _, c := range cases {
		var boxes bytes.Buffer
		if len(c.cues) == 0 {
			boxes.Write(vtteSample)
		}
		for i := range c.cues {
			_ = encodeBox(cueBox(&c.cues[i]), &boxes)
		}
		sample, err := tx3gSampleOfWebVTT(boxes.Bytes())
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got, err := parseTx3gSample(sample)
		if err != nil || got.text != c.text {
			t.Errorf("%s: text %q, %v, want %q", c.name, got.text, err, c.text)
			continue
		}
		if c.name == "nothing" && !bytes.Equal(sample, tx3gEmptySample) {
			t.Errorf("nothing = %x", sample)
		}
		if c.faces != "" {
			// A sample with no style record is read with no faces, which is
			// the same as all plain.
			faces := got.faces
			if faces == nil {
				faces = make([]byte, len(c.faces))
			}
			if string(faces) != c.faces {
				t.Errorf("%s: faces = %v, want %q", c.name, faces, c.faces)
			}
		}
	}
}

func TestTheTx3gSampleEntryIsCheckedWhereverItIsStated(t *testing.T) {
	bad := map[string][]byte{
		"too short":       make([]byte, tx3gMinEntry-1),
		"a stub trailer":  append(bytes.Clone(tx3gDefaultEntry), 0, 0),
		"a box too small": append(bytes.Clone(tx3gDefaultEntry[:tx3gMinEntry]), 0, 0, 0, 4, 'f', 't', 'a', 'b'),
		"a box too big":   append(bytes.Clone(tx3gDefaultEntry[:tx3gMinEntry]), 0, 0, 0, 99, 'f', 't', 'a', 'b'),
	}
	for name, entry := range bad {
		cfg := TrackConfig{Kind: Subtitle, Codec: "tx3g", Timescale: 1000, CodecConfig: entry}
		for _, add := range []struct {
			who string
			add func(TrackConfig) (uint32, error)
		}{
			{"Muxer", NewMuxer(io.Discard).AddTrack},
			{"ProgressiveMuxer", NewProgressiveMuxer(io.Discard).AddTrack},
			{"WebMMuxer", NewWebMMuxer(io.Discard).AddTrack},
		} {
			if _, err := add.add(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("%s with an entry that is %s: %v", add.who, name, err)
			}
		}
	}
	// Left out, the entry is the default one, which is valid.
	if err := checkTx3gEntry(tx3gDefaultEntry); err != nil {
		t.Errorf("the default entry: %v", err)
	}
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	id, err := m.AddTrack(TrackConfig{Kind: Subtitle, Codec: "TX3G", Timescale: 1000})
	if err != nil {
		t.Fatal(err)
	}
	m.WriteSample(id, Sample{Data: tx3gEmptySample, Duration: 10, Sync: true})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	cfgs, _ := readBack(t, buf.Bytes())
	if !bytes.Equal(cfgs[0].CodecConfig, tx3gDefaultEntry) {
		t.Errorf("entry written = %x, want the default %x", cfgs[0].CodecConfig, tx3gDefaultEntry)
	}
}

func TestATx3gTrackOfAnMP4WithNoSampleDescriptionHasNoEntry(t *testing.T) {
	if tx3gEntry(&mp4.StsdBox{}) != nil {
		t.Error("an empty stsd has a tx3g entry")
	}
	stsd := &mp4.StsdBox{}
	stsd.AddChild(mp4.NewWvttBox())
	if tx3gEntry(stsd) != nil {
		t.Error("a wvtt entry is a tx3g one")
	}
	stsd = &mp4.StsdBox{}
	stsd.AddChild(mp4.CreateUnknownBox("tx3g", 8, nil))
	if tx3gEntry(stsd) == nil {
		t.Error("an unknown box named tx3g is not one")
	}
}

func TestATx3gTrackIsCutAndConcatenatedLikeAnyOtherCueTrack(t *testing.T) {
	hello := tx3gSampleOf("hello")
	clip := func(samples ...Sample) *Reader {
		return fixtureReader(t,
			fixtureTrack{cfg: av1Track(1000, 64, 48), samples: marked('v', 10, 100, 5)},
			fixtureTrack{cfg: TrackConfig{Kind: Subtitle, Codec: "tx3g", Timescale: 1000}, samples: samples})
	}
	r := clip(
		Sample{Data: tx3gEmptySample, Duration: 300, Sync: true},
		Sample{Data: hello, Duration: 400, Sync: true},
		Sample{Data: tx3gEmptySample, Duration: 300, Sync: true})
	var buf bytes.Buffer
	if err := Cut(&buf, r, 500*time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	_, samples := readBack(t, buf.Bytes())
	if len(samples[1]) != 2 || samples[1][0].Duration != 200 || !bytes.Equal(samples[1][0].Data, hello) {
		t.Errorf("cut samples = %+v", samples[1])
	}
	// A concatenation ends the first input's text where its pictures end.
	short := clip(Sample{Data: hello, Duration: 300, Sync: true})
	buf.Reset()
	if err := Concat(&buf, []*Reader{short, short}); err != nil {
		t.Fatal(err)
	}
	_, samples = readBack(t, buf.Bytes())
	if len(samples[1]) != 3 || !bytes.Equal(samples[1][1].Data, tx3gEmptySample) || samples[1][1].Duration != 700 {
		t.Errorf("concatenated samples = %+v", samples[1])
	}
	// A karaoke box counts from the start of its sample, which a cut or a join
	// would move, so both are refused; a copy is not.
	karaoke := clip(Sample{Data: tx3gSampleOf("a b", plainBox("krok", make([]byte, 6)...)), Duration: 1000, Sync: true})
	if err := Cut(io.Discard, karaoke, 500*time.Millisecond, 0); !errors.Is(err, ErrTimeRange) {
		t.Errorf("Cut: %v", err)
	}
	if err := Concat(io.Discard, []*Reader{karaoke, karaoke}); !errors.Is(err, ErrTrackMismatch) {
		t.Errorf("Concat: %v", err)
	}
	if err := Remux(io.Discard, karaoke); err != nil {
		t.Errorf("Remux: %v", err)
	}
	broken := clip(Sample{Data: []byte{9}, Duration: 1000, Sync: true})
	if err := Concat(io.Discard, []*Reader{broken, broken}); !errors.Is(err, ErrSample) {
		t.Errorf("Concat of a broken sample: %v", err)
	}
}

func TestTx3gHasNoConfigurationInItsSamples(t *testing.T) {
	_, err := ConfigFromSamples("tx3g", oneSample())
	if !errors.Is(err, ErrNotInSamples) {
		t.Errorf("ConfigFromSamples(tx3g) = %v", err)
	}
}

func TestTheTransportStreamMuxerRefusesTx3gAndPGS(t *testing.T) {
	for codec, reason := range map[string]string{"tx3g": "subtitles", "pgs": "Blu-ray"} {
		_, err := NewTSMuxer(io.Discard).AddTrack(TrackConfig{Kind: Subtitle, Codec: codec, Timescale: 1000})
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), reason) {
			t.Errorf("%s: %v, want a refusal that says %q", codec, err, reason)
		}
	}
}

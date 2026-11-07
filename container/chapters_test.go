// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The chapter fixtures were written by FFmpeg 9.0.2 from testdata/chapters.ffmetadata
// and a 4 s sine tone encoded as AAC (24 kb/s, mono, 48 kHz):
//
//	ffmpeg -fflags +bitexact -f lavfi -i sine=frequency=440:duration=4:sample_rate=48000 \
//	       -i chapters.ffmetadata -map 0:a -map_chapters 1 -c:a aac -b:a 24k -ac 1 \
//	       -flags:a +bitexact ffmpeg-chapters.mp4            (a chpl and a chapter track)
//	... -movflags +disable_chpl ffmpeg-chapters-track.mp4      (the chapter track alone)
//	ffmpeg -i ffmpeg-chapters.mp4 -map 0:a -map_chapters 0 -c copy ffmpeg-chapters.mkv
//	ffmpeg -i ffmpeg-chapters.mp4 -map 0:a -map_chapters 0 -c copy -f mov ffmpeg-chapters.mov
func fourChapters(language string) []Chapter {
	return []Chapter{
		{Start: 0, End: time.Second, Title: "Intro", Language: language},
		{Start: time.Second, End: 1600 * time.Millisecond, Title: "Second é 日本", Language: language},
		{Start: 1600 * time.Millisecond, End: 2900 * time.Millisecond, Title: "Third", Language: language},
		{Start: 2900 * time.Millisecond, End: 4 * time.Second, Title: "Outro", Language: language},
	}
}

var chapterFixtures = []struct {
	file     string
	language string
}{
	{"ffmpeg-chapters.mp4", ""},
	{"ffmpeg-chapters-track.mp4", ""},
	{"ffmpeg-chapters.mov", ""},
	{"ffmpeg-chapters.mkv", "und"},
}

func TestChaptersAreReadFromEveryForm(t *testing.T) {
	for _, fx := range chapterFixtures {
		t.Run(fx.file, func(t *testing.T) {
			data := readFixture(t, fx.file)
			f, err := Demux(data)
			if err != nil {
				t.Fatal(err)
			}
			want := fourChapters(fx.language)
			if !slices.Equal(f.Chapters, want) {
				t.Errorf("Demux read %+v, want %+v", f.Chapters, want)
			}
			r := mustReader(t, data)
			if !slices.Equal(r.File().Chapters, want) {
				t.Errorf("Reader read %+v, want %+v", r.File().Chapters, want)
			}
			// And from disk, where the samples of a chapter track are fetched.
			if strings.HasSuffix(fx.file, ".mkv") {
				return
			}
			disk, err := NewFileReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(disk.File().Chapters, want) {
				t.Errorf("NewFileReader read %+v, want %+v", disk.File().Chapters, want)
			}
		})
	}
}

// chapterMuxer is what the three muxers have in common that states chapters.
type chapterMuxer interface {
	audioMuxer
	SetChapters([]Chapter) error
}

// writeChaptered writes one track and its chapters into a muxer.
func writeChaptered(t *testing.T, kind string, cfg TrackConfig, samples []Sample, chapters []Chapter) []byte {
	t.Helper()
	var buf bytes.Buffer
	var m chapterMuxer
	switch kind {
	case "fragmented":
		m = NewMuxer(&buf)
	case "progressive":
		m = NewProgressiveMuxer(&buf)
	default:
		m = NewWebMMuxer(&buf, BufferedSegment())
	}
	if err := m.SetChapters(chapters); err != nil {
		t.Fatalf("%s SetChapters: %v", kind, err)
	}
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withoutEnds is chapters as an MP4's chpl states them.
func withoutEnds(cs []Chapter, language string) []Chapter {
	out := slices.Clone(cs)
	for i := range out {
		out[i].End, out[i].Language = 0, language
	}
	return out
}

func audioOf(t *testing.T, r *Reader) (TrackConfig, []Sample) {
	t.Helper()
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	return cfg, samples
}

func TestEveryMuxerWritesTheChaptersItIsGiven(t *testing.T) {
	r := mustReader(t, readFixture(t, "ffmpeg-chapters.mp4"))
	cfg, samples := audioOf(t, r)
	for _, kind := range []string{"fragmented", "progressive", "matroska"} {
		t.Run(kind, func(t *testing.T) {
			data := writeChaptered(t, kind, cfg, samples, fourChapters("eng"))
			f, err := Demux(data)
			if err != nil {
				t.Fatal(err)
			}
			want := fourChapters("eng")
			if kind != "matroska" {
				// A chpl states a start and a title.
				want = withoutEnds(want, "")
			}
			if !slices.Equal(f.Chapters, want) {
				t.Errorf("read back %+v, want %+v", f.Chapters, want)
			}
			ext := ".mp4"
			if kind == "matroska" {
				ext = ".mkv"
			}
			checkOracle(t, "chapters "+kind, ext, data, oracleChapters|oracleEnds,
				func(string) string { return "testdata/ffmpeg-chapters.mkv" })
		})
	}
}

func TestRemuxCarriesChaptersAndDropsTheChapterTrack(t *testing.T) {
	for _, fx := range chapterFixtures {
		t.Run(fx.file, func(t *testing.T) {
			r := mustReader(t, readFixture(t, fx.file))
			var out bytes.Buffer
			if err := Remux(&out, r); err != nil {
				t.Fatalf("Remux: %v", err)
			}
			f, err := Demux(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Tracks) != 1 || f.Tracks[0].Codec != "mp4a" {
				t.Errorf("the output holds %+v, want the audio track alone", f.Tracks)
			}
			if want := withoutEnds(fourChapters(""), ""); !slices.Equal(f.Chapters, want) {
				t.Errorf("Remux wrote %+v, want %+v", f.Chapters, want)
			}
			checkOracle(t, "remux "+fx.file, ".mp4", out.Bytes(), oracleChapters|oracleEnds,
				func(string) string { return "testdata/" + fx.file })
		})
	}
}

func TestReplaceChaptersWritesTheCallersOwn(t *testing.T) {
	r := mustReader(t, readFixture(t, "ffmpeg-chapters.mp4"))
	own := []Chapter{{Start: 0, Title: "Only"}}
	for name, run := range map[string]func(*bytes.Buffer, ...RemuxOption) error{
		"Remux":  func(b *bytes.Buffer, o ...RemuxOption) error { return Remux(b, r, o...) },
		"Cut":    func(b *bytes.Buffer, o ...RemuxOption) error { return Cut(b, r, time.Second, 2*time.Second, o...) },
		"Concat": func(b *bytes.Buffer, o ...RemuxOption) error { return Concat(b, []*Reader{r, r}, o...) },
		"Join":   func(b *bytes.Buffer, o ...RemuxOption) error { return Join(b, []*Reader{r}, o...) },
		"JoinProgressive": func(b *bytes.Buffer, o ...RemuxOption) error {
			return JoinProgressive(b, []*Reader{r}, o...)
		},
	} {
		var out bytes.Buffer
		if err := run(&out, ReplaceChapters(own)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, err := Demux(out.Bytes())
		if err != nil || !slices.Equal(f.Chapters, own) {
			t.Errorf("%s: wrote %+v, %v; want %+v", name, f.Chapters, err, own)
		}
		out.Reset()
		if err := run(&out, ReplaceChapters(nil)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f, err := Demux(out.Bytes()); err != nil || len(f.Chapters) != 0 {
			t.Errorf("%s: ReplaceChapters(nil) wrote %+v, %v", name, f.Chapters, err)
		}
	}
	// Join carries the first input's chapters as they are.
	var out bytes.Buffer
	if err := Join(&out, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	if f, _ := Demux(out.Bytes()); !slices.Equal(f.Chapters, withoutEnds(fourChapters(""), "")) {
		t.Errorf("Join wrote %+v", f.Chapters)
	}
	out.Reset()
	if err := JoinProgressive(&out, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	if f, _ := Demux(out.Bytes()); !slices.Equal(f.Chapters, withoutEnds(fourChapters(""), "")) {
		t.Errorf("JoinProgressive wrote %+v", f.Chapters)
	}
	// Chapters the container cannot hold fail the copy, and say so.
	many := make([]Chapter, 256)
	if err := Remux(&out, r, ReplaceChapters(many)); !errors.Is(err, ErrChapters) {
		t.Errorf("256 chapters into a chpl: %v, want ErrChapters", err)
	}
	if err := Join(&out, []*Reader{r}, ReplaceChapters(many)); !errors.Is(err, ErrChapters) {
		t.Errorf("Join of 256 chapters: %v, want ErrChapters", err)
	}
	if err := JoinProgressive(&out, []*Reader{r}, ReplaceChapters(many)); !errors.Is(err, ErrChapters) {
		t.Errorf("JoinProgressive of 256 chapters: %v, want ErrChapters", err)
	}
}

// ffmpegChapterReference writes, with FFmpeg, a file whose chapters are these
// and whose audio is a tone of this length: the reference an oracle comparison
// of a cut or a concatenation is made against.
func ffmpegChapterReference(t *testing.T, dir string, seconds float64, chapters []Chapter) string {
	t.Helper()
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\n")
	for _, c := range chapters {
		end := c.End
		if end == 0 {
			end = time.Duration(seconds * float64(time.Second))
		}
		fmt.Fprintf(&meta, "[CHAPTER]\nTIMEBASE=1/1000000000\nSTART=%d\nEND=%d\ntitle=%s\n", int64(c.Start), int64(end), c.Title)
	}
	metaPath := filepath.Join(dir, "reference.ffmetadata")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "reference.mp4")
	oracleRun(t, oracleFFmpeg, "-v", "error", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf("sine=frequency=440:duration=%v:sample_rate=48000", seconds),
		"-i", metaPath, "-map", "0:a", "-map_chapters", "1", "-c:a", "aac", "-b:a", "24k", "-ac", "1", out)
	return out
}

func TestCutShiftsAndTrimsTheChapters(t *testing.T) {
	// 0.64 s and 2.56 s are 30 and 120 AAC frames at 48 kHz: the cut lands where
	// it is asked to, inside the first chapter and inside the third, and FFmpeg's
	// own cut (-ss and -to as output options, with -c copy) is the reference.
	//
	// The second and third windows are the boundaries. FFmpeg's copy_chapters
	// skips a chapter only when it ends BEFORE the cut and stops at one that begins
	// AFTER the window's end, so the chapter that ends exactly on the cut (1.6 s)
	// stays as one of no length at 0, and the one that begins exactly at the
	// window's end (1.6 s) stays as one of no length at the end. Cut does the same.
	ms := time.Millisecond
	for _, w := range []struct {
		name     string
		from, to time.Duration
		want     []Chapter
	}{
		{"inside", 640 * ms, 2560 * ms, []Chapter{
			{Start: 0, Title: "Intro"},
			{Start: 360 * ms, Title: "Second é 日本"},
			{Start: 960 * ms, Title: "Third"},
		}},
		{"ends on the cut", 1600 * ms, 3200 * ms, []Chapter{
			{Start: 0, Title: "Second é 日本"},
			{Start: 0, Title: "Third"},
			{Start: 1300 * ms, Title: "Outro"},
		}},
		{"begins at the end", 0, 1600 * ms, []Chapter{
			{Start: 0, Title: "Intro"},
			{Start: time.Second, Title: "Second é 日本"},
			{Start: 1600 * ms, Title: "Third"},
		}},
	} {
		for _, src := range []string{"ffmpeg-chapters.mp4", "ffmpeg-chapters.mkv"} {
			t.Run(w.name+" "+src, func(t *testing.T) {
				r := mustReader(t, readFixture(t, src))
				var out bytes.Buffer
				if err := Cut(&out, r, w.from, w.to); err != nil {
					t.Fatalf("Cut: %v", err)
				}
				f, err := Demux(out.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(f.Chapters, w.want) {
					t.Errorf("Cut wrote %+v, want %+v", f.Chapters, w.want)
				}
				key := fmt.Sprintf("cut %s %s", w.name, src)
				what := oracleChapters
				if w.name != "ends on the cut" {
					// The chapter that ends on the cut has no length, and a
					// chpl states no end: FFmpeg reads it ending where the
					// next chapter does, which starts at 0 as well.
					what |= oracleEnds
				}
				checkOracle(t, key, ".mp4", out.Bytes(), what, func(dir string) string {
					ref := filepath.Join(dir, "cut.mp4")
					oracleRun(t, oracleFFmpeg, "-v", "error", "-y", "-i", "testdata/"+src,
						"-ss", fmt.Sprint(w.from.Seconds()), "-to", fmt.Sprint(w.to.Seconds()),
						"-map", "0:a", "-map_chapters", "0", "-c", "copy", ref)
					return ref
				})
			})
		}
	}
}

func TestConcatPutsTheChaptersOfEachInputAfterTheOnesBefore(t *testing.T) {
	r1 := mustReader(t, readFixture(t, "ffmpeg-chapters.mp4"))
	r2 := mustReader(t, readFixture(t, "ffmpeg-chapters-track.mp4"))
	var out bytes.Buffer
	if err := Concat(&out, []*Reader{r1, r2}); err != nil {
		t.Fatalf("Concat: %v", err)
	}
	f, err := Demux(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The second input begins where the first one's audio ends.
	cfg, samples := audioOf(t, r1)
	var ticks uint64
	for _, s := range samples {
		ticks += uint64(s.Duration)
	}
	offset := time.Duration(scaleDuration(ticks, cfg.Timescale, nsScale))
	want := withoutEnds(fourChapters(""), "")
	for _, c := range withoutEnds(fourChapters(""), "") {
		// A chpl counts in units of 100 ns.
		c.Start = (c.Start + offset) / 100 * 100
		want = append(want, c)
	}
	if !slices.Equal(f.Chapters, want) {
		t.Errorf("Concat wrote %+v, want %+v", f.Chapters, want)
	}
	total := offset + offset
	checkOracle(t, "concat", ".mp4", out.Bytes(), oracleChapters|oracleEnds, func(dir string) string {
		return ffmpegChapterReference(t, dir, total.Seconds(), want)
	})
}

func TestValidChaptersAreTheOnlyOnesWritten(t *testing.T) {
	long := strings.Repeat("x", 256)
	tooMany := make([]Chapter, maxChapters+1)
	for name, cs := range map[string][]Chapter{
		"too many":     tooMany,
		"negative":     {{Start: -1}},
		"unsorted":     {{Start: 2 * time.Second}, {Start: time.Second}},
		"end first":    {{Start: 2 * time.Second, End: time.Second}},
		"negative end": {{Start: 0, End: -time.Second}},
		"not text":     {{Title: "a\x00b"}},
		"not utf-8":    {{Title: "\xff"}},
		"language":     {{Language: "english"}},
		"language2":    {{Language: "EN"}},
		"language3":    {{Language: "e1g"}},
	} {
		if err := validateChapters(cs); !errors.Is(err, ErrChapters) {
			t.Errorf("%s: validateChapters = %v, want ErrChapters", name, err)
		}
		if err := NewWebMMuxer(&bytes.Buffer{}).SetChapters(cs); !errors.Is(err, ErrChapters) {
			t.Errorf("%s: SetChapters = %v, want ErrChapters", name, err)
		}
	}
	if err := validateChapters([]Chapter{{Start: 0, End: 0}, {Start: 0, End: 0, Title: "é", Language: "fre"}, {Start: time.Second, End: time.Second}}); err != nil {
		t.Errorf("a valid list was refused: %v", err)
	}
	// An MP4's chpl is the tighter container.
	for name, cs := range map[string][]Chapter{
		"256 chapters": make([]Chapter, 256),
		"long title":   {{Title: long}},
	} {
		if err := checkChpl(cs); !errors.Is(err, ErrChapters) {
			t.Errorf("%s: checkChpl = %v, want ErrChapters", name, err)
		}
		if err := NewMuxer(&bytes.Buffer{}).SetChapters(cs); !errors.Is(err, ErrChapters) {
			t.Errorf("%s: Muxer.SetChapters = %v", name, err)
		}
		if err := NewProgressiveMuxer(&bytes.Buffer{}).SetChapters(cs); !errors.Is(err, ErrChapters) {
			t.Errorf("%s: ProgressiveMuxer.SetChapters = %v", name, err)
		}
		// Matroska states as many and as long as it is given.
		if err := NewWebMMuxer(&bytes.Buffer{}).SetChapters(cs); err != nil {
			t.Errorf("%s: WebMMuxer.SetChapters = %v", name, err)
		}
	}
	if err := checkChpl(make([]Chapter, 255)); err != nil {
		t.Errorf("255 chapters: %v", err)
	}
}

func TestChaptersCannotBeSetOnceTheFileIsWritten(t *testing.T) {
	cfg := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000, Channels: 1, SampleRate: 48000}
	sample := Sample{Data: []byte{1, 2, 3}, Duration: 1024, Sync: true}
	for _, kind := range []string{"fragmented", "progressive", "matroska"} {
		var buf bytes.Buffer
		var m chapterMuxer
		switch kind {
		case "fragmented":
			m = NewMuxer(&buf)
		case "progressive":
			m = NewProgressiveMuxer(&buf)
		default:
			m = NewWebMMuxer(&buf)
		}
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, sample); err != nil {
			t.Fatal(err)
		}
		late := m.SetChapters([]Chapter{{Title: "x"}})
		if kind == "progressive" {
			// The movie box of a progressive file is written last.
			if late != nil {
				t.Errorf("%s: SetChapters after a sample: %v", kind, late)
			}
		} else if !errors.Is(late, ErrChapters) {
			t.Errorf("%s: SetChapters after a sample = %v, want ErrChapters", kind, late)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.SetChapters(nil); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: SetChapters after Close = %v, want ErrClosed", kind, err)
		}
	}
}

func TestAChplIsReadWhateverItHolds(t *testing.T) {
	moov := &mp4.MoovBox{}
	if got := mp4Chapters(moov); got != nil {
		t.Errorf("a movie with no udta has chapters %+v", got)
	}
	moov.AddChild(&mp4.UdtaBox{})
	if got := mp4Chapters(moov); got != nil {
		t.Errorf("a udta with no chpl has chapters %+v", got)
	}
	other := &mp4.UdtaBox{}
	other.AddChild(mp4.CreateUnknownBox("free", 8, nil))
	other.AddChild(&mp4.ChplBox{Version: 1, Chapters: []mp4.NeroChapter{
		{Start: 20_000_000, Title: "b"}, {Start: 0, Title: "a"}, {Start: 5, Title: "bad\x00"},
		{Start: 1 << 62, Title: "far"},
	}})
	moov = &mp4.MoovBox{}
	moov.AddChild(mp4.CreateUnknownBox("free", 8, nil))
	moov.AddChild(other)
	// In start order, 100 ns to the unit, and a title that is not text empty.
	want := []Chapter{{Start: 0, Title: "a"}, {Start: 500, Title: ""}, {Start: 2 * time.Second, Title: "b"},
		{Start: time.Duration(1<<63 - 1), Title: "far"}}
	if got := mp4Chapters(moov); !slices.Equal(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
	}
	many := &mp4.ChplBox{Chapters: make([]mp4.NeroChapter, maxChapters+10)}
	if got := chaptersOfChpl(many); len(got) != maxChapters {
		t.Errorf("a chpl of %d chapters gave %d", len(many.Chapters), len(got))
	}
	// And what is written is what Nero and FFmpeg write: version 1, 100 ns units.
	box := chplOf([]Chapter{{Start: 1500 * time.Millisecond, Title: "x"}})
	if box.Version != 1 || box.Chapters[0].Start != 15_000_000 || box.Chapters[0].Title != "x" {
		t.Errorf("chplOf = %+v", box)
	}
	if mp4Udta(nil, nil) != nil {
		t.Errorf("nothing to state produced a udta")
	}
}

func TestAChapterSampleIsReadAsTheTextItHolds(t *testing.T) {
	be := utf16.Encode([]rune("日本"))
	utf16BE := []byte{0xFE, 0xFF}
	utf16LE := []byte{0xFF, 0xFE}
	for _, u := range be {
		utf16BE = append(utf16BE, byte(u>>8), byte(u))
		utf16LE = append(utf16LE, byte(u), byte(u>>8))
	}
	sample := func(text []byte, tail ...byte) []byte {
		return append(append([]byte{byte(len(text) >> 8), byte(len(text))}, text...), tail...)
	}
	encd := []byte{0, 0, 0, 12, 'e', 'n', 'c', 'd', 0, 0, 1, 0}
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"utf-8":            {sample([]byte("Intro"), encd...), "Intro"},
		"empty":            {sample(nil), ""},
		"no length":        {[]byte{1}, ""},
		"length too long":  {[]byte{0, 9, 'a'}, ""},
		"utf-16 big":       {sample(utf16BE), "日本"},
		"utf-16 little":    {sample(utf16LE), "日本"},
		"utf-16 with NUL":  {sample([]byte{0xFE, 0xFF, 0, 0}), ""},
		"odd after marker": {sample([]byte{0xFE, 0xFF, 'a'}), ""},
		"NUL":              {sample([]byte("a\x00b")), ""},
	} {
		if got := chapterText(tc.data); got != tc.want {
			t.Errorf("%s: chapterText = %q, want %q", name, got, tc.want)
		}
	}
}

// chapterReader is a Reader over the fixture whose chapter track can be pointed
// somewhere else: what the references of a damaged file say.
func chapterReader(t *testing.T, point func(ids *[]uint32, r *Reader)) *Reader {
	t.Helper()
	r := mustReader(t, readFixture(t, "ffmpeg-chapters-track.mp4"))
	r.file.Chapters, r.chaptersFromTrack, r.chapterTracks = nil, false, nil
	for _, trak := range movieBox(r.mp4).Traks {
		for _, child := range trak.Children {
			if tref, ok := child.(*mp4.TrefBox); ok {
				for _, ref := range tref.Children {
					if typed, ok := ref.(*mp4.TrefTypeBox); ok {
						point(&typed.TrackIDs, r)
					}
				}
			}
		}
	}
	r.loadChapters()
	return r
}

func TestAChapterTrackThatIsNotOneHoldsNoChapters(t *testing.T) {
	// The reference as written.
	r := chapterReader(t, func(*[]uint32, *Reader) {})
	if len(r.file.Chapters) != 4 || !r.isChapterTrack(2) || r.isChapterTrack(1) {
		t.Fatalf("chapters %+v, chapter tracks %v", r.file.Chapters, r.chapterTracks)
	}
	// A track that is not there, one that is not text, and two that are.
	r = chapterReader(t, func(ids *[]uint32, _ *Reader) { *ids = []uint32{99} })
	if len(r.file.Chapters) != 0 || !r.isChapterTrack(99) {
		t.Errorf("a missing track gave %+v", r.file.Chapters)
	}
	r = chapterReader(t, func(ids *[]uint32, _ *Reader) { *ids = []uint32{1} })
	if len(r.file.Chapters) != 0 {
		t.Errorf("an audio track gave chapters %+v", r.file.Chapters)
	}
	r = chapterReader(t, func(ids *[]uint32, _ *Reader) { *ids = []uint32{1, 2, 2} })
	if len(r.file.Chapters) != 4 {
		t.Errorf("the first chapter track of several gave %+v", r.file.Chapters)
	}
	// A text track with no timescale, and one with no samples.
	r = chapterReader(t, func(_ *[]uint32, r *Reader) { r.traks[2].Mdia.Mdhd.Timescale = 0 })
	if len(r.file.Chapters) != 0 {
		t.Errorf("a track with no timescale gave %+v", r.file.Chapters)
	}
	r = chapterReader(t, func(_ *[]uint32, r *Reader) { delete(r.traks, 2) })
	if len(r.file.Chapters) != 0 {
		t.Errorf("an unreadable track gave %+v", r.file.Chapters)
	}
	r = chapterReader(t, func(_ *[]uint32, r *Reader) { r.traks[2].Mdia.Minf.Stbl.Stsz = &mp4.StszBox{} })
	if len(r.file.Chapters) != 0 {
		t.Errorf("a track with no samples gave %+v", r.file.Chapters)
	}
	// Chapters in the file's chpl stay when the track gives none.
	r = chapterReader(t, func(ids *[]uint32, _ *Reader) { *ids = []uint32{99} })
	r.file.Chapters = []Chapter{{Title: "chpl"}}
	r.loadChapters()
	if len(r.file.Chapters) != 1 {
		t.Errorf("an empty chapter track replaced the chpl: %+v", r.file.Chapters)
	}
	// A reference of another type, and a box in a tref that is not one.
	r = mustReader(t, readFixture(t, "ffmpeg-chapters-track.mp4"))
	r.file.Chapters, r.chaptersFromTrack, r.chapterTracks = nil, false, nil
	tref := &mp4.TrefBox{}
	tref.AddChild(&mp4.TrefTypeBox{Name: "tmcd", TrackIDs: []uint32{2}})
	tref.AddChild(mp4.CreateUnknownBox("chap", 9, []byte{1}))
	movieBox(r.mp4).Traks[0].Children = []mp4.Box{tref, mp4.CreateUnknownBox("free", 8, nil)}
	r.loadChapters()
	if len(r.file.Chapters) != 0 || r.isChapterTrack(2) {
		t.Errorf("a reference that is not a chap one gave %+v", r.file.Chapters)
	}
}

func TestMatroskaChaptersAreReadFromTheDefaultEdition(t *testing.T) {
	atom := func(uid, start, end uint64, title, lang string) mkvChapAtom {
		a := mkvChapAtom{ChapterUID: uid, ChapterTimeStart: start, ChapterTimeEnd: end}
		if title != "" || lang != "" {
			a.ChapterDisplay = []mkvChapterDisplay{{ChapString: title, ChapLanguage: lang}}
		}
		return a
	}
	if mkvReadChapters(nil) != nil || mkvReadChapters([]mkvChapters{{}}) != nil {
		t.Errorf("no edition gave chapters")
	}
	first := mkvEdition{ChapterAtom: []mkvChapAtom{atom(1, 0, 10, "first", "fre")}}
	second := mkvEdition{EditionFlagDefault: 1, ChapterAtom: []mkvChapAtom{
		atom(2, 2_000_000_000, 0, "late", ""),
		atom(3, 1_000_000_000, 500_000_000, "end before start", "ger"),
		atom(4, 0, 0, "", ""),
		atom(5, 3_000_000_000, 3_000_000_000, "a\x00b", "eng"),
		atom(6, 1<<63+5, 0, "huge", "eng"),
	}}
	got := mkvReadChapters([]mkvChapters{{EditionEntry: []mkvEdition{first}}, {EditionEntry: []mkvEdition{second}}})
	want := []Chapter{
		{Start: 0},
		{Start: time.Second, Title: "end before start", Language: "ger"},
		{Start: 2 * time.Second, Title: "late", Language: "eng"},
		{Start: 3 * time.Second, End: 3 * time.Second, Language: "eng"},
		{Start: time.Duration(1<<63 - 1), Title: "huge", Language: "eng"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
	}
	// With no default edition, the first one.
	got = mkvReadChapters([]mkvChapters{{EditionEntry: []mkvEdition{first, {ChapterAtom: second.ChapterAtom}}}})
	if len(got) != 1 || got[0].Title != "first" || got[0].End != 10 {
		t.Errorf("the first edition gave %+v", got)
	}
	// An ordered edition is a timeline of its own, not points on the file's.
	ordered := first
	ordered.EditionFlagOrdered = 1
	if got := mkvReadChapters([]mkvChapters{{EditionEntry: []mkvEdition{ordered}}}); got != nil {
		t.Errorf("an ordered edition gave %+v", got)
	}
	// And a file cannot make a reader keep more than it is willing to hold.
	huge := mkvEdition{ChapterAtom: make([]mkvChapAtom, maxChapters+5)}
	if got := mkvReadChapters([]mkvChapters{{EditionEntry: []mkvEdition{huge}}}); len(got) != maxChapters {
		t.Errorf("%d atoms gave %d chapters", len(huge.ChapterAtom), len(got))
	}
	// What is written is one edition, one atom each, the language "und" when none.
	written := mkvChaptersOf([]Chapter{{Start: time.Second, End: 2 * time.Second, Title: "a"}, {Title: "b", Language: "fre"}})
	atoms := written[0].EditionEntry[0].ChapterAtom
	if len(written) != 1 || len(atoms) != 2 || atoms[0].ChapterDisplay[0].ChapLanguage != "und" ||
		atoms[1].ChapterDisplay[0].ChapLanguage != "fre" || atoms[0].ChapterTimeEnd != 2_000_000_000 ||
		atoms[1].ChapterUID != 2 {
		t.Errorf("mkvChaptersOf = %+v", written)
	}
	if mkvChaptersOf(nil) != nil {
		t.Errorf("no chapters wrote an element")
	}
}

func TestACutKeepsWhatItOverlaps(t *testing.T) {
	s := time.Second
	cs := []Chapter{
		{Start: 0, End: 2 * s, Title: "a"},
		{Start: 2 * s, Title: "b"}, // runs until c
		{Start: 5 * s, End: 8 * s, Title: "c"},
		{Start: 8 * s, End: 8 * s, Title: "point"},
		{Start: 9 * s, Title: "last"}, // runs for ever
	}
	for name, tc := range map[string]struct {
		from, to time.Duration
		want     []Chapter
	}{
		"whole":       {0, 0, cs},
		"inside b":    {3 * s, 4 * s, []Chapter{{Start: 0, Title: "b"}}},
		"across":      {1 * s, 6 * s, []Chapter{{Start: 0, End: 1 * s, Title: "a"}, {Start: 1 * s, Title: "b"}, {Start: 4 * s, End: 5 * s, Title: "c"}}},
		"clipped end": {5 * s, 7 * s, []Chapter{{Start: 0, Title: "b"}, {Start: 0, End: 2 * s, Title: "c"}}},
		"a point":     {8 * s, 8*s + 1, []Chapter{{Start: 0, Title: "c"}, {Start: 0, Title: "point"}}},
		"to the end":  {9 * s, 0, []Chapter{{Start: 0, Title: "last"}}},
		"after a":     {2 * s, 3 * s, []Chapter{{Start: 0, End: 0, Title: "a"}, {Start: 0, Title: "b"}}},
		"to c":        {0, 5 * s, []Chapter{{Start: 0, End: 2 * s, Title: "a"}, {Start: 2 * s, Title: "b"}, {Start: 5 * s, End: 5 * s, Title: "c"}}},
		"for ever":    {20 * s, 30 * s, []Chapter{{Start: 0, Title: "last"}}},
	} {
		got := cutChapters(cs, tc.from, tc.to)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: cutChapters(%v, %v) = %+v, want %+v", name, tc.from, tc.to, got, tc.want)
		}
	}
	if got := cutChapters(cs[:4], 20*s, 30*s); got != nil {
		t.Errorf("a window past every chapter kept %+v", got)
	}
	if got := shiftChapters([]Chapter{{Start: s, End: 2 * s}, {Start: 3 * s}}, 10*s); got[0].Start != 11*s ||
		got[0].End != 12*s || got[1].Start != 13*s || got[1].End != 0 {
		t.Errorf("shiftChapters = %+v", got)
	}
}

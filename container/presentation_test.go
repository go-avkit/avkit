// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// testdata/bframes.mkv and testdata/bframes.mp4 are ffmpeg's: ten frames of
// H.264 at 25 fps with two B-frames (libx264 -bf 2) and 0.4 s of AAC, written to
// Matroska and then copied into an MP4 with -c copy. ffprobe gives both tracks
// of both files a start_time of 0.000.
//
// Before the fix, avkit's copies of the Matroska file presented the first
// picture 40 ms late in the fragmented MP4, the progressive MP4 and the
// Matroska it wrote, and its progressive copy of the MP4 80 ms late: the
// pictures' reordering delay, by which the sound ran ahead of them.

// sampleCloser is what every muxer offers, and all these tests need of one.
type sampleCloser interface {
	sampleWriter
	Close() error
}

// copyInto writes every track of src into m, interleaved by decode time, the
// way Remux writes a fragmented MP4. It is how the tests reach the muxers Remux
// does not use.
func copyInto(t *testing.T, m sampleCloser, src *Reader) {
	t.Helper()
	var tracks []sourceTrack
	var outIDs []uint32
	for _, id := range src.TrackIDs() {
		cfg, err := src.TrackConfig(id)
		if err != nil {
			t.Fatal(err)
		}
		samples, err := src.Samples(id)
		if err != nil {
			t.Fatal(err)
		}
		out, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, sourceTrack{id: id, cfg: cfg, samples: samples})
		outIDs = append(outIDs, out)
	}
	next := make([]int, len(tracks))
	at := make([]uint64, len(tracks))
	for {
		pick := -1
		for i := range tracks {
			if next[i] < len(tracks[i].samples) && (pick < 0 ||
				compareTimes(at[i], tracks[i].cfg.Timescale, at[pick], tracks[pick].cfg.Timescale) < 0) {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		s := tracks[pick].samples[next[pick]]
		if err := m.WriteSample(outIDs[pick], s); err != nil {
			t.Fatal(err)
		}
		at[pick] += uint64(s.Duration)
		next[pick]++
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

// openFixture opens one of the B-frame files.
func openFixture(t *testing.T, name string) (*Reader, []byte) {
	t.Helper()
	data := readFixture(t, name)
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	return r, data
}

// edits is the edit list of every track of an MP4, in track order; a track
// without one has none.
func edits(t *testing.T, data []byte) [][]mp4.ElstEntry {
	t.Helper()
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	moov := movieBox(parsed)
	out := make([][]mp4.ElstEntry, len(moov.Traks))
	for i, trak := range moov.Traks {
		if trak.Edts != nil {
			out[i] = trak.Edts.Elst[0].Entries
		}
	}
	return out
}

// firstPresented is when a track of an MP4 shows its first frame, in its own
// timescale: the first sample's composition time, less the media time its edit
// list starts at. It is what ffprobe reports as the stream's start_time.
func firstPresented(t *testing.T, data []byte, track int) int64 {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(r.TrackIDs()[track])
	if err != nil {
		t.Fatal(err)
	}
	var media int64
	if e := edits(t, data)[track]; e != nil {
		media = e[0].MediaTime
	}
	return int64(samples[0].CompositionOffset) - media
}

// The defect: a B-frame track copied by avkit is shown from zero, as the
// source shows it, in both MP4 forms.
func TestABFrameTrackIsPresentedFromZero(t *testing.T) {
	for _, source := range []string{"bframes.mkv", "bframes.mp4"} {
		for _, form := range []struct {
			name  string
			write func(io.Writer, *Reader) error
		}{
			{"fragmented", func(w io.Writer, r *Reader) error { return Remux(w, r) }},
			{"progressive", func(w io.Writer, r *Reader) error { return JoinProgressive(w, []*Reader{r}) }},
		} {
			t.Run(source+" to "+form.name, func(t *testing.T) {
				r, _ := openFixture(t, source)
				var out bytes.Buffer
				if err := form.write(&out, r); err != nil {
					t.Fatal(err)
				}
				if got := firstPresented(t, out.Bytes(), 0); got != 0 {
					t.Errorf("the first picture is shown at %d ticks; want 0", got)
				}
				// The first sample is the one reordered: the fixture would not
				// test anything if it were not.
				back, err := NewReader(out.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				samples, err := back.Samples(back.TrackIDs()[0])
				if err != nil {
					t.Fatal(err)
				}
				if samples[0].CompositionOffset <= 0 {
					t.Fatalf("the fixture's first picture is not reordered (offset %d)",
						samples[0].CompositionOffset)
				}
				// And the composition offset is not read back as a priming,
				// which the muxer would stack on top of it on the next copy.
				cfg, err := back.TrackConfig(back.TrackIDs()[0])
				if err != nil {
					t.Fatal(err)
				}
				if cfg.StartDelay != 0 {
					t.Errorf("the video track reads back a start delay of %v; want none", cfg.StartDelay)
				}
			})
		}
	}
}

// ffmpeg's own edit lists are the reference: avkit's progressive copy of
// ffmpeg's MP4 states exactly what ffmpeg stated, entry for entry, for the
// pictures' reordering and for the sound's priming alike.
func TestAProgressiveCopyRestatesFfmpegsEditLists(t *testing.T) {
	r, source := openFixture(t, "bframes.mp4")
	var out bytes.Buffer
	if err := JoinProgressive(&out, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	want, got := edits(t, source), edits(t, out.Bytes())
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("track %d: edit list %+v; ffmpeg wrote %+v", i+1, got[i], want[i])
		}
	}
	// The fragmented copy cannot state a length its header precedes, and
	// states the same start.
	var frag bytes.Buffer
	if err := Remux(&frag, r); err != nil {
		t.Fatal(err)
	}
	for i, entries := range edits(t, frag.Bytes()) {
		if len(entries) != 1 || entries[0].MediaTime != want[i][0].MediaTime || entries[0].SegmentDuration != 0 {
			t.Errorf("track %d: fragmented edit list %+v; want media time %d to the end",
				i+1, entries, want[i][0].MediaTime)
		}
	}
}

// Matroska states presentation times, so there is no edit list to write: the
// blocks themselves must be shown when the source showed them.
func TestAMatroskaCopyShowsTheFramesWhenTheSourceDid(t *testing.T) {
	for _, source := range []string{"bframes.mkv", "bframes.mp4"} {
		t.Run(source, func(t *testing.T) {
			r, _ := openFixture(t, source)
			var out bytes.Buffer
			copyInto(t, NewWebMMuxer(&out), r)
			got := blockTicks(readWebM(t, out.Bytes()), 1)
			// The Matroska source's own blocks are the reference, whichever
			// file was copied: both hold the same pictures at the same times.
			_, mkv := openFixture(t, "bframes.mkv")
			want := blockTicks(readWebM(t, mkv), 1)
			if !slices.Equal(got, want) {
				t.Errorf("pictures shown at %v; the source shows them at %v", got, want)
			}
		})
	}
}

// blockTicks are the times a track's blocks are shown at, in order.
func blockTicks(f webmFile, track uint64) []int64 {
	var out []int64
	for _, b := range webmBlocks(f) {
		if b.track == track {
			out = append(out, b.tick)
		}
	}
	slices.Sort(out)
	return out
}

// A frame shown before the first one is refused rather than written before the
// start of the track: the first frame is the one shown at zero.
func TestAWebMFrameShownBeforeTheFirstIsRefused(t *testing.T) {
	m := NewWebMMuxer(io.Discard)
	id, err := m.AddTrack(webmVideoConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 3000, CompositionOffset: 6000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{2}, Duration: 3000}); err == nil {
		t.Error("a frame shown 3000 ticks before the first one was written")
	}
}

// The pictures' reordering and the sound's priming are two different things,
// and a track that has both skips both.
func TestACompositionOffsetAndAPrimingAddUp(t *testing.T) {
	cfg := videoConfig(t)
	cfg.StartDelay = 10 * time.Millisecond // 128 ticks at 12.8 kHz
	first := Sample{Data: []byte{1}, Duration: 3000, CompositionOffset: 6000, Sync: true}
	for _, form := range []struct {
		name string
		m    func(io.Writer) sampleCloser
		want mp4.ElstEntry
	}{
		{"fragmented", func(w io.Writer) sampleCloser { return NewMuxer(w) },
			mp4.ElstEntry{SegmentDuration: 0, MediaTime: 6128, MediaRateInteger: 1}},
		// Presented from 6128 to 9000, the end of the only frame: 2872 ticks,
		// which is 224.4 ms and rounds UP to 225 so the frame is not cut short.
		{"progressive", func(w io.Writer) sampleCloser { return NewProgressiveMuxer(w) },
			mp4.ElstEntry{SegmentDuration: 225, MediaTime: 6128, MediaRateInteger: 1}},
	} {
		t.Run(form.name, func(t *testing.T) {
			var out bytes.Buffer
			m := form.m(&out)
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.WriteSample(id, first); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			got := edits(t, out.Bytes())[0]
			if len(got) != 1 || got[0] != form.want {
				t.Errorf("edit list %+v; want %+v", got, form.want)
			}
		})
	}
}

// A fragmented header goes out with the first fragment, and can only state the
// tracks whose first sample has arrived by then; a track starting later is
// given its priming alone, which is what it had before the header waited.
func TestATrackStartingAfterTheFirstFragmentKeepsItsPriming(t *testing.T) {
	var out bytes.Buffer
	m := NewMuxer(&out, FragmentDuration(time.Millisecond))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	acfg := audioConfig()
	acfg.StartDelay = time.Second / 2
	audio, err := m.AddTrack(acfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Sample{
		{Data: []byte{1}, Duration: 3000, CompositionOffset: 3000, Sync: true},
		{Data: []byte{2}, Duration: 3000, Sync: true}, // closes the first fragment
	} {
		if err := m.WriteSample(video, s); err != nil {
			t.Fatal(err)
		}
	}
	// The audio track's first sample arrives with the header already out.
	if err := m.WriteSample(audio, Sample{Data: []byte{3}, Duration: 1024, CompositionOffset: 512, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	e := edits(t, out.Bytes())
	if len(e[0]) != 1 || e[0][0].MediaTime != 3000 {
		t.Errorf("video edit list %+v; want one starting at 3000", e[0])
	}
	if want := int64(acfg.SampleRate / 2); len(e[1]) != 1 || e[1][0].MediaTime != want {
		t.Errorf("audio edit list %+v; want its priming alone, %d", e[1], want)
	}
}

// What needs no edit list gets none: a first frame presented at zero, or one a
// version 1 ctts presents before it decodes, which an edit cannot reach before.
func TestATrackPresentedFromZeroGetsNoEditList(t *testing.T) {
	for _, offset := range []int32{0, -3000} {
		for _, m := range []func(io.Writer) sampleCloser{
			func(w io.Writer) sampleCloser { return NewMuxer(w) },
			func(w io.Writer) sampleCloser { return NewProgressiveMuxer(w) },
		} {
			var out bytes.Buffer
			mux := m(&out)
			id, err := mux.AddTrack(videoConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := mux.WriteSample(id, Sample{Data: []byte{1}, Duration: 3000, CompositionOffset: offset, Sync: true}); err != nil {
				t.Fatal(err)
			}
			if err := mux.Close(); err != nil {
				t.Fatal(err)
			}
			if e := edits(t, out.Bytes())[0]; e != nil {
				t.Errorf("offset %d: edit list %+v; want none", offset, e)
			}
		}
	}
}

// A priming as long as everything the track holds leaves nothing to present,
// and a progressive file has no edit that says so: none is written, and the
// track keeps the duration of what it holds.
func TestAPrimingCoveringTheWholeTrackWritesNoEdit(t *testing.T) {
	var out bytes.Buffer
	m := NewProgressiveMuxer(&out)
	cfg := audioConfig()
	cfg.StartDelay = time.Second
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 1024, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if e := edits(t, out.Bytes())[0]; e != nil {
		t.Errorf("edit list %+v; want none", e)
	}
}

// The edit box is placed where ISO/IEC 14496-12 lists it, between the track
// header and the media box.
func TestTheEditBoxSitsBeforeTheMediaBox(t *testing.T) {
	r, _ := openFixture(t, "bframes.mkv")
	var out bytes.Buffer
	if err := JoinProgressive(&out, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, b := range parsed.Moov.Traks[0].Children {
		order = append(order, b.Type())
	}
	if !slices.Equal(order, []string{"tkhd", "edts", "mdia"}) {
		t.Errorf("track box holds %v", order)
	}
}

func TestScaleDurationUp(t *testing.T) {
	for _, c := range []struct {
		d        uint64
		from, to uint32
		want     uint64
	}{
		{2100, 90000, 1000, 24}, // 23.33 rounds up
		{6400, 16000, 1000, 400},
		{0, 48000, 1000, 0},
	} {
		if got := scaleDurationUp(c.d, c.from, c.to); got != c.want {
			t.Errorf("scaleDurationUp(%d, %d, %d) = %d; want %d", c.d, c.from, c.to, got, c.want)
		}
	}
}

// An edit list whose values outgrow 32 bits is written in the 64-bit form,
// and one that fits in the 32-bit form ffmpeg writes.
func TestNewEdtsPicksTheNarrowestVersion(t *testing.T) {
	if v := newEdts(mp4.ElstEntry{MediaTime: 1280, MediaRateInteger: 1}).Elst[0].Version; v != 0 {
		t.Errorf("a small edit is written as version %d", v)
	}
	for _, e := range []mp4.ElstEntry{
		{SegmentDuration: 1 << 32, MediaTime: 0, MediaRateInteger: 1},
		{MediaTime: 1 << 31, MediaRateInteger: 1},
		{MediaTime: -1 << 32, MediaRateInteger: 1},
	} {
		if v := newEdts(e).Elst[0].Version; v != 1 {
			t.Errorf("%+v is written as version %d", e, v)
		}
	}
}

// A fragmented file read back states the first composition offset its sample
// runs give, and a track with no sample in any fragment states none.
func TestTheFirstCompositionOffsetOfAFragmentedFile(t *testing.T) {
	var out bytes.Buffer
	m := NewMuxer(&out)
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(audioConfig()); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{Data: []byte{1}, Duration: 3000, CompositionOffset: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.firstCompositionOffset(r.traks[1]); got != 3000 {
		t.Errorf("video: %d; want 3000", got)
	}
	if got := r.firstCompositionOffset(r.traks[2]); got != 0 {
		t.Errorf("a track with no sample: %d; want 0", got)
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// testdata/bframes-delayed.mp4 is bframes.mkv copied by ffmpeg with the sound
// put 0.1 s late (-itsoffset 0.1): ffmpeg states it as an empty edit of 79 ms
// followed by the whole of the media. testdata/bframes-twoedits.mp4 is
// bframes.mp4 with the picture's edit list rewritten into two edits of 100 ms,
// the first and the third tenth of a second of the media, one after the other;
// ffprobe gives the picture a duration of 0.200 s and decodes five frames.

// asEditList is an mp4ff edit list as the reader hands it over.
func asEditList(timescale uint32, entries []mp4.ElstEntry) *EditList {
	el := &EditList{MovieTimescale: timescale}
	for _, e := range entries {
		el.Edits = append(el.Edits, Edit{SegmentDuration: e.SegmentDuration, MediaTime: e.MediaTime, Rate: e.MediaRateFixed32()})
	}
	return el
}

// The reader hands over every edit, as the file states it.
func TestTheReaderExposesTheWholeEditList(t *testing.T) {
	for _, name := range []string{"bframes-delayed.mp4", "bframes-twoedits.mp4", "bframes.mp4"} {
		t.Run(name, func(t *testing.T) {
			r, data := openFixture(t, name)
			for i, entries := range edits(t, data) {
				cfg, err := r.TrackConfig(r.TrackIDs()[i])
				if err != nil {
					t.Fatal(err)
				}
				want := asEditList(1000, entries)
				if cfg.EditList == nil || cfg.EditList.MovieTimescale != want.MovieTimescale ||
					!slices.Equal(cfg.EditList.Edits, want.Edits) {
					t.Errorf("track %d: %+v; the file states %+v", i+1, cfg.EditList, want)
				}
			}
		})
	}
	// A file stating none hands over none.
	r, _ := openFixture(t, "bframes.mkv")
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EditList != nil {
		t.Errorf("a Matroska track came with an edit list: %+v", cfg.EditList)
	}
}

// An MP4 copied into an MP4 keeps its edit lists, edit for edit; the
// fragmented muxer restates the durations in its 90 kHz movie timescale.
func TestACopyKeepsTheEditLists(t *testing.T) {
	r, source := openFixture(t, "bframes-delayed.mp4")
	want := edits(t, source)
	var prog, frag bytes.Buffer
	if err := JoinProgressive(&prog, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	if err := Remux(&frag, r); err != nil {
		t.Fatal(err)
	}
	for i, got := range edits(t, prog.Bytes()) {
		if !slices.Equal(got, want[i]) {
			t.Errorf("progressive track %d: %+v; the source states %+v", i+1, got, want[i])
		}
	}
	for i, got := range edits(t, frag.Bytes()) {
		scaled := slices.Clone(want[i])
		for j := range scaled {
			scaled[j].SegmentDuration *= 90
		}
		if !slices.Equal(got, scaled) {
			t.Errorf("fragmented track %d: %+v; want %+v", i+1, got, scaled)
		}
	}
	// The track header of the progressive copy lasts what its edits do.
	parsed, err := mp4.DecodeFile(bytes.NewReader(prog.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Moov.Traks[1].Tkhd.Duration; got != 79+422 {
		t.Errorf("the delayed track lasts %d ms; its edits last %d", got, 79+422)
	}
}

// Several edits are written to a progressive file, which every reader
// honours, and refused for a fragmented one, which ffmpeg reads ignoring all
// but the start.
func TestSeveralEditsAreCopiedIntoAProgressiveFileOnly(t *testing.T) {
	r, source := openFixture(t, "bframes-twoedits.mp4")
	var prog bytes.Buffer
	if err := JoinProgressive(&prog, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	if got, want := edits(t, prog.Bytes())[0], edits(t, source)[0]; !slices.Equal(got, want) {
		t.Errorf("edit list %+v; the source states %+v", got, want)
	}
	if err := Remux(io.Discard, r); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("a fragmented copy of two edits: %v; want ErrTrackConfig", err)
	}
}

// Matroska has no edit lists: an empty edit at the head becomes a later start,
// and anything more is refused.
func TestMatroskaStatesTheStartOfAnEditListAndRefusesTheRest(t *testing.T) {
	r, _ := openFixture(t, "bframes-delayed.mp4")
	var out bytes.Buffer
	copyInto(t, NewWebMMuxer(&out), r)
	sound := blockTicks(readWebM(t, out.Bytes()), 2)
	// 79 ms of nothing, then the media from its start: the priming is shown,
	// as WebMMuxer shows any priming but Opus's.
	if sound[0] != 79 {
		t.Errorf("the sound starts at %d ms; the source starts it at 79", sound[0])
	}

	two, _ := openFixture(t, "bframes-twoedits.mp4")
	cfg, err := two.TrackConfig(two.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebMMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("two edits into Matroska: %v; want ErrTrackConfig", err)
	}
	// A list that is no list at all is refused before anything else.
	cfg.EditList = &EditList{MovieTimescale: 1000}
	if _, err := NewWebMMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("an empty list into Matroska: %v; want ErrTrackConfig", err)
	}
}

// A list starting before the first frame's composition time shows the first
// frame late, in the source and in a Matroska copy alike.
func TestAnEditStartingAtZeroShowsAReorderedFrameLate(t *testing.T) {
	cfg := webmVideoConfig()
	cfg.EditList = &EditList{MovieTimescale: 1000, Edits: []Edit{{MediaTime: 0, Rate: NormalRate}}}
	var out bytes.Buffer
	m := NewWebMMuxer(&out)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 9000, CompositionOffset: 9000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// 9000 ticks at 90 kHz is 100 ms.
	if got := blockTicks(readWebM(t, out.Bytes()), 1); got[0] != 100 {
		t.Errorf("the frame is shown at %d ms; the edit list shows it at 100", got[0])
	}
}

// A cut moves the media an edit list was written for. A list that only says
// where the presentation starts is replaced by the muxer's own; one that says
// more is refused.
func TestACutCarriesOnlyAStartOnlyEditList(t *testing.T) {
	r, _ := openFixture(t, "bframes.mp4")
	var out bytes.Buffer
	if err := Cut(&out, r, 120*time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	if got := firstPresented(t, out.Bytes(), 0); got != 0 {
		t.Errorf("the cut's first picture is shown at %d ticks; want 0", got)
	}
	for _, name := range []string{"bframes-delayed.mp4", "bframes-twoedits.mp4"} {
		r, _ := openFixture(t, name)
		if err := Cut(io.Discard, r, 120*time.Millisecond, 0); !errors.Is(err, ErrEditList) {
			t.Errorf("%s: %v; want ErrEditList", name, err)
		}
	}
}

// Two inputs one after the other: lists that only state a start are replaced,
// and one placing a track later than its siblings is refused.
func TestAConcatenationCarriesOnlyStartOnlyEditLists(t *testing.T) {
	a, _ := openFixture(t, "bframes.mp4")
	b, _ := openFixture(t, "bframes.mp4")
	var out bytes.Buffer
	if err := Concat(&out, []*Reader{a, b}); err != nil {
		t.Fatal(err)
	}
	if got := firstPresented(t, out.Bytes(), 0); got != 0 {
		t.Errorf("the first picture is shown at %d ticks; want 0", got)
	}
	c, _ := openFixture(t, "bframes-delayed.mp4")
	d, _ := openFixture(t, "bframes-delayed.mp4")
	if err := Concat(io.Discard, []*Reader{c, d}); !errors.Is(err, ErrEditList) {
		t.Errorf("%v; want ErrEditList", err)
	}
}

// An edit that ends before the media does trims the tail, which only matters
// to a copy that keeps the tail.
func TestAnEditEndingEarlyIsRefusedOnlyWhenTheTailIsKept(t *testing.T) {
	samples := []Sample{{Duration: 1000}, {Duration: 1000}, {Duration: 1000}}
	trimmed := sourceTrack{id: 7, samples: samples, cfg: TrackConfig{Timescale: 1000,
		EditList: &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 2500, MediaTime: 0, Rate: NormalRate}}}}}
	if _, err := dropEditList(trimmed, samples, true, "a test"); !errors.Is(err, ErrEditList) {
		t.Errorf("keeping the tail: %v; want ErrEditList", err)
	}
	cfg, err := dropEditList(trimmed, samples, false, "a test")
	if err != nil || cfg.EditList != nil {
		t.Errorf("dropping the tail: %v, %+v; want the list dropped", err, cfg.EditList)
	}
	// Short of the end by less than one tick of the movie timescale is
	// rounding, not trimming.
	coarse := trimmed
	coarse.cfg.Timescale = 48000
	coarse.samples = []Sample{{Duration: 48000}}
	coarse.cfg.EditList = &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 999, MediaTime: 10, Rate: NormalRate}}}
	if _, err := dropEditList(coarse, coarse.samples, true, "a test"); err != nil {
		t.Errorf("a list one movie tick short: %v", err)
	}
	// A list with no movie timescale cannot be measured against the media,
	// and is dropped as it stands: the muxers refuse to write it anyway.
	coarse.cfg.EditList = &EditList{Edits: []Edit{{SegmentDuration: 1, MediaTime: 0, Rate: NormalRate}}}
	if _, err := dropEditList(coarse, coarse.samples, true, "a test"); err != nil {
		t.Errorf("a list without a movie timescale: %v", err)
	}
}

// What the muxers refuse to write, and why.
func TestTheMuxersRefuseAListThatDescribesNoPresentation(t *testing.T) {
	for _, c := range []struct {
		name string
		el   EditList
	}{
		{"no movie timescale", EditList{Edits: []Edit{{MediaTime: 0, Rate: NormalRate}}}},
		{"no edit", EditList{MovieTimescale: 1000}},
		{"ending with an empty edit", EditList{MovieTimescale: 1000, Edits: []Edit{{MediaTime: 0, Rate: NormalRate}, {SegmentDuration: 5, MediaTime: EmptyEdit}}}},
		{"before the first sample", EditList{MovieTimescale: 1000, Edits: []Edit{{MediaTime: -5, Rate: NormalRate}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := audioConfig()
			cfg.EditList = &c.el
			if _, err := NewMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("Muxer: %v; want ErrTrackConfig", err)
			}
			if _, err := NewProgressiveMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("ProgressiveMuxer: %v; want ErrTrackConfig", err)
			}
		})
	}
}

// A fragmented file's last edit may run "to the end" with a duration of zero;
// a progressive copy states the length instead, which only a fragmented file
// may leave out.
func TestAProgressiveCopyOfAFragmentedFileStatesTheEditsLength(t *testing.T) {
	r, _ := openFixture(t, "bframes.mkv")
	var frag bytes.Buffer
	if err := Remux(&frag, r); err != nil {
		t.Fatal(err)
	}
	if e := edits(t, frag.Bytes())[0]; len(e) != 1 || e[0].SegmentDuration != 0 {
		t.Fatalf("the fragmented copy's edit list is %+v; the test needs one of duration zero", e)
	}
	back, err := NewReader(frag.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var prog bytes.Buffer
	if err := JoinProgressive(&prog, []*Reader{back}); err != nil {
		t.Fatal(err)
	}
	// Ten frames of 40 ms, the first shown at its composition offset.
	if e := edits(t, prog.Bytes())[0]; len(e) != 1 || e[0].SegmentDuration != 400 || e[0].MediaTime != 40 {
		t.Errorf("the progressive copy's edit list is %+v; want 400 ms from 40", e)
	}
}

// A fragmented file may start its first fragment late on its media timeline;
// its edit list counts from there, and the reader rebases it onto the samples
// it returns, which start at zero.
func TestAFragmentedEditListIsRebasedOntoTheFirstSample(t *testing.T) {
	r, _ := openFixture(t, "bframes.mkv")
	var frag bytes.Buffer
	if err := Remux(&frag, r); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(frag.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	video := back.TrackIDs()[0]
	for _, traf := range back.trackFragments(video) {
		traf.Tfdt.SetBaseMediaDecodeTime(traf.Tfdt.BaseMediaDecodeTime() + 1000)
	}
	cfg, err := back.TrackConfig(video)
	if err != nil {
		t.Fatal(err)
	}
	// The file says media time 40, counted from a timeline whose first sample
	// now decodes at 1000: 960 before it, which no sample holds.
	if got := cfg.EditList.Edits[0].MediaTime; got != 40-1000 {
		t.Errorf("rebased media time %d; want %d", got, 40-1000)
	}
	if _, err := NewProgressiveMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("a list naming media the file does not hold: %v; want ErrTrackConfig", err)
	}
	// A track fragment that states no decoding time starts where the track
	// does, and a track with no fragment at all has nothing to rebase.
	for _, traf := range back.trackFragments(video) {
		traf.Tfdt = nil
	}
	if got := back.firstDecodeTime(back.traks[video]); got != 0 {
		t.Errorf("without a tfdt: %d; want 0", got)
	}
	var empty bytes.Buffer
	m := NewMuxer(&empty)
	if _, err := m.AddTrack(audioConfig()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	none, err := NewReader(empty.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := none.firstDecodeTime(none.traks[1]); got != 0 {
		t.Errorf("a track with no fragment: %d; want 0", got)
	}
}

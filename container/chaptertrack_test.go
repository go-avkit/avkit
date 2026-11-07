// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// The sample entry and the media header of the chapter track are FFmpeg's, byte
// for byte, less the entry's btrt: the fixture is the file they were taken from.
func TestTheChapterTrackBoxesAreFFmpegs(t *testing.T) {
	r := mustReader(t, readFixture(t, "ffmpeg-chapters.mp4"))
	var found bool
	for _, trak := range movieBox(r.mp4).Traks {
		if trak.Mdia.Hdlr.HandlerType != "text" {
			continue
		}
		found = true
		var entry, gmhd bytes.Buffer
		if err := trak.Mdia.Minf.Stbl.Stsd.Children[0].Encode(&entry); err != nil {
			t.Fatal(err)
		}
		if err := trak.Mdia.Minf.Children[0].Encode(&gmhd); err != nil {
			t.Fatal(err)
		}
		// Size and type, the body, and a btrt of 20 bytes.
		e := entry.Bytes()
		if !bytes.Equal(e[8:len(e)-20], chapterEntry) {
			t.Errorf("entry body % x, FFmpeg writes % x", chapterEntry, e[8:len(e)-20])
		}
		if !bytes.Equal(gmhd.Bytes(), chapterGmhd) {
			t.Errorf("gmhd % x, FFmpeg writes % x", chapterGmhd, gmhd.Bytes())
		}
	}
	if !found {
		t.Fatal("the fixture has no chapter track")
	}
	// And a sample is what FFmpeg's is: the first of the fixture, byte for byte.
	samples, err := r.Samples(2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chapterSample("Intro"), samples[0].Data) || !bytes.Equal(chapterSample("Second é 日本"), samples[1].Data) {
		t.Errorf("chapterSample is not what FFmpeg writes: % x", chapterSample("Intro"))
	}
}

func TestAChapterTrackIsPlannedFromTheChapters(t *testing.T) {
	if _, ok := planChapterTrack(nil, 48000); ok {
		t.Errorf("no chapters were planned a track")
	}
	if _, ok := planChapterTrack([]Chapter{{Start: time.Second, Title: "late"}}, 48000); ok {
		t.Errorf("chapters beginning after 0 were planned a track")
	}
	plan, ok := planChapterTrack([]Chapter{{Title: "a"}, {Start: time.Second, Title: "b"}, {Start: time.Second, Title: "c"},
		{Start: 3 * time.Second, Title: "d"}}, 48000)
	if !ok || len(plan.samples) != 3 || plan.samples[0].Duration != 48000 || plan.samples[1].Duration != 0 ||
		plan.samples[2].Duration != 96000 || plan.start != 144000 || !bytes.Equal(plan.last, chapterSample("d")) {
		t.Fatalf("plan %+v, %v", plan, ok)
	}
	// The last chapter lasts to the end of the media, and has no length when
	// the media ends before it begins.
	if s := plan.lastSample(192000); s.Duration != 48000 || !s.Sync {
		t.Errorf("last sample %+v", s)
	}
	if s := plan.lastSample(100); s.Duration != 0 {
		t.Errorf("a media ending before the chapter gave it %d ticks", s.Duration)
	}
}

func TestAFirstChapterAfterZeroIsOnlyInTheChpl(t *testing.T) {
	r := mustReader(t, readFixture(t, "ffmpeg-chapters.mp4"))
	cfg, samples := audioOf(t, r)
	late := []Chapter{{Start: time.Second, Title: "late"}}
	for _, kind := range []string{"fragmented", "progressive"} {
		data := writeChaptered(t, kind, cfg, samples, late)
		f, err := Demux(data)
		if err != nil || len(f.Tracks) != 1 || len(f.Chapters) != 1 || f.Chapters[0].End != 0 {
			t.Errorf("%s: %+v, %v", kind, f, err)
		}
	}
}

func TestTheChapterTrackOfAProgressiveFileIsWrittenWhateverIsLeft(t *testing.T) {
	cfg := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000, Channels: 1, SampleRate: 48000}
	chapters := []Chapter{{Title: "a"}}
	// Chapters and no track.
	m := NewProgressiveMuxer(&bytes.Buffer{})
	if err := m.SetChapters(chapters); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); !errors.Is(err, ErrNoTracks) {
		t.Errorf("Close with chapters and no track = %v, want ErrNoTracks", err)
	}
	// A track declared and never written is Close's to name.
	m = NewProgressiveMuxer(&bytes.Buffer{})
	_ = m.SetChapters(chapters)
	if _, err := m.AddTrack(cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); !errors.Is(err, ErrNoSamples) {
		t.Errorf("Close with an unwritten track = %v, want ErrNoSamples", err)
	}
	// A writer that holds the media in memory has a limit, and the chapter
	// samples count against it.
	m = NewProgressiveMuxer(&bytes.Buffer{}, MediaMemoryLimit(10))
	_ = m.SetChapters(chapters)
	id, _ := m.AddTrack(cfg)
	if err := m.WriteSample(id, Sample{Data: []byte{1, 2, 3}, Duration: 1024, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err == nil {
		t.Errorf("Close wrote a chapter sample past the memory limit")
	}
}

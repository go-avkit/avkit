// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// What L12 of #54 found and never filed: an MP3 track copied from Matroska to
// MP4 kept Matroska's 1000 Hz media timescale, and ffmpeg 9.0.2 then decoded the
// encoder's priming the edit list said to leave out. testdata/sine.mp3 is
// libmp3lame's 0.3 s sine (13 230 samples); its Matroska copy by WebMMuxer states the delay as a
// CodecDelay of 25.056689 ms, and ffmpeg's own `-c copy` MP3-to-MP4 states it as
// an edit at media time 1105 in a track counted in samples (44100 Hz), which is
// what mov.c reads: the difference of two timescale ticks is its count of
// samples to skip (see audioclock.go).

// mp3MatroskaCopy is sine.mp3 as avkit writes it into Matroska, read back.
func mp3MatroskaCopy(t *testing.T) *Reader {
	t.Helper()
	data, err := os.ReadFile("testdata/sine.mp3")
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	var mkv bytes.Buffer
	copyInto(t, NewWebMMuxer(&mkv, BufferedSegment()), src)
	r, err := NewReader(mkv.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timescale != 1000 || cfg.SampleRate != 44100 {
		t.Fatalf("precondition: the Matroska copy counts in %d Hz at %d Hz", cfg.Timescale, cfg.SampleRate)
	}
	return r
}

// trakOf is the only track of an MP4 written by one of the muxers.
func trakOf(t *testing.T, data []byte) *mp4.TrakBox {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Moov == nil || len(f.Moov.Traks) != 1 {
		t.Fatal("not one track")
	}
	return f.Moov.Traks[0]
}

// The root cause held where CI sees it. ffmpeg's mov_fix_index reads three
// things of an audio track: the media timescale (mdhd), the edit's media_time
// and segment_duration (elst), and the sample durations (stts). Each is
// asserted from the bytes a muxer wrote, for the MP3 and the AAC that ffmpeg
// made, through both muxers.
//
// The decode counts that measured the defect -- 45 168 samples before for a
// 44 100-sample presentation, ffmpeg 9.0.2 -- are not a test: no ffmpeg runs in
// CI, and a test that skips every run tests nothing. They are in the PR.
func TestAnAudioTrackFromMatroskaIsWrittenInSamples(t *testing.T) {
	aac, err := os.ReadFile("testdata/ffmpeg-heaac.mkv")
	if err != nil {
		t.Fatal(err)
	}
	for name, fx := range map[string]struct {
		src     func() *Reader
		priming int64 // in samples, as ffmpeg's own MP4 of the same stream states it
	}{
		"mp3": {func() *Reader { return mp3MatroskaCopy(t) }, 1105},
		"aac": {func() *Reader {
			r, err := NewReader(aac)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}, 2112}, // HE-AAC: 2112 samples at 44.1 kHz, what libfdk-style SBR priming is
	} {
		for mux, newMuxer := range map[string]func(*bytes.Buffer) sampleCloser{
			"Muxer":            func(b *bytes.Buffer) sampleCloser { return NewMuxer(b) },
			"ProgressiveMuxer": func(b *bytes.Buffer) sampleCloser { return NewProgressiveMuxer(b) },
		} {
			t.Run(name+"/"+mux, func(t *testing.T) {
				src := fx.src()
				cfg, err := src.TrackConfig(1)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Timescale != 1000 {
					t.Fatalf("precondition: the source counts in %d Hz", cfg.Timescale)
				}
				rate := uint32(cfg.SampleRate)
				priming := primingTicks(cfg.StartDelay, rate)
				if priming != fx.priming {
					t.Fatalf("precondition: a priming of %d samples at %d Hz; want %d", priming, rate, fx.priming)
				}
				sourceSamples, err := src.Samples(1)
				if err != nil {
					t.Fatal(err)
				}
				var ms uint64
				for _, s := range sourceSamples {
					ms += uint64(s.Duration)
				}

				var out bytes.Buffer
				copyInto(t, newMuxer(&out), src)
				file, err := mp4.DecodeFile(bytes.NewReader(out.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				trak := file.Moov.Traks[0]
				if got := trak.Mdia.Mdhd.Timescale; got != rate {
					t.Errorf("mdhd timescale %d; want the sample rate %d", got, rate)
				}
				entries := trak.Edts.Elst[0].Entries
				if len(entries) != 1 || entries[0].MediaTime != priming {
					t.Fatalf("elst %+v; want one edit at media time %d samples", entries, priming)
				}
				// The sum of the stts durations is the source's length, converted:
				// read back through this package, which reads the tables.
				back, err := NewReader(out.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				written, err := back.Samples(1)
				if err != nil {
					t.Fatal(err)
				}
				var total uint64
				for _, s := range written {
					total += uint64(s.Duration)
				}
				if want := (ms*uint64(rate) + 500) / 1000; total != want {
					t.Errorf("stts durations add up to %d; want %d, the source's %d ms in samples", total, want, ms)
				}
				// The edit's length: the source's own, when it stated one; else what
				// the muxer states, nothing in a fragmented file (to the end) and the
				// rest of the media after the priming in a progressive one.
				movie := file.Moov.Mvhd.Timescale
				var wantSegment uint64
				switch {
				case cfg.EditList != nil:
					wantSegment = scaleDurationUp(cfg.EditList.Edits[0].SegmentDuration, cfg.EditList.MovieTimescale, movie)
				case mux == "ProgressiveMuxer":
					wantSegment = scaleDurationUp(total-uint64(priming), rate, movie)
				}
				if got := entries[0].SegmentDuration; got != wantSegment {
					t.Errorf("segment duration %d; want %d", got, wantSegment)
				}
				if len(written) != len(sourceSamples) {
					t.Errorf("%d samples written; want %d", len(written), len(sourceSamples))
				}
			})
		}
	}
}

// A file whose track is already counted in samples, or finer, is written as it
// came, and so is anything that is not audio.
func TestAudioTimescaleIsTheSampleRateOnlyWhereTheTimescaleIsCoarser(t *testing.T) {
	huge := uint64(1) << 33
	for name, tc := range map[string]struct {
		cfg  TrackConfig
		want uint32
	}{
		"mp3 in milliseconds": {TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000, SampleRate: 44100}, 44100},
		"aac in milliseconds": {TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 1000, SampleRate: 48000}, 48000},
		"opus at 44.1 kHz":    {TrackConfig{Kind: Audio, Codec: "Opus", Timescale: 1000, SampleRate: 44100}, 48000},
		"opus, lower case":    {TrackConfig{Kind: Audio, Codec: "opus", Timescale: 1000}, 48000},
		"already in samples":  {TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 44100, SampleRate: 44100}, 44100},
		"finer than samples":  {TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 90000, SampleRate: 44100}, 90000},
		"no rate stated":      {TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000}, 1000},
		"a rate no box holds": {TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000, SampleRate: int(huge)}, 1000},
		"video":               {TrackConfig{Kind: Video, Codec: "avc1", Timescale: 1000, SampleRate: 44100}, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			if got := audioTimescale(tc.cfg); got != tc.want {
				t.Errorf("audioTimescale = %d; want %d", got, tc.want)
			}
		})
	}
}

func TestWithAudioClockConvertsTheEditListAndLeavesTheCallersAlone(t *testing.T) {
	el := &EditList{MovieTimescale: 1000, Edits: []Edit{
		{SegmentDuration: 100, MediaTime: EmptyEdit, Rate: NormalRate},
		{SegmentDuration: 900, MediaTime: 25, Rate: NormalRate},
		{SegmentDuration: 50, MediaTime: 500, Rate: NormalRate},
	}}
	cfg := TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000, SampleRate: 44100,
		StartDelay: 25056689 * time.Nanosecond, EditList: el}
	got, clock := withAudioClock(cfg)
	if clock == nil || got.Timescale != 44100 {
		t.Fatalf("timescale %d, clock %v", got.Timescale, clock)
	}
	want := []int64{EmptyEdit, 1105, 22050}
	for i, e := range got.EditList.Edits {
		if e.MediaTime != want[i] || e.SegmentDuration != el.Edits[i].SegmentDuration {
			t.Errorf("edit %d = %+v; want media time %d and the same segment duration", i, e, want[i])
		}
	}
	if el.Edits[1].MediaTime != 25 || cfg.Timescale != 1000 {
		t.Errorf("the caller's configuration was changed: %+v %+v", el.Edits, cfg)
	}

	// Nothing to convert is nothing changed; a track without an edit list needs
	// no edit list built.
	same, clock := withAudioClock(TrackConfig{Kind: Video, Timescale: 1000})
	if clock != nil || same.Timescale != 1000 {
		t.Errorf("a video track was converted: %+v %v", same, clock)
	}
	bare, clock := withAudioClock(TrackConfig{Kind: Audio, Timescale: 1000, SampleRate: 48000})
	if clock == nil || bare.EditList != nil || bare.Timescale != 48000 {
		t.Errorf("a track without an edit list: %+v %v", bare, clock)
	}
}

func TestAnAudioClockKeepsTheTrackItsLength(t *testing.T) {
	clock := &audioClock{from: 1000, to: 44100}
	var total uint64
	for i := 0; i < 1000; i++ {
		s, err := clock.sample(Sample{Duration: 26, CompositionOffset: -3})
		if err != nil {
			t.Fatal(err)
		}
		total += uint64(s.Duration)
		if s.CompositionOffset != -132 {
			t.Fatalf("offset %d; want -3 ms = -132 samples", s.CompositionOffset)
		}
	}
	if want := uint64(26 * 44100); total != want {
		t.Errorf("1000 samples of 26 ms last %d ticks; want %d", total, want)
	}
	s, err := (*audioClock)(nil).sample(Sample{Duration: 7, CompositionOffset: 3})
	if err != nil || s.Duration != 7 || s.CompositionOffset != 3 {
		t.Errorf("a nil clock changed %+v: %v", s, err)
	}
	s, err = (&audioClock{from: 1000, to: 44100}).sample(Sample{Duration: 10, CompositionOffset: 5})
	if err != nil || s.Duration != 441 || s.CompositionOffset != 221 {
		t.Errorf("10 ms and +5 ms became %+v: %v", s, err)
	}
}

func TestAnAudioClockRefusesATimeItsTimescaleCannotHold(t *testing.T) {
	for name, tc := range map[string]struct {
		clock *audioClock
		s     Sample
	}{
		"a total past 64 bits":   {&audioClock{from: 1000, to: 44100, in: math.MaxUint64 - 1}, Sample{Duration: 1}},
		"a duration past 32 bit": {&audioClock{from: 1000, to: 44100}, Sample{Duration: 1 << 31}},
		"an offset past 32 bit":  {&audioClock{from: 1000, to: 44100}, Sample{Duration: 1, CompositionOffset: math.MaxInt32}},
		"the smallest offset":    {&audioClock{from: 1000, to: 44100}, Sample{Duration: 1, CompositionOffset: math.MinInt32}},
	} {
		t.Run(name, func(t *testing.T) {
			before := *tc.clock
			if _, err := tc.clock.sample(tc.s); !errors.Is(err, ErrSample) {
				t.Errorf("error = %v; want ErrSample", err)
			}
			if *tc.clock != before {
				t.Errorf("a refused sample moved the clock: %+v", tc.clock)
			}
		})
	}
	for name, run := range map[string]func() error{
		"Muxer": func() error {
			m := NewMuxer(new(bytes.Buffer))
			id, err := m.AddTrack(TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000, SampleRate: 44100, Channels: 2})
			if err != nil {
				return err
			}
			return m.WriteSample(id, Sample{Data: []byte{1}, Duration: 1 << 31, Sync: true})
		},
		"ProgressiveMuxer": func() error {
			m := NewProgressiveMuxer(new(bytes.Buffer))
			id, err := m.AddTrack(TrackConfig{Kind: Audio, Codec: "mp3", Timescale: 1000, SampleRate: 44100, Channels: 2})
			if err != nil {
				return err
			}
			return m.WriteSample(id, Sample{Data: []byte{1}, Duration: 1 << 31, Sync: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, ErrSample) {
				t.Errorf("error = %v; want ErrSample", err)
			}
		})
	}
}

// ffmpeg's own copies are the reference for what is written: the MP3 in an MP4
// it makes states its priming in samples.
func TestFFmpegsMP3CopyStatesItsPrimingInSamples(t *testing.T) {
	data, err := os.ReadFile("testdata/ffmpeg-mp3.mp4")
	if err != nil {
		t.Fatal(err)
	}
	trak := trakOf(t, data)
	entry, err := trak.Mdia.Minf.Stbl.Stsd.GetSampleDescription(0)
	if err != nil {
		t.Fatal(err)
	}
	audio, ok := entry.(*mp4.AudioSampleEntryBox)
	if !ok {
		t.Fatalf("sample entry %T", entry)
	}
	if got := trak.Mdia.Mdhd.Timescale; got != uint32(audio.SampleRate) {
		t.Errorf("ffmpeg's MP3 in MP4 counts in %d Hz at a sample rate of %d", got, audio.SampleRate)
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// What L11 found: the LAME extension states the encoder's padding at the end
// as well as its delay at the start, and only the delay was read (#59), so a
// copy of an MP3 decoded that padding as sound: ffmpeg 9.0.2 decoded 44 975
// samples of avkit's MP4 and Matroska copies of a 1 s, 44 100-sample
// libmp3lame file, and 44 100 of the file itself.
//
// testdata/sine.mp3 is 13 frames of 1152 behind a LAME tag stating a delay of
// 576 and a padding of 1170: mp3dec.c presents 13230 samples from 1105 on, and
// ffmpeg's -c copy writes that as an MP4 edit of 0.3 s at media time 1105 and
// as a DiscardPadding of 14535147 ns -- 641 samples -- on the last Matroska
// block.
func TestAnMP3TrimsTheEncodersPaddingAtTheEnd(t *testing.T) {
	data, err := os.ReadFile("testdata/sine.mp3")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	el := cfg.EditList
	if el == nil || el.MovieTimescale != 44100 || len(el.Edits) != 1 ||
		el.Edits[0] != (Edit{SegmentDuration: 13230, MediaTime: 1105, Rate: NormalRate}) {
		t.Fatalf("edit list %+v; want 13230 samples from 1105 at 44.1 kHz", el)
	}

	var progressive bytes.Buffer
	copyInto(t, NewProgressiveMuxer(&progressive), r)
	parsed, err := mp4.DecodeFile(bytes.NewReader(progressive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	entries := parsed.Moov.Traks[0].Edts.Elst[0].Entries
	movie := parsed.Moov.Mvhd.Timescale
	if len(entries) != 1 || entries[0].MediaTime != 1105 ||
		entries[0].SegmentDuration != scaleDurationUp(13230, 44100, movie) {
		t.Errorf("MP4 edit list %+v in a movie timescale of %d; want 0.3 s at 1105", entries, movie)
	}

	var mkv bytes.Buffer
	copyInto(t, NewWebMMuxer(&mkv, BufferedSegment()), r)
	var f trimFile
	if err := ebml.Unmarshal(bytes.NewReader(mkv.Bytes()), &f); err != nil {
		t.Fatal(err)
	}
	clusters := f.Segment.Cluster
	groups := clusters[len(clusters)-1].BlockGroup
	if len(groups) != 1 || groups[0].DiscardPadding != 14535147 {
		t.Errorf("last block groups %+v; want one with ffmpeg's padding of 14535147 ns", groups)
	}
}

// A trimmed MP3 is now what a gapless AAC MP4 already was to Concat: an edit
// ending before the media, which a concatenation cannot carry, so joining two
// is refused with ErrEditList instead of presenting the first one's padding
// between them. Cut keeps the stretch it is asked for, and so drops the trim
// only where it keeps the end.
func TestConcatOfTwoTrimmedMP3sIsRefused(t *testing.T) {
	data, err := os.ReadFile("testdata/sine.mp3")
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Concat(io.Discard, []*Reader{a, b}); !errors.Is(err, ErrEditList) {
		t.Errorf("Concat = %v, want ErrEditList", err)
	}
	if err := Cut(io.Discard, a, 0, 100*time.Millisecond); err != nil {
		t.Errorf("a cut that leaves the end out: %v", err)
	}
}

// The padding is read only where mp3dec.c reads it, and only where it trims
// something the stream holds.
func TestAnMP3EndTrimNeedsAFrameCountThatHolds(t *testing.T) {
	lame := mpegAudioTag{lame: true, delay: 1105, endPad: 1170, frames: 13, counted: true}
	if el := lame.endTrim(13, 1152, 44100); el == nil || el.Edits[0].SegmentDuration != 13230 {
		t.Fatalf("the trim of sine.mp3 = %+v", el)
	}
	for name, tc := range map[string]struct {
		tag    mpegAudioTag
		frames int
	}{
		"no LAME extension":             {mpegAudioTag{endPad: 1170, frames: 13, counted: true}, 13},
		"no frame count":                {mpegAudioTag{lame: true, delay: 1105, endPad: 1170}, 13},
		"a count the stream outran":     {lame, 14},
		"a count the stream fell short": {lame, 12},
		// The decoder's own 529 carry a padding this short past the last frame.
		"a padding the decoder hides": {mpegAudioTag{lame: true, delay: 1105, endPad: 529, frames: 13, counted: true}, 13},
		"nothing left to present":     {mpegAudioTag{lame: true, delay: 1105, endPad: 4095, frames: 2, counted: true}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			if el := tc.tag.endTrim(tc.frames, 1152, 44100); el != nil {
				t.Errorf("trim %+v; want none", el)
			}
		})
	}
}

// The frame count and the padding are read from the tag's own bytes: an Info
// tag stating 3 frames over a stream of 3, with a LAME delay of 576 and a
// padding of 2000. 3456 samples decode; mp3dec.c presents from 1105 and drops
// from 3456 - 2000 + 529 = 1985 on, 880 samples.
func TestTheLAMETagStatesItsPaddingAndFrameCount(t *testing.T) {
	frame := xingFrame(t, mpaHeader(1, 3, 9, 0, 0, 0), "Info", xingFrames, "LAME3.100", 576)
	// The frame count is the field after the flags; the padding is the low
	// twelve bits of the three bytes past the encoder string and the twelve
	// bytes after it.
	at := 4 + 32 + 8
	frame[at+3] = 3
	d := frame[at+4+21:]
	d[1] |= byte(2000 >> 8)
	d[2] = byte(2000 & 0xFF)
	h, _ := parseMPAFrame(frame)
	tag, info := mpegAudioInfoFrame(frame, h)
	if !info || !tag.lame || tag.delay != 1105 || tag.endPad != 2000 || !tag.counted || tag.frames != 3 {
		t.Fatalf("tag %+v", tag)
	}
	r, err := NewReader(concat(frame, mp3Frame(t), mp3Frame(t), mp3Frame(t)))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.TrackConfig(1)
	if el := cfg.EditList; el == nil || el.Edits[0] != (Edit{SegmentDuration: 880, MediaTime: 1105, Rate: NormalRate}) {
		t.Errorf("edit list %+v; want 880 samples from 1105", el)
	}
}

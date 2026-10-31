// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The parsers below read text and boxes out of files nobody vouches for. What
// these targets ask is that no input makes them panic, and that what one of
// them accepts the next stage can use.

func FuzzReadWebVTTSample(f *testing.F) {
	f.Add(vtteSample)
	for _, b := range []mp4.Box{
		cueBox(&webvttCue{id: "id", settings: "line:0", text: "text", source: 3}),
		&mp4.VttaBox{CueAdditionalText: "NOTE"},
	} {
		var buf bytes.Buffer
		b.Encode(&buf)
		f.Add(buf.Bytes())
	}
	f.Add([]byte{0, 0, 0, 0, 'v', 't', 't', 'c'})
	f.Add([]byte{0, 0, 0, 1, 'v', 't', 't', 'c', 0, 0, 0, 0, 0, 0, 0, 16})
	f.Fuzz(func(t *testing.T, data []byte) {
		cues, err := readWebVTTSample(data)
		if err != nil {
			return
		}
		// What reads as cues writes as a Matroska block in either mapping.
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf)
		id, _ := m.AddTrack(TrackConfig{Codec: "wvtt", Timescale: 1000})
		if len(data) > 0 {
			if err := m.WriteSample(id, Sample{Data: data, Duration: 10, Sync: true}); err != nil {
				t.Fatalf("a sample that reads as %d cues does not write: %v", len(cues), err)
			}
		}
		m.Close()
	})
}

func FuzzMatroskaCue(f *testing.F) {
	f.Add([]byte("id\nline:0\ntext"), []byte("line:0\nid\nNOTE a\n\nNOTE b"))
	f.Add([]byte("\r\n\r\n"), []byte("\n\n"))
	f.Add([]byte("no newline"), []byte("one\n"))
	f.Fuzz(func(t *testing.T, block, addition []byte) {
		if c, err := readWebMCueBlock(block); err == nil {
			// The WebM mapping writes what it reads, line terminators aside.
			again, err := readWebMCueBlock(webmCueBlock(c))
			if err != nil || again.id != c.id || again.settings != c.settings {
				t.Fatalf("%q read as %+v and back as %+v, %v", block, c, again, err)
			}
		}
		var c webvttCue
		if err := readMatroskaCueAddition(&c, addition); err == nil && addition != nil {
			var again webvttCue
			if err := readMatroskaCueAddition(&again, matroskaCueAddition(c)); err != nil {
				t.Fatalf("%q read as %+v and does not read back: %v", addition, c, err)
			}
		}
	})
}

func FuzzShiftCueTimestamps(f *testing.F) {
	f.Add("a <00:01.000>b <01:02:03.456>c", int64(time.Second))
	f.Add("<b>bold</b> <00:00.000>", int64(-time.Second))
	f.Add("<99:59:59.999> <", int64(time.Hour))
	f.Fuzz(func(t *testing.T, text string, delta int64) {
		// A delta beyond a few days is not a cue's; it keeps the arithmetic
		// in range the way a real timeline does.
		d := time.Duration(delta % int64(72*time.Hour))
		shifted, err := shiftCueTimestamps(text, d)
		if err != nil {
			return
		}
		if d == 0 && shifted != text {
			t.Fatalf("shifting %q by nothing changed it to %q", text, shifted)
		}
		if !hasCueTimestamp(text) && shifted != text {
			t.Fatalf("%q holds no timestamp and changed to %q", text, shifted)
		}
	})
}

func FuzzMatroskaTextTrack(f *testing.F) {
	for _, name := range []string{"subtitles-webvtt.mkv", "subtitles-srt.mkv", "subtitles-overlap.mkv"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := NewReader(data)
		if r == nil || err != nil {
			return
		}
		// Whatever a text track reads as, every muxer that takes it takes it.
		for _, id := range r.TrackIDs() {
			cfg, err := r.TrackConfig(id)
			if err != nil || cfg.Codec != codecWebVTT {
				continue
			}
			samples, err := r.Samples(id)
			if err != nil {
				continue
			}
			for _, m := range []closer{NewMuxer(io.Discard), NewWebMMuxer(io.Discard)} {
				out, err := m.AddTrack(cfg)
				if err != nil {
					t.Fatalf("AddTrack(%+v): %v", cfg, err)
				}
				for i, s := range samples {
					if err := m.WriteSample(out, s); err != nil {
						t.Fatalf("sample %d: %v", i+1, err)
					}
				}
				if err := m.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
		}
	})
}

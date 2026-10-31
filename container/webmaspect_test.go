// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/at-wat/ebml-go"
)

// displayFile is a written file's video track entries read back with ebml-go.
type displayFile struct {
	Header struct {
		DocType string `ebml:"EBMLDocType"`
	} `ebml:"EBML"`
	Segment struct {
		Tracks struct {
			TrackEntry []struct {
				Video *mkvVideo `ebml:"Video"`
			} `ebml:"TrackEntry"`
		} `ebml:"Tracks"`
	} `ebml:"Segment"`
}

// writeDisplay writes one VP9 track, and an AAC one beside it when matroska is
// set -- which is what makes the document "matroska" rather than "webm" -- and
// returns the file with the video track's Video element as written.
func writeDisplay(t *testing.T, cfg TrackConfig, matroska bool) ([]byte, string, mkvVideo) {
	t.Helper()
	var out bytes.Buffer
	m := NewWebMMuxer(&out, BufferedSegment())
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if matroska {
		if _, err := m.AddTrack(TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000,
			SampleRate: 48000, Channels: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.WriteSample(id, Sample{Data: vp9KeyFrame, Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	var f displayFile
	if err := ebml.Unmarshal(bytes.NewReader(out.Bytes()), &f, ebml.WithIgnoreUnknown(true)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), f.Header.DocType, *f.Segment.Tracks.TrackEntry[0].Video
}

// readSAR reads the first track's aspect ratio back through this package.
func readSAR(t *testing.T, data []byte) (int, int) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	return cfg.SARWidth, cfg.SARHeight
}

// What L11 found: WebMMuxer wrote no DisplayWidth or DisplayHeight, so an
// anamorphic MP4 copied to Matroska read back with square pixels -- ffprobe said
// 1:1 for ffmpeg's 320x240 setsar=4/3 file, where ffmpeg's own copy says 4:3.
// The display size is now written the way ffmpeg's matroskaenc writes it: in a
// Matroska file the display aspect ratio, 16x9 under DisplayUnit 3, which reads
// back exactly; in a WebM file the width in pixels, 427 by 240, which reads back
// as the 427:320 ffmpeg's own WebM reads back as.
func TestWebMMuxerStatesTheAspectRatio(t *testing.T) {
	cfg := vp9Anamorphic()
	data, doctype, v := writeDisplay(t, cfg, true)
	if doctype != "matroska" || v.DisplayWidth != 16 || v.DisplayHeight != 9 || v.DisplayUnit != 3 {
		t.Errorf("%s: display %dx%d unit %d; want 16x9 unit 3", doctype, v.DisplayWidth, v.DisplayHeight, v.DisplayUnit)
	}
	if h, w := readSAR(t, data); h != 4 || w != 3 {
		t.Errorf("matroska read back %d:%d, want 4:3", h, w)
	}
	data, doctype, v = writeDisplay(t, cfg, false)
	if doctype != "webm" || v.DisplayWidth != 427 || v.DisplayHeight != 240 || v.DisplayUnit != 0 {
		t.Errorf("%s: display %dx%d unit %d; want 427x240 unit 0", doctype, v.DisplayWidth, v.DisplayHeight, v.DisplayUnit)
	}
	if h, w := readSAR(t, data); h != 427 || w != 320 {
		t.Errorf("webm read back %d:%d, want 427:320", h, w)
	}
}

// Square pixels need no display size in either document: its default is the
// pixel size, which states them.
func TestWebMMuxerWritesNoDisplaySizeForSquarePixels(t *testing.T) {
	cfg := vp9Anamorphic()
	cfg.SARWidth, cfg.SARHeight = 3, 3
	for _, matroska := range []bool{true, false} {
		data, doctype, v := writeDisplay(t, cfg, matroska)
		if v.DisplayWidth != 0 || v.DisplayHeight != 0 || v.DisplayUnit != 0 {
			t.Errorf("%s: display %dx%d unit %d for square pixels", doctype, v.DisplayWidth, v.DisplayHeight, v.DisplayUnit)
		}
		if h, w := readSAR(t, data); h != 1 || w != 1 {
			t.Errorf("%s read back %d:%d, want 1:1", doctype, h, w)
		}
	}
}

// A ratio nobody knows is not written as a square one where Matroska can say
// so: DisplayUnit 4, "unknown", which reads back as 0:0. A WebM file has no such
// unit in ffmpeg's practice and states nothing.
func TestWebMMuxerStatesAnUnknownAspectRatio(t *testing.T) {
	cfg := vp9Anamorphic()
	cfg.SARWidth, cfg.SARHeight = 0, 0
	data, _, v := writeDisplay(t, cfg, true)
	if v.DisplayUnit != mkvDisplayUnknown || v.DisplayWidth != 0 {
		t.Errorf("matroska: display %dx%d unit %d; want unit 4 alone", v.DisplayWidth, v.DisplayHeight, v.DisplayUnit)
	}
	if h, w := readSAR(t, data); h != 0 || w != 0 {
		t.Errorf("matroska read back %d:%d, want 0:0", h, w)
	}
	_, _, v = writeDisplay(t, cfg, false)
	if v.DisplayUnit != 0 || v.DisplayWidth != 0 || v.DisplayHeight != 0 {
		t.Errorf("webm: display %dx%d unit %d; want nothing", v.DisplayWidth, v.DisplayHeight, v.DisplayUnit)
	}
}

// A ratio whose display size Matroska cannot hold is refused when the track is
// added, as matroskaenc refuses a display width past INT_MAX.
func TestWebMMuxerRefusesADisplaySizeItCannotState(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*TrackConfig)
		// wide says only an int wider than 32 bits can state the case: two
		// 32-bit factors always fit a 64-bit product.
		wide bool
	}{
		"a width past INT_MAX":     {func(c *TrackConfig) { c.SARWidth, c.SARHeight = math.MaxInt32, 1 }, false},
		"a width rounding to none": {func(c *TrackConfig) { c.SARWidth, c.SARHeight = 1, 1000 }, false},
		"a product past 64 bits":   {func(c *TrackConfig) { c.Width, c.SARWidth, c.SARHeight = 1<<16, math.MaxInt, 3 }, true},
		"a height past 64 bits":    {func(c *TrackConfig) { c.Height, c.SARWidth, c.SARHeight = 1<<16, 1, math.MaxInt }, true},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.wide && math.MaxInt <= math.MaxInt32 {
				t.Skip("an int on this platform cannot state the ratio")
			}
			cfg := vp9Anamorphic()
			tc.edit(&cfg)
			if _, err := NewWebMMuxer(&bytes.Buffer{}).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

// The width is rounded to the nearest pixel, half up, as av_rescale rounds it,
// and the display aspect ratio reduced exactly.
func TestWebMAspectRounding(t *testing.T) {
	for _, c := range []struct {
		w, h, sarW, sarH  int
		width, darW, darH uint64
	}{
		{320, 240, 4, 3, 427, 16, 9},  // 426.67
		{2, 2, 3, 4, 2, 3, 4},         // 1.5 rounds up
		{720, 576, 16, 15, 768, 4, 3}, // exact
		{1, 1, 1, 1, 1, 1, 1},
	} {
		a, err := webmAspectOf(TrackConfig{Width: c.w, Height: c.h, SARWidth: c.sarW, SARHeight: c.sarH})
		if err != nil {
			t.Fatal(err)
		}
		if !a.known || a.webmWidth != c.width || a.darW != c.darW || a.darH != c.darH {
			t.Errorf("%dx%d at %d:%d = %+v; want width %d, ratio %d:%d", c.w, c.h, c.sarW, c.sarH, a, c.width, c.darW, c.darH)
		}
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// vp9Anamorphic is a VP9 track whose pixels are 4:3 wide, stated unreduced so
// the reduction is exercised too.
func vp9Anamorphic() TrackConfig {
	return TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 320, Height: 240,
		SARWidth: 8, SARHeight: 6,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8},
	}
}

// What L10 found: a VP8 or VP9 track written to MP4 carried no pasp box, so
// ffmpeg read its sample aspect ratio as 0/1 -- unknown -- where the WebM it came
// from said 1/1. VP8 and VP9 state no aspect ratio in their bitstream, so the
// sample entry is the only place an MP4 can say it (ISO/IEC 14496-12 §12.1.4).
// Both muxers now write it, reduced as movenc reduces it, with the track
// header's width scaled by it, and the reader reads it back.
func TestTheMP4MuxersWriteAKnownAspectRatioAsPasp(t *testing.T) {
	for name, write := range map[string]func(*testing.T, TrackConfig) []byte{
		"fragmented": func(t *testing.T, cfg TrackConfig) []byte { return writeOneTrack(t, cfg, 2) },
		"progressive": func(t *testing.T, cfg TrackConfig) []byte {
			var buf bytes.Buffer
			m := NewProgressiveMuxer(&buf)
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.WriteSample(id, Sample{Data: vp9KeyFrame, Duration: 3000, Sync: true}); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := write(t, vp9Anamorphic())
			parsed, err := mp4.DecodeFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			trak := parsed.Moov.Traks[0]
			pasp := trak.Mdia.Minf.Stbl.Stsd.VpXX.Pasp
			if pasp == nil || pasp.HSpacing != 4 || pasp.VSpacing != 3 {
				t.Fatalf("pasp = %+v, want 4:3", pasp)
			}
			// 320 pixels four thirds wide are shown 426.67 wide, in 16.16.
			if want := mp4.Fixed32(320 << 16 * 4 / 3); trak.Tkhd.Width != want {
				t.Errorf("track header width = %#x, want %#x", trak.Tkhd.Width, want)
			}
			r, err := NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.TrackConfig(r.TrackIDs()[0])
			if err != nil {
				t.Fatal(err)
			}
			if got.SARWidth != 4 || got.SARHeight != 3 {
				t.Errorf("read back %d:%d, want 4:3", got.SARWidth, got.SARHeight)
			}
		})
	}
}

// An unknown ratio is not written as a square one: no pasp at all, the track
// header keeps the frame width, and the reader says 0:0 back.
func TestAnUnknownAspectRatioWritesNoPasp(t *testing.T) {
	cfg := vp9Anamorphic()
	cfg.SARWidth, cfg.SARHeight = 0, 0
	data := writeOneTrack(t, cfg, 1)
	if pasp := sampleEntryOf(t, data).VpXX.Pasp; pasp != nil {
		t.Errorf("pasp = %+v for a ratio nobody stated", pasp)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.SARWidth != 0 || got.SARHeight != 0 {
		t.Errorf("read back %d:%d, want 0:0", got.SARWidth, got.SARHeight)
	}
}

// An audio track has no pixel: a ratio stated on one is not a reason to refuse
// it, and no box is written for it.
func TestAnAspectRatioOnAnAudioTrackIsIgnored(t *testing.T) {
	data := writeOneTrack(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2,
		SARWidth: 4, SARHeight: 3,
	}, 1)
	if sampleEntryOf(t, data).Mp4a == nil {
		t.Fatal("no mp4a entry")
	}
}

// A display width the 16.16 track header cannot hold keeps the frame width,
// as movenc keeps it, rather than wrapping.
func TestATrackHeaderWidthThatDoesNotFitKeepsTheFrameWidth(t *testing.T) {
	cfg := vp9Anamorphic()
	cfg.Width, cfg.SARWidth, cfg.SARHeight = 640, math.MaxInt32, 1
	parsed, err := mp4.DecodeFile(bytes.NewReader(writeOneTrack(t, cfg, 1)))
	if err != nil {
		t.Fatal(err)
	}
	trak := parsed.Moov.Traks[0]
	if want := mp4.Fixed32(640 << 16); trak.Tkhd.Width != want {
		t.Errorf("track header width = %#x, want the frame's %#x", trak.Tkhd.Width, want)
	}
	if pasp := trak.Mdia.Minf.Stbl.Stsd.VpXX.Pasp; pasp == nil || pasp.HSpacing != math.MaxInt32 {
		t.Errorf("pasp = %+v", pasp)
	}
}

// A side a pasp box cannot hold is refused, rather than truncated into a
// different shape. Only an int wider than 32 bits can state one.
func TestAnAspectRatioPastThirtyTwoBitsIsRefused(t *testing.T) {
	if math.MaxInt <= math.MaxUint32 {
		t.Skip("an int on this platform cannot exceed a pasp field")
	}
	wide := uint64(math.MaxUint32)
	wide++
	cfg := vp9Anamorphic()
	cfg.SARWidth = int(wide)
	if _, err := NewMuxer(io.Discard).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
		t.Errorf("err = %v, want ErrTrackConfig", err)
	}
}

// paspRatio reads only what describes a pixel.
func TestPaspRatioReadsOnlyAPixel(t *testing.T) {
	visual := func(p *mp4.PaspBox) *mp4.StsdBox {
		stsd := &mp4.StsdBox{}
		entry := mp4.CreateVisualSampleEntryBox("vp09", 320, 240, nil)
		if p != nil {
			entry.AddChild(p)
		}
		stsd.AddChild(entry)
		return stsd
	}
	audio := &mp4.StsdBox{}
	audio.AddChild(mp4.CreateAudioSampleEntryBox("mp4a", 2, 16, 48000, nil))
	for name, tc := range map[string]struct {
		stsd *mp4.StsdBox
		h, v int
	}{
		"no sample entry":     {&mp4.StsdBox{}, 0, 0},
		"an audio entry":      {audio, 0, 0},
		"no pasp":             {visual(nil), 0, 0},
		"a zero side":         {visual(&mp4.PaspBox{HSpacing: 0, VSpacing: 1}), 0, 0},
		"a ratio":             {visual(&mp4.PaspBox{HSpacing: 16, VSpacing: 11}), 16, 11},
		"the largest spacing": {visual(&mp4.PaspBox{HSpacing: math.MaxInt32, VSpacing: 1}), math.MaxInt32, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if h, v := paspRatio(tc.stsd); h != tc.h || v != tc.v {
				t.Errorf("paspRatio = %d:%d, want %d:%d", h, v, tc.h, tc.v)
			}
		})
	}
}

// mkvAspect reads a Matroska track's pixel shape as RFC 9559 §5.1.4.1.28
// defines it and as ffmpeg's matroskadec computes it.
func TestMatroskaDisplaySizeStatesThePixelShape(t *testing.T) {
	for name, tc := range map[string]struct {
		video mkvVideo
		h, v  int
	}{
		// What ffmpeg writes for square pixels: nothing, so the display
		// size defaults to the pixel size.
		"no display size": {mkvVideo{PixelWidth: 320, PixelHeight: 240}, 1, 1},
		// What ffmpeg writes for setsar=4/3 on 320x240: DisplayWidth 427,
		// which it reads back as 427:320.
		"a display width":  {mkvVideo{PixelWidth: 320, PixelHeight: 240, DisplayWidth: 427}, 427, 320},
		"a display height": {mkvVideo{PixelWidth: 320, PixelHeight: 240, DisplayHeight: 120}, 2, 1},
		"a display aspect ratio": {mkvVideo{PixelWidth: 320, PixelHeight: 240,
			DisplayWidth: 16, DisplayHeight: 9, DisplayUnit: 3}, 4, 3},
		"centimetres with no size": {mkvVideo{PixelWidth: 320, PixelHeight: 240, DisplayUnit: 1}, 0, 0},
		"an unknown unit": {mkvVideo{PixelWidth: 320, PixelHeight: 240,
			DisplayWidth: 427, DisplayHeight: 240, DisplayUnit: 4}, 0, 0},
		// The display size is of the frame left after cropping.
		"a crop": {mkvVideo{PixelWidth: 330, PixelHeight: 250, PixelCropLeft: 4, PixelCropRight: 6,
			PixelCropTop: 2, PixelCropBottom: 8, DisplayWidth: 640, DisplayHeight: 240}, 2, 1},
		"a crop that leaves no column": {mkvVideo{PixelWidth: 320, PixelHeight: 240, PixelCropLeft: 320}, 0, 0},
		"a crop that leaves no row":    {mkvVideo{PixelWidth: 320, PixelHeight: 240, PixelCropTop: 240}, 0, 0},
		"no pixel size":                {mkvVideo{}, 0, 0},
		"a product past 64 bits": {mkvVideo{PixelWidth: 320, PixelHeight: 240,
			DisplayWidth: 1 << 62, DisplayHeight: 1}, 0, 0},
		"a side past 31 bits": {mkvVideo{PixelWidth: 3, PixelHeight: 1,
			DisplayWidth: 1 << 40, DisplayHeight: 1}, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if h, v := mkvAspect(&tc.video); h != tc.h || v != tc.v {
				t.Errorf("mkvAspect = %d:%d, want %d:%d", h, v, tc.h, tc.v)
			}
		})
	}
}

// End to end, Matroska to MP4: a WebM VP9 track that states nothing about its
// pixels has square ones, and the remux says so in a pasp box -- the case L10
// measured with ffprobe, 0/1 before and 1/1 now.
func TestARemuxOfWebMVP9WritesSquarePixels(t *testing.T) {
	src, err := NewReader(vp9WebM(t))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Remux(&out, src); err != nil {
		t.Fatal(err)
	}
	pasp := sampleEntryOf(t, out.Bytes()).VpXX.Pasp
	if pasp == nil || pasp.HSpacing != 1 || pasp.VSpacing != 1 {
		t.Errorf("pasp = %+v, want 1:1", pasp)
	}
}

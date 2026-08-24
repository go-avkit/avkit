// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// jpegFrame encodes one picture the way a caller building on the standard
// library would: a complete JPEG image per sample, no shared tables.
func jpegFrame(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A gradient that moves with the seed, so one frame differs from
			// the next in a way a comparison can see.
			img.Set(x, y, color.RGBA{
				R: uint8((x + seed*8) % 256),
				G: uint8((y + seed*4) % 256),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode a frame: %v", err)
	}
	return buf.Bytes()
}

// TestMuxerWritesMotionJPEG is the whole point of the entry: pictures encoded
// by the standard library, written as a film, and read back byte for byte.
// Nothing here needs a codec this package does not have.
func TestMuxerWritesMotionJPEG(t *testing.T) {
	const w, h, frames = 64, 48, 5
	want := make([][]byte, 0, frames)
	for i := 0; i < frames; i++ {
		want = append(want, jpegFrame(t, w, h, i))
	}
	for _, tc := range []struct {
		name        string
		progressive bool
	}{
		{"fragmented", false},
		{"progressive", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var m sampleWriter
			if tc.progressive {
				m = NewProgressiveMuxer(&out)
			} else {
				m = NewMuxer(&out)
			}
			cfg := TrackConfig{Kind: Video, Codec: "mjpg", Timescale: 30000,
				Width: w, Height: h}
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatalf("AddTrack: %v", err)
			}
			for i, frame := range want {
				// Every JPEG is a picture in its own right, so every sample
				// is one a player can start at.
				if err := m.WriteSample(id, Sample{Data: frame, Duration: 1000, Sync: true}); err != nil {
					t.Fatalf("WriteSample %d: %v", i, err)
				}
			}
			if err := m.(interface{ Close() error }).Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			stsd := sampleEntryOf(t, out.Bytes())
			if stsd.Mjpg == nil {
				t.Fatalf("no mjpg sample entry: %v", stsd.Children)
			}
			if stsd.Mjpg.Width != w || stsd.Mjpg.Height != h {
				t.Fatalf("frame size = %dx%d", stsd.Mjpg.Width, stsd.Mjpg.Height)
			}
			// Whole images, so no shared prefix is stated.
			if stsd.Mjpg.JpgC != nil {
				t.Errorf("a jpgC prefix was written for complete images")
			}

			r, err := NewReader(out.Bytes())
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			cfgBack, err := r.TrackConfig(r.TrackIDs()[0])
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if cfgBack.Codec != "mjpg" || cfgBack.Width != w || cfgBack.Height != h {
				t.Fatalf("read back %+v", cfgBack)
			}
			got, err := r.Samples(r.TrackIDs()[0])
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(got) != frames {
				t.Fatalf("read %d frames, want %d", len(got), frames)
			}
			for i := range got {
				if !bytes.Equal(got[i].Data, want[i]) {
					t.Fatalf("frame %d came back changed", i)
				}
				// And what came back is still a picture the standard library
				// reads, at the size it was made.
				img, err := jpeg.Decode(bytes.NewReader(got[i].Data))
				if err != nil {
					t.Fatalf("frame %d does not decode: %v", i, err)
				}
				if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
					t.Fatalf("frame %d decodes to %dx%d", i, b.Dx(), b.Dy())
				}
			}
		})
	}
}

// TestMuxerWritesMotionJPEGWithSharedTables covers the other shape: samples
// that are not complete images, with the tables they share stated once.
func TestMuxerWritesMotionJPEGWithSharedTables(t *testing.T) {
	prefix := []byte{0xFF, 0xD8, 0xFF, 0xDB, 0x00, 0x43, 0x00}
	var out bytes.Buffer
	m := NewMuxer(&out)
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "mjpg", Timescale: 30000,
		Width: 32, Height: 32, CodecConfig: prefix})
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{0x01, 0x02, 0x03}, Duration: 1000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	stsd := sampleEntryOf(t, out.Bytes())
	if stsd.Mjpg == nil || stsd.Mjpg.JpgC == nil {
		t.Fatalf("the shared tables were not written: %+v", stsd.Mjpg)
	}
	if !bytes.Equal(stsd.Mjpg.JpgC.JpegPrefix, prefix) {
		t.Fatalf("prefix = % x, want % x", stsd.Mjpg.JpgC.JpegPrefix, prefix)
	}
}

func TestMuxerRefusesMotionJPEGItCannotDescribe(t *testing.T) {
	cases := []struct {
		name string
		cfg  TrackConfig
	}{
		{"no frame size", TrackConfig{Codec: "mjpg", Timescale: 30000}},
		{"no height", TrackConfig{Codec: "mjpg", Timescale: 30000, Width: 64}},
		{"a frame wider than a sample entry can state",
			TrackConfig{Codec: "mjpg", Timescale: 30000, Width: 70000, Height: 48}},
		{"a frame taller than a sample entry can state",
			TrackConfig{Codec: "mjpg", Timescale: 30000, Width: 64, Height: 70000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := NewMuxer(&buf).AddTrack(tc.cfg); !errors.Is(err, ErrTrackConfig) {
				t.Fatalf("err = %v, want ErrTrackConfig", err)
			}
			if buf.Len() != 0 {
				t.Errorf("%d bytes were written for a track that was refused", buf.Len())
			}
		})
	}
}

// TestMotionJPEGReportsAnEntryItCannotBuild stages the library refusing the
// entry, which the values checked above should already have prevented.
func TestMotionJPEGReportsAnEntryItCannotBuild(t *testing.T) {
	original := setMJpegEntry
	defer func() { setMJpegEntry = original }()
	setMJpegEntry = func(*mp4.TrakBox, uint16, uint16, []byte) error {
		return errors.New("staged refusal")
	}
	var buf bytes.Buffer
	_, err := NewMuxer(&buf).AddTrack(TrackConfig{Kind: Video, Codec: "mjpg",
		Timescale: 30000, Width: 64, Height: 48})
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "staged refusal") {
		t.Fatalf("err = %v, want the staged refusal reported", err)
	}
}

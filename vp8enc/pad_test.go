// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package vp8enc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strconv"
	"testing"
	"time"

	"github.com/go-avkit/avkit/vp8enc/internal/uiseq"
	"github.com/thesyncim/govpx"
)

// gradient is the content of thesyncim/govpx#143: a textured gradient that
// moves from one frame to the next.
func gradient(i, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := uint32((x+3*i)*7+(y+i)*13) * 2654435761 >> 28 // 0..15 of texture
			img.SetRGBA(x, y, color.RGBA{
				R: byte((x+3*i)*255/(w+1)) + byte(t),
				G: byte((y+i)*255/(h+1)) + byte(t),
				B: byte(128 + (x+y)/2 + i),
				A: 255,
			})
		}
	}
	return img
}

// roundTrip encodes n frames of gen and returns the mean PSNR of the decoded
// frames against the 4:2:0 source.
func roundTrip(t *testing.T, o Options, n int, gen func(i, w, h int) *image.RGBA) float64 {
	t.Helper()
	var buf bytes.Buffer
	enc, err := New(&buf, o)
	if err != nil {
		t.Fatal(err)
	}
	var frames []*image.RGBA
	for i := 0; i < n; i++ {
		f := gen(i, o.Width, o.Height)
		frames = append(frames, f)
		if err := enc.AddFrame(f, time.Duration(i)*time.Second/time.Duration(o.FPS)); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	ss, _ := samples(t, buf.Bytes())
	dec, err := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var sum float64
	k := 0
	for _, s := range ss {
		if err := dec.Decode(s.Data); err != nil {
			t.Fatalf("%dx%d: %v", o.Width, o.Height, err)
		}
		img, ok := dec.NextFrame()
		if !ok {
			t.Fatalf("%dx%d: no picture", o.Width, o.Height)
		}
		if img.Width != o.Width || img.Height != o.Height {
			t.Fatalf("decoded %dx%d, want %dx%d", img.Width, img.Height, o.Width, o.Height)
		}
		sum += psnr(fromGovpx(img), ToYCbCr(frames[k]))
		k++
	}
	if k == 0 {
		t.Fatalf("%dx%d: no coded frame", o.Width, o.Height)
	}
	return sum / float64(k)
}

// TestNonAlignedSizes is thesyncim/govpx#143: sizes that are no multiple of
// 16 crashed the encoder. It runs the issue's content (realtime, CpuUsed 16,
// 120 frames) and the UI sequence, and asserts the PSNR.
func TestNonAlignedSizes(t *testing.T) {
	sizes := [][2]int{{320, 192}, {1280, 720}, {320, 180}, {322, 182}, {640, 360}, {1366, 768}}
	for _, s := range sizes {
		w, h := s[0], s[1]
		t.Run(fmt.Sprintf("gradient/%dx%d", w, h), func(t *testing.T) {
			if testing.Short() && w*h > 1<<17 {
				t.Skip("short")
			}
			o := Options{Width: w, Height: h, FPS: 30, BitrateKbps: 4000, Realtime: true, CPUUsed: 16}
			m := roundTrip(t, o, 120, gradient)
			t.Logf("mean PSNR %.2f dB", m)
			if m < bound(minGradientPSNR) {
				t.Fatalf("mean PSNR %.2f dB below %.1f", m, bound(minGradientPSNR))
			}
		})
		t.Run(fmt.Sprintf("ui/%dx%d", w, h), func(t *testing.T) {
			if testing.Short() && w*h > 1<<17 {
				t.Skip("short")
			}
			o := Options{Width: w, Height: h, FPS: 30, BitrateKbps: 1500, ScreenContent: true, CPUUsed: 5}
			m := roundTrip(t, o, 30, uiseq.Frame)
			t.Logf("mean PSNR %.2f dB", m)
			if m < bound(minUIPSNR) {
				t.Fatalf("mean PSNR %.2f dB below %.1f", m, bound(minUIPSNR))
			}
		})
	}
}

// On 32-bit targets govpx v0.5.2 codes the same content at 21 to 31 dB, at
// aligned sizes as well (measured on linux/386 at 320x192 and 1280x720), so
// the bound there only says that the picture is recognisable.
const (
	minGradientPSNR = 30.0
	minUIPSNR       = 30.0
)

// bound halves a PSNR bound on 32-bit targets (no branch: the repository
// gates on 100% coverage on every platform).
func bound(v float64) float64 {
	return v * [2]float64{1, 0.5}[64/strconv.IntSize-1]
}

// TestDirectYCbCr feeds a 4:2:0 image straight to an encoder whose size needs
// padding, which copies the caller's planes into the padded scratch.
func TestDirectYCbCr(t *testing.T) {
	const w, h = 33, 17
	o := Options{Width: w, Height: h, FPS: 30, BitrateKbps: 500, CPUUsed: 5}
	var buf bytes.Buffer
	enc, err := New(&buf, o)
	if err != nil {
		t.Fatal(err)
	}
	var src []*image.YCbCr
	for i := 0; i < 4; i++ {
		yc := ToYCbCr(gradient(i, w, h))
		src = append(src, yc)
		if err := enc.AddFrame(yc, time.Duration(i)*33*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	ss, _ := samples(t, buf.Bytes())
	dec, err := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i, s := range ss {
		if err := dec.Decode(s.Data); err != nil {
			t.Fatal(err)
		}
		img, _ := dec.NextFrame()
		if p := psnr(fromGovpx(img), src[i]); p < bound(30) {
			t.Fatalf("frame %d: PSNR %.2f dB", i, p)
		}
	}
}

// TestSweepSizes encodes every size from 1x1 to 40x40 (both sides) with the
// presets that took different code paths in govpx, and checks that the output
// decodes at the visible size. Before the padding, most sizes with a side
// under 16 panicked.
func TestSweepSizes(t *testing.T) {
	for _, p := range []struct {
		cpu int
		rt  bool
	}{{1, false}, {5, false}, {16, true}} {
		for w := 1; w <= 40; w++ {
			for h := 1; h <= 40; h++ {
				o := Options{Width: w, Height: h, FPS: 30, BitrateKbps: 500, CPUUsed: p.cpu, Realtime: p.rt}
				var buf bytes.Buffer
				enc, err := New(&buf, o)
				if err != nil {
					t.Fatalf("%+v: %v", o, err)
				}
				for i := 0; i < 3; i++ {
					if err := enc.AddFrame(gradient(i, w, h), time.Duration(i)*33*time.Millisecond); err != nil {
						t.Fatalf("%+v frame %d: %v", o, i, err)
					}
				}
				if err := enc.Close(); err != nil {
					t.Fatalf("%+v: %v", o, err)
				}
				ss, cfg := samples(t, buf.Bytes())
				if len(ss) == 0 || int(cfg.Width) != w || int(cfg.Height) != h {
					t.Fatalf("%+v: %d samples, track %dx%d", o, len(ss), cfg.Width, cfg.Height)
				}
			}
		}
	}
}

// TestPadEdges pins the padding: the last visible column and row are
// replicated, in every plane, up to whole macroblocks.
func TestPadEdges(t *testing.T) {
	m := newScratch(3, 2)
	if len(m.Y) != 256 || m.YStride != 16 || len(m.Cb) != 64 || m.CStride != 8 {
		t.Fatalf("scratch: Y %d stride %d, Cb %d stride %d", len(m.Y), m.YStride, len(m.Cb), m.CStride)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			m.Y[y*16+x] = byte(10*y + x + 1)
		}
	}
	m.Cb[0], m.Cb[1] = 7, 9
	padEdges(m)
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			want := byte(10*min(y, 1) + min(x, 2) + 1)
			if got := m.Y[y*16+x]; got != want {
				t.Fatalf("Y[%d,%d] = %d, want %d", x, y, got, want)
			}
		}
	}
	if m.Cb[7] != 9 || m.Cb[63] != 9 || m.Cb[56] != 7 {
		t.Fatalf("Cb not replicated: %v", m.Cb[:8])
	}
}

// TestNoPerFrameAllocation: the padded scratch is reused, so a size that needs
// padding allocates no more per frame than the aligned size beside it.
func TestNoPerFrameAllocation(t *testing.T) {
	allocs := func(w, h int) float64 {
		o := Options{Width: w, Height: h, FPS: 30, BitrateKbps: 500, Realtime: true, CPUUsed: 16}
		var buf bytes.Buffer
		enc, err := New(&buf, o)
		if err != nil {
			t.Fatal(err)
		}
		img := gradient(0, w, h)
		i := 0
		add := func() {
			i++
			if err := enc.AddFrame(img, time.Duration(i)*33*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
		add()
		add()
		n := testing.AllocsPerRun(20, add)
		_ = enc.Close()
		return n
	}
	padded, aligned := allocs(322, 182), allocs(336, 192)
	t.Logf("%.0f allocations per frame padded, %.0f aligned", padded, aligned)
	if padded > aligned+1 {
		t.Fatalf("padding allocates per frame: %.0f against %.0f", padded, aligned)
	}
}

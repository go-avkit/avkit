// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package vp8enc

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-avkit/avkit/vp8enc/internal/uiseq"
	"github.com/thesyncim/govpx"
	xvp8 "golang.org/x/image/vp8"
)

// minPSNR is the bound the round trips assert: the mean over frames of the
// PSNR of the decoded frame against the 4:2:0 source it was encoded from,
// luma plus both chroma planes together, at 600 kbit/s for a 30 frame, 330x181
// UI sequence. The measured mean is 54.9 dB (keyframes 54.7 to 55.1); the bound
// leaves 10 dB, so noise cannot trip it and a broken prediction or a wrong
// colour conversion (which costs 20 dB or more) does.
const minPSNR = 45.0

const (
	tw, th = 330, 181 // odd height and a width that is no multiple of 16
	tn     = 30
)

func testOptions() Options {
	return Options{Width: tw, Height: th, FPS: 30, BitrateKbps: 600, KeyFrameInterval: 10, ScreenContent: true,
		CPUUsed: 1} // the PSNR bounds below are those of the slow presets; DefaultCPUUsed measures lower
}

// encodeSeq encodes tn UI frames at 30 fps and returns the file and the frames.
func encodeSeq(t testing.TB, o Options) ([]byte, []*image.RGBA) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := New(&buf, o)
	if err != nil {
		t.Fatal(err)
	}
	var frames []*image.RGBA
	for i := 0; i < tn; i++ {
		f := uiseq.Frame(i, o.Width, o.Height)
		frames = append(frames, f)
		if err := enc.AddFrame(f, time.Duration(i)*time.Second/30); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), frames
}

func psnr(a, b *image.YCbCr) float64 {
	var sse, n float64
	w, h := a.Rect.Dx(), a.Rect.Dy()
	cw, ch := (w+1)/2, (h+1)/2
	plane := func(pa, pb []byte, sa, sb, w, h int) {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				d := float64(pa[y*sa+x]) - float64(pb[y*sb+x])
				sse += d * d
				n++
			}
		}
	}
	plane(a.Y, b.Y, a.YStride, b.YStride, w, h)
	plane(a.Cb, b.Cb, a.CStride, b.CStride, cw, ch)
	plane(a.Cr, b.Cr, a.CStride, b.CStride, cw, ch)
	if sse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255*n/sse)
}

func fromGovpx(img govpx.Image) *image.YCbCr {
	out := image.NewYCbCr(image.Rect(0, 0, img.Width, img.Height), image.YCbCrSubsampleRatio420)
	for y := 0; y < img.Height; y++ {
		copy(out.Y[y*out.YStride:y*out.YStride+img.Width], img.Y[y*img.YStride:])
	}
	for y := 0; y < (img.Height+1)/2; y++ {
		cw := (img.Width + 1) / 2
		copy(out.Cb[y*out.CStride:y*out.CStride+cw], img.U[y*img.UStride:])
		copy(out.Cr[y*out.CStride:y*out.CStride+cw], img.V[y*img.VStride:])
	}
	return out
}

func samples(t *testing.T, file []byte) ([]container.Sample, container.TrackConfig) {
	t.Helper()
	r, err := container.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	vt := r.File().VideoTracks()
	if len(vt) != 1 {
		t.Fatalf("%d video tracks", len(vt))
	}
	ss, err := r.Samples(vt[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(vt[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return ss, cfg
}

// TestRoundTripGovpx decodes every sample of the file with govpx's decoder,
// which is what go-avkit/decode runs for VP8, and asserts the PSNR bound.
func TestRoundTripGovpx(t *testing.T) {
	file, frames := encodeSeq(t, testOptions())
	f, err := container.Demux(file)
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != "webm" || len(f.Tracks) != 1 || f.Tracks[0].Codec != "V_VP8" && f.Tracks[0].Codec != "vp08" {
		t.Fatalf("container: %+v", f)
	}
	if f.Tracks[0].Width != tw || f.Tracks[0].Height != th {
		t.Fatalf("track size %dx%d", f.Tracks[0].Width, f.Tracks[0].Height)
	}
	ss, cfg := samples(t, file)
	if cfg.Codec != "vp08" || len(ss) != tn {
		t.Fatalf("codec %q, %d samples, want vp08 and %d", cfg.Codec, len(ss), tn)
	}
	dec, err := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var sum float64
	keys := 0
	for i, s := range ss {
		if s.Sync {
			keys++
		}
		if err := dec.Decode(s.Data); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		img, ok := dec.NextFrame()
		if !ok {
			t.Fatalf("frame %d: no picture", i)
		}
		sum += psnr(fromGovpx(img), ToYCbCr(frames[i]))
	}
	mean := sum / tn
	t.Logf("mean PSNR %.2f dB over %d frames, %d keyframes", mean, tn, keys)
	if mean < minPSNR {
		t.Fatalf("mean PSNR %.2f dB below the bound %.1f", mean, minPSNR)
	}
	if keys != 3 || !ss[0].Sync {
		t.Fatalf("keyframes %d (want 3 for an interval of 10), first sync %v", keys, ss[0].Sync)
	}
}

// TestKeyframesXImage decodes each keyframe with golang.org/x/image/vp8, an
// independent decoder, and asserts the PSNR bound and agreement with govpx.
func TestKeyframesXImage(t *testing.T) {
	file, frames := encodeSeq(t, testOptions())
	ss, _ := samples(t, file)
	dec, err := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	seen := 0
	for i, s := range ss {
		if err := dec.Decode(s.Data); err != nil {
			t.Fatal(err)
		}
		if !s.Sync {
			continue
		}
		seen++
		xd := xvp8.NewDecoder()
		xd.Init(bytes.NewReader(s.Data), len(s.Data))
		hdr, err := xd.DecodeFrameHeader()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !hdr.KeyFrame || hdr.Width != tw || hdr.Height != th {
			t.Fatalf("frame %d: header %+v", i, hdr)
		}
		xi, err := xd.DecodeFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		ref := ToYCbCr(frames[i])
		p := psnr(xi, ref)
		g, _ := dec.NextFrame()
		agree := psnr(xi, fromGovpx(g))
		t.Logf("keyframe %d: PSNR %.2f dB, %.2f dB against govpx's decode", i, p, agree)
		if p < minPSNR {
			t.Errorf("keyframe %d: PSNR %.2f dB below %.1f", i, p, minPSNR)
		}
		if agree < 60 {
			t.Errorf("keyframe %d: the two decoders disagree, %.2f dB", i, agree)
		}
	}
	if seen == 0 {
		t.Fatal("no keyframe")
	}
}

// TestTimestamps: an irregular clock and a late start come out as the
// durations and the first-at-zero the container states.
func TestTimestamps(t *testing.T) {
	o := testOptions()
	o.KeyFrameInterval, o.Buffered = 0, true
	var buf bytes.Buffer
	enc, err := New(&buf, o)
	if err != nil {
		t.Fatal(err)
	}
	at := []time.Duration{5 * time.Second, 5*time.Second + 33*time.Millisecond, 5*time.Second + 100*time.Millisecond, 5*time.Second + 133*time.Millisecond}
	for i, p := range at {
		if err := enc.AddFrame(uiseq.Frame(i, tw, th), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	ss, _ := samples(t, buf.Bytes())
	if len(ss) != len(at) {
		t.Fatalf("%d samples", len(ss))
	}
	want := []uint32{33, 67, 33, 33} // the last lasts as long as the one before it
	for i, s := range ss {
		if s.Duration != want[i] {
			t.Errorf("sample %d lasts %d ms, want %d", i, s.Duration, want[i])
		}
	}
	f, _ := container.Demux(buf.Bytes())
	if d := f.DurationSeconds(); math.Abs(d-0.166) > 0.002 {
		t.Errorf("duration %.3f s, want 0.166", d)
	}
}

func TestInputKinds(t *testing.T) {
	// The same picture as RGBA, NRGBA (opaque), a generic image and YCbCr
	// converts to the same planes, within the rounding of premultiplication.
	rgba := uiseq.Frame(7, 64, 64)
	ref := ToYCbCr(rgba)
	nr := image.NewNRGBA(rgba.Rect)
	for i := 0; i < len(rgba.Pix); i++ {
		nr.Pix[i] = rgba.Pix[i]
	}
	if p := psnr(ToYCbCr(nr), ref); p < 99 {
		t.Errorf("NRGBA differs from RGBA: %.1f dB", p)
	}
	gen := image.NewGray(rgba.Rect)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			gen.SetGray(x, y, color.Gray{uint8(x * 4)})
		}
	}
	g := ToYCbCr(gen)
	if g.Y[10*g.YStride+10] != byte(((66*40+129*40+25*40+128)>>8)+16) || g.Cb[0] != 128 || g.Cr[0] != 128 {
		t.Errorf("grey converts to Y=%d Cb=%d Cr=%d", g.Y[10*g.YStride+10], g.Cb[0], g.Cr[0])
	}
	// A sub-image of a larger YCbCr is encoded in place, at an even origin.
	big := ToYCbCr(uiseq.Frame(0, 128, 128))
	sub := big.SubImage(image.Rect(16, 16, 80, 80)).(*image.YCbCr)
	var buf bytes.Buffer
	enc, err := New(&buf, Options{Width: 64, Height: 64, FPS: 30, BitrateKbps: 500})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.AddFrame(sub, 0); err != nil {
		t.Fatal(err)
	}
	// And at an odd origin through the converting path.
	if err := enc.AddFrame(big.SubImage(image.Rect(1, 1, 65, 65)), 33*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	ss, _ := samples(t, buf.Bytes())
	dec, _ := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	defer dec.Close()
	if err := dec.Decode(ss[0].Data); err != nil {
		t.Fatal(err)
	}
	img, _ := dec.NextFrame()
	want := image.NewYCbCr(image.Rect(0, 0, 64, 64), image.YCbCrSubsampleRatio420)
	for y := 0; y < 64; y++ {
		copy(want.Y[y*want.YStride:y*want.YStride+64], sub.Y[sub.YOffset(16, 16+y):])
	}
	for y := 0; y < 32; y++ {
		copy(want.Cb[y*want.CStride:y*want.CStride+32], sub.Cb[sub.COffset(16, 16+2*y):])
		copy(want.Cr[y*want.CStride:y*want.CStride+32], sub.Cr[sub.COffset(16, 16+2*y):])
	}
	if p := psnr(fromGovpx(img), want); p < 35 {
		t.Errorf("sub-image frame decodes at %.1f dB", p)
	}
}

// TestDefaultPreset pins the meaning of CPUUsed zero: the same file as the
// explicit DefaultCPUUsed, and not the one of preset 1.
func TestDefaultPreset(t *testing.T) {
	o := testOptions()
	o.CPUUsed = 0
	def, _ := encodeSeq(t, o)
	o.CPUUsed = DefaultCPUUsed
	explicit, _ := encodeSeq(t, o)
	if !bytes.Equal(def, explicit) {
		t.Error("CPUUsed 0 is not DefaultCPUUsed")
	}
	o.CPUUsed = 1
	if slow, _ := encodeSeq(t, o); bytes.Equal(def, slow) {
		t.Error("CPUUsed 1 gives the default preset's file")
	}
}

func TestQualityMode(t *testing.T) {
	o := testOptions()
	o.BitrateKbps, o.Quality = 0, 8
	file, frames := encodeSeq(t, o)
	ss, _ := samples(t, file)
	dec, _ := govpx.NewVP8Decoder(govpx.DecoderOptions{})
	defer dec.Close()
	var sum float64
	for i, s := range ss {
		if err := dec.Decode(s.Data); err != nil {
			t.Fatal(err)
		}
		img, _ := dec.NextFrame()
		sum += psnr(fromGovpx(img), ToYCbCr(frames[i]))
	}
	m := sum / tn
	t.Logf("quality 8: mean PSNR %.2f dB, %d bytes", m, len(file))
	if m < 45 {
		t.Errorf("quality 8 gives %.2f dB, want at least 45", m)
	}
}

func TestOptionsAndFrames(t *testing.T) {
	var buf bytes.Buffer
	bad := []Options{
		{},
		{Width: 64, Height: 64, FPS: 30},
		{Width: 0, Height: 64, FPS: 30, BitrateKbps: 100},
		{Width: MinDimension - 1, Height: 64, FPS: 30, BitrateKbps: 100},
		{Width: 64, Height: MinDimension - 1, FPS: 30, BitrateKbps: 100},
		{Width: MaxDimension + 1, Height: 16, FPS: 30, BitrateKbps: 100},
		{Width: MaxDimension, Height: MaxDimension, FPS: 30, BitrateKbps: 100},
		{Width: 64, Height: 64, FPS: 0, BitrateKbps: 100},
		{Width: 64, Height: 64, FPS: 30, Quality: 64},
		{Width: 64, Height: 64, FPS: 30, BitrateKbps: -1},
		{Width: 64, Height: 64, FPS: 30, BitrateKbps: 100, CPUUsed: 17},
		{Width: -1 << 31, Height: -1 << 31, FPS: 30, BitrateKbps: 100},
	}
	for i, o := range bad {
		if _, err := New(&buf, o); !errors.Is(err, ErrOptions) {
			t.Errorf("options %d: %v, want ErrOptions", i, err)
		}
	}
	enc, err := New(&buf, Options{Width: 64, Height: 64, FPS: 30, BitrateKbps: 200})
	if err != nil {
		t.Fatal(err)
	}
	f := uiseq.Frame(0, 64, 64)
	for _, c := range []struct {
		name string
		img  image.Image
		pts  time.Duration
	}{
		{"smaller", uiseq.Frame(0, 64, 64).SubImage(image.Rect(0, 0, 32, 64)), 0},
		{"shorter", uiseq.Frame(0, 64, 64).SubImage(image.Rect(0, 0, 64, 32)), 0},
		{"wider", uiseq.Frame(0, 80, 64), 0},
		{"negative time", f, -time.Second},
		{"huge time", f, 1 << 62},
	} {
		if err := enc.AddFrame(c.img, c.pts); !errors.Is(err, ErrFrame) {
			t.Errorf("%s: %v, want ErrFrame", c.name, err)
		}
	}
	if err := enc.AddFrame(f, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := enc.AddFrame(f, time.Second); !errors.Is(err, ErrFrame) {
		t.Errorf("repeated timestamp: %v", err)
	}
	if err := enc.AddFrame(f, time.Second+200*time.Microsecond); !errors.Is(err, ErrFrame) {
		t.Errorf("timestamp inside the same millisecond: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enc.AddFrame(f, 2*time.Second); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: %v", err)
	}
	if err := enc.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: %v", err)
	}
}

// TestEmpty: a file with no frame is still a valid, empty WebM.
func TestEmpty(t *testing.T) {
	var buf bytes.Buffer
	enc, err := New(&buf, Options{Width: 64, Height: 64, FPS: 30, BitrateKbps: 200})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := container.Demux(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func FuzzEncode(f *testing.F) {
	f.Add(uint8(16), uint8(16), uint8(30), uint16(100), uint8(0), uint8(3))
	f.Add(uint8(1), uint8(1), uint8(1), uint16(1), uint8(63), uint8(1))
	f.Add(uint8(33), uint8(17), uint8(255), uint16(5000), uint8(1), uint8(40))
	// Sizes that are no multiple of 16 (thesyncim/govpx#143).
	f.Add(uint8(1), uint8(200), uint8(30), uint16(300), uint8(0), uint8(5))
	f.Add(uint8(200), uint8(1), uint8(30), uint16(300), uint8(0), uint8(5))
	f.Add(uint8(15), uint8(17), uint8(30), uint16(100), uint8(0), uint8(7))
	f.Add(uint8(161), uint8(91), uint8(30), uint16(800), uint8(0), uint8(7))
	f.Add(uint8(250), uint8(181), uint8(25), uint16(800), uint8(20), uint8(7))
	f.Fuzz(func(t *testing.T, w, h, fps uint8, kbps uint16, q, n uint8) {
		o := Options{Width: int(w), Height: int(h), FPS: int(fps), BitrateKbps: int(kbps), Quality: int(q % 70)}
		var buf bytes.Buffer
		enc, err := New(&buf, o)
		if err != nil {
			if !errors.Is(err, ErrOptions) {
				t.Fatalf("%+v: %v", o, err)
			}
			return
		}
		for i := 0; i < int(n%8); i++ {
			img := image.NewRGBA(image.Rect(0, 0, o.Width, o.Height))
			for j := range img.Pix {
				img.Pix[j] = byte(j*int(n) + i)
			}
			if err := enc.AddFrame(img, time.Duration(i)*time.Second/time.Duration(o.FPS)+time.Duration(i)*time.Millisecond); err != nil {
				t.Fatalf("%+v frame %d: %v", o, i, err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if _, err := container.Demux(buf.Bytes()); err != nil {
			t.Fatalf("%+v: output does not demux: %v", o, err)
		}
	})
}

// TestPrimaries pins the colour matrix: BT.601, studio swing, for the colours
// whose values every reference lists.
func TestPrimaries(t *testing.T) {
	for _, c := range []struct {
		name    string
		c       color.RGBA
		y, u, v byte
	}{
		{"black", color.RGBA{0, 0, 0, 255}, 16, 128, 128},
		{"white", color.RGBA{255, 255, 255, 255}, 235, 128, 128},
		{"red", color.RGBA{255, 0, 0, 255}, 82, 90, 240},
		{"green", color.RGBA{0, 255, 0, 255}, 144, 54, 34},
		{"blue", color.RGBA{0, 0, 255, 255}, 41, 240, 110},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 3, 3)) // odd: the edge block averages what it has
		for i := 0; i < 9; i++ {
			img.SetRGBA(i%3, i/3, c.c)
		}
		yc := ToYCbCr(img)
		if yc.Y[0] != c.y || yc.Y[2*yc.YStride+2] != c.y || yc.Cb[3] != c.u || yc.Cr[3] != c.v {
			t.Errorf("%s: Y %d Cb %d Cr %d, want %d %d %d", c.name, yc.Y[0], yc.Cb[3], yc.Cr[3], c.y, c.u, c.v)
		}
	}
}

type fakeEnc struct {
	res govpx.EncodeResult
	err error
}

func (f fakeEnc) EncodeInto([]byte, govpx.Image, uint64, uint64, govpx.EncodeFlags) (govpx.EncodeResult, error) {
	return f.res, f.err
}
func (fakeEnc) Close() error { return nil }

// TestDroppedFrame: a frame the encoder drops lengthens the one before it.
func TestDroppedFrame(t *testing.T) {
	o := testOptions()
	o.Buffered = true
	var buf bytes.Buffer
	enc, err := New(&buf, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.AddFrame(uiseq.Frame(0, tw, th), 0); err != nil {
		t.Fatal(err)
	}
	real := enc.enc
	enc.enc = fakeEnc{} // no data, no error: rate control dropped it
	if err := enc.AddFrame(uiseq.Frame(1, tw, th), 33*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	enc.enc = real
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	ss, _ := samples(t, buf.Bytes())
	if len(ss) != 1 || ss[0].Duration != 66 {
		t.Fatalf("%d samples, first lasts %v ms; want one of 66", len(ss), ss[0].Duration)
	}
}

func TestEncoderFailures(t *testing.T) {
	boom := errors.New("boom")
	var buf bytes.Buffer
	enc, err := New(&buf, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	enc.enc = fakeEnc{err: boom}
	if err := enc.AddFrame(uiseq.Frame(0, tw, th), 0); !errors.Is(err, boom) {
		t.Errorf("encoder failure: %v", err)
	}

	save, saveTrack := newVP8, addTrack
	defer func() { newVP8, addTrack = save, saveTrack }()
	newVP8 = func(govpx.EncoderOptions) (frameEncoder, error) { return nil, boom }
	if _, err := New(&buf, testOptions()); !errors.Is(err, ErrOptions) {
		t.Errorf("govpx refusing the options: %v", err)
	}
	newVP8 = save
	addTrack = func(*container.WebMMuxer, container.TrackConfig) (uint32, error) { return 0, boom }
	if _, err := New(&buf, testOptions()); !errors.Is(err, boom) {
		t.Errorf("muxer refusing the track: %v", err)
	}
}

// TestGapTooLong: a sample's duration is 32 bits of milliseconds, and a gap
// past that is refused rather than wrapped.
func TestGapTooLong(t *testing.T) {
	var buf bytes.Buffer
	enc, err := New(&buf, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	f := uiseq.Frame(0, tw, th)
	if err := enc.AddFrame(f, 0); err != nil {
		t.Fatal(err)
	}
	if err := enc.AddFrame(f, 60*24*time.Hour); !errors.Is(err, ErrFrame) {
		t.Fatalf("60 day gap: %v", err)
	}
}

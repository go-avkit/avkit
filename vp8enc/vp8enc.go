// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package vp8enc writes WebM video in pure Go: frames in, a VP8 track in a
// WebM file out, with no cgo and no libvpx.
//
// The VP8 bitstream (RFC 6386) comes from the encoder of
// github.com/thesyncim/govpx; the file is written by
// [container.WebMMuxer], so the V_VP8 track, its timestamps, its keyframe flags
// and its Cues are the muxer's. This package is the glue between the two: it
// converts frames to the planar 4:2:0 the encoder takes, keeps the timestamps,
// and hands each coded frame to the muxer with the duration its successor's
// timestamp gives it.
//
//	enc, err := vp8enc.New(f, vp8enc.Options{Width: 1280, Height: 720, FPS: 30, BitrateKbps: 600})
//	for i, img := range frames {
//		err = enc.AddFrame(img, time.Duration(i)*time.Second/30)
//	}
//	err = enc.Close()
//
// A rendered user interface changes little from one frame to the next, which
// is the case the encoder's inter and skip modes are for: Options.ScreenContent
// turns on its screen-content tuning.
package vp8enc

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/thesyncim/govpx"
)

// Limits on what the encoder is asked to hold. VP8 states a frame size in 14
// bits (RFC 6386 §9.1), and a frame is held three times over (the converted
// source, the encoder's references, the output buffer), so the area is bounded
// as well, in a unit that cannot overflow a 32-bit int.
const (
	// MaxDimension is the largest width or height VP8 can state.
	MaxDimension = 16383
	// MaxArea is the largest width times height accepted, 8192 x 8192.
	MaxArea = 1 << 26
)

// timescale is the unit of the track: one millisecond, the WebM default tick.
const timescale = 1000

// Errors reported by the encoder.
var (
	// ErrOptions means the options cannot configure an encoder.
	ErrOptions = errors.New("vp8enc: invalid options")
	// ErrFrame means a frame cannot be encoded as given: a size other than the
	// one the encoder was made for, or a timestamp that does not advance.
	ErrFrame = errors.New("vp8enc: invalid frame")
	// ErrClosed means the encoder was used after Close.
	ErrClosed = errors.New("vp8enc: encoder is closed")
)

// Options configure an Encoder.
type Options struct {
	// Width and Height are the frame size in pixels. Both are required, at
	// most MaxDimension, and their product at most MaxArea.
	Width, Height int
	// FPS is the nominal frame rate, which sizes the last frame's duration and
	// the first frame's budget. Required.
	FPS int
	// BitrateKbps is the target bitrate in kbit/s, for the default
	// variable-bitrate mode. Required unless Quality is set.
	BitrateKbps int
	// Quality, when non-zero, selects constant quality instead of a bitrate:
	// 1 (best) to 63 (worst) is the quantizer the encoder holds to.
	Quality int
	// KeyFrameInterval is the longest run of frames between keyframes, and
	// the granularity of seeking. Zero means 120 frames.
	KeyFrameInterval int
	// CPUUsed trades speed for compression, 0 (slowest, smallest) to 16
	// (fastest). Zero is the encoder's default for the good-quality deadline.
	CPUUsed int
	// ScreenContent turns on the encoder's screen-content mode, for
	// rendered user interfaces and slides.
	ScreenContent bool
	// Realtime selects the encoder's realtime deadline instead of its
	// good-quality one: faster, and larger for the same quality.
	Realtime bool
	// Buffered holds the whole file in memory until Close, so that a writer
	// that cannot seek still gets the duration and every size written in the
	// file. A writer that can seek, such as an *os.File, needs no help.
	Buffered bool
}

// Encoder encodes frames to VP8 and writes them to a WebM file.
//
// It is not safe for concurrent use.
type Encoder struct {
	mux   *container.WebMMuxer
	track uint32
	enc   frameEncoder
	opts  Options

	buf             *image.YCbCr // conversion scratch, reused
	out             []byte
	started, closed bool

	// held is the last coded frame: a sample's duration is the distance to the
	// next timestamp, so it is written when the next frame arrives, or at
	// Close.
	held    []byte
	heldKey bool
	heldAt  int64 // milliseconds
	lastAt  int64
	lastDur int64
}

// frameEncoder is the part of govpx's encoder this package uses. It, newVP8 and
// addTrack are seams: the refusals behind them cannot be reached with options
// this package has already checked, and a test has to be able to reach them.
type frameEncoder interface {
	EncodeInto(dst []byte, src govpx.Image, pts, duration uint64, flags govpx.EncodeFlags) (govpx.EncodeResult, error)
	Close() error
}

var newVP8 = func(o govpx.EncoderOptions) (frameEncoder, error) {
	e, err := govpx.NewVP8Encoder(o)
	if err != nil {
		return nil, err
	}
	return e, nil
}

var addTrack = (*container.WebMMuxer).AddTrack

// New starts a WebM file on w. A w that is an [io.Seeker], such as an
// *os.File, gets the duration and the segment size written at Close.
func New(w io.Writer, o Options) (*Encoder, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	enc, err := newVP8(o.govpx())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOptions, err)
	}
	var mopts []container.WebMOption
	if o.Buffered {
		mopts = append(mopts, container.BufferedSegment())
	}
	mux := container.NewWebMMuxer(w, mopts...)
	id, err := addTrack(mux, container.TrackConfig{
		Kind: container.Video, Codec: "vp08", Timescale: timescale,
		Width: o.Width, Height: o.Height,
	})
	if err != nil {
		_ = enc.Close()
		return nil, err
	}
	e := &Encoder{
		mux: mux, track: id, enc: enc, opts: o,
		buf: image.NewYCbCr(image.Rect(0, 0, o.Width, o.Height), image.YCbCrSubsampleRatio420),
		out: make([]byte, 2*o.Width*o.Height+64<<10),
	}
	return e, nil
}

func (o Options) validate() error {
	switch {
	case o.Width <= 0 || o.Height <= 0 || o.Width > MaxDimension || o.Height > MaxDimension:
		return fmt.Errorf("%w: size %dx%d outside 1..%d", ErrOptions, o.Width, o.Height, MaxDimension)
	case int64(o.Width)*int64(o.Height) > MaxArea:
		return fmt.Errorf("%w: %dx%d exceeds %d pixels", ErrOptions, o.Width, o.Height, MaxArea)
	case o.FPS <= 0 || o.FPS > timescale:
		return fmt.Errorf("%w: FPS %d outside 1..%d", ErrOptions, o.FPS, timescale)
	case o.Quality < 0 || o.Quality > 63:
		return fmt.Errorf("%w: Quality %d outside 0..63", ErrOptions, o.Quality)
	case o.Quality == 0 && o.BitrateKbps <= 0:
		return fmt.Errorf("%w: set BitrateKbps or Quality", ErrOptions)
	case o.BitrateKbps < 0 || o.KeyFrameInterval < 0 || o.CPUUsed < 0 || o.CPUUsed > 16:
		return fmt.Errorf("%w: negative or out-of-range value", ErrOptions)
	}
	return nil
}

func (o Options) govpx() govpx.EncoderOptions {
	g := govpx.EncoderOptions{
		Width: o.Width, Height: o.Height,
		TimebaseNum: 1, TimebaseDen: timescale,
		TargetBitrateKbps: o.BitrateKbps,
		KeyFrameInterval:  o.KeyFrameInterval,
		CpuUsed:           o.CPUUsed,
		Deadline:          govpx.DeadlineGoodQuality,
	}
	if o.Realtime {
		g.Deadline = govpx.DeadlineRealtime
	}
	if o.ScreenContent {
		g.ScreenContentMode = 1
	}
	if o.Quality > 0 {
		g.RateControlMode = govpx.RateControlQ
		g.CQLevel = o.Quality
		g.MinQuantizer, g.MaxQuantizer, g.QuantizerRangeSet = o.Quality, o.Quality, true
		if g.TargetBitrateKbps == 0 {
			g.TargetBitrateKbps = 1
		}
	}
	return g
}

// AddFrame encodes img, shown at pts from the start of the file. Timestamps
// must increase strictly; the first may be anything non-negative and is shown
// at zero. img must be Options.Width by Options.Height. An *image.YCbCr in
// 4:2:0 is encoded as it stands; any other image is converted from RGB with the
// BT.601 matrix, alpha ignored (a premultiplied image is shown over black).
//
// The encoder may drop a frame to hold its bitrate; the previous frame then
// stays on screen for the extra time, as the timestamps say.
func (e *Encoder) AddFrame(img image.Image, pts time.Duration) error {
	if e.closed {
		return ErrClosed
	}
	b := img.Bounds()
	if b.Dx() != e.opts.Width || b.Dy() != e.opts.Height {
		return fmt.Errorf("%w: %dx%d, encoder is %dx%d", ErrFrame, b.Dx(), b.Dy(), e.opts.Width, e.opts.Height)
	}
	if pts < 0 || pts > time.Duration(1<<40)*time.Millisecond {
		return fmt.Errorf("%w: timestamp %v", ErrFrame, pts)
	}
	at := int64((pts + time.Millisecond/2) / time.Millisecond)
	dur := int64(1000 / e.opts.FPS)
	if e.started {
		if at <= e.lastAt {
			return fmt.Errorf("%w: timestamp %v does not advance past %dms", ErrFrame, pts, e.lastAt)
		}
		dur = at - e.lastAt
	}
	src := e.planes(img)
	res, err := e.enc.EncodeInto(e.out, src, uint64(at), uint64(dur), 0)
	if err != nil {
		return fmt.Errorf("vp8enc: %w", err)
	}
	if !e.started {
		e.started, e.heldAt = true, at
	}
	e.lastAt, e.lastDur = at, dur
	if len(res.Data) == 0 { // dropped by rate control: the held frame lasts longer
		return nil
	}
	if e.held != nil {
		if err := e.flush(at); err != nil {
			return err
		}
	}
	// A fresh copy each time: the muxer may keep a sample's bytes until a later
	// call, and the encoder's buffer is reused by the next frame.
	e.held = bytes.Clone(res.Data)
	e.heldKey, e.heldAt = res.KeyFrame, at
	return nil
}

// flush writes the held frame, which lasts until `until` milliseconds.
func (e *Encoder) flush(until int64) error {
	// d is at least one: timestamps advance, and the last frame lasts as long
	// as one before it or a nominal frame, never less than a millisecond.
	d := until - e.heldAt
	if d > math.MaxUint32 {
		return fmt.Errorf("%w: duration %dms", ErrFrame, d)
	}
	return e.mux.WriteSample(e.track, container.Sample{
		Data: e.held, Duration: uint32(d), Sync: e.heldKey,
	})
}

// Close writes the last frame, the Cues and, for a seekable writer, the
// duration, and releases the encoder. It does not close the writer.
func (e *Encoder) Close() error {
	if e.closed {
		return ErrClosed
	}
	e.closed = true
	var err error
	if e.held != nil {
		err = e.flush(e.lastAt + e.lastDur)
	}
	if cerr := e.enc.Close(); err == nil {
		err = cerr
	}
	if cerr := e.mux.Close(); err == nil {
		err = cerr
	}
	return err
}

// planes returns img as the encoder's I420 image.
func (e *Encoder) planes(img image.Image) govpx.Image {
	yc, ok := img.(*image.YCbCr)
	if !ok || yc.SubsampleRatio != image.YCbCrSubsampleRatio420 ||
		yc.Rect.Min.X%2 != 0 || yc.Rect.Min.Y%2 != 0 {
		yc = e.buf
		convert(yc, img)
	}
	x0, y0 := yc.Rect.Min.X, yc.Rect.Min.Y
	return govpx.Image{
		Width: e.opts.Width, Height: e.opts.Height,
		Y: yc.Y[yc.YOffset(x0, y0):], U: yc.Cb[yc.COffset(x0, y0):], V: yc.Cr[yc.COffset(x0, y0):],
		YStride: yc.YStride, UStride: yc.CStride, VStride: yc.CStride,
	}
}

// ToYCbCr converts img to 4:2:0 as the encoder does: BT.601, studio swing,
// chroma averaged over each 2x2 block, alpha ignored. It is the reference a
// decoded frame is compared against.
func ToYCbCr(img image.Image) *image.YCbCr {
	b := img.Bounds()
	dst := image.NewYCbCr(image.Rect(0, 0, b.Dx(), b.Dy()), image.YCbCrSubsampleRatio420)
	convert(dst, img)
	return dst
}

// convert fills dst (4:2:0, origin at zero, the size of img) from img.
func convert(dst *image.YCbCr, img image.Image) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cw, ch := (w+1)/2, (h+1)/2
	rgb := rgbReader(img)
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			var sr, sg, sb, n int
			for dy := 0; dy < 2; dy++ {
				y := 2*cy + dy
				if y >= h {
					continue
				}
				for dx := 0; dx < 2; dx++ {
					x := 2*cx + dx
					if x >= w {
						continue
					}
					r, g, bl := rgb(b.Min.X+x, b.Min.Y+y)
					dst.Y[y*dst.YStride+x] = byte(((66*r + 129*g + 25*bl + 128) >> 8) + 16) // 16..235, 16..240 for Cb, Cr: no clamp
					sr, sg, sb, n = sr+r, sg+g, sb+bl, n+1
				}
			}
			r, g, bl := (sr+n/2)/n, (sg+n/2)/n, (sb+n/2)/n
			dst.Cb[cy*dst.CStride+cx] = byte(((-38*r - 74*g + 112*bl + 128) >> 8) + 128)
			dst.Cr[cy*dst.CStride+cx] = byte(((112*r - 94*g - 18*bl + 128) >> 8) + 128)
		}
	}
}

// rgbReader returns a function reading 8-bit RGB at absolute coordinates, with
// a direct path for the two common in-memory formats.
func rgbReader(img image.Image) func(x, y int) (r, g, b int) {
	switch m := img.(type) {
	case *image.RGBA:
		return func(x, y int) (int, int, int) {
			i := m.PixOffset(x, y)
			return int(m.Pix[i]), int(m.Pix[i+1]), int(m.Pix[i+2])
		}
	case *image.NRGBA:
		return func(x, y int) (int, int, int) {
			i := m.PixOffset(x, y)
			a := int(m.Pix[i+3])
			return (int(m.Pix[i])*a + 127) / 255, (int(m.Pix[i+1])*a + 127) / 255, (int(m.Pix[i+2])*a + 127) / 255
		}
	}
	return func(x, y int) (int, int, int) {
		c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
		return int(c.R), int(c.G), int(c.B)
	}
}

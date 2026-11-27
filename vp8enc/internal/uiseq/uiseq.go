// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package uiseq renders a deterministic, UI-like frame sequence: a dark
// window with a sidebar, a header, a log panel that scrolls a line every 15
// frames, a progress bar that grows, a list whose highlight moves, and a
// cursor that blinks. Most pixels do not change from one frame to the next,
// which is what a screen recording of an application looks like, and the
// text-like detail is sharp, which is what makes it hard on a lossy codec.
package uiseq

import (
	"image"
	"image/color"
)

// Frame renders frame i at w x h (both at least 64).
func Frame(i, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(img, img.Rect, color.RGBA{0x1e, 0x1f, 0x22, 255})

	side := w / 5
	fill(img, image.Rect(0, 0, side, h), color.RGBA{0x2b, 0x2d, 0x31, 255})
	fill(img, image.Rect(side, 0, w, 40), color.RGBA{0x31, 0x33, 0x38, 255})
	text(img, image.Rect(side+16, 12, side+16+200, 28), 7001, color.RGBA{0xdd, 0xdd, 0xdd, 255})

	// Sidebar list: a highlight that moves every 10 frames.
	const rows = 12
	rh := (h - 60) / rows
	sel := (i / 10) % rows
	for r := 0; r < rows; r++ {
		y := 50 + r*rh
		if r == sel {
			fill(img, image.Rect(4, y, side-4, y+rh-4), color.RGBA{0x3d, 0x6a, 0xb8, 255})
		}
		text(img, image.Rect(14, y+4, side-14, y+rh-8), 100+r, color.RGBA{0xc8, 0xc8, 0xc8, 255})
	}

	// Log panel: lines scroll up one text row every 15 frames.
	px0, py0 := side+16, 56
	lh := 18
	lines := (h - py0 - 60) / lh
	base := i / 15
	for l := 0; l < lines; l++ {
		text(img, image.Rect(px0, py0+l*lh, px0+(w-side)*3/4+(base+l)%5*20, py0+l*lh+lh-6),
			1000+base+l, color.RGBA{0xa9, 0xb7, 0xc6, 255})
	}

	// Progress bar: grows 1 px per frame, wrapping.
	by := h - 40
	fill(img, image.Rect(px0, by, w-16, by+14), color.RGBA{0x3a, 0x3c, 0x40, 255})
	pw := (w - 16 - px0) * (i % 300) / 300
	fill(img, image.Rect(px0, by, px0+pw, by+14), color.RGBA{0x4c, 0xaf, 0x50, 255})

	// Cursor: blinks every 15 frames, at the end of the last log line.
	if (i/15)%2 == 0 {
		fill(img, image.Rect(px0+(w-side)*3/4+24, py0+(lines-1)*lh, px0+(w-side)*3/4+32, py0+(lines-1)*lh+lh-6),
			color.RGBA{0xff, 0xff, 0xff, 255})
	}
	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		o := img.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = c.R, c.G, c.B, 255
			o += 4
		}
	}
}

// text draws glyph-like 6x10 cells with 1-pixel gaps in r, the pattern of each
// cell a pure function of (seed, position), two of every five cells left blank
// as spaces, and an anti-alias-like half tone on a cell's edge.
func text(img *image.RGBA, r image.Rectangle, seed int, c color.RGBA) {
	const cw = 7
	n := 0
	for x := r.Min.X; x+cw <= r.Max.X; x += cw + 1 {
		n++
		hsh := mix(uint32(seed)*2654435761 + uint32(n)*40503)
		if hsh%5 < 2 {
			continue
		}
		for gy := 0; gy < 10 && r.Min.Y+gy < r.Max.Y; gy++ {
			bits := mix(hsh + uint32(gy)*977)
			for gx := 0; gx < 6; gx++ {
				if bits>>(uint(gx)*3)&7 < 3 {
					continue
				}
				col := c
				if bits>>(uint(gx)*3)&7 == 3 {
					col = color.RGBA{c.R / 2, c.G / 2, c.B / 2, 255}
				}
				img.SetRGBA(x+gx, r.Min.Y+gy, col)
			}
		}
	}
}

func mix(v uint32) uint32 {
	v ^= v >> 16
	v *= 0x7feb352d
	v ^= v >> 15
	v *= 0x846ca68b
	v ^= v >> 16
	return v
}

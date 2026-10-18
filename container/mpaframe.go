// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import "fmt"

// MPEG audio — Layers I, II and III of ISO/IEC 11172-3, and their lower
// sampling frequencies in ISO/IEC 13818-3 — states its format in a four-byte
// header in front of every frame, and nowhere else. A transport stream carries
// those frames as they are, so reading one means finding where each frame ends,
// which only the header says.
//
// Syntax and tables are ISO/IEC 11172-3 2.4.1.3 and 2.4.2.3, and ISO/IEC
// 13818-3 2.4.2.3 for the lower rates, read against FFmpeg's
// libavcodec/mpegaudiodecheader.c and mpegaudiotabs.h (2026-10-18). Only what
// a container needs is read: this is not a decoder.
//
// ⛔ The "MPEG 2.5" rates (8, 11.025 and 12 kHz) are not in either standard:
// they are an extension that clears the twelfth bit of the sync word. A frame
// that states one is not recognised here, rather than recognised by a guess.

// mpaHeaderSize is the length of the fixed header every frame begins with.
const mpaHeaderSize = 4

// mpaRates is indexed by the ID bit (1 for ISO/IEC 11172-3, 0 for the lower
// rates of ISO/IEC 13818-3) and then by sampling_frequency. The fourth value is
// reserved.
var mpaRates = [2][4]int{
	{22050, 24000, 16000, 0},
	{44100, 48000, 32000, 0},
}

// mpaBitRates is in kbit/s, indexed by the ID bit, the layer (1 to 3, less
// one) and bitrate_index. Index 0 is the "free format", whose frame length the
// header does not state, and index 15 is forbidden; both are refused.
var mpaBitRates = [2][3][15]int{
	{ // ISO/IEC 13818-3, Table B.1 at the lower sampling frequencies
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	},
	{ // ISO/IEC 11172-3, 2.4.2.3
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	},
}

// mpaFrame is what one frame header states about the frame.
type mpaFrame struct {
	// MPEG1 is the ID bit: true for ISO/IEC 11172-3, false for the lower
	// sampling frequencies ISO/IEC 13818-3 adds.
	MPEG1      bool
	Layer      int // 1, 2 or 3
	BitRate    int // kbit/s
	SampleRate int
	Channels   int
	FrameSize  int // bytes, header included
	Samples    int // per channel, which is what a frame lasts
}

// Codec is the name a track of these frames goes by: the layer decides it, as
// it does for FFmpeg's codec names and Matroska's A_MPEG/L1, /L2 and /L3.
func (f mpaFrame) Codec() string {
	return fmt.Sprintf("mp%d", f.Layer)
}

// parseMPAFrame reads one frame header.
func parseMPAFrame(data []byte) (mpaFrame, error) {
	var f mpaFrame
	if len(data) < mpaHeaderSize {
		return f, fmt.Errorf("%w: an MPEG audio header is %d bytes, not %d",
			ErrCodecMismatch, mpaHeaderSize, len(data))
	}
	// Twelve set bits of sync word, then the ID bit.
	if data[0] != 0xFF || data[1]&0xF0 != 0xF0 {
		return f, fmt.Errorf("%w: no MPEG audio sync word", ErrCodecMismatch)
	}
	id := int(data[1]>>3) & 1
	layerBits := int(data[1]>>1) & 3
	if layerBits == 0 {
		return f, fmt.Errorf("%w: reserved layer", ErrCodecMismatch)
	}
	// The field counts down: 3 is Layer I and 1 is Layer III.
	f.Layer = 4 - layerBits
	f.MPEG1 = id == 1
	bitRateIndex := int(data[2] >> 4)
	if bitRateIndex == 0 || bitRateIndex == 15 {
		// Free format states no length, so the frame's end could only be
		// found by searching for the next one; index 15 is forbidden.
		return f, fmt.Errorf("%w: bitrate index %d", ErrCodecMismatch, bitRateIndex)
	}
	f.BitRate = mpaBitRates[id][f.Layer-1][bitRateIndex]
	f.SampleRate = mpaRates[id][data[2]>>2&3]
	if f.SampleRate == 0 {
		return f, fmt.Errorf("%w: reserved sampling frequency", ErrCodecMismatch)
	}
	padding := int(data[2]>>1) & 1
	f.Channels = 2
	if data[3]>>6 == 3 {
		f.Channels = 1 // single_channel mode
	}
	// 2.4.3.1 of ISO/IEC 11172-3: a Layer I frame is counted in four-byte
	// slots of 384 samples, the others in bytes of 1152. At the lower rates
	// a Layer III frame holds half as many samples (ISO/IEC 13818-3 2.4.3.1),
	// which halves its length too.
	switch {
	case f.Layer == 1:
		f.Samples = 384
		f.FrameSize = (12*f.BitRate*1000/f.SampleRate + padding) * 4
	case f.Layer == 3 && !f.MPEG1:
		f.Samples = 576
		f.FrameSize = 72*f.BitRate*1000/f.SampleRate + padding
	default:
		f.Samples = 1152
		f.FrameSize = 144*f.BitRate*1000/f.SampleRate + padding
	}
	return f, nil
}

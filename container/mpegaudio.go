// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"time"
)

// An MPEG audio elementary stream -- an .mp3, an .mp2 -- is its frames and
// nothing else. Each frame starts with a four-byte header that states its
// layer, its bit rate and its sampling rate, and from those its length; no
// header anywhere states the stream as a whole.
//
// The header is ISO/IEC 11172-3 §2.4.1.3 (MPEG-1) as extended to the lower
// sampling rates by ISO/IEC 13818-3 §2.4.1.3, plus the unofficial "MPEG 2.5"
// that halves them again, which every encoder writes. Tables and the frame
// length formula are read against FFmpeg's libavcodec/mpegaudiodecheader.c and
// mpegaudiotabs.h, and the demuxing against libavformat/mp3dec.c (2026-10-18).
//
// Only the header is read. The codec is not decoded here, and a free-format
// stream -- bit rate index 0, whose frames state no length -- is not taken: it
// would have to be measured by searching for the next header, and none of the
// encoders in use writes it.

// FormatMPEGAudio is an MPEG-1/2 audio elementary stream: MP3, MP2 or MP1.
const FormatMPEGAudio Format = 5

func init() {
	registerElementary(elementaryFormat{
		format: FormatMPEGAudio, name: "mpegaudio", order: orderSyncFrames,
		sniff: mpegAudioSyntax.sniffFrames, read: readMPEGAudio,
	})
}

// orderSyncFrames places the formats recognised by their frames after the ones
// with a magic number: a sync word is weaker evidence than four letters.
const orderSyncFrames = 20

// mpegAudioHeaderSize is the length of a frame header.
const mpegAudioHeaderSize = 4

// mpegAudioBitRates is in kbit/s, indexed by [low sampling frequency][layer-1]
// [bitrate_index]. Index 0 is free format and 15 is forbidden.
var mpegAudioBitRates = [2][3][15]int{
	{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	},
	{
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	},
}

// mpegAudioRates is MPEG-1's sampling frequency table; MPEG-2 halves it and
// MPEG 2.5 quarters it.
var mpegAudioRates = [3]int{44100, 48000, 32000}

// mpegAudioHeader is what one frame header states.
type mpegAudioHeader struct {
	layer      int  // 1, 2 or 3
	lsf        bool // MPEG-2 or 2.5: the low sampling frequencies
	mpeg25     bool
	crc        bool // a 16-bit CRC follows the header
	sampleRate int
	channels   int
	size       int // bytes, header included
}

// samples is how many audio samples per channel the frame holds: 384 for
// Layer I, 1152 for Layer II, and 1152 or 576 for Layer III, which codes one
// granule instead of two at the low sampling frequencies.
func (h mpegAudioHeader) samples() int {
	switch {
	case h.layer == 1:
		return 384
	case h.layer == 3 && h.lsf:
		return 576
	}
	return 1152
}

// codec is the name a track of this layer is reported under.
func (h mpegAudioHeader) codec() string {
	return [...]string{"", "mp1", "mp2", "mp3"}[h.layer]
}

// parseMPEGAudioHeader reads a frame header, refusing what
// ff_mpa_check_header refuses: no sync, the reserved version, the reserved
// layer, the forbidden bit rate and the reserved sampling frequency; and, here,
// free format too.
func parseMPEGAudioHeader(data []byte) (mpegAudioHeader, bool) {
	var h mpegAudioHeader
	if len(data) < mpegAudioHeaderSize {
		return h, false
	}
	word := binary.BigEndian.Uint32(data)
	version := word >> 19 & 3
	layerBits := word >> 17 & 3
	rateIndex := word >> 12 & 0xF
	freqIndex := word >> 10 & 3
	if word&0xFFE00000 != 0xFFE00000 || version == 1 || layerBits == 0 ||
		rateIndex == 0 || rateIndex == 15 || freqIndex == 3 {
		return h, false
	}
	h.layer = 4 - int(layerBits)
	h.lsf = version != 3
	h.mpeg25 = version == 0
	h.crc = word>>16&1 == 0
	shift := 0
	if h.lsf {
		shift++
	}
	if h.mpeg25 {
		shift++
	}
	h.sampleRate = mpegAudioRates[freqIndex] >> shift
	h.channels = 2
	if word>>6&3 == 3 {
		h.channels = 1
	}
	low := 0
	if h.lsf {
		low = 1
	}
	kbps := mpegAudioBitRates[low][h.layer-1][rateIndex]
	padding := int(word >> 9 & 1)
	switch h.layer {
	case 1:
		h.size = (12000*kbps/h.sampleRate + padding) * 4
	case 2:
		h.size = 144000*kbps/h.sampleRate + padding
	default:
		lsfShift := 0
		if h.lsf {
			lsfShift = 1
		}
		h.size = 144000*kbps/(h.sampleRate<<lsfShift) + padding
	}
	return h, true
}

// mpegAudioSyntax walks MPEG audio frames.
var mpegAudioSyntax = frameSyntax{
	name:       "mpegaudio",
	headerSize: mpegAudioHeaderSize,
	length: func(data []byte) int {
		h, _ := parseMPEGAudioHeader(data)
		return h.size
	},
	// The version, the layer, the sampling rate and whether it is mono. The
	// channel mode itself is NOT compared: an encoder switches between joint
	// and plain stereo from one frame to the next.
	sameStream: func(a, b []byte) bool {
		const mask = 0xFFFE0C00
		x, y := binary.BigEndian.Uint32(a), binary.BigEndian.Uint32(b)
		return x&mask == y&mask && (x>>6&3 == 3) == (y>>6&3 == 3)
	},
}

// readMPEGAudio reads an MPEG audio elementary stream.
func readMPEGAudio(data []byte) ([]*mkvReadTrack, error) {
	frames, err := mpegAudioSyntax.walkFrames(data)
	if len(frames) == 0 {
		return nil, noFrames("mpegaudio", err)
	}
	first, _ := parseMPEGAudioHeader(data[frames[0][0]:])
	delay := time.Duration(0)
	if info, ok := mpegAudioInfoFrame(data[frames[0][0]:frames[0][0]+frames[0][1]], first); ok {
		// The Xing, Info or VBRI frame describes the stream and holds no
		// audio: a decoder outputs silence for it, and FFmpeg drops it.
		frames = frames[1:]
		delay = info
	}
	t := audioTrack(first.codec(), uint32(first.sampleRate), first.channels, first.sampleRate)
	t.config.StartDelay = delay
	for _, f := range frames {
		t.samples = append(t.samples, Sample{
			Data:     data[f[0] : f[0]+f[1]],
			Duration: uint32(first.samples()),
			Sync:     true,
		})
	}
	return oneTrack(t, err)
}

// noFrames is the refusal of a stream in which no frame was found.
func noFrames(format string, err error) error {
	if err != nil {
		return err
	}
	return errorf(ErrMalformed, "%s: no frame found", format)
}

// The offsets of the Xing or Info tag, after the four-byte header: it sits
// where Layer III's side information would, whose length depends on the
// version and on whether the frame is mono. These are LAME's own offsets
// (VbrTag.c), which FFmpeg's mp3dec.c repeats, and like both of them they do not
// move for a CRC.
const (
	xingOffsetMPEG1Stereo = 32
	xingOffsetMPEG1Mono   = 17
	xingOffsetLSFStereo   = 17
	xingOffsetLSFMono     = 9
	// vbriOffset is fixed: Fraunhofer's tag is 32 bytes past the header
	// whatever the frame.
	vbriOffset = 32
)

// Xing header flags, which say which optional fields follow.
const (
	xingFrames  = 1
	xingBytes   = 2
	xingTOC     = 4
	xingQuality = 8
)

// lameDecoderDelay is what an MP3 decoder adds at the start on top of what the
// encoder states: 528 samples of the synthesis filterbank, plus one. It is the
// constant LAME documents and FFmpeg's mp3dec.c applies.
const lameDecoderDelay = 528 + 1

// mpegAudioInfoFrame reports whether a Layer III frame is the Xing, Info or
// VBRI frame an encoder puts first, and the encoder delay the LAME extension of
// a Xing or Info tag states, as the time a decoder has to discard.
//
// The delay is read only from an encoder that states it in the LAME layout --
// LAME itself and FFmpeg's own libavformat and libavcodec, the three version
// strings mp3dec.c trusts -- and is what TrackConfig.StartDelay carries so a
// remux keeps the priming out of what is presented.
func mpegAudioInfoFrame(frame []byte, h mpegAudioHeader) (time.Duration, bool) {
	if h.layer != 3 {
		return 0, false
	}
	at := mpegAudioHeaderSize
	switch {
	case !h.lsf && h.channels == 2:
		at += xingOffsetMPEG1Stereo
	case !h.lsf:
		at += xingOffsetMPEG1Mono
	case h.channels == 2:
		at += xingOffsetLSFStereo
	default:
		at += xingOffsetLSFMono
	}
	if v := mpegAudioHeaderSize + vbriOffset; len(frame) >= v+4 && string(frame[v:v+4]) == "VBRI" {
		return 0, true
	}
	if len(frame) < at+8 {
		return 0, false
	}
	if tag := string(frame[at : at+4]); tag != "Xing" && tag != "Info" {
		return 0, false
	}
	flags := binary.BigEndian.Uint32(frame[at+4:])
	at += 8
	for _, f := range []struct {
		bit  uint32
		size int
	}{{xingFrames, 4}, {xingBytes, 4}, {xingTOC, 100}, {xingQuality, 4}} {
		if flags&f.bit != 0 {
			at += f.size
		}
	}
	// The LAME extension: a nine-byte version string, then twelve bytes of
	// gains and settings, then the encoder delay and padding, twelve bits
	// each.
	const delayAt = 9 + 12
	if len(frame) < at+delayAt+3 {
		return 0, true
	}
	switch string(frame[at : at+4]) {
	case "LAME", "Lavf", "Lavc":
	default:
		return 0, true
	}
	d := frame[at+delayAt:]
	start := int64(d[0])<<4 | int64(d[1])>>4
	samples := start + lameDecoderDelay
	return time.Duration(samples * int64(time.Second) / int64(h.sampleRate)), true
}

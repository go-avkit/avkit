// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"time"
)

// An MPEG audio elementary stream -- an .mp3, an .mp2 -- is its frames and
// nothing else: no header anywhere states the stream as a whole. Each frame's
// header is read by parseMPAFrame, the reader the transport stream carriage
// uses, so the two cannot disagree about where a frame ends; the walk over the
// frames, the tags around them and the resynchronisation are framewalk.go's,
// shared with ADTS. The demuxing follows FFmpeg's libavformat/mp3dec.c
// (2026-10-18).
//
// What this adds is the frame an encoder puts first to describe the stream --
// Xing or Info (LAME), VBRI (Fraunhofer) -- which a decoder outputs as silence
// and FFmpeg drops, and the encoder delay LAME states in it.
//
// ⛔ The unofficial "MPEG 2.5" rates and the free format are not read, because
// parseMPAFrame does not read them: an MP3 at 8, 11.025 or 12 kHz is refused as
// holding no frame rather than read by a guess.

// FormatMPEGAudio is an MPEG-1/2 audio elementary stream: MP3, MP2 or MP1.
const FormatMPEGAudio Format = 5

func init() {
	registerElementary(elementaryFormat{
		format: FormatMPEGAudio, name: "mpegaudio", order: orderSyncFrames,
		sniff: mpegAudioSyntax.sniffFrames, read: readMPEGAudio,
	})
}

// mpegAudioSyntax walks MPEG audio frames.
var mpegAudioSyntax = frameSyntax{
	name:       "mpegaudio",
	headerSize: mpaHeaderSize,
	length: func(data []byte) int {
		f, err := parseMPAFrame(data)
		if err != nil {
			return 0
		}
		return f.FrameSize
	},
	// The version, the layer, the sampling rate and whether it is mono --
	// what a decoder locks to. The channel mode itself is NOT compared: an
	// encoder switches between joint and plain stereo from frame to frame.
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
	first, _ := parseMPAFrame(data[frames[0][0]:])
	t := audioTrack(first.Codec(), uint32(first.SampleRate), first.Channels, first.SampleRate)
	if delay, info := mpegAudioInfoFrame(data[frames[0][0]:frames[0][0]+frames[0][1]], first); info {
		// The Xing, Info or VBRI frame describes the stream and holds no
		// audio: a decoder outputs silence for it, and FFmpeg drops it.
		frames = frames[1:]
		t.config.StartDelay = delay
	}
	for _, f := range frames {
		t.samples = append(t.samples, Sample{
			Data:     data[f[0] : f[0]+f[1]],
			Duration: uint32(first.Samples),
			Sync:     true,
		})
	}
	return oneTrack(t, err)
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
func mpegAudioInfoFrame(frame []byte, h mpaFrame) (time.Duration, bool) {
	if h.Layer != 3 {
		return 0, false
	}
	at := mpaHeaderSize
	switch {
	case h.MPEG1 && h.Channels == 2:
		at += xingOffsetMPEG1Stereo
	case h.MPEG1:
		at += xingOffsetMPEG1Mono
	case h.Channels == 2:
		at += xingOffsetLSFStereo
	default:
		at += xingOffsetLSFMono
	}
	if v := mpaHeaderSize + vbriOffset; len(frame) >= v+4 && string(frame[v:v+4]) == "VBRI" {
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
	return time.Duration(samples * int64(time.Second) / int64(h.SampleRate)), true
}

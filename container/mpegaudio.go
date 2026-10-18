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
// and FFmpeg drops, and the encoder delay and end padding LAME states in it:
// the delay as TrackConfig.StartDelay, the padding as the end of an edit list
// (see mpegAudioTag.endTrim).
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
	sync:       0xFF,
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
	if tag, info := mpegAudioInfoFrame(data[frames[0][0]:frames[0][0]+frames[0][1]], first); info {
		// The Xing, Info or VBRI frame describes the stream and holds no
		// audio: a decoder outputs silence for it, and FFmpeg drops it.
		frames = frames[1:]
		t.config.StartDelay = tag.startDelay(first.SampleRate)
		t.config.EditList = tag.endTrim(len(frames), first.Samples, first.SampleRate)
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

// mpegAudioTag is what the LAME extension of a Xing or Info tag states about
// the stream, when an encoder that writes it in that layout wrote it: lame is
// set then, and only then is anything else here worth reading.
type mpegAudioTag struct {
	lame bool
	// delay is what a decoder discards at the start, in samples: the
	// encoder's delay plus the decoder's own 529 (mp3dec.c start_skip_samples).
	// endPad is the encoder's padding at the end, in samples, as stated.
	delay, endPad int64
	// frames is the tag's count of the audio frames that follow it, and
	// counted says the tag states one (the Xing "frames" flag).
	frames  uint32
	counted bool
}

// startDelay is the priming a decoder discards, as TrackConfig.StartDelay
// carries it so a remux keeps it out of what is presented.
func (tag mpegAudioTag) startDelay(rate int) time.Duration {
	if !tag.lame {
		return 0
	}
	return time.Duration(tag.delay * int64(time.Second) / int64(rate))
}

// endTrim is the edit list that keeps the encoder's padding at the end out of
// the presentation, or nil when the tag states none that reaches into the
// decoded audio.
//
// It is the trim ffmpeg's mp3dec.c applies (mp3_parse_info_tag): the decoded
// stream is the tag's frame count times the samples of a frame, and samples
// from first_discard_sample = frames x spf - end_pad + 529 on are dropped --
// the padding less the 529 the decoder's own delay pushes past the last frame.
// The presentation starts at the priming, as StartDelay says, so the edit is
// that start and the length up to the trim: frames x spf - start_pad -
// end_pad, the duration mp3dec states. It is how ffmpeg's movenc writes the
// same file into an MP4 (an edit ending before the media, measured: a 1 s
// libmp3lame MP3 copies to segment 44100 at media time 1105, over 40 frames of
// 1152), and WebMMuxer writes such an edit as the DiscardPadding of the last
// block, which is ffmpeg's Matroska form of it.
//
// Like mp3dec, only a tag stating its frame count is read, since the trim is
// placed by it. Unlike mp3dec, the count has to be the number of frames the
// stream holds: a stream cut short or added to after the tag was written would
// otherwise lose audio from wherever the count lands, and ffmpeg's own check
// for that, against the tag's byte count, is weaker.
func (tag mpegAudioTag) endTrim(frames, perFrame, rate int) *EditList {
	if !tag.lame || !tag.counted || uint64(tag.frames) != uint64(frames) {
		return nil
	}
	total := int64(frames) * int64(perFrame)
	end := total - tag.endPad + lameDecoderDelay
	if end >= total || end <= tag.delay {
		return nil
	}
	return &EditList{MovieTimescale: uint32(rate), Edits: []Edit{{
		SegmentDuration: uint64(end - tag.delay), MediaTime: tag.delay, Rate: NormalRate,
	}}}
}

// mpegAudioInfoFrame reports whether a Layer III frame is the Xing, Info or
// VBRI frame an encoder puts first, and what the LAME extension of a Xing or
// Info tag states.
//
// The extension is read only from an encoder that states it in the LAME layout
// -- LAME itself and FFmpeg's own libavformat and libavcodec, the three version
// strings mp3dec.c trusts.
func mpegAudioInfoFrame(frame []byte, h mpaFrame) (mpegAudioTag, bool) {
	var tag mpegAudioTag
	if h.Layer != 3 {
		return tag, false
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
		return tag, true
	}
	if len(frame) < at+8 {
		return tag, false
	}
	if name := string(frame[at : at+4]); name != "Xing" && name != "Info" {
		return tag, false
	}
	flags := binary.BigEndian.Uint32(frame[at+4:])
	at += 8
	// The frame count is the first optional field; it is read once the frame
	// is known to hold the whole extension, which comes after it.
	framesAt := at
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
		return tag, true
	}
	switch string(frame[at : at+4]) {
	case "LAME", "Lavf", "Lavc":
	default:
		return tag, true
	}
	d := frame[at+delayAt:]
	tag.lame = true
	tag.delay = (int64(d[0])<<4 | int64(d[1])>>4) + lameDecoderDelay
	tag.endPad = int64(d[1]&0x0F)<<8 | int64(d[2])
	if flags&xingFrames != 0 {
		tag.frames, tag.counted = binary.BigEndian.Uint32(frame[framesAt:]), true
	}
	return tag, true
}

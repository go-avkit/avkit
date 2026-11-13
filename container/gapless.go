// Copyright (c) 2026, go-avkit
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"strconv"
	"strings"
)

// Gapless is the gapless-playback information an Apple encoder (iTunes,
// afconvert, Music) writes into an MP4: the iTunSMPB freeform item of the
// movie's ilst, whose text is four hexadecimal fields,
//
//	" 00000000 00000840 000001C6 0000000000011C3D 00000000 ..."
//
// a reserved one, the encoder delay, the end padding and the number of samples
// the original audio had. The three numbers are counted in ticks of the
// track's media timescale -- for an AAC track whose timescale is the core rate
// of an HE-AAC stream they are core-rate samples, half of what the decoder
// outputs -- and describe a track that is a whole number of codec frames long:
//
//	Delay + Samples + Padding = the track's duration
//
// Metadata.Gapless is file-level, like the item: it does not say which track it
// describes (an audio track; a file with several has to be told). ffmpeg attaches
// it to the last stream it has parsed when it reads the item, so in a file whose
// audio track comes first it lands on a later track and ffmpeg trims no audio.
//
// Metadata.Gapless is set from an MP4 only, and only when the item reads as
// those four fields; a Matroska file, whose gapless information is a codec delay
// and a discard padding, has none. It is read-only: an MP4 output carries the
// item itself byte for byte, as it does every item no field names, and
// ReplaceMetadata writes none.
type Gapless struct {
	// Delay is the encoder delay: how many samples at the start of the track
	// are not audio. Padding is how many at its end are not. Samples is how
	// many the original audio had.
	Delay, Padding, Samples uint64
}

// maxGapless bounds each of the three numbers, as ffmpeg does (libavformat's
// SMPB_MAX_SAMPLES): about nine months of audio at 44.1 kHz, so that no sum or
// rescaling of them overflows.
const maxGapless = 1 << 40

// parseGapless reads the text of an iTunSMPB item, or returns nil when it is
// not four hexadecimal fields of at most sixteen digits each (anything after the
// fourth is ignored, the text ends at its first NUL) with the last three within
// maxGapless. A field cut short at sixteen digits would shift every field after
// it, so it is refused. This is stricter and wider than ffmpeg's MP4 reader, which
// scans three 32-bit hexadecimal numbers (libavformat/mov.c, n9.0.2) and uses the
// delay only when it is in (0, 16384) and the padding only when the track's
// duration exceeds both it and the sample count; the bound here is that of ffmpeg's
// shared parser (demux_utils.c), which the MP4 path does not call. The values are
// kept as stated: what to do with them is the decoder's.
func parseGapless(text string) *Gapless {
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	f := strings.Fields(text)
	if len(f) < 4 {
		return nil
	}
	var v [4]uint64
	for i := range v {
		if len(f[i]) > 16 {
			return nil
		}
		for _, c := range []byte(f[i]) {
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return nil
			}
		}
		v[i], _ = strconv.ParseUint(f[i], 16, 64)
	}
	if v[1] > maxGapless || v[2] > maxGapless || v[3] > maxGapless {
		return nil
	}
	return &Gapless{Delay: v[1], Padding: v[2], Samples: v[3]}
}

// freeform reads a `----` item: its name and the value of its first data box,
// with false when it holds no mean, no name or no data. Only those boxes are
// looked at, in whatever order they come, and the first of each counts. The mean
// (the reverse-DNS namespace, "com.apple.iTunes" for iTunSMPB) must be there but
// is not compared: ffmpeg does not either.
func freeform(body []byte) (name string, value []byte, ok bool) {
	var haveMean, haveName, haveData bool
	for rest := body; len(rest) > 0; {
		typ, content, next, good := splitBox(rest)
		if !good {
			break
		}
		rest = next
		switch {
		case typ == "mean" && len(content) >= 4: // version and flags, then the text
			haveMean = true
		case typ == "name" && len(content) >= 4 && !haveName:
			name, haveName = string(content[4:]), true
		case typ == "data" && len(content) >= ilstDataHeader && !haveData:
			value, haveData = content[ilstDataHeader:], true
		}
	}
	return name, value, haveMean && haveName && haveData
}

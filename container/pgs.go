// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import "fmt"

// codecPGS names the Blu-ray presentation graphics stream, the bitmap subtitles
// of HDMV discs. It is the name this package gives a track whose Matroska
// CodecID is S_HDMV/PGS; there is no four-character code for it because MP4RA
// registers none (sample-entries.csv holds no PGS or HDMV entry, measured
// against mp4ra.github.io on 2026-11-01).
//
// A PGS track is carried Matroska to Matroska and nowhere else. Each block of
// the track is one display set -- the segments from the presentation
// composition segment to the end segment, each as its type byte, 16-bit size and
// payload, without the "PG" header and the PTS and DTS a .sup file puts before
// every segment, which the block's own timestamp replaces -- and travels as it
// stands. This package does not read inside a display set: nothing it does
// needs the bitmaps, and a rewritten one would be a guess.
const (
	codecPGS = "pgs"
	mkvPGS   = "S_HDMV/PGS"
)

// errPGSNotInTS is why a transport stream does not take a PGS track.
//
// ⛔ Not because it cannot be done: ffmpeg writes it (mpegtsenc.c maps
// AV_CODEC_ID_HDMV_PGS_SUBTITLE to stream type 0x90, STREAM_TYPE_BLURAY_SUBTITLE_PGS,
// and mpegts.c reads it back), and the PES payload is the same segments a
// Matroska block holds. It is out of scope here because stream type 0x90 is
// defined by the Blu-ray Disc specifications, which are not public and so were
// not opened for this package, and this package writes what a specification it
// has read defines. What is known without them says a faithful stream needs more
// than the block states: the DTS of each display set (the graphics decoder model
// of the disc), which a block does not store and which cannot be had without
// reading inside the set. The population this was written for -- two Matroska
// files -- asks for Matroska to Matroska, and a Blu-ray rip's own M2TS is the
// disc's, not this package's, to write.
func errPGSNotInTS() error {
	return fmt.Errorf("%w: pgs: stream type 0x90 belongs to the Blu-ray Disc specifications, which this package "+
		"has not read, and a PES needs the decode time of each display set that a Matroska block does not store "+
		"(Matroska carries the track)", ErrUnsupportedCodec)
}

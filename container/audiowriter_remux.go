// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"fmt"
	"io"
)

// AudioOutput makes Remux, Cut and Concat write an audio-only file instead of a
// fragmented MP4: format is FormatADTS, FormatMPEGAudio, FormatFLAC, FormatOgg
// or FormatWAV, and the input must come down to one audio track -- DropTracks
// leaves out the rest. A track whose codec the format does not carry is refused
// with ErrUnsupportedCodec, naming what it does.
//
// Nothing is re-encoded and no sample's bytes are touched (WAVE aside, whose
// integers are little-endian and whose 8-bit ones unsigned). The container's
// metadata, and the priming and edit lists an MP4 or Matroska states outside
// the frames, are not carried: see the notes at the top of audiowriter.go.
func AudioOutput(format Format, opts ...AudioOption) RemuxOption {
	return func(s *remuxSettings) { s.audio, s.audioOpts = format, opts }
}

// writeAudioCopy is writeCopy for an audio-only output: the first input's track
// is declared, then every input's samples are written behind one another.
func writeAudioCopy(w io.Writer, set *remuxSettings, inputs [][]sourceTrack) error {
	if n := len(inputs[0]); n != 1 {
		return fmt.Errorf("%w: %s output holds one track and the input has %d (DropTracks leaves tracks out)",
			ErrTrackConfig, describeFormat(set.audio), n)
	}
	a := NewAudioWriter(set.audio, w, set.audioOpts...)
	id, err := a.AddTrack(inputs[0][0].cfg)
	if err != nil {
		return err
	}
	for _, in := range inputs {
		for _, s := range in[0].samples {
			if err := a.WriteSample(id, s); err != nil {
				return err
			}
		}
	}
	return a.Close()
}

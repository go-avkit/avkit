// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"

	"github.com/Eyevinn/mp4ff/aac"
)

// An ADTS stream -- a raw .aac file -- is AAC frames each behind a header that
// repeats the stream's configuration: ISO/IEC 13818-7 §6.2 adts_frame, which
// ISO/IEC 14496-3 §1.A.2 carries over to MPEG-4 audio. An MP4 states that
// configuration once, as an AudioSpecificConfig, and holds the frames bare, so
// reading one means stripping every header and building the record from the
// first.
//
// The header is read by mp4ff's aac.DecodeADTSHeader, which the transport
// stream reader already uses; the walk over the frames, the tags around them
// and the resynchronisation follow FFmpeg's libavformat/aacdec.c (2026-10-18).
//
// ⛔ A frame of several raw data blocks (number_of_raw_data_blocks_in_frame
// above 0) is refused, by mp4ff and so here. An MP4 sample is one raw data
// block, and cutting a frame into its blocks needs the blocks' own lengths,
// which only a decoder can find -- the header states the frame's length, not
// theirs. Every encoder in use writes one block per frame.

// FormatADTS is a raw AAC stream in ADTS frames.
const FormatADTS Format = 4

func init() {
	registerElementary(elementaryFormat{
		format: FormatADTS, name: "adts", order: orderSyncFrames,
		sniff: adtsSyntax.sniffFrames, read: readADTS,
	})
}

// adtsHeaderSize is the length of a header without its CRC.
const adtsHeaderSize = 7

// adtsFrameHeader reads the header at the start of data, which must start
// there: mp4ff searches for a sync word, and a header found further on is not
// the one at this offset.
func adtsFrameHeader(data []byte) (*aac.ADTSHeader, bool) {
	h, skipped, err := aac.DecodeADTSHeader(bytes.NewReader(data[:adtsHeaderSize+2]))
	if err != nil || skipped != 0 || adtsFrequency(h.SamplingFrequencyIndex) == 0 {
		return nil, false
	}
	return h, true
}

// adtsSyntax walks ADTS frames.
var adtsSyntax = frameSyntax{
	name: "adts",
	// Nine bytes, so the CRC of a protected header is there to be read.
	headerSize: adtsHeaderSize + 2,
	length: func(data []byte) int {
		h, ok := adtsFrameHeader(data)
		if !ok {
			return 0
		}
		return int(h.HeaderLength) + int(h.PayloadLength)
	},
	// The MPEG version, the profile, the sampling frequency and the channel
	// configuration; not the protection bit, nor the private bit, which say
	// nothing about the audio.
	sameStream: func(a, b []byte) bool {
		return a[1]&0xFE == b[1]&0xFE && a[2]&0xFD == b[2]&0xFD && a[3]&0xC0 == b[3]&0xC0
	},
}

// readADTS reads an ADTS stream: the frames stripped of their headers, and the
// AudioSpecificConfig the first header describes.
func readADTS(data []byte) ([]*mkvReadTrack, error) {
	frames, err := adtsSyntax.walkFrames(data)
	if len(frames) == 0 {
		return nil, noFrames("adts", err)
	}
	firstFrame := data[frames[0][0] : frames[0][0]+frames[0][1]]
	h, _ := adtsFrameHeader(firstFrame)
	rate := adtsFrequency(h.SamplingFrequencyIndex)
	t := audioTrack("mp4a", uint32(rate), int(h.ChannelConfig), rate)
	// The record is the one ConfigFromSamples builds from an ADTS frame, so
	// the two ways of reading the same header cannot disagree. A stream whose
	// channel configuration is 0 states its layout in a program config
	// element inside the frames, which a record built from the header cannot
	// carry: its frames still read, and the track says why it cannot be
	// written.
	if derived, cerr := aacStreamConfig("mp4a", []Sample{{Data: firstFrame}}); cerr != nil {
		t.configErr = cerr
	} else {
		t.config.CodecConfig = derived.CodecConfig
		t.config.AudioObjectType = derived.AudioObjectType
	}
	for _, f := range frames {
		frame := data[f[0] : f[0]+f[1]]
		header, _ := adtsFrameHeader(frame)
		t.samples = append(t.samples, Sample{
			Data:     frame[header.HeaderLength:],
			Duration: aacFrameSamples,
			Sync:     true,
		})
	}
	return oneTrack(t, err)
}

// aacFrameSamples is how many samples per channel an AAC frame holds, at the
// rate its header states: 1024, the frame length every ADTS profile uses.
const aacFrameSamples = 1024

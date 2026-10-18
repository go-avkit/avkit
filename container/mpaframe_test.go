// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"testing"
)

// mpaHeader builds the four-byte header of an MPEG audio frame: the ID bit
// (1 for ISO/IEC 11172-3), the layer, bitrate_index, sampling_frequency,
// padding_bit and mode, with no CRC.
func mpaHeader(id, layer int, bitRateIndex, rateIndex, padding, mode byte) []byte {
	return []byte{
		0xFF,
		0xF0 | byte(id)<<3 | byte(4-layer)<<1 | 1,
		bitRateIndex<<4 | rateIndex<<2 | padding<<1,
		mode << 6,
	}
}

// TestTheMPEGAudioHeaderStatesTheFramesLength checks the lengths against what
// FFmpeg's mpegaudiodecheader.c computes for the same headers, including the
// two the ffmpeg-mp3*.ts fixtures hold.
func TestTheMPEGAudioHeaderStatesTheFramesLength(t *testing.T) {
	for _, c := range []struct {
		name   string
		header []byte
		want   mpaFrame
	}{
		{"MPEG-1 Layer III, 64 kbit/s at 44.1 kHz, mono",
			mpaHeader(1, 3, 5, 0, 0, 3),
			mpaFrame{MPEG1: true, Layer: 3, BitRate: 64, SampleRate: 44100, Channels: 1, FrameSize: 208, Samples: 1152}},
		{"the same, padded",
			mpaHeader(1, 3, 5, 0, 1, 3),
			mpaFrame{MPEG1: true, Layer: 3, BitRate: 64, SampleRate: 44100, Channels: 1, FrameSize: 209, Samples: 1152}},
		{"MPEG-2 Layer III, 32 kbit/s at 22.05 kHz: half the samples",
			mpaHeader(0, 3, 4, 0, 0, 3),
			mpaFrame{Layer: 3, BitRate: 32, SampleRate: 22050, Channels: 1, FrameSize: 104, Samples: 576}},
		{"MPEG-1 Layer II, 64 kbit/s at 48 kHz, stereo",
			mpaHeader(1, 2, 4, 1, 0, 0),
			mpaFrame{MPEG1: true, Layer: 2, BitRate: 64, SampleRate: 48000, Channels: 2, FrameSize: 192, Samples: 1152}},
		{"MPEG-2 Layer II keeps 1152 samples, 64 kbit/s at 24 kHz",
			mpaHeader(0, 2, 8, 1, 0, 0),
			mpaFrame{Layer: 2, BitRate: 64, SampleRate: 24000, Channels: 2, FrameSize: 384, Samples: 1152}},
		{"MPEG-1 Layer I counts four-byte slots, 32 kbit/s at 48 kHz",
			mpaHeader(1, 1, 1, 1, 1, 1),
			mpaFrame{MPEG1: true, Layer: 1, BitRate: 32, SampleRate: 48000, Channels: 2, FrameSize: 36, Samples: 384}},
		{"MPEG-2 Layer I, 256 kbit/s at 16 kHz",
			mpaHeader(0, 1, 14, 2, 0, 3),
			mpaFrame{Layer: 1, BitRate: 256, SampleRate: 16000, Channels: 1, FrameSize: 768, Samples: 384}},
	} {
		got, err := parseMPAFrame(c.header)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestTheLayerNamesTheCodec(t *testing.T) {
	for layer, want := range map[int]string{1: "mp1", 2: "mp2", 3: "mp3"} {
		if got := (mpaFrame{Layer: layer}).Codec(); got != want {
			t.Errorf("Layer %d is %q, want %q", layer, got, want)
		}
	}
}

// TestAnMPEGAudioHeaderThatStatesNoLengthIsRefused: free format states no
// length, and the reserved and forbidden values state nothing.
func TestAnMPEGAudioHeaderThatStatesNoLengthIsRefused(t *testing.T) {
	reservedLayer := mpaHeader(1, 3, 5, 0, 0, 0)
	reservedLayer[1] &^= 0x06
	for name, header := range map[string][]byte{
		"three bytes":              {0xFF, 0xFB, 0x50},
		"no sync word":             {0xFF, 0xE3, 0x50, 0},
		"an MPEG 2.5 sync word":    {0xFF, 0xE3, 0x50, 0xC0},
		"a reserved layer":         reservedLayer,
		"free format":              mpaHeader(1, 3, 0, 0, 0, 0),
		"the forbidden bitrate":    mpaHeader(1, 3, 15, 0, 0, 0),
		"a reserved sampling rate": mpaHeader(1, 3, 5, 3, 0, 0),
		"an AC-3 sync word":        {0x0B, 0x77, 0, 0},
	} {
		if _, err := parseMPAFrame(header); !errors.Is(err, ErrCodecMismatch) {
			t.Errorf("%s: %v, want ErrCodecMismatch", name, err)
		}
	}
}

// FuzzParseMPAFrame: whatever the bytes, the parser returns, and a frame it
// accepts has a length that at least holds its own header.
func FuzzParseMPAFrame(f *testing.F) {
	f.Add(mpaHeader(1, 3, 5, 0, 0, 3))
	f.Add(mpaHeader(0, 1, 14, 2, 0, 3))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := parseMPAFrame(data)
		if err == nil && got.FrameSize < mpaHeaderSize {
			t.Fatalf("a %d-byte frame from %x", got.FrameSize, data)
		}
	})
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
)

// A WAV file is a RIFF form of type WAVE: a "fmt " chunk that describes the
// samples, then a "data" chunk that holds them, among chunks that do neither
// (Microsoft's Multimedia Programming Interface and Data Specifications 1.0,
// "WAVE Form Definition", and the WAVEFORMATEXTENSIBLE of mmreg.h). RF64 (EBU
// Tech 3306) and its successor BW64 (ITU-R BS.2088) are the same form with
// 64-bit sizes, kept in a "ds64" chunk because the RIFF fields are 32 bits.
//
// Only linear PCM is read: WAVE_FORMAT_PCM, WAVE_FORMAT_IEEE_FLOAT, and either
// of the two inside WAVE_FORMAT_EXTENSIBLE. Anything else -- ADPCM, an MP3 or
// an AC-3 stream in a WAVE wrapper -- is reported by its format tag and not
// read: guessing what a codec's frames are from the wrapper's block size is how
// a file comes out as noise.
//
// The chunk walk follows FFmpeg's libavformat/wavdec.c, and the samples are
// handed out in packets as its PCM demuxer cuts them (libavformat/pcm.c,
// ff_pcm_default_packet_size): a power of two of frames close to a tenth of a
// second. A sample of PCM is a single instant, and a track of one Sample per
// instant would be millions of them.

// FormatWAV is a RIFF WAVE file, or its 64-bit forms RF64 and BW64.
const FormatWAV Format = 7

func init() {
	registerElementary(elementaryFormat{
		format: FormatWAV, name: "wav", order: orderMagic,
		sniff: sniffWAV, read: readWAV,
	})
}

// WAVE format tags this reader knows (mmreg.h).
const (
	waveFormatPCM        = 0x0001
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xFFFE
)

// waveSubFormatTail is the KSDATAFORMAT_SUBTYPE GUID with its first two bytes
// -- the format tag -- left out: {xxxx0000-0000-0010-8000-00AA00389B71}, as it
// is laid out in the file.
var waveSubFormatTail = []byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00,
	0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}

// riffSizeUnknown is what RF64 writes in a 32-bit size whose real value is in
// the ds64 chunk.
const riffSizeUnknown = 0xFFFFFFFF

// riffChunkHeader is the length of a chunk's identifier and size.
const riffChunkHeader = 8

// sniffWAV recognises the three RIFF forms of WAVE.
func sniffWAV(data []byte) bool {
	if len(data) < 12 || string(data[8:12]) != "WAVE" {
		return false
	}
	switch string(data[:4]) {
	case "RIFF", "RF64", "BW64":
		return true
	}
	return false
}

// wavFormat is what a "fmt " chunk states.
type wavFormat struct {
	tag        uint16
	channels   int
	sampleRate int
	blockAlign int
	bits       int
}

// readWAV reads a WAVE file.
func readWAV(data []byte) ([]*mkvReadTrack, error) {
	wide := string(data[:4]) != "RIFF"
	var (
		format   *wavFormat
		dataSize uint64 // from ds64, when the data chunk's own field is unknown
	)
	for at := 12; at+riffChunkHeader <= len(data); {
		id := string(data[at : at+4])
		size := uint64(binary.LittleEndian.Uint32(data[at+4:]))
		body := at + riffChunkHeader
		switch {
		case id == "ds64" && wide:
			if size < 24 || body+24 > len(data) {
				return nil, errorf(ErrMalformed, "wav: the ds64 chunk is %d bytes, too short for its sizes", size)
			}
			dataSize = binary.LittleEndian.Uint64(data[body+8:])
		case id == "fmt ":
			if body+int(min(size, 40)) > len(data) {
				return nil, truncatedAt("wav", len(data)-body, int(size))
			}
			f, err := parseWAVFormat(data[body : body+int(min(size, 40))])
			if err != nil {
				return nil, err
			}
			format = &f
		case id == "data":
			if format == nil {
				return nil, errorf(ErrMalformed, "wav: the data chunk comes before any fmt chunk")
			}
			if size == riffSizeUnknown && wide {
				size = dataSize
			}
			return oneTrack(wavTrack(*format, data[body:], size))
		}
		// Chunks are padded to an even length, and the pad is not counted.
		next := uint64(body) + size + size&1
		if next > uint64(len(data)) {
			break
		}
		at = int(next)
	}
	if format == nil {
		return nil, errorf(ErrMalformed, "wav: no fmt chunk")
	}
	return nil, errorf(ErrMalformed, "wav: no data chunk")
}

// parseWAVFormat reads a WAVEFORMATEX, or a WAVEFORMATEXTENSIBLE, which
// carries the real format tag in the first two bytes of its sub-format GUID.
func parseWAVFormat(b []byte) (wavFormat, error) {
	if len(b) < 16 {
		return wavFormat{}, errorf(ErrMalformed, "wav: the fmt chunk is %d bytes; a WAVEFORMAT is 16", len(b))
	}
	f := wavFormat{
		tag:        binary.LittleEndian.Uint16(b),
		channels:   int(binary.LittleEndian.Uint16(b[2:])),
		sampleRate: int(binary.LittleEndian.Uint32(b[4:])),
		blockAlign: int(binary.LittleEndian.Uint16(b[12:])),
		bits:       int(binary.LittleEndian.Uint16(b[14:])),
	}
	if f.tag == waveFormatExtensible {
		if len(b) < 40 || !bytes.Equal(b[26:40], waveSubFormatTail) {
			return wavFormat{}, errorf(ErrMalformed, "wav: WAVE_FORMAT_EXTENSIBLE without a KSDATAFORMAT sub-format")
		}
		f.tag = binary.LittleEndian.Uint16(b[24:])
	}
	if f.channels == 0 || f.sampleRate == 0 || f.blockAlign == 0 {
		return wavFormat{}, errorf(ErrMalformed, "wav: %d channels at %d Hz in blocks of %d bytes",
			f.channels, f.sampleRate, f.blockAlign)
	}
	return f, nil
}

// wavCodec is the sample entry a PCM format is reported under, and the pcmC
// record that describes it (ISO/IEC 23003-5 §5.1): version and flags, then
// format_flags, whose low bit says little-endian, then PCM_sample_size in bits.
//
// Eight-bit WAVE is unsigned, which ipcm cannot state -- its samples are two's
// complement -- so it is reported as QuickTime's 'raw ', the entry for
// offset-binary eight-bit samples, and carries no pcmC.
func wavCodec(f wavFormat) (string, []byte, error) {
	if f.tag != waveFormatPCM && f.tag != waveFormatIEEEFloat {
		// A compressed format's block is its own business, not a frame of
		// samples, so nothing below applies to it.
		return wavTagName(f.tag), nil, errorf(ErrUnsupportedCodec,
			"wav: format tag 0x%04x is not linear PCM, and its frames are not read from a WAVE wrapper", f.tag)
	}
	if f.blockAlign%f.channels != 0 {
		return "", nil, errorf(ErrMalformed, "wav: blocks of %d bytes do not divide among %d channels",
			f.blockAlign, f.channels)
	}
	width := f.blockAlign / f.channels // the container of one sample, in bytes
	if f.bits > width*8 {
		return "", nil, errorf(ErrMalformed, "wav: %d-bit samples in %d-byte containers", f.bits, width)
	}
	pcmC := []byte{0, 0, 0, 0, 1, byte(width * 8)}
	switch {
	case f.tag == waveFormatPCM && width == 1:
		return "raw ", nil, nil
	case f.tag == waveFormatPCM && width <= 4:
		return "ipcm", pcmC, nil
	case f.tag == waveFormatIEEEFloat && (width == 4 || width == 8):
		return "fpcm", pcmC, nil
	}
	return "", nil, errorf(ErrUnsupportedCodec, "wav: format tag 0x%04x in %d-byte samples", f.tag, width)
}

// wavTagName names a WAVE format tag the way RFC 2361 §4 spells it as a MIME
// codecs parameter, which is the only name a tag has that is not a vendor's.
func wavTagName(tag uint16) string {
	return fmt.Sprintf("audio/vnd.wave;codec=%x", tag)
}

// wavTrack builds the track of a data chunk. size is what the chunk states;
// payload runs to the end of the file. A size of zero, or the RIFF "unknown",
// is what a writer that could not seek back leaves, and means "to the end".
func wavTrack(f wavFormat, payload []byte, size uint64) (*mkvReadTrack, error) {
	codec, pcmC, err := wavCodec(f)
	t := audioTrack(codec, uint32(f.sampleRate), f.channels, f.sampleRate)
	if err != nil {
		t.configErr, t.samplesErr = err, err
		return t, nil
	}
	t.config.CodecConfig = pcmC
	var cut error
	switch {
	case size == 0 || size == riffSizeUnknown:
		size = uint64(len(payload))
	case size > uint64(len(payload)):
		cut = truncatedAt("wav", len(payload), int(min(size-uint64(len(payload)), 1<<31)))
		size = uint64(len(payload))
	}
	usable := int(size) / f.blockAlign * f.blockAlign
	if usable != int(size) && cut == nil {
		cut = truncatedAt("wav", int(size)-usable, f.blockAlign-(int(size)-usable))
	}
	step := wavPacketFrames(f.sampleRate) * f.blockAlign
	for at := 0; at < usable; at += step {
		n := min(step, usable-at)
		t.samples = append(t.samples, Sample{
			Data:     payload[at : at+n],
			Duration: uint32(n / f.blockAlign),
			Sync:     true,
		})
	}
	return t, cut
}

// wavPacketFrames is how many frames a packet holds: the largest power of two
// not above a tenth of a second, as ff_pcm_default_packet_size chooses it.
func wavPacketFrames(sampleRate int) int {
	tenth := max(sampleRate/10, 1)
	return 1 << (bits.Len(uint(tenth)) - 1)
}

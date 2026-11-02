// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// The WAVE writer: a RIFF form of type WAVE (Microsoft's Multimedia Programming
// Interface and Data Specifications 1.0, and mmreg.h), and its 64-bit form RF64
// (EBU Tech 3306, ITU-R BS.2088), from linear PCM tracks.
//
// The layout and the choices are FFmpeg's wavenc.c and riffenc.c
// (ff_put_wav_header), with its "-rf64 auto" behaviour:
//
//   - The format chunk is WAVE_FORMAT_EXTENSIBLE (a 40-byte chunk: cbSize 22,
//     the valid bits, a channel mask, and the KSDATAFORMAT_SUBTYPE GUID that
//     holds the format tag) when there are more than two channels, when the rate
//     is above 48 kHz, or when integer samples are wider than 16 bits; a plain
//     16-byte WAVEFORMAT otherwise, and an 18-byte WAVEFORMATEX with a cbSize
//     of 0 for float, as riffenc.c writes it.
//   - The channel mask is the default layout for the channel count that
//     FFmpeg's av_channel_layout_default gives (mono, stereo, 2.1, 4.0, 5.0,
//     5.1, 6.1, 7.1); a track's own mask is not carried by TrackConfig, and a
//     count with no default layout gets a mask of 0, "no assignment".
//   - A float stream has a "fact" chunk holding the number of sample frames.
//   - A RIFF header sized for the file is written when the output can seek, and
//     the sizes are patched in at Close. 28 bytes are kept after the format
//     chunk for a ds64 chunk: if the file turns out too large for RIFF's 32-bit
//     sizes (the RIFF size, which counts everything after the first eight
//     bytes, would pass 4 GiB - 1) the header is rewritten as RF64, with the
//     ds64 chunk where the format chunk was and the format chunk after it, and
//     the 32-bit fields set to 0xFFFFFFFF. An output that cannot seek has no
//     way to know the size at the start: it is RIFF with the sizes left at
//     0xFFFFFFFF, "unknown" -- how FFmpeg writes to a pipe, and how this
//     package's reader takes it -- unless WAVRF64 asked for an RF64 header with
//     an empty ds64 chunk, which a stream longer than 4 GiB needs.
//
// The samples are the track's, in the byte order and signedness WAVE wants:
// ipcm of ISO/IEC 23003-5 is two's complement and may be big-endian (the pcmC
// record's format_flags), WAVE's integers are little-endian and its 8-bit ones
// unsigned; the 'raw ' entry already is unsigned 8-bit. Everything else is
// copied as it is.

// wavRIFFMax is the largest RIFF size a 32-bit field states; a variable so a
// test can reach the RF64 path without writing 4 GiB.
var wavRIFFMax uint64 = math.MaxUint32

// wavChannelMask is the default channel mask by channel count.
var wavChannelMask = [...]uint32{0, 0x4, 0x3, 0xB, 0x107, 0x37, 0x3F, 0x70F, 0x63F}

type wavSink struct {
	out    *stickyWriter
	seeker io.WriteSeeker // nil when the output cannot seek
	base   int64          // where the file starts in the seeker

	rf64 bool
	// Offsets from the start of the file: the format chunk (which a ds64 chunk
	// takes the place of), the size field of the data chunk, and the number
	// field of the fact chunk, zero when there is none.
	fmtAt, dataAt, factAt int64
	fmtChunk              []byte

	blockAlign, width int
	swap, flip        bool
	written           uint64
}

func newWAVSink(a *AudioWriter, cfg TrackConfig) (audioSink, error) {
	var tag uint16 = 1
	width, swap, flip := 1, false, false
	switch codec := audioCodec(cfg); codec {
	case "raw":
	case "ipcm", "fpcm":
		little, bits, err := wavPCMC(codec, cfg)
		if err != nil {
			return nil, err
		}
		width, swap = bits/8, !little && bits > 8
		flip = codec == "ipcm" && bits == 8 // two's complement to WAVE's unsigned
		if codec == "fpcm" {
			tag = waveFormatIEEEFloat
		}
	default:
		return nil, refuseAudioCodec(FormatWAV, cfg, `linear PCM ("ipcm", "fpcm" and "raw ")`)
	}
	blockAlign := cfg.Channels * width
	switch {
	case cfg.Channels < 1 || blockAlign > math.MaxUint16:
		return nil, fmt.Errorf("%w: wav: %d channels of %d bytes", ErrTrackConfig, cfg.Channels, width)
	case cfg.SampleRate < 1 || uint64(cfg.SampleRate) > math.MaxUint32:
		return nil, fmt.Errorf("%w: wav: a sample rate of %d Hz", ErrTrackConfig, cfg.SampleRate)
	}
	s := &wavSink{out: a.out, rf64: a.set.rf64, blockAlign: blockAlign, width: width, swap: swap, flip: flip}
	s.seeker, _ = a.w.(io.WriteSeeker)
	if s.seeker != nil {
		var err error
		if s.base, err = s.seeker.Seek(0, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("container: write wav: %w", err)
		}
	}
	s.fmtChunk = wavFormatChunk(cfg, tag, width)

	hdr := []byte("RIFF")
	if s.rf64 && s.seeker == nil {
		hdr = []byte("RF64")
	}
	hdr = binary.LittleEndian.AppendUint32(hdr, riffSizeUnknown)
	hdr = append(hdr, "WAVE"...)
	if s.rf64 && s.seeker == nil {
		// An empty ds64 chunk: the sizes are unknown, which readers take as
		// "to the end of the file".
		hdr = binary.LittleEndian.AppendUint32(append(hdr, "ds64"...), 28)
		hdr = append(hdr, make([]byte, 28)...)
	}
	s.fmtAt = int64(len(hdr))
	hdr = append(hdr, s.fmtChunk...)
	if s.seeker != nil {
		// Room for the ds64 chunk an RF64 file needs.
		hdr = binary.LittleEndian.AppendUint32(append(hdr, "JUNK"...), 28)
		hdr = append(hdr, make([]byte, 28)...)
		if tag == waveFormatIEEEFloat {
			hdr = binary.LittleEndian.AppendUint32(append(hdr, "fact"...), 4)
			s.factAt = int64(len(hdr))
			hdr = append(hdr, 0, 0, 0, 0)
		}
	}
	hdr = binary.LittleEndian.AppendUint32(append(hdr, "data"...), riffSizeUnknown)
	s.dataAt = int64(len(hdr)) - 4
	a.out.Write(hdr)
	return s, nil
}

// wavPCMC reads the pcmC record of an ipcm or fpcm entry (ISO/IEC 23003-5
// §5.1): version and flags, then format_flags, whose low bit says
// little-endian, then the sample size in bits.
func wavPCMC(codec string, cfg TrackConfig) (little bool, bits int, err error) {
	if len(cfg.CodecConfig) < 6 {
		return false, 0, fmt.Errorf("%w: %s needs its pcmC record as CodecConfig (the sample size)", ErrTrackConfig, codec)
	}
	little, bits = cfg.CodecConfig[4]&1 != 0, int(cfg.CodecConfig[5])
	ok := bits == 8 || bits == 16 || bits == 24 || bits == 32
	if codec == "fpcm" {
		ok = bits == 32 || bits == 64
	}
	if !ok {
		return false, 0, fmt.Errorf("%w: %s samples of %d bits cannot be written as WAVE", ErrTrackConfig, codec, bits)
	}
	return little, bits, nil
}

// wavFormatChunk is the "fmt " chunk, header included.
func wavFormatChunk(cfg TrackConfig, tag uint16, width int) []byte {
	bits := width * 8
	blockAlign := cfg.Channels * width
	extensible := cfg.Channels > 2 || cfg.SampleRate > 48000 || (bits > 16 && tag != waveFormatIEEEFloat)
	formatTag := tag
	if extensible {
		formatTag = waveFormatExtensible
	}
	b := binary.LittleEndian.AppendUint16(nil, formatTag)
	b = binary.LittleEndian.AppendUint16(b, uint16(cfg.Channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(cfg.SampleRate))
	b = binary.LittleEndian.AppendUint32(b, uint32(cfg.SampleRate)*uint32(blockAlign)) // wraps, as a 32-bit field does
	b = binary.LittleEndian.AppendUint16(b, uint16(blockAlign))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	switch {
	case extensible:
		var mask uint32
		if cfg.Channels < len(wavChannelMask) {
			mask = wavChannelMask[cfg.Channels]
		}
		b = binary.LittleEndian.AppendUint16(b, 22) // cbSize
		b = binary.LittleEndian.AppendUint16(b, uint16(bits))
		b = binary.LittleEndian.AppendUint32(b, mask)
		b = binary.LittleEndian.AppendUint16(b, tag)
		b = append(b, waveSubFormatTail...)
	case tag != 1:
		b = binary.LittleEndian.AppendUint16(b, 0) // cbSize
	}
	chunk := binary.LittleEndian.AppendUint32([]byte("fmt "), uint32(len(b)))
	return append(chunk, b...)
}

func (s *wavSink) sample(smp Sample) error {
	data := smp.Data
	if len(data)%s.blockAlign != 0 {
		return fmt.Errorf("%w: %d bytes are not a whole number of %d-byte sample frames",
			ErrSample, len(data), s.blockAlign)
	}
	if s.swap || s.flip {
		data = append([]byte(nil), data...)
		for i := 0; i+s.width <= len(data); i += s.width {
			if s.swap {
				for l, r := i, i+s.width-1; l < r; l, r = l+1, r-1 {
					data[l], data[r] = data[r], data[l]
				}
			}
			if s.flip {
				data[i] ^= 0x80
			}
		}
	}
	s.out.Write(data)
	s.written += uint64(len(data))
	return nil
}

// finish pads the data chunk to an even length, and states the sizes when the
// output can seek back to them.
func (s *wavSink) finish() {
	if s.written&1 != 0 {
		s.out.Write([]byte{0}) // RIFF chunks are padded; the pad is not counted
	}
	if s.seeker == nil || s.out.err != nil {
		return
	}
	total := s.out.n
	riff := uint64(total) - 8
	samples := s.written / uint64(s.blockAlign)
	if !s.rf64 && riff <= wavRIFFMax {
		s.patch(4, binary.LittleEndian.AppendUint32(nil, uint32(riff)))
		s.patch(s.dataAt, binary.LittleEndian.AppendUint32(nil, uint32(s.written)))
		if s.factAt != 0 {
			s.patch(s.factAt, binary.LittleEndian.AppendUint32(nil, uint32(samples)))
		}
	} else {
		s.patch(0, binary.LittleEndian.AppendUint32([]byte("RF64"), riffSizeUnknown))
		ds64 := binary.LittleEndian.AppendUint32([]byte("ds64"), 28)
		ds64 = binary.LittleEndian.AppendUint64(ds64, riff)
		ds64 = binary.LittleEndian.AppendUint64(ds64, s.written)
		ds64 = binary.LittleEndian.AppendUint64(ds64, samples)
		ds64 = binary.LittleEndian.AppendUint32(ds64, 0) // no table of other chunks
		s.patch(s.fmtAt, append(ds64, s.fmtChunk...))
		unknown := binary.LittleEndian.AppendUint32(nil, riffSizeUnknown)
		s.patch(s.dataAt, unknown)
		if s.factAt != 0 {
			s.patch(s.factAt, unknown)
		}
	}
	s.patch(total, nil)
}

// patch writes p at an offset of the file and leaves the position there; a
// failure is kept as the writer's error.
func (s *wavSink) patch(at int64, p []byte) {
	if s.out.err != nil {
		return
	}
	if _, err := s.seeker.Seek(s.base+at, io.SeekStart); err != nil {
		s.out.err = err
		return
	}
	if _, err := s.seeker.Write(p); err != nil {
		s.out.err = err
	}
}

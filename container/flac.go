// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"math/bits"
)

// A native FLAC file is RFC 9639 §6: the marker "fLaC", metadata blocks of which
// the first is STREAMINFO, then frames. STREAMINFO is the configuration a
// container carries -- Matroska's A_FLAC CodecPrivate and the dfLa box of
// FLAC-in-ISOBMFF both hold it -- and the frames are the samples.
//
// No length field anywhere says where a frame ends. FFmpeg's flac_parser.c
// finds the end by looking for the next frame header and checking the CRCs
// either side of it, and so does this: a candidate is taken only where the
// header's own CRC-8 holds (§9.1.8), the frame before it closes with a CRC-16
// over all of its bytes (§9.3), and the frame or sample number it states follows
// on from the previous one. A sync pattern in the middle of a frame clears the
// first test now and then; it clears all three essentially never.
//
// Header and metadata layouts are read against FFmpeg's libavcodec/flac.c and
// libavformat/flacdec.c (2026-10-18). Only headers are read: no subframe is
// decoded, and the MD5 signature of the audio is not checked.

// FormatFLAC is a native FLAC file.
const FormatFLAC Format = 6

func init() {
	registerElementary(elementaryFormat{
		format: FormatFLAC, name: "flac", order: orderMagic,
		sniff: sniffFLAC, read: readFLAC,
	})
}

// flacMarker opens a native FLAC stream. It is the signature that opens the
// CodecConfig of a FLAC track as well, and the metadata constants
// (flacStreamInfo, flacStreamInfoSize, flacBlockHeaderSize) are declared once,
// with the configuration parser, in audiocodecs.go.
const flacMarker = flacSignature

// flacInvalidBlock is reserved by RFC 9639 §8.1 so that a block header cannot
// be mistaken for a frame sync code.
const flacInvalidBlock = flacForbiddenBlock

// sniffFLAC recognises the marker, behind an ID3v2 tag if one leads.
func sniffFLAC(data []byte) bool {
	i := id3v2Length(data)
	return i+len(flacMarker) <= len(data) && string(data[i:i+len(flacMarker)]) == flacMarker
}

// flacInfo is what STREAMINFO states, as far as a container needs it.
type flacInfo struct {
	sampleRate   int
	channels     int
	bitsPerSamp  int
	totalSamples uint64
	// block is the STREAMINFO block, header included, with its last-block
	// flag set: the form a container carries it in once the other blocks are
	// left behind.
	block []byte
}

// parseFLACStreamInfo reads a STREAMINFO body (RFC 9639 §8.2).
func parseFLACStreamInfo(body []byte) (flacInfo, error) {
	if len(body) != flacStreamInfoSize {
		return flacInfo{}, errorf(ErrMalformed, "flac: STREAMINFO is %d bytes, not %d",
			len(body), flacStreamInfoSize)
	}
	packed := binary.BigEndian.Uint64(body[10:18])
	info := flacInfo{
		sampleRate:   int(packed >> 44),
		channels:     int(packed>>41&7) + 1,
		bitsPerSamp:  int(packed>>36&0x1F) + 1,
		totalSamples: packed & (1<<36 - 1),
	}
	if info.sampleRate == 0 {
		return flacInfo{}, errorf(ErrMalformed, "flac: STREAMINFO states a sample rate of 0, which audio cannot have")
	}
	info.block = append([]byte{0x80 | flacStreamInfo, 0, 0, flacStreamInfoSize}, body...)
	return info, nil
}

// flacCodecConfig is what TrackConfig.CodecConfig holds for a FLAC track: the
// marker and STREAMINFO, which is Matroska's A_FLAC CodecPrivate as its
// specification lays it out, and which the dfLa box of ISO-BMFF holds after its
// version and flags, without the marker.
//
// The other metadata blocks are left behind: a SEEKTABLE states byte offsets
// into a file that is about to stop existing, and a PICTURE or VORBIS_COMMENT
// is metadata rather than configuration.
func flacCodecConfig(info flacInfo) []byte {
	return append([]byte(flacMarker), info.block...)
}

// readFLAC reads a native FLAC file.
func readFLAC(data []byte) ([]*mkvReadTrack, error) {
	at := id3v2Length(data) + len(flacMarker)
	var info flacInfo
	for last, n := false, 0; !last; n++ {
		if at+flacBlockHeaderSize > len(data) {
			return nil, truncatedAt("flac", len(data)-at, flacBlockHeaderSize)
		}
		header := data[at:]
		last = header[0]&0x80 != 0
		kind := header[0] & 0x7F
		size := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		body := at + flacBlockHeaderSize
		if body+size > len(data) {
			return nil, truncatedAt("flac", len(data)-body, size)
		}
		switch {
		case kind == flacInvalidBlock:
			return nil, errorf(ErrMalformed, "flac: metadata block %d is of the reserved type 127", n)
		case (n == 0) != (kind == flacStreamInfo):
			// §8.1: STREAMINFO is the first block, and there is one.
			return nil, errorf(ErrMalformed, "flac: metadata block %d is of type %d; STREAMINFO is the first and only the first",
				n, kind)
		case n == 0:
			var err error
			if info, err = parseFLACStreamInfo(data[body : body+size]); err != nil {
				return nil, err
			}
		}
		at = body + size
	}
	t := flacTrack(info)
	frames, err := flacFrames(data, at, tailTagsStart(data))
	for _, f := range frames {
		t.samples = append(t.samples, Sample{Data: f.data, Duration: uint32(f.header.blockSize), Sync: true})
	}
	return oneTrack(t, err)
}

// flacTrack is the record of a FLAC track, before its frames.
func flacTrack(info flacInfo) *mkvReadTrack {
	t := audioTrack("fLaC", uint32(info.sampleRate), info.channels, info.sampleRate)
	t.config.CodecConfig = flacCodecConfig(info)
	return t
}

// flacFrameHeader is what a frame header states (RFC 9639 §9.1).
type flacFrameHeader struct {
	variable  bool   // blocking strategy: the number is a sample number
	number    uint64 // the frame number, or the first sample's number
	blockSize int
	size      int // the header's length, CRC-8 included
}

// flacBlockSizes is §9.1.1's table; 0 is reserved and 6 and 7 are stated
// after the coded number.
var flacBlockSizes = [16]int{0, 192, 576, 1152, 2304, 4608, 0, 0,
	256, 512, 1024, 2048, 4096, 8192, 16384, 32768}

// parseFLACFrameHeader reads the frame header at the start of data and checks
// its CRC-8, refusing every value §9.1 reserves.
func parseFLACFrameHeader(data []byte) (flacFrameHeader, bool) {
	var h flacFrameHeader
	if len(data) < 6 || data[0] != 0xFF || data[1]&0xFE != 0xF8 {
		return h, false
	}
	h.variable = data[1]&1 != 0
	sizeCode, rateCode := data[2]>>4, data[2]&0xF
	channels, depth := data[3]>>4, data[3]>>1&7
	if sizeCode == 0 || rateCode == 15 || channels > 10 || depth == 3 || data[3]&1 != 0 {
		return h, false
	}
	number, width, ok := flacCodedNumber(data[4:])
	if !ok {
		return h, false
	}
	h.number = number
	at := 4 + width
	h.blockSize = flacBlockSizes[sizeCode]
	switch sizeCode {
	case 6:
		if at+1 > len(data) {
			return h, false
		}
		h.blockSize = int(data[at]) + 1
		at++
	case 7:
		if at+2 > len(data) {
			return h, false
		}
		h.blockSize = int(binary.BigEndian.Uint16(data[at:])) + 1
		at += 2
	}
	switch rateCode {
	case 12:
		at++
	case 13, 14:
		at += 2
	}
	if at+1 > len(data) || crc8(data[:at+1]) != 0 {
		return h, false
	}
	h.size = at + 1
	return h, true
}

// flacCodedNumber reads the UTF-8-like coded number of §9.1.5: up to 31 bits
// in six bytes for a frame number, up to 36 bits in seven for a sample number.
func flacCodedNumber(data []byte) (uint64, int, bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	lead := data[0]
	if lead < 0x80 {
		return uint64(lead), 1, true
	}
	// The leading ones count the bytes. A single one marks a continuation
	// byte, which cannot lead, and eight would be a width the format does not
	// have.
	n := bits.LeadingZeros8(^lead)
	if n < 2 || n > 7 || len(data) < n {
		return 0, 0, false
	}
	value := uint64(lead & (0x7F >> n))
	for _, b := range data[1:n] {
		if b&0xC0 != 0x80 {
			return 0, 0, false
		}
		value = value<<6 | uint64(b&0x3F)
	}
	return value, n, true
}

// flacFrame is one frame of a native stream.
type flacFrame struct {
	header flacFrameHeader
	data   []byte
}

// flacFrames cuts data[from:end] into frames. A frame ends where the next one
// begins, and the next one begins at a header whose CRC-8 holds, whose number
// follows on, and before which the CRC-16 over everything since the previous
// header comes to zero: §9.3's footer is a CRC that makes it so.
//
// The last frame ends at end, and if its CRC-16 does not hold there it was cut
// short, and is dropped and reported.
func flacFrames(data []byte, from, end int) ([]flacFrame, error) {
	if from >= end {
		return nil, nil
	}
	head, ok := parseFLACFrameHeader(data[from:end])
	if !ok {
		return nil, errorf(ErrMalformed, "flac: no frame header at byte %d, where the metadata ends", from)
	}
	var frames []flacFrame
	start := from
	crc := uint16(0)
	for j := start; ; j++ {
		if j == end {
			if crc != 0 {
				return frames, truncatedAt("flac", end-start, 1)
			}
			return append(frames, flacFrame{head, data[start:end]}), nil
		}
		// A frame is at least its header and its footer.
		if crc == 0 && j >= start+head.size+2 && data[j] == 0xFF {
			if next, ok := parseFLACFrameHeader(data[j:end]); ok && follows(head, next) {
				frames = append(frames, flacFrame{head, data[start:j]})
				start, head = j, next
			}
		}
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^data[j]]
	}
}

// follows reports whether next is the frame after h: the same blocking strategy,
// and the frame number one more or the sample number one block further.
func follows(h, next flacFrameHeader) bool {
	if h.variable != next.variable {
		return false
	}
	if h.variable {
		return next.number == h.number+uint64(h.blockSize)
	}
	return next.number == h.number+1
}

// The two CRCs of a FLAC frame (RFC 9639 §9.1.8 and §9.3): CRC-8 with the
// polynomial x^8+x^2+x+1 and CRC-16 with x^16+x^15+x^2+1, both initialised to
// zero, unreflected, with no final XOR.
var (
	crc8Table  = crcTable8(0x07)
	crc16Table = crcTable16(0x8005)
)

func crcTable8(poly byte) (t [256]byte) {
	for i := range t {
		c := byte(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ poly
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

func crcTable16(poly uint16) (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ poly
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

func crc8(data []byte) byte {
	var c byte
	for _, b := range data {
		c = crc8Table[c^b]
	}
	return c
}

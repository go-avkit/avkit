// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// flacNumber codes n the way RFC 9639 §9.1.5 does, in as few bytes as it fits.
func flacNumber(n uint64) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	for width := 2; width <= 7; width++ {
		if n < 1<<(5*width+1) || width == 7 {
			out := make([]byte, width)
			for i := width - 1; i > 0; i-- {
				out[i] = 0x80 | byte(n&0x3F)
				n >>= 6
			}
			out[0] = ^byte(0xFF>>width) | byte(n)
			return out
		}
	}
	return nil
}

// flacHeaderBytes is a frame header with its CRC-8: sizeCode and rateCode are
// the §9.1.1 and §9.1.2 codes, and extra the uncommon block size and rate
// bytes they call for.
func flacHeaderBytes(variable bool, sizeCode, rateCode byte, number uint64, extra ...byte) []byte {
	second := byte(0xF8)
	if variable {
		second = 0xF9
	}
	b := []byte{0xFF, second, sizeCode<<4 | rateCode, 0x00 | 4<<1}
	b = append(b, flacNumber(number)...)
	b = append(b, extra...)
	return append(b, crc8(b))
}

// flacFrameBytes closes a frame with the CRC-16 of §9.3.
func flacFrameBytes(header, body []byte) []byte {
	frame := append(append([]byte(nil), header...), body...)
	var crc uint16
	for _, b := range frame {
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^b]
	}
	return binary.BigEndian.AppendUint16(frame, crc)
}

// streamInfo is a STREAMINFO body.
func streamInfo(rate, channels, bits int, total uint64) []byte {
	b := make([]byte, flacStreamInfoSize)
	binary.BigEndian.PutUint16(b[0:], 4096)
	binary.BigEndian.PutUint16(b[2:], 4096)
	binary.BigEndian.PutUint64(b[10:], uint64(rate)<<44|uint64(channels-1)<<41|uint64(bits-1)<<36|total)
	return b
}

// flacBlock is a metadata block.
func flacBlock(last bool, kind byte, body []byte) []byte {
	if last {
		kind |= 0x80
	}
	n := len(body)
	return append([]byte{kind, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

// TestANativeFLACStreamIsCutAtTheHeadersWhoseCRCsHold. The second frame's body
// carries a whole valid frame header -- sync code, a number that follows, a
// CRC-8 that holds -- and is not cut there, because the CRC-16 of the frame
// before it does not.
func TestANativeFLACStreamIsCutAtTheHeadersWhoseCRCsHold(t *testing.T) {
	decoy := flacHeaderBytes(false, 12, 4, 2)
	frames := [][]byte{
		flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), bytes.Repeat([]byte{0x55}, 40)),
		flacFrameBytes(flacHeaderBytes(false, 12, 4, 1), concat([]byte{1, 2, 3}, decoy, []byte{4, 5, 6})),
		flacFrameBytes(flacHeaderBytes(false, 6, 13, 2, 99, 0x1F, 0x40), []byte{7, 8, 9}),
	}
	data := concat(id3v2Tag(12, false), []byte(flacMarker),
		flacBlock(false, flacStreamInfo, streamInfo(8000, 2, 16, 4096*2+100)),
		flacBlock(true, 4, []byte("comment")),
		frames[0], frames[1], frames[2])
	if Sniff(data) != FormatFLAC {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 3 {
		t.Fatalf("%d frames, want 3", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, frames[i]) {
			t.Errorf("frame %d: %d bytes, want %d", i, len(s.Data), len(frames[i]))
		}
	}
	if samples[0].Duration != 4096 || samples[2].Duration != 100 {
		t.Errorf("durations %d and %d, want 4096 and 100", samples[0].Duration, samples[2].Duration)
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Codec != "fLaC" || cfg.SampleRate != 8000 || cfg.Channels != 2 ||
		!bytes.Equal(cfg.CodecConfig[:8], []byte("fLaC\x80\x00\x00\x22")) {
		t.Errorf("configuration %+v", cfg)
	}
}

// TestAFLACFrameIsNotCutAtAHeaderThatDoesNotFollowOn: inside the first frame
// sits a point where the CRC-16 comes to zero -- a whole frame's worth of bytes
// and its footer -- followed by a header whose CRC-8 holds. Both CRCs agree it
// could be a boundary; only the frame number, 7 where 1 is next, says it is not.
func TestAFLACFrameIsNotCutAtAHeaderThatDoesNotFollowOn(t *testing.T) {
	inner := flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), []byte{1, 2, 3})
	first := flacFrameBytes(inner, concat(flacHeaderBytes(false, 12, 4, 7), []byte{4, 5}))
	second := flacFrameBytes(flacHeaderBytes(false, 12, 4, 1), []byte{6})
	data := concat([]byte(flacMarker), flacBlock(true, flacStreamInfo, streamInfo(8000, 1, 16, 0)), first, second)
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 2 || !bytes.Equal(samples[0].Data, first) {
		t.Errorf("%d frames, the first %d bytes; want 2, the first %d", len(samples), len(samples[0].Data), len(first))
	}
}

func TestAVariableBlockSizeFLACStreamIsNumberedBySample(t *testing.T) {
	frames := [][]byte{
		flacFrameBytes(flacHeaderBytes(true, 7, 0, 0, 0x01, 0x00), []byte{1}),       // 257 samples
		flacFrameBytes(flacHeaderBytes(true, 1, 0, 257), []byte{2}),                 // 192
		flacFrameBytes(flacHeaderBytes(true, 8, 14, 449, 0x0F, 0xA0), []byte{3, 4}), // 256
	}
	data := concat([]byte(flacMarker), flacBlock(true, flacStreamInfo, streamInfo(44100, 1, 16, 0)),
		frames[0], frames[1], frames[2])
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 3 || samples[0].Duration != 257 || samples[1].Duration != 192 || samples[2].Duration != 256 {
		t.Errorf("%d frames: %+v", len(samples), samples)
	}
}

func TestAFLACFrameCutShortIsDroppedAndReported(t *testing.T) {
	frame := flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), bytes.Repeat([]byte{0x55}, 40))
	next := flacFrameBytes(flacHeaderBytes(false, 12, 4, 1), bytes.Repeat([]byte{0x66}, 40))
	data := concat([]byte(flacMarker), flacBlock(true, flacStreamInfo, streamInfo(8000, 1, 16, 0)),
		frame, next[:30])
	r, err := NewReader(data)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("NewReader: %v", err)
	}
	if samples, _ := r.Samples(1); len(samples) != 1 {
		t.Errorf("%d frames kept, want 1", len(samples))
	}
}

func TestAFLACFileWithNoFrameHasNoSamples(t *testing.T) {
	data := concat([]byte(flacMarker), flacBlock(true, flacStreamInfo, streamInfo(8000, 1, 16, 0)))
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrNoSamples) {
		t.Errorf("Samples: %v, want ErrNoSamples", err)
	}
}

func TestAFLACFileThatBreaksRFC9639IsRefused(t *testing.T) {
	info := flacBlock(true, flacStreamInfo, streamInfo(8000, 1, 16, 0))
	frame := flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), []byte{1, 2, 3})
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a block header cut":        {concat([]byte(flacMarker), info[:2]), ErrTruncated},
		"a block body cut":          {concat([]byte(flacMarker), info[:20]), ErrTruncated},
		"the reserved block type":   {concat([]byte(flacMarker), flacBlock(true, 127, nil)), ErrMalformed},
		"no STREAMINFO first":       {concat([]byte(flacMarker), flacBlock(true, 4, nil)), ErrMalformed},
		"a second STREAMINFO":       {concat([]byte(flacMarker), flacBlock(false, 0, streamInfo(8000, 1, 16, 0)), info), ErrMalformed},
		"a STREAMINFO of 33 bytes":  {concat([]byte(flacMarker), flacBlock(true, 0, make([]byte, 33))), ErrMalformed},
		"a sample rate of 0":        {concat([]byte(flacMarker), flacBlock(true, 0, streamInfo(0, 1, 16, 0))), ErrMalformed},
		"no frame after the blocks": {concat([]byte(flacMarker), info, []byte{1, 2, 3, 4, 5, 6, 7}), ErrMalformed},
		"a frame followed by junk":  {concat([]byte(flacMarker), info, frame, []byte{9}), ErrTruncated},
	} {
		_, err := NewReader(c.data)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestAFLACFrameHeaderRefusesWhatRFC9639Reserves(t *testing.T) {
	good := flacHeaderBytes(false, 12, 4, 0)
	if h, ok := parseFLACFrameHeader(good); !ok || h.blockSize != 4096 || h.size != len(good) {
		t.Fatalf("a good header: %+v %t", h, ok)
	}
	for _, c := range []struct {
		rateCode byte
		extra    []byte
	}{{12, []byte{44}}, {13, []byte{0x1F, 0x40}}, {14, []byte{0x03, 0x20}}} {
		h := flacHeaderBytes(false, 12, c.rateCode, 0, c.extra...)
		if parsed, ok := parseFLACFrameHeader(h); !ok || parsed.size != len(h) {
			t.Errorf("rate code %d: %+v %t", c.rateCode, parsed, ok)
		}
	}
	withCRC := func(b []byte) []byte { return append(b, crc8(b)) }
	for name, b := range map[string][]byte{
		"too short":                 good[:5],
		"no sync":                   withCRC([]byte{0xFF, 0xF0, 0xC4, 0x08, 0x00}),
		"block size code 0":         withCRC([]byte{0xFF, 0xF8, 0x04, 0x08, 0x00}),
		"sample rate code 15":       withCRC([]byte{0xFF, 0xF8, 0xCF, 0x08, 0x00}),
		"channel code 11":           withCRC([]byte{0xFF, 0xF8, 0xC4, 0xB8, 0x00}),
		"bit depth code 3":          withCRC([]byte{0xFF, 0xF8, 0xC4, 0x06, 0x00}),
		"the reserved bit":          withCRC([]byte{0xFF, 0xF8, 0xC4, 0x09, 0x00}),
		"a broken number":           withCRC([]byte{0xFF, 0xF8, 0xC4, 0x08, 0x80}),
		"an 8-bit size cut":         {0xFF, 0xF8, 0x64, 0x08, 0xC2, 0x80},
		"a 16-bit size cut":         {0xFF, 0xF8, 0x74, 0x08, 0xC2, 0x80, 0x01},
		"a CRC that is wrong":       append(good[:len(good)-1:len(good)-1], good[len(good)-1]^1),
		"the CRC is not there":      []byte{0xFF, 0xF8, 0xC4, 0x08, 0x00, 0x00}[:5],
		"a rate cut before its CRC": {0xFF, 0xF8, 0xCD, 0x08, 0x00, 0x1F},
	} {
		if _, ok := parseFLACFrameHeader(b); ok {
			t.Errorf("%s: read as a header", name)
		}
	}
}

func TestAFLACCodedNumberIsReadAsRFC9639Codes(t *testing.T) {
	for _, n := range []uint64{0, 0x7F, 0x80, 0x7FF, 0x800, 1 << 20, 1<<31 - 1, 1<<36 - 1} {
		coded := flacNumber(n)
		got, width, ok := flacCodedNumber(coded)
		if !ok || got != n || width != len(coded) {
			t.Errorf("%d coded as % x reads %d in %d bytes (%t)", n, coded, got, width, ok)
		}
	}
	for name, b := range map[string][]byte{
		"nothing":               nil,
		"a continuation byte":   {0x80},
		"eight leading ones":    {0xFF, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80},
		"cut short":             {0xE0, 0x80},
		"a broken continuation": {0xC2, 0x40},
	} {
		if _, _, ok := flacCodedNumber(b); ok {
			t.Errorf("%s: read as a number", name)
		}
	}
}

func TestAFLACFrameFollowsOnlyTheOneBeforeIt(t *testing.T) {
	fixed := flacFrameHeader{number: 4, blockSize: 4096}
	variable := flacFrameHeader{variable: true, number: 4096, blockSize: 4096}
	for _, c := range []struct {
		h, next flacFrameHeader
		want    bool
	}{
		{fixed, flacFrameHeader{number: 5}, true},
		{fixed, flacFrameHeader{number: 6}, false},
		{variable, flacFrameHeader{variable: true, number: 8192}, true},
		{variable, flacFrameHeader{variable: true, number: 8191}, false},
		{fixed, flacFrameHeader{variable: true, number: 5}, false},
	} {
		if got := follows(c.h, c.next); got != c.want {
			t.Errorf("%+v then %+v: %t, want %t", c.h, c.next, got, c.want)
		}
	}
}

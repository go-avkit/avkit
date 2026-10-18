// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/av1"
)

// AV1 in a transport stream follows the AOM "Carriage of AV1 in MPEG-2 TS"
// specification (Working Group Draft, 25 March 2026,
// https://aomediacodec.github.io/av1-mpeg2-ts/). It is a draft, and says so;
// it is also the only text that defines the carriage, and FFmpeg neither
// writes nor reads AV1 in a transport stream at all, so there is no other
// convention to follow.
//
// It does not carry an MP4 sample as it stands. Each OBU is put behind a
// three-byte start code, as an H.264 NAL unit is, and escaped so the start
// code cannot occur inside it (3.2). An MP4 sample is the low-overhead form,
// where each OBU states its own size (AV1-ISOBMFF 2.4), so the two directions
// below convert between them.

// av1VideoDescriptorTag is the tag of the AV1 video descriptor (2.2), which
// follows the registration descriptor in a stream's descriptor loop.
const av1VideoDescriptorTag = 0x80

// av1TemporalDelimiter is the OBU type a temporal unit may begin with. A
// transport stream may carry it or not (3.1); an MP4 sample SHOULD NOT
// (AV1-ISOBMFF 2.4), so reading drops it.
const av1TemporalDelimiter = 2

// av1VideoDescriptor is the payload of the AV1 video descriptor, built from the
// av1C record the track is configured with: the specification reuses the
// record's first four bytes "to aid conversion between the two formats" (2.2),
// changing only what its last byte holds.
func av1VideoDescriptor(av1C []byte) []byte {
	return []byte{
		0x81, // marker 1, version 1
		av1C[1],
		av1C[2],
		// hdr_wcg_idc is 3, "no indication": an av1C record does not state
		// whether the stream is HDR, and saying SDR would be a claim. The
		// initial presentation delay is not stated, being ignored in a
		// transport stream (2.2.2).
		0xC0,
	}
}

// av1ToStartCodes turns an MP4 sample into the payload a transport stream
// carries: every OBU behind a start code, escaped.
func av1ToStartCodes(sample []byte) ([]byte, error) {
	var out bytes.Buffer
	for pos := 0; pos < len(sample); {
		obu, n, err := av1SizedOBU(sample[pos:])
		if err != nil {
			return nil, fmt.Errorf("%w: AV1 OBU at byte %d: %v", ErrSample, pos, err)
		}
		out.Write([]byte{0, 0, 1})
		av1Escape(&out, obu)
		pos += n
	}
	return out.Bytes(), nil
}

// av1SizedOBU returns the OBU at the head of data in the form that states its
// size, and how many bytes of data it took.
//
// An OBU that already states its size is returned as it stands, so a round
// trip gives back the sample's own bytes. Only the last OBU of an MP4 sample
// may leave its size out (AV1-ISOBMFF 2.4): it fills the rest of the sample,
// and is given the field here, because the start code that follows it in a
// transport stream is no reliable end — a payload may itself end in zero
// bytes.
func av1SizedOBU(data []byte) ([]byte, int, error) {
	hdr, err := av1.ParseOBUHeader(data)
	if err != nil {
		return nil, 0, err
	}
	if !hdr.HasSizeField {
		obu := av1.OBU{Header: hdr, Payload: data[hdr.HeaderSize:]}
		return obu.Encode(), len(data), nil
	}
	size, n, err := av1.ReadLEB128(data[hdr.HeaderSize:])
	if err != nil {
		return nil, 0, err
	}
	end := hdr.HeaderSize + n
	// Compared before converting, since an int may be 32 bits wide.
	if size > uint64(len(data)-end) {
		return nil, 0, fmt.Errorf("a payload of %d bytes where %d remain", size, len(data)-end)
	}
	end += int(size)
	return data[:end], end, nil
}

// av1Escape writes an OBU with an emulation_prevention_three_byte after every
// two zero bytes that a byte of 0 to 3 follows (3.2): the start code 0x000001
// can then never occur inside it, nor 0x000000 or 0x000002.
//
// ⛔ The draft does not say what happens at the END of an OBU, where the zero
// bytes it ends in meet the next start code and form 0x000000 across the
// boundary. A unit ending in zero is closed with 0x03 here, as H.264 closes an
// RBSP (ITU-T H.264 7.4.1). A reader that honours obu_size, as this package's
// does, never reads that byte: it lies after the OBU's stated end.
func av1Escape(w *bytes.Buffer, obu []byte) {
	zeros := 0
	for _, b := range obu {
		if zeros >= 2 && b <= 3 {
			w.WriteByte(3)
			zeros = 0
		}
		w.WriteByte(b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	if zeros > 0 {
		w.WriteByte(3)
	}
}

// av1FromStartCodes turns a transport stream payload in the start-code form
// back into an MP4 sample. Temporal delimiters are dropped, and an OBU that
// states no size is given one, so the sample is in the form AV1-ISOBMFF 2.4
// asks for.
func av1FromStartCodes(payload []byte) ([]byte, error) {
	var out []byte
	for i, unit := range splitStartCodes(payload) {
		obu, _, err := av1SizedOBU(av1Unescape(unit))
		if err != nil {
			return nil, fmt.Errorf("%w: AV1 OBU %d: %v", ErrSample, i, err)
		}
		if obu[0]>>3&0x0F == av1TemporalDelimiter {
			continue
		}
		out = append(out, obu...)
	}
	return out, nil
}

// splitStartCodes cuts a payload on its three-byte start codes, keeping what
// lies between them as it stands. Unlike splitAnnexB it does not trim the zero
// bytes a unit ends in: an AV1 payload may end in them, and the OBU's own size
// says where it stops.
func splitStartCodes(data []byte) [][]byte {
	var (
		out   [][]byte
		start = -1
	)
	for i := 0; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		if start >= 0 {
			out = append(out, data[start:i])
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

// av1Unescape removes the emulation_prevention_three_byte the writer put after
// every two zero bytes (3.2).
func av1Unescape(unit []byte) []byte {
	out := make([]byte, 0, len(unit))
	zeros := 0
	for _, b := range unit {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

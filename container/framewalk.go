// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
)

// An elementary stream of sync frames -- ADTS, MPEG audio -- has no container
// around it: the frames follow one another, each stating its own length, and
// whatever else is in the file is someone's addition. Three such additions are
// common enough that a reader which refused them would refuse most of a music
// library, and FFmpeg's aacdec.c and mp3dec.c skip all three:
//
//   - an ID3v2 tag at the head (id3v2.4.0-structure §3.1), and sometimes again
//     between two frames, where a stream was cut from a broadcast;
//   - an ID3v1 tag, the last 128 bytes, starting "TAG";
//   - an APEv2 tag at the tail, ending in a 32-byte footer "APETAGEX".
//
// What is between two frames and is none of those is resynchronised over, the
// way every decoder does, but only to a position where a frame is followed by
// another one: a lone sync pattern inside a frame's data is common, and two in
// a row whose lengths agree is not.

// id3v2HeaderSize is the length of an ID3v2 header, and of its footer.
const id3v2HeaderSize = 10

// id3v2Length is how long the ID3v2 tag at the start of data is, header and
// footer included, or 0 when none starts there. The size is a 28-bit
// "synchsafe" integer, seven bits to a byte, so a byte with its high bit set is
// not one and the tag is not taken.
func id3v2Length(data []byte) int {
	if len(data) < id3v2HeaderSize || string(data[:3]) != "ID3" ||
		data[3] == 0xFF || data[4] == 0xFF {
		return 0
	}
	size := 0
	for _, b := range data[6:10] {
		if b&0x80 != 0 {
			return 0
		}
		size = size<<7 | int(b)
	}
	size += id3v2HeaderSize
	if data[5]&0x10 != 0 { // a footer follows the tag
		size += id3v2HeaderSize
	}
	return size
}

// id3v1Size is the fixed length of an ID3v1 tag.
const id3v1Size = 128

// apeFooterSize is the length of an APEv2 tag's footer, and of its header.
const apeFooterSize = 32

// tailTagsStart is where the tags at the end of data begin: before an ID3v1
// tag, and before an APEv2 tag that precedes it or stands alone.
func tailTagsStart(data []byte) int {
	end := len(data)
	if end >= id3v1Size && string(data[end-id3v1Size:end-id3v1Size+3]) == "TAG" {
		end -= id3v1Size
	}
	if end >= apeFooterSize && string(data[end-apeFooterSize:end-apeFooterSize+8]) == "APETAGEX" {
		footer := data[end-apeFooterSize:]
		// The stated size counts the items and the footer, not the header,
		// which bit 31 of the flags says is present.
		size := int64(binary.LittleEndian.Uint32(footer[12:16]))
		if binary.LittleEndian.Uint32(footer[20:24])&(1<<31) != 0 {
			size += apeFooterSize
		}
		if size >= apeFooterSize && size <= int64(end) {
			end -= int(size)
		}
	}
	return end
}

// frameSyntax is what the walker needs of a sync-frame format.
type frameSyntax struct {
	name string
	// length is the length of the frame whose header starts data, or 0 when
	// none does. It is never asked about fewer bytes than headerSize.
	length func(data []byte) int
	// headerSize is how many bytes length needs to see.
	headerSize int
	// sameStream reports whether two frame headers can belong to one stream:
	// a decoder locked to the first will not accept a frame that changes the
	// layer or the sampling rate, and neither does a resynchronisation.
	sameStream func(a, b []byte) bool
	// sync is the first byte of every frame's sync word -- 0xFF for ADTS
	// and MPEG audio, 0x0B for AC-3 -- which is all a resynchronisation
	// stops to look at.
	sync byte
}

// frameAt is the length of a frame at offset i of data[:end] that belongs to
// the stream first is the header of, or 0.
func (s frameSyntax) frameAt(data []byte, i, end int, first []byte) int {
	if end-i < s.headerSize {
		return 0
	}
	n := s.length(data[i:end])
	if n == 0 || (first != nil && !s.sameStream(first, data[i:])) {
		return 0
	}
	return n
}

// startsChain reports whether a frame at i is followed by another frame, or by
// the end of the stream: what tells a real frame from a sync pattern that
// happens to sit inside one.
func (s frameSyntax) startsChain(data []byte, i, end int, first []byte) bool {
	n := s.frameAt(data, i, end, first)
	if n == 0 {
		return false
	}
	next := i + n
	if next >= end {
		return true // the last frame, whole or cut
	}
	if id3v2Length(data[next:end]) > 0 {
		return true
	}
	return s.frameAt(data, next, end, first) > 0
}

// walkFrames cuts data into the frames of one stream. It returns the offset and
// length of each, the header of the first, and ErrTruncated when the last frame
// is cut short, which is dropped.
func (s frameSyntax) walkFrames(data []byte) (frames [][2]int, err error) {
	end := tailTagsStart(data)
	var first []byte
	for i := 0; i < end; {
		if tag := id3v2Length(data[i:end]); tag > 0 {
			i += tag
			continue
		}
		n := s.frameAt(data, i, end, first)
		if n > 0 && (first != nil || s.startsChain(data, i, end, nil)) {
			if i+n > end {
				return frames, truncatedAt(s.name, end-i, i+n-end)
			}
			if first == nil {
				first = data[i : i+s.headerSize]
			}
			frames = append(frames, [2]int{i, n})
			i += n
			continue
		}
		next := s.resync(data, i+1, end, first)
		if next < 0 {
			break // what is left is not this stream: a tag of some other kind, or rubbish
		}
		i = next
	}
	return frames, nil
}

// noFrames is the refusal of a stream in which no whole frame was found: the
// cut, when the only frame there was cut short, or else a malformation.
func noFrames(format string, err error) error {
	if err != nil {
		return err
	}
	return errorf(ErrMalformed, "%s: no frame found", format)
}

// resync is the first offset from i on where a chain of frames starts, or -1.
// Bytes that cannot begin a sync word are passed over without a look.
func (s frameSyntax) resync(data []byte, i, end int, first []byte) int {
	for ; i < end; i++ {
		if data[i] != s.sync {
			if k := bytes.IndexByte(data[i:end], s.sync); k > 0 {
				i += k - 1
			} else if k < 0 {
				return -1
			}
			continue
		}
		if s.startsChain(data, i, end, first) {
			return i
		}
	}
	return -1
}

// sniffFrames reports whether data begins, after an ID3v2 tag and any zero
// padding, with frames of this syntax: three in a row, or as many as there is
// room for when data is shorter than that -- the head of a file a caller has not
// finished reading, where the last frame runs off the end.
//
// A tag that runs past the end of data hides what follows it, and the answer is
// no rather than a guess: an ID3v2 tag precedes ADTS, MPEG audio and FLAC alike.
func (s frameSyntax) sniffFrames(data []byte) bool {
	i := id3v2Length(data)
	if i >= len(data) {
		return false
	}
	for i < len(data) && data[i] == 0 {
		i++
	}
	var first []byte
	for frames := 0; ; frames++ {
		if frames == 3 || (frames > 0 && i >= len(data)) {
			return true
		}
		if frames > 0 {
			i += id3v2Length(data[i:]) // a tag between two frames, as walkFrames allows
		}
		n := s.frameAt(data, i, len(data), first)
		if n == 0 {
			return false
		}
		if first == nil {
			first = data[i:]
		}
		i += n
	}
}

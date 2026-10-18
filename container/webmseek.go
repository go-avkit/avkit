// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"fmt"
	"io"

	"github.com/at-wat/ebml-go"
)

// The SeekHead of a Matroska segment (RFC 9559 §5.1.1, §6.3) says where each
// Top-Level Element is, so that a player opening the file finds the Cues
// without walking the clusters to reach them. It is written first in the
// segment, followed by a Void element (§25.2), as ffmpeg's matroskaenc writes
// it: the room the Void takes lets the SeekHead grow in place, by this muxer
// when the Cues become known and by an editor adding Tags or Chapters later.

// webmSeekHead is the SeekHead element.
type webmSeekHead struct {
	Seek []webmSeek `ebml:"Seek"`
}

// webmSeek is one entry. SeekPosition is written in eight bytes whatever its
// value, as ebml-go's own mkvcore writes it, so that an entry is the same size
// before and after its position is known and the SeekHead can be rewritten in
// place without moving what follows it.
type webmSeek struct {
	SeekID       []byte `ebml:"SeekID"`
	SeekPosition uint64 `ebml:"SeekPosition,size=8"`
}

// webmSeekEntrySize is how long one entry is: the Seek element's two-byte
// identifier and one-byte size, a SeekID of two bytes, one of size and the four
// bytes of a Top-Level Element's identifier (§6.1 makes every one four bytes
// long), and a SeekPosition of two, one and eight.
const webmSeekEntrySize = 3 + (3 + 4) + (3 + 8)

// webmSeekSlots is how many entries the room after the SeekHead holds at
// least: ffmpeg's MAX_SEEKHEAD_ENTRIES, which covers Info, Tracks, Chapters,
// Attachments, Tags twice and Cues.
const webmSeekSlots = 7

// webmSeekReserve is how many bytes the SeekHead and the Void after it take
// together, for a SeekHead that will hold at most entries entries.
//
// What is left once the entries are written is a whole number of entries,
// 21 bytes each, plus the byte the SeekHead's own size field saves when it
// shrinks below 127 bytes: never between 1 and 20. Every length a Void can be
// written at with a minimal size field is reachable from there except 129, which
// no count of 21-byte entries leaves.
func webmSeekReserve(entries int) int {
	return webmSeekHeadSize(max(entries, webmSeekSlots))
}

// webmSeekHeadSize is the length of a SeekHead of this many entries: its
// four-byte identifier, the size field ebml-go writes for its content, and the
// entries.
func webmSeekHeadSize(entries int) int {
	content := entries * webmSeekEntrySize
	return 4 + len(ebmlSizeOf(uint64(content))) + content
}

// ebmlSizeOf is the size field ebml-go writes for a content of this length:
// the shortest one, with the all-ones value of each length reserved for
// "unknown".
func ebmlSizeOf(n uint64) []byte {
	for width := 1; ; width++ {
		if n < 1<<(7*uint(width))-1 {
			return make([]byte, width)
		}
	}
}

// webmVoid is the payload of a Void element that, identifier and size field
// included, is exactly total bytes long; nil, which writes no element at all,
// when there is no room left.
func webmVoid(total int) []byte {
	switch {
	case total == 0:
		return nil
	case total-2 < 127:
		return make([]byte, total-2)
	default:
		return make([]byte, total-3)
	}
}

// webmSeekRegion is the SeekHead and the Void that fill the room reserved for
// them, as the two fields every segment this muxer writes starts with.
func webmSeekRegion(seeks []webmSeek, reserve int) (*webmSeekHead, []byte) {
	return &webmSeekHead{Seek: seeks}, webmVoid(reserve - webmSeekHeadSize(len(seeks)))
}

// webmRegionDoc is the SeekHead and its Void marshalled on their own, to be
// written over the ones a seekable writer was given at the start.
type webmRegionDoc struct {
	SeekHead *webmSeekHead `ebml:"SeekHead"`
	Void     []byte        `ebml:"Void,omitempty"`
}

// measureHead encodes what precedes the clusters and lists where each of its
// Top-Level Elements begins, counted from the head's first byte. The list is
// read off what the encoder wrote rather than kept by hand, so an element
// added to the head — Tags, Attachments — is indexed without anything here
// knowing its name.
func measureHead(doc any) (*bytes.Buffer, []webmSeek, error) {
	var head bytes.Buffer
	var seeks []webmSeek
	hook := func(e *ebml.Element) {
		if e.Parent == nil {
			seeks = append(seeks, webmSeek{SeekID: e.Type.Bytes(), SeekPosition: e.Position})
		}
	}
	if err := measureEBML(doc, &head, ebml.WithElementWriteHooks(hook)); err != nil {
		return nil, nil, err
	}
	return &head, seeks, nil
}

// placeSeeks moves head positions past the room the SeekHead takes.
func placeSeeks(seeks []webmSeek, by uint64) {
	for i := range seeks {
		seeks[i].SeekPosition += by
	}
}

// cuesSeek is the entry naming the Cues at this position.
func cuesSeek(position uint64) webmSeek {
	return webmSeek{SeekID: ebml.ElementCues.Bytes(), SeekPosition: position}
}

// finishSeekable completes a segment streamed to a writer that can seek with
// what only its end knows, as ffmpeg's matroskaenc does in mkv_write_trailer,
// and goes back to the end of the file:
//
//   - the SeekHead, written again over the one given at the start, now naming
//     the Cues when there are any;
//   - the Info, written again with its Duration (RFC 9559 §5.1.2.10) where the
//     Void holding its place was, so a player shows the total time and a seek
//     bar without reading the file to its end;
//   - the Segment's size (§5.1, and RFC 8794 §6.2 for the unknown size it
//     replaces), in the same eight bytes the unknown size took, so a player
//     knows where the segment ends without reaching it. ffmpeg writes it there
//     the same way (put_ebml_length(pb, size, 8)); eight bytes state any size up
//     to 2^56 - 2, more than any writer here will reach;
//   - every Cluster's size (§5.1.3) likewise. Segment and Cluster are the two
//     elements RFC 9559 allows an unknown size, and ffmpeg writes every cluster
//     sized, buffering it.
//
// Each element is rewritten at the length it had, so nothing after it moves.
func (m *WebMMuxer) finishSeekable(cuesAt uint64) error {
	end := m.origin + int64(m.w.n)
	if len(m.cues) > 0 {
		seeks := append(m.seeks, cuesSeek(cuesAt))
		head, void := webmSeekRegion(seeks, m.seekReserve)
		var region bytes.Buffer
		if err := measureEBML(&webmRegionDoc{SeekHead: head, Void: void}, &region); err != nil {
			return fmt.Errorf("container: encode the seek head: %w", err)
		}
		if err := m.writeAt(m.segmentStart, region.Bytes(), "the seek head"); err != nil {
			return err
		}
	}
	info := m.info(m.duration())
	if info.Duration == 0 {
		// A duration that rounds to no tick at all is not written, and the
		// Void keeps its place, so the Info keeps its length.
		info.DurationRoom = make([]byte, webmDurationRoom)
	}
	var infoBytes bytes.Buffer
	if err := measureEBML(&webmInfoDoc{Info: info}, &infoBytes); err != nil {
		return fmt.Errorf("container: encode the segment info: %w", err)
	}
	if err := m.writeAt(m.segmentStart+uint64(m.seekReserve), infoBytes.Bytes(), "the segment info"); err != nil {
		return err
	}
	// Every cluster was opened with its size unknown too, and a segment of
	// known size holding clusters of unknown size is one ebml-go -- this
	// package's reader -- refuses. Each cue says where its cluster begins, one
	// per cluster, and the next one (or the Cues) where it ends.
	for i, cue := range m.cues {
		start := cue.CueTrackPositions[0].CueClusterPosition
		stop := cuesAt
		if i+1 < len(m.cues) {
			stop = m.cues[i+1].CueTrackPositions[0].CueClusterPosition
		}
		at := m.segmentStart + start + webmClusterIDSize
		if err := m.writeAt(at, ebmlSize8(stop-start-webmClusterIDSize-8), "a cluster size"); err != nil {
			return err
		}
	}
	if err := m.writeAt(m.segmentStart-8, ebmlSize8(m.w.n-m.segmentStart), "the segment size"); err != nil {
		return err
	}
	if _, err := m.seeker.Seek(end, io.SeekStart); err != nil {
		return fmt.Errorf("container: seek past the cues: %w", err)
	}
	return nil
}

// webmClusterIDSize is the length of the Cluster element's identifier.
const webmClusterIDSize = 4

// ebmlSize8 is an element size in the eight bytes an unknown size takes
// (RFC 8794 §4: a VINT_WIDTH of 8, marker 0x01, 56 bits of value).
func ebmlSize8(size uint64) []byte {
	return []byte{0x01, byte(size >> 48), byte(size >> 40), byte(size >> 32),
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
}

// writeAt writes b over what the file holds at this position, counted from the
// muxer's first byte, without counting it as written: it replaces bytes the
// counter already holds.
func (m *WebMMuxer) writeAt(at uint64, b []byte, what string) error {
	if _, err := m.seeker.Seek(m.origin+int64(at), io.SeekStart); err != nil {
		return fmt.Errorf("container: seek back to %s: %w", what, err)
	}
	if _, err := m.w.w.Write(b); err != nil {
		return fmt.Errorf("container: write %s: %w", what, err)
	}
	return nil
}

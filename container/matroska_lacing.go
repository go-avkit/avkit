// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

// Matroska IDs the lacing check needs.
const (
	ebmlIDSimpleBlock = 0xA3
	ebmlIDBlock       = 0xA1
)

// matroskaMaster says whether an element ID is a master element of the
// Matroska schema, one whose body is more elements. checkMatroskaFrames walks
// into these and steps over everything else.
//
// ebml-go decodes a block wherever it meets one, in a field of its document or
// not, so a master the walk stepped over could hide a block it never counted.
func matroskaMaster(id uint64) bool {
	switch id {
	case ebmlIDHeader, ebmlIDSegment,
		0x114D9B74, 0x4DBB, // SeekHead, Seek
		0x1549A966, 0x6924, // Info, ChapterTranslate
		0x1F43B675, 0x5854, 0xA0, 0x75A1, 0xA6, 0x8E, 0xE8, 0xC8, // Cluster and below
		0x1654AE6B, 0xAE, 0x6624, // Tracks, TrackEntry, TrackTranslate
		0xE0, 0x55B0, 0x55D0, 0x7670, 0xE1, // Video, Colour, MasteringMetadata, Projection, Audio
		0xE2, 0xE3, 0xE4, 0xE9, // TrackOperation and below
		0x6D80, 0x6240, 0x5034, 0x5035, 0x47E7, // ContentEncodings and below
		0x1C53BB6B, 0xBB, 0xB7, 0xDB, // Cues, CuePoint, CueTrackPositions, CueReference
		0x1941A469, 0x61A7, // Attachments, AttachedFile
		0x1043A770, 0x45B9, 0xB6, 0x8F, 0x80, 0x6944, 0x6911, // Chapters and below
		0x1254C367, 0x7373, 0x63C0, 0x67C8: // Tags, Tag, Targets, SimpleTag
		return true
	}
	return false
}

// checkMatroskaFrames refuses a file whose blocks would unpack to more frames
// than limits allow, reading nothing but the headers of its elements.
//
// ⛔ ebml-go takes a laced block apart while it parses it, allocating a slice for
// every frame the block states, and states are cheap: a 7-byte block of fixed
// lacing with a lace count of 255 is 256 frames, 13 KiB allocated per input
// byte (go-avkit/avkit#120). The count is the byte after the block's flags, and
// that byte is read here, before ebml-go sees the block, then weighed:
//
//   - a frame is at least one byte, so a block cannot hold more frames than it
//     has bytes after its lace count. This alone bounds the frames of a file by
//     its size, and refuses the empty frames that make a block a bomb;
//   - the frames of one track, and of the whole file, are bounded by limits.
//
// A structure this walk cannot follow ends it without an error: ebml-go reads
// the same bytes next and says what is wrong with them.
func checkMatroskaFrames(data []byte, limits ReaderLimits) error {
	_, err := countMatroskaFrames(data, limits.bounded())
	return err
}

// countMatroskaFrames returns the frames each track number holds, or the
// refusal. limits are applied as given: callers go through bounded.
func countMatroskaFrames(data []byte, limits ReaderLimits) (map[uint64]int, error) {
	perTrack := map[uint64]int{}
	total := 0
	for i := 0; i < len(data); {
		id, idWidth, _, ok := ebmlVint(data, i, true)
		if !ok {
			return perTrack, nil
		}
		size, sizeWidth, unknown, ok := ebmlVint(data, i+idWidth, false)
		if !ok {
			return perTrack, nil
		}
		body := i + idWidth + sizeWidth
		if matroskaMaster(id) {
			i = body
			continue
		}
		if unknown || !fitsAfter(data, body, size) {
			return perTrack, nil
		}
		end := body + int(size)
		if id == ebmlIDSimpleBlock || id == ebmlIDBlock {
			track, frames, err := blockFrames(data[body:end])
			if err != nil {
				return perTrack, err
			}
			if frames > 0 {
				perTrack[track] += frames
				total += frames
				if perTrack[track] > limits.MaxTrackFrames {
					return perTrack, limitError("track %d holds more than %d frames", track, limits.MaxTrackFrames)
				}
				if total > limits.MaxFrames {
					return perTrack, limitError("the file holds more than %d frames", limits.MaxFrames)
				}
			}
		}
		i = end
	}
	return perTrack, nil
}

// blockFrames reads the header of one block: the track it belongs to and how
// many frames it states. A block too short to have a header states none, and is
// left for ebml-go to refuse.
func blockFrames(b []byte) (track uint64, frames int, err error) {
	track, width, _, ok := ebmlVint(b, 0, false)
	if !ok || len(b) < width+3 {
		return 0, 0, nil
	}
	flags := b[width+2]
	lacing := flags >> 1 & 3
	if lacing == 0 {
		return track, 1, nil
	}
	if len(b) < width+4 {
		return 0, 0, nil
	}
	frames = int(b[width+3]) + 1
	if room := len(b) - (width + 4); frames > room {
		return 0, 0, limitError("a block of track %d states %d laced frames and holds %d bytes after the count",
			track, frames, room)
	}
	return track, frames, nil
}

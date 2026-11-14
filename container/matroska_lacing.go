// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

// The walk below descends into the elements in ebmlMasters and counts the
// ones in ebmlBlocks. Both are GENERATED from ebml-go's own element table
// (matroska_masters.go, by internal/ebmlmasters), and a test fails
// when the module's table and the generated sets differ: the walk and the
// unlacer must agree on every master, because ebml-go unlaces a block wherever
// it meets one, and a master the walk stepped over would hide blocks it never
// counted (BlockAdditionMapping did, in the first version of this check).

// checkMatroskaFrames refuses a file whose blocks would unpack to more frames
// than limits allow, reading nothing but the headers of its elements.
//
// ⛔ ebml-go takes a laced block apart while it parses it, allocating a slice for
// every frame the block states, and states are cheap: a 7-byte block of fixed
// lacing with a lace count of 255 is 256 frames, 13 KiB allocated per input
// byte (go-avkit/avkit#120). The count is the byte after the block's flags, and
// that byte is read here, before ebml-go sees the block, then weighed:
//
//   - a block cannot state more frames than it has bytes after its lace count
//     (zero-size frames included: Xiph lacing accepts 256 of them, because its
//     lace-size bytes count as room). This alone bounds the frames of a file by
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
		if ebmlMasters[id] {
			i = body
			continue
		}
		if unknown || !fitsAfter(data, body, size) {
			return perTrack, nil
		}
		end := body + int(size)
		if ebmlBlocks[id] {
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

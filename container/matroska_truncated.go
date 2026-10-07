package container

import "errors"

// ErrTruncated says a file could not be read to its end, and that what came
// before the point it stopped at was read anyway.
//
// It does NOT claim the file was cut cleanly. A file damaged in the middle
// stops the walk at the damage and reports the same thing, because from here
// the two are indistinguishable: in both cases the bytes up to a point are
// whole and nothing after it could be trusted. What the sentinel promises is
// only that -- what you have parsed strictly, and there was more that did not.
//
// NewReader returns it ALONGSIDE a usable Reader, which is the whole point: a
// caller that writes the ordinary `if err != nil { return err }` keeps today's
// refusal exactly, and only a caller that asks for the bytes --
// errors.Is(err, ErrTruncated) -- gets them. The safe behaviour stays the
// default and the useful one is one line away.
var ErrTruncated = errors.New("container: the file ends before its structure does")

// Matroska element IDs this file needs by name.
const (
	ebmlIDSegment = 0x18538067
	ebmlIDHeader  = 0x1A45DFA3
)

// ebmlVint reads one EBML variable-length integer at i.
//
// An ID keeps its marker bit and a size strips it, which is why keepMarker
// exists rather than two near-identical functions. A size whose value bits are
// all ones means "unknown", and callers must treat that as a length they
// cannot use rather than as a very large one.
func ebmlVint(b []byte, i int, keepMarker bool) (value uint64, width int, unknown, ok bool) {
	if i < 0 || i >= len(b) || b[i] == 0 {
		// A leading zero byte would mean a width above eight, which Matroska
		// does not use and which is more likely to be padding or rubbish.
		return 0, 0, false, false
	}
	n := 0
	for b[i]>>(7-n)&1 == 0 {
		n++
	}
	width = n + 1
	if i+width > len(b) {
		return 0, 0, false, false
	}
	if keepMarker {
		value = uint64(b[i])
	} else {
		value = uint64(b[i]) & (1<<(7-n) - 1)
	}
	for k := 1; k < width; k++ {
		value = value<<8 | uint64(b[i+k])
	}
	if !keepMarker && value == 1<<(7*uint(width))-1 {
		unknown = true
	}
	return value, width, unknown, true
}

// putEbmlSize writes value into w bytes as an EBML size, keeping the width the
// file already used. EBML allows a non-minimal encoding, so rewriting a size in
// place needs no shifting of everything after it.
func putEbmlSize(b []byte, value uint64, width int) bool {
	if width < 1 || width > 8 || len(b) < width {
		return false
	}
	if value >= 1<<(7*uint(width))-1 { // the all-ones pattern is "unknown"
		return false
	}
	for k := width - 1; k >= 0; k-- {
		b[k] = byte(value & 0xff)
		value >>= 8
	}
	b[0] |= 1 << (8 - uint(width))
	return true
}

// completeMatroskaPrefix returns the longest prefix of a Matroska file that is
// structurally whole, and says whether anything was dropped.
//
// The point is to keep the strictness that unmarshalMatroska deliberately has.
// Turning on ebml-go's unknown-element tolerance would swallow read and size
// errors and hand back whatever it had, so a truncated file would read as a
// SHORT one -- a duration and a sample count that are simply wrong, with
// nothing downstream able to tell. Cutting at an element boundary and then
// parsing that prefix strictly keeps the guarantee: a parse that succeeds says
// the bytes really were well formed, and the only thing lost is named.
//
// The Segment's declared size is rewritten to the kept length, in the width it
// already used, because a Segment that still claimed the original size would
// fail the strict parse for the same reason the whole file did.
func completeMatroskaPrefix(data []byte) (prefix []byte, cut bool) {
	segStart, segSizeAt, segSizeWidth, segUnknown := -1, -1, 0, false
	for i := 0; i < len(data); {
		id, idWidth, _, ok := ebmlVint(data, i, true)
		if !ok {
			break
		}
		size, szWidth, unknown, ok := ebmlVint(data, i+idWidth, false)
		if !ok {
			break
		}
		header := idWidth + szWidth
		if id == ebmlIDSegment {
			segStart, segSizeAt, segSizeWidth, segUnknown = i+header, i+idWidth, szWidth, unknown
			break
		}
		if unknown || i+header+int(size) > len(data) {
			break // the header itself is cut: nothing here is readable
		}
		i += header + int(size)
		if id != ebmlIDHeader {
			break
		}
	}
	if segStart < 0 {
		return nil, false // no Segment at all: nothing this can rescue
	}

	end := walkSegmentChildren(data, segStart)
	if end == segStart {
		return nil, false // not one complete child: there is nothing to hand back
	}
	if end >= len(data) && !segUnknown {
		return nil, false // nothing was dropped; the caller's error is not truncation
	}

	prefix = make([]byte, end)
	copy(prefix, data[:end])
	if !segUnknown {
		if !putEbmlSize(prefix[segSizeAt:], uint64(end-segStart), segSizeWidth) {
			return nil, false
		}
	}
	return prefix, true
}

// segmentChild says whether an ID is one of the Segment's own children, which
// is how an element of UNKNOWN size is known to have ended: nothing states its
// length, so the only thing that closes it is the next sibling starting.
func segmentChild(id uint64) bool {
	switch id {
	case 0x114D9B74, // SeekHead
		0x1549A966, // Info
		0x1654AE6B, // Tracks
		0x1F43B675, // Cluster
		0x1C53BB6B, // Cues
		0x1941A469, // Attachments
		0x1043A770, // Chapters
		0x1254C367: // Tags
		return true
	}
	return false
}

// walkSegmentChildren returns the offset just past the last Segment child that
// is whole.
//
// A cluster is commonly written with an UNKNOWN size -- that is what a muxer
// does when it cannot know the length before writing the contents, and it is
// the normal shape of anything recorded live. Such a cluster ends where the
// next sibling begins, so it has to be walked from the inside; and because
// nothing declares its length, a cluster cut part way through is still usable
// up to its last complete child. That is precisely the case this exists for.
func walkSegmentChildren(data []byte, start int) int {
	end := start
	for j := start; j < len(data); {
		_, idWidth, _, ok := ebmlVint(data, j, true)
		if !ok {
			return end
		}
		size, szWidth, unknown, ok := ebmlVint(data, j+idWidth, false)
		if !ok {
			return end
		}
		if !unknown {
			next := j + idWidth + szWidth + int(size)
			if next > len(data) || next < j {
				return end
			}
			end, j = next, next
			continue
		}
		// Unknown size: walk the children until a sibling starts or the data
		// runs out, keeping every grandchild that is whole.
		k := j + idWidth + szWidth
		for k < len(data) {
			gid, gidWidth, _, ok := ebmlVint(data, k, true)
			if !ok {
				return end
			}
			if segmentChild(gid) {
				break // the next sibling: this element ended cleanly here
			}
			gsize, gszWidth, gunknown, ok := ebmlVint(data, k+gidWidth, false)
			if !ok || gunknown {
				return end
			}
			next := k + gidWidth + gszWidth + int(gsize)
			if next > len(data) || next < k {
				return end
			}
			end, k = next, next
		}
		j = k
	}
	return end
}

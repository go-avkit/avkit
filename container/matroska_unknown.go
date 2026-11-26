// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

// ebmlVoidID is the Void element (RFC 8794 §11.2.6): a body nobody reads.
const ebmlVoidID = 0xEC

// skipUnknownElements returns data with every element ebml-go does not know
// turned into a Void of the same length, so that ebml-go skips it by its size.
//
// RFC 9559 §7 says a reader "MUST skip Matroska/EBML elements it encounters but
// does not know about if that unknown element fits into the size constraints
// set by the current Parent Element", and RFC 8794 §16 that unknown IDs may be
// accepted in order to skip such elements. FrameRate (0x2383E3, RFC 9559
// Appendix A.26, informational only, which libvpx's old muxer wrote into every
// video track) is one the format no longer defines. ebml-go refuses it with
// ErrUnknownElement (go-avkit/avkit#129): 201 of the 376 libvpx test vectors
// did not open. The parent's bound stays ebml-go's to enforce: a Void that
// overruns its parent is refused as any element would be.
//
// ebml-go's own way out, WithIgnoreUnknown, is not taken: it scans on byte by
// byte after any read or size error and returns whatever it had, so a corrupt
// file would read as a short one (see demuxMatroska). This skips only an
// element whose header states a size that lies within the file, and only by
// that size; everything else, ebml-go still reads strictly.
//
//   - An element of unknown size is not skipped: where it ends cannot be said,
//     and ebml-go refuses it as before.
//   - The skip is bounded by the file, and costs nothing when no element is
//     unknown: data comes back as it was, uncopied. Otherwise one copy is made,
//     the same length, because every later offset (and the truncated-file cut)
//     is an offset into it.
//   - Nothing is allocated from a stated size: the Void keeps the bytes it
//     covers, and ebml-go's leaf limit (mkvLeafLimit) already caps it at the
//     file's length.
//
// The walk is the one countMatroskaFrames makes: it descends into the masters
// ebml-go knows and steps over any other element, so an unknown master is
// skipped whole, children included. A structure it cannot follow ends it, and
// ebml-go says what is wrong with the rest.
func skipUnknownElements(data []byte) []byte {
	out := data
	for i := 0; i < len(data); {
		// An ID that cannot be read has width zero, and the size is then read at
		// the same byte and cannot be either, so one check covers both.
		id, idWidth, _, _ := ebmlVint(data, i, true)
		size, sizeWidth, unknown, ok := ebmlVint(data, i+idWidth, false)
		if !ok {
			return out
		}
		body := i + idWidth + sizeWidth
		if ebmlMasters[id] {
			i = body
			continue
		}
		if unknown || !fitsAfter(data, body, size) {
			return out
		}
		end := body + int(size)
		if !ebmlKnown[id] {
			if &out[0] == &data[0] {
				out = append([]byte(nil), data...)
			}
			voidElement(out[i:end])
		}
		i = end
	}
	return out
}

// voidElement rewrites the element in b, header and body, as a Void of the same
// length. The size is written in as many bytes as the old header had less the
// one-byte Void ID (EBML permits a size wider than it needs), up to eight, and
// states the rest.
func voidElement(b []byte) {
	total := len(b)
	// An element has at least two bytes of header, so width is at least one.
	width := min(total-1, 8)
	size := uint64(total - 1 - width)
	b[0] = ebmlVoidID
	for k := width; k >= 1; k-- {
		b[k] = byte(size)
		size >>= 8
	}
	// The marker bit that states the width; the value's own top bits are zero.
	b[1] |= 0x80 >> (width - 1)
}

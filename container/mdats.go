// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/Eyevinn/mp4ff/mp4"
)

// A progressive MP4 may hold its media in several mdat boxes. ISO/IEC 14496-12
// §8.1.1 allows any number of them, and a sample is found by the absolute file
// offset its chunk states in stco or co64, so where one mdat ends and the next
// begins means nothing to a reader that goes by offset. Writers that flush a
// recording in pieces produce exactly this; ffmpeg's mov_read_mdat notes that
// an mdat was seen and reads on. Measured over 2375 MP4 files: six progressive
// files hold two or three non-empty mdat boxes, every one of them directly
// after the last, and ffprobe reads all six.
//
// mp4ff refuses such a file, and declined to accept it (Eyevinn/mp4ff#606):
// its own sample readers slice File.Mdat, which can only be one of the boxes.
// This package never reads samples through File.Mdat -- it reads them by
// offset from the bytes or the file it was handed -- so the remedy belongs
// here, before mp4ff sees the file: the first box of a run of adjacent mdats is
// shown to mp4ff with a size that covers the whole run. The swallowed headers
// are eight bytes of media as far as mp4ff knows, and no sample table points
// into them. Nothing is written: the bytes are patched in a view, and samples
// are still read from the caller's source.
//
// What is left alone, so mp4ff refuses it as before:
//   - mdat boxes that are not adjacent: a box between them is not media, and
//     swallowing it would hide it from the parse;
//   - a run whose length does not fit a 32-bit size, or that starts with a
//     64-bit header: growing it would take a wider header, which moves bytes;
//   - a fragmented file: its mdat boxes belong to their fragments.

// mdatHeaderPatch is a new 32-bit size for the box that starts at offset at.
type mdatHeaderPatch struct {
	at   int64
	size uint32
}

// adjacentMdatPatches walks the top-level boxes of a file of the given size and
// returns, for each run of two or more adjacent mdat boxes, the size its first
// box must state to cover the run. A box it cannot read or measure ends the
// walk, and the runs measured before it still stand: their boxes were read
// whole, and the decode will meet the bad box and judge it on its own.
func adjacentMdatPatches(src io.ReaderAt, size int64) []mdatHeaderPatch {
	var patches []mdatHeaderPatch
	runStart, runEnd, runLen := int64(-1), int64(0), 0
	closeRun := func() {
		if runLen > 1 && runEnd-runStart <= math.MaxUint32 {
			patches = append(patches, mdatHeaderPatch{at: runStart, size: uint32(runEnd - runStart)})
		}
		runStart, runLen = -1, 0
	}
	header := make([]byte, 16)
walk:
	for pos := int64(0); pos < size; {
		if size-pos < 8 {
			break
		}
		if _, err := src.ReadAt(header[:8], pos); err != nil {
			break
		}
		boxSize, wide := int64(binary.BigEndian.Uint32(header)), false
		switch boxSize {
		case 0:
			// To the end of the file: legal, and nothing can follow it.
			boxSize = size - pos
		case 1:
			if size-pos < 16 {
				break walk
			}
			if _, err := src.ReadAt(header[8:16], pos+8); err != nil {
				break walk
			}
			large := binary.BigEndian.Uint64(header[8:16])
			if large < 16 || large > uint64(size-pos) {
				break walk
			}
			boxSize, wide = int64(large), true
		}
		if boxSize < 8 || boxSize > size-pos {
			break
		}
		switch typ := string(header[4:8]); {
		case typ == "moof":
			return nil
		case typ != "mdat":
			closeRun()
		case runStart >= 0:
			runEnd, runLen = pos+boxSize, runLen+1
		case !wide:
			runStart, runEnd, runLen = pos, pos+boxSize, 1
		}
		pos += boxSize
	}
	closeRun()
	return patches
}

// patchedSource is a file seen through new sizes for some of its boxes.
type patchedSource struct {
	src     io.ReaderAt
	patches []mdatHeaderPatch
}

func (p patchedSource) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.src.ReadAt(b, off)
	var field [4]byte
	for _, pt := range p.patches {
		binary.BigEndian.PutUint32(field[:], pt.size)
		for i, v := range field {
			if at := pt.at + int64(i) - off; at >= 0 && at < int64(n) {
				b[at] = v
			}
		}
	}
	return n, err
}

// decodeMP4 parses the box tree of an MP4 of the given size. A run of adjacent
// mdat boxes is shown to mp4ff as one, and a failure after a complete movie box
// is tolerated (usableDespite) -- the same for every entry point, because a
// file one of them accepts and another refuses is a defect of its own.
func decodeMP4(src io.ReadSeeker, at io.ReaderAt, size int64, opts ...mp4.Option) (*mp4.File, error) {
	if patches := adjacentMdatPatches(at, size); len(patches) > 0 {
		src = io.NewSectionReader(patchedSource{src: at, patches: patches}, 0, size)
	}
	parsed, err := mp4.DecodeFile(src, opts...)
	if err != nil {
		return usableDespite(parsed, err)
	}
	return parsed, nil
}

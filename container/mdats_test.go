// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// twoTrackProgressive is a progressive file, ftyp mdat moov, whose two tracks
// are each written in many chunks, so that the media can be cut at a chunk
// start with samples of both tracks on either side.
func twoTrackProgressive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(80*time.Millisecond))
	video, err := m.AddTrack(av1Track(30000, 320, 180))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(aacTrack(48000))
	if err != nil {
		t.Fatal(err)
	}
	vs, as := marked(0xA1, 30, 1000, 5), marked(0xB2, 45, 1024, 1)
	for i := 0; i < len(as); i++ {
		if i < len(vs) {
			if err := m.WriteSample(video, vs[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.WriteSample(audio, as[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chunkStarts lists every chunk offset of every track, in file order.
func chunkStarts(moov *mp4.MoovBox) []uint64 {
	var out []uint64
	for _, trak := range moov.Traks {
		for _, off := range trak.Mdia.Minf.Stbl.Stco.ChunkOffset {
			out = append(out, uint64(off))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// splitMdat rewrites a progressive ftyp-mdat-moov file so that its media sits
// in several adjacent mdat boxes, a new one starting at each cut -- which must
// be chunk starts. Every chunk at or after a cut moves by the eight bytes of
// each header inserted before it, and the moov says so: this is the file a
// writer flushing in pieces produces, and the shape mp4ff refuses.
func splitMdat(t *testing.T, data []byte, cuts ...uint64) []byte {
	t.Helper()
	moov := progressiveMoov(t, data)
	ftypSize := uint64(binary.BigEndian.Uint32(data))
	mdatEnd := ftypSize + uint64(binary.BigEndian.Uint32(data[ftypSize:]))
	bounds := append(append([]uint64{ftypSize + 8}, cuts...), mdatEnd)

	var out bytes.Buffer
	out.Write(data[:ftypSize])
	for i := 0; i+1 < len(bounds); i++ {
		var header [8]byte
		binary.BigEndian.PutUint32(header[:], uint32(8+bounds[i+1]-bounds[i]))
		copy(header[4:], "mdat")
		out.Write(header[:])
		out.Write(data[bounds[i]:bounds[i+1]])
	}
	for _, trak := range moov.Traks {
		offsets := trak.Mdia.Minf.Stbl.Stco.ChunkOffset
		for j, off := range offsets {
			for _, cut := range cuts {
				if uint64(off) >= cut {
					offsets[j] += 8
				}
			}
		}
	}
	if err := moov.Encode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// samplesByTrack reads every sample of every track.
func samplesByTrack(t *testing.T, r *Reader) map[uint32][]Sample {
	t.Helper()
	out := map[uint32][]Sample{}
	for _, id := range r.TrackIDs() {
		s, err := r.Samples(id)
		if err != nil {
			t.Fatalf("track %d: %v", id, err)
		}
		out[id] = s
	}
	return out
}

// TestAdjacentMdatBoxesReadAsOne.
//
// ⛔ The file is cut where a chunk starts, with samples of both tracks before
// and after every cut, so the later chunks really do move. A second mdat that
// holds nothing, or one appended after the moov, would leave every offset where
// it was and prove nothing about reading across a boundary -- which is how the
// first attempt at this, upstream in mp4ff, passed its own test while its
// in-memory sample reads failed for every sample past the first box.
func TestAdjacentMdatBoxesReadAsOne(t *testing.T) {
	whole := twoTrackProgressive(t)
	ref, err := NewReader(whole)
	if err != nil {
		t.Fatal(err)
	}
	want := samplesByTrack(t, ref)
	starts := chunkStarts(progressiveMoov(t, whole))
	if len(starts) < 8 {
		t.Fatalf("only %d chunks; the fixture must have several on each side of a cut", len(starts))
	}

	for name, cuts := range map[string][]uint64{
		"two boxes":   {starts[len(starts)/2]},
		"three boxes": {starts[len(starts)/3], starts[2*len(starts)/3]},
	} {
		t.Run(name, func(t *testing.T) {
			split := splitMdat(t, whole, cuts...)
			if got := topLevelBoxes(t, split); len(got) != 3+len(cuts) {
				t.Fatalf("boxes = %v", got)
			}
			// The shape is the one mp4ff refuses on its own.
			if _, err := mp4.DecodeFile(bytes.NewReader(split)); err == nil {
				t.Fatal("mp4ff read the split file unaided; the fixture does not exercise the remedy")
			}

			inMemory, err := NewReader(split)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			onDisk, err := NewFileReader(bytes.NewReader(split), int64(len(split)))
			if err != nil {
				t.Fatalf("NewFileReader: %v", err)
			}
			for mode, r := range map[string]*Reader{"in memory": inMemory, "on disk": onDisk} {
				got := samplesByTrack(t, r)
				if len(got) != len(want) {
					t.Fatalf("%s: %d tracks, want %d", mode, len(got), len(want))
				}
				for id, ws := range want {
					gs := got[id]
					if len(gs) != len(ws) {
						t.Fatalf("%s: track %d: %d samples, want %d", mode, id, len(gs), len(ws))
					}
					for i := range gs {
						if !bytes.Equal(gs[i].Data, ws[i].Data) {
							t.Errorf("%s: track %d sample %d = %x, want %x", mode, id, i, gs[i].Data, ws[i].Data)
						}
					}
				}
			}
			f, err := Demux(split)
			if err != nil {
				t.Fatalf("Demux: %v", err)
			}
			if len(f.Tracks) != 2 {
				t.Errorf("Demux: %d tracks", len(f.Tracks))
			}
		})
	}
}

// TestMdatBoxesThatAreNotAdjacentAreStillRefused: a box between two mdats is
// not media, and growing the first over it would hide it from the parse.
func TestMdatBoxesThatAreNotAdjacentAreStillRefused(t *testing.T) {
	whole := twoTrackProgressive(t)
	split := splitMdat(t, whole, chunkStarts(progressiveMoov(t, whole))[4])
	// Put a free box between the two mdats. The chunk offsets are left as they
	// were: the file is refused before any of them is read.
	ftypSize := int(binary.BigEndian.Uint32(split))
	firstEnd := ftypSize + int(binary.BigEndian.Uint32(split[ftypSize:]))
	free := []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}
	var out []byte
	out = append(out, split[:firstEnd]...)
	out = append(out, free...)
	out = append(out, split[firstEnd:]...)
	if got := adjacentMdatPatches(bytes.NewReader(out), int64(len(out))); len(got) != 0 {
		t.Fatalf("patches = %v, want none", got)
	}
	if _, err := NewReader(out); err == nil {
		t.Error("two mdat boxes with a box between them were accepted")
	}
}

// boxBytes is a top-level box of the given type and total size, zero-filled.
func boxBytes(typ string, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, uint32(size))
	copy(b[4:], typ)
	return b
}

// wideBox is a box with a 64-bit size field.
func wideBox(typ string, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], typ)
	binary.BigEndian.PutUint64(b[8:], uint64(size))
	return b
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestAdjacentMdatPatches(t *testing.T) {
	ftyp, moov := boxBytes("ftyp", 16), boxBytes("moov", 24)
	cases := []struct {
		name string
		file []byte
		want []mdatHeaderPatch
	}{
		{"one mdat", concat(ftyp, boxBytes("mdat", 40), moov), nil},
		{"two adjacent", concat(ftyp, boxBytes("mdat", 40), boxBytes("mdat", 30), moov),
			[]mdatHeaderPatch{{at: 16, size: 70}}},
		{"three adjacent, the last empty", concat(ftyp, boxBytes("mdat", 40), boxBytes("mdat", 30), boxBytes("mdat", 8), moov),
			[]mdatHeaderPatch{{at: 16, size: 78}}},
		{"two runs", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), boxBytes("free", 8), boxBytes("mdat", 10), boxBytes("mdat", 12), moov),
			[]mdatHeaderPatch{{at: 16, size: 20}, {at: 44, size: 22}}},
		{"a run ending the file", concat(ftyp, moov, boxBytes("mdat", 10), boxBytes("mdat", 10)),
			[]mdatHeaderPatch{{at: 40, size: 20}}},
		{"a box between", concat(ftyp, boxBytes("mdat", 40), moov, boxBytes("mdat", 30)), nil},
		// A last box of size 0 runs to the end of the file (ISO/IEC 14496-12
		// §4.2), so it can close a run as well as any other.
		{"a last mdat of size 0", concat(ftyp, moov, boxBytes("mdat", 10), func() []byte {
			b := boxBytes("mdat", 20)
			binary.BigEndian.PutUint32(b, 0)
			return b
		}()), []mdatHeaderPatch{{at: 40, size: 30}}},
		// A 64-bit header cannot be given a 32-bit size without moving the
		// payload, so such a box does not start a run -- but it ends one.
		{"a wide first mdat", concat(ftyp, wideBox("mdat", 40), boxBytes("mdat", 30), moov), nil},
		{"a wide mdat after a narrow one", concat(ftyp, boxBytes("mdat", 20), wideBox("mdat", 40), moov),
			[]mdatHeaderPatch{{at: 16, size: 60}}},
		// The fragments own their mdat boxes.
		{"fragmented", concat(ftyp, moov, boxBytes("moof", 16), boxBytes("mdat", 10), boxBytes("mdat", 10)), nil},
		// What cannot be measured ends the walk; the run before it stands.
		{"a short tail", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), moov, []byte{1, 2, 3}),
			[]mdatHeaderPatch{{at: 16, size: 20}}},
		{"a box claiming more than is there", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), []byte{0, 0, 1, 0, 'j', 'u', 'n', 'k'}),
			[]mdatHeaderPatch{{at: 16, size: 20}}},
		{"a size below a header", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), []byte{0, 0, 0, 4, 'j', 'u', 'n', 'k'}),
			[]mdatHeaderPatch{{at: 16, size: 20}}},
		{"a 64-bit size cut short", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0}),
			[]mdatHeaderPatch{{at: 16, size: 20}}},
		{"a 64-bit size claiming more than is there", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), func() []byte {
			b := wideBox("mdat", 16)
			binary.BigEndian.PutUint64(b[8:], 1<<40)
			return b
		}()), []mdatHeaderPatch{{at: 16, size: 20}}},
		{"a 64-bit size below its header", concat(ftyp, boxBytes("mdat", 10), boxBytes("mdat", 10), func() []byte {
			b := wideBox("mdat", 16)
			binary.BigEndian.PutUint64(b[8:], 8)
			return b
		}()), []mdatHeaderPatch{{at: 16, size: 20}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := adjacentMdatPatches(bytes.NewReader(c.file), int64(len(c.file)))
			if len(got) != len(c.want) {
				t.Fatalf("patches = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("patch %d = %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// sparseFile is a file of the given size holding only the bytes it was given,
// at their offsets, and zeros elsewhere: big enough to need a 64-bit size,
// without the memory.
type sparseFile struct {
	size  int64
	parts map[int64][]byte
	fail  int64 // a read at this offset fails, when not -1
}

func (s sparseFile) ReadAt(p []byte, off int64) (int, error) {
	if off == s.fail {
		return 0, errors.New("the disk gave way")
	}
	if off >= s.size {
		return 0, io.EOF
	}
	n := len(p)
	if rest := s.size - off; int64(n) > rest {
		n = int(rest)
	}
	clear(p[:n])
	for at, b := range s.parts {
		for i, v := range b {
			if j := at + int64(i) - off; j >= 0 && j < int64(n) {
				p[j] = v
			}
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestARunTooLongForA32BitSizeIsLeftAlone(t *testing.T) {
	first, second := int64(3<<30), int64(2<<30)
	header := func(size int64) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, uint32(size))
		copy(b[4:], "mdat")
		return b
	}
	f := sparseFile{size: 16 + first + second + 24, fail: -1, parts: map[int64][]byte{
		0:                   boxBytes("ftyp", 16),
		16:                  header(first),
		16 + first:          header(second),
		16 + first + second: boxBytes("moov", 24)[:8],
	}}
	if first+second <= math.MaxUint32 {
		t.Fatal("the fixture fits 32 bits")
	}
	if got := adjacentMdatPatches(f, f.size); len(got) != 0 {
		t.Errorf("patches = %v, want none: the run is %d bytes", got, first+second)
	}
}

func TestAWalkThatCannotReadKeepsWhatItMeasured(t *testing.T) {
	file := concat(boxBytes("ftyp", 16), boxBytes("mdat", 10), boxBytes("mdat", 10), wideBox("free", 16))
	parts := map[int64][]byte{0: file}
	want := []mdatHeaderPatch{{at: 16, size: 20}}
	for name, fail := range map[string]int64{"a header": 36, "a 64-bit size": 44} {
		t.Run(name, func(t *testing.T) {
			f := sparseFile{size: int64(len(file)), parts: parts, fail: fail}
			got := adjacentMdatPatches(f, f.size)
			if len(got) != 1 || got[0] != want[0] {
				t.Errorf("patches = %v, want %v", got, want)
			}
		})
	}
}

// TestThePatchedViewReadsAcrossTheField: a read that starts or ends inside the
// size field must see the patched bytes it covers and the source's elsewhere.
func TestThePatchedViewReadsAcrossTheField(t *testing.T) {
	file := concat(boxBytes("ftyp", 16), boxBytes("mdat", 10), boxBytes("mdat", 10))
	view := patchedSource{src: bytes.NewReader(file), patches: []mdatHeaderPatch{{at: 16, size: 20}}}
	want := bytes.Clone(file)
	binary.BigEndian.PutUint32(want[16:], 20)
	for start := 0; start < len(file); start++ {
		for end := start + 1; end <= len(file); end++ {
			got := make([]byte, end-start)
			if _, err := view.ReadAt(got, int64(start)); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want[start:end]) {
				t.Fatalf("[%d:%d] = %x, want %x", start, end, got, want[start:end])
			}
		}
	}
	if !bytes.Equal(file[16:20], []byte{0, 0, 0, 10}) {
		t.Error("the source itself was written to")
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// withTail returns an MP4 with extra bytes after its last box.
func withTail(file []byte, tail []byte) []byte {
	return append(append([]byte(nil), file...), tail...)
}

// aJunkBoxHeader is four bytes stating a length, and four naming a type, for a
// box whose stated length is not there. Two files of 2003 measured end in
// something of this shape.
func aJunkBoxHeader(stated uint32) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, stated)
	copy(out[4:], "\x00\x00\x00\x00")
	return out
}

// TestATailAfterTheMovieBoxDoesNotLoseTheFile.
//
// ⛔ The movie box is what says where every sample is. Once it has been read, the
// rest of the file is media addressed by offset, and bytes beyond it that do not
// form a box describe nothing a reader needs -- so refusing the file for them
// loses a film that every player opens. Measured over 2003 MP4 files, two end
// this way: one has a single stray byte after its last box, and one has 24 bytes
// whose first four read as a length of 170 MB that is not there.
func TestATailAfterTheMovieBoxDoesNotLoseTheFile(t *testing.T) {
	whole := hevcMP4(t, "hvc1", videoSamples(lengthPrefixed(hevcIDR), lengthPrefixed(hevcIDR)))
	if _, err := NewReader(whole); err != nil {
		t.Fatalf("the fixture itself does not read: %v", err)
	}
	for name, tail := range map[string][]byte{
		"one stray byte":                 {0x42},
		"seven bytes, short of a header": {1, 2, 3, 4, 5, 6, 7},
		"a header claiming 170 MB":       aJunkBoxHeader(170 << 20),
		"a header and some of its body":  append(aJunkBoxHeader(1<<20), 1, 2, 3, 4),
	} {
		t.Run(name, func(t *testing.T) {
			data := withTail(whole, tail)
			r, err := NewReader(data)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			tracks := r.File().VideoTracks()
			if len(tracks) != 1 {
				t.Fatalf("%d video tracks", len(tracks))
			}
			got, err := r.Samples(tracks[0].ID)
			if err != nil {
				t.Fatalf("samples: %v", err)
			}
			// The samples must be the ones the whole file holds, not fewer.
			ref, err := NewReader(whole)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ref.Samples(ref.File().VideoTracks()[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			for i := range got {
				if !bytes.Equal(got[i].Data, want[i].Data) {
					t.Errorf("sample %d differs from the untailed file's", i)
				}
			}
		})
	}
}

// TestAFailureBeforeTheMovieBoxIsStillAFailure.
//
// ⛔ The tolerance is for a tail, and nothing else. Where the sample tables are
// what is broken, a reader that pressed on would be reading offsets it had not
// finished parsing -- so a file whose movie box never arrived, or arrived
// without the tables that say where samples are, is refused as before.
func TestAFailureBeforeTheMovieBoxIsStillAFailure(t *testing.T) {
	t.Run("no movie box at all", func(t *testing.T) {
		// An ftyp, then a box claiming more than is there.
		var buf bytes.Buffer
		buf.Write([]byte{0, 0, 0, 0x14, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		buf.Write(aJunkBoxHeader(1 << 20))
		if _, err := NewReader(buf.Bytes()); err == nil {
			t.Error("a file with no movie box was accepted")
		}
	})
	t.Run("nothing at all", func(t *testing.T) {
		if _, err := NewReader(nil); err == nil {
			t.Error("no bytes at all were accepted")
		}
	})
	t.Run("a parse that returned no file", func(t *testing.T) {
		want := errors.New("the parse failed")
		if _, err := usableDespite(nil, want); !errors.Is(err, want) {
			t.Errorf("err = %v, want the original", err)
		}
	})
}

// TestTheToleranceRefusesAMovieBoxWithoutSampleTables drives the decision
// directly, because a muxer cannot be asked to write a track whose tables are
// missing -- and those are precisely the shapes where pressing on would mean
// reading offsets that were never parsed.
func TestTheToleranceRefusesAMovieBoxWithoutSampleTables(t *testing.T) {
	broke := errors.New("the parse failed")
	full := func() *mp4.TrakBox {
		return &mp4.TrakBox{Mdia: &mp4.MdiaBox{Minf: &mp4.MinfBox{Stbl: &mp4.StblBox{
			Stsz: &mp4.StszBox{}, Stco: &mp4.StcoBox{},
		}}}}
	}
	cases := map[string]*mp4.MoovBox{
		"no track":        {},
		"no media box":    {Traks: []*mp4.TrakBox{{}}},
		"no media info":   {Traks: []*mp4.TrakBox{{Mdia: &mp4.MdiaBox{}}}},
		"no sample table": {Traks: []*mp4.TrakBox{{Mdia: &mp4.MdiaBox{Minf: &mp4.MinfBox{}}}}},
		"no sample sizes": {Traks: []*mp4.TrakBox{{Mdia: &mp4.MdiaBox{Minf: &mp4.MinfBox{
			Stbl: &mp4.StblBox{Stco: &mp4.StcoBox{}},
		}}}}},
		"no chunk offsets": {Traks: []*mp4.TrakBox{{Mdia: &mp4.MdiaBox{Minf: &mp4.MinfBox{
			Stbl: &mp4.StblBox{Stsz: &mp4.StszBox{}},
		}}}}},
		// One good track does not excuse a second that is missing its tables:
		// samples are asked for per track, and the broken one would answer with
		// offsets nobody read.
		"one good track and one without tables": {Traks: []*mp4.TrakBox{full(), {}}},
	}
	for name, moov := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := usableDespite(&mp4.File{Moov: moov}, broke); !errors.Is(err, broke) {
				t.Errorf("err = %v, want the original failure", err)
			}
		})
	}
	// And the shape it does accept: every track states its sizes and offsets.
	if _, err := usableDespite(&mp4.File{Moov: &mp4.MoovBox{Traks: []*mp4.TrakBox{full()}}}, broke); err != nil {
		t.Errorf("a complete movie box was refused: %v", err)
	}
	// Co64 in place of stco is the same thing for a file over 4 GiB, which is
	// most of what this was written for.
	big := full()
	big.Mdia.Minf.Stbl.Stco, big.Mdia.Minf.Stbl.Co64 = nil, &mp4.Co64Box{}
	if _, err := usableDespite(&mp4.File{Moov: &mp4.MoovBox{Traks: []*mp4.TrakBox{big}}}, broke); err != nil {
		t.Errorf("a 64-bit offset table was refused: %v", err)
	}
}

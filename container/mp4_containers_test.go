// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-avkit/avkit/internal/mp4boxes"
)

// The walk and mp4ff agree on every box that has children, and on how many bytes
// it holds before them: the table in mp4_containers.go is the one mp4ff shows
// when it is put to the test. A new mp4ff that descends into one more box, or
// reads children at another offset, fails this until the file is regenerated
// with
//
//	AVKIT_REGEN=1 go test -run TestTheWalkDescendsIntoEveryBoxMp4ffDoes ./container
func TestTheWalkDescendsIntoEveryBoxMp4ffDoes(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Version}}", "github.com/Eyevinn/mp4ff").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dir, version, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	src, err := os.ReadFile(filepath.Join(dir, "mp4", "box.go"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AVKIT_REGEN") != "" {
		generated, err := mp4boxes.Generate(src, version)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("mp4_containers.go", generated, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("mp4_containers.go regenerated; run the tests again")
	}
	want, err := mp4boxes.Probe(src)
	if err != nil {
		t.Fatal(err)
	}
	for typ, skips := range want {
		if got, ok := mp4Containers[typ]; !ok {
			t.Errorf("mp4ff descends into %q (header bytes %v) and the walk does not", typ, skips)
		} else if !slices.Equal(got, skips) {
			t.Errorf("%q: mp4ff reads children after %v bytes, the walk after %v", typ, skips, got)
		}
	}
	for typ := range mp4Containers {
		if _, ok := want[typ]; !ok {
			t.Errorf("the walk descends into %q and mp4ff does not", typ)
		}
	}
	// The boxes the first version of the walk did not know of.
	for _, typ := range []string{"stsd", "dref", "avc1", "mp4a", "trgr", "ludt", "vexu", "wvtt", "tx3g"} {
		if _, ok := mp4Containers[typ]; !ok {
			t.Errorf("%q is not in the table", typ)
		}
	}
}

// containerBomb is typ holding n empty free boxes after its own header bytes,
// the first form it has: the shape of go-avkit/avkit#122's review, 500,000 of
// them being 120 MB.
func containerBomb(typ string, skip, n int) []byte {
	body := slices.Concat(make([]byte, skip), bytes.Repeat(box("free"), n))
	return box(typ, body)
}

// Every box mp4ff decodes the children of is walked: a flood inside it is
// refused whatever the box is, by every entry point, and none is accepted.
func TestBombInsideEveryContainerMp4ffDecodesIsRefused(t *testing.T) {
	// The boxes come from mp4ff, not from the table under test.
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/Eyevinn/mp4ff").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "mp4", "box.go"))
	if err != nil {
		t.Fatal(err)
	}
	probed, err := mp4boxes.Probe(src)
	if err != nil {
		t.Fatal(err)
	}
	for typ, skips := range probed {
		for _, skip := range skips {
			data := slices.Concat(mp4Ftyp(), containerBomb(typ, skip, 1_000))
			// A form of the header that mp4ff itself chooses by content (an audio
			// entry's version) is reached by the walk through the tiling.
			if skip == 44 {
				binary.BigEndian.PutUint16(data[len(mp4Ftyp())+8+8:], 1)
			}
			for name, call := range mp4EntryPoints(t, data, ReaderLimits{MaxBoxes: 100}) {
				if name == "NewReader" || name == "Demux" || name == "NewFileReader" || name == "OpenFile" {
					continue
				}
				if err := call(); !errors.Is(err, ErrReaderLimit) {
					t.Errorf("%s(%s after %d bytes) = %v, want ErrReaderLimit", name, typ, skip, err)
				}
			}
		}
	}
}

// The chains of the review: the flood sits where a real file keeps such a box.
func TestBombInsideTheContainersOfARealMovieIsRefused(t *testing.T) {
	frees := bytes.Repeat(box("free"), 1_000)
	movie := func(inner []byte) []byte { return slices.Concat(mp4Ftyp(), box("moov", box("trak", inner))) }
	for name, data := range map[string][]byte{
		"stsd>avc1": movie(box("mdia", box("minf", box("stbl", box("stsd", make([]byte, 8), containerBomb("avc1", 78, 1_000)))))),
		"dinf>dref": movie(box("mdia", box("minf", box("dinf", box("dref", make([]byte, 8), frees))))),
		"trak>trgr": movie(box("trgr", frees)),
		"udta>ludt": slices.Concat(mp4Ftyp(), box("moov", box("udta", box("ludt", frees)))),
		"mp4a v1":   movie(box("mdia", box("minf", box("stbl", box("stsd", make([]byte, 8), audioEntryV1(frees)))))),
	} {
		for entry, call := range mp4EntryPoints(t, data, ReaderLimits{MaxBoxes: 100}) {
			if entry == "NewReader" || entry == "Demux" || entry == "NewFileReader" || entry == "OpenFile" {
				continue
			}
			if err := call(); !errors.Is(err, ErrReaderLimit) {
				t.Errorf("%s(%s) = %v, want ErrReaderLimit", entry, name, err)
			}
		}
	}
}

// audioEntryV1 is an mp4a entry of QuickTime version 1, 44 bytes before its
// children where version 0 has 28.
func audioEntryV1(children []byte) []byte {
	head := make([]byte, 44)
	binary.BigEndian.PutUint16(head[8:], 1)
	return box("mp4a", head, children)
}

// A box with several forms is read in the one whose children tile it, and a
// version of an audio entry that mp4ff reads at 44 is not read at 28.
func TestAFormIsChosenByWhatTilesTheBox(t *testing.T) {
	w := &mp4Walk{}
	one := box("free")
	data := slices.Concat(make([]byte, 16), one, one) // children after 16 bytes
	w.at = bytes.NewReader(data)
	if got := w.formOf([]int{0, 8, 16}, 0, int64(len(data))); got != 16 {
		t.Errorf("formOf = %d, want 16", got)
	}
	if got := w.formOf([]int{0, 8}, 0, int64(len(data))); got != 0 {
		t.Errorf("formOf with no tiling form = %d, want the first, 0", got)
	}
	if got := w.formOf([]int{4}, 0, int64(len(data))); got != 4 {
		t.Errorf("formOf of a single form = %d, want 4", got)
	}
	if w.tiles(int64(len(data))+8, int64(len(data))) {
		t.Error("tiles accepted a start past the end")
	}
	if (&mp4Walk{at: shortAt{make([]byte, 4)}}).tiles(0, 100) {
		t.Error("tiles accepted a header that cannot be read")
	}
	if w.tiles(0, int64(len(data))+100) {
		t.Error("tiles accepted boxes that stop short of the end")
	}
}

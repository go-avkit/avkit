// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

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
	dir = filepath.Join(dir, "mp4")
	if os.Getenv("AVKIT_REGEN") != "" {
		generated, err := mp4boxes.Generate(dir, version)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("mp4_containers.go", generated, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("mp4_containers.go regenerated; run the tests again")
	}
	src, err := os.ReadFile(filepath.Join(dir, "box.go"))
	if err != nil {
		t.Fatal(err)
	}
	containers, err := mp4boxes.Containers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(containers, mp4Containers) {
		t.Errorf("mp4ff decodes children of %v, the walk of %v", mp4boxes.Sorted(containers), mp4boxes.Sorted(mp4Containers))
	}
	// What mp4ff is seen to do, put to the test, is within what its source can do:
	// a box the probe saw read children is in the set the source gives.
	probed, err := mp4boxes.Probe(src)
	if err != nil {
		t.Fatal(err)
	}
	for typ := range probed.Skips {
		if !mp4Containers[typ] {
			t.Errorf("mp4ff was seen to read children of %q and the walk does not descend into it", typ)
		}
	}
	if !maps.Equal(probed.Strings, mp4StringForms) {
		t.Errorf("mp4ff holds strings before the children of %v, the walk knows %v", probed.Strings, mp4StringForms)
	}
	if !maps.Equal(probed.Fixed, mp4Fixed) {
		t.Errorf("mp4ff reads children of %v from one offset, the walk knows %v", probed.Fixed, mp4Fixed)
	}
	for _, typ := range []string{"moov", "trak", "udta", "stbl", "stsd", "avc1"} {
		if _, ok := mp4Fixed[typ]; !ok {
			t.Errorf("%q has children at one offset and is not fixed", typ)
		}
	}
	for _, typ := range []string{"text", "mp4a", "meta", "stpp", "tmcd"} {
		if _, ok := mp4Fixed[typ]; ok {
			t.Errorf("%q has children at an offset its bytes choose and is fixed", typ)
		}
	}
	if !maps.Equal(probed.AnyKind, mp4AnyKind) {
		t.Errorf("mp4ff takes children of any type in %v, the walk in %v", probed.AnyKind, mp4AnyKind)
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
	for typ, skips := range probed.Skips {
		for _, skip := range skips {
			data := slices.Concat(mp4Ftyp(), containerBomb(typ, skip, 1_000))
			// A form of the header that mp4ff itself chooses by content (an audio
			// entry's version) is made to be the one it chooses.
			at := len(mp4Ftyp()) + 8 + 8
			switch skip {
			case 44:
				binary.BigEndian.PutUint16(data[at:], 1)
			case 64:
				binary.BigEndian.PutUint16(data[at:], 2)
				binary.BigEndian.PutUint32(data[at+36:], 0x7f000000)
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

// bombBoxes is the flood of each bomb: 30,000 boxes, 240 KB, against a bound of
// 1,000. The reviews measured 300,000 (2.4 MB, 46 to 74 MB decoded); the tests
// keep a tenth of it, which is as much a bomb to a walk that does not count it.
const bombBoxes = 30_000

// The re-review of go-avkit/avkit#122: boxes whose children mp4ff reads from an
// offset its own bytes choose, each built so that a walk that chose differently
// counts almost nothing, and mp4ff decodes 300,000 boxes (46 to 74 MB) from 2.4 MB.
// metaTilingAtZero is a payload that one box fills from 0, with [4:8] not hdlr,
// so that mp4ff reads a meta box's children from 4, where an 8-byte free box
// (the size 8 that is the box's type field, then "free") opens the flood.
func metaTilingAtZero(flood []byte) []byte {
	p := slices.Concat(make([]byte, 4), binary.BigEndian.AppendUint32(nil, 8), []byte("free"), flood)
	binary.BigEndian.PutUint32(p, uint32(len(p)))
	return p
}

func chosenOffsetBombs() map[string][]byte {
	flood := bytes.Repeat(box("free"), bombBoxes)
	audio := func(head []byte) []byte { return box("mp4a", head, flood) }
	v1 := make([]byte, 44)
	binary.BigEndian.PutUint16(v1[8:], 1)
	// One box fills the payload from 28, so that the ISO layout tiles it; mp4ff
	// prefers the version 1 layout, and the flood from 44.
	filling := slices.Clone(v1)
	binary.BigEndian.PutUint32(filling[28:], uint32(16+len(flood)))
	copy(filling[32:], "junk")
	v2 := make([]byte, 64)
	binary.BigEndian.PutUint16(v2[8:], 2)
	binary.BigEndian.PutUint32(v2[44:], 0x7f000000)
	// A QuickTime text entry puts its children after a font name of a length it
	// states in one byte: 51 + n. A 3GPP one, recognised by an ftab box after its
	// fields, puts them there. mp4ff decodes the same flood either way.
	text := func(n int) []byte {
		return box("text", make([]byte, 50), []byte{byte(n)}, bytes.Repeat([]byte("A"), n), flood)
	}
	return map[string][]byte{
		"text, font name of 0":             text(0),
		"text, font name of 1":             text(1),
		"text, font name of 10":            text(10),
		"text, font name of 200":           text(200),
		"text, font name of 255":           text(255),
		"text, ftab form":                  box("text", make([]byte, 38), box("ftab", make([]byte, 2)), flood),
		"gmhd, text of 10":                 box("gmhd", text(10)),
		"stsd, text of 10":                 box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, text(10)),
		"mp4a v1, a box fills from 28":     audio(filling),
		"mp4a v2, children at 64":          audio(v2),
		"mp4a v1, flood then garbage":      box("mp4a", v1, flood, []byte{1, 2, 3, 4, 5}),
		"meta, tiles at 0":                 box("meta", flood),
		"meta, one box at 0, flood from 4": box("meta", metaTilingAtZero(flood)),
		"meta, version and flags":          box("meta", make([]byte, 4), flood),
		"stpp, a namespace":                box("stpp", make([]byte, 8), []byte("urn:example:ns\x00\x00\x00"), flood),
	}
}

func TestBombsThatChooseTheOffsetAreRefused(t *testing.T) {
	for name, payload := range chosenOffsetBombs() {
		data := slices.Concat(mp4Ftyp(), payload)
		for entry, call := range mp4EntryPoints(t, data, ReaderLimits{MaxBoxes: 1_000}) {
			if entry == "NewReader" || entry == "Demux" || entry == "NewFileReader" || entry == "OpenFile" {
				continue
			}
			if err := call(); !errors.Is(err, ErrReaderLimit) {
				t.Errorf("%s(%s) = %v, want ErrReaderLimit", entry, name, err)
			}
		}
	}
}

// The strings of a stpp that run past the window leave its children unplaced: the
// box is refused, and one that ends within the window with no strings is not.
func TestAStringTooLongToBePlacedIsRefused(t *testing.T) {
	long := box("stpp", make([]byte, 8), bytes.Repeat([]byte("a"), 2*mp4OffsetWindow))
	data := slices.Concat(mp4Ftyp(), long)
	if err := checkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{}.bounded()); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("strings past the window: %v, want ErrReaderLimit", err)
	}
	short := slices.Concat(mp4Ftyp(), box("stpp", make([]byte, 8), []byte("aaaa")))
	if err := checkMP4(bytes.NewReader(short), int64(len(short)), ReaderLimits{}.bounded()); err != nil {
		t.Errorf("a box without its strings: %v", err)
	}
	// The strings are cut off by the end of what the source holds.
	cut := slices.Concat(mp4Ftyp(), box("stpp", make([]byte, 8), []byte("aaaa")))
	if err := checkMP4(shortAt{cut[:len(cut)-2]}, int64(len(cut)), ReaderLimits{}.bounded()); err != nil {
		t.Errorf("strings cut off: %v", err)
	}
}

// The scan for the end of the strings spends steps too.
func TestTheScanForStringsSpendsSteps(t *testing.T) {
	w := &mp4Walk{at: bytes.NewReader(make([]byte, 100)), steps: 93}
	// 93 steps look at the 93 offsets of a 100-byte payload; the 94th is the first
	// byte of the strings.
	if _, err := w.offsets("stpp", true, 0, 100); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("offsets with 93 steps = %v, want ErrReaderLimit", err)
	}
	if err := (&mp4Walk{at: bytes.NewReader(make([]byte, 100))}).level(0, 100, 1, false, nil, nil); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("level with no steps = %v, want ErrReaderLimit", err)
	}
	w = &mp4Walk{at: bytes.NewReader(make([]byte, 100)), steps: 10}
	if _, err := w.offsets("stpp", true, 0, 100); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("offsets with 10 steps = %v, want ErrReaderLimit", err)
	}
}

// A box too short to hold the header before its children has none to count.
func TestABoxTooShortForItsHeaderHoldsNoChildren(t *testing.T) {
	data := slices.Concat(mp4Ftyp(), box("avc1", make([]byte, 10)))
	st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{MaxBoxes: 100, MaxBoxDepth: 64})
	if err != nil || st.boxes != 2 {
		t.Errorf("walk = %+v, %v; want 2 boxes", st, err)
	}
}

// The fourth review: a flood of boxes of a type that is not printable text
// (\x01\x02\x03\x04) is decoded by mp4ff as unknown boxes, as costly as any, and
// was counted by no walk that filtered the types it looked for. It is counted
// where the children start at a fixed offset (moov, udta, stsd), and in the boxes
// whose bytes choose where they start (mp4a v0 and v1, text with a font name of 1,
// 10 or 200 bytes, meta).
func oddTypeBombs() map[string][]byte {
	odd := bytes.Repeat(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), 0x01020304), bombBoxes)
	v1 := make([]byte, 44)
	binary.BigEndian.PutUint16(v1[8:], 1)
	text := func(n int) []byte {
		return box("text", make([]byte, 50), []byte{byte(n)}, bytes.Repeat([]byte("A"), n), odd)
	}
	return map[string][]byte{
		"moov":           box("moov", odd),
		"moov>udta":      box("moov", box("udta", odd)),
		"udta":           box("udta", odd),
		"stsd":           box("moov", box("trak", box("mdia", box("minf", box("stbl", box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, box("avc1", make([]byte, 78), odd))))))),
		"mp4a v0":        box("mp4a", make([]byte, 28), odd),
		"mp4a v1":        box("mp4a", v1, odd),
		"meta":           box("meta", make([]byte, 4), odd),
		"text, name 1":   text(1),
		"text, name 10":  text(10),
		"text, name 200": text(200),
	}
}

func TestAFloodOfBoxesOfAnyTypeIsRefusedBeforeItIsDecoded(t *testing.T) {
	for name, payload := range oddTypeBombs() {
		data := slices.Concat(mp4Ftyp(), payload)
		refusedEverywhere(t, data, ReaderLimits{MaxBoxes: 1_000}, false, 1<<20)
		if n, _ := mp4ffBoxCount(data); n < bombBoxes/2 && name != "stsd" {
			t.Errorf("%s: mp4ff decoded %d boxes: not a bomb", name, n)
		}
	}
}

// audioMaze is nested audio entries whose every candidate offset parses: a free
// box over 28 to 44, another over 44 to 64, and the next entry at 64. A walk that
// follows every offset of each would take 3^depth steps; the budget stops it.
func audioMaze(depth int) []byte {
	inner := []byte(nil)
	for range depth {
		head := make([]byte, 64)
		binary.BigEndian.PutUint32(head[28:], 16)
		copy(head[32:], "free")
		binary.BigEndian.PutUint32(head[44:], 20)
		copy(head[48:], "free")
		inner = box("mp4a", head, inner)
	}
	return slices.Concat(mp4Ftyp(), inner)
}

func TestAWalkThatTakesTooManyStepsIsRefused(t *testing.T) {
	data := audioMaze(30)
	if err := checkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{MaxBoxes: 1_000}.bounded()); !errors.Is(err, ErrReaderLimit) {
		t.Errorf("the maze = %v, want ErrReaderLimit", err)
	}
	// A shallow one is within the budget, and counts what is there, not the sum of
	// the paths.
	shallow := audioMaze(4)
	st, err := walkMP4(bytes.NewReader(shallow), int64(len(shallow)), ReaderLimits{MaxBoxes: 1_000, MaxBoxDepth: 64})
	if err != nil || st.boxes != 1+4+4*2 {
		t.Errorf("the shallow maze = %+v, %v; want %d boxes", st, err, 1+4+4*2)
	}
}

// The text bombs are bombs: mp4ff decodes the flood of every one of them, from
// whichever offset its font name or its ftab puts the children at.
func TestTheTextBombsAreDecodedByMp4ff(t *testing.T) {
	for name, payload := range chosenOffsetBombs() {
		if !strings.Contains(name, "text") && !strings.Contains(name, "ftab") {
			continue
		}
		data := slices.Concat(mp4Ftyp(), payload)
		if n, _ := mp4ffBoxCount(data); n < bombBoxes {
			t.Errorf("%s: mp4ff decoded %d boxes", name, n)
		}
	}
}

// mp4ffBoxCount is the boxes mp4ff decodes the file into, counted from its own
// account of them, and the objects it allocated to do it. The count is taken from
// the tree it returns even when it returns an error with it, which leaves out the
// boxes it was building when it failed; the allocations do not leave them out, and
// are the second witness of a decode that outgrew the walk.
func mp4ffBoxCount(data []byte) (n int, mallocs uint64) {
	var f *mp4.File
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	func() {
		defer func() { _ = recover() }()
		f, _ = mp4.DecodeFile(bytes.NewReader(data))
	}()
	runtime.ReadMemStats(&after)
	mallocs = after.Mallocs - before.Mallocs
	if f == nil {
		return 0, mallocs
	}
	// ftab is left out: mp4ff makes one up for a QuickTime text entry from the
	// font name the entry holds, and it is in no box of the file.
	//
	// The boxes are counted from the tree, not from the text of Info: Info does not
	// list the unknown boxes of a container, which is how a flood of them (the
	// fourth review's) went by an oracle that counted lines.
	var count func(b mp4.Box)
	count = func(b mp4.Box) {
		if b.Type() != "ftab" {
			n++
		}
		if c, ok := b.(interface{ GetChildren() []mp4.Box }); ok {
			for _, child := range c.GetChildren() {
				count(child)
			}
		}
	}
	for _, child := range f.Children {
		count(child)
	}
	return n, mallocs
}

// mp4ffAllocsPerBox bounds the objects mp4ff allocates for a box it decodes,
// chosen far above what the testdata costs: a decode that outgrows the count by a
// factor, and not by a few boxes, is what it catches.
const mp4ffAllocsPerBox = 64

// The property the walk exists for: whatever offset mp4ff picks, the boxes it
// decodes are no more than the walk counted, and its allocations are no more
// than a box costs it, even when it fails.
func checkWalkCovers(t *testing.T, data []byte) {
	t.Helper()
	st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{MaxBoxes: 1 << 22, MaxBoxDepth: 64})
	if err != nil {
		return
	}
	n, mallocs := mp4ffBoxCount(data)
	if n > st.boxes {
		t.Fatalf("mp4ff decoded %d boxes and the walk counted %d (%d input bytes)", n, st.boxes, len(data))
	}
	if limit := mp4ffAllocsPerBox*uint64(st.boxes) + 1<<14; mallocs > limit {
		t.Fatalf("mp4ff allocated %d objects and the walk counted %d boxes (%d input bytes)", mallocs, st.boxes, len(data))
	}
}

func FuzzMP4WalkCovers(f *testing.F) {
	fuzzSeeds(f)
	for _, payload := range chosenOffsetBombs() {
		f.Add(slices.Concat(mp4Ftyp(), payload))
	}
	for _, payload := range oddTypeBombs() {
		f.Add(slices.Concat(mp4Ftyp(), payload))
	}
	f.Add(audioMaze(3))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4<<20 {
			t.Skip()
		}
		checkWalkCovers(t, data)
	})
}

// The seeds run as a test, bombs included: what mp4ff decodes is within the count.
func TestTheWalkCoversWhatMp4ffDecodes(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.mp4")
	more, _ := filepath.Glob("testdata/*.m4a")
	paths = append(paths, more...)
	more, _ = filepath.Glob("testdata/*.mov")
	paths = append(paths, more...)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		checkWalkCovers(t, data)
	}
	bombs := chosenOffsetBombs()
	maps.Copy(bombs, oddTypeBombs())
	for name, payload := range bombs {
		data := slices.Concat(mp4Ftyp(), payload)
		st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{MaxBoxes: 1 << 22, MaxBoxDepth: 64})
		n, mallocs := mp4ffBoxCount(data)
		// mp4ff may refuse the file after decoding its flood (garbage after it), and
		// the walk counted the flood all the same.
		if err != nil || n > st.boxes || st.boxes < bombBoxes || mallocs > mp4ffAllocsPerBox*uint64(st.boxes) {
			t.Errorf("%s: walk %+v (%v), mp4ff %d boxes, %d allocations", name, st, err, n, mallocs)
		}
	}
}

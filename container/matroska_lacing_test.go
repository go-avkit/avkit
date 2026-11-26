// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/at-wat/ebml-go"
	"github.com/go-avkit/avkit/internal/ebmlmasters"
)

// bombDoc is a document of one track and one cluster of blocks laced at their
// maximum, written by hand because ebml-go's own marshaller refuses 256 frames:
// each block states a lace count of 255 and holds 256 frames of frameLen bytes
// (zero or one), in the given lacing.
func bombDoc(tb testing.TB, mode ebml.LacingMode, blocks, frameLen int) []byte {
	tb.Helper()
	return bombDocIn(tb, mode, blocks, frameLen, func(blocks []byte) []byte {
		return ebmlElem(idCluster, 8, append(ebmlElem(idTimecode, 1, []byte{0}), blocks...))
	})
}

// bombDocIn is bombDoc with the blocks put where place says: in a cluster, or
// in a master element ebml-go unlaces in all the same.
func bombDocIn(tb testing.TB, mode ebml.LacingMode, blocks, frameLen int, place func(blocks []byte) []byte) []byte {
	tb.Helper()
	doc := lacedDoc(mode, [][]byte{{1}, {1}}, 20_000_000, 0)
	doc.Segment.Cluster = nil
	head := new(bytes.Buffer)
	if err := ebml.Marshal(doc, head); err != nil {
		tb.Fatalf("marshal the bomb's head: %v", err)
	}
	b := head.Bytes()
	_, w, _, _ := ebmlVint(b, 0, true)
	hsize, sw, _, _ := ebmlVint(b, w, false)
	headerEnd := w + sw + int(hsize)
	_, w, _, _ = ebmlVint(b, headerEnd, true)
	_, sw, _, _ = ebmlVint(b, headerEnd+w, false)
	body := b[headerEnd+w+sw:]

	flags := map[ebml.LacingMode]byte{ebml.LacingXiph: 0x82, ebml.LacingFixed: 0x84, ebml.LacingEBML: 0x86}[mode]
	block := []byte{0x81, 0, 0, flags, 0xFF}
	if frameLen > 0 {
		switch mode {
		case ebml.LacingXiph:
			for i := 0; i < 255; i++ {
				block = append(block, byte(frameLen))
			}
		case ebml.LacingEBML:
			block = append(block, 0x80|byte(frameLen))
			for i := 0; i < 254; i++ {
				block = append(block, 0xBF) // a difference of zero
			}
		}
		block = append(block, make([]byte, 256*frameLen)...)
	}
	var all []byte
	one := ebmlElem([]byte{0xA3}, 2, block)
	for i := 0; i < blocks; i++ {
		all = append(all, one...)
	}
	segment := append(append([]byte{}, body...), place(all)...)
	out := append([]byte{}, b[:headerEnd]...)
	return append(out, ebmlElem(idSegment, 8, segment)...)
}

var lacings = []struct {
	name string
	mode ebml.LacingMode
}{
	{"xiph", ebml.LacingXiph},
	{"fixed", ebml.LacingFixed},
	{"ebml", ebml.LacingEBML},
}

// refusedSmall reads data with limits and asserts the refusal, and that the
// refusal allocated next to nothing, under a memory limit that the unlaced file
// would not fit in.
func refusedSmall(t *testing.T, data []byte, limits ReaderLimits) {
	t.Helper()
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, err := NewReaderWithLimits(data, limits)
	runtime.ReadMemStats(&after)
	got := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d input bytes: %d bytes allocated", len(data), got)
	if r != nil || !errors.Is(err, ErrReaderLimit) {
		t.Fatalf("NewReader = (%v, %v), want ErrReaderLimit", r != nil, err)
	}
	if got > 1<<20 {
		t.Fatalf("the refusal of a %d-byte file allocated %d bytes", len(data), got)
	}
}

// The bomb of go-avkit/avkit#120: fixed lacing, a lace count of 255 and nothing
// to lace, which unlaced into 13 KiB per input byte. A frame is at least one
// byte, so the block is refused for what it states against what it holds.
func TestBombOfEmptyLacedFramesIsRefusedBeforeItIsUnlaced(t *testing.T) {
	for _, blocks := range []int{1_000, 10_000} {
		data := bombDoc(t, ebml.LacingFixed, blocks, 0)
		refusedSmall(t, data, ReaderLimits{})
	}
}

// Frames that are not empty pass the per-block check, and are held back by the
// budget: a thousand blocks past a lowered bound, ten thousand past the default.
func TestBombOfMaximalLaceCountsIsRefusedByTheFrameBudget(t *testing.T) {
	for _, l := range lacings {
		t.Run(l.name, func(t *testing.T) {
			refusedSmall(t, bombDoc(t, l.mode, 1_000, 1), ReaderLimits{MaxTrackFrames: 100_000})
			refusedSmall(t, bombDoc(t, l.mode, 10_000, 1), ReaderLimits{MaxTrackFrames: 1_000_000})
		})
	}
}

// The default itself: one frame past 1<<25 in one file, 131,073 blocks of 256.
func TestBombPastTheDefaultFrameBudgetIsRefused(t *testing.T) {
	data := bombDoc(t, ebml.LacingFixed, DefaultMaxFrames/256+1, 1)
	refusedSmall(t, data, ReaderLimits{})
}

// idBlockAdditionMapping is a master ebml-go descends into although the walk
// of the first version of this check did not: blocks put in it were unlaced
// and never counted (review of go-avkit/avkit#121, 171 MiB from 140 KB).
var idBlockAdditionMapping = []byte{0x41, 0xE4}

func inBlockAdditionMapping(blocks []byte) []byte {
	return ebmlElem(idBlockAdditionMapping, 8, blocks)
}

func TestBombInsideAMasterTheWalkOnceSkippedIsRefused(t *testing.T) {
	for _, l := range lacings {
		t.Run(l.name, func(t *testing.T) {
			data := bombDocIn(t, l.mode, 10_000, 1, inBlockAdditionMapping)
			refusedSmall(t, data, ReaderLimits{MaxTrackFrames: 1_000_000})
		})
	}
	refusedSmall(t, bombDocIn(t, ebml.LacingFixed, 10_000, 0, inBlockAdditionMapping), ReaderLimits{})
}

// Every entry point that unmarshals Matroska applies the check: Demux read the
// same bomb with no check at all.
func TestEveryMatroskaEntryPointRefusesTheBomb(t *testing.T) {
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	dir := t.TempDir()
	for name, place := range map[string]func([]byte) []byte{
		"cluster": func(b []byte) []byte {
			return ebmlElem(idCluster, 8, append(ebmlElem(idTimecode, 1, []byte{0}), b...))
		},
		"mapping": inBlockAdditionMapping,
	} {
		data := bombDocIn(t, ebml.LacingFixed, 10_000, 0, place)
		path := filepath.Join(dir, name+".mkv")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		entries := map[string]func() error{
			"Demux":           func() error { _, err := Demux(data); return err },
			"DemuxWithLimits": func() error { _, err := DemuxWithLimits(data, ReaderLimits{MaxFrames: 10}); return err },
			"NewReader":       func() error { _, err := NewReader(data); return err },
			"OpenFile": func() error {
				_, closer, err := OpenFile(path)
				if err == nil {
					closer()
				}
				return err
			},
			"OpenFileWithLimits": func() error {
				_, closer, err := OpenFileWithLimits(path, ReaderLimits{MaxFrames: 10})
				if err == nil {
					closer()
				}
				return err
			},
			"NewFileReader": func() error {
				_, err := NewFileReader(bytes.NewReader(data), int64(len(data)))
				return err
			},
		}
		for entry, call := range entries {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			err := call()
			runtime.ReadMemStats(&after)
			if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
				t.Errorf("%s(%s): %d bytes allocated", entry, name, got)
			}
			// NewFileReader reads MP4 only, and refuses the format before it
			// unmarshals anything; the others must name the limit, for the
			// empty-frame bomb is refused whatever the bounds.
			if entry == "NewFileReader" {
				if err == nil {
					t.Errorf("%s(%s) accepted a Matroska file", entry, name)
				}
			} else if !errors.Is(err, ErrReaderLimit) {
				t.Errorf("%s(%s) = %v, want ErrReaderLimit", entry, name, err)
			}
		}
	}
}

// The walk and ebml-go agree on every master: the sets generated into
// matroska_masters.go are the ones in the module's own element table. A new
// ebml-go that adds a master fails this until the file is regenerated with
//
//	AVKIT_REGEN=1 go test -run TestTheWalkDescendsIntoEveryMasterEbmlGoDoes ./container
func TestTheWalkDescendsIntoEveryMasterEbmlGoDoes(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Version}}", "github.com/at-wat/ebml-go").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dir, version, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	src, err := os.ReadFile(filepath.Join(dir, "elementtable.go"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AVKIT_REGEN") != "" {
		generated, err := ebmlmasters.Generate(src, version)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("matroska_masters.go", generated, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("matroska_masters.go regenerated; run the tests again")
	}
	sets, err := ebmlmasters.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want []uint64
		got  map[uint64]bool
	}{{"masters", sets.Masters, ebmlMasters}, {"blocks", sets.Blocks, ebmlBlocks}, {"known", sets.Known, ebmlKnown}} {
		if len(c.want) != len(c.got) {
			t.Errorf("%s: ebml-go has %d, the generated set %d", c.name, len(c.want), len(c.got))
		}
		for _, id := range c.want {
			if !c.got[id] {
				t.Errorf("%s: 0x%X is missing from the generated set", c.name, id)
			}
		}
	}
	if !ebmlMasters[0x41E4] {
		t.Error("BlockAdditionMapping is not a master")
	}
}

// The total budget is the one that stops frames spread over several tracks.
func TestFramesOfManyTracksAreBoundedInTotal(t *testing.T) {
	data := bombDoc(t, ebml.LacingFixed, 100, 1) // 25,600 frames of one track
	if _, err := NewReaderWithLimits(data, ReaderLimits{MaxFrames: 25_599}); !errors.Is(err, ErrReaderLimit) {
		t.Fatalf("one frame past the total = %v, want ErrReaderLimit", err)
	}
	if _, err := NewReaderWithLimits(data, ReaderLimits{MaxFrames: 25_600, MaxTrackFrames: 25_600}); err != nil {
		t.Fatalf("exactly the total: %v", err)
	}
	if _, err := NewReaderWithLimits(data, ReaderLimits{MaxTrackFrames: 25_599}); !errors.Is(err, ErrReaderLimit) {
		t.Fatalf("one frame past the track = %v, want ErrReaderLimit", err)
	}
}

// A caller can only lower a bound: asking for more is the default again.
func TestAReaderLimitCanOnlyBeLowered(t *testing.T) {
	defaults := ReaderLimits{
		MaxTrackFrames: DefaultMaxTrackFrames, MaxFrames: DefaultMaxFrames,
		MaxBoxes: DefaultMaxBoxes, MaxBoxDepth: DefaultMaxBoxDepth,
	}
	const big = 1<<31 - 1
	for _, c := range []struct{ in, want ReaderLimits }{
		{ReaderLimits{}, defaults},
		{ReaderLimits{-1, -1, -1, -1}, defaults},
		{ReaderLimits{big, big, big, big}, defaults},
		{ReaderLimits{10, 20, 30, 4}, ReaderLimits{10, 20, 30, 4}},
	} {
		if got := c.in.bounded(); got != c.want {
			t.Errorf("%+v.bounded() = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// A file that holds what it states reads as it did: the check counts what the
// reader then returns, on every Matroska file of the testdata that carries
// frames of its own.
func TestRealMatroskaFilesReadUnchanged(t *testing.T) {
	for _, name := range []string{"tiny.mkv", "bframes.mkv", "ffmpeg-opus.mkv", "ffmpeg-aaclc.mkv", "ffmpeg-vorbis.mkv", "ffmpeg-flac.mkv"} {
		t.Run(name, func(t *testing.T) {
			data := fixture(t, name)
			counted, err := countMatroskaFrames(data, ReaderLimits{}.bounded())
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			r, err := NewReader(data)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if len(counted) == 0 {
				t.Fatal("no frame was counted")
			}
			for _, id := range r.TrackIDs() {
				samples, err := r.Samples(id)
				if err != nil {
					t.Fatalf("Samples(%d): %v", id, err)
				}
				if got := counted[uint64(id)]; got != len(samples) {
					t.Errorf("track %d: %d frames counted, %d samples read", id, got, len(samples))
				}
			}
		})
	}
}

// Real WebM, too: a file the muxers here write.
func TestAWebMOfTheMuxersReadsUnchanged(t *testing.T) {
	data := marshalDoc(t, webmDoc())
	counted, err := countMatroskaFrames(data, ReaderLimits{}.bounded())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for _, id := range r.TrackIDs() {
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		if counted[uint64(id)] != len(samples) || len(samples) == 0 {
			t.Errorf("track %d: %d counted, %d read", id, counted[uint64(id)], len(samples))
		}
	}
}

func BenchmarkNewReaderWebM(b *testing.B) {
	data, err := os.ReadFile("testdata/subtitles-webvtt.webm")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewReader(data); err != nil {
			b.Fatal(err)
		}
	}
}

// A block that states nothing the walk can follow is left to ebml-go, which
// says what is wrong with it: the walk's silence is not acceptance.
func TestMalformedBlocksAreLeftToTheParser(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		{0xA3},
		{0xA3, 0x81},
		{0xA3, 0x82, 0x81, 0x00},
		{0xA3, 0x84, 0x81, 0x00, 0x00, 0x02}, // laced, no count byte
	} {
		if err := checkMatroskaFrames(data, ReaderLimits{}); err != nil {
			t.Errorf("checkMatroskaFrames(% x) = %v", data, err)
		}
	}
}

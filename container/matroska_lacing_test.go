// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/at-wat/ebml-go"
)

// bombDoc is a document of one track and one cluster of blocks laced at their
// maximum, written by hand because ebml-go's own marshaller refuses 256 frames:
// each block states a lace count of 255 and holds 256 frames of frameLen bytes
// (zero or one), in the given lacing.
func bombDoc(tb testing.TB, mode ebml.LacingMode, blocks, frameLen int) []byte {
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
	cluster := ebmlElem(idTimecode, 1, []byte{0})
	one := ebmlElem([]byte{0xA3}, 2, block)
	for i := 0; i < blocks; i++ {
		cluster = append(cluster, one...)
	}
	segment := append(append([]byte{}, body...), ebmlElem(idCluster, 8, cluster)...)
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
			refusedSmall(t, bombDoc(t, l.mode, 10_000, 1), ReaderLimits{})
		})
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
	for _, c := range []struct{ in, want ReaderLimits }{
		{ReaderLimits{}, ReaderLimits{DefaultMaxTrackFrames, DefaultMaxFrames}},
		{ReaderLimits{-1, -1}, ReaderLimits{DefaultMaxTrackFrames, DefaultMaxFrames}},
		{ReaderLimits{1<<31 - 1, 1<<31 - 1}, ReaderLimits{DefaultMaxTrackFrames, DefaultMaxFrames}},
		{ReaderLimits{10, 20}, ReaderLimits{10, 20}},
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

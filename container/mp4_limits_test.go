// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The bombs of go-avkit/avkit#113: an MP4 whose cost is in what it states, not
// in the bytes it holds.

func mp4Ftyp() []byte {
	return box("ftyp", []byte("isom"), make([]byte, 4), []byte("isom"))
}

// freeFlood is an ftyp and n empty 8-byte free boxes: 1,048,576 of them were
// 8 MiB and 270 MB allocated.
func freeFlood(n int) []byte {
	data := mp4Ftyp()
	free := box("free")
	return slices.Concat(data, bytes.Repeat(free, n))
}

// nested is depth boxes of the given type inside one another, the innermost
// empty.
func nested(typ string, depth int) []byte {
	// Written outermost first: box i states 8 bytes for itself and the boxes in it.
	out := mp4Ftyp()
	for i := range depth {
		out = binary.BigEndian.AppendUint32(out, uint32(8*(depth-i)))
		out = append(out, typ...)
	}
	return out
}

// tableClaim is a movie whose stts states count entries in 16 bytes: mp4ff
// refuses it by itself against the size of the box, without the walk.
func tableClaim(count uint32) []byte {
	payload := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 0), count)
	return slices.Concat(mp4Ftyp(), box("moov", box("trak", box("mdia", box("minf", box("stbl", box("stts", payload)))))))
}

// mp4EntryPoints calls every way of reading an MP4 with the given bounds.
func mp4EntryPoints(t *testing.T, data []byte, limits ReaderLimits) map[string]func() error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bomb.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func(l ReaderLimits) func() error {
		return func() error {
			r, closer, err := OpenFileWithLimits(path, l)
			if err == nil {
				closer()
				_ = r
			}
			return err
		}
	}
	return map[string]func() error{
		"NewReader":           func() error { _, err := NewReader(data); return err },
		"NewReaderWithLimits": func() error { _, err := NewReaderWithLimits(data, limits); return err },
		"Demux":               func() error { _, err := Demux(data); return err },
		"DemuxWithLimits":     func() error { _, err := DemuxWithLimits(data, limits); return err },
		"NewFileReader":       func() error { _, err := NewFileReader(bytes.NewReader(data), int64(len(data))); return err },
		"NewFileReaderWithLimits": func() error {
			_, err := NewFileReaderWithLimits(bytes.NewReader(data), int64(len(data)), limits)
			return err
		},
		"OpenFile": func() error {
			r, c, err := OpenFile(path)
			if err == nil {
				c()
				_ = r
			}
			return err
		},
		"OpenFileWithLimits": open(limits),
	}
}

// refusedEverywhere asserts that every entry point that takes the limits (and
// the ones that apply the defaults, when defaults is set) refuses data with
// ErrReaderLimit, allocating under maxAlloc, under a memory limit the decode of
// the bomb would not fit in.
func refusedEverywhere(t *testing.T, data []byte, limits ReaderLimits, defaults bool, maxAlloc uint64) {
	t.Helper()
	old := debug.SetMemoryLimit(512 << 20)
	defer debug.SetMemoryLimit(old)
	for name, call := range mp4EntryPoints(t, data, limits) {
		isDefault := name == "NewReader" || name == "Demux" || name == "NewFileReader" || name == "OpenFile"
		if isDefault && !defaults {
			continue
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		err := call()
		runtime.ReadMemStats(&after)
		got := after.TotalAlloc - before.TotalAlloc
		t.Logf("%s: %d input bytes, %d bytes allocated", name, len(data), got)
		if !errors.Is(err, ErrReaderLimit) {
			t.Errorf("%s = %v, want ErrReaderLimit", name, err)
		}
		if got > maxAlloc {
			t.Errorf("%s: the refusal of a %d-byte file allocated %d bytes, over %d", name, len(data), got, maxAlloc)
		}
	}
}

func TestBombOfManyBoxesIsRefusedBeforeItIsParsed(t *testing.T) {
	// 100,001 boxes against a bound of 100,000, and the file of the issue,
	// 1<<20 + 1 empty boxes, against the default.
	refusedEverywhere(t, freeFlood(100_001), ReaderLimits{MaxBoxes: 100_000}, false, 1<<20)
	refusedEverywhere(t, freeFlood(DefaultMaxBoxes+1), ReaderLimits{}, true, 1<<20)
}

func TestBombOfDeepNestingIsRefusedBeforeItIsParsed(t *testing.T) {
	refusedEverywhere(t, nested("moov", 10), ReaderLimits{MaxBoxDepth: 8}, false, 1<<20)
	// Past the default, where mp4ff refuses it itself with another error after
	// 0.8 MB: now it is the limit, and nothing is allocated.
	refusedEverywhere(t, nested("moov", 100_000), ReaderLimits{}, true, 1<<20)
}

// A table that states more entries than its bytes hold is not a bound of ours:
// mp4ff refuses it against the size of its box, after a few KB. This pins the
// premise the README states, so that an mp4ff that stopped checking fails here.
func TestATableStatingMoreThanItHoldsIsRefusedByTheDecode(t *testing.T) {
	data := tableClaim(1<<32 - 1)
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := NewReader(data)
	runtime.ReadMemStats(&after)
	if err == nil || errors.Is(err, ErrReaderLimit) {
		t.Fatalf("NewReader = %v, want the decode's refusal", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Errorf("a %d-byte file allocated %d bytes", len(data), got)
	}
}

// A bound is a count that may be reached: one box or one level past it is
// refused, and the count itself is read.
func TestTheBoundsAreInclusive(t *testing.T) {
	cases := []struct {
		name   string
		data   func(n int) []byte
		limits func(n int) ReaderLimits
	}{
		{"boxes", func(n int) []byte { return freeFlood(n - 1) }, func(n int) ReaderLimits { return ReaderLimits{MaxBoxes: n} }},
		{"depth", func(n int) []byte { return nested("moov", n) }, func(n int) ReaderLimits { return ReaderLimits{MaxBoxDepth: n} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const n = 20
			if err := checkMP4(bytes.NewReader(c.data(n)), int64(len(c.data(n))), c.limits(n).bounded()); err != nil {
				t.Errorf("exactly %d: %v", n, err)
			}
			if err := checkMP4(bytes.NewReader(c.data(n+1)), int64(len(c.data(n+1))), c.limits(n).bounded()); !errors.Is(err, ErrReaderLimit) {
				t.Errorf("%d: %v, want ErrReaderLimit", n+1, err)
			}
		})
	}
}

// The walk reads the boxes the decode does: a container the decode descends into
// is walked, and a box it keeps as bytes is not.
func TestTheWalkCountsWhatItDescendsInto(t *testing.T) {
	data := slices.Concat(mp4Ftyp(),
		box("moov", box("mvhd", make([]byte, 100)), box("trak", box("tkhd", make([]byte, 80))), box("meta", make([]byte, 4), box("hdlr"))),
		box("mdat", box("free"), box("free")), // media, not boxes
	)
	st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{}.bounded())
	if err != nil {
		t.Fatal(err)
	}
	// ftyp, moov, mvhd, trak, tkhd, meta, hdlr, mdat
	if st.boxes != 8 || st.depth != 3 {
		t.Errorf("stats = %+v, want 8 boxes and depth 3", st)
	}
}

// Headers the walk cannot make sense of end the level without an error: the
// decode judges them, and says what is wrong.
func TestTheWalkLeavesMalformedHeadersToTheDecode(t *testing.T) {
	for name, data := range map[string][]byte{
		"size below a header":      binary.BigEndian.AppendUint32(nil, 4),
		"size past the file":       slices.Concat(box("free"), binary.BigEndian.AppendUint32(nil, 1<<30), []byte("free")),
		"wide size cut short":      slices.Concat(binary.BigEndian.AppendUint32(nil, 1), []byte("free"), []byte{0, 0, 0, 0}),
		"wide size below a header": slices.Concat(binary.BigEndian.AppendUint32(nil, 1), []byte("free"), binary.BigEndian.AppendUint64(nil, 8)),
		"wide size past the file":  slices.Concat(binary.BigEndian.AppendUint32(nil, 1), []byte("free"), binary.BigEndian.AppendUint64(nil, 1<<40)),
		"to the end of the file":   slices.Concat(binary.BigEndian.AppendUint32(nil, 0), []byte("mdat"), make([]byte, 20)),
		"a wide box":               slices.Concat(binary.BigEndian.AppendUint32(nil, 1), []byte("free"), binary.BigEndian.AppendUint64(nil, 16)),
	} {
		if err := checkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{}.bounded()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := checkMP4(failingAt{}, 100, ReaderLimits{}.bounded()); err != nil {
		t.Errorf("an unreadable file: %v", err)
	}
	// A wide header whose second half cannot be read.
	hdr := slices.Concat(binary.BigEndian.AppendUint32(nil, 1), []byte("free"), make([]byte, 8))
	if err := checkMP4(shortAt{hdr[:8]}, 16, ReaderLimits{}.bounded()); err != nil {
		t.Errorf("a wide header cut off: %v", err)
	}
}

type failingAt struct{}

func (failingAt) ReadAt([]byte, int64) (int, error) { return 0, errors.New("unreadable") }

// shortAt is a file that ends after its first bytes although it states more.
type shortAt struct{ data []byte }

func (s shortAt) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(b, s.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// A real MP4 reads as it did: the check counts, it does not alter the read.
func TestRealMP4FilesReadUnchanged(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.mp4")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixture: %v", err)
	}
	more, _ := filepath.Glob("testdata/*.m4a")
	paths = append(paths, more...)
	more, _ = filepath.Glob("testdata/*.mov")
	paths = append(paths, more...)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		st, err := walkMP4(bytes.NewReader(data), int64(len(data)), ReaderLimits{}.bounded())
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if st.boxes == 0 || st.boxes > DefaultMaxBoxes/1000 || st.depth > 16 {
			t.Errorf("%s: stats %+v are not those of an ordinary file", p, st)
		}
		r, err := NewReader(data)
		if err != nil {
			// Not every fixture is an MP4 NewReader reads (tiny edge cases); then
			// the walk must not be what refused it.
			if errors.Is(err, ErrReaderLimit) {
				t.Errorf("%s refused by a limit: %v", p, err)
			}
			continue
		}
		low := ReaderLimits{MaxBoxes: st.boxes, MaxBoxDepth: st.depth}
		r2, err := NewReaderWithLimits(data, low)
		if err != nil {
			t.Errorf("%s: limits at exactly what the file holds: %v", p, err)
			continue
		}
		if len(r.File().Tracks) != len(r2.File().Tracks) {
			t.Errorf("%s: tracks differ", p)
		}
		for _, id := range r.TrackIDs() {
			a, _ := r.Samples(id)
			b, _ := r2.Samples(id)
			if len(a) != len(b) {
				t.Errorf("%s track %d: %d vs %d samples", p, id, len(a), len(b))
			}
		}
	}
}

// The default limits are those the documentation states; a lowered one is kept
// and a raised one is not (TestAReaderLimitCanOnlyBeLowered covers the rest).
func TestMP4DefaultsAreTheDocumentedOnes(t *testing.T) {
	if DefaultMaxBoxDepth != mp4.MaxBoxDepth {
		t.Errorf("DefaultMaxBoxDepth = %d, mp4ff's bound is %d", DefaultMaxBoxDepth, mp4.MaxBoxDepth)
	}
}

// The amplification the README states: what NewReader allocates per input byte
// of the worst file the defaults let through, a flood of empty boxes.
const mp4AmplificationBound = 400 // bytes allocated per input byte

func TestAllocationPerInputByteStaysWithinTheDocumentedBound(t *testing.T) {
	data := freeFlood(DefaultMaxBoxes - 8)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := NewReader(data)
	runtime.ReadMemStats(&after)
	got := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d input bytes (%d boxes): %d bytes allocated, %.1f per input byte, error %v",
		len(data), DefaultMaxBoxes-8, got, float64(got)/float64(len(data)), err)
	if errors.Is(err, ErrReaderLimit) {
		t.Fatalf("a file at the box bound was refused: %v", err)
	}
	if got > mp4AmplificationBound*uint64(len(data)) {
		t.Errorf("%d bytes allocated for %d input bytes: %.1f per byte, over the documented %d",
			got, len(data), float64(got)/float64(len(data)), mp4AmplificationBound)
	}
}

// BenchmarkNewReaderMP4 reads a real MP4 from memory: the fixture, or the file
// AVKIT_BENCH_MP4 names, a large one of a library for instance.
func BenchmarkNewReaderMP4(b *testing.B) {
	path := "testdata/ffmpeg-aaclc.mp4"
	if p := os.Getenv("AVKIT_BENCH_MP4"); p != "" {
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewReader(data); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzMP4Limits(f *testing.F) {
	fuzzSeeds(f)
	f.Add(freeFlood(1000))
	f.Add(nested("moov", 70))
	f.Add(tableClaim(1<<32 - 1))
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() {
			low := ReaderLimits{MaxBoxes: 64, MaxBoxDepth: 8}
			_, errLow := NewReaderWithLimits(data, low)
			_, errDef := NewReader(data)
			// A file the defaults refuse is refused under lower bounds too.
			if errors.Is(errDef, ErrReaderLimit) && !errors.Is(errLow, ErrReaderLimit) {
				t.Fatalf("refused by the defaults (%v) and not by lower bounds (%v)", errDef, errLow)
			}
		})
	})
}

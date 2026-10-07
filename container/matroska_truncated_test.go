package container

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// truncatedFixture writes a WebM holding n samples over many clusters, which is
// what a recording looks like and what the single-cluster testdata file cannot
// show: a cut should cost the LAST cluster, not the file.
func truncatedFixture(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf, ClusterDuration(30*time.Millisecond))
	id, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "vp09", Timescale: 90000,
		Width: 320, Height: 180, VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := m.WriteSample(id, Sample{Data: bytes.Repeat([]byte{byte(i)}, 64),
			Duration: 3000, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// What the refusal used to cost: everything. A file cut at 50% gave nothing at
// all, where the loss should be proportionate to the cut.
func TestTruncatedMatroskaKeepsWhatCameBeforeTheCut(t *testing.T) {
	const n = 120
	data := truncatedFixture(t, n)
	whole, err := NewReader(data)
	if err != nil {
		t.Fatalf("the whole file must read: %v", err)
	}
	if whole.Truncated() {
		t.Error("a whole file reports itself truncated")
	}
	if ws, _ := whole.Samples(whole.TrackIDs()[0]); len(ws) != n {
		t.Fatalf("the whole file gives %d samples, want %d", len(ws), n)
	}

	// Each cut must keep roughly its own share, and never more than the file.
	for _, c := range []struct{ frac, atLeast int }{{90, 100}, {75, 80}, {50, 50}, {25, 20}} {
		cut := len(data) * c.frac / 100
		r, err := NewReader(data[:cut])
		if err != nil && !errors.Is(err, ErrTruncated) {
			t.Errorf("cut at %d%%: refused with %v", c.frac, err)
			continue
		}
		if r == nil {
			t.Errorf("cut at %d%%: ErrTruncated with no reader", c.frac)
			continue
		}
		got, _ := r.Samples(r.TrackIDs()[0])
		pct := 100 * len(got) / n
		t.Logf("cut at %3d%%: %3d/%d samples (%d%%), Truncated=%v", c.frac, len(got), n, pct, r.Truncated())
		if pct < c.atLeast {
			t.Errorf("cut at %d%% kept %d%% of the samples; want at least %d%%", c.frac, pct, c.atLeast)
		}
		if len(got) > n {
			t.Errorf("cut at %d%% gave %d samples, more than the whole file", c.frac, len(got))
		}
	}
}

// The sentinel comes WITH a reader, and the plain `if err != nil` caller is
// left exactly as strict as before. That is the whole design: the safe
// behaviour stays the default.
func TestTruncatedMatroskaStillReturnsAnErrorForAnOrdinaryCaller(t *testing.T) {
	data := truncatedFixture(t, 120)
	r, err := NewReader(data[:len(data)/2])
	if err == nil {
		t.Fatal("a truncated file returned no error; every existing caller would now accept it")
	}
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("error does not wrap ErrTruncated: %v", err)
	}
	if r == nil {
		t.Fatal("no reader came with the sentinel, so the error cannot be acted on")
	}
	if !r.Truncated() {
		t.Error("the reader does not report itself truncated, so nothing downstream can see it")
	}
}

// The negative that matters: rubbish in the MIDDLE is corruption, not
// truncation, and must still be refused outright.
func TestDamageInTheMiddleIsNotReportedAsWhole(t *testing.T) {
	const n = 120
	data := truncatedFixture(t, n)
	// Deleting bytes from the middle desynchronises every size after them, so
	// nothing downstream lands on an element boundary again. Clobbering a
	// block's PAYLOAD would not do: those bytes are opaque and the file stays
	// well formed, which a first attempt at this test found out by passing.
	at := len(data) / 2
	bad := append(bytes.Clone(data[:at]), data[at+7:]...)

	r, err := NewReader(bad)
	if err == nil {
		t.Fatal("a damaged file was reported as whole")
	}
	if !errors.Is(err, ErrTruncated) {
		return // refused outright, which is also correct
	}
	if r == nil {
		t.Fatal("ErrTruncated with no reader")
	}
	got, _ := r.Samples(r.TrackIDs()[0])
	t.Logf("damaged at %d: %d/%d samples kept", at, len(got), n)
	if len(got) >= n {
		t.Errorf("a damaged file gave %d of %d samples: the damage was not noticed at all", len(got), n)
	}
}

// segmentChildOffset returns where the Segment's first child starts.
func segmentChildOffset(t *testing.T, data []byte) int {
	t.Helper()
	for i := 0; i < len(data); {
		id, idWidth, _, ok := ebmlVint(data, i, true)
		if !ok {
			t.Fatal("no Segment in the fixture")
		}
		size, szWidth, unknown, ok := ebmlVint(data, i+idWidth, false)
		if !ok {
			t.Fatal("unreadable size in the fixture")
		}
		if id == ebmlIDSegment {
			return i + idWidth + szWidth
		}
		if unknown {
			t.Fatal("unexpected unknown size before the Segment")
		}
		i += idWidth + szWidth + int(size)
	}
	t.Fatal("no Segment in the fixture")
	return 0
}

// A truncated input must not become an output that claims to be whole.
func TestRemuxRefusesATruncatedSourceUnlessAsked(t *testing.T) {
	data := truncatedFixture(t, 120)
	r, err := NewReader(data[:len(data)*3/4])
	if err != nil && !errors.Is(err, ErrTruncated) {
		t.Fatalf("setup: %v", err)
	}
	if err := Remux(io.Discard, r); err == nil {
		t.Fatal("Remux wrote a truncated source without being asked")
	} else if !errors.Is(err, ErrTruncated) {
		t.Fatalf("Remux refused for the wrong reason: %v", err)
	}
	// With the option, the guard must let it through. Whether the remux then
	// succeeds is a different question and deliberately not asserted here: this
	// fixture's VP9 track has no level, which is issue #31, and entangling the
	// two would make this test fail for a reason it is not about.
	if err := Remux(io.Discard, r, AllowTruncated()); errors.Is(err, ErrTruncated) {
		t.Fatalf("AllowTruncated did not lift the refusal: %v", err)
	}
}

func TestEbmlVintReadsWidthsAndTheUnknownSize(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []byte
		keep    bool
		value   uint64
		width   int
		unknown bool
		ok      bool
	}{
		{"one-byte size", []byte{0x84}, false, 4, 1, false, true},
		{"two-byte size", []byte{0x40, 0x7F}, false, 127, 2, false, true},
		{"unknown size", []byte{0xFF}, false, 0x7F, 1, true, true},
		{"four-byte id keeps its marker", []byte{0x1A, 0x45, 0xDF, 0xA3}, true, 0x1A45DFA3, 4, false, true},
		{"a leading zero is refused", []byte{0x00, 0x01}, false, 0, 0, false, false},
		{"a width past the end is refused", []byte{0x40}, false, 0, 0, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, w, unk, ok := ebmlVint(c.in, 0, c.keep)
			if ok != c.ok || (ok && (v != c.value || w != c.width || unk != c.unknown)) {
				t.Errorf("ebmlVint(%x) = (%#x, %d, %v, %v); want (%#x, %d, %v, %v)",
					c.in, v, w, unk, ok, c.value, c.width, c.unknown, c.ok)
			}
		})
	}
}

// Rewriting a size keeps the width the file already used, so nothing after it
// has to move; and the all-ones pattern is refused because it means "unknown".
func TestPutEbmlSizeKeepsTheWidth(t *testing.T) {
	b := make([]byte, 8)
	if !putEbmlSize(b, 300, 4) {
		t.Fatal("a four-byte write was refused")
	}
	v, w, unk, ok := ebmlVint(b, 0, false)
	if !ok || v != 300 || w != 4 || unk {
		t.Fatalf("read back (%d, %d, %v, %v); want (300, 4, false, true)", v, w, unk, ok)
	}
	if putEbmlSize(b, 1<<7-1, 1) {
		t.Error("the unknown-size pattern was written as a length")
	}
	if putEbmlSize(b[:2], 1, 4) {
		t.Error("a write past the end of the buffer was allowed")
	}
}

// --- malformed buffers, driven straight at the walk -------------------------
//
// These are the defensive branches. Driving them through a muxer would need a
// muxer willing to write rubbish, so the bytes are built here instead: it is
// the same code under test and the malformation is visible in the test.

// ebmlElem builds one element: an ID written as-is, then a size of the given
// width, then the payload.
func ebmlElem(id []byte, width int, payload []byte) []byte {
	size := make([]byte, width)
	if !putEbmlSize(size, uint64(len(payload)), width) {
		panic("test: size does not fit the width asked for")
	}
	out := append([]byte{}, id...)
	out = append(out, size...)
	return append(out, payload...)
}

var (
	idEBMLHeader = []byte{0x1A, 0x45, 0xDF, 0xA3}
	idSegment    = []byte{0x18, 0x53, 0x80, 0x67}
	idInfo       = []byte{0x15, 0x49, 0xA9, 0x66}
	idCluster    = []byte{0x1F, 0x43, 0xB6, 0x75}
	idTimecode   = []byte{0xE7}
)

func TestCompleteMatroskaPrefixRefusesWhatItCannotRescue(t *testing.T) {
	header := ebmlElem(idEBMLHeader, 1, []byte{0x01})
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"a leading zero is not an id", []byte{0x00, 0x01}},
		{"an id with no size after it", []byte{0xEC}},
		{"a first element whose size runs past the end",
			append(append([]byte{}, idEBMLHeader...), 0x50, 0xFF)},
		{"a first element of unknown size",
			append(append([]byte{}, idEBMLHeader...), 0xFF)},
		{"something that is neither header nor Segment",
			ebmlElem(idInfo, 1, []byte{0x01})},
		{"a header but no Segment", header},
		{"a Segment header cut before any child",
			append(header, append(append([]byte{}, idSegment...), 0x41)...)},
		{"a Segment with not one complete child",
			append(header, append(append(append([]byte{}, idSegment...), 0x50, 0x10), idInfo...)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := completeMatroskaPrefix(c.data); ok {
				t.Error("claimed to rescue a prefix from bytes that hold none")
			}
		})
	}
}

// A file that is whole is not truncation, and saying so would send every caller
// down the wrong branch.
func TestCompleteMatroskaPrefixSaysNothingWasDropped(t *testing.T) {
	info := ebmlElem(idInfo, 1, []byte{0x01, 0x02})
	seg := ebmlElem(idSegment, 2, info)
	data := append(ebmlElem(idEBMLHeader, 1, []byte{0x01}), seg...)
	if _, ok := completeMatroskaPrefix(data); ok {
		t.Error("a complete file was reported as truncated")
	}
}

// The Segment's declared end is the limit: children are not looked for past it,
// so a Segment that declares less than it contains cannot drag bytes that are
// not its own into the prefix.
func TestWalkStopsAtTheDeclaredSegmentEnd(t *testing.T) {
	info := ebmlElem(idInfo, 1, []byte{0x01, 0x02})
	seg := ebmlElem(idSegment, 2, info)
	trailing := ebmlElem(idInfo, 1, []byte{0x09})
	data := append(append(ebmlElem(idEBMLHeader, 1, []byte{0x01}), seg...), trailing...)
	prefix, ok := completeMatroskaPrefix(data)
	if !ok {
		t.Fatal("the trailing bytes should have been seen as a cut")
	}
	if len(prefix) >= len(data) {
		t.Errorf("prefix is %d of %d bytes: the walk went past the Segment", len(prefix), len(data))
	}
}

func TestWalkSegmentChildrenStopsAtEachMalformation(t *testing.T) {
	cluster := func(payload []byte) []byte {
		return append(append(append([]byte{}, idCluster...), 0xFF), payload...) // unknown size
	}
	good := ebmlElem(idInfo, 1, []byte{0x01})
	for _, c := range []struct {
		name string
		tail []byte
	}{
		{"a child whose id is a zero byte", []byte{0x00}},
		{"a child with no size after its id", []byte{0xEC}},
		{"a child whose size runs past the end", append(append([]byte{}, idInfo...), 0x50, 0xFF)},
		{"a grandchild whose id is a zero byte", cluster([]byte{0x00})},
		{"a grandchild of unknown size", cluster(append(append([]byte{}, idTimecode...), 0xFF))},
		{"a grandchild whose size runs past the end",
			cluster(append(append([]byte{}, idTimecode...), 0x50, 0xFF))},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := append(append([]byte{}, good...), c.tail...)
			if end := walkSegmentChildren(data, 0); end != len(good) {
				t.Errorf("walk ended at %d; want %d, just past the one whole child", end, len(good))
			}
		})
	}
}

// A prefix that cuts cleanly but still means nothing to the parser must report
// the ORIGINAL error, not a truncation the caller could act on.
func TestAPrefixTheParserStillRefusesReportsTheOriginalError(t *testing.T) {
	// A Segment holding one complete element that is not a Matroska child, and
	// trailing bytes so the walk sees a cut.
	odd := ebmlElem([]byte{0xBF}, 1, []byte{0x01, 0x02}) // CRC-32, legal but not a Segment child
	seg := ebmlElem(idSegment, 2, odd)
	data := append(append(ebmlElem(idEBMLHeader, 1, []byte{0x01}), seg...), 0xEC)
	_, err := newMatroskaReader(data)
	if err == nil {
		t.Fatal("rubbish was accepted")
	}
	if errors.Is(err, ErrTruncated) {
		t.Errorf("reported as truncated, which promises a usable reader: %v", err)
	}
}

// With several inputs the refusal names which one, because the whole difficulty
// of a truncated file is that nothing downstream can see it.
func TestJoinAndConcatNameTheTruncatedInput(t *testing.T) {
	data := truncatedFixture(t, 120)
	whole, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	part, err := NewReader(data[:len(data)*3/4])
	if err != nil && !errors.Is(err, ErrTruncated) {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"Concat", func() error { return Concat(io.Discard, []*Reader{whole, part}) }},
		{"Join", func() error { return Join(io.Discard, []*Reader{whole, part}) }},
		{"JoinProgressive", func() error { return JoinProgressive(io.Discard, []*Reader{whole, part}) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("did not refuse the truncated input: %v", err)
			}
			if !strings.Contains(err.Error(), "input 2") {
				t.Errorf("the refusal does not say which input: %v", err)
			}
		})
	}
}

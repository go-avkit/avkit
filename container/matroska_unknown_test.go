// Copyright (c) the go-avkit/avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/at-wat/ebml-go"
)

// The documents of go-avkit/avkit#129: a WebM whose video track carries
// FrameRate (0x2383E3, deprecated, RFC 9559 Appendix A.26) and whose segment
// carries an element of an ID nobody assigned, with children. ebml-go cannot
// marshal an ID it does not know, so the fixture is written with IDs it does
// know that take the same number of bytes, and the IDs are swapped in the
// encoded bytes afterwards.
type unknownVideo struct {
	PixelWidth  uint64  `ebml:"PixelWidth"`
	PixelHeight uint64  `ebml:"PixelHeight"`
	FrameRate   float64 `ebml:"TrackTimestampScale"` // 23 31 4F, swapped to 23 83 E3
}

type unknownTrack struct {
	TrackNumber uint64        `ebml:"TrackNumber"`
	TrackType   uint64        `ebml:"TrackType"`
	CodecID     string        `ebml:"CodecID"`
	Video       *unknownVideo `ebml:"Video"`
}

type unknownMaster struct {
	Void []byte `ebml:"Void"`
}

type unknownSegment struct {
	Info   mkvInfo `ebml:"Info"`
	Tracks struct {
		TrackEntry []unknownTrack `ebml:"TrackEntry"`
	} `ebml:"Tracks"`
	Stray   unknownMaster `ebml:"Cues"` // 1C 53 BB 6B, swapped to 1C 53 BB 6C
	Cluster []mkvCluster  `ebml:"Cluster"`
}

type unknownDoc struct {
	Header  mkvHeader      `ebml:"EBML"`
	Segment unknownSegment `ebml:"Segment"`
}

var (
	idTrackTimecodeScale = []byte{0x23, 0x31, 0x4F}
	idFrameRate          = []byte{0x23, 0x83, 0xE3}
	idCues               = []byte{0x1C, 0x53, 0xBB, 0x6B}
	idUnassigned         = []byte{0x1C, 0x53, 0xBB, 0x6C}
)

// unknownElementsDoc returns the document, and the same one without the two
// elements, which is the one it has to read as.
func unknownElementsDoc(tb testing.TB) (with, without []byte) {
	tb.Helper()
	build := func() []byte {
		doc := unknownDoc{
			Header: mkvHeader{DocType: "webm"},
			Segment: unknownSegment{
				Info:  mkvInfo{TimecodeScale: defaultTimecodeScale, Duration: 80},
				Stray: unknownMaster{Void: []byte("junk")},
				Cluster: []mkvCluster{{Timecode: 0, SimpleBlock: []ebml.Block{
					simple(1, 0, true, []byte("k0")),
					simple(1, 40, false, []byte("p1")),
					simple(1, 80, false, []byte("p2")),
				}}},
			},
		}
		video := &unknownVideo{PixelWidth: 352, PixelHeight: 288, FrameRate: 25}
		doc.Segment.Tracks.TrackEntry = []unknownTrack{{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_VP8", Video: video}}
		var buf bytes.Buffer
		if err := ebml.Marshal(&doc, &buf); err != nil {
			tb.Fatalf("marshal: %v", err)
		}
		return buf.Bytes()
	}
	with = build()
	for _, c := range []struct{ from, to []byte }{
		{idTrackTimecodeScale, idFrameRate}, {idCues, idUnassigned},
	} {
		if n := bytes.Count(with, c.from); n != 1 {
			tb.Fatalf("fixture: %x appears %d times, want 1", c.from, n)
		}
		with = bytes.Replace(with, c.from, c.to, 1)
	}
	without = cutElement(tb, cutElement(tb, with, idFrameRate, 0), idUnassigned, 0)
	return with, without
}

// cutElement removes the element with the given ID, header and body, and fixes
// the size of the one master (the track's Video, the Segment) holding it, which
// are the only ancestors this fixture has.
func cutElement(tb testing.TB, data, id []byte, _ int) []byte {
	tb.Helper()
	at := bytes.Index(data, id)
	if at < 0 {
		tb.Fatalf("fixture: %x not found", id)
	}
	_, idWidth, _, _ := ebmlVint(data, at, true)
	size, sizeWidth, _, _ := ebmlVint(data, at+idWidth, false)
	n := idWidth + sizeWidth + int(size)
	out := append(append([]byte(nil), data[:at]...), data[at+n:]...)
	// Walk the masters that contain at, shrinking each size by n.
	for i := 0; i < at; {
		mid, mw, _, _ := ebmlVint(out, i, true)
		msize, msw, _, _ := ebmlVint(out, i+mw, false)
		body := i + mw + msw
		if ebmlMasters[mid] && body <= at && at < body+int(msize) {
			if !putEbmlSize(out[i+mw:], msize-uint64(n), msw) {
				tb.Fatalf("fixture: size %d does not fit %d bytes", msize-uint64(n), msw)
			}
			i = body
			continue
		}
		i = body + int(msize)
	}
	return out
}

func samplesOf(t *testing.T, data []byte) [][]byte {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var out [][]byte
	for _, id := range r.TrackIDs() {
		samples, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		for _, s := range samples {
			out = append(out, s.Data)
		}
	}
	return out
}

// Preconditions of the fixtures below: ebml-go refuses the document as it is,
// so a pass further down is the skip and nothing else.
func TestTheFixtureIsOneEbmlGoRefuses(t *testing.T) {
	with, without := unknownElementsDoc(t)
	var doc mkvReadDoc
	err := ebml.Unmarshal(bytes.NewReader(with), &doc)
	if !errors.Is(err, ebml.ErrUnknownElement) {
		t.Fatalf("ebml-go on the fixture = %v, want ErrUnknownElement", err)
	}
	if err := ebml.Unmarshal(bytes.NewReader(without), &doc); err != nil {
		t.Fatalf("ebml-go on the fixture without the elements: %v", err)
	}
}

// go-avkit/avkit#129: FrameRate, and any other element the reader does not
// know, are skipped (RFC 9559 §7), and the file reads as it does without them.
func TestAnUnknownElementIsSkipped(t *testing.T) {
	with, without := unknownElementsDoc(t)
	got, want := samplesOf(t, with), samplesOf(t, without)
	if len(want) != 3 {
		t.Fatalf("the reference reads %d samples, want 3", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("read %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("sample %d = %q, want %q", i, got[i], want[i])
		}
	}
	file, err := Demux(with)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if v := file.VideoTracks(); len(v) != 1 {
		t.Fatalf("Demux found %d video tracks, want 1", len(v))
	}
}

// The skip is by the size an element states, and no further: where an unknown
// element of unknown size ends cannot be said, and one that states more than
// its parent holds is malformed, whatever else the file holds after it.
func TestAnUnknownElementThatCannotBeSkippedIsStillRefused(t *testing.T) {
	with, _ := unknownElementsDoc(t)
	for _, c := range []struct {
		name  string
		id    []byte
		patch func(size []byte)
	}{
		{"unknown size", idUnassigned, func(s []byte) { s[0] = 0xFF }},
		{"larger than its parent", idFrameRate, func(s []byte) { s[0] = 0x80 | 30 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := append([]byte(nil), with...)
			at := bytes.Index(data, c.id) + len(c.id)
			if len(data)-at < 32 {
				t.Fatalf("fixture: %d bytes after the element, too few for a size that fits the file", len(data)-at)
			}
			c.patch(data[at:])
			if _, err := NewReader(data); err == nil {
				t.Error("NewReader accepted it")
			}
			if _, err := Demux(data); err == nil {
				t.Error("Demux accepted it")
			}
		})
	}
}

// With nothing to skip the bytes come back as they are, uncopied: the reader
// pays nothing for the files that never needed it.
func TestNothingUnknownCostsNoCopy(t *testing.T) {
	_, without := unknownElementsDoc(t)
	if out := skipUnknownElements(without); &out[0] != &without[0] {
		t.Error("a document with nothing unknown was copied")
	}
	with, _ := unknownElementsDoc(t)
	out := skipUnknownElements(with)
	if len(out) != len(with) {
		t.Fatalf("length %d, want %d", len(out), len(with))
	}
	if bytes.Count(with, idFrameRate) != 1 || bytes.Contains(out, idFrameRate) {
		t.Error("FrameRate was not turned into a Void")
	}
}

// A Void takes the place of an element of any header width and any body length
// and covers exactly the bytes it replaces.
func TestVoidElementCoversExactlyWhatItReplaces(t *testing.T) {
	for idWidth := 1; idWidth <= 4; idWidth++ {
		for _, body := range []int{0, 1, 2, 5, 6, 7, 8, 126, 127, 128, 300, 70000} {
			el := make([]byte, idWidth+1+body)
			el[0] = 0x40 >> (idWidth - 1) // an ID of idWidth bytes
			if idWidth == 1 {
				el[0] = 0x80
			}
			el[idWidth] = byte(0x80 | body&0x7F)
			if body > 126 {
				el = make([]byte, idWidth+2+body)
				el[0] = 0x40 >> (idWidth - 1)
				if idWidth == 1 {
					el[0] = 0x80
				}
				el[idWidth], el[idWidth+1] = 0x40|byte(body>>8), byte(body)
			}
			voidElement(el)
			id, iw, _, ok := ebmlVint(el, 0, true)
			if !ok || id != ebmlVoidID {
				t.Fatalf("id %d body %d: id = %#x", idWidth, body, id)
			}
			size, sw, unknown, ok := ebmlVint(el, iw, false)
			if !ok || unknown || iw+sw+int(size) != len(el) {
				t.Fatalf("id %d body %d: header %d+%d states %d, element is %d", idWidth, body, iw, sw, size, len(el))
			}
		}
	}
}

// The fixture is also a seed of every fuzz target, as a file: a change to what
// the reader skips is fuzzed from the case that motivated it.
func TestTheFuzzSeedIsTheFixture(t *testing.T) {
	const path = "testdata/fuzzseed/unknown-elements.webm"
	with, _ := unknownElementsDoc(t)
	if os.Getenv("AVKIT_REGEN") != "" {
		if err := os.WriteFile(path, with, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip(path + " regenerated; run the tests again")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, with) {
		t.Errorf("%s is not the fixture; regenerate it with AVKIT_REGEN=1 go test -run TestTheFuzzSeedIsTheFixture ./container", path)
	}
}

// The walk ends at a structure it cannot follow and leaves the rest to
// ebml-go, which says what is wrong with it. What it has voided before that
// point stays voided, and nothing after the point is touched.
func TestTheSkipEndsWhereTheWalkCannotFollow(t *testing.T) {
	unknownElem := []byte{0x7F, 0x7F, 0x82, 0xAA, 0xBB} // an ID nobody assigned, two bytes of body
	voided := []byte{ebmlVoidID, 0x10, 0x00, 0x00, 0x00}
	for _, c := range []struct {
		name string
		tail []byte
	}{
		{"an ID that is not a number", []byte{0x00, 0x01}},
		{"an ID without its size", []byte{0x7F, 0x7F}},
		{"a size that is not a number", []byte{0x7F, 0x7F, 0x00}},
		{"a known element of unknown size", append([]byte{0xA3, 0xFF}, unknownElem...)},
		{"a known element past the end", append([]byte{0xA3, 0x90}, unknownElem...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := append(append([]byte(nil), unknownElem...), c.tail...)
			want := append(append([]byte(nil), voided...), c.tail...)
			if got := skipUnknownElements(in); !bytes.Equal(got, want) {
				t.Errorf("skipUnknownElements(% x) = % x, want % x", in, got, want)
			}
			if !bytes.Equal(in[:len(unknownElem)], unknownElem) {
				t.Error("the input was written to")
			}
		})
	}
}

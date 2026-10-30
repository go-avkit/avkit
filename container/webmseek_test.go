// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/at-wat/ebml-go"
)

// segmentChildAt is one Top-Level Element of a written segment: its ID and where
// it begins, counted from the segment's first content byte as a SeekPosition is.
type segmentChildAt struct {
	id  uint64
	pos uint64
}

// segmentChildren walks the Top-Level Elements of the first segment of a file,
// stepping over an unknown-size cluster child by child until the next sibling
// starts. It is written against RFC 9559 rather than with the muxer's own
// helpers, so that what it finds is what a reader would.
func segmentChildren(t *testing.T, data []byte) (children []segmentChildAt, content int) {
	t.Helper()
	i := 0
	for {
		id, iw, _, ok := ebmlVint(data, i, true)
		if !ok {
			t.Fatalf("no segment found")
		}
		size, sw, _, _ := ebmlVint(data, i+iw, false)
		if id == ebmlIDSegment {
			content = i + iw + sw
			break
		}
		i += iw + sw + int(size)
	}
	for j := content; j < len(data); {
		id, iw, _, ok := ebmlVint(data, j, true)
		if !ok {
			t.Fatalf("unreadable element at %d", j)
		}
		size, sw, unknown, _ := ebmlVint(data, j+iw, false)
		children = append(children, segmentChildAt{id: id, pos: uint64(j - content)})
		if !unknown {
			j += iw + sw + int(size)
			continue
		}
		k := j + iw + sw
		for k < len(data) {
			cid, cw, _, _ := ebmlVint(data, k, true)
			if segmentChild(cid) {
				break
			}
			csz, csw, _, _ := ebmlVint(data, k+cw, false)
			k += cw + csw + int(csz)
		}
		j = k
	}
	return children, content
}

// ebmlIDVoid is the Void element.
const ebmlIDVoid = 0xEC

// seekHeadOf reads the SeekHead of a file back with ebml-go.
func seekHeadOf(t *testing.T, data []byte) map[uint64]uint64 {
	t.Helper()
	f := readWebM(t, data)
	if f.Segment.SeekHead == nil {
		return nil
	}
	out := map[uint64]uint64{}
	for _, s := range f.Segment.SeekHead.Seek {
		var id uint64
		for _, b := range s.SeekID {
			id = id<<8 | uint64(b)
		}
		out[id] = s.SeekPosition
	}
	return out
}

// checkSeekHead fails unless the SeekHead is the segment's first child, a Void
// follows it, and it names every Top-Level Element but itself, the Void and the
// clusters (§6.3), each at the position the element really has.
func checkSeekHead(t *testing.T, data []byte, wantCues bool) {
	t.Helper()
	children, _ := segmentChildren(t, data)
	if len(children) < 2 || children[0].id != 0x114D9B74 || children[1].id != ebmlIDVoid {
		t.Fatalf("the segment opens with % x, want a SeekHead and a Void", children[:min(2, len(children))])
	}
	seeks := seekHeadOf(t, data)
	for _, c := range children {
		switch c.id {
		case 0x114D9B74, ebmlIDVoid, 0x1F43B675:
			continue
		}
		pos, named := seeks[c.id]
		if !named {
			t.Errorf("the SeekHead does not name the element %x at %d", c.id, c.pos)
			continue
		}
		if pos != c.pos {
			t.Errorf("the SeekHead puts %x at %d, it is at %d", c.id, pos, c.pos)
		}
		delete(seeks, c.id)
	}
	if len(seeks) != 0 {
		t.Errorf("the SeekHead names elements the segment does not hold: %v", seeks)
	}
	cues := false
	for _, c := range children {
		cues = cues || c.id == 0x1C53BB6B
	}
	if cues != wantCues {
		t.Errorf("the file holds Cues = %v, want %v", cues, wantCues)
	}
	// The SeekHead and its Void fill the room reserved for them exactly:
	// Info follows.
	if want := uint64(webmSeekReserve(3)); children[2].pos != want {
		t.Errorf("the element after the Void is at %d, want %d", children[2].pos, want)
	}
}

// writeWebMTo writes the round-trip script's video and audio to w and returns
// the first failure.
func writeWebMTo(w io.Writer, videoFrames, audioFrames int, opts ...WebMOption) error {
	m := NewWebMMuxer(w, opts...)
	video, err := m.AddTrack(webmVideoConfig())
	if err != nil {
		return err
	}
	audio, err := m.AddTrack(webmAudioConfig())
	if err != nil {
		return err
	}
	v, a := 0, 0
	for v < videoFrames || a < audioFrames {
		videoTime := float64(v*webmVideoFrame) / webmVideoTimescale
		audioTime := float64(a*webmAudioFrame) / webmAudioTimescale
		if v < videoFrames && (a >= audioFrames || videoTime <= audioTime) {
			if err := m.WriteSample(video, Sample{
				Data: webmSampleData(video, v), Duration: webmVideoFrame, Sync: v%webmSyncEvery == 0,
			}); err != nil {
				return err
			}
			v++
			continue
		}
		if err := m.WriteSample(audio, Sample{
			Data: webmSampleData(audio, a), Duration: webmAudioFrame, Sync: true,
		}); err != nil {
			return err
		}
		a++
	}
	return m.Close()
}

func TestWebMSeekHeadNamesEveryTopLevelElement(t *testing.T) {
	short := ClusterDuration(100 * time.Millisecond)
	for _, frames := range []int{13, 0} {
		t.Run(fmt.Sprintf("%d frames", frames), func(t *testing.T) {
			var buffered bytes.Buffer
			if err := writeWebMTo(&buffered, frames, 2*frames, short, BufferedSegment()); err != nil {
				t.Fatal(err)
			}
			checkSeekHead(t, buffered.Bytes(), frames > 0)

			// A writer that can seek, and that already holds something: the
			// SeekHead is completed in place, wherever the file began.
			sink := newOffsetSink(100)
			if err := writeWebMTo(sink, frames, 2*frames, short); err != nil {
				t.Fatal(err)
			}
			checkSeekHead(t, sink.data, frames > 0)
			if want := int64(100 + len(sink.data)); sink.pos != want {
				t.Errorf("the writer is left at %d, want the end of the file at %d", sink.pos, want)
			}
			// And its size and duration are stated, as the buffered mode states
			// them: the two files say the same thing about the same media.
			checkSeekableSegment(t, sink.data, buffered.Bytes())
			// Both still read, and the cues still point at clusters.
			for name, data := range map[string][]byte{"buffered": buffered.Bytes(), "seekable": sink.data} {
				if _, err := NewReader(data); err != nil {
					t.Errorf("NewReader(%s): %v", name, err)
				}
			}
		})
	}
}

// checkSeekableSegment holds a segment streamed to a writer that can seek to
// what ffmpeg's matroskaenc writes at its trailer: the Segment's real size in
// the eight bytes the unknown size took, every Cluster's likewise, and the
// Info's Duration where a Void held its place -- the Duration the buffered mode
// states for the same media (RFC 9559 §5.1.2.10).
func checkSeekableSegment(t *testing.T, data, buffered []byte) {
	t.Helper()
	size := segmentSizeBytes(t, data)
	if bytes.Equal(size, unknownSize) {
		t.Fatalf("the segment states an unknown size")
	}
	_, content := segmentChildren(t, data)
	if got, want := binaryValue(size[1:]), uint64(len(data)-content); got != want {
		t.Errorf("the segment states %d bytes, it holds %d", got, want)
	}
	children, _ := segmentChildren(t, data)
	for _, c := range children {
		if c.id != 0x1F43B675 {
			continue
		}
		_, w, unknown, _ := ebmlVint(data, content+int(c.pos)+4, false)
		if unknown || w != 8 {
			t.Errorf("the cluster at %d keeps an unknown size", c.pos)
		}
	}
	f, want := readWebM(t, data), readWebM(t, buffered)
	if f.Segment.Info.Duration != want.Segment.Info.Duration {
		t.Errorf("duration = %v, the buffered segment states %v", f.Segment.Info.Duration, want.Segment.Info.Duration)
	}
	if len(f.Segment.Cluster) != len(want.Segment.Cluster) {
		t.Errorf("%d clusters read back, the buffered segment holds %d", len(f.Segment.Cluster), len(want.Segment.Cluster))
	}
}

// binaryValue is a big-endian unsigned number.
func binaryValue(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

func TestWebMStreamedToAWriterThatCannotSeekHasNoSeekHead(t *testing.T) {
	var plain bytes.Buffer
	if err := writeWebMTo(&plain, 13, 26); err != nil {
		t.Fatal(err)
	}
	if seeks := seekHeadOf(t, plain.Bytes()); seeks != nil {
		t.Errorf("a stream nothing can seek back into carries a SeekHead: %v", seeks)
	}
	// A writer that claims it can seek and cannot, as os.Stdout on a pipe
	// does, is written to as one that cannot.
	sink := newOffsetSink(0)
	sink.failSeekAt = 1
	if err := writeWebMTo(sink, 13, 26); err != nil {
		t.Fatalf("a writer whose Seek fails: %v", err)
	}
	if !bytes.Equal(sink.data, plain.Bytes()) {
		t.Errorf("a writer that cannot seek after all got %d bytes, not the stream's %d", len(sink.data), plain.Len())
	}
}

func TestWebMSeekHeadRoomFitsEveryCount(t *testing.T) {
	// Every count of entries a SeekHead may be written with, in a room
	// reserved for any count at least as large, fills that room exactly.
	for room := 1; room <= 12; room++ {
		reserve := webmSeekReserve(room)
		for n := 0; n <= room; n++ {
			seeks := make([]webmSeek, n)
			for i := range seeks {
				seeks[i] = cuesSeek(uint64(i) << 40)
			}
			head, void := webmSeekRegion(seeks, reserve)
			var region bytes.Buffer
			if err := ebml.Marshal(&webmRegionDoc{SeekHead: head, Void: void}, &region); err != nil {
				t.Fatal(err)
			}
			if region.Len() != reserve {
				t.Errorf("%d entries in room for %d: %d bytes, want %d", n, room, region.Len(), reserve)
			}
		}
	}
	if webmVoid(0) != nil {
		t.Error("no room should write no Void")
	}
}

// TestWebMSeekHeadNamesWhatTheHeadHolds checks the entries come from what was
// encoded rather than from a list of names: an element added to the head —
// Attachments here, which the metadata work writes — is named without anything
// in the SeekHead code knowing it.
func TestWebMSeekHeadNamesWhatTheHeadHolds(t *testing.T) {
	type attached struct {
		FileName string `ebml:"FileName"`
	}
	doc := struct {
		Info        webmInfo   `ebml:"Info"`
		Tracks      webmTracks `ebml:"Tracks"`
		Attachments struct {
			AttachedFile []attached `ebml:"AttachedFile"`
		} `ebml:"Attachments"`
	}{Info: webmInfo{TimecodeScale: 1}}
	doc.Attachments.AttachedFile = []attached{{FileName: "cover.jpg"}}
	head, seeks, err := measureHead(&doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0x1549A966, 0x1654AE6B, 0x1941A469}
	if len(seeks) != len(want) {
		t.Fatalf("entries = %d, want %d", len(seeks), len(want))
	}
	for i, s := range seeks {
		id, _, _, _ := ebmlVint(s.SeekID, 0, true)
		at, _, _, _ := ebmlVint(head.Bytes(), int(s.SeekPosition), true)
		if id != want[i] || at != id {
			t.Errorf("entry %d names %x at %d, where %x begins; want %x", i, id, s.SeekPosition, at, want[i])
		}
	}
}

func TestWebMSeekHeadReportsAFailureToComplete(t *testing.T) {
	// The rewrites at Close are the last seeks and writes the muxer makes: a
	// seek and a write each for the SeekHead, the Info, every cluster's size
	// and the segment's size, then the seek back to the end. Each is made to
	// fail in turn.
	probe := newOffsetSink(0)
	if err := writeWebMTo(probe, 7, 0); err != nil {
		t.Fatal(err)
	}
	children, _ := segmentChildren(t, probe.data)
	clusters := 0
	for _, c := range children {
		if c.id == 0x1F43B675 {
			clusters++
		}
	}
	steps := []string{"the seek head", "the segment info"}
	for range clusters {
		steps = append(steps, "a cluster size")
	}
	steps = append(steps, "the segment size")
	type failure struct {
		name string
		set  func(*offsetSink)
		says string
	}
	cases := []failure{
		{"seek past", func(s *offsetSink) { s.failSeekAt = probe.seeks }, "seek past the cues"},
		{"cues", func(s *offsetSink) { s.failWriteAt = probe.writes - len(steps) }, "write cues"},
	}
	for j, what := range steps {
		seekAt, writeAt := probe.seeks-(len(steps)-j), probe.writes-(len(steps)-1-j)
		cases = append(cases,
			failure{"seek back to " + what, func(s *offsetSink) { s.failSeekAt = seekAt }, "seek back to " + what},
			failure{"write " + what, func(s *offsetSink) { s.failWriteAt = writeAt }, "write " + what})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newOffsetSink(0)
			tc.set(sink)
			err := writeWebMTo(sink, 7, 0)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want a failure to %s", err, tc.says)
			}
		})
	}

	// And the encoding of the new SeekHead and the new Info, which cannot
	// fail on their own.
	original := measureEBML
	defer func() { measureEBML = original }()
	refused := errors.New("staged refusal")
	for _, doc := range []any{&webmRegionDoc{}, &webmInfoDoc{}} {
		measureEBML = func(v any, w io.Writer, opts ...ebml.MarshalOption) error {
			if fmt.Sprintf("%T", v) == fmt.Sprintf("%T", doc) {
				return refused
			}
			return original(v, w, opts...)
		}
		if err := writeWebMTo(newOffsetSink(0), 7, 0); !errors.Is(err, refused) {
			t.Errorf("%T: err = %v, want the staged refusal", doc, err)
		}
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// topBox is one top-level box of a written file, where it starts and what it
// decoded as.
type topBox struct {
	at  uint64
	box mp4.Box
}

// topBoxes decodes the top-level boxes of a file with mp4ff, so that what is
// asserted is what the reference library sees rather than what the muxer meant
// to write.
func topBoxes(t *testing.T, data []byte) []topBox {
	t.Helper()
	var out []topBox
	for at := uint64(0); at < uint64(len(data)); {
		box, err := mp4.DecodeBox(at, bytes.NewReader(data[at:]))
		if err != nil {
			t.Fatalf("decode the box at %d: %v", at, err)
		}
		out = append(out, topBox{at: at, box: box})
		at += box.Size()
	}
	return out
}

// sidxBoxes picks the segment index boxes out of a file's top-level boxes, with
// the byte each one ends at, which is the anchor its first_offset counts from.
func sidxBoxes(boxes []topBox) (out []*mp4.SidxBox, ends []uint64) {
	for _, b := range boxes {
		if s, ok := b.box.(*mp4.SidxBox); ok {
			out = append(out, s)
			ends = append(ends, b.at+s.Size())
		}
	}
	return out, ends
}

// fragmentStarts lists where every moof begins, and the size of it together
// with the mdat after it, which is what one reference has to state.
func fragmentRanges(t *testing.T, boxes []topBox) (starts, sizes []uint64) {
	t.Helper()
	for i, b := range boxes {
		if b.box.Type() != "moof" {
			continue
		}
		if i+1 >= len(boxes) || boxes[i+1].box.Type() != "mdat" {
			t.Fatalf("the moof at %d is not followed by an mdat", b.at)
		}
		starts = append(starts, b.at)
		sizes = append(sizes, b.box.Size()+boxes[i+1].box.Size())
	}
	return starts, sizes
}

// writeIndexedScript writes a video track at 25 fps with a sync sample every
// second and an audio track of 1024-sample frames at 48 kHz, interleaved by
// time, in fragments of one second, and returns what the muxer wrote.
func writeIndexedScript(t *testing.T, w io.Writer, opts ...MuxOption) {
	t.Helper()
	opts = append([]MuxOption{SegmentIndex(), FragmentDuration(time.Second)}, opts...)
	m := NewMuxer(w, opts...)
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	const frames, frame = 70, 512 // 2.8 s at 12800
	a := 0
	for v := 0; v < frames; v++ {
		if err := m.WriteSample(video, Sample{
			Data: []byte{0, 0, 0, 2, 9, byte(v)}, Duration: frame, Sync: v%25 == 0,
		}); err != nil {
			t.Fatal(err)
		}
		// Audio up to where the video has reached.
		for ; uint64(a)*1024*12800 < uint64(v+1)*frame*48000; a++ {
			if err := m.WriteSample(audio, Sample{
				Data: []byte{0xA0, byte(a)}, Duration: 1024, Sync: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentIndexReferencesEveryFragment(t *testing.T) {
	var buf bytes.Buffer
	writeIndexedScript(t, &buf)
	data := buf.Bytes()
	boxes := topBoxes(t, data)

	// ftyp, moov, then the index, before any fragment: ISO/IEC 23009-1
	// §6.3.4.4 wants the first sidx ahead of the first moof.
	var order []string
	for _, b := range boxes {
		if n := len(order); n == 0 || order[n-1] != b.box.Type() {
			order = append(order, b.box.Type())
		}
	}
	if got := strings.Join(order[:min(4, len(order))], " "); got != "ftyp moov sidx moof" {
		t.Fatalf("the file starts %q, want the index between the init segment and the first fragment", got)
	}

	starts, sizes := fragmentRanges(t, boxes)
	if len(starts) != 3 {
		t.Fatalf("fragments = %d, want 3 for 2.8 s cut every second", len(starts))
	}
	index, ends := sidxBoxes(boxes)
	if len(index) != 2 {
		t.Fatalf("sidx boxes = %d, want one per track", len(index))
	}
	want := []struct {
		id, timescale uint32
		total         uint64
	}{
		{1, 12800, 70 * 512},
		// The audio runs to the end of the frame that crosses 2.8 s.
		{2, 48000, 132 * 1024},
	}
	var lastByte uint64
	for k, sidx := range index {
		if sidx.ReferenceID != want[k].id || sidx.Timescale != want[k].timescale {
			t.Errorf("sidx %d indexes track %d at %d, want %d at %d", k,
				sidx.ReferenceID, sidx.Timescale, want[k].id, want[k].timescale)
		}
		if sidx.EarliestPresentationTime != 0 {
			t.Errorf("sidx %d starts at %d, want 0", k, sidx.EarliestPresentationTime)
		}
		if len(sidx.SidxRefs) != len(starts) {
			t.Fatalf("sidx %d has %d references for %d fragments", k, len(sidx.SidxRefs), len(starts))
		}
		// The references tile the fragments: the first starts where
		// first_offset says, each is exactly one moof and its mdat, and
		// the last ends where the file does.
		at := ends[k] + sidx.FirstOffset
		var total uint64
		for i, ref := range sidx.SidxRefs {
			if at != starts[i] {
				t.Errorf("sidx %d reference %d points at %d, the fragment starts at %d", k, i, at, starts[i])
			}
			if uint64(ref.ReferencedSize) != sizes[i] {
				t.Errorf("sidx %d reference %d states %d bytes, the fragment is %d", k, i, ref.ReferencedSize, sizes[i])
			}
			if ref.ReferenceType != 0 || ref.StartsWithSAP != 1 || ref.SAPType != 1 || ref.SAPDeltaTime != 0 {
				t.Errorf("sidx %d reference %d = %+v, want media starting with a type 1 SAP", k, i, ref)
			}
			at += uint64(ref.ReferencedSize)
			total += uint64(ref.SubSegmentDuration)
		}
		lastByte = at
		if total != want[k].total {
			t.Errorf("sidx %d durations add up to %d, want the track's %d", k, total, want[k].total)
		}
	}
	if lastByte != uint64(len(data)) {
		t.Errorf("the references end at %d of %d bytes", lastByte, len(data))
	}
	// One second of video per fragment, the last one the 0.8 s left.
	for i, d := range []uint32{12800, 12800, 10240} {
		if got := index[0].SidxRefs[i].SubSegmentDuration; got != d {
			t.Errorf("video reference %d lasts %d, want %d", i, got, d)
		}
	}
}

// readAllSamples reads every track of a file back with this package's reader.
func readAllSamples(t *testing.T, data []byte) map[uint32][]Sample {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	out := map[uint32][]Sample{}
	for _, id := range r.TrackIDs() {
		s, err := r.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		out[id] = s
	}
	return out
}

// withoutIndex is the file with its sidx boxes taken out. A fragment states
// where its samples are from its own moof, so the rest still reads.
func withoutIndex(t *testing.T, data []byte) []byte {
	t.Helper()
	var plain bytes.Buffer
	for _, b := range topBoxes(t, data) {
		if b.box.Type() != "sidx" {
			plain.Write(data[b.at : b.at+b.box.Size()])
		}
	}
	return plain.Bytes()
}

// sameSamples fails unless two readings of a file agree sample for sample.
func sameSamples(t *testing.T, with, without map[uint32][]Sample) {
	t.Helper()
	if len(with) != len(without) {
		t.Fatalf("%d tracks with the index, %d without", len(with), len(without))
	}
	for id, samples := range without {
		if len(with[id]) != len(samples) {
			t.Fatalf("track %d: %d samples with the index, %d without", id, len(with[id]), len(samples))
		}
		for i, s := range samples {
			got := with[id][i]
			if !bytes.Equal(got.Data, s.Data) || got.Duration != s.Duration ||
				got.Sync != s.Sync || got.CompositionOffset != s.CompositionOffset {
				t.Fatalf("track %d sample %d reads %+v with the index, %+v without", id, i, got, s)
			}
		}
	}
}

// TestTheReaderReadsAnIndexedFileAsItReadsAPlainOne checks the index changes
// nothing about the samples: what this muxer writes, and what ffmpeg writes
// with -movflags +dash+global_sidx, read back the same with and without their
// sidx boxes.
func TestTheReaderReadsAnIndexedFileAsItReadsAPlainOne(t *testing.T) {
	var indexed bytes.Buffer
	writeIndexedScript(t, &indexed)
	ours := readAllSamples(t, indexed.Bytes())
	if len(ours[1]) != 70 || len(ours[2]) != 132 {
		t.Fatalf("read back %d video and %d audio samples, want 70 and 132", len(ours[1]), len(ours[2]))
	}
	sameSamples(t, ours, readAllSamples(t, withoutIndex(t, indexed.Bytes())))

	// Made by ffmpeg 9.0.2:
	//   ffmpeg -f lavfi -i testsrc=size=64x48:rate=10 \
	//     -f lavfi -i sine=frequency=440:sample_rate=8000 -t 3 \
	//     -c:v libx264 -g 10 -bf 2 -c:a aac -b:a 16k \
	//     -movflags +dash+global_sidx -fflags +bitexact -map_metadata -1 \
	//     ffmpeg-global-sidx.mp4
	// Two sidx boxes, one per track, each referencing the same three
	// fragments; an mfra at the end.
	fixture, err := os.ReadFile("testdata/ffmpeg-global-sidx.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if index, _ := sidxBoxes(topBoxes(t, fixture)); len(index) != 2 {
		t.Fatalf("the fixture holds %d sidx boxes, want 2", len(index))
	}
	theirs := readAllSamples(t, fixture)
	// ffprobe -count_frames reads 30 video frames.
	if len(theirs[1]) != 30 {
		t.Errorf("read %d video samples out of ffmpeg's file, ffprobe reads 30", len(theirs[1]))
	}
	sameSamples(t, theirs, readAllSamples(t, withoutIndex(t, fixture)))
}

func TestSegmentIndexIsOnlyWrittenWhenAskedFor(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if n := countBoxes(t, buf.Bytes(), "sidx"); n != 0 {
		t.Errorf("a muxer not asked for an index wrote %d sidx boxes", n)
	}
}

// indexSink is a file in memory: it can be written, seeked and read back, which
// is what lets the muxer hand fragments over as they come and make room for the
// index at Close. It starts at a non-zero origin, as a file a caller has
// already written into does.
type indexSink struct {
	*offsetSink
	reads, failReadAt int
}

func newIndexSink(origin int64) *indexSink {
	return &indexSink{offsetSink: newOffsetSink(origin)}
}

func (s *indexSink) Read(p []byte) (int, error) {
	s.reads++
	if s.reads == s.failReadAt {
		return 0, errors.New("unreadable sector")
	}
	at := s.pos - s.origin
	if at >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[at:])
	s.pos += int64(n)
	return n, nil
}

func TestSegmentIndexIsTheSameWhetherFragmentsAreHeldOrMoved(t *testing.T) {
	var held bytes.Buffer
	writeIndexedScript(t, &held)

	// A block far smaller than the file, and not a divisor of it, so the
	// fragments are moved in many pieces and the last one is short.
	original := shiftBlock
	defer func() { shiftBlock = original }()
	shiftBlock = 333

	sink := newIndexSink(1000)
	writeIndexedScript(t, sink)
	if !bytes.Equal(sink.data, held.Bytes()) {
		t.Fatalf("the moved file (%d bytes) differs from the held one (%d bytes)", len(sink.data), held.Len())
	}
	if want := int64(1000 + len(sink.data)); sink.pos != want {
		t.Errorf("the writer is left at %d, want the end of the file at %d", sink.pos, want)
	}

	// And through a real file, which is what the moving is for.
	f, err := os.CreateTemp(t.TempDir(), "indexed-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeIndexedScript(t, f)
	onDisk, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, held.Bytes()) {
		t.Fatalf("the file on disk (%d bytes) differs from the held one (%d bytes)", len(onDisk), held.Len())
	}
}

// indexedTrack is one track of a hand-written script: its configuration, and
// the samples of each fragment, nil where the track sits a fragment out.
type indexedTrack struct {
	cfg       TrackConfig
	fragments [][]Sample
}

func TestSegmentIndexDescribesWhatEachTrackHolds(t *testing.T) {
	audio := audioConfig()
	tracks := []indexedTrack{
		// Video with frames reordered, as B-frames are. The first fragment
		// opens on a sync sample nothing presents before (type 1); the
		// second on one a later-decoded frame presents before, which only
		// the bitstream could classify (0).
		{videoConfig(t), [][]Sample{
			{
				{Data: []byte{1}, Duration: 512, CompositionOffset: 1024, Sync: true}, // pts 1024
				{Data: []byte{2}, Duration: 512, CompositionOffset: 2048},             // pts 2560
				{Data: []byte{3}, Duration: 512, CompositionOffset: 512},              // pts 1536
				{Data: []byte{4}, Duration: 512, CompositionOffset: 512},              // pts 2048
			},
			{
				{Data: []byte{5}, Duration: 512, CompositionOffset: 1024, Sync: true}, // pts 3072
				{Data: []byte{6}, Duration: 512},                                      // pts 2560
			},
		}},
		// Opens on a frame that is not a sync sample, then sits the second
		// fragment out.
		{audio, [][]Sample{
			{{Data: []byte{7}, Duration: 1024}, {Data: []byte{8}, Duration: 1024, Sync: true}},
			nil,
		}},
		// Presents its first fragment later than its second, whose time is
		// before zero and counted from zero.
		{audio, [][]Sample{
			{{Data: []byte{9}, Duration: 1024, CompositionOffset: 5000, Sync: true}},   // pts 5000
			{{Data: []byte{10}, Duration: 1024, CompositionOffset: -2048, Sync: true}}, // pts -1024
		}},
		// Declared and never written: there is nothing to index.
		{audio, [][]Sample{nil, nil}},
	}
	var buf bytes.Buffer
	m := NewMuxer(&buf, SegmentIndex())
	ids := make([]uint32, len(tracks))
	for i, tr := range tracks {
		id, err := m.AddTrack(tr.cfg)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	for f := 0; f < 2; f++ {
		for i, tr := range tracks {
			for _, s := range tr.fragments[f] {
				if err := m.WriteSample(ids[i], s); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := m.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	index, _ := sidxBoxes(topBoxes(t, buf.Bytes()))
	type ref struct {
		duration          uint32
		startsWithSAP, ty uint8
	}
	want := []struct {
		id       uint32
		earliest uint64
		refs     []ref
	}{
		// 1024 to the second fragment's 2560, then to 3584, where the
		// frame at 3072 ends.
		{1, 1024, []ref{{1536, 1, 1}, {1024, 1, 0}}},
		{2, 0, []ref{{2048, 0, 0}, {0, 0, 0}}},
		// The second fragment covers everything from zero to 6024, where
		// the first one's frame ends; the first adds nothing to that.
		{3, 5000, []ref{{0, 1, 1}, {6024, 1, 1}}},
	}
	if len(index) != len(want) {
		t.Fatalf("sidx boxes = %d, want %d: none for a track without samples", len(index), len(want))
	}
	for k, w := range want {
		got := index[k]
		if got.ReferenceID != w.id || got.EarliestPresentationTime != w.earliest {
			t.Errorf("sidx %d indexes track %d from %d, want track %d from %d",
				k, got.ReferenceID, got.EarliestPresentationTime, w.id, w.earliest)
		}
		for i, r := range w.refs {
			g := got.SidxRefs[i]
			if g.SubSegmentDuration != r.duration || g.StartsWithSAP != r.startsWithSAP || g.SAPType != r.ty {
				t.Errorf("track %d reference %d = duration %d, SAP %d type %d; want %d, %d, %d",
					w.id, i, g.SubSegmentDuration, g.StartsWithSAP, g.SAPType,
					r.duration, r.startsWithSAP, r.ty)
			}
		}
	}
}

func TestSegmentIndexRefusesWhatItCannotState(t *testing.T) {
	t.Run("more media held than allowed", func(t *testing.T) {
		m := NewMuxer(io.Discard, SegmentIndex(), IndexMemoryLimit(64))
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, Sample{Data: make([]byte, 100), Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); !errors.Is(err, ErrMediaTooLarge) {
			t.Errorf("Close = %v, want ErrMediaTooLarge", err)
		}
	})
	t.Run("more fragments than one index references", func(t *testing.T) {
		original := maxIndexReferences
		defer func() { maxIndexReferences = original }()
		maxIndexReferences = 2
		m := NewMuxer(io.Discard, SegmentIndex())
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		var failed error
		for i := 0; i < 3 && failed == nil; i++ {
			if failed = m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); failed == nil {
				failed = m.Flush()
			}
		}
		if !errors.Is(failed, ErrMediaTooLarge) {
			t.Errorf("the third fragment = %v, want ErrMediaTooLarge", failed)
		}
	})
	t.Run("a fragment larger than a reference states", func(t *testing.T) {
		original := maxReferencedSize
		defer func() { maxReferencedSize = original }()
		maxReferencedSize = 100
		m := NewMuxer(io.Discard, SegmentIndex())
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, Sample{Data: make([]byte, 200), Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); !errors.Is(err, ErrMediaTooLarge) {
			t.Errorf("Close = %v, want ErrMediaTooLarge", err)
		}
	})
	t.Run("a fragment longer than a reference states", func(t *testing.T) {
		cfg := videoConfig(t)
		cfg.Timescale = 1_000_000_000
		m := NewMuxer(io.Discard, SegmentIndex())
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Two three-second frames in one fragment: six thousand million
		// ticks, past the 32 bits subsegment_duration has.
		for i := 0; i < 2; i++ {
			if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 3_000_000_000, Sync: i == 0}); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); !errors.Is(err, ErrMediaTooLarge) {
			t.Errorf("Close = %v, want ErrMediaTooLarge", err)
		}
	})
}

func TestSegmentIndexOfNothingIsNotWritten(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, SegmentIndex())
	if _, err := m.AddTrack(videoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if n := countBoxes(t, buf.Bytes(), "sidx"); n != 0 {
		t.Errorf("a file with no fragment carries %d sidx boxes", n)
	}
	if got := NewMuxer(io.Discard, IndexMemoryLimit(0)).settings.indexMemoryLimit; got != DefaultMediaMemoryLimit {
		t.Errorf("IndexMemoryLimit(0) = %d, want the default", got)
	}
}

func TestSegmentIndexReportsEveryFailedWrite(t *testing.T) {
	// A writer that cannot be read back: the index, then the fragments held
	// for it, each refused in turn.
	var good bytes.Buffer
	writeIndexedScript(t, &good)
	boxes := topBoxes(t, good.Bytes())
	index, ends := sidxBoxes(boxes)
	if len(index) != 2 {
		t.Fatalf("sidx boxes = %d, want 2", len(index))
	}
	afterIndex := int(ends[len(ends)-1])
	beforeIndex := afterIndex - int(index[0].Size()) - int(index[1].Size())
	for _, tc := range []struct {
		allow int
		says  string
	}{
		{beforeIndex, "write segment index"},
		{afterIndex, "write fragments"},
	} {
		m := NewMuxer(&failWriter{allow: tc.allow}, SegmentIndex())
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		audio, err := m.AddTrack(audioConfig())
		if err != nil {
			t.Fatal(err)
		}
		_ = m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true})
		_ = m.WriteSample(audio, Sample{Data: []byte{1}, Duration: 1024, Sync: true})
		err = m.Close()
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("with room for %d bytes: %v, want a failure to %s", tc.allow, err, tc.says)
		}
	}

	// A file: every seek, read and write the muxer makes is made to fail in
	// one run, and each run must say so.
	original := shiftBlock
	defer func() { shiftBlock = original }()
	shiftBlock = 4096
	probe := newIndexSink(0)
	if err := runIndexedScript(t, probe); err != nil {
		t.Fatalf("the script itself fails: %v", err)
	}
	for _, op := range []string{"seek", "read", "write"} {
		calls := map[string]int{"seek": probe.seeks, "read": probe.reads, "write": probe.writes}[op]
		if calls == 0 {
			t.Fatalf("the script makes no %s", op)
		}
		for n := 1; n <= calls; n++ {
			sink := newIndexSink(0)
			switch op {
			case "seek":
				sink.failSeekAt = n
			case "read":
				sink.failReadAt = n
			default:
				sink.failWriteAt = n
			}
			if err := runIndexedScript(t, sink); err == nil {
				t.Errorf("%s %d of %d failed and nothing was reported", op, n, calls)
			}
		}
	}
	// A file that ends before the fragments it was handed is a read that
	// comes up short, which is a failure like any other.
	short := &truncatingSink{indexSink: newIndexSink(0)}
	if err := runIndexedScript(t, short); !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Errorf("a file that lost its fragments: %v", err)
	}
}

// truncatingSink loses everything written after the first kilobyte, as a
// device that silently drops writes would.
type truncatingSink struct{ *indexSink }

func (s *truncatingSink) Write(p []byte) (int, error) {
	n, err := s.indexSink.Write(p)
	if len(s.data) > 1024 {
		s.data = s.data[:1024]
	}
	return n, err
}

// runIndexedScript is writeIndexedScript returning the first failure instead of
// failing the test with it.
func runIndexedScript(t *testing.T, w io.Writer) error {
	t.Helper()
	m := NewMuxer(w, SegmentIndex(), FragmentDuration(time.Second))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 70; v++ {
		if err := m.WriteSample(video, Sample{
			Data: []byte{0, 0, 0, 2, 9, byte(v)}, Duration: 512, Sync: v%25 == 0,
		}); err != nil {
			return err
		}
	}
	return m.Close()
}

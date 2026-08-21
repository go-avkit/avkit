// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// topLevelBoxes names the boxes a file holds at its top level, in order. It
// reads the 64-bit size a box announces with a size of one, which is the form
// the mdat of a streamed file takes.
func topLevelBoxes(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	for offset := 0; offset+8 <= len(data); {
		size := uint64(binary.BigEndian.Uint32(data[offset:]))
		name := string(data[offset+4 : offset+8])
		header := 8
		if size == 1 {
			if offset+16 > len(data) {
				t.Fatalf("box %q at %d announces a 64-bit size it does not carry", name, offset)
			}
			size = binary.BigEndian.Uint64(data[offset+8:])
			header = 16
		}
		if size < uint64(header) || offset+int(size) > len(data) {
			t.Fatalf("box %q at %d states size %d of %d bytes", name, offset, size, len(data))
		}
		names = append(names, name)
		offset += int(size)
	}
	return names
}

// progressiveMoov insists the bytes are a progressive MP4 and hands back its
// movie box. Being progressive is not assumed from the writer that produced the
// file: it is read back out of the box tree, which must hold a moov whose
// sample tables are populated and not one fragment anywhere.
func progressiveMoov(t *testing.T, data []byte) *mp4.MoovBox {
	t.Helper()
	boxes := topLevelBoxes(t, data)
	for _, name := range boxes {
		switch name {
		case "moof", "styp", "sidx":
			t.Fatalf("a progressive file carries no %s: %v", name, boxes)
		}
	}
	if len(boxes) != 3 || boxes[0] != "ftyp" || boxes[1] != "mdat" || boxes[2] != "moov" {
		t.Fatalf("box order = %v, want ftyp, mdat, moov", boxes)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.IsFragmented() {
		t.Fatal("the file reads back as fragmented")
	}
	if parsed.Moov == nil {
		t.Fatal("no moov")
	}
	if parsed.Moov.Mvex != nil {
		t.Fatal("moov carries an mvex, which announces fragments")
	}
	if len(parsed.Segments) != 0 {
		t.Fatalf("segments = %d", len(parsed.Segments))
	}
	for _, trak := range parsed.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		switch {
		case stbl == nil:
			t.Fatalf("track %d has no sample table", trak.Tkhd.TrackID)
		case stbl.Stsd == nil:
			t.Fatalf("track %d has no sample description", trak.Tkhd.TrackID)
		case stbl.Stts == nil || len(stbl.Stts.SampleCount) == 0:
			t.Fatalf("track %d has an empty stts", trak.Tkhd.TrackID)
		case stbl.Stsc == nil || len(stbl.Stsc.Entries) == 0:
			t.Fatalf("track %d has an empty stsc", trak.Tkhd.TrackID)
		case stbl.Stsz == nil || stbl.Stsz.GetNrSamples() == 0:
			t.Fatalf("track %d has an empty stsz", trak.Tkhd.TrackID)
		case stbl.Stco == nil && stbl.Co64 == nil:
			t.Fatalf("track %d has no chunk offset table", trak.Tkhd.TrackID)
		}
	}
	return parsed.Moov
}

// stblOf is one track's sample table, by track identifier.
func stblOf(t *testing.T, moov *mp4.MoovBox, trackID uint32) *mp4.StblBox {
	t.Helper()
	for _, trak := range moov.Traks {
		if trak.Tkhd.TrackID == trackID {
			return trak.Mdia.Minf.Stbl
		}
	}
	t.Fatalf("no track %d in %d tracks", trackID, len(moov.Traks))
	return nil
}

func TestProgressiveRoundTripsThroughReader(t *testing.T) {
	var buf bytes.Buffer
	// A chunk of 80 ms holds two video frames of 40 ms, so the two tracks
	// alternate through the file instead of following one another.
	m := NewProgressiveMuxer(&buf, ChunkDuration(80*time.Millisecond))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatalf("AddTrack(audio): %v", err)
	}
	const frames = 8
	for i := 0; i < frames; i++ {
		if err := m.WriteSample(video, Sample{
			Data: []byte{0, 0, 0, 2, 9, byte(i)}, Duration: 512, Sync: i%4 == 0,
		}); err != nil {
			t.Fatalf("WriteSample(video, %d): %v", i, err)
		}
		if err := m.WriteSample(audio, Sample{
			Data: []byte{0xde, byte(i)}, Duration: 1024, Sync: true,
		}); err != nil {
			t.Fatalf("WriteSample(audio, %d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	moov := progressiveMoov(t, buf.Bytes())
	if moov.Mvhd.Timescale != progressiveMovieTimescale {
		t.Errorf("movie timescale = %d", moov.Mvhd.Timescale)
	}
	// Eight video frames of 40 ms is 320 ms; the audio is shorter, so the
	// movie lasts as long as the longest track.
	if moov.Mvhd.Duration != 320 {
		t.Errorf("movie duration = %d, want 320", moov.Mvhd.Duration)
	}
	if moov.Mvhd.NextTrackID != 3 {
		t.Errorf("next track id = %d", moov.Mvhd.NextTrackID)
	}

	vstbl := stblOf(t, moov, video)
	// Every video sample lasts the same, so run-length compaction must leave
	// a single stts entry however many samples there are.
	if got := len(vstbl.Stts.SampleCount); got != 1 {
		t.Errorf("video stts entries = %d, want 1: %+v", got, vstbl.Stts)
	}
	if vstbl.Stts.SampleCount[0] != frames || vstbl.Stts.SampleTimeDelta[0] != 512 {
		t.Errorf("video stts entry = %d x %d", vstbl.Stts.SampleCount[0], vstbl.Stts.SampleTimeDelta[0])
	}
	if got := vstbl.Stsz.GetNrSamples(); got != frames {
		t.Errorf("video sample count = %d, want %d", got, frames)
	}
	// Nothing presents away from its decode time, so there is no ctts at all.
	if vstbl.Ctts != nil {
		t.Errorf("video carries a ctts: %+v", vstbl.Ctts)
	}
	// Two of the eight video frames are sync samples, so the table has to say
	// which; every audio sample is one, so its table is left out.
	if vstbl.Stss == nil || len(vstbl.Stss.SampleNumber) != 2 {
		t.Fatalf("video stss = %+v", vstbl.Stss)
	}
	if vstbl.Stss.SampleNumber[0] != 1 || vstbl.Stss.SampleNumber[1] != 5 {
		t.Errorf("video sync samples = %v", vstbl.Stss.SampleNumber)
	}
	astbl := stblOf(t, moov, audio)
	if astbl.Stss != nil {
		t.Errorf("audio carries an stss although every sample is a sync sample")
	}

	// The mdat payload starts after a 28-byte ftyp and an 8-byte mdat header,
	// and the first chunk is the first thing in it.
	if vstbl.Stco == nil {
		t.Fatal("a file this small must be addressed by stco")
	}
	if got := vstbl.Stco.ChunkOffset[0]; got != 36 {
		t.Errorf("first video chunk at %d, want 36", got)
	}
	// Two video frames of six bytes, then the audio the muxer held while they
	// were written.
	if got := astbl.Stco.ChunkOffset[0]; got != 48 {
		t.Errorf("first audio chunk at %d, want 48", got)
	}

	// What the muxer wrote, this package's own reader must hand back.
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	vsamples, err := r.Samples(video)
	if err != nil {
		t.Fatalf("Samples(video): %v", err)
	}
	if len(vsamples) != frames {
		t.Fatalf("read back %d video samples, want %d", len(vsamples), frames)
	}
	for i, s := range vsamples {
		want := Sample{Data: []byte{0, 0, 0, 2, 9, byte(i)}, Duration: 512, Sync: i%4 == 0}
		if !bytes.Equal(s.Data, want.Data) || s.Duration != want.Duration || s.Sync != want.Sync {
			t.Errorf("video sample %d = %+v, want %+v", i, s, want)
		}
	}
	asamples, err := r.Samples(audio)
	if err != nil {
		t.Fatalf("Samples(audio): %v", err)
	}
	if len(asamples) != frames {
		t.Fatalf("read back %d audio samples, want %d", len(asamples), frames)
	}
	for i, s := range asamples {
		if !bytes.Equal(s.Data, []byte{0xde, byte(i)}) || s.Duration != 1024 || !s.Sync {
			t.Errorf("audio sample %d = %+v", i, s)
		}
	}
}

func TestProgressiveCompactsTablesByRun(t *testing.T) {
	var buf bytes.Buffer
	// One chunk for the whole track, so the chunk table is not what varies
	// here.
	m := NewProgressiveMuxer(&buf, ChunkDuration(time.Hour))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	// Three durations in two runs of three and one of one, and composition
	// offsets in three runs — one of them negative, which only the 64-bit
	// form of the box can state.
	samples := []Sample{
		{Duration: 512, CompositionOffset: 0, Sync: true},
		{Duration: 512, CompositionOffset: 0},
		{Duration: 512, CompositionOffset: 1024},
		{Duration: 600, CompositionOffset: 1024},
		{Duration: 600, CompositionOffset: 1024},
		{Duration: 600, CompositionOffset: -512},
		{Duration: 512, CompositionOffset: -512},
	}
	for i := range samples {
		samples[i].Data = []byte{byte(i), byte(i)}
		if err := m.WriteSample(video, samples[i]); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stbl := stblOf(t, progressiveMoov(t, buf.Bytes()), video)
	// 512x3, 600x3, 512x1: three entries for seven samples, not seven.
	wantCounts := []uint32{3, 3, 1}
	wantDeltas := []uint32{512, 600, 512}
	if fmt.Sprint(stbl.Stts.SampleCount) != fmt.Sprint(wantCounts) ||
		fmt.Sprint(stbl.Stts.SampleTimeDelta) != fmt.Sprint(wantDeltas) {
		t.Errorf("stts = %v x %v, want %v x %v",
			stbl.Stts.SampleCount, stbl.Stts.SampleTimeDelta, wantCounts, wantDeltas)
	}
	if stbl.Ctts == nil {
		t.Fatal("no ctts although samples present out of decode order")
	}
	if got := stbl.Ctts.NrSampleCount(); got != 3 {
		t.Errorf("ctts entries = %d, want 3: %+v", got, stbl.Ctts)
	}
	if stbl.Ctts.Version != 1 {
		t.Errorf("ctts version = %d, want 1 for a negative offset", stbl.Ctts.Version)
	}
	for i, want := range []int32{0, 0, 1024, 1024, 1024, -512, -512} {
		if got := stbl.Ctts.GetCompositionTimeOffset(uint32(i) + 1); got != want {
			t.Errorf("composition offset of sample %d = %d, want %d", i+1, got, want)
		}
	}
	// A single chunk holds them all, so one stsc entry addresses the lot.
	if len(stbl.Stsc.Entries) != 1 {
		t.Errorf("stsc = %+v", stbl.Stsc.Entries)
	}
	if len(stbl.Stco.ChunkOffset) != 1 {
		t.Errorf("stco = %v", stbl.Stco.ChunkOffset)
	}

	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	read, err := r.Samples(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != len(samples) {
		t.Fatalf("read back %d samples, want %d", len(read), len(samples))
	}
	for i, s := range read {
		if s.Duration != samples[i].Duration ||
			s.CompositionOffset != samples[i].CompositionOffset ||
			s.Sync != samples[i].Sync ||
			!bytes.Equal(s.Data, samples[i].Data) {
			t.Errorf("sample %d = %+v, want %+v", i, s, samples[i])
		}
	}
}

func TestProgressiveInterleavesTracksInDecodeOrder(t *testing.T) {
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(80*time.Millisecond))
	video, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := m.WriteSample(video, Sample{Data: []byte{1, 2, 3, 4, 5, 6}, Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(audio, Sample{Data: []byte{7, 8}, Duration: 1024, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	moov := progressiveMoov(t, buf.Bytes())
	vchunks := stblOf(t, moov, video).Stco.ChunkOffset
	achunks := stblOf(t, moov, audio).Stco.ChunkOffset
	if len(vchunks) < 3 || len(achunks) < 3 {
		t.Fatalf("chunks: video %v, audio %v", vchunks, achunks)
	}
	// Interleaved means neither track's media sits entirely before the
	// other's: a player reading front to back meets both as it goes.
	if vchunks[len(vchunks)-1] < achunks[0] || achunks[len(achunks)-1] < vchunks[0] {
		t.Fatalf("the tracks are not interleaved: video %v, audio %v", vchunks, achunks)
	}
	// Chunk offsets rise with decode time within a track, and each track's
	// chunks are separated by the other's.
	crossings := 0
	for _, a := range achunks {
		for i := 0; i+1 < len(vchunks); i++ {
			if vchunks[i] < a && a < vchunks[i+1] {
				crossings++
			}
		}
	}
	if crossings < 2 {
		t.Errorf("audio chunks fall between video chunks %d times: video %v, audio %v",
			crossings, vchunks, achunks)
	}
}

// offsetSink is a writer that can also seek, the way a file can, and that
// starts wherever it is told. Only the bytes actually written are kept, so a
// file whose chunks sit beyond four gigabytes costs a few hundred bytes to
// produce rather than four gigabytes.
type offsetSink struct {
	origin int64
	pos    int64
	data   []byte

	seeks, writes           int
	failSeekAt, failWriteAt int // 1-based call number that fails; 0 for none
}

func newOffsetSink(origin int64) *offsetSink {
	return &offsetSink{origin: origin, pos: origin}
}

func (s *offsetSink) Write(p []byte) (int, error) {
	s.writes++
	if s.writes == s.failWriteAt {
		return 0, errors.New("disk full")
	}
	at := s.pos - s.origin
	if at < 0 {
		return 0, fmt.Errorf("write at %d, before the origin %d", s.pos, s.origin)
	}
	if grow := at + int64(len(p)) - int64(len(s.data)); grow > 0 {
		s.data = append(s.data, make([]byte, grow)...)
	}
	copy(s.data[at:], p)
	s.pos += int64(len(p))
	return len(p), nil
}

func (s *offsetSink) Seek(offset int64, whence int) (int64, error) {
	s.seeks++
	if s.seeks == s.failSeekAt {
		return 0, errors.New("this device does not seek")
	}
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	default:
		return 0, fmt.Errorf("unexpected whence %d", whence)
	}
	return s.pos, nil
}

// writeOneVideoTrack writes a short video track and closes the muxer, which is
// what most of what follows needs and none of it varies.
func writeOneVideoTrack(t *testing.T, m *ProgressiveMuxer, samples int) uint32 {
	t.Helper()
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for i := 0; i < samples; i++ {
		if err := m.WriteSample(id, Sample{
			Data: []byte{0, 0, 0, 2, 9, byte(i)}, Duration: 512, Sync: i == 0,
		}); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return id
}

func TestProgressiveStreamsToAWriterThatSeeks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewProgressiveMuxer(f, ChunkDuration(80*time.Millisecond))
	video := writeOneVideoTrack(t, m, 8)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Nothing was held: the media went out to the file as it arrived, and only
	// the mdat size had to be patched afterwards.
	if len(m.buffered) != 0 {
		t.Errorf("%d bytes were held although the writer can seek", len(m.buffered))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The mdat of a streamed file carries a 64-bit size, because its value is
	// only known once every sample has been written.
	if got := binary.BigEndian.Uint32(data[28:]); got != 1 {
		t.Errorf("mdat size field = %d, want 1 (a 64-bit size follows)", got)
	}
	if got := binary.BigEndian.Uint64(data[36:]); got != mdatLargeHeaderLen+8*6 {
		t.Errorf("mdat 64-bit size = %d, want %d", got, mdatLargeHeaderLen+8*6)
	}
	moov := progressiveMoov(t, data)
	stco := stblOf(t, moov, video).Stco
	if stco == nil || stco.ChunkOffset[0] != 28+mdatLargeHeaderLen {
		t.Fatalf("stco = %+v", stco)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 8 {
		t.Fatalf("read back %d samples", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, []byte{0, 0, 0, 2, 9, byte(i)}) || s.Duration != 512 || s.Sync != (i == 0) {
			t.Errorf("sample %d = %+v", i, s)
		}
	}
}

func TestProgressiveWritesIntoAFileThatAlreadyHoldsSomething(t *testing.T) {
	// stco and co64 state offsets from the start of the file, so a muxer
	// handed a writer positioned past something else has to count from there.
	const origin = 1000
	sink := newOffsetSink(origin)
	m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour))
	video := writeOneVideoTrack(t, m, 3)
	moov := progressiveMoov(t, sink.data)
	stco := stblOf(t, moov, video).Stco
	if stco == nil {
		t.Fatal("no stco")
	}
	if want := uint32(origin + 28 + mdatLargeHeaderLen); stco.ChunkOffset[0] != want {
		t.Errorf("chunk offset = %d, want %d", stco.ChunkOffset[0], want)
	}
}

func TestProgressiveCrossesToCo64AtTheRealBoundary(t *testing.T) {
	// The one chunk of a one-sample file starts right after a 28-byte ftyp and
	// the 16-byte mdat header of a streamed file, so putting the file's first
	// byte here puts that chunk at the largest offset an stco entry can state.
	const lastFits = int64(math.MaxUint32) - 28 - mdatLargeHeaderLen
	for _, tc := range []struct {
		name   string
		origin int64
		wide   bool
		offset uint64
	}{
		{"largest offset stco can state", lastFits, false, math.MaxUint32},
		{"one byte further", lastFits + 1, true, 1 << 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newOffsetSink(tc.origin)
			m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour))
			video := writeOneVideoTrack(t, m, 1)
			// The bytes are a whole file; only the offsets its tables state
			// point past the end of this slice, which is the point.
			stbl := stblOf(t, progressiveMoov(t, sink.data), video)
			switch {
			case tc.wide && (stbl.Co64 == nil || stbl.Stco != nil):
				t.Fatalf("stco=%+v co64=%+v, want co64 alone", stbl.Stco, stbl.Co64)
			case !tc.wide && (stbl.Stco == nil || stbl.Co64 != nil):
				t.Fatalf("stco=%+v co64=%+v, want stco alone", stbl.Stco, stbl.Co64)
			}
			var got uint64
			if tc.wide {
				got = stbl.Co64.ChunkOffset[0]
			} else {
				got = uint64(stbl.Stco.ChunkOffset[0])
			}
			if got != tc.offset {
				t.Errorf("chunk offset = %d, want %d", got, tc.offset)
			}
			// The offsets really are beyond what this slice holds, which is
			// what makes the crossover the right decision.
			r, err := NewReader(sink.data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Samples(video); !errors.Is(err, ErrSampleData) {
				t.Errorf("Samples error = %v, want %v", err, ErrSampleData)
			}
		})
	}
}

func TestProgressiveCo64ReadsBackThroughTheReader(t *testing.T) {
	// Proving the wide table end to end needs a file whose chunks sit past
	// four gigabytes, which is not something to write. The threshold is
	// lowered instead: the table is built and read by exactly the code a real
	// file of that size would use, and the value of the threshold itself is
	// what the boundary test above pins down.
	restore := maxChunkOffset32
	maxChunkOffset32 = 1
	defer func() { maxChunkOffset32 = restore }()

	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(80*time.Millisecond))
	video := writeOneVideoTrack(t, m, 8)
	stbl := stblOf(t, progressiveMoov(t, buf.Bytes()), video)
	if stbl.Co64 == nil || stbl.Stco != nil {
		t.Fatalf("stco=%+v co64=%+v, want co64 alone", stbl.Stco, stbl.Co64)
	}
	if stbl.Co64.ChunkOffset[0] != 36 {
		t.Errorf("co64 offsets = %v, want the first at 36", stbl.Co64.ChunkOffset)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(video)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 8 {
		t.Fatalf("read back %d samples through co64", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, []byte{0, 0, 0, 2, 9, byte(i)}) {
			t.Errorf("sample %d = %x", i, s.Data)
		}
	}
}

func TestProgressiveRefusals(t *testing.T) {
	t.Run("a file with no track names nothing", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		if err := m.WriteSample(1, Sample{Data: []byte{1}, Duration: 1}); !errors.Is(err, ErrNoTracks) {
			t.Errorf("WriteSample = %v, want %v", err, ErrNoTracks)
		}
		if err := m.Close(); !errors.Is(err, ErrNoTracks) {
			t.Errorf("Close = %v, want %v", err, ErrNoTracks)
		}
	})

	t.Run("a track with no timescale has no unit of time", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		if _, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "avc1"}); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("AddTrack = %v, want %v", err, ErrTrackConfig)
		}
	})

	t.Run("a codec without its parameter sets cannot be described", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		if _, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "avc1", Timescale: 90000}); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("AddTrack = %v, want %v", err, ErrTrackConfig)
		}
		if _, err := m.AddTrack(TrackConfig{Kind: Video, Codec: "nope", Timescale: 90000}); !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("AddTrack = %v, want %v", err, ErrUnsupportedCodec)
		}
	})

	t.Run("a track added after the first sample is not in the moov", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddTrack(audioConfig()); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("AddTrack = %v, want %v", err, ErrTrackConfig)
		}
	})

	t.Run("a sample must carry data and last some time", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		id, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, Sample{Duration: 512}); !errors.Is(err, ErrSample) {
			t.Errorf("empty sample = %v, want %v", err, ErrSample)
		}
		if err := m.WriteSample(id, Sample{Data: []byte{1}}); !errors.Is(err, ErrSample) {
			t.Errorf("sample without duration = %v, want %v", err, ErrSample)
		}
		if err := m.WriteSample(9, Sample{Data: []byte{1}, Duration: 512}); !errors.Is(err, ErrUnknownTrack) {
			t.Errorf("unknown track = %v, want %v", err, ErrUnknownTrack)
		}
	})

	t.Run("a closed muxer refuses everything", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		id := writeOneVideoTrack(t, m, 1)
		if err := m.Close(); !errors.Is(err, ErrClosed) {
			t.Errorf("second Close = %v, want %v", err, ErrClosed)
		}
		if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512}); !errors.Is(err, ErrClosed) {
			t.Errorf("WriteSample = %v, want %v", err, ErrClosed)
		}
		if _, err := m.AddTrack(audioConfig()); !errors.Is(err, ErrClosed) {
			t.Errorf("AddTrack = %v, want %v", err, ErrClosed)
		}
	})

	t.Run("a track declared but never written would name nothing", func(t *testing.T) {
		// A progressive moov whose first track has an empty sample table is
		// read back as the initialisation segment of a fragmented file, so
		// such a file must not be written at all.
		m := NewProgressiveMuxer(io.Discard)
		if _, err := m.AddTrack(videoConfig(t)); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); !errors.Is(err, ErrNoSamples) {
			t.Errorf("Close = %v, want %v", err, ErrNoSamples)
		}
	})

	t.Run("a track left empty beside a written one", func(t *testing.T) {
		m := NewProgressiveMuxer(io.Discard)
		video, err := m.AddTrack(videoConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddTrack(audioConfig()); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(video, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); !errors.Is(err, ErrNoSamples) {
			t.Errorf("Close = %v, want %v", err, ErrNoSamples)
		}
	})
}

func TestProgressiveOptions(t *testing.T) {
	m := NewProgressiveMuxer(io.Discard, ChunkDuration(0), ProgressiveBrand(""), MediaMemoryLimit(0))
	if m.settings.chunkDuration != DefaultChunkDuration {
		t.Errorf("chunk duration = %v", m.settings.chunkDuration)
	}
	if m.settings.brand != DefaultProgressiveBrand {
		t.Errorf("brand = %q", m.settings.brand)
	}
	if m.settings.memoryLimit != DefaultMediaMemoryLimit {
		t.Errorf("memory limit = %d", m.settings.memoryLimit)
	}
	// A limit above what an mdat with a 32-bit size can announce is lowered to
	// it: holding more could not be written out as one box.
	wide := NewProgressiveMuxer(io.Discard, MediaMemoryLimit(1<<40))
	if wide.settings.memoryLimit != maxMdatPayload {
		t.Errorf("memory limit = %d, want %d", wide.settings.memoryLimit, maxMdatPayload)
	}
	small := NewProgressiveMuxer(io.Discard, MediaMemoryLimit(4096))
	if small.settings.memoryLimit != 4096 {
		t.Errorf("memory limit = %d", small.settings.memoryLimit)
	}
}

func TestProgressiveBrandReachesTheFtyp(t *testing.T) {
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ProgressiveBrand("mp42"))
	writeOneVideoTrack(t, m, 1)
	file, err := Demux(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if file.Brand != "mp42" {
		t.Errorf("brand = %q", file.Brand)
	}
	// A major brand the compatible list does not already claim is added to it;
	// one it does is not repeated.
	if got := progressiveBrands("mp42"); len(got) != 4 || got[3] != "mp42" {
		t.Errorf("compatible brands = %v", got)
	}
	if got := progressiveBrands("isom"); len(got) != 3 {
		t.Errorf("compatible brands = %v", got)
	}
}

func TestProgressiveMemoryLimitStopsAnUnboundedFile(t *testing.T) {
	// A writer that cannot seek has to be handed the mdat size before the
	// media, so the media is held; past the limit the muxer says so instead of
	// growing until the machine gives up.
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(time.Hour), MediaMemoryLimit(16))
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 8 && last == nil; i++ {
		last = m.WriteSample(id, Sample{Data: make([]byte, 6), Duration: 512, Sync: true})
	}
	if !errors.Is(last, ErrMediaTooLarge) {
		t.Fatalf("error = %v, want %v", last, ErrMediaTooLarge)
	}
}

func TestProgressiveChunksOnACoarseTimescale(t *testing.T) {
	// A chunk shorter than one unit of the track's timescale still closes on
	// every sample rather than never.
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(time.Millisecond))
	id, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1, Channels: 2, SampleRate: 48000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.chunkLimit(m.byID[id]); got != 1 {
		t.Errorf("chunk limit = %d, want 1", got)
	}
	for i := 0; i < 3; i++ {
		if err := m.WriteSample(id, Sample{Data: []byte{1, 2}, Duration: 1, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	stbl := stblOf(t, progressiveMoov(t, buf.Bytes()), id)
	if len(stbl.Stco.ChunkOffset) != 3 {
		t.Errorf("chunks = %v, want one per sample", stbl.Stco.ChunkOffset)
	}
}

func TestProgressiveWidensTheHeadersOfALongFile(t *testing.T) {
	// A duration past what a 32-bit field holds needs the 64-bit form of the
	// header boxes; written as version 0 it would announce a duration that has
	// wrapped.
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf, ChunkDuration(time.Hour))
	id, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1000, Channels: 2, SampleRate: 48000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.WriteSample(id, Sample{Data: []byte{1, 2}, Duration: 1 << 31, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	moov := progressiveMoov(t, buf.Bytes())
	if moov.Mvhd.Version != 1 || moov.Mvhd.Duration != 3<<31 {
		t.Errorf("mvhd version %d duration %d", moov.Mvhd.Version, moov.Mvhd.Duration)
	}
	trak := moov.Traks[0]
	if trak.Tkhd.Version != 1 || trak.Tkhd.Duration != 3<<31 {
		t.Errorf("tkhd version %d duration %d", trak.Tkhd.Version, trak.Tkhd.Duration)
	}
	if trak.Mdia.Mdhd.Version != 1 || trak.Mdia.Mdhd.Duration != 3<<31 {
		t.Errorf("mdhd version %d duration %d", trak.Mdia.Mdhd.Version, trak.Mdia.Mdhd.Duration)
	}
}

func TestProgressiveMemoryLimitOnAWriterThatSeeks(t *testing.T) {
	// A writer that can seek frees its room by handing the open chunks over,
	// so only a single sample larger than the whole limit is refused.
	sink := newOffsetSink(0)
	m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour), MediaMemoryLimit(8))
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := m.WriteSample(id, Sample{Data: make([]byte, 6), Duration: 512, Sync: true}); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
	}
	if err := m.WriteSample(id, Sample{Data: make([]byte, 9), Duration: 512}); !errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("oversized sample = %v, want %v", err, ErrMediaTooLarge)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stbl := stblOf(t, progressiveMoov(t, sink.data), id)
	// Each sample had to close a chunk of its own, the limit leaving no room
	// for two.
	if len(stbl.Stco.ChunkOffset) != 4 {
		t.Errorf("chunks = %v, want one per sample", stbl.Stco.ChunkOffset)
	}
}

func TestProgressiveWriteFailures(t *testing.T) {
	// A short video track, to measure where each box of the output starts.
	var sized bytes.Buffer
	probe := NewProgressiveMuxer(&sized, ChunkDuration(time.Hour))
	writeOneVideoTrack(t, probe, 2)
	whole := sized.Len()
	const ftypLen = 28
	const mediaLen = 2 * 6

	for _, tc := range []struct {
		name  string
		allow int
	}{
		{"the file type cannot be written", 0},
		{"the media data box header cannot be written", ftypLen},
		{"the media data cannot be written", ftypLen + mdatHeaderLen},
		{"the movie box cannot be written", ftypLen + mdatHeaderLen + mediaLen},
		{"the movie box is cut short", whole - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewProgressiveMuxer(&failWriter{allow: tc.allow}, ChunkDuration(time.Hour))
			id, err := m.AddTrack(videoConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for i := 0; i < 2 && last == nil; i++ {
				last = m.WriteSample(id, Sample{
					Data: []byte{0, 0, 0, 2, 9, byte(i)}, Duration: 512, Sync: i == 0,
				})
			}
			if last == nil {
				last = m.Close()
			}
			if last == nil {
				t.Fatalf("a writer refusing everything past %d bytes was ignored", tc.allow)
			}
		})
	}
}

func TestProgressiveStreamedWriteFailures(t *testing.T) {
	// The writes of the streaming path, in the order they happen: the file
	// type, the media data box header, the media itself, the patched size and
	// the movie box.
	for _, tc := range []struct {
		name string
		nth  int
	}{
		{"the file type", 1},
		{"the media data box header", 2},
		{"the media data", 3},
		{"the patched media data size", 4},
		{"the movie box", 5},
	} {
		t.Run(tc.name+" cannot be written", func(t *testing.T) {
			sink := newOffsetSink(0)
			sink.failWriteAt = tc.nth
			m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour))
			id, err := m.AddTrack(videoConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			last := m.WriteSample(id, Sample{Data: []byte{1, 2, 3, 4, 5, 6}, Duration: 512, Sync: true})
			if last == nil {
				last = m.Close()
			}
			if last == nil {
				t.Fatalf("a writer failing on write %d was ignored", tc.nth)
			}
		})
	}
}

func TestProgressiveSeekFailures(t *testing.T) {
	// The seeks of the streaming path: finding where the file starts, going
	// back to the media data size, and returning past the media to write the
	// movie box.
	for _, nth := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("seek %d fails", nth), func(t *testing.T) {
			sink := newOffsetSink(0)
			sink.failSeekAt = nth
			m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour))
			id, err := m.AddTrack(videoConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			last := m.WriteSample(id, Sample{Data: []byte{1, 2, 3, 4, 5, 6}, Duration: 512, Sync: true})
			if last == nil {
				last = m.Close()
			}
			if last == nil {
				t.Fatalf("a writer failing on seek %d was ignored", nth)
			}
		})
	}
}

// nowhereSeeker is a writer that claims to sit at a position no file has. A
// chunk offset counted from there would be meaningless, so it is refused.
type nowhereSeeker struct{}

func (nowhereSeeker) Write(p []byte) (int, error) { return len(p), nil }

func (nowhereSeeker) Seek(int64, int) (int64, error) { return -1, nil }

func TestProgressiveRefusesAWriterAtANegativePosition(t *testing.T) {
	m := NewProgressiveMuxer(nowhereSeeker{})
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	err = m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true})
	if err == nil || !strings.Contains(err.Error(), "position -1") {
		t.Errorf("error = %v, want the reported position", err)
	}
}

func TestProgressiveReportsAChunkTableItCannotBuild(t *testing.T) {
	// The chunk table is built from chunk one upwards, which is the only thing
	// the reference library refuses, so the call is staged to fail.
	restore := addChunkEntry
	addChunkEntry = func(*mp4.StscBox, uint32, uint32, uint32) error {
		return errors.New("first stsc entry does not have firstChunk == 1")
	}
	defer func() { addChunkEntry = restore }()

	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 512, Sync: true}); err != nil {
		t.Fatal(err)
	}
	err = m.Close()
	if err == nil || !strings.Contains(err.Error(), "chunk table of track 1") {
		t.Errorf("Close = %v, want the chunk table to be blamed", err)
	}
}

func TestFragmentedToProgressiveRoundTrip(t *testing.T) {
	// A fragmented file written by this package's other muxer, read back, and
	// written out again as a progressive one: the samples that come out the far
	// end must be the samples that went in.
	var fragmented bytes.Buffer
	fmux := NewMuxer(&fragmented, FragmentDuration(80*time.Millisecond))
	video, err := fmux.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := fmux.AddTrack(audioConfig())
	if err != nil {
		t.Fatal(err)
	}
	const frames = 10
	want := map[uint32][]Sample{}
	for i := 0; i < frames; i++ {
		v := Sample{
			Data:              []byte{0, 0, 0, 2, 9, byte(i)},
			Duration:          512,
			CompositionOffset: int32(512 * (i / 3)),
			Sync:              i%3 == 0,
		}
		if err := fmux.WriteSample(video, v); err != nil {
			t.Fatal(err)
		}
		want[video] = append(want[video], v)
		a := Sample{Data: []byte{0xa0, byte(i)}, Duration: 1024, Sync: true}
		if err := fmux.WriteSample(audio, a); err != nil {
			t.Fatal(err)
		}
		want[audio] = append(want[audio], a)
	}
	if err := fmux.Close(); err != nil {
		t.Fatal(err)
	}
	if n := countBoxes(t, fragmented.Bytes(), "moof"); n < 2 {
		t.Fatalf("the input carries %d fragments, so it proves nothing", n)
	}

	in, err := NewReader(fragmented.Bytes())
	if err != nil {
		t.Fatalf("read the fragmented file: %v", err)
	}
	var progressive bytes.Buffer
	pmux := NewProgressiveMuxer(&progressive, ChunkDuration(80*time.Millisecond))
	ids := map[uint32]uint32{}
	// Samples are handed over interleaved, the way they decode, so the
	// progressive file keeps the tracks side by side.
	var byTrack [][]Sample
	var order []uint32
	for _, id := range in.TrackIDs() {
		cfg, err := in.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		newID, err := pmux.AddTrack(cfg)
		if err != nil {
			t.Fatalf("AddTrack(%d): %v", id, err)
		}
		ids[id] = newID
		samples, err := in.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		byTrack = append(byTrack, samples)
		order = append(order, newID)
	}
	for i := 0; i < frames; i++ {
		for track, samples := range byTrack {
			if err := pmux.WriteSample(order[track], samples[i]); err != nil {
				t.Fatalf("WriteSample: %v", err)
			}
		}
	}
	if err := pmux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	moov := progressiveMoov(t, progressive.Bytes())
	if len(moov.Traks) != 2 {
		t.Fatalf("tracks = %d", len(moov.Traks))
	}
	out, err := NewReader(progressive.Bytes())
	if err != nil {
		t.Fatalf("read the progressive file: %v", err)
	}
	for oldID, newID := range ids {
		got, err := out.Samples(newID)
		if err != nil {
			t.Fatalf("Samples(%d): %v", newID, err)
		}
		expect := want[oldID]
		if len(got) != len(expect) {
			t.Fatalf("track %d came back with %d samples, want %d", newID, len(got), len(expect))
		}
		for i, s := range got {
			if !bytes.Equal(s.Data, expect[i].Data) || s.Duration != expect[i].Duration ||
				s.CompositionOffset != expect[i].CompositionOffset || s.Sync != expect[i].Sync {
				t.Errorf("track %d sample %d = %+v, want %+v", newID, i, s, expect[i])
			}
		}
	}
	// The durations survive the trip as a whole, not only sample by sample.
	for _, track := range out.File().Tracks {
		var total uint64
		for _, s := range want[track.ID] {
			total += uint64(s.Duration)
		}
		if track.Duration != total {
			t.Errorf("track %d duration = %d, want %d", track.ID, track.Duration, total)
		}
	}
}

func TestProgressiveReportsAFlushMadeToFreeMemory(t *testing.T) {
	// Making room for a sample closes the open chunks, and that write can fail
	// like any other: the sample is refused with the reason, not with the
	// limit.
	sink := newOffsetSink(0)
	sink.failWriteAt = 3 // the file type, the mdat header, then the media
	m := NewProgressiveMuxer(sink, ChunkDuration(time.Hour), MediaMemoryLimit(6))
	id, err := m.AddTrack(videoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: make([]byte, 6), Duration: 512, Sync: true}); err != nil {
		t.Fatal(err)
	}
	err = m.WriteSample(id, Sample{Data: make([]byte, 6), Duration: 512})
	if err == nil {
		t.Fatal("the failing flush was not reported")
	}
	if errors.Is(err, ErrMediaTooLarge) {
		t.Errorf("error = %v, want the write failure rather than the limit", err)
	}
}

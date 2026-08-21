// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asticode/go-astits"
)

// tsSegment builds one self-contained transport stream, as an HLS segment is:
// its own tables, and access units that simply stop at its end.
func tsSegment(t *testing.T, units int, firstPTS int64, marker byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: astits.StreamTypeH264Video}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)
	sps, pps, _, _ := avcParameterSets(t)
	for i := 0; i < units; i++ {
		var au bytes.Buffer
		nalus := [][]byte{{0x09, 0x10}}
		if i == 0 {
			// A segment repeats the parameter sets, so a player can join the
			// stream at any of them.
			nalus = append(nalus, sps[0], pps[0])
		}
		nalus = append(nalus, []byte{0x65, marker, byte(i + 1)})
		for _, nalu := range nalus {
			au.Write([]byte{0, 0, 0, 1})
			au.Write(nalu)
		}
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: firstPTS + int64(i)*3600},
			}},
			Data: au.Bytes(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// markersOf lists the byte each picture was stamped with, so a lost or
// reordered access unit is named rather than merely counted.
func markersOf(t *testing.T, samples []Sample) []byte {
	t.Helper()
	var out []byte
	for _, s := range samples {
		nalus, ok := splitLengthPrefixed(s.Data)
		if !ok || len(nalus) == 0 {
			t.Fatalf("a sample holds no NAL unit: %x", s.Data)
		}
		out = append(out, nalus[0][1])
	}
	return out
}

func TestSegmentedReaderKeepsEverySegmentsLastUnit(t *testing.T) {
	first := tsSegment(t, 3, 0, 0xa1)
	second := tsSegment(t, 3, 10800, 0xb2)
	third := tsSegment(t, 3, 21600, 0xc3)

	// Read as one concatenation, the unit each segment ends on is lost: the
	// demuxer is still accumulating it when the next segment's tables arrive.
	joined := append(append(append([]byte{}, first...), second...), third...)
	blob, err := NewReader(joined)
	if err != nil {
		t.Fatal(err)
	}
	lossy, err := blob.Samples(256)
	if err != nil {
		t.Fatal(err)
	}
	if len(lossy) != 7 {
		t.Fatalf("a concatenation gave %d units; this test exists because it gives 7", len(lossy))
	}

	// Handed over as segments, all nine are there, in order.
	seq, err := NewSegmentedReader([][]byte{first, second, third})
	if err != nil {
		t.Fatalf("NewSegmentedReader: %v", err)
	}
	samples, err := seq.Samples(256)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 9 {
		t.Fatalf("read %d units, want the 9 that were written", len(samples))
	}
	want := []byte{0xa1, 0xa1, 0xa1, 0xb2, 0xb2, 0xb2, 0xc3, 0xc3, 0xc3}
	if got := markersOf(t, samples); !bytes.Equal(got, want) {
		t.Fatalf("units read %x, want %x", got, want)
	}
	for i, s := range samples {
		if s.Duration == 0 {
			t.Errorf("unit %d has no duration", i)
		}
	}
	// The metadata describes the whole sequence, not one segment.
	file := seq.File()
	if file.Format != "mpegts" || len(file.Tracks) != 1 {
		t.Fatalf("file = %+v", file)
	}
	if file.Tracks[0].Duration != file.Duration || file.Duration == 0 {
		t.Fatalf("duration = %d, track = %d", file.Duration, file.Tracks[0].Duration)
	}
	cfg, err := seq.TrackConfig(256)
	if err != nil || cfg.Codec != "avc1" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}

func TestSegmentedReaderRemuxesIntoOneFile(t *testing.T) {
	// What the whole thing is for: several segments become one MP4.
	seq, err := NewSegmentedReader([][]byte{
		tsSegment(t, 2, 0, 0x11), tsSegment(t, 2, 7200, 0x22),
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m := NewMuxer(&out)
	id, err := m.AddTrack(mustConfig(t, seq, 256))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := seq.Samples(256)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatalf("the remuxed file does not read back: %v", err)
	}
	got, err := back.Samples(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("the MP4 holds %d units, want 4", len(got))
	}
}

func mustConfig(t *testing.T, r *Reader, id uint32) TrackConfig {
	t.Helper()
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	return cfg
}

func TestSegmentedReaderEdges(t *testing.T) {
	if _, err := NewSegmentedReader(nil); !errors.Is(err, ErrNoSamples) {
		t.Errorf("no segment: %v", err)
	}
	// One segment is just a reader.
	one := tsSegment(t, 2, 0, 0x33)
	r, err := NewSegmentedReader([][]byte{one})
	if err != nil {
		t.Fatalf("one segment: %v", err)
	}
	if samples, err := r.Samples(256); err != nil || len(samples) != 2 {
		t.Fatalf("one segment gave %d units, %v", len(samples), err)
	}
	// A segment that cannot be read names itself.
	if _, err := NewSegmentedReader([][]byte{one, []byte("not a stream")}); err == nil {
		t.Error("an unreadable segment was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("segment 2")) {
		t.Errorf("the error must name the segment: %v", err)
	}
	// Segments carrying no track at all.
	empty := make([]byte, 4*tsPacketSize)
	for i := 0; i < len(empty); i += tsPacketSize {
		empty[i], empty[i+1], empty[i+2], empty[i+3] = 0x47, 0x1f, 0xff, 0x10
	}
	if _, err := NewSegmentedReader([][]byte{empty, empty}); err == nil {
		t.Error("segments declaring nothing were accepted")
	}
	// Segments that read, name a track, and carry not one sample for it: an
	// initialisation segment on its own is exactly that.
	var init bytes.Buffer
	m := NewMuxer(&init)
	if _, err := m.AddTrack(videoConfig(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSegmentedReader([][]byte{init.Bytes(), init.Bytes()}); !errors.Is(err, ErrNoSamples) {
		t.Errorf("segments without a single sample: %v, want ErrNoSamples", err)
	}
}

func TestSegmentedReaderJoinsMP4Segments(t *testing.T) {
	// The same works for fragmented MP4 segments, which is what a DASH or an
	// HLS playlist delivers when it is not carrying transport streams.
	part := func(marker byte) []byte {
		var buf bytes.Buffer
		m := NewMuxer(&buf)
		id, err := m.AddTrack(TrackConfig{
			Kind: Video, Codec: "av01", Timescale: 30000, Width: 64, Height: 64,
			CodecConfig: []byte{0x81, 0x00, 0x0c, 0x00},
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := m.WriteSample(id, Sample{
				Data: []byte{marker, byte(i)}, Duration: 1001, Sync: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	seq, err := NewSegmentedReader([][]byte{part(0x01), part(0x02)})
	if err != nil {
		t.Fatalf("NewSegmentedReader: %v", err)
	}
	ids := seq.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("tracks = %v", ids)
	}
	samples, err := seq.Samples(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 4 {
		t.Fatalf("read %d samples, want 4", len(samples))
	}
	if samples[0].Data[0] != 0x01 || samples[2].Data[0] != 0x02 {
		t.Fatalf("the segments were not kept in order: %x %x", samples[0].Data, samples[2].Data)
	}
}

// tsSegmentDeclaringTwo builds a segment whose table names two streams but
// which carries data for one of them only — a playlist may drop its audio for
// a segment, and the video must still read.
func tsSegmentDeclaringTwo(t *testing.T, firstPTS int64, marker byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	for _, es := range []astits.PMTElementaryStream{
		{ElementaryPID: 256, StreamType: astits.StreamTypeH264Video},
		{ElementaryPID: 257, StreamType: astits.StreamTypeAACAudio},
	} {
		if err := m.AddElementaryStream(es); err != nil {
			t.Fatal(err)
		}
	}
	m.SetPCRPID(256)
	sps, pps, _, _ := avcParameterSets(t)
	for i := 0; i < 2; i++ {
		var au bytes.Buffer
		nalus := [][]byte{{0x09, 0x10}}
		if i == 0 {
			nalus = append(nalus, sps[0], pps[0])
		}
		nalus = append(nalus, []byte{0x65, marker, byte(i + 1)})
		for _, nalu := range nalus {
			au.Write([]byte{0, 0, 0, 1})
			au.Write(nalu)
		}
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xE0, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: firstPTS + int64(i)*3600},
			}},
			Data: au.Bytes(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestSegmentedReaderSkipsATrackASegmentDoesNotCarry(t *testing.T) {
	seq, err := NewSegmentedReader([][]byte{
		tsSegmentDeclaringTwo(t, 0, 0x44),
		tsSegmentDeclaringTwo(t, 7200, 0x55),
	})
	if err != nil {
		t.Fatalf("NewSegmentedReader: %v", err)
	}
	// The declared but silent audio track is not offered, and the video of
	// both segments is there.
	if ids := seq.TrackIDs(); len(ids) != 1 || ids[0] != 256 {
		t.Fatalf("tracks = %v, want only the one carrying data", ids)
	}
	samples, err := seq.Samples(256)
	if err != nil {
		t.Fatal(err)
	}
	if got := markersOf(t, samples); !bytes.Equal(got, []byte{0x44, 0x44, 0x55, 0x55}) {
		t.Fatalf("units read %x", got)
	}
}

func TestSegmentedReaderReportsAConfigurationItCannotRead(t *testing.T) {
	original := trackConfig
	defer func() { trackConfig = original }()
	trackConfig = func(*Reader, uint32) (TrackConfig, error) {
		return TrackConfig{}, errors.New("unreadable configuration")
	}
	_, err := NewSegmentedReader([][]byte{tsSegment(t, 2, 0, 0x66), tsSegment(t, 2, 7200, 0x77)})
	if err == nil {
		t.Fatal("a configuration that cannot be read must be reported")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("segment 1")) {
		t.Errorf("the error must name the segment: %v", err)
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

// TestTSMuxPresentsEveryTrackFromItsFirstPresentedFrame is the B-frame defect
// of the transport stream muxer. x264 -bf 2 at 25 fps gives the first picture a
// composition offset of one or two frames, and an AAC encoder a 1024-sample
// priming. Before, the picture was shown at its offset and the priming at zero:
// ffprobe read video start_time 0.040 against audio 0.000 for an ffmpeg mkv.
// Now the first presented picture and the first presented sound -- the one
// after the priming -- share the program's start, tsTimestampOffset, and the
// units decoded before it are stated before it, as ffmpeg's mpegtsenc.c states
// them.
func TestTSMuxPresentsEveryTrackFromItsFirstPresentedFrame(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	vcfg := tsVideoConfig(t)
	vcfg.Timescale = 12800
	video, err := m.AddTrack(vcfg)
	if err != nil {
		t.Fatal(err)
	}
	acfg := tsAudioConfig()
	acfg.Timescale = 48000
	acfg.StartDelay = 1024 * time.Second / 48000
	audio, err := m.AddTrack(acfg)
	if err != nil {
		t.Fatal(err)
	}
	// I P B B in decode order, 512 ticks a frame, presented I B B P: the
	// I-frame two frames after it is decoded, as x264 -bf 2 orders them.
	for _, s := range []Sample{
		{Data: lengthPrefixed(tsIDR), Duration: 512, CompositionOffset: 1024, Sync: true},
		{Data: lengthPrefixed(tsSlice), Duration: 512, CompositionOffset: 2048},
		{Data: lengthPrefixed(tsSlice), Duration: 512, CompositionOffset: 512},
		{Data: lengthPrefixed(tsSlice), Duration: 512, CompositionOffset: 512},
	} {
		if err := m.WriteSample(video, s); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := m.WriteSample(audio, Sample{Data: []byte{1, 2, 3}, Duration: 1024, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	v := tsUnitsOfStream(t, buf.Bytes(), uint16(video))
	// 512 ticks of 12800 are 3600 of the transport clock; shown in the order
	// I B B P, from the program's start.
	if len(v) != 4 {
		t.Fatalf("%d video units, want 4", len(v))
	}
	for i, frame := range []int64{0, 3, 1, 2} {
		if got, want := v[i].PES.Header.OptionalHeader.PTS.Base, tsTimestampOffset+frame*3600; got != want {
			t.Errorf("video unit %d is shown at %d, want %d", i, got, want)
		}
	}
	// The first picture is shown at the program's start and decoded 80 ms
	// before it; the clock reference precedes that decode by tsMuxDelay.
	first := v[0].PES.Header.OptionalHeader
	if first.PTS.Base != tsTimestampOffset || first.DTS == nil || first.DTS.Base != tsTimestampOffset-7200 {
		t.Errorf("first picture: PTS %d, DTS %v; want %d and %d",
			first.PTS.Base, first.DTS, tsTimestampOffset, tsTimestampOffset-7200)
	}
	if pcr := v[0].FirstPacket.AdaptationField.PCR.Base; pcr != tsTimestampOffset-7200-tsMuxDelay {
		t.Errorf("clock reference %d, want %d", pcr, tsTimestampOffset-7200-tsMuxDelay)
	}
	// The priming frame is presented 1920 ticks (21.333 ms) before the start,
	// and the first frame of the media proper at it, with the first picture.
	a := tsUnitsOfStream(t, buf.Bytes(), uint16(audio))
	for i, want := range []int64{tsTimestampOffset - 1920, tsTimestampOffset, tsTimestampOffset + 1920} {
		if got := a[i].PES.Header.OptionalHeader.PTS.Base; got != want {
			t.Errorf("audio unit %d is shown at %d, want %d", i, got, want)
		}
	}
}

// A track whose presentation starts further into its media than the clock runs
// ahead of the program cannot be stated, and the refusal does not decide where
// the next sample starts.
func TestTSMuxRefusesAStartItsClockCannotState(t *testing.T) {
	m := NewTSMuxer(io.Discard)
	cfg := tsAudioConfig()
	cfg.StartDelay = time.Second
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1}, Duration: 1920, Sync: true}); !errors.Is(err, ErrSample) {
		t.Errorf("a start delay of 1 s: err = %v, want ErrSample", err)
	}

	var buf bytes.Buffer
	m2 := NewTSMuxer(&buf)
	id2, err := m2.AddTrack(tsVideoConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	huge := Sample{Data: lengthPrefixed(tsIDR), Duration: 3600, CompositionOffset: int32(tsMuxDelay) + 1, Sync: true}
	if err := m2.WriteSample(id2, huge); !errors.Is(err, ErrSample) {
		t.Fatalf("a composition offset past the clock's lead: err = %v, want ErrSample", err)
	}
	if err := m2.WriteSample(id2, Sample{Data: lengthPrefixed(tsIDR), Duration: 3600, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tsUnitsOfStream(t, buf.Bytes(), uint16(id2))[0].PES.Header.OptionalHeader.PTS.Base; got != tsTimestampOffset {
		t.Errorf("after a refused first sample the next is shown at %d, want %d", got, tsTimestampOffset)
	}
}

func TestTSPrimingTicks(t *testing.T) {
	for _, c := range []struct {
		delay     time.Duration
		timescale uint32
		want      int64
	}{
		{0, 48000, 0},
		{-time.Millisecond, 48000, 0},
		{1024 * time.Second / 48000, 48000, 1024},
		{1024 * time.Second / 48000, 1000, 21}, // 21.333 rounds to nearest
	} {
		if got := tsPrimingTicks(c.delay, c.timescale); got != c.want {
			t.Errorf("tsPrimingTicks(%v, %d) = %d, want %d", c.delay, c.timescale, got, c.want)
		}
	}
}

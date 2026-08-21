// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
)

// TestVP9WebMToFragmentedMP4 walks the whole chain a caller actually has: a
// WebM holding VP9 is read, its configuration is derived from the bitstream —
// Matroska states neither profile nor level, so nothing else can supply them —
// and the result is written as a fragmented MP4 and read back.
//
// No single part of this package can be trusted to prove it: reading Matroska,
// deriving a vpcC and writing a vp09 sample entry were built separately, and
// the join is where a wrong answer would show up as an unplayable file rather
// than as an error.
func TestVP9WebMToFragmentedMP4(t *testing.T) {
	// A WebM written by this package, so the input is a real container and not
	// a hand-made byte string.
	var webm bytes.Buffer
	wm := NewWebMMuxer(&webm)
	in := []Sample{
		{Data: vp9KeyFrame, Duration: 3000, Sync: true},
		{Data: vp9InterFrame, Duration: 3000},
		{Data: vp9InterFrame, Duration: 3000},
	}
	id, err := wm.AddTrack(TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 320, Height: 180,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8},
	})
	if err != nil {
		t.Fatalf("declare the VP9 track: %v", err)
	}
	for i, s := range in {
		if err := wm.WriteSample(id, s); err != nil {
			t.Fatalf("write sample %d: %v", i, err)
		}
	}
	if err := wm.Close(); err != nil {
		t.Fatalf("finish the WebM: %v", err)
	}
	if Sniff(webm.Bytes()) != FormatMatroska {
		t.Fatalf("what was written is not Matroska")
	}

	// Read it back as samples. This is the step that did not exist before.
	r, err := NewReader(webm.Bytes())
	if err != nil {
		t.Fatalf("read the WebM: %v", err)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("track ids = %v, want one", ids)
	}
	cfg, err := r.TrackConfig(ids[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	samples, err := r.Samples(ids[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != len(in) {
		t.Fatalf("read %d samples, want %d", len(samples), len(in))
	}
	for i := range samples {
		if !bytes.Equal(samples[i].Data, in[i].Data) {
			t.Fatalf("sample %d came back changed", i)
		}
		if samples[i].Sync != in[i].Sync {
			t.Fatalf("sample %d sync = %v, want %v", i, samples[i].Sync, in[i].Sync)
		}
	}

	// Matroska states no VP9 profile and no level: they live in the frame
	// header. So the configuration read out of the container cannot describe
	// the track, and the muxer must say so rather than write a vpcC of zeroes.
	if cfg.VPx != nil && cfg.VPx.Level != 0 {
		t.Fatalf("the container stated a level it cannot know: %+v", cfg.VPx)
	}
	var refused bytes.Buffer
	if _, err := NewMuxer(&refused).AddTrack(cfg); err == nil {
		t.Fatal("a VP9 track with no level was described anyway")
	}

	// Derived from the samples themselves, it can.
	derived, err := ConfigFromSamples("vp09", samples, SampleTimescale(cfg.Timescale))
	if err != nil {
		t.Fatalf("derive the configuration from the bitstream: %v", err)
	}
	if derived.VPx == nil || derived.VPx.Level == 0 {
		t.Fatalf("no level was derived: %+v", derived.VPx)
	}
	if derived.Width != 320 || derived.Height != 180 {
		t.Fatalf("derived frame size = %dx%d, want 320x180", derived.Width, derived.Height)
	}

	var mp4out bytes.Buffer
	m := NewMuxer(&mp4out)
	outID, err := m.AddTrack(derived.TrackConfig)
	if err != nil {
		t.Fatalf("describe the derived track: %v", err)
	}
	for i, s := range samples {
		if err := m.WriteSample(outID, s); err != nil {
			t.Fatalf("write sample %d as MP4: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("finish the MP4: %v", err)
	}

	// And the MP4 holds the same pictures, described the way a player needs.
	back, err := NewReader(mp4out.Bytes())
	if err != nil {
		t.Fatalf("read the MP4 back: %v", err)
	}
	backCfg, err := back.TrackConfig(back.TrackIDs()[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if backCfg.Codec != "vp09" || backCfg.Width != 320 || backCfg.Height != 180 {
		t.Fatalf("the MP4 describes %+v", backCfg)
	}
	if backCfg.VPx == nil || *backCfg.VPx != *derived.VPx {
		t.Fatalf("vpcC came back as %+v, want %+v", backCfg.VPx, derived.VPx)
	}
	backSamples, err := back.Samples(back.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(backSamples) != len(in) {
		t.Fatalf("the MP4 holds %d samples, want %d", len(backSamples), len(in))
	}
	for i := range backSamples {
		if !bytes.Equal(backSamples[i].Data, in[i].Data) {
			t.Fatalf("sample %d changed on the way through", i)
		}
		if backSamples[i].Sync != in[i].Sync {
			t.Fatalf("sample %d sync = %v, want %v", i, backSamples[i].Sync, in[i].Sync)
		}
	}
}

// TestMP4ThroughWebMAndBack takes the repository's own MP4 fixture out to WebM
// and back, comparing what comes home with what left. Writing Matroska and
// reading it were built as separate pieces of work, so the only way to know the
// pair is lossless is to close the loop on real media: parameter sets, sample
// bytes, sync flags and durations all have to survive both crossings.
func TestMP4ThroughWebMAndBack(t *testing.T) {
	source, err := os.ReadFile("testdata/tiny.mp4")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	in, err := NewReader(source)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	ids := in.TrackIDs()
	if len(ids) == 0 {
		t.Fatal("the fixture holds no track")
	}

	// Out to WebM, every track the fixture holds that Matroska can carry.
	var webm bytes.Buffer
	wm := NewWebMMuxer(&webm)
	type original struct {
		cfg     TrackConfig
		samples []Sample
	}
	sent := map[uint32]original{}
	for _, id := range ids {
		cfg, err := in.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		samples, err := in.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		outID, err := wm.AddTrack(cfg)
		if err != nil {
			// A codec Matroska cannot carry is a refusal, not a surprise;
			// the tracks it can carry must still make the trip.
			t.Logf("track %d (%s) is not written to Matroska: %v", id, cfg.Codec, err)
			continue
		}
		for i, s := range samples {
			if err := wm.WriteSample(outID, s); err != nil {
				t.Fatalf("write sample %d of track %d: %v", i, id, err)
			}
		}
		sent[outID] = original{cfg: cfg, samples: samples}
	}
	if len(sent) == 0 {
		t.Fatal("nothing was written, so nothing below would be tested")
	}
	if err := wm.Close(); err != nil {
		t.Fatalf("finish the WebM: %v", err)
	}

	// And home again.
	back, err := NewReader(webm.Bytes())
	if err != nil {
		t.Fatalf("read the WebM back: %v", err)
	}
	got := back.TrackIDs()
	if len(got) != len(sent) {
		t.Fatalf("read %d tracks back, wrote %d", len(got), len(sent))
	}
	for _, id := range got {
		want, ok := sent[id]
		if !ok {
			t.Fatalf("track %d was not one of the tracks written", id)
		}
		cfg, err := back.TrackConfig(id)
		if err != nil {
			t.Fatalf("TrackConfig(%d): %v", id, err)
		}
		if cfg.Codec != want.cfg.Codec {
			t.Errorf("track %d came back as %q, was %q", id, cfg.Codec, want.cfg.Codec)
		}
		// The parameter sets are what a decoder cannot start without.
		if !sameNALUs(cfg.SPS, want.cfg.SPS) || !sameNALUs(cfg.PPS, want.cfg.PPS) ||
			!sameNALUs(cfg.VPS, want.cfg.VPS) {
			t.Fatalf("track %d lost or changed its parameter sets", id)
		}
		samples, err := back.Samples(id)
		if err != nil {
			t.Fatalf("Samples(%d): %v", id, err)
		}
		if len(samples) != len(want.samples) {
			t.Fatalf("track %d came back with %d samples, sent %d",
				id, len(samples), len(want.samples))
		}
		for i := range samples {
			if !bytes.Equal(samples[i].Data, want.samples[i].Data) {
				t.Fatalf("track %d sample %d changed on the way", id, i)
			}
			if samples[i].Sync != want.samples[i].Sync {
				t.Fatalf("track %d sample %d sync = %v, was %v",
					id, i, samples[i].Sync, want.samples[i].Sync)
			}
			// The last sample's duration is the one Matroska cannot state
			// per block, so it is the one worth checking apart.
			if i < len(samples)-1 && samples[i].Duration != want.samples[i].Duration {
				t.Fatalf("track %d sample %d lasts %d, was %d",
					id, i, samples[i].Duration, want.samples[i].Duration)
			}
		}
	}
}

// sameNALUs reports whether two lists of parameter sets hold the same bytes.
func sameNALUs(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestWebMStatesTheLastFrameLength covers the three ways stating a frame's
// length can go wrong at the tail of a track: a duration too large for the
// tick scale, a duration too small for it, and a frame with nothing before it
// to name as a reference.
func TestWebMStatesTheLastFrameLength(t *testing.T) {
	cfg := TrackConfig{
		Kind: Video, Codec: "vp09", Timescale: 90000, Width: 320, Height: 180,
		VPx: &VPxConfig{Profile: 0, Level: 10, BitDepth: 8},
	}

	t.Run("a frame ending beyond what the tick scale can state", func(t *testing.T) {
		// One tick per nanosecond and one track unit per second: a frame
		// starts within what a signed 64-bit tick count can hold and ends
		// past it. The start is what the block states, so the failure is the
		// end alone, and it must be reported rather than wrapped.
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf, TimestampScale(time.Nanosecond))
		seconds := cfg
		seconds.Timescale = 1
		id, err := m.AddTrack(seconds)
		if err != nil {
			t.Fatal(err)
		}
		var failure error
		for i := 0; i < 4 && failure == nil; i++ {
			failure = m.WriteSample(id, Sample{
				Data: vp9KeyFrame, Duration: math.MaxUint32, Sync: true,
			})
		}
		if !errors.Is(failure, ErrSample) {
			t.Fatalf("err = %v, want ErrSample", failure)
		}
	})

	t.Run("a frame starting beyond what the tick scale can state", func(t *testing.T) {
		// The same scale, but here it is the frame's own time that is out of
		// range: a composition offset pushes its presentation past the
		// ceiling while what came before it stayed inside.
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf, TimestampScale(time.Nanosecond))
		seconds := cfg
		seconds.Timescale = 1
		id, err := m.AddTrack(seconds)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := m.WriteSample(id, Sample{
				Data: vp9KeyFrame, Duration: math.MaxUint32, Sync: true,
			}); err != nil {
				t.Fatalf("the run-up must succeed, or the refusal below is not the one under test: %v", err)
			}
		}
		err = m.WriteSample(id, Sample{
			Data: vp9KeyFrame, Duration: 1, Sync: true, CompositionOffset: math.MaxInt32,
		})
		if !errors.Is(err, ErrSample) {
			t.Fatalf("err = %v, want ErrSample", err)
		}
	})

	t.Run("a duration smaller than one tick", func(t *testing.T) {
		// One-second ticks cannot state a thirtieth of a second, and a stated
		// zero would read as no duration at all.
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf, TimestampScale(time.Second), BufferedSegment())
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WriteSample(id, Sample{Data: vp9KeyFrame, Duration: 3000, Sync: true}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		groups := webmBlockGroupsOf(t, buf.Bytes())
		if len(groups) != 1 {
			t.Fatalf("block groups = %d, want one", len(groups))
		}
		if groups[0].BlockDuration != 1 {
			t.Fatalf("stated duration = %d, want the smallest the file can hold",
				groups[0].BlockDuration)
		}
	})

	t.Run("a lone frame that is not a keyframe", func(t *testing.T) {
		var buf bytes.Buffer
		m := NewWebMMuxer(&buf, BufferedSegment())
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Not a sync sample and nothing before it: the reference has to be
		// stated anyway, because its absence would mean the opposite.
		if err := m.WriteSample(id, Sample{Data: vp9InterFrame, Duration: 3000}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		groups := webmBlockGroupsOf(t, buf.Bytes())
		if len(groups) != 1 {
			t.Fatalf("block groups = %d, want one", len(groups))
		}
		if groups[0].ReferenceBlock == 0 {
			t.Fatal("a frame that is not a keyframe states no reference, so it reads as one")
		}
		// And the reader agrees it is not a place to start.
		r, err := NewReader(buf.Bytes())
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		samples, err := r.Samples(r.TrackIDs()[0])
		if err != nil {
			t.Fatalf("Samples: %v", err)
		}
		if len(samples) != 1 || samples[0].Sync {
			t.Fatalf("read back %+v", samples)
		}
	})
}

// webmBlockGroupsOf lists the block groups of a written file, read back with
// ebml-go's own declarations.
func webmBlockGroupsOf(t *testing.T, data []byte) []webm.BlockGroup {
	t.Helper()
	var f struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(bytes.NewReader(data), &f); err != nil {
		t.Fatalf("read back with ebml-go: %v", err)
	}
	var out []webm.BlockGroup
	for _, cluster := range f.Segment.Cluster {
		out = append(out, cluster.BlockGroup...)
	}
	return out
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// TestAFragmentWithNoSamplesForOneTrackRoundTrips.
//
// ⛔ This package wrote files it could not read back. Flush declared a track
// fragment for every track and filled only the ones holding samples, and the
// reader refused a track fragment with no sample run -- so any flush where one
// track had samples and another did not produced a file this package rejected.
// Two Matroska files in one library failed to remux for exactly that reason, and
// one MP4 in twenty failed to have its samples read.
//
// Both halves are fixed, and this asserts the pair: the writer declares only the
// tracks it is writing, and the reader treats a fragment with nothing for a track
// as nothing rather than as an error.
func TestAFragmentWithNoSamplesForOneTrackRoundTrips(t *testing.T) {
	sps, pps, w, h := avcParameterSets(t)
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: 90000, Width: w, Height: h,
		SPS: sps, PPS: pps,
	})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, Channels: 2, SampleRate: 48000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One fragment holding both tracks, then a fragment holding only video --
	// which is what a track that ends first, or runs at another rate, produces.
	videoData := [][]byte{{0x65, 1, 2}, {0x41, 3, 4}, {0x41, 5, 6}}
	audioData := [][]byte{{0xde, 1}, {0xad, 2}}
	if err := m.WriteSample(video, Sample{Data: videoData[0], Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(audio, Sample{Data: audioData[0], Duration: 1024, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{Data: videoData[1], Duration: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(audio, Sample{Data: audioData[1], Duration: 1024, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	// The audio has stopped; only video goes into the second fragment.
	if err := m.WriteSample(video, Sample{Data: videoData[2], Duration: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("the file this package wrote does not read: %v", err)
	}
	for _, c := range []struct {
		name string
		id   uint32
		want [][]byte
	}{{"video", video, videoData}, {"audio", audio, audioData}} {
		got, err := r.Samples(c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: read %d samples, wrote %d", c.name, len(got), len(c.want))
		}
		for i := range c.want {
			if !bytes.Equal(got[i].Data, c.want[i]) {
				t.Errorf("%s sample %d = %x, wrote %x", c.name, i, got[i].Data, c.want[i])
			}
		}
	}
}

// TestATrackWithNoSamplesAnywhereIsStillRefused: tolerating an empty fragment
// must not tolerate an empty track. A caller asking for a track's samples and
// getting none silently would decode nothing and say nothing.
func TestATrackWithNoSamplesAnywhereIsStillRefused(t *testing.T) {
	sps, pps, w, h := avcParameterSets(t)
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: 90000, Width: w, Height: h,
		SPS: sps, PPS: pps,
	})
	if err != nil {
		t.Fatal(err)
	}
	silent, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, Channels: 2, SampleRate: 48000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{Data: []byte{0x65, 1, 2}, Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(silent); !errors.Is(err, ErrNoSamples) {
		t.Errorf("a track that was never written read as %v, want ErrNoSamples", err)
	}
	if _, err := r.Samples(video); err != nil {
		t.Errorf("the track that was written: %v", err)
	}
}

// TestAFlushDeclaresOnlyTheTracksItIsWriting looks at the bytes, because the
// round-trip test cannot: with the reader tolerating an empty track fragment,
// writing one is harmless and invisible. Ablating either half on its own leaves
// that test green, so each half needs a witness of its own -- the reader's is
// TestTrafSamplesRejectsWhatItCannotRead, and this is the writer's.
func TestAFlushDeclaresOnlyTheTracksItIsWriting(t *testing.T) {
	sps, pps, w, h := avcParameterSets(t)
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	video, err := m.AddTrack(TrackConfig{
		Kind: Video, Codec: "avc1", Timescale: 90000, Width: w, Height: h,
		SPS: sps, PPS: pps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, Channels: 2, SampleRate: 48000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSample(video, Sample{Data: []byte{0x65, 1, 2}, Duration: 3000, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	fragments, trafs := 0, 0
	for _, seg := range parsed.Segments {
		for _, frag := range seg.Fragments {
			if frag.Moof == nil {
				continue
			}
			fragments++
			trafs += len(frag.Moof.Trafs)
			for _, traf := range frag.Moof.Trafs {
				if len(traf.Truns) == 0 {
					t.Errorf("a track fragment for track %d carries no sample run",
						traf.Tfhd.TrackID)
				}
			}
		}
	}
	if fragments == 0 {
		t.Fatal("no fragment was written, so this asserts nothing")
	}
	if trafs != fragments {
		t.Errorf("%d track fragments across %d fragments, want one each: the silent track was declared",
			trafs, fragments)
	}
}

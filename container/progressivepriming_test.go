// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"
	"time"
)

// What L10 found: an AAC track remuxed through the progressive muxer read back
// with no StartDelay, where the fragmented muxer's output read back 21.333 ms.
// The progressive muxer wrote no edit list at all, so the priming frame was
// presented and the track ran 1024 samples long (ffprobe duration_ts 97024
// against the source's 96000). Both forms of the copy, by hand and through
// JoinProgressive, now read the priming back.
func TestAProgressiveAACTrackReadsBackItsPriming(t *testing.T) {
	cfg := TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2,
		StartDelay: aacPriming,
	}
	write := func(t *testing.T) []byte {
		var out bytes.Buffer
		m := NewProgressiveMuxer(&out)
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if err := m.WriteSample(id, Sample{Data: []byte{byte(i), 1, 2}, Duration: 1024, Sync: true}); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	readBack := func(t *testing.T, data []byte) time.Duration {
		r, err := NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.TrackConfig(r.TrackIDs()[0])
		if err != nil {
			t.Fatal(err)
		}
		return got.StartDelay
	}
	for name, data := range map[string]func(*testing.T) []byte{
		"ProgressiveMuxer": write,
		"JoinProgressive": func(t *testing.T) []byte {
			src, err := NewReader(write(t))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := JoinProgressive(&out, []*Reader{src}); err != nil {
				t.Fatal(err)
			}
			return out.Bytes()
		},
		// The fragmented muxer is the reference the defect was measured
		// against, and must keep agreeing.
		"Muxer": func(t *testing.T) []byte {
			var out bytes.Buffer
			m := NewMuxer(&out)
			id, err := m.AddTrack(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.WriteSample(id, Sample{Data: []byte{1, 2}, Duration: 1024, Sync: true}); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			return out.Bytes()
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := readBack(t, data(t)); got != aacPriming {
				t.Errorf("StartDelay read back = %v; want the %v it was written with", got, aacPriming)
			}
		})
	}
}

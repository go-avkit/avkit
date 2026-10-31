// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// fragmentedMP3 is ffmpeg's MP3-in-MP4 fixture rewritten as a fragmented MP4 of
// several fragments, so a read that stopped at the first one is told apart from
// one that did not.
func fragmentedMP3(t *testing.T, src []byte) []byte {
	t.Helper()
	r, err := NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	copyInto(t, NewMuxer(&out, FragmentDuration(100*time.Millisecond)), r)
	return out.Bytes()
}

// What L11 found: since #61, TrackConfig named an MP3-in-MP4 track by reading
// the WHOLE track and looking at its first frame. From a reader left on disk
// (NewFileReader) that is the whole file: measured on ffmpeg's 60 s, 970 424-byte
// libmp3lame MP4, TrackConfig fetched 960 470 bytes in 2 298 reads. It now
// reads the first sample alone -- 417 bytes in one read there -- progressive or
// fragmented, and names the track the same. The samples are read first, so
// what is counted is TrackConfig's own reading and nothing of the file's
// opening.
func TestTrackConfigReadsOnlyTheFirstMPEGAudioFrame(t *testing.T) {
	progressive, err := os.ReadFile("testdata/ffmpeg-mp3.mp4")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"progressive": progressive,
		"fragmented":  fragmentedMP3(t, progressive),
	} {
		t.Run(name, func(t *testing.T) {
			src := &countingSource{src: bytes.NewReader(data)}
			r, err := NewFileReader(src, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			id := r.TrackIDs()[0]
			samples, err := r.Samples(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(samples) < 3 {
				t.Fatalf("the fixture holds %d samples; the test needs several", len(samples))
			}
			src.read = 0
			cfg, err := r.TrackConfig(id)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Codec != "mp3" {
				t.Errorf("codec %q, want mp3", cfg.Codec)
			}
			if want := int64(len(samples[0].Data)); src.read != want {
				t.Errorf("TrackConfig read %d bytes; want the first frame's %d", src.read, want)
			}
		})
	}
}

// The limit stops a progressive table and a fragmented track at the samples
// asked for, and asks for no more than the table holds.
func TestMP4SamplesStopsAtTheLimit(t *testing.T) {
	progressive, err := os.ReadFile("testdata/ffmpeg-mp3.mp4")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"progressive": progressive,
		"fragmented":  fragmentedMP3(t, progressive),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			trak := r.traks[r.TrackIDs()[0]]
			all, err := r.mp4Samples(trak, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{1, 2, len(all), len(all) + 5} {
				got, err := r.mp4Samples(trak, limit)
				if err != nil {
					t.Fatal(err)
				}
				if want := min(limit, len(all)); len(got) != want {
					t.Errorf("limit %d read %d samples, want %d", limit, len(got), want)
				}
			}
		})
	}
}

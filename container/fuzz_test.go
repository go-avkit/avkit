// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The fuzz targets below drive every entry point that reads bytes a caller
// did not write -- a downloaded file is the normal input of this package. What
// they assert is what a hostile file must not be able to do: panic, or make the
// reader allocate out of all proportion to the bytes it was handed. Returning
// an error is always an acceptable answer.
//
// Seeds are the fixtures in testdata and testdata/fuzzseed, one per shape the
// readers know: MP4 progressive and fragmented, Matroska, MPEG-TS with AVC and
// with HEVC. Run one with, for example,
//
//	go test -run='^$' -fuzz='^FuzzNewReader$' -fuzztime=60s ./container

// fuzzSeeds adds every fixture file to a fuzz target's corpus.
func fuzzSeeds(f *testing.F) {
	f.Helper()
	var paths []string
	for _, pattern := range []string{"testdata/*.mp4", "testdata/*.mkv", "testdata/fuzzseed/*"} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			f.Fatalf("glob %s: %v", pattern, err)
		}
		paths = append(paths, found...)
	}
	if len(paths) == 0 {
		f.Fatal("no seed found")
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatalf("read seed: %v", err)
		}
		f.Add(data)
	}
}

// allocBudget is how much a call may allocate for an input of n bytes: a
// fixed allowance for the muxers' own buffers, and a generous multiple of the
// input. A reader that sizes a table from a count field instead of from the
// bytes that hold it overshoots this by orders of magnitude, which is the
// defect it is here to catch.
func allocBudget(n int) uint64 { return 64<<20 + 1024*uint64(n) }

// withinBudget runs fn and fails if it allocated more than allocBudget allows
// for an input of n bytes.
func withinBudget(t *testing.T, n int, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > allocBudget(n) {
		t.Fatalf("%d input bytes allocated %d bytes, over the budget of %d", n, got, allocBudget(n))
	}
}

func FuzzSniff(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = Sniff(data)
	})
}

func FuzzDemux(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() {
			file, err := Demux(data)
			if err == nil {
				_ = file.DurationSeconds()
				_ = file.VideoTracks()
				_ = file.AudioTracks()
			}
		})
	})
}

// readAll asks a reader for everything it can say about every track, which is
// where the sample tables are actually walked.
func readAll(r *Reader) {
	_ = r.File()
	_ = r.Truncated()
	for _, id := range r.TrackIDs() {
		cfg, cfgErr := r.TrackConfig(id)
		samples, err := r.Samples(id)
		if err != nil || cfgErr != nil {
			continue
		}
		_, _ = ConfigFromSamples(cfg.Codec, samples)
		_, _ = CheckHVC1(cfg, samples)
	}
}

func FuzzNewReader(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() {
			r, err := NewReader(data)
			if err != nil {
				return
			}
			readAll(r)
		})
	})
}

func FuzzNewFileReader(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() {
			_, _ = HoldsFragments(bytes.NewReader(data))
			r, err := NewFileReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				return
			}
			readAll(r)
		})
	})
}

func FuzzRemux(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() {
			r, err := NewReader(data)
			if err != nil {
				return
			}
			_ = Remux(io.Discard, r, AllowTruncated())
			_ = Remux(io.Discard, r, AllowTruncated(), ConformHVC1())
			_ = JoinProgressive(io.Discard, []*Reader{r, r}, AllowTruncated())
		})
	})
}

// FuzzNewSegmentedReader cuts the input into segments -- at a point the first
// byte chooses, so a transport stream is split mid-packet as well as between
// packets -- and reads them as one stream, the way a downloader hands over the
// segments of a playlist.
func FuzzNewSegmentedReader(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		cut := 1 + int(data[0])*len(data)/256
		parts := [][]byte{data[1:cut], data[cut:], data[1:]}
		withinBudget(t, len(data), func() {
			r, err := NewSegmentedReader(parts)
			if err != nil {
				return
			}
			readAll(r)
		})
	})
}

// FuzzConfigFromSamples hands the configuration reader samples cut from fuzzed
// bytes, under every codec name it knows: this is where the codec headers in a
// sample -- parameter sets, sequence headers, sync frames -- are parsed.
func FuzzConfigFromSamples(f *testing.F) {
	codecs := []string{"avc1", "avc3", "hvc1", "hev1", "av01", "vp09", "mp4a", "vp08", "opus", "ac-3", "ec-3"}
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		codec := codecs[int(data[0])%len(codecs)]
		// The second byte says how many samples to cut the rest into, so that
		// a reader that looks at several samples is reached as well as one
		// that looks at the first.
		parts := int(data[1])%4 + 1
		rest := data[2:]
		samples := make([]Sample, 0, parts)
		step := len(rest)/parts + 1
		for len(rest) > 0 {
			n := min(step, len(rest))
			samples = append(samples, Sample{Data: rest[:n], Sync: true})
			rest = rest[n:]
		}
		withinBudget(t, len(data), func() {
			_, _ = ConfigFromSamples(codec, samples)
			_, _ = CheckHVC1(TrackConfig{Codec: "hev1"}, samples)
		})
	})
}

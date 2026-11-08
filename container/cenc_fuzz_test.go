// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"os"
	"path/filepath"
	"testing"
)

// The decryption path reads protection boxes (sinf, tenc, senc, saiz, saio, pssh)
// and subsample maps from a file nobody vouches for. These targets hold it to
// what fuzz_test.go holds every reader to: no panic, no allocation out of
// proportion to the input. The seeds are the encrypted files of testdata/cenc
// (cenc and cbcs, video and audio, ours and ffmpeg's), whole and cut into their
// initialisation segment and media segments.
//
//	go test -run='^$' -fuzz='^FuzzDecryptFile$' -fuzztime=60s ./container
//	go test -run='^$' -fuzz='^FuzzDecryptSegment$' -fuzztime=60s ./container

// splitFragmented cuts a fragmented MP4 into its initialisation segment and
// media segments (a moof and what follows up to the next moof).
func splitFragmented(b []byte) ([]byte, [][]byte) {
	var cuts []int
	for _, bx := range segmentBoxes(b) {
		if bx.typ == "moof" {
			cuts = append(cuts, bx.start)
		}
	}
	if len(cuts) == 0 {
		return b, nil
	}
	init, segs := b[:cuts[0]], [][]byte{}
	for i, c := range cuts {
		end := len(b)
		if i+1 < len(cuts) {
			end = cuts[i+1]
		}
		segs = append(segs, b[c:end])
	}
	return init, segs
}

func cencSeedFiles(tb testing.TB) [][]byte {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "cenc", "*.mp4"))
	if err != nil || len(paths) == 0 {
		tb.Fatalf("no cenc seeds: %v", err)
	}
	var out [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func FuzzDecryptFile(f *testing.F) {
	for _, b := range cencSeedFiles(f) {
		f.Add(b)
	}
	key := cencKey(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		withinBudget(t, len(data), func() { _, _ = DecryptFile(data, key) })
	})
}

// FuzzDecryptSegment keeps a valid initialisation segment and fuzzes the media
// segment (its senc, saiz, saio and the subsample counts), then the other way
// round.
func FuzzDecryptSegment(f *testing.F) {
	for _, b := range cencSeedFiles(f) {
		init, segs := splitFragmented(b)
		if len(segs) > 0 {
			f.Add(init, segs[0])
		}
	}
	key := cencKey(f)
	f.Fuzz(func(t *testing.T, init, seg []byte) {
		withinBudget(t, len(init)+len(seg), func() { _, _, _ = Decrypt(init, [][]byte{seg}, key) })
	})
}

func FuzzEncryptSegment(f *testing.F) {
	clear, err := DecryptFile(cencData(f, "cenc-video1.mp4"), cencKey(f))
	if err != nil {
		f.Fatal(err)
	}
	init, segs := splitFragmented(clear)
	if len(segs) == 0 {
		f.Fatal("no seed segment")
	}
	f.Add(segs[0])
	enc := cencConfig(f, SchemeCBCS)
	f.Fuzz(func(t *testing.T, seg []byte) {
		withinBudget(t, len(seg), func() { _, _, _ = Encrypt(init, [][]byte{seg}, enc) })
	})
}

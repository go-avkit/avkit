// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"runtime"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// withUniformSampleSize rewrites the first track of a progressive MP4 so that
// its sample size box states one size for every sample and a count of n -- the
// 20-byte form of stsz, which carries no table and so costs nothing however
// large the count it states.
func withUniformSampleSize(t *testing.T, data []byte, size, n uint32) []byte {
	t.Helper()
	parsed, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stbl := parsed.Moov.Traks[0].Mdia.Minf.Stbl
	stsz := &mp4.StszBox{SampleUniformSize: size, SampleNumber: n}
	for i, child := range stbl.Children {
		if child.Type() == "stsz" {
			stbl.Children[i] = stsz
		}
	}
	stbl.Stsz = stsz
	var buf bytes.Buffer
	if err := parsed.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// ⛔ A sample count is a claim, and the reader used to size its sample list
// from it before checking it against anything. With a uniform sample size the
// count costs the file nothing -- stsz is twenty bytes whatever it says -- so a
// file of under a kilobyte stating 0xFFFFFFFF samples made Samples reserve
// 4294967295 entries of a Sample each: 160 GiB, from one field. Measured
// before the fix with 1<<26: 2560 MiB allocated for a 722-byte file.
//
// What the count must agree with is the file: every sample is at least the
// uniform size, so the samples together cannot be longer than the bytes there
// are. A count that says otherwise is refused before anything is sized from it.
func TestUniformSampleCountIsBoundedByTheFile(t *testing.T) {
	for _, n := range []uint32{1 << 26, 0xFFFFFFFF} {
		data := withUniformSampleSize(t, progressiveSample(t), 1, n)
		for name, open := range map[string]func() (*Reader, error){
			"memory": func() (*Reader, error) { return NewReader(data) },
			"file": func() (*Reader, error) {
				return NewFileReader(bytes.NewReader(data), int64(len(data)))
			},
		} {
			r, err := open()
			if err != nil {
				t.Fatalf("%s: open: %v", name, err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err = r.Samples(r.TrackIDs()[0])
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrSampleData) {
				t.Errorf("%s: %d samples of one byte in a %d-byte file: err = %v",
					name, n, len(data), err)
			}
			if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
				t.Errorf("%s: a count of %d allocated %d bytes for a %d-byte file",
					name, n, got, len(data))
			}
		}
	}
}

// The bound is the file and nothing narrower: a uniform-size track whose
// samples do fit still reads, every sample of it.
func TestUniformSampleCountThatFitsStillReads(t *testing.T) {
	data := withUniformSampleSize(t, progressiveSample(t), 1, 12)
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 12 {
		t.Fatalf("%d samples, want 12", len(samples))
	}
	for i, s := range samples {
		if len(s.Data) != 1 {
			t.Fatalf("sample %d holds %d bytes, want the uniform 1", i+1, len(s.Data))
		}
	}
}

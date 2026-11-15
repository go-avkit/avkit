// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// The boxes inside a sample are bytes of an mdat, which the walk of a file does
// not enter: a wvtt sample, or a configuration record handed to the muxer, holds
// boxes mp4ff decodes into Go values, and they are counted first.

// wvttFlood is a wvtt sample of n empty boxes, at the top or inside a vttc.
func wvttFlood(n int, inVttc bool) []byte {
	flood := bytes.Repeat(box("free"), n)
	if inVttc {
		return box("vttc", flood)
	}
	return flood
}

// TestSampleBombsAreRefusedBeforeTheyAreDecoded: every path that decodes the
// boxes of a sample refuses a flood and a nesting with ErrReaderLimit, with a
// small allocation, under a memory limit the decode would not fit in.
func TestSampleBombsAreRefusedBeforeTheyAreDecoded(t *testing.T) {
	paths := map[string]func([]byte) error{
		"readWebVTTSample": func(b []byte) error { _, err := readWebVTTSample(b); return err },
		"tx3gSampleOfWebVTT": func(b []byte) error {
			_, err := tx3gSampleOfWebVTT(b)
			return err
		},
		"refuseTimedTextShift (remux, cut)": func(b []byte) error {
			return refuseTimedTextShift(sourceTrack{cfg: TrackConfig{Codec: "wvtt"}}, []Sample{{Data: b}}, ErrTimeRange, "cutting it")
		},
		"cutText (Segment)": func(b []byte) error {
			_, err := cutText(&segSource{cfg: TrackConfig{Codec: "wvtt", Timescale: 1000}, samples: []Sample{{Data: b, Duration: 1}}, starts: []uint64{0}}, nil, refCuts{})
			return err
		},
		"decodeConfigBox (stpp)": func(b []byte) error {
			// The record is the content of an stpp box: three strings, then children.
			_, err := decodeConfigBox("stpp", append(make([]byte, 8+3), b...), mp4.DecodeStpp)
			return err
		},
	}
	bombs := map[string][]byte{
		"flood":         wvttFlood(DefaultMaxBoxes+1, false),
		"flood in vttc": wvttFlood(DefaultMaxBoxes+1, true),
		"nesting":       nestedWithoutFtyp("vttc", DefaultMaxBoxDepth+1),
	}
	old := debug.SetMemoryLimit(512 << 20)
	defer debug.SetMemoryLimit(old)
	for pname, call := range paths {
		for bname, data := range bombs {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			err := call(data)
			runtime.ReadMemStats(&after)
			got := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s / %s: %d input bytes, %d bytes allocated, %v", pname, bname, len(data), got, err)
			if !errors.Is(err, ErrReaderLimit) {
				t.Errorf("%s / %s = %v, want ErrReaderLimit", pname, bname, err)
			}
			// The stpp path is handed a copy by the test and makes one to walk the
			// record: two copies of the input, against 33 bytes per input byte decoded.
			if got > 3*uint64(len(data))+1<<20 {
				t.Errorf("%s / %s: allocated %d bytes for %d input bytes", pname, bname, got, len(data))
			}
		}
	}
}

// nestedWithoutFtyp is depth boxes of the given type inside one another.
func nestedWithoutFtyp(typ string, depth int) []byte {
	return nested(typ, depth)[len(mp4Ftyp()):]
}

// TestSamplesWithinTheBoundsStillDecode: a sample of a few boxes is read as before.
func TestSamplesWithinTheBoundsStillDecode(t *testing.T) {
	cues, err := readWebVTTSample(box("vttc", box("payl", []byte("hello"))))
	if err != nil || len(cues) != 1 || cues[0].text != "hello" {
		t.Fatalf("%v %v", cues, err)
	}
}

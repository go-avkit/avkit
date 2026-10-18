// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"testing"

	"github.com/asticode/go-astits"
)

// What L10 found: the transport stream reader timed every unit by its DTS and
// never read the PTS back, so a track with B-frames came out of a remux in
// decoding order as if it were presented in it -- ffmpeg decoded 49 pictures of
// 50 from such a copy, at the wrong times.
//
// The fixture is ffmpeg 9.0.2's own: ten 64x64 frames of libx264 -bf 2 -g 10 in
// MPEG-TS. Its PTS and DTS, as ffprobe -show_entries packet=pts,dts lists
// them, are the oracle; the reader must hand over their difference as each
// sample's composition offset.
func TestTheTransportStreamReaderReadsThePresentationTimesBack(t *testing.T) {
	data, err := os.ReadFile("testdata/ffmpeg-bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	pts := []int64{133200, 144000, 136800, 140400, 151200, 147600, 158400, 154800, 165600, 162000}
	dts := []int64{126000, 129600, 133200, 136800, 140400, 144000, 147600, 151200, 154800, 158400}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != len(pts) {
		t.Fatalf("%d samples, want %d", len(samples), len(pts))
	}
	for i, s := range samples {
		if want := int32(pts[i] - dts[i]); s.CompositionOffset != want {
			t.Errorf("sample %d: composition offset %d, want PTS-DTS = %d", i, s.CompositionOffset, want)
		}
	}
	// Copied into an MP4 and read back, the pictures present in the order the
	// source presents them.
	var out bytes.Buffer
	if err := JoinProgressive(&out, []*Reader{r}); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	copied, err := back.Samples(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	var decode int64
	for i, s := range copied {
		if got, want := decode+int64(s.CompositionOffset), pts[i]-dts[0]; got != want {
			t.Errorf("copied sample %d is presented at %d, the source presents it at %d", i, got, want)
		}
		decode += int64(s.Duration)
	}
}

// pesCompositionOffset is PTS less DTS, modulo the 33-bit clock, and nothing
// for a unit that states no DTS or whose PTS comes before it.
func TestPESCompositionOffset(t *testing.T) {
	with := func(pts, dts *int64) *astits.PESData {
		h := &astits.PESOptionalHeader{}
		if pts != nil {
			h.PTS = &astits.ClockReference{Base: *pts}
		}
		if dts != nil {
			h.DTS = &astits.ClockReference{Base: *dts}
		}
		return &astits.PESData{Header: &astits.PESHeader{OptionalHeader: h}}
	}
	at := func(v int64) *int64 { return &v }
	for name, c := range map[string]struct {
		pes  *astits.PESData
		want int32
	}{
		"no header":             {&astits.PESData{}, 0},
		"no optional header":    {&astits.PESData{Header: &astits.PESHeader{}}, 0},
		"a PTS alone":           {with(at(9000), nil), 0},
		"a DTS alone":           {with(nil, at(9000)), 0},
		"two frames of delay":   {with(at(16200), at(9000)), 7200},
		"the same time":         {with(at(9000), at(9000)), 0},
		"a PTS that wrapped":    {with(at(100), at(ptsWrap-3500)), 3600},
		"a PTS before its DTS":  {with(at(9000), at(16200)), 0},
		"more than 32 bits say": {with(at(1<<31+10), at(0)), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pesCompositionOffset(c.pes); got != c.want {
				t.Errorf("pesCompositionOffset = %d, want %d", got, c.want)
			}
		})
	}
}

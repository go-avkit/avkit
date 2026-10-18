// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// readFixtureConfig reads a fixture's first track configuration.
func readFixtureConfig(t *testing.T, name string) (*Reader, TrackConfig) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	return r, cfg
}

// What L11 found: the Matroska reader did not read DiscardPadding, so a
// Matroska track whose end is padding went to MP4 presenting the padding.
// testdata/ffmpeg-mp3-padding.mkv is ffmpeg 9.0.2's -c copy of sine.mp3: 13
// blocks, the last stating no duration and a DiscardPadding of 14535147 ns, so
// ffmpeg presents 14335 of the 14976 samples it decodes. Its MP4 copy decoded
// 14976 before; with the end of the edit list now at 324 ms it decodes 14309,
// and ffmpeg's own copy 14377 -- the Matroska millisecond is the precision.
func TestMatroskaDiscardPaddingEndsTheEditList(t *testing.T) {
	r, cfg := readFixtureConfig(t, "testdata/ffmpeg-mp3-padding.mkv")
	el := cfg.EditList
	// The last block, at 313 ms, decodes for the 26 ms of the frame before it;
	// 14.535 ms of padding round to 15 ticks.
	if el == nil || el.MovieTimescale != 1000 || len(el.Edits) != 1 ||
		el.Edits[0] != (Edit{SegmentDuration: 313 + 26 - 15, MediaTime: 0, Rate: NormalRate}) {
		t.Fatalf("edit list %+v; want 324 ms from 0", el)
	}
	var out bytes.Buffer
	copyInto(t, NewProgressiveMuxer(&out), r)
	parsed, err := mp4.DecodeFile(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	entries := parsed.Moov.Traks[0].Edts.Elst[0].Entries
	if movie := parsed.Moov.Mvhd.Timescale; len(entries) != 1 ||
		entries[0].SegmentDuration != scaleDurationUp(324, 1000, movie) || entries[0].MediaTime != 0 {
		t.Errorf("MP4 edit list %+v; want 324 ms at 0", entries)
	}
}

// A last block that states only what it presents, beside the padding that
// makes up the rest of its frame -- how ffmpeg writes what its own encoders
// trim -- already ends where the padding begins: no edit list is stated.
// testdata/ffmpeg-mp3.mkv's last block lasts 18 ms with 7.8 ms of padding, a
// 26 ms frame.
func TestAPaddingTheBlockDurationAlreadyLeavesOutStatesNoEdit(t *testing.T) {
	_, cfg := readFixtureConfig(t, "testdata/ffmpeg-mp3.mkv")
	if cfg.EditList != nil {
		t.Errorf("edit list %+v; the samples already end where the padding begins", cfg.EditList)
	}
}

// WebMMuxer's own trim reads back: an edit ending 50 ms into four 20 ms Opus
// frames is written as 10 ms of padding on the third block and 20 ms on the
// last (TestAnEditEndingEarlyIsWrittenAsDiscardPadding), and read as the edit
// it was.
func TestWebMMuxerDiscardPaddingReadsBackAsTheEdit(t *testing.T) {
	cfg := TrackConfig{
		Kind: Audio, Codec: "Opus", Timescale: 48000, SampleRate: 48000, Channels: 2,
		EditList: &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 50, MediaTime: 0, Rate: NormalRate}}},
	}
	var out bytes.Buffer
	m := NewWebMMuxer(&out)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := m.WriteSample(id, Sample{Data: []byte{byte(i), 1}, Duration: 960, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if el := got.EditList; el == nil || el.Edits[0] != (Edit{SegmentDuration: 50, MediaTime: 0, Rate: NormalRate}) {
		t.Errorf("read back %+v; want 50 ms from 0", el)
	}
}

// mkvEndTrim reads the run of padded blocks at the end of the track, and only
// that run.
func TestMkvEndTrim(t *testing.T) {
	audio := TrackConfig{Kind: Audio, Timescale: 1000}
	frames := func(n int) [][]byte { return make([][]byte, n) }
	samples := func(durations ...uint32) []Sample {
		out := make([]Sample, len(durations))
		for i, d := range durations {
			out[i] = Sample{Duration: d}
		}
		return out
	}
	const ms = int64(time.Millisecond)
	for name, tc := range map[string]struct {
		cfg     TrackConfig
		blocks  []mkvBlock
		samples []Sample
		want    *Edit
	}{
		"no padding": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1)}}, samples(20, 20), nil},
		"the last block's": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: 5 * ms}},
			samples(20, 20), &Edit{SegmentDuration: 35, Rate: NormalRate}},
		// A short last block decodes for the length of the one before.
		"a short last block": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: 12 * ms}},
			samples(20, 10), &Edit{SegmentDuration: 28, Rate: NormalRate}},
		// A laced block's padding is at the end of its last frame.
		"a laced block": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(2), discard: 4 * ms}},
			samples(20, 20, 20), &Edit{SegmentDuration: 56, Rate: NormalRate}},
		// The earliest end of the run is where it stops.
		"a run of two": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: 10 * ms}, {frames: frames(1), discard: 20 * ms}},
			samples(20, 20, 20), &Edit{SegmentDuration: 30, Rate: NormalRate}},
		// A padding inside the track is not an end.
		"a padding inside": {audio, []mkvBlock{{frames: frames(1), discard: 10 * ms}, {frames: frames(1)}}, samples(20, 20), nil},
		// A negative padding is at the start of its block.
		"a negative padding": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: -5 * ms}}, samples(20, 20), nil},
		// Half a tick trims nothing; a little more trims one.
		"half a tick": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: ms / 2}}, samples(20, 20), nil},
		"past half a tick": {audio, []mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: ms/2 + 1}},
			samples(20, 20), &Edit{SegmentDuration: 39, Rate: NormalRate}},
		// One block on its own decodes for what it states.
		"a single block": {audio, []mkvBlock{{frames: frames(1), discard: 5 * ms}}, samples(20),
			&Edit{SegmentDuration: 15, Rate: NormalRate}},
		// The presentation starts after the priming, and is measured from it.
		"after a priming": {TrackConfig{Kind: Audio, Timescale: 1000, StartDelay: 6 * time.Millisecond},
			[]mkvBlock{{frames: frames(1)}, {frames: frames(1), discard: 5 * ms}}, samples(20, 20),
			&Edit{SegmentDuration: 29, MediaTime: 6, Rate: NormalRate}},
		// Padding over the whole presentation leaves nothing to state.
		"nothing presented": {audio, []mkvBlock{{frames: frames(1), discard: 20 * ms}}, samples(20), nil},
	} {
		t.Run(name, func(t *testing.T) {
			el := mkvEndTrim(tc.cfg, tc.blocks, tc.samples, ms)
			switch {
			case tc.want == nil && el != nil:
				t.Errorf("edit list %+v; want none", el)
			case tc.want != nil && (el == nil || el.MovieTimescale != 1000 || len(el.Edits) != 1 || el.Edits[0] != *tc.want):
				t.Errorf("edit list %+v; want %+v", el, *tc.want)
			}
		})
	}
}

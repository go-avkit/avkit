// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// aacPriming is one AAC frame at 48 kHz: 1024 samples, about 21 ms. It is what
// an AAC encoder emits before the media proper, and what #34 reported arriving
// in the output as an extra frame.
const aacPriming = 1024 * time.Second / 48000

// mkvWithCodecDelay writes a Matroska holding one AAC track that states a
// priming. Our own WebM muxer writes CodecDelay only for Opus, so the document
// is built directly, which is also how the other Matroska tests make a fixture
// they could not otherwise produce.
func mkvWithCodecDelay(t *testing.T, delay time.Duration) []byte {
	t.Helper()
	return marshalDoc(t, &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_AAC",
				CodecDelay: uint64(delay),
				Audio:      &mkvAudio{SamplingFrequency: 48000, Channels: 2},
				// AAC-LC, 48 kHz, stereo.
				CodecPrivate: []byte{0x11, 0x90},
			}}},
			Cluster: []mkvCluster{{Timecode: 0, SimpleBlock: []ebml.Block{
				simple(1, 0, true, []byte("frame-one")),
				simple(1, 21, true, []byte("frame-two")),
			}}},
		},
	})
}

// The reader states what the container states, in the container's own words.
func TestMatroskaCodecDelayBecomesAStartDelay(t *testing.T) {
	r, err := NewReader(mkvWithCodecDelay(t, aacPriming))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartDelay != aacPriming {
		t.Errorf("start delay = %v; want %v", cfg.StartDelay, aacPriming)
	}
	// A file that states none says none, rather than a zero that means "unread".
	r2, err := NewReader(mkvWithCodecDelay(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := r2.TrackConfig(r2.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.StartDelay != 0 {
		t.Errorf("a file stating no delay gave %v", cfg2.StartDelay)
	}
}

// What #34 asked for: the delay survives the remux instead of becoming an extra
// frame at the head of the output.
func TestRemuxCarriesTheStartDelayIntoAnEditList(t *testing.T) {
	r, err := NewReader(mkvWithCodecDelay(t, aacPriming))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		t.Fatalf("remux: %v", err)
	}

	// Read as this package reads it: the delay comes back.
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := back.TrackConfig(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	// The output track counts in milliseconds -- Matroska's default timecode
	// scale, carried over -- so 21.333 ms is stated as 21. That is the
	// precision the timescale allows, and it is the difference between losing
	// a third of a millisecond and gaining a whole 21.333 ms frame.
	if want := 21 * time.Millisecond; cfg.StartDelay != want {
		t.Errorf("after the remux the delay is %v; want %v", cfg.StartDelay, want)
	}

	// And in the file itself, so a player that never uses this package sees it.
	parsed, err := mp4.DecodeFile(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	trak := parsed.Moov.Traks[0]
	if trak.Edts == nil || len(trak.Edts.Elst) != 1 {
		t.Fatal("the output carries no edit list")
	}
	entries := trak.Edts.Elst[0].Entries
	if len(entries) != 1 {
		t.Fatalf("edit list has %d entries; want one", len(entries))
	}
	// 1024 samples at a 48 kHz timescale.
	if entries[0].MediaTime != 21 {
		t.Errorf("media time = %d; want 21, the priming in the track's own timescale",
			entries[0].MediaTime)
	}
	if entries[0].SegmentDuration != 0 {
		t.Errorf("segment duration = %d; want 0, which means to the end",
			entries[0].SegmentDuration)
	}
}

// The other half #34 named: an MP4 source with an edit list of its own must not
// lose it either.
func TestAnMP4EditListSurvivesARemux(t *testing.T) {
	first := mkvWithCodecDelay(t, aacPriming)
	r, err := NewReader(first)
	if err != nil {
		t.Fatal(err)
	}
	var asMP4 bytes.Buffer
	if err := Remux(&asMP4, r); err != nil {
		t.Fatal(err)
	}
	// Now remux the MP4 again: the delay has to come from the edit list this
	// time, not from a CodecDelay.
	r2, err := NewReader(asMP4.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := Remux(&again, r2); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(again.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := back.TrackConfig(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := 21 * time.Millisecond; cfg.StartDelay != want {
		t.Errorf("after a second remux the delay is %v; want %v", cfg.StartDelay, want)
	}
}

// Only the one shape is read. An edit list saying something this package does
// not reproduce must report nothing rather than a delay the output will not have.
func TestOnlyAPrimingEditListIsRead(t *testing.T) {
	trakWith := func(entries ...mp4.ElstEntry) *mp4.TrakBox {
		trak := &mp4.TrakBox{}
		elst := &mp4.ElstBox{Version: 1, Entries: entries}
		// Elst is what a decoded file carries, so that is what a fixture
		// standing in for one must set: AddChild alone fills Children only.
		edts := &mp4.EdtsBox{Elst: []*mp4.ElstBox{elst}}
		edts.AddChild(elst)
		trak.AddChild(edts)
		return trak
	}
	one := mp4.ElstEntry{MediaTime: 1024, MediaRateInteger: 1}
	for _, c := range []struct {
		name string
		trak *mp4.TrakBox
		want time.Duration
	}{
		{"priming", trakWith(one), 1024 * time.Second / 48000},
		{"no edit list at all", &mp4.TrakBox{}, 0},
		{"an empty edit, which is a gap and not a priming",
			trakWith(mp4.ElstEntry{MediaTime: -1, MediaRateInteger: 1}), 0},
		{"two entries describe a presentation this does not reproduce",
			trakWith(one, one), 0},
		{"a rate other than 1 is not a plain skip",
			trakWith(mp4.ElstEntry{MediaTime: 1024, MediaRateInteger: 2}), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := mp4StartDelay(c.trak, 48000); got != c.want {
				t.Errorf("mp4StartDelay = %v; want %v", got, c.want)
			}
		})
	}
	if got := mp4StartDelay(trakWith(one), 0); got != 0 {
		t.Errorf("without a timescale the delay cannot be stated, got %v", got)
	}
	if got := mp4StartDelay(nil, 48000); got != 0 {
		t.Errorf("a nil trak gave %v", got)
	}
}

// A delay shorter than one tick of the track's own timescale cannot be written,
// and rounding it up would move the track further than the delay it describes.
func TestADelayShorterThanATickIsNotWritten(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf)
	if _, err := m.AddTrack(TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1000, SampleRate: 48000, Channels: 2,
		StartDelay: 100 * time.Microsecond, // a tenth of a tick at 1 kHz
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if trak := parsed.Moov.Traks[0]; trak.Edts != nil {
		t.Error("an edit list was written for a delay the timescale cannot express")
	}
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/at-wat/ebml-go"
)

// trimFile is a written file read back with ebml-go, DiscardPadding included,
// which ebml-go's own webm.BlockGroup does not declare.
type trimFile struct {
	Segment struct {
		Cluster []struct {
			Timecode    uint64       `ebml:"Timecode"`
			SimpleBlock []ebml.Block `ebml:"SimpleBlock"`
			BlockGroup  []struct {
				Block          ebml.Block `ebml:"Block"`
				BlockDuration  uint64     `ebml:"BlockDuration,omitempty"`
				DiscardPadding int64      `ebml:"DiscardPadding,omitempty"`
			} `ebml:"BlockGroup"`
		} `ebml:"Cluster"`
	} `ebml:"Segment"`
}

// trimmedBlock is one block read back: when it is shown, in ticks, what it
// states as its duration and padding, and whether it is a block group.
type trimmedBlock struct {
	tick     int64
	duration uint64
	discard  int64
	group    bool
}

// writeTrimmed writes one track through WebMMuxer and reads its blocks back in
// presentation order, checking on the way that this package's own reader still
// takes the file whole.
func writeTrimmed(t *testing.T, cfg TrackConfig, durations ...uint32) []trimmedBlock {
	t.Helper()
	var out bytes.Buffer
	m := NewWebMMuxer(&out)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range durations {
		if err := m.WriteSample(id, Sample{Data: []byte{byte(i), 1}, Duration: d, Sync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatalf("the file does not read back: %v", err)
	}
	if s, err := r.Samples(r.TrackIDs()[0]); err != nil || len(s) != len(durations) {
		t.Fatalf("read back %d samples (%v), want %d", len(s), err, len(durations))
	}
	var f trimFile
	if err := ebml.Unmarshal(bytes.NewReader(out.Bytes()), &f); err != nil {
		t.Fatal(err)
	}
	var blocks []trimmedBlock
	for _, c := range f.Segment.Cluster {
		for _, b := range c.SimpleBlock {
			blocks = append(blocks, trimmedBlock{tick: int64(c.Timecode) + int64(b.Timecode)})
		}
		for _, g := range c.BlockGroup {
			blocks = append(blocks, trimmedBlock{tick: int64(c.Timecode) + int64(g.Block.Timecode),
				duration: g.BlockDuration, discard: g.DiscardPadding, group: true})
		}
	}
	return blocks
}

// What L10 found: an MP4 to Matroska copy of ffmpeg's AAC decoded 128 samples
// more than the source. ffmpeg's MP4 states the padding its encoder leaves after
// the last sample as a last sample SHORTER than the frame it decodes to (stts
// 768 for a 1024-sample frame), and its mov.c discards the rest. Matroska says
// the same as DiscardPadding on the last block, which is what ffmpeg's own
// copy writes: 256 samples at 48 kHz, 5333333 ns, on a block lasting 16 ms.
func TestAShortLastAACFrameIsWrittenAsDiscardPadding(t *testing.T) {
	blocks := writeTrimmed(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000, Channels: 2,
	}, 1024, 1024, 768)
	last := blocks[len(blocks)-1]
	if !last.group || last.discard != 5333333 || last.duration != 16 {
		t.Errorf("last block %+v; want a group lasting 16 ms with a padding of 5333333 ns", last)
	}
	for _, b := range blocks[:len(blocks)-1] {
		if b.group {
			t.Errorf("block %+v is a group; only the last frame is trimmed", b)
		}
	}
}

// An edit that ends before the media does trims too, across as many frames as
// it reaches into: here 50 ms of 80, so the third Opus frame loses half of
// itself and the fourth all of itself. A frame trimmed that is not the last is
// a block group with a padding and no duration.
func TestAnEditEndingEarlyIsWrittenAsDiscardPadding(t *testing.T) {
	blocks := writeTrimmed(t, TrackConfig{
		Kind: Audio, Codec: "Opus", Timescale: 48000, SampleRate: 48000, Channels: 2,
		EditList: &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 50, MediaTime: 0, Rate: NormalRate}}},
	}, 960, 960, 960, 960)
	var groups []trimmedBlock
	for _, b := range blocks {
		if b.group {
			groups = append(groups, b)
		}
	}
	if len(groups) != 2 {
		t.Fatalf("blocks %+v; want the last two as groups", blocks)
	}
	if g := groups[0]; g.tick != 40 || g.discard != 10_000_000 || g.duration != 0 {
		t.Errorf("third frame %+v; want 10 ms of padding and no duration", g)
	}
	if g := groups[1]; g.tick != 60 || g.discard != 20_000_000 || g.duration != 20 {
		t.Errorf("last frame %+v; want all 20 ms of it as padding", g)
	}
}

// Only a codec that decodes every frame to one length says anything by a short
// last frame: Opus frames are as long as they state, so nothing is trimmed.
func TestAShortLastFrameOfAnotherCodecIsNotTrimmed(t *testing.T) {
	blocks := writeTrimmed(t, TrackConfig{
		Kind: Audio, Codec: "Opus", Timescale: 48000, SampleRate: 48000, Channels: 2,
	}, 960, 480)
	if last := blocks[len(blocks)-1]; last.discard != 0 {
		t.Errorf("last block %+v; want no padding", last)
	}
}

// A picture cannot be partly shown, and DiscardPadding is silence: a video
// track's edit ending early writes none.
func TestAVideoEditEndingEarlyWritesNoPadding(t *testing.T) {
	cfg := webmVideoConfig()
	cfg.EditList = &EditList{MovieTimescale: 1000, Edits: []Edit{{SegmentDuration: 10, MediaTime: 0, Rate: NormalRate}}}
	for _, b := range writeTrimmed(t, cfg, 3000, 3000) {
		if b.discard != 0 {
			t.Errorf("block %+v; want no padding on video", b)
		}
	}
}

func TestScaleDurationNearest(t *testing.T) {
	for _, c := range []struct {
		d        uint64
		from, to uint32
		want     uint64
	}{
		{50, 1000, 48000, 2400},
		{1, 3, 2, 1},             // 0.67 rounds up
		{1, 4, 1, 0},             // 0.25 rounds down
		{1 << 62, 1, 1, 1 << 62}, // no overflow on the whole part
	} {
		if got := scaleDurationNearest(c.d, c.from, c.to); got != c.want {
			t.Errorf("scaleDurationNearest(%d, %d, %d) = %d; want %d", c.d, c.from, c.to, got, c.want)
		}
	}
}

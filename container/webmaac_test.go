// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
)

// webmRoundTrip writes a one-track WebM and reads its configuration back.
func webmRoundTrip(t *testing.T, cfg TrackConfig) TrackConfig {
	t.Helper()
	var buf bytes.Buffer
	m := NewWebMMuxer(&buf)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1, 2, 3}, Duration: 21, Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	back, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// What L10 found: the WebM muxer rebuilt the AudioSpecificConfig from the
// track's fields, so ffmpeg's five-byte mono record (11 88 56 e5 00) came out
// as two (11 88), its SBR signalling gone. The record a track brings is now
// written byte for byte, as ffmpeg's matroskaenc writes AAC extradata, and the
// Audio element states the channels the record states, not the sample entry's
// template value.
func TestWebMWritesTheAudioSpecificConfigItIsGiven(t *testing.T) {
	back := webmRoundTrip(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1000, SampleRate: 48000,
		Channels: 2, CodecConfig: monoASC,
	})
	if !bytes.Equal(back.CodecConfig, monoASC) {
		t.Errorf("CodecPrivate = % x, want the source's % x", back.CodecConfig, monoASC)
	}
	if back.Channels != 1 {
		t.Errorf("channels = %d, want the record's 1", back.Channels)
	}
}

// A record that leaves the layout to the bitstream (configuration 0) is still
// written as it is, and the track's own count is the one stated.
func TestWebMTakesTheChannelsFromTheTrackWhenTheRecordLeavesThem(t *testing.T) {
	record := []byte{0x11, 0x80}
	back := webmRoundTrip(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1000, SampleRate: 48000,
		Channels: 3, CodecConfig: record,
	})
	if !bytes.Equal(back.CodecConfig, record) || back.Channels != 3 {
		t.Errorf("CodecPrivate % x, %d channels; want % x and 3", back.CodecConfig, back.Channels, record)
	}
}

// Without a record, the channel count goes through Table 1.19: eight channels
// are configuration 7, and read back as eight.
func TestWebMBuildsARecordThroughTheChannelTable(t *testing.T) {
	back := webmRoundTrip(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 1000, SampleRate: 48000, Channels: 8,
	})
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(back.CodecConfig))
	if err != nil {
		t.Fatal(err)
	}
	if asc.ChannelConfiguration != 7 || back.Channels != 8 {
		t.Errorf("configuration %d, %d channels; want 7 and 8", asc.ChannelConfiguration, back.Channels)
	}
}

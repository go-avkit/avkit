// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
)

// monoASC is the AudioSpecificConfig ffmpeg 9.0.2's native AAC encoder writes
// for `-ac 1 -c:a aac` at 48 kHz: AAC-LC, frequency index 3, channel
// configuration 1, then the 0x2b7 sync extension stating no SBR. Five bytes,
// three of which a record rebuilt from the track's fields would not carry.
var monoASC = []byte{0x11, 0x88, 0x56, 0xe5, 0x00}

// writeOneProgressiveTrack is writeOneTrack for the progressive muxer.
func writeOneProgressiveTrack(t *testing.T, cfg TrackConfig) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack(%s): %v", cfg.Codec, err)
	}
	if err := m.WriteSample(id, Sample{Data: []byte{1, 2, 3}, Duration: 1024, Sync: true}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// aacEntryOf is the mp4a sample entry of a written single-track file, with the
// AudioSpecificConfig its esds carries.
func aacEntryOf(t *testing.T, data []byte) (*mp4.AudioSampleEntryBox, []byte) {
	t.Helper()
	stsd := sampleEntryOf(t, data)
	if stsd.Mp4a == nil || stsd.Mp4a.Esds == nil {
		t.Fatalf("no mp4a sample entry with an esds: %v", stsd.Children)
	}
	asc := audioSpecificConfig(stsd.Mp4a.Esds)
	if asc == nil {
		t.Fatalf("the esds carries no AudioSpecificConfig")
	}
	return stsd.Mp4a, asc
}

// TestMP4MuxersWriteTheAudioSpecificConfigTheyAreGiven is the mono AAC remux
// ffprobe reported as stereo: mp4ff's SetAACDescriptor wrote a stereo record
// and a channelcount of 2 whatever the track was, and the record the source
// carried was dropped. ISO/IEC 14496-12 §12.2.3 channelcount and ISO/IEC
// 14496-3 Table 1.19 channelConfiguration must both say 1, and the record must
// be the source's, byte for byte, as ffmpeg's movenc.c writes it.
func TestMP4MuxersWriteTheAudioSpecificConfigTheyAreGiven(t *testing.T) {
	cfg := TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000,
		// The sample entry of the source may say 2 -- afconvert writes that for
		// a mono track -- and the record is what is believed.
		Channels: 2, CodecConfig: monoASC,
	}
	for name, data := range map[string][]byte{
		"fragmented":  writeOneTrack(t, cfg, 2),
		"progressive": writeOneProgressiveTrack(t, cfg),
	} {
		t.Run(name, func(t *testing.T) {
			entry, asc := aacEntryOf(t, data)
			if entry.ChannelCount != 1 {
				t.Errorf("channelcount = %d, want 1", entry.ChannelCount)
			}
			if entry.SampleRate != 48000 {
				t.Errorf("samplerate = %d, want 48000", entry.SampleRate)
			}
			if !bytes.Equal(asc, monoASC) {
				t.Errorf("AudioSpecificConfig = % x, want the source's % x", asc, monoASC)
			}
			r, err := NewReader(data)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			back, err := r.TrackConfig(r.TrackIDs()[0])
			if err != nil {
				t.Fatalf("TrackConfig: %v", err)
			}
			if back.Channels != 1 || !bytes.Equal(back.CodecConfig, monoASC) {
				t.Errorf("read back %d channels, record % x", back.Channels, back.CodecConfig)
			}
		})
	}
}

// TestMP4MuxersStateTheChannelCountOfTheTrackFields covers a caller with no
// record: the one built states the track's channels, in both fields.
func TestMP4MuxersStateTheChannelCountOfTheTrackFields(t *testing.T) {
	cases := []struct {
		name          string
		channels      int
		objectType    byte
		configuration byte
		count         uint16
	}{
		{"mono", 1, 0, 1, 1},
		{"stereo", 2, 0, 2, 2},
		{"5.1", 6, 0, 6, 6},
		// Table 1.19's one row that is not the number it is written as.
		{"7.1", 8, 0, 7, 8},
		// Nothing stated has always meant stereo.
		{"unstated", 0, 0, 2, 2},
		// HE-AACv2's record states the mono core parametric stereo builds on;
		// the sample entry states what is heard.
		{"HE-AACv2", 2, aac.HEAACv2, 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, raw := aacEntryOf(t, writeOneTrack(t, TrackConfig{
				Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000,
				Channels: tc.channels, AudioObjectType: tc.objectType,
			}, 1))
			asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("the record written cannot be read: %v", err)
			}
			if asc.ChannelConfiguration != tc.configuration || entry.ChannelCount != tc.count {
				t.Errorf("channelConfiguration %d, channelcount %d; want %d and %d",
					asc.ChannelConfiguration, entry.ChannelCount, tc.configuration, tc.count)
			}
		})
	}
}

// TestMP4MuxersTakeTheChannelCountFromTheTrackWhenTheRecordLeavesIt covers
// channelConfiguration 0, which says a program config element states the layout:
// the record is still written as it is, and the sample entry states the count
// the caller gave.
func TestMP4MuxersTakeTheChannelCountFromTheTrackWhenTheRecordLeavesIt(t *testing.T) {
	record := []byte{0x11, 0x80} // AAC-LC, 48 kHz, configuration 0
	entry, asc := aacEntryOf(t, writeOneTrack(t, TrackConfig{
		Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000,
		Channels: 3, CodecConfig: record,
	}, 1))
	if entry.ChannelCount != 3 || !bytes.Equal(asc, record) {
		t.Errorf("channelcount %d, record % x; want 3 and % x", entry.ChannelCount, asc, record)
	}
}

func TestMP4MuxersRefuseAnAACTrackTheyCannotDescribe(t *testing.T) {
	base := TrackConfig{Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000}
	with := func(f func(*TrackConfig)) TrackConfig {
		cfg := base
		f(&cfg)
		return cfg
	}
	cases := map[string]TrackConfig{
		"no sample rate":    with(func(c *TrackConfig) { c.SampleRate = 0 }),
		"a record unread":   with(func(c *TrackConfig) { c.CodecConfig = []byte{0xff} }),
		"no channel at all": with(func(c *TrackConfig) { c.CodecConfig = []byte{0x11, 0x80} }),
		"more channels than a sample entry holds": with(func(c *TrackConfig) {
			c.CodecConfig, c.Channels = []byte{0x11, 0x80}, 0x10000
		}),
		// Seven channels is no row of Table 1.19: configuration 7 is EIGHT.
		"seven channels and no record": with(func(c *TrackConfig) { c.Channels = 7 }),
		"a profile no record is built for": with(func(c *TrackConfig) {
			c.Channels, c.AudioObjectType = 2, 1
		}),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := NewMuxer(&buf).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("Muxer: err = %v, want ErrTrackConfig", err)
			}
			if _, err := NewProgressiveMuxer(&buf).AddTrack(cfg); !errors.Is(err, ErrTrackConfig) {
				t.Errorf("ProgressiveMuxer: err = %v, want ErrTrackConfig", err)
			}
		})
	}
}

func TestAACConfiguration(t *testing.T) {
	// The inverse of aacChannels over every count Table 1.19 states, and
	// nothing else.
	for channels := -1; channels <= 9; channels++ {
		configuration, ok := aacConfiguration(channels)
		stated := channels >= 1 && channels <= 6 || channels == 8
		if ok != stated || ok && aacChannels(configuration) != channels {
			t.Errorf("aacConfiguration(%d) = %d, %v", channels, configuration, ok)
		}
	}
}

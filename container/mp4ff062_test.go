// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
)

// mp4ff 0.62.0 writes AAC Main (ISO/IEC 14496-3 audioObjectType 1), which 0.61.0
// refused, so a track of that profile is described like any other.
func TestMP4MuxersBuildAnAACMainRecord(t *testing.T) {
	for name, data := range map[string][]byte{
		"fragmented": writeOneTrack(t, TrackConfig{
			Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000,
			Channels: 2, AudioObjectType: aac.AACMain,
		}, 1),
		"progressive": writeOneProgressiveTrack(t, TrackConfig{
			Kind: Audio, Codec: "mp4a", Timescale: 48000, SampleRate: 48000,
			Channels: 2, AudioObjectType: aac.AACMain,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			entry, raw := aacEntryOf(t, data)
			asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if asc.ObjectType != aac.AACMain || asc.ChannelConfiguration != 2 || entry.ChannelCount != 2 {
				t.Errorf("object type %d, configuration %d, channelcount %d; want 1, 2 and 2",
					asc.ObjectType, asc.ChannelConfiguration, entry.ChannelCount)
			}
		})
	}
}

// An ADTS stream of AAC Main states its profile as 0, object type 1, and the
// record written for it reads back as that.
func TestConfigFromAACStatesAnAACMainRecord(t *testing.T) {
	frame := adtsFrame(t, []byte{0x21, 0x22, 0x23, 0x24})
	frame[2] &= 0x3f
	sc, err := ConfigFromSamples("mp4a", []Sample{{Data: frame, Duration: 1024}}, SampleTimescale(48000))
	if err != nil {
		t.Fatal(err)
	}
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(sc.CodecConfig))
	if err != nil {
		t.Fatal(err)
	}
	if asc.ObjectType != aac.AACMain || sc.AudioObjectType != aac.AACMain {
		t.Errorf("object type %d in the record and %d in the track, want 1", asc.ObjectType, sc.AudioObjectType)
	}
}

// unwritableBox is a box whose writing fails in the slice writer too.
type unwritableBox struct{ mp4.UnknownBox }

func (unwritableBox) Type() string                                 { return "fail" }
func (unwritableBox) Size() uint64                                 { return 8 }
func (unwritableBox) Encode(io.Writer) error                       { return errors.New("no") }
func (unwritableBox) EncodeSW(bits.SliceWriter) error              { return errors.New("no") }
func (unwritableBox) Info(io.Writer, string, string, string) error { return nil }

// A tx3g entry that mp4ff 0.62.0 decodes is its own box, whose payload is what
// travels; one it cannot decode stays an unknown box and travels as it stands.
func TestATx3gEntryIsReadAsMp4ffsBoxOrAsAnUnknownOne(t *testing.T) {
	typed := mp4.NewTextSampleEntryBox("tx3g")
	typed.DataReferenceIndex = 1
	typed.AddChild(&mp4.FtabBox{Fonts: []mp4.FontRecord{{ID: 1, Name: "Serif"}}})
	stsd := &mp4.StsdBox{}
	stsd.AddChild(typed)
	entry := tx3gEntry(stsd)
	if entry == nil {
		t.Fatal("a tx3g sample entry is not one")
	}
	payload, err := tx3gPayload(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTx3gEntry(payload); err != nil || !bytes.Contains(payload, []byte("Serif")) {
		t.Errorf("payload %x: %v", payload, err)
	}

	// The 'text' type is not a tx3g entry, though its layout is.
	stsd = &mp4.StsdBox{}
	stsd.AddChild(mp4.NewTextSampleEntryBox("text"))
	if tx3gEntry(stsd) != nil {
		t.Error("a text sample entry is a tx3g one")
	}

	unknown := mp4.CreateUnknownBox("tx3g", 8+3, []byte{1, 2, 3})
	if got, err := tx3gPayload(unknown); err != nil || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("unknown payload %x: %v", got, err)
	}

	broken := mp4.NewTextSampleEntryBox("tx3g")
	broken.AddChild(&unwritableBox{})
	if _, err := tx3gPayload(broken); err == nil {
		t.Error("an entry that cannot be written has a payload")
	}
}

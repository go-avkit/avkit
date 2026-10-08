// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/at-wat/ebml-go"
)

// eac3Frame builds an Enhanced AC-3 sync frame header.
//
// ⛔ bsid lands 29 bits past the sync word in BOTH syntaxes, which is what lets
// it be read ahead to tell them apart: here 2+3+11+2+2+3+1 = 24 bits precede it,
// and in plain AC-3 16+2+6 = 24 do. Getting that wrong would read one syntax as
// the other, and both would parse.
func eac3Frame(t *testing.T, acmod byte, lfe bool, fscod, numblkscod byte, bytes int) []byte {
	t.Helper()
	var w ac3Writer
	w.put(0x0B77, 16)
	w.put(0, 2)                  // strmtyp: independent
	w.put(0, 3)                  // substreamid
	w.put(uint32(bytes/2-1), 11) // frmsiz
	w.put(uint32(fscod), 2)      //
	w.put(uint32(numblkscod), 2) //
	w.put(uint32(acmod), 3)      //
	w.put(boolToBit(lfe), 1)     //
	w.put(16, 5)                 // bsid: above ten, so Enhanced AC-3
	for len(w.b) < bytes {
		w.b = append(w.b, 0)
	}
	return w.b
}

// ac3ClassicFrame builds a plain AC-3 sync frame header.
func ac3ClassicFrame(t *testing.T, acmod byte, lfe bool, fscod, frameSizeCode byte) []byte {
	t.Helper()
	var w ac3Writer
	w.put(0x0B77, 16)
	w.put(0, 16)                    // crc1
	w.put(uint32(fscod), 2)         //
	w.put(uint32(frameSizeCode), 6) //
	w.put(8, 5)                     // bsid: ten or below, so plain AC-3
	w.put(0, 3)                     // bsmod
	w.put(uint32(acmod), 3)         //
	if acmod == 2 {
		w.put(0, 2) // dsurmod
	} else {
		if acmod&1 != 0 && acmod != 1 {
			w.put(0, 2) // cmixlev
		}
		if acmod&4 != 0 {
			w.put(0, 2) // surmixlev
		}
	}
	w.put(boolToBit(lfe), 1)
	for len(w.b) < 16 {
		w.b = append(w.b, 0)
	}
	return w.b
}

func TestTheSyncFrameSaysWhatTheContainerDidNot(t *testing.T) {
	for _, c := range []struct {
		name     string
		data     []byte
		eac3     bool
		channels int
		rate     int
	}{
		{"Enhanced AC-3 5.1 at 48 kHz", eac3Frame(t, 7, true, 0, 3, 1792), true, 6, 48000},
		{"Enhanced AC-3 stereo at 48 kHz", eac3Frame(t, 2, false, 0, 3, 768), true, 2, 48000},
		{"Enhanced AC-3 at 44.1 kHz", eac3Frame(t, 2, false, 1, 3, 768), true, 2, 44100},
		{"AC-3 5.1 at 48 kHz", ac3ClassicFrame(t, 7, true, 0, 30), false, 6, 48000},
		{"AC-3 stereo at 32 kHz", ac3ClassicFrame(t, 2, false, 2, 20), false, 2, 32000},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := parseAC3Frame(c.data)
			if err != nil {
				t.Fatalf("parseAC3Frame: %v", err)
			}
			if f.EAC3 != c.eac3 {
				t.Errorf("EAC3 = %v; want %v -- the two syntaxes were told apart wrongly", f.EAC3, c.eac3)
			}
			if f.Channels != c.channels {
				t.Errorf("channels = %d; want %d", f.Channels, c.channels)
			}
			if f.SampleRate != c.rate {
				t.Errorf("sample rate = %d; want %d", f.SampleRate, c.rate)
			}
		})
	}
}

// The record has to be one the muxer accepts, which is the only thing that
// makes reading the frame worth anything.
func TestTheRecordBuiltFromAFrameIsOneTheMuxerTakes(t *testing.T) {
	for _, c := range []struct {
		name  string
		codec string
		data  []byte
	}{
		{"Enhanced AC-3", "ec-3", eac3Frame(t, 7, true, 0, 3, 1792)},
		{"AC-3", "ac-3", ac3ClassicFrame(t, 7, true, 0, 30)},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ac3TrackConfig(c.codec, []Sample{{Data: c.data, Duration: 1536, Sync: true}})
			if err != nil {
				t.Fatalf("ac3TrackConfig: %v", err)
			}
			cfg.Timescale = 48000
			if _, err := NewMuxer(&bytes.Buffer{}).AddTrack(cfg); err != nil {
				t.Fatalf("the record the frame gave was refused: %v", err)
			}
		})
	}
}

// The witness from the issue: an Enhanced AC-3 track read out of Matroska,
// which states no CodecPrivate for it, used to fail with "a dec3 record is at
// least 5 bytes, not 0".
func TestEAC3FromMatroskaRemuxes(t *testing.T) {
	frame := eac3Frame(t, 7, true, 0, 3, 1792)
	doc := &mkvReadDoc{
		Header: mkvHeader{DocType: "matroska"},
		Segment: mkvReadSegment{
			Info: mkvInfo{TimecodeScale: defaultTimecodeScale},
			Tracks: mkvTracks{TrackEntry: []mkvTrackEntry{{
				TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_EAC3",
				DefaultDuration: 32_000_000,
				Audio:           &mkvAudio{SamplingFrequency: 48000, Channels: 6},
			}}},
			Cluster: []mkvCluster{{Timecode: 0, SimpleBlock: []ebml.Block{
				simple(1, 0, true, frame),
				simple(1, 32, true, frame),
			}}},
		},
	}
	r, err := NewReader(marshalDoc(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodecConfig) != 0 {
		t.Fatalf("the container stated a configuration it does not carry: %d bytes", len(cfg.CodecConfig))
	}

	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		if strings.Contains(err.Error(), "dec3 record") {
			t.Fatalf("still refused for want of a dec3 record: %v", err)
		}
		t.Fatalf("remux: %v", err)
	}

	// And the output carries a real EC3SpecificBox.
	parsed, err := mp4.DecodeFile(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	stsd := parsed.Moov.Traks[0].Mdia.Minf.Stbl.Stsd
	if stsd.EC3 == nil || stsd.EC3.Dec3 == nil {
		t.Fatal("the output has no ec-3 sample entry")
	}
	if got := stsd.EC3.SampleRate; got != 48000 {
		t.Errorf("sample entry rate = %d; want 48000", got)
	}
	_ = io.Discard
}

func TestASyncFrameThatIsNotOneIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"too short to hold a header", []byte{0x0B, 0x77}},
		{"no sync word", []byte{0x00, 0x00, 0, 0, 0, 0, 0, 0}},
		{"a bit stream identification past sixteen", func() []byte {
			var w ac3Writer
			w.put(0x0B77, 16)
			w.put(0, 24)
			w.put(31, 5) // bsid
			return w.b
		}()},
		{"a reserved sample rate code, Enhanced AC-3", func() []byte {
			var w ac3Writer
			w.put(0x0B77, 16)
			w.put(0, 2)
			w.put(0, 3)
			w.put(100, 11)
			w.put(3, 2) // fscod: the reduced-rate form
			w.put(3, 2) // fscod2: reserved
			w.put(7, 3)
			w.put(1, 1)
			w.put(16, 5)
			return w.b
		}()},
		{"a reserved sample rate code, AC-3", ac3ClassicFrame(t, 2, false, 3, 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseAC3Frame(c.data); err == nil {
				t.Error("accepted")
			}
		})
	}
	// And a track whose samples hold no frame at all.
	if _, err := ac3TrackConfig("ec-3", []Sample{{Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}}}); err == nil {
		t.Error("a track with no sync frame was described anyway")
	}
}

// Low-rate AC-3: above bit stream identification eight, the sample rate and the
// bit rate are halved once per step. A reader taking the table value as it
// stands reports 48 kHz for a 24 kHz stream.
func TestAC3AboveBSIDEightHalvesItsRates(t *testing.T) {
	frame := func(bsid uint32) []byte {
		var w ac3Writer
		w.put(0x0B77, 16)
		w.put(0, 16) // crc1
		w.put(0, 2)  // fscod: 48 kHz at bsid eight
		w.put(20, 6) // frmsizecod
		w.put(bsid, 5)
		w.put(0, 3) // bsmod
		w.put(2, 3) // acmod: stereo
		w.put(0, 2) // dsurmod
		w.put(0, 1) // lfeon
		for len(w.b) < 16 {
			w.b = append(w.b, 0)
		}
		return w.b
	}
	at8, err := parseAC3Frame(frame(8))
	if err != nil {
		t.Fatal(err)
	}
	at9, err := parseAC3Frame(frame(9))
	if err != nil {
		t.Fatal(err)
	}
	if at8.SampleRate != 48000 || at9.SampleRate != 24000 {
		t.Errorf("rates = %d and %d; want 48000 and 24000", at8.SampleRate, at9.SampleRate)
	}
	if at9.BitRate()*2 != at8.BitRate() {
		t.Errorf("bit rates = %d and %d; the second should be half the first",
			at8.BitRate(), at9.BitRate())
	}
}

// The mix levels are stated only for some channel modes, and each one read or
// skipped wrongly shifts everything after it -- including lfeon, which decides
// the channel count.
func TestTheMixLevelsAreSkippedForTheRightChannelModes(t *testing.T) {
	for _, c := range []struct {
		acmod    byte
		lfe      bool
		channels int
	}{
		{0, false, 2}, // dual mono
		{1, true, 2},  // mono with the low-frequency channel
		{2, false, 2}, // stereo: states a surround mode instead
		{3, false, 3}, // 3/0: states a centre mix level
		{4, true, 4},  // 2/1: states a surround mix level
		{7, true, 6},  // 3/2 with the low-frequency channel
	} {
		t.Run("", func(t *testing.T) {
			f, err := parseAC3Frame(ac3ClassicFrame(t, c.acmod, c.lfe, 0, 20))
			if err != nil {
				t.Fatalf("acmod %d: %v", c.acmod, err)
			}
			if f.Channels != c.channels || f.LFEOn != c.lfe {
				t.Errorf("acmod %d: %d channels, lfe %v; want %d and %v",
					c.acmod, f.Channels, f.LFEOn, c.channels, c.lfe)
			}
		})
	}
}

// Enhanced AC-3 at a reduced rate states its code in a second field, and the
// rate is halved.
func TestEAC3AtAReducedRate(t *testing.T) {
	var w ac3Writer
	w.put(0x0B77, 16)
	w.put(0, 2)    // strmtyp
	w.put(0, 3)    // substreamid
	w.put(383, 11) // frmsiz
	w.put(3, 2)    // fscod: the reduced-rate form
	w.put(0, 2)    // fscod2: 48 kHz halved
	w.put(2, 3)    // acmod
	w.put(0, 1)    // lfeon
	w.put(16, 5)   // bsid
	for len(w.b) < 768 {
		w.b = append(w.b, 0)
	}
	f, err := parseAC3Frame(w.b)
	if err != nil {
		t.Fatal(err)
	}
	if f.SampleRate != 24000 {
		t.Errorf("sample rate = %d; want 24000", f.SampleRate)
	}
	// With no block count stated the frame keeps the default of six.
	if f.NumBlocks != 6 {
		t.Errorf("blocks = %d; want 6", f.NumBlocks)
	}
	if r := dec3Record(f); len(r) == 0 {
		t.Error("no record was built")
	}
}

// A dependent substream describes part of another stream, not a track. Passing
// one over and taking the independent one is what makes the channel count right.
func TestADependentSubstreamIsPassedOver(t *testing.T) {
	dependent := func() []byte {
		var w ac3Writer
		w.put(0x0B77, 16)
		w.put(1, 2)    // strmtyp: dependent
		w.put(0, 3)    //
		w.put(100, 11) //
		w.put(0, 2)    // fscod
		w.put(3, 2)    // numblkscod
		w.put(2, 3)    // acmod: stereo, which is not the track's shape
		w.put(0, 1)    // lfeon
		w.put(16, 5)   // bsid
		for len(w.b) < 202 {
			w.b = append(w.b, 0)
		}
		return w.b
	}()
	cfg, err := ac3TrackConfig("ec-3", []Sample{
		{Data: dependent},
		{Data: eac3Frame(t, 7, true, 0, 3, 1792)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels != 6 {
		t.Errorf("channels = %d; want 6, from the independent substream", cfg.Channels)
	}
}

func TestTheFrameReaderRefusesTheRestOfWhatCannotBeOne(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"a frame size code past the table", ac3ClassicFrame(t, 2, false, 0, 38)},
		{"an Enhanced AC-3 frame of no length", func() []byte {
			var w ac3Writer
			w.put(0x0B77, 16)
			w.put(0, 2)
			w.put(0, 3)
			w.put(0, 11) // frmsiz: two bytes, which is shorter than a header
			w.put(0, 2)
			w.put(3, 2)
			w.put(2, 3)
			w.put(0, 1)
			w.put(16, 5)
			return w.b
		}()},
		{"a reserved Enhanced AC-3 frame type", func() []byte {
			var w ac3Writer
			w.put(0x0B77, 16)
			w.put(3, 2) // strmtyp: reserved
			w.put(0, 3)
			w.put(100, 11)
			w.put(0, 2)
			w.put(3, 2)
			w.put(2, 3)
			w.put(0, 1)
			w.put(16, 5)
			return w.b
		}()},
		{"it ends inside the Enhanced AC-3 header", []byte{0x0B, 0x77, 0, 0, 0, 0x80}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseAC3Frame(c.data); err == nil {
				t.Error("accepted")
			}
		})
	}
	if got := ac3FrameSize(200, 0); got != 0 {
		t.Errorf("a code past the table gave %d", got)
	}
	if got := ac3FrameSize(0, 9); got != 0 {
		t.Errorf("a sample rate code past the table gave %d", got)
	}
	// A bit rate code past the table is not one this can state.
	if got := (ac3Frame{BitRateCode: 200}).BitRate(); got != 0 {
		t.Errorf("a bit rate code past the table gave %d", got)
	}
	// Nor is a frame that states no blocks.
	if got := (ac3Frame{EAC3: true}).BitRate(); got != 0 {
		t.Errorf("a frame with no blocks gave a rate of %d", got)
	}
}

// A rate past what the record can hold is clamped rather than wrapped: thirteen
// bits cannot say more, and wrapping would state a small number for a large one.
func TestADataRatePastTheRecordIsClamped(t *testing.T) {
	f := ac3Frame{EAC3: true, FrameSize: 1 << 20, SampleRate: 48000, NumBlocks: 1}
	r := dec3Record(f)
	if len(r) < 2 {
		t.Fatal("no record")
	}
	if got := int(r[0])<<5 | int(r[1])>>3; got != 0x1FFF {
		t.Errorf("data rate = %d; want the largest the field holds, 8191", got)
	}
}

// A track whose frames cannot describe it is refused naming the track, rather
// than written with an empty record.
func TestARemuxOfUndescribableAC3IsRefused(t *testing.T) {
	cfg := TrackConfig{Kind: Audio, Codec: "ec-3", Timescale: 48000}
	_, err := describeFromSamples(cfg, []Sample{{Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}}})
	if err == nil {
		t.Fatal("samples that describe nothing were accepted")
	}
	if !strings.Contains(err.Error(), "ec-3") {
		t.Errorf("the refusal does not name the track: %v", err)
	}
}

// Plain AC-3 reads up to forty bits of header where the sync check guarantees
// thirty-two, so a frame can run out part way through -- and must say so rather
// than report the zeros a short read leaves behind.
func TestAnAC3HeaderThatRunsOutIsRefused(t *testing.T) {
	// 3/2 with the low-frequency channel is the longest header: crc1 16, fscod
	// 2, frmsizecod 6, bsid 5, bsmod 3, acmod 3, two mix levels 4, lfeon 1 --
	// forty bits past the sync word, so seven bytes exactly. Six is one short,
	// and the boundary is pinned here rather than guessed: the first version of
	// this test expected seven to fail and found out otherwise.
	full := ac3ClassicFrame(t, 7, true, 0, 20)
	if _, err := parseAC3Frame(full[:6]); err == nil {
		t.Error("six bytes: accepted, where the header wants seven")
	}
	if _, err := parseAC3Frame(full[:7]); err != nil {
		t.Errorf("seven bytes: refused, where the header fits exactly: %v", err)
	}
}

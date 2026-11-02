// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
)

// HE-AAC v2 is an AAC-LC core at half the output rate (spectral band
// replication, SBR) and, for a stereo output, a MONO core (parametric stereo,
// PS), so the stream is signalled in two places that disagree on purpose: the
// AudioSpecificConfig (ISO/IEC 14496-3 §1.6.2.1, Table 1.15) says channel
// configuration 1 and the core's rate, and the player is to hear two channels at
// the extension's rate.
//
// What the files in the wild do, measured on ffprobe over the 778 HE-AACv2 files
// of the library this package is used on, is what these tests keep:
//
//   - The fixture heaacv2.m4a is ffmpeg 9.0.2 with `-c:a aac_at -profile:a 28`:
//     the BACKWARD-COMPATIBLE record 13 88 56 e5 a5 48 80, AAC-LC at 22050 Hz,
//     mono, then sync extension 0x2b7, SBR at 44100 Hz, then 0x548 and the
//     psPresentFlag. Its sample entry says 2 channels at 44100 Hz.
//   - Encoders that write the HIERARCHICAL form (audioObjectType 29) are the
//     other common shape; the second half of these tests rewrites the fixture's
//     record into it.
//
// ffmpeg is the oracle: its matroskaenc writes Channels = 2 for the fixture and
// its movenc a channelcount of 2, both measured; its adtsenc writes the header
// from the record's CORE (adts_decode_extradata). Before this file avkit wrote 1
// in both, and an ADTS header at the output rate -- 88200 Hz for the fixture.

// bitsToBytes packs a string of '0' and '1', zero padded at the end.
func bitsToBytes(s string) []byte {
	s = strings.ReplaceAll(s, " ", "")
	out := make([]byte, (len(s)+7)/8)
	for i, c := range s {
		if c == '1' {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// heaacv2ASC is the fixture's record.
var heaacv2ASC = []byte{0x13, 0x88, 0x56, 0xe5, 0xa5, 0x48, 0x80}

// explicitPS is the same stream signalled hierarchically: audioObjectType 29,
// the core at 22050 Hz, mono, the output at 44100 Hz, then the core's AAC-LC.
func explicitPS(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	asc := aac.AudioSpecificConfig{ObjectType: aac.HEAACv2, ChannelConfiguration: 1,
		SamplingFrequency: 22050, ExtensionFrequency: 44100}
	if err := asc.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestASCSignalling(t *testing.T) {
	// 0001 0 0111 0001 000 is AAC-LC, 22050 Hz, mono, frameLengthFlag 0,
	// dependsOnCoreCoder 0, extensionFlag 0; the sync extension follows.
	const core = "00010 0111 0001 000"
	const sync = "01010110111"   // 0x2b7
	const sbrOn = "00101 1 0100" // extensionAudioObjectType 5, sbrPresentFlag, 44100 Hz
	const sbrOff = "00101 0"     // ... sbrPresentFlag 0
	const psSync = "10101001000" // 0x548
	const stereoCore = "00010 0111 0010 000"
	for name, tc := range map[string]struct {
		asc          []byte
		core, output int
		ps           bool
		channels     int
		adts         bool // an ADTS header can state it
		adtsRate     int
		adtsConfig   byte
	}{
		"the fixture":     {heaacv2ASC, 22050, 44100, true, 2, true, 22050, 1},
		"hierarchical PS": {explicitPS(t), 22050, 44100, true, 2, true, 22050, 1},
		"hierarchical PS over a stereo core": {
			(func() []byte {
				var buf bytes.Buffer
				asc := aac.AudioSpecificConfig{ObjectType: aac.HEAACv2, ChannelConfiguration: 2,
					SamplingFrequency: 22050, ExtensionFrequency: 44100}
				_ = asc.Encode(&buf)
				return buf.Bytes()
			})(), 22050, 44100, false, 2, true, 22050, 2},
		// PS is kept for a mono core only, as ffmpeg keeps it.
		"hierarchical SBR, mono":    {bitsToBytes("00101 0111 0001 0100 00010 000"), 22050, 44100, false, 1, true, 22050, 1},
		"backward-compatible SBR":   {[]byte{0x13, 0x90, 0x56, 0xe5, 0xa0}, 22050, 44100, false, 2, true, 22050, 2},
		"SBR on a mono core, no PS": {bitsToBytes(core + sync + sbrOn), 22050, 44100, false, 1, true, 22050, 1},
		"PS sync after SBR":         {bitsToBytes(core + sync + sbrOn + psSync + "1"), 22050, 44100, true, 2, true, 22050, 1},
		"PS flag clear":             {bitsToBytes(core + sync + sbrOn + psSync + "0"), 22050, 44100, false, 1, true, 22050, 1},
		// The PS extension is read behind an SBR one that is on, and not otherwise.
		"PS behind SBR that is off": {bitsToBytes(core + sync + sbrOff + psSync + "1"), 22050, 0, false, 1, true, 22050, 1},
		"PS on a stereo core":       {bitsToBytes(stereoCore + sync + sbrOn + psSync + "1"), 22050, 44100, false, 2, true, 22050, 2},
		"plain AAC-LC":              {[]byte{0x12, 0x10}, 44100, 0, false, 2, true, 44100, 2},
		"a layout in the bitstream": {[]byte{0x12, 0x00}, 44100, 0, false, 0, true, 44100, 0},
		"7.1":                       {[]byte{0x11, 0xb8}, 48000, 0, false, 8, true, 48000, 7},
		// What no ADTS header can state: a core that is not Main, LC, SSR or
		// LTP, a rate written in full, a configuration past 7.
		"ER AAC LC":        {bitsToBytes("10001 0011 0010 0"), 48000, 0, false, 2, false, 0, 0},
		"a rate in full":   {[]byte{0x17, 0x80, 0x1f, 0x40, 0x10}, 16000, 0, false, 2, false, 0, 0},
		"configuration 11": {[]byte{0x11, 0xd8}, 48000, 0, false, 0, false, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s := readASC(tc.asc)
			if s.core != tc.core || s.output != tc.output || s.ps != tc.ps {
				t.Errorf("readASC(% x) = core %d, output %d, ps %v; want %d, %d, %v",
					tc.asc, s.core, s.output, s.ps, tc.core, tc.output, tc.ps)
			}
			if got := aacOutputChannels(tc.asc); got != tc.channels {
				t.Errorf("aacOutputChannels(% x) = %d; want %d", tc.asc, got, tc.channels)
			}
			objectType, rate, configuration, ok := adtsParameters(tc.asc)
			if ok != tc.adts || ok && (objectType != aac.AAClc || rate != tc.adtsRate || configuration != tc.adtsConfig) {
				t.Errorf("adtsParameters(% x) = %d, %d, %d, %v; want an LC core at %d with configuration %d (%v)",
					tc.asc, objectType, rate, configuration, ok, tc.adtsRate, tc.adtsConfig, tc.adts)
			}
		})
	}
}

// A mono core with SBR and no PS signalled, in a file whose sample entry says 2,
// is HE-AACv2 signalled implicitly (two real files of the library are); the same
// record under an entry saying 1, or a mono record with no SBR under an entry
// saying 2 (afconvert's template field), stays mono.
func TestImplicitPSIsTakenFromTheContainersTwo(t *testing.T) {
	sbrMono := bitsToBytes("00010 0111 0001 000" + "01010110111" + "00101 1 0100")
	for name, tc := range map[string]struct {
		asc    []byte
		stated int
		want   int
	}{
		"SBR over a mono core, entry 2": {sbrMono, 2, 2},
		"SBR over a mono core, entry 1": {sbrMono, 1, 1},
		"mono AAC-LC, entry 2":          {monoASC, 2, 1},
	} {
		if got := aacTrackChannels(tc.asc, tc.stated); got != tc.want {
			t.Errorf("%s: %d channels; want %d", name, got, tc.want)
		}
	}
}

// heaacv2Sources are the same HE-AACv2 stream as a caller meets it: read from
// the fixture, whose record is the backward-compatible one, and with the record
// rewritten into the hierarchical form (the samples are the same bytes: a
// decoder finds the PS in them either way).
func heaacv2Sources(t *testing.T) map[string]*Reader {
	t.Helper()
	data, err := os.ReadFile("testdata/heaacv2.m4a")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*Reader{"backward-compatible": r, "hierarchical": withRecord(t, r, explicitPS(t))}
}

// withRecord is r with its audio track's AudioSpecificConfig replaced, as a
// Reader would hand a file whose esds carried it: built by writing the track out
// again with the other record and reading that.
func withRecord(t *testing.T, r *Reader, record []byte) *Reader {
	t.Helper()
	id := r.TrackIDs()[0]
	cfg, err := r.TrackConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CodecConfig = record
	var buf bytes.Buffer
	m := NewProgressiveMuxer(&buf)
	out, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if err := m.WriteSample(out, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// payload is the coded frames of a reader's audio track.
func payload(t *testing.T, r *Reader) [][]byte {
	t.Helper()
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, len(samples))
	for i, s := range samples {
		out[i] = s.Data
	}
	return out
}

func samePayload(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestHEAACv2SurvivesEveryWriter is lot M1 of go-avkit#89: an HE-AACv2 track
// read from an MP4 and written to a fragmented MP4, a progressive MP4, Matroska
// and an MPEG transport stream keeps what it is, read back from each, and goes
// back to an MP4 from the ones that can carry it.
//
// What "what it is" means is the oracle's, ffmpeg 9.0.2 with ffprobe: the
// AudioSpecificConfig byte for byte where the container has one, two channels
// at 44100 Hz stated by the sample entry and the Audio element, the core's 22050
// Hz and one channel in an ADTS header; and the coded frames unchanged.
func TestHEAACv2SurvivesEveryWriter(t *testing.T) {
	for kind, src := range heaacv2Sources(t) {
		t.Run(kind, func(t *testing.T) {
			cfg, err := src.TrackConfig(src.TrackIDs()[0])
			if err != nil {
				t.Fatal(err)
			}
			record := cfg.CodecConfig
			if cfg.Channels != 2 || cfg.SampleRate != 44100 {
				t.Fatalf("the source reads as %d channels at %d Hz; want 2 at 44100", cfg.Channels, cfg.SampleRate)
			}
			frames := payload(t, src)

			// The MP4 writers state the record as it was and a channelcount of 2.
			for name, write := range map[string]func(*bytes.Buffer) sampleCloser{
				"fragmented":  func(b *bytes.Buffer) sampleCloser { return NewMuxer(b) },
				"progressive": func(b *bytes.Buffer) sampleCloser { return NewProgressiveMuxer(b) },
			} {
				var out bytes.Buffer
				copyInto(t, write(&out), src)
				entry, asc := aacEntryOf(t, out.Bytes())
				if entry.ChannelCount != 2 || entry.SampleRate != 44100 || !bytes.Equal(asc, record) {
					t.Errorf("%s: channelcount %d, samplerate %d, record % x; want 2, 44100, % x",
						name, entry.ChannelCount, entry.SampleRate, asc, record)
				}
				assertBack(t, name, out.Bytes(), record, 2, 44100, frames)
			}

			// Matroska states 2 channels, the core's rate and the output's.
			var mkv bytes.Buffer
			copyInto(t, NewWebMMuxer(&mkv, BufferedSegment()), src)
			a := audioElement(t, mkv.Bytes())
			if a.Channels != 2 || a.SamplingFrequency != 22050 || a.OutputSamplingFrequency != 44100 {
				t.Errorf("matroska: %d channels, %v and %v Hz; want 2, 22050 and 44100",
					a.Channels, a.SamplingFrequency, a.OutputSamplingFrequency)
			}
			assertBack(t, "matroska", mkv.Bytes(), record, 2, 44100, frames)
			viaMatroska, err := NewReader(mkv.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			for name, write := range map[string]func(*bytes.Buffer) sampleCloser{
				"matroska to fragmented":  func(b *bytes.Buffer) sampleCloser { return NewMuxer(b) },
				"matroska to progressive": func(b *bytes.Buffer) sampleCloser { return NewProgressiveMuxer(b) },
			} {
				var out bytes.Buffer
				copyInto(t, write(&out), viaMatroska)
				entry, asc := aacEntryOf(t, out.Bytes())
				if entry.ChannelCount != 2 || !bytes.Equal(asc, record) {
					t.Errorf("%s: channelcount %d, record % x; want 2 and % x", name, entry.ChannelCount, asc, record)
				}
			}

			// A transport stream has no record: the ADTS header states the core.
			var ts bytes.Buffer
			copyInto(t, NewTSMuxer(&ts), src)
			back, err := NewReader(ts.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			tc, err := back.TrackConfig(back.TrackIDs()[0])
			if err != nil {
				t.Fatal(err)
			}
			if tc.Channels != 1 || tc.SampleRate != 22050 || tc.AudioObjectType != aac.AAClc {
				t.Errorf("transport stream: %d channels at %d Hz, object type %d; want the core, 1 at 22050 Hz, AAC-LC",
					tc.Channels, tc.SampleRate, tc.AudioObjectType)
			}
			if !samePayload(payload(t, back), frames) {
				t.Errorf("transport stream: the coded frames changed")
			}
			// And back to an MP4: the core's record, which is what ffmpeg's own
			// copy of the same stream writes (13 88), since nothing in the stream
			// says more.
			for name, write := range map[string]func(*bytes.Buffer) sampleCloser{
				"transport stream to fragmented":  func(b *bytes.Buffer) sampleCloser { return NewMuxer(b) },
				"transport stream to progressive": func(b *bytes.Buffer) sampleCloser { return NewProgressiveMuxer(b) },
			} {
				var out bytes.Buffer
				copyInto(t, write(&out), back)
				entry, asc := aacEntryOf(t, out.Bytes())
				if !bytes.Equal(asc, []byte{0x13, 0x88}) || entry.ChannelCount != 1 || entry.SampleRate != 22050 {
					t.Errorf("%s: record % x, channelcount %d, samplerate %d; want 13 88, 1, 22050",
						name, asc, entry.ChannelCount, entry.SampleRate)
				}
			}
		})
	}
}

// assertBack reads a written file again and checks the track it says.
func assertBack(t *testing.T, name string, data, record []byte, channels, rate int, frames [][]byte) {
	t.Helper()
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	cfg, err := r.TrackConfig(r.TrackIDs()[0])
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if cfg.Channels != channels || cfg.SampleRate != rate || !bytes.Equal(cfg.CodecConfig, record) {
		t.Errorf("%s: read back %d channels at %d Hz, record % x; want %d, %d, % x",
			name, cfg.Channels, cfg.SampleRate, cfg.CodecConfig, channels, rate, record)
	}
	if tr := r.File().Tracks[0]; tr.Channels != channels || tr.SampleRate != rate {
		t.Errorf("%s: demuxed %d channels at %d Hz; want %d at %d", name, tr.Channels, tr.SampleRate, channels, rate)
	}
	if !samePayload(payload(t, r), frames) {
		t.Errorf("%s: the coded frames changed", name)
	}
}

// TestHEAACv1KeepsItsRatesInATransportStream is the HE-AAC (SBR, no PS) half:
// its ADTS header states the core's 22050 Hz, not the output's 44100 Hz that
// made ffprobe read the stream as 88200 Hz.
func TestHEAACv1KeepsItsRatesInATransportStream(t *testing.T) {
	data, err := os.ReadFile("testdata/ffmpeg-heaac.mp4")
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	var ts bytes.Buffer
	copyInto(t, NewTSMuxer(&ts), src)
	back, err := NewReader(ts.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := back.TrackConfig(back.TrackIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels != 2 || cfg.SampleRate != 22050 {
		t.Errorf("read back %d channels at %d Hz; want 2 at the core's 22050", cfg.Channels, cfg.SampleRate)
	}
}

// TestTSMuxerRefusesACoreNoADTSHeaderCanState keeps the refusals that stay: the
// header has no escape for a rate, no profile past LTP and no configuration
// past 7.
func TestTSMuxerRefusesACoreNoADTSHeaderCanState(t *testing.T) {
	for name, record := range map[string][]byte{
		"a rate in full":   {0x17, 0x80, 0x1f, 0x40, 0x10},
		"ER AAC LC":        bitsToBytes("10001 0011 0010 0"),
		"configuration 11": {0x11, 0xd8},
		// configuration 0: the layout is in the bitstream, which a header
		// cannot say either way and this muxer has no frame to read it from.
		"configuration 0": {0x12, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewTSMuxer(&bytes.Buffer{}).AddTrack(TrackConfig{
				Kind: Audio, Codec: "mp4a", Timescale: TSTimescale, SampleRate: 44100, Channels: 2, CodecConfig: record})
			if !errors.Is(err, ErrTrackConfig) {
				t.Errorf("AddTrack = %v; want ErrTrackConfig", err)
			}
		})
	}
}

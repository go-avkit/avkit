// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

// adtsFrameOf builds an ADTS frame: profile is the two-bit field (1 is
// AAC-LC), and a protected frame carries a two-byte CRC after its seven-byte
// header.
func adtsFrameOf(profile, freqIndex, channels byte, payload []byte, crc bool) []byte {
	header := 7
	if crc {
		header = 9
	}
	n := header + len(payload)
	b := []byte{0xFF, 0xF1, profile<<6 | freqIndex<<2 | channels>>2,
		channels&3<<6 | byte(n>>11), byte(n >> 3), byte(n&7)<<5 | 0x1F, 0xFC}
	if crc {
		b[1] = 0xF0
		b = append(b, 0, 0)
	}
	return append(b, payload...)
}

// aacFrame is an AAC-LC stereo frame at 44.1 kHz of 107 bytes.
func aacFrame() []byte {
	return adtsFrameOf(1, 4, 2, bytes.Repeat([]byte{0x11}, 100), false)
}

// id3v2Tag is an ID3v2.4 tag of size body bytes, with a footer if asked.
func id3v2Tag(body int, footer bool) []byte {
	flags := byte(0)
	if footer {
		flags = 0x10
	}
	b := []byte{'I', 'D', '3', 4, 0, flags,
		byte(body >> 21 & 0x7F), byte(body >> 14 & 0x7F), byte(body >> 7 & 0x7F), byte(body & 0x7F)}
	b = append(b, make([]byte, body)...)
	if footer {
		b = append(b, '3', 'D', 'I', 4, 0, flags, b[6], b[7], b[8], b[9])
	}
	return b
}

// apeTag is an APEv2 tag of items bytes, with a header if asked, and the size
// its footer states.
func apeTag(items int, header bool, size uint32) []byte {
	footer := make([]byte, apeFooterSize)
	copy(footer, "APETAGEX")
	binary.LittleEndian.PutUint32(footer[8:], 2000)
	binary.LittleEndian.PutUint32(footer[12:], size)
	var b []byte
	if header {
		binary.LittleEndian.PutUint32(footer[20:], 1<<31)
		b = append(b, make([]byte, apeFooterSize)...)
	}
	b = append(b, make([]byte, items)...)
	return append(b, footer...)
}

func TestAnID3v2TagIsMeasuredByItsSynchsafeSize(t *testing.T) {
	if n := id3v2Length(id3v2Tag(300, false)); n != 310 {
		t.Errorf("a tag of 300 bytes measures %d, want 310", n)
	}
	if n := id3v2Length(id3v2Tag(300, true)); n != 320 {
		t.Errorf("a tag with a footer measures %d, want 320", n)
	}
	broken := id3v2Tag(10, false)
	broken[7] = 0x80 // not synchsafe
	for name, b := range map[string][]byte{
		"short":            []byte("ID3"),
		"not a tag":        []byte("TAG0123456789"),
		"version 0xFF":     {'I', 'D', '3', 0xFF, 0, 0, 0, 0, 0, 0},
		"revision 0xFF":    {'I', 'D', '3', 4, 0xFF, 0, 0, 0, 0, 0},
		"not synchsafe":    broken,
		"an empty capture": nil,
	} {
		if n := id3v2Length(b); n != 0 {
			t.Errorf("%s: measured %d, want no tag", name, n)
		}
	}
}

func TestTheTagsAtTheEndAreLeftOutOfTheStream(t *testing.T) {
	audio := bytes.Repeat([]byte{1}, 500)
	id3v1 := append([]byte("TAG"), make([]byte, id3v1Size-3)...)
	for name, c := range map[string]struct {
		data []byte
		want int
	}{
		"nothing":                       {audio, 500},
		"ID3v1":                         {concat(audio, id3v1), 500},
		"APEv2 with a header":           {concat(audio, apeTag(40, true, 72)), 500},
		"APEv2 without, before ID3v1":   {concat(audio, apeTag(40, false, 72), id3v1), 500},
		"an APE size larger than all":   {concat(audio, apeTag(40, false, 1<<20)), 572},
		"an APE size smaller than one":  {concat(audio, apeTag(40, false, 8)), 572},
		"shorter than any tag":          {[]byte{1, 2}, 2},
		"an ID3v1 that is the whole of": {id3v1, 0},
	} {
		if got := tailTagsStart(c.data); got != c.want {
			t.Errorf("%s: the stream ends at %d, want %d", name, got, c.want)
		}
	}
}

// TestASyncFrameStreamIsReadPastWhatSurroundsItsFrames: a tag at the head, zero
// padding, a tag between two frames, rubbish between two others, and the tags
// at the tail. Every frame is found and nothing else is.
func TestASyncFrameStreamIsReadPastWhatSurroundsItsFrames(t *testing.T) {
	frame := aacFrame()
	// A sync pattern in the rubbish that is not followed by a frame, and one
	// that is two frames of ANOTHER stream -- 48 kHz -- which are not taken.
	// They chain, so only the stream lock keeps them out.
	other := adtsFrameOf(1, 3, 2, make([]byte, 40), false)
	rubbish := concat([]byte{1, 2, 0xFF, 0xF1, 3}, other, other, []byte{4, 5})
	data := concat(id3v2Tag(20, false), []byte{0, 0, 0}, frame, frame, id3v2Tag(5, true), frame,
		rubbish, frame, frame, apeTag(10, true, 42), append([]byte("TAG"), make([]byte, 125)...))
	if Sniff(data) != FormatADTS {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 5 {
		t.Fatalf("%d frames, want 5", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, frame[7:]) || s.Duration != 1024 {
			t.Errorf("frame %d: %d bytes lasting %d", i, len(s.Data), s.Duration)
		}
	}
	f := r.File()
	if f.Format != "adts" || f.Timescale != 44100 || f.Duration != 5*1024 {
		t.Errorf("file %+v", f)
	}
	// A tag straight after the first frame does not stop it being taken as
	// the start of the stream.
	tracks, err := readADTS(concat(frame, id3v2Tag(10, false), frame))
	if err != nil || len(tracks[0].samples) != 2 {
		t.Errorf("a tag after the first frame: %v", err)
	}
}

func TestAStreamCutInItsLastFrameKeepsTheFramesBefore(t *testing.T) {
	frame := aacFrame()
	data := concat(frame, frame, frame[:50])
	r, err := NewReader(data)
	if !errors.Is(err, ErrTruncated) || r == nil {
		t.Fatalf("NewReader: %v", err)
	}
	if !r.Truncated() {
		t.Error("the reader does not say it is truncated")
	}
	if samples, _ := r.Samples(1); len(samples) != 2 {
		t.Errorf("%d frames kept, want the 2 whole ones", len(samples))
	}
	// Demux reports what it read and the cut beside it.
	f, err := Demux(data)
	if !errors.Is(err, ErrTruncated) || f == nil || len(f.Tracks) != 1 {
		t.Errorf("Demux: %+v, %v", f, err)
	}
	// One frame cut short is all there is: nothing to keep, and the cut is
	// what is reported.
	if _, err := readADTS(frame[:50]); !errors.Is(err, ErrTruncated) {
		t.Errorf("a lone cut frame: %v", err)
	}
}

func TestRubbishIsNotASyncFrameStream(t *testing.T) {
	for name, data := range map[string][]byte{
		"no 0xFF at all":    bytes.Repeat([]byte{1, 2, 3}, 100),
		"one byte":          {0xFF},
		"a lone false sync": concat([]byte{0xFF, 0xF1}, bytes.Repeat([]byte{7}, 50)),
	} {
		if _, err := readADTS(data); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
}

func TestSniffingSyncFramesWantsAChain(t *testing.T) {
	frame := aacFrame()
	for name, c := range map[string]struct {
		data []byte
		want Format
	}{
		"three frames":                {concat(frame, frame, frame), FormatADTS},
		"one frame and the end":       {frame, FormatADTS},
		"the head of a longer file":   {frame[:50], FormatADTS},
		"a tag and zeros first":       {concat(id3v2Tag(10, false), []byte{0, 0}, frame, frame), FormatADTS},
		"a tag after the first frame": {concat(frame, id3v2Tag(10, false), frame), FormatADTS},
		"a tag running past the data": {id3v2Tag(4000, false)[:100], FormatUnknown},
		"a frame and then rubbish":    {concat(frame, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}), FormatUnknown},
		"nothing":                     {nil, FormatUnknown},
	} {
		if got := Sniff(c.data); got != c.want {
			t.Errorf("%s: sniffed %d, want %d", name, got, c.want)
		}
	}
}

func TestAnADTSStreamIsItsFramesWithoutTheirHeaders(t *testing.T) {
	payload := bytes.Repeat([]byte{0x21}, 50)
	protected := adtsFrameOf(1, 3, 2, payload, true)
	data := concat(id3v2Tag(4, false), protected, protected, adtsFrameOf(1, 3, 2, payload, false))
	if Sniff(data) != FormatADTS {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.TrackConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codec != "mp4a" || cfg.SampleRate != 48000 || cfg.Channels != 2 || cfg.AudioObjectType != 2 ||
		!bytes.Equal(cfg.CodecConfig, []byte{0x11, 0x90}) {
		t.Errorf("configuration %+v", cfg)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 3 {
		t.Fatalf("%d frames, want 3", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, payload) || s.Duration != 1024 {
			t.Errorf("frame %d: % x lasting %d", i, s.Data[:4], s.Duration)
		}
	}
}

// TestAnADTSStreamWithItsLayoutInsideIsReadAndNotDescribed: channel
// configuration 0 puts the layout in a program config element inside the
// frames, which no record built from the header can carry.
func TestAnADTSStreamWithItsLayoutInsideIsReadAndNotDescribed(t *testing.T) {
	frame := adtsFrameOf(1, 4, 0, make([]byte, 30), false)
	r, err := NewReader(concat(frame, frame, frame))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.TrackConfig(1); !errors.Is(err, ErrNoConfiguration) {
		t.Errorf("TrackConfig: %v, want ErrNoConfiguration", err)
	}
	if samples, err := r.Samples(1); err != nil || len(samples) != 3 {
		t.Errorf("Samples: %d, %v", len(samples), err)
	}
}

func TestWhatIsNotAnADTSFrameIsNotTakenForOne(t *testing.T) {
	good := adtsFrameOf(1, 4, 2, make([]byte, 30), false)
	for name, data := range map[string][]byte{
		"a reserved sampling frequency": adtsFrameOf(1, 13, 2, make([]byte, 30), false),
		"a header one byte further on":  concat([]byte{0x00}, good),
		"no sync at all":                bytes.Repeat([]byte{1}, 9),
		"a header and no payload":       concat(adtsFrameOf(1, 4, 2, nil, false), []byte{0, 0}),
	} {
		if adtsSyntax.length(data) != 0 {
			t.Errorf("%s: read as a frame", name)
		}
	}
}

func TestDurationsAreRescaledWithoutOverflowOrFloats(t *testing.T) {
	for _, c := range []struct {
		ticks    uint64
		from, to uint32
		want     uint64
	}{
		{100, 0, 1000, 100},
		{100, 1000, 1000, 100},
		{44100, 44100, microsecond, microsecond},
		{math.MaxUint64, 1, microsecond, math.MaxUint64},
		{1 << 62, 48000, 44100, (1 << 62) / 48000 * 44100},
	} {
		got := rescale(c.ticks, c.from, c.to)
		// The last case is exact to within the rounding of the expectation.
		if diff := int64(got - c.want); diff > 44100 || diff < -44100 {
			t.Errorf("rescale(%d, %d, %d) = %d, want %d", c.ticks, c.from, c.to, got, c.want)
		}
	}
	if scale, d := fileTiming(nil); scale != 0 || d != 0 {
		t.Errorf("no track: %d, %d", scale, d)
	}
	scale, d := fileTiming([]Track{
		{Timescale: 48000, Duration: 48000},
		{Timescale: 44100, Duration: 88200},
	})
	if scale != microsecond || d != 2*microsecond {
		t.Errorf("two clocks: %d ticks of 1/%d, want 2 s in microseconds", d, scale)
	}
}

// TestTheREADMEListsEveryFormatReadInOnePass: the containers table is where a
// reader looks for what a file can be, so a format registered without a row
// there fails here, and so does one whose demux column leaves out a codec read.
func TestTheREADMEListsEveryFormatReadInOnePass(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	containers, ok := sectionOf(string(readme), "### Containers")
	carried, ok2 := sectionOf(string(readme), "### Tracks a container can carry")
	if !ok || !ok2 {
		t.Fatal("the README has lost its containers or its tracks section")
	}
	rows := map[string][]string{
		"adts":      {"| ADTS (raw AAC) |", "| ADTS demux |", "`mp4a`"},
		"mpegaudio": {"| MPEG audio (MP3, MP2, MP1) |", "| MPEG audio demux |", "`mp1`", "`mp2`", "`mp3`"},
		"flac":      {"| native FLAC |", "| FLAC demux |", "`fLaC`"},
		"ogg":       {"| Ogg |", "| Ogg demux |", "`Opus`", "`vorb`"},
		"wav":       {"| WAV (RIFF, RF64, BW64) |", "| WAV demux |", "`ipcm`", "`fpcm`", "`raw `"},
	}
	for _, f := range elementaryFormats {
		want, listed := rows[f.name]
		if !listed {
			t.Errorf("%s is registered and this test does not know its README rows", f.name)
			continue
		}
		if !strings.Contains(containers, want[0]) {
			t.Errorf("the containers table has no row %q", want[0])
		}
		for _, w := range want[1:] {
			if !strings.Contains(carried, w) {
				t.Errorf("the tracks table does not carry %q for %s", w, f.name)
			}
		}
	}
}

// TestTheElementaryFormatsAreNamedWhereTheOthersAre: a format that cannot be
// read from disk says which one it is, and a stream whose reader fails outright
// is refused by Demux and NewReader alike.
func TestTheElementaryFormatsAreNamedWhereTheOthersAre(t *testing.T) {
	data, err := os.ReadFile("testdata/sine.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewFileReader(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, ErrUnsupportedFormat) || !strings.Contains(err.Error(), "wav") {
		t.Errorf("NewFileReader: %v", err)
	}
	for _, f := range elementaryFormats {
		if describeFormat(f.format) != f.name {
			t.Errorf("format %d is described as %q, want %q", f.format, describeFormat(f.format), f.name)
		}
	}
	noData := []byte("RIFF\x04\x00\x00\x00WAVE")
	if _, err := Demux(noData); !errors.Is(err, ErrMalformed) {
		t.Errorf("Demux: %v", err)
	}
	if _, err := NewReader(noData); !errors.Is(err, ErrMalformed) {
		t.Errorf("NewReader: %v", err)
	}
	if f, err := Demux(data); err != nil || f.Format != "wav" {
		t.Errorf("Demux: %+v, %v", f, err)
	}
	if _, ok := elementaryFor(FormatMP4); ok {
		t.Error("MP4 is not one of the formats read in one pass here")
	}
}

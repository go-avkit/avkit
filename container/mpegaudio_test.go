// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"
)

// mpegFrame is a whole frame behind header, its body bytes all fill.
func mpegFrame(t *testing.T, header []byte, fill byte) []byte {
	t.Helper()
	f, err := parseMPAFrame(header)
	if err != nil {
		t.Fatalf("header % x: %v", header, err)
	}
	return append(append([]byte(nil), header...), bytes.Repeat([]byte{fill}, f.FrameSize-len(header))...)
}

// mp3Frame is an MPEG-1 Layer III frame at 128 kbit/s and 44.1 kHz: 417 bytes.
func mp3Frame(t *testing.T) []byte {
	return mpegFrame(t, mpaHeader(1, 3, 9, 0, 0, 0), 0x11)
}

// TestAnMPEGStreamIsReadPastWhatSurroundsItsFrames: a tag at the head, zero
// padding, a tag between two frames, rubbish between two others -- among it a
// whole Layer II frame, which is another stream and is not taken -- and the
// tags at the tail.
func TestAnMPEGStreamIsReadPastWhatSurroundsItsFrames(t *testing.T) {
	frame := mp3Frame(t)
	rubbish := concat([]byte{1, 2, 0xFF, 0xFB, 3}, mpegFrame(t, mpaHeader(1, 2, 1, 0, 0, 0), 0x22), []byte{4, 5})
	data := concat(id3v2Tag(20, false), []byte{0, 0, 0}, frame, frame, id3v2Tag(5, true), frame,
		rubbish, frame, frame, apeTag(10, true, 42), append([]byte("TAG"), make([]byte, 125)...))
	if Sniff(data) != FormatMPEGAudio {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 5 {
		t.Fatalf("%d frames, want 5", len(samples))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, frame) || s.Duration != 1152 {
			t.Errorf("frame %d: %d bytes lasting %d", i, len(s.Data), s.Duration)
		}
	}
	f := r.File()
	if f.Format != "mpegaudio" || f.Timescale != 44100 || f.Duration != 5*1152 || f.Tracks[0].Codec != "mp3" {
		t.Errorf("file %+v", f)
	}
	// Cut in the last frame: the whole ones are kept and the cut reported.
	r, err = NewReader(concat(frame, frame, frame[:100]))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("a cut stream: %v", err)
	}
	if samples, _ := r.Samples(1); len(samples) != 2 {
		t.Errorf("%d frames kept of a cut stream, want 2", len(samples))
	}
}

// TestTheMPEG25RatesAreNotTakenForAStream: parseMPAFrame does not read them,
// so neither does this.
func TestTheMPEG25RatesAreNotTakenForAStream(t *testing.T) {
	header := []byte{0xFF, 0xE3, 0x18, 0xC0} // MPEG 2.5 Layer III, 8 kHz
	data := bytes.Repeat(concat(header, make([]byte, 68)), 3)
	if Sniff(data) == FormatMPEGAudio {
		t.Error("an MPEG 2.5 stream sniffs as MPEG audio")
	}
	if _, err := readMPEGAudio(data); !errors.Is(err, ErrMalformed) {
		t.Errorf("readMPEGAudio: %v", err)
	}
}

// xingFrame is a Layer III frame carrying a Xing or Info tag where its side
// information would be, with the optional fields flags asks for and, if
// encoder is set, the LAME extension stating delay.
func xingFrame(t *testing.T, header []byte, tag string, flags uint32, encoder string, delay int) []byte {
	t.Helper()
	frame := mpegFrame(t, header, 0)
	h, _ := parseMPAFrame(header)
	at := 4
	switch {
	case h.MPEG1 && h.Channels == 2:
		at += 32
	case h.MPEG1:
		at += 17
	case h.Channels == 2:
		at += 17
	default:
		at += 9
	}
	copy(frame[at:], tag)
	binary.BigEndian.PutUint32(frame[at+4:], flags)
	at += 8
	for _, f := range []struct {
		bit  uint32
		size int
	}{{1, 4}, {2, 4}, {4, 100}, {8, 4}} {
		if flags&f.bit != 0 {
			at += f.size
		}
	}
	if encoder != "" {
		copy(frame[at:], encoder)
		d := frame[at+21:]
		d[0], d[1] = byte(delay>>4), byte(delay<<4)
	}
	return frame
}

func TestTheInfoFrameIsMetadataAndStatesTheEncoderDelay(t *testing.T) {
	stereo1 := mpaHeader(1, 3, 9, 0, 0, 0)
	mono1 := mpaHeader(1, 3, 9, 0, 0, 3)
	stereo2 := mpaHeader(0, 3, 8, 0, 0, 0)
	mono2 := mpaHeader(0, 3, 8, 0, 0, 3)
	vbri := mpegFrame(t, stereo1, 0)
	copy(vbri[36:], "VBRI")
	for _, c := range []struct {
		name  string
		frame []byte
		info  bool
		delay time.Duration
	}{
		{"LAME, every field", xingFrame(t, stereo1, "Xing", 15, "LAME3.100", 576), true, 1105 * time.Second / 44100},
		{"Lavf, no field", xingFrame(t, mono1, "Info", 0, "Lavf63.1", 0), true, 529 * time.Second / 44100},
		{"Lavc at the low rates", xingFrame(t, stereo2, "Xing", 1, "Lavc", 100), true, 629 * time.Second / 22050},
		{"another encoder", xingFrame(t, mono2, "Info", 0, "GOGO", 50), true, 0},
		{"no extension", xingFrame(t, stereo1, "Xing", 0, "", 0), true, 0},
		{"Fraunhofer", vbri, true, 0},
		{"plain audio", mpegFrame(t, stereo1, 0), false, 0},
	} {
		h, _ := parseMPAFrame(c.frame)
		delay, info := mpegAudioInfoFrame(c.frame, h)
		if info != c.info || delay != c.delay {
			t.Errorf("%s: info %t delay %v, want %t %v", c.name, info, delay, c.info, c.delay)
		}
	}
	// A Xing frame whose extension runs off its end states no delay.
	short := xingFrame(t, stereo1, "Xing", 7, "", 0)
	h, _ := parseMPAFrame(short)
	if delay, info := mpegAudioInfoFrame(short[:150], h); !info || delay != 0 {
		t.Errorf("a cut extension: info %t delay %v", info, delay)
	}
	// A frame too small to hold the tag is audio: 24 bytes, the tag at 21.
	tiny := mpegFrame(t, mpaHeader(0, 3, 1, 1, 0, 0), 0)
	h, _ = parseMPAFrame(tiny)
	if _, info := mpegAudioInfoFrame(tiny, h); info {
		t.Error("a 24-byte frame read as an info frame")
	}
	// Only Layer III has one.
	layer2 := mpegFrame(t, mpaHeader(1, 2, 9, 0, 0, 0), 0)
	copy(layer2[36:], "Xing")
	h, _ = parseMPAFrame(layer2)
	if _, info := mpegAudioInfoFrame(layer2, h); info {
		t.Error("a Layer II frame read as an info frame")
	}
	// In a stream, the frame is dropped and the delay is the track's.
	data := concat(xingFrame(t, stereo1, "Info", 0, "LAME3.100", 576), mp3Frame(t), mp3Frame(t))
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	cfg, _ := r.TrackConfig(1)
	if len(samples) != 2 || cfg.StartDelay != 1105*time.Second/44100 {
		t.Errorf("%d samples and a delay of %v, want 2 and 25.06ms", len(samples), cfg.StartDelay)
	}
}

// TestAnMP3CarriesItsEncoderDelayAsAStartDelay: FFmpeg starts this file's first
// packet at -353604/14112000 s, which is LAME's stated delay of 576 samples
// plus the decoder's 529, at 44.1 kHz.
func TestAnMP3CarriesItsEncoderDelayAsAStartDelay(t *testing.T) {
	data, err := os.ReadFile("testdata/sine.mp3")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.TrackConfig(1)
	if want := time.Duration(1105) * time.Second / 44100; cfg.StartDelay != want {
		t.Errorf("start delay %v, want %v", cfg.StartDelay, want)
	}
}

func FuzzMPEGAudio(f *testing.F) {
	fuzzElementary(f, readMPEGAudio, seedFile(f, "sine.mp3"), seedFile(f, "sine.mp2"))
}

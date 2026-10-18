// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// riffChunk is a chunk with its pad byte.
func riffChunk(id string, body []byte) []byte {
	b := binary.LittleEndian.AppendUint32([]byte(id), uint32(len(body)))
	b = append(b, body...)
	if len(body)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// riffSized is a chunk whose size field states size, whatever its body.
func riffSized(id string, size uint32, body []byte) []byte {
	return append(binary.LittleEndian.AppendUint32([]byte(id), size), body...)
}

// waveFmt is a WAVEFORMATEX body, or a WAVEFORMATEXTENSIBLE one when sub is
// not zero.
func waveFmt(tag uint16, channels, rate, blockAlign, bits int, sub uint16) []byte {
	b := binary.LittleEndian.AppendUint16(nil, tag)
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*blockAlign))
	b = binary.LittleEndian.AppendUint16(b, uint16(blockAlign))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	if sub != 0 {
		b = binary.LittleEndian.AppendUint16(b, 22)
		b = binary.LittleEndian.AppendUint16(b, uint16(bits))
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint16(b, sub)
		b = append(b, waveSubFormatTail...)
	}
	return b
}

// riffWave is a RIFF WAVE form of chunks.
func riffWave(form string, chunks ...[]byte) []byte {
	body := concat(append([][]byte{[]byte("WAVE")}, chunks...)...)
	size := uint32(len(body))
	if form != "RIFF" {
		size = riffSizeUnknown
	}
	return append(binary.LittleEndian.AppendUint32([]byte(form), size), body...)
}

func TestAWAVFileIsItsPCMInPacketsOfATenthOfASecond(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2}, 1000) // 1000 frames of mono 16-bit
	data := riffWave("RIFF", riffChunk("LIST", []byte("odd")), riffChunk("fmt ", waveFmt(1, 1, 8000, 2, 16, 0)),
		riffChunk("data", pcm), riffChunk("id3 ", []byte{1}))
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	// 8000/10 = 800 rounds down to 512 frames a packet: 512 and 488.
	if len(samples) != 2 || samples[0].Duration != 512 || samples[1].Duration != 488 ||
		len(samples[0].Data) != 1024 {
		t.Fatalf("%d packets: %+v", len(samples), samples)
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Codec != "ipcm" || !bytes.Equal(cfg.CodecConfig, []byte{0, 0, 0, 0, 1, 16}) {
		t.Errorf("configuration %+v", cfg)
	}
}

func TestAWAVFileNamesItsPCMBySampleKind(t *testing.T) {
	for _, c := range []struct {
		name  string
		fmt   []byte
		codec string
		pcmC  []byte
	}{
		{"unsigned 8-bit", waveFmt(1, 2, 8000, 2, 8, 0), "raw ", nil},
		{"20 bits in 24", waveFmt(1, 1, 8000, 3, 20, 0), "ipcm", []byte{0, 0, 0, 0, 1, 24}},
		{"32-bit integer, extensible", waveFmt(waveFormatExtensible, 2, 8000, 8, 32, 1), "ipcm", []byte{0, 0, 0, 0, 1, 32}},
		{"64-bit float, extensible", waveFmt(waveFormatExtensible, 1, 8000, 8, 64, 3), "fpcm", []byte{0, 0, 0, 0, 1, 64}},
	} {
		r, err := NewReader(riffWave("RIFF", riffChunk("fmt ", c.fmt), riffChunk("data", make([]byte, 48))))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		cfg, err := r.TrackConfig(1)
		if err != nil || cfg.Codec != c.codec || !bytes.Equal(cfg.CodecConfig, c.pcmC) {
			t.Errorf("%s: %+v, %v", c.name, cfg, err)
		}
	}
}

// TestAWAVFileOfAnotherCodecIsReportedAndNotRead: the format tag names the
// codec, and the frames inside a WAVE wrapper are not guessed at.
func TestAWAVFileOfAnotherCodecIsReportedAndNotRead(t *testing.T) {
	for _, c := range []struct {
		fmt   []byte
		codec string
	}{
		{waveFmt(0x55, 2, 44100, 1, 0, 0), "audio/vnd.wave;codec=55"},
		{waveFmt(1, 1, 8000, 5, 40, 0), ""},
		{waveFmt(3, 1, 8000, 2, 16, 0), ""},
	} {
		r, err := NewReader(riffWave("RIFF", riffChunk("fmt ", c.fmt), riffChunk("data", make([]byte, 20))))
		if err != nil {
			t.Fatal(err)
		}
		if got := r.File().Tracks[0].Codec; got != c.codec {
			t.Errorf("codec %q, want %q", got, c.codec)
		}
		if _, err := r.TrackConfig(1); !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("TrackConfig: %v", err)
		}
		if _, err := r.Samples(1); !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("Samples: %v", err)
		}
	}
}

func TestRF64StatesItsDataSizeInDs64(t *testing.T) {
	ds64 := make([]byte, 28)
	binary.LittleEndian.PutUint64(ds64[8:], 6)
	data := riffWave("RF64", riffChunk("ds64", ds64), riffChunk("fmt ", waveFmt(1, 1, 8000, 2, 16, 0)),
		riffSized("data", riffSizeUnknown, make([]byte, 10)))
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if samples, _ := r.Samples(1); len(samples) != 1 || samples[0].Duration != 3 {
		t.Errorf("%+v, want the 3 frames ds64 states", samples)
	}
	short := riffWave("BW64", riffChunk("ds64", make([]byte, 8)))
	if _, err := NewReader(short); !errors.Is(err, ErrMalformed) {
		t.Errorf("a short ds64: %v", err)
	}
}

func TestAWAVDataSizeIsTakenAsTheWriterMeantIt(t *testing.T) {
	format := riffChunk("fmt ", waveFmt(1, 1, 8000, 2, 16, 0))
	for _, c := range []struct {
		name   string
		size   uint32
		body   int
		frames int
		cut    bool
	}{
		{"zero: a writer that could not seek back", 0, 20, 10, false},
		{"unknown in plain RIFF", riffSizeUnknown, 20, 10, false},
		{"larger than the file", 100, 20, 10, true},
		{"half a frame at the end", 21, 21, 10, true},
	} {
		r, err := NewReader(riffWave("RIFF", format, riffSized("data", c.size, make([]byte, c.body))))
		if c.cut != errors.Is(err, ErrTruncated) {
			t.Errorf("%s: %v", c.name, err)
		}
		if samples, _ := r.Samples(1); len(samples) != 1 || int(samples[0].Duration) != c.frames {
			t.Errorf("%s: %+v, want %d frames", c.name, samples, c.frames)
		}
	}
}

func TestAWAVFileThatBreaksItsFormIsRefused(t *testing.T) {
	format := riffChunk("fmt ", waveFmt(1, 1, 8000, 2, 16, 0))
	badGUID := waveFmt(waveFormatExtensible, 1, 8000, 2, 16, 1)
	badGUID[len(badGUID)-1] ^= 1
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"no fmt":                     {riffWave("RIFF", riffChunk("LIST", nil)), ErrMalformed},
		"no data":                    {riffWave("RIFF", format), ErrMalformed},
		"data first":                 {riffWave("RIFF", riffChunk("data", nil), format), ErrMalformed},
		"a fmt cut short":            {riffWave("RIFF", riffSized("fmt ", 16, make([]byte, 4))), ErrTruncated},
		"a fmt of 14 bytes":          {riffWave("RIFF", riffChunk("fmt ", make([]byte, 14))), ErrMalformed},
		"an extensible without GUID": {riffWave("RIFF", riffChunk("fmt ", badGUID)), ErrMalformed},
		"no channel":                 {riffWave("RIFF", riffChunk("fmt ", waveFmt(1, 0, 8000, 2, 16, 0))), ErrMalformed},
		"blocks that do not divide":  {riffWave("RIFF", riffChunk("fmt ", waveFmt(1, 2, 8000, 3, 8, 0)), riffChunk("data", nil)), ErrMalformed},
		"samples wider than blocks":  {riffWave("RIFF", riffChunk("fmt ", waveFmt(1, 1, 8000, 2, 24, 0)), riffChunk("data", nil)), ErrMalformed},
		"a chunk past the end":       {riffWave("RIFF", riffSized("JUNK", 1000, nil), format), ErrMalformed},
	} {
		r, err := NewReader(c.data)
		if err == nil && r != nil {
			_, err = r.TrackConfig(1)
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestTheThreeRIFFFormsOfWAVESniffAsWAV(t *testing.T) {
	for form, want := range map[string]Format{
		"RIFF....WAVE": FormatWAV, "RF64....WAVE": FormatWAV, "BW64....WAVE": FormatWAV,
		"RIFF....AVI ": FormatAVI, "RIFX....WAVE": FormatUnknown, "RIFF": FormatUnknown,
	} {
		if got := Sniff([]byte(form)); got != want {
			t.Errorf("%q sniffed as %d, want %d", form, got, want)
		}
	}
	if n := wavPacketFrames(4); n != 1 {
		t.Errorf("a packet at 4 Hz holds %d frames, want 1", n)
	}
	if !strings.HasSuffix(wavTagName(0x2000), "=2000") {
		t.Errorf("tag 0x2000 named %q", wavTagName(0x2000))
	}
}

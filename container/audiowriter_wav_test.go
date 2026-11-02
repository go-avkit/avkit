// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"
)

// awMem is an in-memory file a writer can seek in, which fails on demand.
type awMem struct {
	buf []byte
	pos int64

	seeks     int
	failSeek  int  // the n-th Seek fails (1-based); 0 never
	failPatch bool // a Write that overwrites fails
	limit     int  // appending past this many bytes fails; 0 never
}

var errAWSeek = errors.New("cannot seek")

func (m *awMem) Seek(offset int64, whence int) (int64, error) {
	m.seeks++
	if m.seeks == m.failSeek {
		return 0, errAWSeek
	}
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	}
	return m.pos, nil
}

func (m *awMem) Write(p []byte) (int, error) {
	if m.failPatch && m.pos < int64(len(m.buf)) {
		return 0, errAWFull
	}
	if m.limit > 0 && int(m.pos)+len(p) > m.limit {
		return 0, errAWFull
	}
	if end := int(m.pos) + len(p); end > len(m.buf) {
		m.buf = append(m.buf, make([]byte, end-len(m.buf))...)
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func awPCMC(bits byte, little bool) []byte {
	flags := byte(0)
	if little {
		flags = 1
	}
	return []byte{0, 0, 0, 0, flags, bits}
}

func awPCMConfig(codec string, channels, rate int, bits byte, little bool) TrackConfig {
	cfg := TrackConfig{Kind: Audio, Codec: codec, Timescale: uint32(rate), Channels: channels, SampleRate: rate}
	if codec != "raw " {
		cfg.CodecConfig = awPCMC(bits, little)
	}
	return cfg
}

// awWriteTo writes a track into a seekable file, or into a plain buffer.
func awWriteWAV(t *testing.T, cfg TrackConfig, data []byte, seekable bool, opts ...AudioOption) []byte {
	t.Helper()
	var w io.Writer
	var mem *awMem
	var buf bytes.Buffer
	if seekable {
		mem = &awMem{}
		w = mem
	} else {
		w = &buf
	}
	a := NewAudioWriter(FormatWAV, w, opts...)
	id, err := a.AddTrack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSample(id, Sample{Data: data, Duration: uint32(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if seekable {
		return mem.buf
	}
	return buf.Bytes()
}

func TestWAVConversionsAndFormats(t *testing.T) {
	le24 := []byte{1, 2, 3, 4, 5, 6}
	for name, c := range map[string]struct {
		cfg   TrackConfig
		in    []byte
		out   []byte // the data chunk's bytes
		codec string // as read back
		tag   uint16 // of the format chunk
		size  uint32 // of the format chunk
	}{
		"8-bit unsigned":       {awPCMConfig("raw ", 1, 8000, 0, true), []byte{0, 128, 255}, []byte{0, 128, 255}, "raw ", 1, 16},
		"8-bit signed to WAVE": {awPCMConfig("ipcm", 1, 8000, 8, true), []byte{0x80, 0, 0x7F}, []byte{0, 0x80, 0xFF}, "raw ", 1, 16},
		"16-bit":               {awPCMConfig("ipcm", 2, 44100, 16, true), []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, "ipcm", 1, 16},
		"16-bit big-endian":    {awPCMConfig("ipcm", 2, 44100, 16, false), []byte{1, 2, 3, 4}, []byte{2, 1, 4, 3}, "ipcm", 1, 16},
		"24-bit":               {awPCMConfig("ipcm", 1, 44100, 24, true), le24, le24, "ipcm", waveFormatExtensible, 40},
		"24-bit big-endian":    {awPCMConfig("ipcm", 1, 44100, 24, false), le24, []byte{3, 2, 1, 6, 5, 4}, "ipcm", waveFormatExtensible, 40},
		"32-bit":               {awPCMConfig("ipcm", 1, 44100, 32, true), []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, "ipcm", waveFormatExtensible, 40},
		"float":                {awPCMConfig("fpcm", 2, 44100, 32, true), make([]byte, 8), make([]byte, 8), "fpcm", waveFormatIEEEFloat, 18},
		"float big-endian":     {awPCMConfig("fpcm", 1, 44100, 64, false), []byte{1, 2, 3, 4, 5, 6, 7, 8}, []byte{8, 7, 6, 5, 4, 3, 2, 1}, "fpcm", waveFormatIEEEFloat, 18},
		"three channels":       {awPCMConfig("ipcm", 3, 44100, 16, true), make([]byte, 12), make([]byte, 12), "ipcm", waveFormatExtensible, 40},
		"above 48 kHz":         {awPCMConfig("ipcm", 2, 96000, 16, true), make([]byte, 8), make([]byte, 8), "ipcm", waveFormatExtensible, 40},
		"float, many channels": {awPCMConfig("fpcm", 6, 48000, 32, true), make([]byte, 24), make([]byte, 24), "fpcm", waveFormatExtensible, 40},
	} {
		for _, seekable := range []bool{false, true} {
			out := awWriteWAV(t, c.cfg, c.in, seekable)
			if got := binary.LittleEndian.Uint16(out[20:]); got != c.tag {
				t.Errorf("%s: format tag %#x, want %#x", name, got, c.tag)
			}
			if got := binary.LittleEndian.Uint32(out[16:]); got != c.size {
				t.Errorf("%s: format chunk of %d bytes, want %d", name, got, c.size)
			}
			cfg, samples := awReadBack(t, out)
			var data []byte
			for _, s := range samples {
				data = append(data, s.Data...)
			}
			// An odd 8-bit mono stream is padded in the file and not in the data.
			if !bytes.Equal(data, c.out) {
				t.Errorf("%s (seekable %v): data % x, want % x", name, seekable, data, c.out)
			}
			if cfg.Codec != c.codec || cfg.Channels != c.cfg.Channels || cfg.SampleRate != c.cfg.SampleRate {
				t.Errorf("%s: read back %s %d ch %d Hz", name, cfg.Codec, cfg.Channels, cfg.SampleRate)
			}
		}
	}
}

func TestWAVChannelMasks(t *testing.T) {
	want := map[int]uint32{3: 0xB, 4: 0x107, 5: 0x37, 6: 0x3F, 7: 0x70F, 8: 0x63F, 9: 0, 16: 0}
	for channels, mask := range want {
		out := awWriteWAV(t, awPCMConfig("ipcm", channels, 48000, 16, true), make([]byte, 2*channels), false)
		if got := binary.LittleEndian.Uint32(out[40:]); got != mask {
			t.Errorf("%d channels: mask %#x, want %#x", channels, got, mask)
		}
	}
}

func TestWAVSizes(t *testing.T) {
	cfg := awPCMConfig("fpcm", 2, 44100, 32, true)
	data := make([]byte, 80)

	t.Run("seekable", func(t *testing.T) {
		out := awWriteWAV(t, cfg, data, true)
		// RIFF, fmt (18 bytes), the room for ds64, fact, data.
		if string(out[:4]) != "RIFF" || binary.LittleEndian.Uint32(out[4:]) != uint32(len(out)-8) {
			t.Errorf("RIFF size %d, file %d", binary.LittleEndian.Uint32(out[4:]), len(out))
		}
		if string(out[38:42]) != "JUNK" || binary.LittleEndian.Uint32(out[42:]) != 28 {
			t.Errorf("no room for ds64: %q", out[38:46])
		}
		if string(out[74:78]) != "fact" || binary.LittleEndian.Uint32(out[82:]) != 10 {
			t.Errorf("fact: %q % x", out[74:78], out[82:86])
		}
		if string(out[86:90]) != "data" || binary.LittleEndian.Uint32(out[90:]) != 80 {
			t.Errorf("data size %d", binary.LittleEndian.Uint32(out[90:]))
		}
	})
	t.Run("not seekable", func(t *testing.T) {
		out := awWriteWAV(t, cfg, data, false)
		if binary.LittleEndian.Uint32(out[4:]) != riffSizeUnknown || bytes.Contains(out, []byte("fact")) ||
			bytes.Contains(out, []byte("JUNK")) {
			t.Errorf("header % x", out[:48])
		}
		if i := bytes.Index(out, []byte("data")); binary.LittleEndian.Uint32(out[i+4:]) != riffSizeUnknown {
			t.Error("a data size where none is known")
		}
		if _, samples := awReadBack(t, out); len(samples) != 1 || len(samples[0].Data) != 80 {
			t.Errorf("an unsized file reads back as %d samples", len(samples))
		}
	})
	t.Run("RF64 asked for", func(t *testing.T) {
		for _, seekable := range []bool{false, true} {
			out := awWriteWAV(t, cfg, data, seekable, WAVRF64())
			if string(out[:4]) != "RF64" || binary.LittleEndian.Uint32(out[4:]) != riffSizeUnknown {
				t.Fatalf("seekable %v: header %q", seekable, out[:12])
			}
			if i := bytes.Index(out, []byte("ds64")); i != 12 || binary.LittleEndian.Uint32(out[16:]) != 28 {
				t.Fatalf("ds64 at %d", i)
			}
			if _, samples := awReadBack(t, out); len(samples) != 1 || len(samples[0].Data) != 80 {
				t.Errorf("seekable %v: reads back as %d samples", seekable, len(samples))
			}
			if seekable {
				ds64 := out[20:]
				if binary.LittleEndian.Uint64(ds64) != uint64(len(out)-8) || binary.LittleEndian.Uint64(ds64[8:]) != 80 ||
					binary.LittleEndian.Uint64(ds64[16:]) != 10 {
					t.Errorf("ds64 sizes % x", ds64[:28])
				}
				if string(out[48:52]) != "fmt " {
					t.Errorf("the format chunk follows ds64: %q", out[48:52])
				}
				if i := bytes.Index(out, []byte("data")); binary.LittleEndian.Uint32(out[i+4:]) != riffSizeUnknown {
					t.Error("the data size of an RF64 file is in ds64")
				}
				if i := bytes.Index(out, []byte("fact")); binary.LittleEndian.Uint32(out[i+8:]) != riffSizeUnknown {
					t.Error("the fact count of an RF64 file is in ds64")
				}
			}
		}
	})
	t.Run("too large for RIFF", func(t *testing.T) {
		saved := wavRIFFMax
		wavRIFFMax = 100
		defer func() { wavRIFFMax = saved }()
		out := awWriteWAV(t, awPCMConfig("ipcm", 2, 44100, 16, true), data, true)
		if string(out[:4]) != "RF64" {
			t.Fatalf("a file of %d bytes is %q", len(out), out[:4])
		}
		if _, samples := awReadBack(t, out); len(samples) != 1 || len(samples[0].Data) != 80 {
			t.Errorf("reads back as %d samples", len(samples))
		}
	})
}

func TestWAVPadsAnOddDataChunk(t *testing.T) {
	out := awWriteWAV(t, awPCMConfig("raw ", 1, 8000, 0, true), []byte{1, 2, 3}, true)
	if len(out)%2 != 0 || out[len(out)-1] != 0 || binary.LittleEndian.Uint32(out[4:]) != uint32(len(out)-8) {
		t.Errorf("file of %d bytes", len(out))
	}
	i := bytes.Index(out, []byte("data"))
	if binary.LittleEndian.Uint32(out[i+4:]) != 3 {
		t.Errorf("the pad is counted: %d", binary.LittleEndian.Uint32(out[i+4:]))
	}
}

func TestWAVRefusals(t *testing.T) {
	add := func(cfg TrackConfig) error {
		_, err := NewAudioWriter(FormatWAV, io.Discard).AddTrack(cfg)
		return err
	}
	huge := int(uint64(math.MaxUint32) + 1) // zero on a 32-bit int, which is refused as well
	for name, cfg := range map[string]TrackConfig{
		"no pcmC":           {Kind: Audio, Codec: "ipcm", Channels: 2, SampleRate: 44100},
		"12-bit integers":   awPCMConfig("ipcm", 2, 44100, 12, true),
		"16-bit floats":     awPCMConfig("fpcm", 2, 44100, 16, true),
		"no channels":       awPCMConfig("ipcm", 0, 44100, 16, true),
		"frames too wide":   awPCMConfig("fpcm", 9000, 44100, 64, true),
		"no rate":           awPCMConfig("ipcm", 2, 0, 16, true),
		"a rate of 2^32 Hz": awPCMConfig("ipcm", 2, huge, 16, true),
	} {
		if err := add(cfg); !errors.Is(err, ErrTrackConfig) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	a := NewAudioWriter(FormatWAV, io.Discard)
	id, _ := a.AddTrack(awPCMConfig("ipcm", 2, 44100, 16, true))
	if err := a.WriteSample(id, Sample{Data: []byte{1, 2, 3}}); !errors.Is(err, ErrSample) {
		t.Errorf("three bytes of 4-byte frames: %v", err)
	}
}

func TestWAVReportsTheFileFailing(t *testing.T) {
	cfg := awPCMConfig("raw ", 1, 8000, 0, true)
	run := func(m *awMem) error {
		a := NewAudioWriter(FormatWAV, m)
		id, err := a.AddTrack(cfg)
		if err != nil {
			return err
		}
		if err := a.WriteSample(id, Sample{Data: []byte{1, 2, 3}}); err != nil {
			return err
		}
		return a.Close()
	}
	if err := run(&awMem{failSeek: 1}); !errors.Is(err, errAWSeek) {
		t.Errorf("the position cannot be read: %v", err)
	}
	if err := run(&awMem{failSeek: 2}); !errors.Is(err, errAWSeek) {
		t.Errorf("the first size cannot be written: %v", err)
	}
	if err := run(&awMem{failPatch: true}); !errors.Is(err, errAWFull) {
		t.Errorf("a size cannot be written: %v", err)
	}
	// The pad byte is the last append, and a file that cannot take it is not patched.
	if err := run(&awMem{limit: 12 + 24 + 36 + 8 + 3}); !errors.Is(err, errAWFull) {
		t.Errorf("the pad cannot be written: %v", err)
	}
}

func TestWAVStartsWhereTheWriterIs(t *testing.T) {
	mem := &awMem{buf: []byte("0123456789"), pos: 10}
	a := NewAudioWriter(FormatWAV, mem)
	id, err := a.AddTrack(awPCMConfig("ipcm", 2, 44100, 16, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSample(id, Sample{Data: make([]byte, 8)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if string(mem.buf[:10]) != "0123456789" || binary.LittleEndian.Uint32(mem.buf[14:]) != uint32(len(mem.buf)-10-8) {
		t.Errorf("sizes patched relative to the start of the file: %d", binary.LittleEndian.Uint32(mem.buf[14:]))
	}
}

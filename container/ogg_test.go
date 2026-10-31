// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// oggPageRaw is a page with its CRC, from the lacing values and body given.
func oggPageRaw(flags byte, granule int64, serial, seq uint32, lacing, body []byte) []byte {
	b := []byte("OggS\x00")
	b = append(b, flags)
	b = binary.LittleEndian.AppendUint64(b, uint64(granule))
	b = binary.LittleEndian.AppendUint32(b, serial)
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = append(b, 0, 0, 0, 0, byte(len(lacing)))
	b = append(b, lacing...)
	b = append(b, body...)
	binary.LittleEndian.PutUint32(b[22:], oggCRC(b))
	return b
}

// oggPageOf is a page holding whole packets.
func oggPageOf(flags byte, granule int64, serial, seq uint32, packets ...[]byte) []byte {
	var lacing, body []byte
	for _, p := range packets {
		n := len(p)
		for ; n >= 255; n -= 255 {
			lacing = append(lacing, 255)
		}
		lacing = append(lacing, byte(n))
		body = append(body, p...)
	}
	return oggPageRaw(flags, granule, serial, seq, lacing, body)
}

// opusHeadOf is an identification header of mapping family 0.
func opusHeadOf(channels byte, preSkip uint16, rate uint32) []byte {
	b := append([]byte(opusHeadMagic), 1, channels)
	b = binary.LittleEndian.AppendUint16(b, preSkip)
	b = binary.LittleEndian.AppendUint32(b, rate)
	return append(b, 0, 0, 0)
}

// celt20 is an Opus packet of one 20 ms CELT frame: 960 samples.
var celt20 = []byte{0xF8, 0xAA, 0xBB}

// TestAnOggFileIsItsStreamsPacketByPacket: two multiplexed streams, a Skeleton
// that describes them and is not one, a packet spanning two pages, and the end
// trimmed to the last granule.
func TestAnOggFileIsItsStreamsPacketByPacket(t *testing.T) {
	long := bytes.Repeat([]byte{0xF8}, 300) // spans a page boundary
	const opus, flac, skeleton = 0x0A, 0x0B, 0x0C
	info := streamInfo(8000, 1, 16, 0)
	flacFirst := concat([]byte("\x7fFLAC\x01\x00\x00\x01fLaC"), flacBlock(false, flacStreamInfo, info))
	frame := flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), []byte{1, 2, 3})
	data := concat(
		oggPageOf(oggBOS, 0, skeleton, 0, []byte("fishead\x00rest")),
		oggPageOf(oggBOS, 0, opus, 0, opusHeadOf(2, 312, 44100)),
		oggPageOf(oggBOS, 0, flac, 0, flacFirst),
		oggPageOf(0, 0, opus, 1, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00")),
		oggPageOf(0, 0, flac, 1, flacBlock(true, 4, []byte("comment"))),
		oggPageOf(0, 1920, opus, 2, celt20, celt20),
		// 300 bytes cut 255 + 45 over two pages.
		oggPageRaw(0, oggNoGranule, opus, 3, []byte{255}, long[:255]),
		oggPageRaw(oggContinued|oggEOS, 2500, opus, 4, []byte{45}, long[255:]),
		oggPageOf(oggEOS, 4096, flac, 2, frame),
		oggPageOf(oggEOS, 0, skeleton, 1),
	)
	if Sniff(data) != FormatOgg {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	f := r.File()
	if len(f.Tracks) != 2 || f.Tracks[0].Codec != "Opus" || f.Tracks[1].Codec != "fLaC" ||
		f.Tracks[0].ID != 1 || f.Tracks[1].ID != 2 || f.Timescale != microsecond {
		t.Fatalf("tracks %+v at 1/%d", f.Tracks, f.Timescale)
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.PreSkip != 312 || cfg.SampleRate != 44100 || cfg.Channels != 2 || cfg.Timescale != 48000 {
		t.Errorf("Opus configuration %+v", cfg)
	}
	samples, _ := r.Samples(1)
	// 960 + 960 + 300 frames of 960; the granule says 2500 in all, so the
	// last is cut from 288000 to 580.
	if len(samples) != 3 || !bytes.Equal(samples[2].Data, long) || samples[2].Duration != 580 {
		t.Errorf("Opus samples: %d, the last %d bytes lasting %d", len(samples), len(samples[2].Data), samples[2].Duration)
	}
	flacSamples, _ := r.Samples(2)
	if len(flacSamples) != 1 || flacSamples[0].Duration != 4096 {
		t.Errorf("FLAC samples: %+v", flacSamples)
	}
}

func TestAStreamThatStartsLaterIsNotTrimmedForWhatItLost(t *testing.T) {
	const s = 1
	data := concat(
		oggPageOf(oggBOS, 0, s, 0, opusHeadOf(1, 0, 0)),
		oggPageOf(0, 0, s, 1, []byte("OpusTags")),
		oggPageOf(0, 100960, s, 2, celt20),
		oggPageOf(oggEOS, 101920, s, 3, celt20),
	)
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 2 || samples[1].Duration != 960 {
		t.Errorf("%+v", samples)
	}
	if cfg, _ := r.TrackConfig(1); cfg.SampleRate != 48000 {
		t.Errorf("a stated rate of 0 reads as %d, want 48000 (RFC 7845 §5.1)", cfg.SampleRate)
	}
	// A last packet the granule leaves nothing of keeps one sample.
	data = concat(
		oggPageOf(oggBOS, 0, s, 0, opusHeadOf(1, 0, 48000)),
		oggPageOf(0, 0, s, 1, []byte("OpusTags")),
		oggPageOf(oggEOS, 900, s, 2, celt20, celt20),
	)
	r, _ = NewReader(data)
	samples, _ = r.Samples(1)
	if samples[1].Duration != 1 {
		t.Errorf("a packet trimmed to nothing lasts %d, want 1", samples[1].Duration)
	}
}

func TestAnOpusPacketStatesItsDurationInItsTOC(t *testing.T) {
	for _, c := range []struct {
		packet []byte
		want   int64
	}{
		{[]byte{0 << 3}, 480},              // SILK 10 ms
		{[]byte{3 << 3}, 2880},             // SILK 60 ms
		{[]byte{12 << 3}, 480},             // hybrid 10 ms
		{[]byte{13<<3 | 1}, 1920},          // hybrid 20 ms, two frames
		{[]byte{16<<3 | 2}, 240},           // CELT 2.5 ms, two frames
		{[]byte{19<<3 | 3, 0x03}, 2880},    // CELT 20 ms, three frames
		{[]byte{16<<3 | 3, 0x30, 0}, 5760}, // 48 frames of 2.5 ms: the limit
	} {
		got, err := opusPacketDuration(c.packet)
		if err != nil || got != c.want {
			t.Errorf("% x: %d, %v; want %d", c.packet, got, err, c.want)
		}
	}
	for name, p := range map[string][]byte{
		"empty":            nil,
		"code 3 alone":     {19<<3 | 3},
		"no frame":         {19<<3 | 3, 0},
		"more than 120 ms": {19<<3 | 3, 7},
	} {
		if _, err := opusPacketDuration(p); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// lsbBits writes fields least significant bit first, as Vorbis packs them.
type lsbBits struct {
	b []byte
	n int
}

func (w *lsbBits) put(v uint32, bits int) {
	for i := range bits {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>i&1) << (w.n % 8)
		w.n++
	}
}

// vorbisSetup is a setup header that ends in the given modes, behind filler
// standing in for the codebooks, floors and residues.
func vorbisSetup(long ...bool) []byte {
	w := lsbBits{b: append([]byte("\x05vorbis"), bytes.Repeat([]byte{0xFF}, 12)...), n: 19 * 8}
	w.put(uint32(len(long)-1), 6)
	for _, l := range long {
		flag := uint32(0)
		if l {
			flag = 1
		}
		w.put(flag, 1)
		w.put(0, 16)
		w.put(0, 16)
		w.put(0, 8)
	}
	w.put(1, 1) // framing
	return w.b
}

// vorbisIDOf is an identification header with short and long blocks of 256
// and 2048.
func vorbisIDOf(channels byte, rate uint32) []byte {
	b := append([]byte("\x01vorbis"), 0, 0, 0, 0, channels)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = append(b, make([]byte, 12)...)
	return append(b, 0xB8, 1)
}

var vorbisComment = []byte("\x03vorbis\x00\x00\x00\x00\x00\x00\x00\x00\x01")

func vorbisStream(serial uint32, setup []byte, audio ...[]byte) []byte {
	return concat(
		oggPageOf(oggBOS, 0, serial, 0, vorbisIDOf(2, 22050)),
		oggPageOf(0, 0, serial, 1, vorbisComment, setup),
		oggPageOf(0, 576, serial, 2, audio[0], audio[1]),
		oggPageOf(oggEOS, 2000, serial, 3, audio[2:]...),
	)
}

// TestAVorbisPacketReturnsAQuarterOfEachOfTwoBlocks, with the mode table read
// from the end of the setup header: mode 0 short, mode 1 long.
//
//	packet 0, short:                 returns nothing; given 256/4+256/4 = 128
//	packet 1, long after a short:    256/4 + 2048/4 =  576   granule  576
//	packet 2, long after a long:    2048/4 + 2048/4 = 1024
//	packet 3, short after a long:   2048/4 +  256/4 =  576   granule 2176, cut to 2000
func TestAVorbisPacketReturnsAQuarterOfEachOfTwoBlocks(t *testing.T) {
	setup := vorbisSetup(false, true)
	data := vorbisStream(7, setup, []byte{0x00}, []byte{0x02}, []byte{0x06}, []byte{0x00, 0x01})
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{128, 576, 1024, 400}
	for i, s := range samples {
		if s.Duration != want[i] {
			t.Errorf("packet %d lasts %d, want %d", i, s.Duration, want[i])
		}
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Codec != "vorb" || cfg.SampleRate != 22050 || cfg.Channels != 2 ||
		!bytes.Equal(cfg.CodecConfig, xiphLace(vorbisIDOf(2, 22050), vorbisComment, setup)) {
		t.Errorf("configuration %+v", cfg)
	}
	// One mode, a long one: no bit names the mode, and the previous-window
	// flag is the one after the packet type.
	data = vorbisStream(7, vorbisSetup(true), []byte{0x00}, []byte{0x00}, []byte{0x02}, []byte{0x02})
	r, _ = NewReader(data)
	samples, _ = r.Samples(1)
	if samples[1].Duration != 576 || samples[2].Duration != 1024 {
		t.Errorf("one long mode: %d and %d, want 576 and 1024", samples[1].Duration, samples[2].Duration)
	}
}

// TestTheVorbisHeadersAreLacedAsMatroskaLacesThem, a packet of 255 bytes or
// more included: its size takes a run of 255s and a remainder.
func TestTheVorbisHeadersAreLacedAsMatroskaLacesThem(t *testing.T) {
	packets := [][]byte{make([]byte, 600), make([]byte, 255), make([]byte, 3)}
	if got, want := xiphLaced(packets...), xiphLace(packets...); !bytes.Equal(got, want) {
		t.Errorf("laced as % x, want % x", got[:8], want[:8])
	}
}

// TestAMisreadModeTableIsCaughtByTheGranules: the heuristic that finds the
// modes is not normative, and the granule positions are.
func TestAMisreadModeTableIsCaughtByTheGranules(t *testing.T) {
	data := concat(
		oggPageOf(oggBOS, 0, 1, 0, vorbisIDOf(2, 22050)),
		oggPageOf(0, 0, 1, 1, vorbisComment, vorbisSetup(false, true)),
		oggPageOf(0, 576, 1, 2, []byte{0x00}, []byte{0x02}),
		oggPageOf(0, 999, 1, 3, []byte{0x06}),
		oggPageOf(oggEOS, 2000, 1, 4, []byte{0x00}),
	)
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMalformed) {
		t.Errorf("Samples: %v, want ErrMalformed", err)
	}
}

func TestAVorbisStreamThatBreaksItsSpecificationIsRefused(t *testing.T) {
	setup := vorbisSetup(false, true)
	audio := [][]byte{{0x00}, {0x02}, {0x06}}
	badID := func(mutate func([]byte)) []byte {
		b := vorbisIDOf(2, 22050)
		mutate(b)
		return b
	}
	many := make([]bool, 64)
	for name, data := range map[string][]byte{
		"two headers": concat(oggPageOf(oggBOS, 0, 1, 0, vorbisIDOf(2, 22050)),
			oggPageOf(oggEOS, 0, 1, 1, vorbisComment)),
		"a short identification header": concat(oggPageOf(oggBOS, 0, 1, 0, vorbisIDOf(2, 22050)[:20]),
			oggPageOf(oggEOS, 0, 1, 1, vorbisComment, setup)),
		"version 1": concat(oggPageOf(oggBOS, 0, 1, 0, badID(func(b []byte) { b[7] = 1 })),
			oggPageOf(oggEOS, 0, 1, 1, vorbisComment, setup)),
		"blocks of 32": concat(oggPageOf(oggBOS, 0, 1, 0, badID(func(b []byte) { b[28] = 0x55 })),
			oggPageOf(oggEOS, 0, 1, 1, vorbisComment, setup)),
		"no framing bit": concat(oggPageOf(oggBOS, 0, 1, 0, badID(func(b []byte) { b[29] = 0 })),
			oggPageOf(oggEOS, 0, 1, 1, vorbisComment, setup)),
		"headers out of order": concat(oggPageOf(oggBOS, 0, 1, 0, vorbisIDOf(2, 22050)),
			oggPageOf(oggEOS, 0, 1, 1, setup, vorbisComment)),
		"a setup header of zeros":  vorbisStream(1, append([]byte("\x05vorbis"), make([]byte, 20)...), audio...),
		"no mode table":            vorbisStream(1, append([]byte("\x05vorbis"), bytes.Repeat([]byte{0xFF}, 20)...), audio...),
		"64 modes":                 vorbisStream(1, vorbisSetup(many...), audio...),
		"a header among the audio": vorbisStream(1, setup, []byte{0x00}, []byte{0x01}, []byte{0x00}),
		"an empty packet":          vorbisStream(1, setup, []byte{0x00}, nil, []byte{0x00}),
		"a mode that is not there": vorbisStream(1, vorbisSetup(false, true, false), []byte{0x00}, []byte{0x06}, []byte{0x00}),
	} {
		r, err := NewReader(data)
		if err == nil {
			_, err = r.Samples(1)
		}
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
}

func TestAnOggStreamOfAnotherCodecIsNamedAndNotRead(t *testing.T) {
	for _, c := range []struct {
		first []byte
		codec string
		kind  Kind
	}{
		{[]byte("\x80theora\x03\x02"), "theora", Video},
		{[]byte("Speex   1.2"), "speex", Audio},
		{[]byte("\x00\x01mystery"), "", Other},
	} {
		r, err := NewReader(oggPageOf(oggBOS|oggEOS, 0, 9, 0, c.first))
		if err != nil {
			t.Fatal(err)
		}
		track := r.File().Tracks[0]
		if track.Codec != c.codec || track.Kind != c.kind {
			t.Errorf("% x: %q %s, want %q %s", c.first[:4], track.Codec, track.Kind, c.codec, c.kind)
		}
		if _, err := r.Samples(1); !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("Samples: %v", err)
		}
	}
}

func TestAnOggOpusOrFLACStreamThatBreaksItsMappingIsRefused(t *testing.T) {
	head := opusHeadOf(1, 312, 48000)
	tags := []byte("OpusTags")
	info := streamInfo(8000, 1, 16, 0)
	flacFirst := concat([]byte("\x7fFLAC\x01\x00\x00\x00fLaC"), flacBlock(true, flacStreamInfo, info))
	badRate := concat([]byte("\x7fFLAC\x01\x00\x00\x00fLaC"), flacBlock(true, flacStreamInfo, streamInfo(0, 1, 16, 0)))
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		// Version 16 is major version 1, which RFC 7845 §5.1 says a reader
		// of version 0 must refuse; 2 to 15 are minor versions of 0 and read
		// (#65).
		"an OpusHead of version 16": {oggPageOf(oggBOS|oggEOS, 0, 1, 0, append([]byte(opusHeadMagic), 16, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0)), ErrTrackConfig},
		"no OpusTags":               {concat(oggPageOf(oggBOS, 0, 1, 0, head), oggPageOf(oggEOS, 0, 1, 1, celt20)), ErrMalformed},
		"OpusHead alone":            {oggPageOf(oggBOS|oggEOS, 0, 1, 0, head), ErrMalformed},
		"an empty Opus packet":      {concat(oggPageOf(oggBOS, 0, 1, 0, head), oggPageOf(oggEOS, 0, 1, 1, tags, nil)), ErrMalformed},
		"a FLAC mapping version 2":  {oggPageOf(oggBOS|oggEOS, 0, 1, 0, concat([]byte("\x7fFLAC\x02"), flacFirst[6:])), ErrMalformed},
		"a FLAC sample rate of 0":   {oggPageOf(oggBOS|oggEOS, 0, 1, 0, badRate), ErrMalformed},
		"a FLAC packet that is not a frame": {concat(oggPageOf(oggBOS, 0, 1, 0, flacFirst),
			oggPageOf(oggEOS, 0, 1, 1, []byte{0xFF, 0x00})), ErrMalformed},
	} {
		r, err := NewReader(c.data)
		if err == nil {
			if _, err = r.Samples(1); err == nil {
				_, err = r.TrackConfig(1)
			}
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// A FLAC stream with no header count reads its metadata packets up to
	// the first frame, and an empty one among them is metadata too.
	frame := flacFrameBytes(flacHeaderBytes(false, 12, 4, 0), []byte{1})
	data := concat(oggPageOf(oggBOS, 0, 1, 0, flacFirst), oggPageOf(0, 0, 1, 1, nil, []byte{0x84, 0}),
		oggPageOf(oggEOS, 4096, 1, 2, frame))
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if samples, _ := r.Samples(1); len(samples) != 1 {
		t.Errorf("%d frames, want 1", len(samples))
	}
}

// TestAStreamWithNoWholePacketIsReportedNotIndexed: found by FuzzOgg, a
// beginning-of-stream page with no segment on it panicked on the first packet
// the codec was to be named by.
func TestAStreamWithNoWholePacketIsReportedNotIndexed(t *testing.T) {
	r, err := NewReader(oggPageRaw(oggBOS|oggEOS, 0, 3, 0, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrMalformed) {
		t.Errorf("Samples: %v, want ErrMalformed", err)
	}
}

func TestAnOggFileThatBreaksRFC3533IsRefused(t *testing.T) {
	head := opusHeadOf(1, 0, 48000)
	good := oggPageOf(oggBOS, 0, 1, 0, head)
	badCRC := append([]byte(nil), good...)
	badCRC[len(badCRC)-1] ^= 1
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"nothing":                  {nil, ErrMalformed},
		"a bad CRC":                {badCRC, ErrMalformed},
		"not a page":               {concat(good, []byte("OggT\x00 and some more bytes to make 27")), ErrMalformed},
		"a header cut":             {good[:20], ErrTruncated},
		"lacing cut":               {good[:27], ErrTruncated},
		"a body cut":               {good[:len(good)-1], ErrTruncated},
		"no beginning of stream":   {oggPageOf(0, 0, 1, 0, head), ErrMalformed},
		"a page after the end":     {concat(oggPageOf(oggBOS|oggEOS, 0, 1, 0, head), oggPageOf(0, 0, 1, 1, head)), ErrMalformed},
		"a lost page":              {concat(good, oggPageOf(0, 0, 1, 2, head)), ErrMalformed},
		"a continuation of none":   {concat(good, oggPageOf(oggContinued, 0, 1, 1, head)), ErrMalformed},
		"no continuation":          {concat(good, oggPageRaw(0, -1, 1, 1, []byte{255}, make([]byte, 255)), oggPageOf(0, 0, 1, 2, head)), ErrMalformed},
		"the end inside a packet":  {concat(good, oggPageRaw(oggEOS, -1, 1, 1, []byte{255}, make([]byte, 255))), ErrMalformed},
		"the file inside a packet": {concat(good, oggPageRaw(0, -1, 1, 1, []byte{255}, make([]byte, 255))), ErrTruncated},
	} {
		_, err := readOgg(c.data)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	for name, data := range map[string][]byte{"OggS version 1": []byte("OggS\x01"), "four bytes": []byte("OggS")} {
		if Sniff(data) == FormatOgg {
			t.Errorf("%s sniffed as Ogg", name)
		}
	}
}

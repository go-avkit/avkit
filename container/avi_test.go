// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The AVI fixtures in testdata/testsrc_*.avi were written by FFmpeg 9.0.2 from
// its lavfi testsrc and sine sources, and each one's .packets beside it is what
// FFprobe's demuxer hands over for it:
//
//	ffprobe -show_streams -show_packets -show_data_hash md5 \
//	  -show_entries stream=index,time_base:packet=stream_index,dts,duration,size,flags,data_hash \
//	  -of csv X.avi
//
// which states, unlike framemd5, which packets are keyframes.

type probePacket struct {
	dts, size int64
	key       bool
	md5       string
}

type probe struct {
	tb      map[int]*big.Rat
	packets map[int][]probePacket
}

func readProbe(t *testing.T, path string) probe {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := probe{tb: map[int]*big.Rat{}, packets: map[int][]probePacket{}}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		switch f[0] {
		case "stream":
			i, _ := strconv.Atoi(f[1])
			tb, _ := new(big.Rat).SetString(f[2])
			p.tb[i] = tb
		case "packet":
			i, _ := strconv.Atoi(f[1])
			dts, _ := strconv.ParseInt(f[2], 10, 64)
			size, _ := strconv.ParseInt(f[4], 10, 64)
			p.packets[i] = append(p.packets[i], probePacket{dts, size, strings.HasPrefix(f[5], "K"),
				strings.TrimPrefix(f[6], "MD5:")})
		}
	}
	return p
}

// TestAnAVIFileReadsAsFFmpegDemuxesIt compares every sample of every AVI
// fixture with FFmpeg's packet: when it starts, how big it is, its bytes, and
// whether it is a keyframe.
//
// Two differences are expected and checked as such. H.264 samples are
// length-prefixed where AVI carries start codes, and the parameter sets are
// lifted into the configuration, so only their sizes compare, and the first
// one's less what was lifted. PCM is cut into packets of its own (as WAV is),
// where FFmpeg hands over the chunks the writer chose, so the whole stream's
// bytes and length are what compare.
func TestAnAVIFileReadsAsFFmpegDemuxesIt(t *testing.T) {
	for _, c := range []struct {
		file   string
		codecs []string
	}{
		{"testsrc_xvid_mp3.avi", []string{"mp4v", "mp3"}},
		{"testsrc_h264_ac3.avi", []string{"avc1", "ac-3"}},
		{"testsrc_mjpeg_pcm.avi", []string{"mjpg", "ipcm"}},
	} {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			oracle := readProbe(t, "testdata/"+c.file+".packets")
			r, err := NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			if r.File().Format != "avi" || len(r.File().Tracks) != len(c.codecs) {
				t.Fatalf("%+v", r.File())
			}
			for i, track := range r.File().Tracks {
				if track.Codec != c.codecs[i] {
					t.Errorf("track %d is %q, want %q", i, track.Codec, c.codecs[i])
				}
				samples, err := r.Samples(track.ID)
				if err != nil {
					t.Fatalf("track %d: %v", i, err)
				}
				compareWithProbe(t, track, samples, oracle.packets[i], oracle.tb[i])
			}
		})
	}
}

func compareWithProbe(t *testing.T, track Track, samples []Sample, packets []probePacket, tb *big.Rat) {
	t.Helper()
	if track.Codec == "ipcm" {
		var ours, theirs int64
		for _, s := range samples {
			ours += int64(len(s.Data))
		}
		for _, p := range packets {
			theirs += p.size
		}
		if ours != theirs || int64(track.Duration)*int64(2) != ours {
			t.Errorf("PCM: %d bytes over %d frames, FFmpeg %d bytes", ours, track.Duration, theirs)
		}
		return
	}
	if len(samples) != len(packets) {
		t.Fatalf("%s: %d samples, FFmpeg %d packets", track.Codec, len(samples), len(packets))
	}
	var at int64
	for j, s := range samples {
		p := packets[j]
		start := big.NewRat(at, int64(track.Timescale))
		if want := new(big.Rat).Mul(big.NewRat(p.dts, 1), tb); start.Cmp(want) != 0 {
			t.Errorf("%s sample %d starts at %s s, FFmpeg %s s", track.Codec, j, start.FloatString(4), want.FloatString(4))
		}
		at += int64(s.Duration)
		if s.Sync != p.key {
			t.Errorf("%s sample %d sync %t, FFmpeg's key flag %t", track.Codec, j, s.Sync, p.key)
		}
		sum := md5.Sum(s.Data)
		switch {
		case track.Codec == "avc1" && p.key:
			// x264 repeats its parameter sets before every IDR picture.
			if int64(len(s.Data)) >= p.size {
				t.Errorf("avc1 sample %d: %d bytes, not less than FFmpeg's %d with its parameter sets", j, len(s.Data), p.size)
			}
		case track.Codec == "avc1":
			if int64(len(s.Data)) != p.size {
				t.Errorf("avc1 sample %d: %d bytes, FFmpeg %d", j, len(s.Data), p.size)
			}
		case hex.EncodeToString(sum[:]) != p.md5:
			t.Errorf("%s sample %d: %x, FFmpeg %s", track.Codec, j, sum, p.md5)
		}
	}
}

// TestAnAVIOfH264AndAC3RemuxesIntoAnMP4: the two codecs the MP4 muxer writes.
func TestAnAVIOfH264AndAC3RemuxesIntoAnMP4(t *testing.T) {
	data, err := os.ReadFile("testdata/testsrc_h264_ac3.avi")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Remux(&out, r); err != nil {
		t.Fatal(err)
	}
	back, err := NewReader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range back.TrackIDs() {
		in, _ := r.Samples(uint32(i + 1))
		got, _ := back.Samples(id)
		if len(got) != len(in) {
			t.Errorf("track %d: %d samples in, %d back", i, len(in), len(got))
		}
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Width != 32 || cfg.Height != 24 || len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Errorf("H.264 configuration %+v", cfg)
	}
}

// aviChunk is a RIFF chunk, padded.
func aviChunk(id string, parts ...[]byte) []byte { return riffChunk(id, concat(parts...)) }

// aviList is a LIST of the given type.
func aviList(kind string, chunks ...[]byte) []byte {
	return aviChunk("LIST", append([][]byte{[]byte(kind)}, chunks...)...)
}

// aviStrh is a stream header of the given type and rate/scale.
func aviStrh(kind string, rate, scale uint32) []byte {
	b := make([]byte, 56)
	copy(b, kind)
	binary.LittleEndian.PutUint32(b[20:], scale)
	binary.LittleEndian.PutUint32(b[24:], rate)
	return aviChunk("strh", b)
}

// bitmapInfo is a BITMAPINFOHEADER with extra bytes after it.
func bitmapInfo(fourcc string, width, height int32, extra ...byte) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b, 40)
	binary.LittleEndian.PutUint32(b[4:], uint32(width))
	binary.LittleEndian.PutUint32(b[8:], uint32(height))
	copy(b[16:], fourcc)
	return append(b, extra...)
}

// aviFile is a RIFF AVI form of a header list of strls and a movie list.
func aviFile(strls [][]byte, movi ...[]byte) []byte {
	hdrl := aviList("hdrl", append([][]byte{aviChunk("avih", make([]byte, 56))}, strls...)...)
	body := concat([]byte("AVI "), hdrl, aviList("movi", movi...))
	return riffChunk("RIFF", body)
}

// mpeg4Frame is a frame of MPEG-4 Part 2: a VOL header if asked, then a VOP of
// the given coding type.
func mpeg4Frame(vol bool, vopType byte) []byte {
	var b []byte
	if vol {
		b = append(b, 0, 0, 1, 0xB0, 1, 0, 0, 1, 0x20, 0x08)
	}
	return append(b, 0, 0, 1, 0xB6, vopType<<6, 0x55, 0x66)
}

func TestAnAVIStreamIsItsChunksInFileOrder(t *testing.T) {
	video := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("XVID", 32, -24)), aviChunk("strn", []byte("x")))
	i, p := mpeg4Frame(true, mpeg4IVOP), mpeg4Frame(false, 1)
	data := concat(aviFile([][]byte{video},
		aviChunk("00dc"), // an empty chunk before any frame is passed over
		aviChunk("00dc", i),
		aviChunk("00dc"), // a repeated frame lengthens the one before
		aviList("rec ", aviChunk("00dc", p), aviChunk("00pc", []byte{1, 2})),
		aviList("JUNK"),
		aviChunk("ix00", make([]byte, 24)),
		aviChunk("07dc", []byte{1}), // no stream 7
	),
		// An OpenDML continuation, and a form of another kind that is not.
		riffChunk("RIFF", concat([]byte("AVIX"), aviList("movi", aviChunk("00dc", p)))),
		riffChunk("RIFF", concat([]byte("WAVE"), aviList("movi", aviChunk("00dc", p)))),
		aviChunk("JUNK", []byte{0}),
	)
	if Sniff(data) != FormatAVI {
		t.Fatalf("sniffed as %d", Sniff(data))
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.Samples(1)
	if len(samples) != 3 || samples[0].Duration != 2 || !samples[0].Sync || samples[1].Sync {
		t.Fatalf("%+v", samples)
	}
	cfg, _ := r.TrackConfig(1)
	if cfg.Codec != "mp4v" || cfg.Width != 32 || cfg.Height != 24 || cfg.Timescale != 25 ||
		!bytes.Equal(cfg.CodecConfig, i[:10]) {
		t.Errorf("configuration %+v", cfg)
	}
	// The stream format's own decoder information wins over the first frame's.
	video = aviList("strl", aviStrh("vids", 30000, 1001), aviChunk("strf", bitmapInfo("DIVX", 32, 24, 7, 7)))
	r, _ = NewReader(aviFile([][]byte{video}, aviChunk("00dc", mpeg4Frame(false, 0))))
	if cfg, _ := r.TrackConfig(1); !bytes.Equal(cfg.CodecConfig, []byte{7, 7}) || cfg.Timescale != 30000 {
		t.Errorf("configuration %+v", cfg)
	}
	// And a frame that holds no VOP at all is not a keyframe.
	r, _ = NewReader(aviFile([][]byte{video}, aviChunk("00dc", []byte{1, 2, 3})))
	if s, _ := r.Samples(1); s[0].Sync {
		t.Error("a frame with no VOP was taken for a keyframe")
	}
}

func TestAnAVIStreamOfReorderedPicturesIsRefused(t *testing.T) {
	mp4v := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("XVID", 32, 24)))
	r, err := NewReader(aviFile([][]byte{mp4v}, aviChunk("00dc", mpeg4Frame(true, 0)), aviChunk("00dc", mpeg4Frame(false, mpeg4BVOP))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Samples(1); !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "B-VOP") {
		t.Errorf("an MPEG-4 B-VOP: %v", err)
	}
	h264 := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("H264", 32, 24)))
	// A non-IDR slice: first_mb_in_slice 0 (1), slice_type 1 (010).
	bSlice := []byte{0, 0, 0, 1, 0x01, 0xA0}
	r, _ = NewReader(aviFile([][]byte{h264}, aviChunk("00dc", bSlice)))
	if _, err := r.Samples(1); !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), "B-slice") {
		t.Errorf("an H.264 B-slice: %v", err)
	}
	// A P-slice reads, and with no parameter set anywhere the track says it
	// cannot be described.
	pSlice := []byte{0, 0, 0, 1, 0x01, 0xE0} // slice_type 0 (1)
	r, _ = NewReader(aviFile([][]byte{h264}, aviChunk("00dc", pSlice)))
	if s, err := r.Samples(1); err != nil || len(s) != 1 || s[0].Sync {
		t.Errorf("a P-slice: %+v, %v", s, err)
	}
	if _, err := r.TrackConfig(1); !errors.Is(err, ErrNoConfiguration) {
		t.Errorf("TrackConfig: %v", err)
	}
	withAVCC := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("AVC1", 32, 24, 1, 0x64)))
	r, _ = NewReader(aviFile([][]byte{withAVCC}, aviChunk("00dc", pSlice)))
	if _, err := r.Samples(1); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("H.264 with avcC: %v", err)
	}
}

func TestAnH264SliceTypeIsReadPastItsEscapes(t *testing.T) {
	for name, c := range map[string]struct {
		nalu []byte
		want bool
	}{
		"a B-slice":               {[]byte{0x01, 0xA0}, true},
		"an SB-slice, type 6":     {[]byte{0x01, 0x9C}, true},  // 1, 00111
		"an I-slice of an IDR":    {[]byte{0x05, 0xB0}, false}, // 1, 011: type 2
		"a parameter set":         {[]byte{0x67, 0xA0}, false},
		"one byte":                {[]byte{0x01}, false},
		"a field that never ends": {[]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00}, false},
	} {
		if got := avcBSlice(c.nalu); got != c.want {
			t.Errorf("%s: %t, want %t", name, got, c.want)
		}
	}
	if got := unescapeRBSP([]byte{0, 0, 3, 1, 0, 0, 3}); !bytes.Equal(got, []byte{0, 0, 1, 0, 0}) {
		t.Errorf("unescaped as % x", got)
	}
	r := expGolomb{data: []byte{0x00, 0x00, 0x00, 0x00, 0x01}} // 39 zeros
	if _, ok := r.ue(); ok {
		t.Error("a 39-bit prefix read as a number")
	}
	r = expGolomb{data: []byte{0x08}} // 0000 1 and then nothing
	if _, ok := r.ue(); ok {
		t.Error("a field cut short read as a number")
	}
}

// ac3Of is a whole AC-3 sync frame: 48 kHz, frame size code 8 (256 bytes).
func ac3Of(t *testing.T, acmod byte) []byte {
	h := ac3ClassicFrame(t, acmod, false, 0, 8)
	f, err := parseAC3Frame(h)
	if err != nil {
		t.Fatal(err)
	}
	return append(h, make([]byte, f.FrameSize-len(h))...)
}

func TestAnAVIAudioStreamIsReadAsItsCodecsOwnStream(t *testing.T) {
	ac3 := ac3Of(t, 2)
	aac := adtsFrameOf(1, 4, 2, make([]byte, 20), false)
	asc := []byte{0x12, 0x10} // AAC-LC, 44.1 kHz, stereo
	for _, c := range []struct {
		name    string
		format  []byte
		chunks  [][]byte
		codec   string
		samples int
	}{
		{"AC-3 cut across chunks", waveFmt(0x2000, 2, 48000, 1, 0, 0), [][]byte{ac3[:100], ac3[100:], ac3}, "ac-3", 2},
		{"ADTS AAC", waveFmt(0xFF, 2, 44100, 1, 0, 0), [][]byte{concat(aac, aac[:5]), aac[5:]}, "mp4a", 2},
		{"raw AAC, a frame a chunk", concat(waveFmt(0xFF, 2, 44100, 1, 16, 0), []byte{2, 0}, asc), [][]byte{{1, 2}, {3}}, "mp4a", 2},
		{"PCM", waveFmt(1, 1, 8000, 2, 16, 0), [][]byte{make([]byte, 6), make([]byte, 4)}, "ipcm", 1},
	} {
		strl := aviList("strl", aviStrh("auds", 1, 1), aviChunk("strf", c.format))
		var movi [][]byte
		for _, ch := range c.chunks {
			movi = append(movi, aviChunk("00wb", ch))
		}
		r, err := NewReader(aviFile([][]byte{strl}, movi...))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		samples, err := r.Samples(1)
		cfg, cerr := r.TrackConfig(1)
		if err != nil || cerr != nil || len(samples) != c.samples || cfg.Codec != c.codec {
			t.Errorf("%s: %d samples of %q, %v, %v", c.name, len(samples), cfg.Codec, err, cerr)
		}
	}
}

func TestAnAVIStreamThisDoesNotReadIsNamed(t *testing.T) {
	for _, c := range []struct {
		name string
		strl []byte
		kind Kind
		want error
	}{
		{"a subtitle stream", aviList("strl", aviStrh("txts", 1, 1)), Subtitle, ErrUnsupportedCodec},
		{"a data stream", aviList("strl", aviStrh("dats", 1, 1)), Other, ErrUnsupportedCodec},
		{"an unknown FourCC", aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("WMV3", 32, 24))), Video, ErrUnsupportedCodec},
		{"a short BITMAPINFOHEADER", aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", make([]byte, 20))), Video, ErrMalformed},
		{"no rate", aviList("strl", aviStrh("vids", 0, 1), aviChunk("strf", bitmapInfo("MJPG", 32, 24))), Video, ErrMalformed},
		{"an unknown audio tag", aviList("strl", aviStrh("auds", 1, 1), aviChunk("strf", waveFmt(0x0161, 2, 44100, 1, 0, 0))), Audio, ErrUnsupportedCodec},
		{"a short WAVEFORMAT", aviList("strl", aviStrh("auds", 1, 1), aviChunk("strf", make([]byte, 8))), Audio, ErrMalformed},
		{"MP3 with no frame", aviList("strl", aviStrh("auds", 1, 1), aviChunk("strf", waveFmt(0x55, 2, 44100, 1, 0, 0))), Audio, ErrMalformed},
		{"raw AAC with a broken config", aviList("strl", aviStrh("auds", 1, 1), aviChunk("strf",
			concat(waveFmt(0xFF, 2, 44100, 1, 16, 0), []byte{1, 0, 0xFF}))), Audio, ErrMalformed},
	} {
		r, err := NewReader(aviFile([][]byte{c.strl}, aviChunk("00wb", []byte{1, 2, 3})))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if k := r.File().Tracks[0].Kind; k != c.kind {
			t.Errorf("%s: kind %s, want %s", c.name, k, c.kind)
		}
		if _, err := r.Samples(1); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func TestAnAVIFileThatBreaksItsFormIsRefused(t *testing.T) {
	strl := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("MJPG", 32, 24)))
	whole := aviFile([][]byte{strl}, aviChunk("00dc", []byte{1, 2, 3, 4}))
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"not AVI first":          {riffChunk("RIFF", []byte("WAVE")), ErrMalformed},
		"no header list":         {riffChunk("RIFF", concat([]byte("AVI "), aviChunk("JUNK"))), ErrMalformed},
		"two header lists":       {riffChunk("RIFF", concat([]byte("AVI "), aviList("hdrl", strl), aviList("hdrl", strl))), ErrMalformed},
		"the movie first":        {riffChunk("RIFF", concat([]byte("AVI "), aviList("movi"), aviList("hdrl", strl))), ErrMalformed},
		"no stream list":         {riffChunk("RIFF", concat([]byte("AVI "), aviList("hdrl"))), ErrMalformed},
		"no stream header":       {riffChunk("RIFF", concat([]byte("AVI "), aviList("hdrl", aviList("strl")))), ErrMalformed},
		"a short stream header":  {riffChunk("RIFF", concat([]byte("AVI "), aviList("hdrl", aviList("strl", aviChunk("strh", make([]byte, 8)))))), ErrMalformed},
		"cut before the headers": {riffSized("RIFF", 4000, []byte("AVI LIST")), ErrTruncated},
		"cut in a frame":         {whole[:len(whole)-2], ErrTruncated},
	} {
		_, err := readAVI(c.data)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// What was read before the cut is kept.
	r, err := NewReader(whole[:len(whole)-2])
	if !errors.Is(err, ErrTruncated) {
		t.Fatal(err)
	}
	if s, _ := r.Samples(1); len(s) != 1 || len(s[0].Data) != 2 {
		t.Errorf("%+v", s)
	}
}

func TestAnAC3StreamIsItsSyncFrames(t *testing.T) {
	stereo, mono := ac3Of(t, 2), ac3Of(t, 1)
	tracks, err := readAC3(concat(stereo, mono, mono, stereo))
	if err != nil {
		t.Fatal(err)
	}
	// The mono frames are another stream, and chain, so only the stream lock
	// keeps them out.
	if len(tracks[0].samples) != 2 || tracks[0].config.Channels != 2 || tracks[0].config.SampleRate != 48000 {
		t.Errorf("%+v", tracks[0])
	}
	eac3 := eac3Frame(t, 2, false, 0, 3, 768)
	if _, err := readAC3(concat(eac3, eac3)); !errors.Is(err, ErrMalformed) {
		t.Errorf("Enhanced AC-3: %v", err)
	}
}

func FuzzAVI(f *testing.F) {
	strl := aviList("strl", aviStrh("vids", 25, 1), aviChunk("strf", bitmapInfo("XVID", 32, 24)))
	fuzzElementary(f, func(data []byte) ([]*mkvReadTrack, error) {
		if !sniffAVI(data) {
			return nil, nil
		}
		return readAVI(data)
	}, seedFile(f, "testsrc_h264_ac3.avi"), seedFile(f, "testsrc_xvid_mp3.avi"),
		aviFile([][]byte{strl}, aviChunk("00dc", mpeg4Frame(true, 0))))
}

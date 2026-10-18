// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The fixtures in testdata/sine.* were encoded by FFmpeg 9.0.2 from its lavfi
// sine source, and each one's .framemd5 beside it is what FFmpeg's own demuxer
// hands over for it:
//
//	ffmpeg -i sine.X -map 0 -c copy -f framemd5 sine.X.framemd5
//
// That is the oracle: the same packets, byte for byte, with the same durations.
// Where the two are expected to differ, the test says so and says why.

// framePacket is one line of a framemd5 file.
type framePacket struct {
	duration int64 // in the stream's time base
	size     int
	md5      string
}

// frameMD5 is a parsed framemd5 file: the time base and the packets of stream 0,
// and the MD5 of its extradata when it states one.
type frameMD5 struct {
	tbNum, tbDen int64
	extradataMD5 string
	packets      []framePacket
}

func readFrameMD5(t *testing.T, path string) frameMD5 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out frameMD5
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		switch {
		case strings.HasPrefix(line, "#tb 0: "):
			parts := strings.Split(strings.TrimPrefix(line, "#tb 0: "), "/")
			out.tbNum, _ = strconv.ParseInt(parts[0], 10, 64)
			out.tbDen, _ = strconv.ParseInt(parts[1], 10, 64)
		case strings.HasPrefix(line, "#extradata 0"):
			out.extradataMD5 = fields[len(fields)-1]
		case strings.HasPrefix(line, "#"):
		default:
			d, _ := strconv.ParseInt(fields[3], 10, 64)
			size, _ := strconv.Atoi(fields[4])
			// FFmpeg prints a negative duration as its 32-bit pattern.
			out.packets = append(out.packets, framePacket{int64(int32(uint32(d))), size, fields[5]})
		}
	}
	return out
}

// TestElementaryStreamsReadAsFFmpegDemuxesThem compares every packet of every
// fixture with what FFmpeg's demuxer hands over for the same file.
func TestElementaryStreamsReadAsFFmpegDemuxesThem(t *testing.T) {
	cases := []struct {
		file, format, codec string
		channels, rate      int
		// adtsHeaders: FFmpeg hands over ADTS frames with their headers on,
		// and strips them only when a muxer asks (aac_adtstoasc). These are
		// the frames without, so each is seven or nine bytes shorter.
		adtsHeaders bool
		// lastDiffers: see the Vorbis note below.
		lastDiffers bool
	}{
		{file: "sine.aac", format: "adts", codec: "mp4a", channels: 1, rate: 44100, adtsHeaders: true},
		{file: "sine.flac", format: "flac", codec: "fLaC", channels: 1, rate: 8000},
		{file: "sine.opus", format: "ogg", codec: "Opus", channels: 1, rate: 48000},
		{file: "sine.ogg", format: "ogg", codec: "vorb", channels: 2, rate: 22050, lastDiffers: true},
		{file: "sine.oga", format: "ogg", codec: "fLaC", channels: 1, rate: 8000},
		{file: "sine.wav", format: "wav", codec: "ipcm", channels: 1, rate: 8000},
		{file: "sine_u8.wav", format: "wav", codec: "raw ", channels: 1, rate: 8000},
		{file: "sine_f32.wav", format: "wav", codec: "fpcm", channels: 1, rate: 8000},
		{file: "sine_3ch.wav", format: "wav", codec: "ipcm", channels: 3, rate: 8000},
		{file: "sine_rf64.wav", format: "wav", codec: "ipcm", channels: 1, rate: 8000},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			oracle := readFrameMD5(t, "testdata/"+c.file+".framemd5")
			r, err := NewReader(data)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			f := r.File()
			if f.Format != c.format || len(f.Tracks) != 1 {
				t.Fatalf("format %q with %d tracks, want %q with 1", f.Format, len(f.Tracks), c.format)
			}
			track := f.Tracks[0]
			if track.Codec != c.codec || track.Channels != c.channels || track.SampleRate != c.rate || track.Kind != Audio {
				t.Errorf("track %+v, want %s %d ch at %d Hz", track, c.codec, c.channels, c.rate)
			}
			samples, err := r.Samples(track.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(samples) != len(oracle.packets) {
				t.Fatalf("%d samples, FFmpeg demuxes %d packets", len(samples), len(oracle.packets))
			}
			for i, s := range samples {
				p := oracle.packets[i]
				sum := md5.Sum(s.Data)
				switch {
				case c.adtsHeaders:
					if d := p.size - len(s.Data); d != 7 && d != 9 {
						t.Errorf("packet %d: %d bytes, FFmpeg's with its header %d", i, len(s.Data), p.size)
					}
				case hex.EncodeToString(sum[:]) != p.md5 || len(s.Data) != p.size:
					t.Errorf("packet %d: %d bytes %x, FFmpeg %d bytes %s", i, len(s.Data), sum, p.size, p.md5)
				}
				ours := big.NewRat(int64(s.Duration), int64(track.Timescale))
				theirs := big.NewRat(p.duration*oracle.tbNum, oracle.tbDen)
				if ours.Cmp(theirs) != 0 && !(c.lastDiffers && i == len(samples)-1) {
					t.Errorf("packet %d lasts %s s, FFmpeg %s s", i, ours.FloatString(6), theirs.FloatString(6))
				}
				if !s.Sync {
					t.Errorf("packet %d is not a sync sample; every audio frame here is", i)
				}
			}
		})
	}
}

// TestTheLastVorbisPacketEndsWhereTheGranuleSays.
//
// ⛔ Here FFmpeg is NOT the oracle. Its oggparsevorbis.c computes the last
// packet's trim against a timeline it has shifted by the first packet, and
// hands over a duration of -192 for this fixture (4294967104 in the framemd5).
// The normative answer is the last page's granule position, 11072 samples
// (`ffprobe -show_entries stream=duration_ts`), which counts what the packets
// after the first one return (Vorbis I §A.2): so the steps of every packet but
// the first have to add up to it.
func TestTheLastVorbisPacketEndsWhereTheGranuleSays(t *testing.T) {
	data, err := os.ReadFile("testdata/sine.ogg")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(data)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := r.Samples(1)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, s := range samples[1:] {
		sum += int64(s.Duration)
	}
	if sum != 11072 {
		t.Errorf("the packets after the first return %d samples; the final granule says 11072", sum)
	}
	if last := samples[len(samples)-1].Duration; last != 832 {
		t.Errorf("the last packet lasts %d, want 1024 cut by 192 to 832", last)
	}
}

// TestTheConfigurationIsWhatFFmpegCallsExtradata: the identification header
// of Opus travels as it stands, and FLAC's STREAMINFO is the same 34 bytes
// whichever wrapper carried it.
func TestTheConfigurationIsWhatFFmpegCallsExtradata(t *testing.T) {
	for _, c := range []struct {
		file string
		skip int // bytes of CodecConfig before what FFmpeg calls extradata
	}{
		{"sine.opus", 0},
		{"sine.flac", 8}, // "fLaC" and the block header
		{"sine.oga", 8},
	} {
		data, err := os.ReadFile("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := r.TrackConfig(1)
		if err != nil {
			t.Fatal(err)
		}
		sum := md5.Sum(cfg.CodecConfig[c.skip:])
		if want := readFrameMD5(t, "testdata/"+c.file+".framemd5").extradataMD5; hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s: configuration %x, FFmpeg's extradata %s", c.file, sum, want)
		}
	}
}

// TestAnOggOpusTrackRemuxesIntoMP4AndWebM, and an ADTS one into both: these
// are the codecs whose sample entries the muxers write today. The others are
// refused by the muxers, by name, which TestTracksTheMuxersCannotWriteAreRefusedByName
// holds.
func TestAnOggOpusTrackRemuxesIntoMP4AndWebM(t *testing.T) {
	for _, file := range []string{"sine.opus", "sine.aac", "sine.ogg"} {
		data, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := r.Samples(1)
		var mp4Out bytes.Buffer
		err = Remux(&mp4Out, r)
		if file == "sine.ogg" {
			// Vorbis has no ISO-BMFF sample entry registered with MP4RA.
			if !errors.Is(err, ErrUnsupportedCodec) {
				t.Errorf("%s into MP4: %v, want ErrUnsupportedCodec", file, err)
			}
		} else {
			if err != nil {
				t.Fatalf("%s into MP4: %v", file, err)
			}
			back, err := NewReader(mp4Out.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			out, _ := back.Samples(back.TrackIDs()[0])
			if len(out) != len(in) {
				t.Errorf("%s: %d samples in, %d back out of the MP4", file, len(in), len(out))
			}
		}
		cfg, _ := r.TrackConfig(1)
		var webm bytes.Buffer
		m := NewWebMMuxer(&webm)
		id, err := m.AddTrack(cfg)
		if err != nil {
			t.Fatalf("%s into WebM: %v", file, err)
		}
		for _, s := range in {
			if err := m.WriteSample(id, s); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		back, err := NewReader(webm.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		out, _ := back.Samples(back.TrackIDs()[0])
		if len(out) != len(in) {
			t.Errorf("%s: %d samples in, %d back out of the WebM", file, len(in), len(out))
		}
	}
}

// TestTracksTheMuxersCannotWriteAreRefusedByName: PCM has ISO/IEC 23003-5
// entries that mp4ff does not write, and FLAC is lot L2's.
func TestTracksTheMuxersCannotWriteAreRefusedByName(t *testing.T) {
	for _, file := range []string{"sine.wav", "sine_u8.wav", "sine_f32.wav", "sine.flac"} {
		data, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := r.TrackConfig(1)
		var out bytes.Buffer
		err = Remux(&out, r)
		if !errors.Is(err, ErrUnsupportedCodec) || !strings.Contains(err.Error(), strings.TrimSpace(cfg.Codec)) {
			t.Errorf("%s: %v, want ErrUnsupportedCodec naming %q", file, err, cfg.Codec)
		}
	}
	err := describe(nil, TrackConfig{Codec: "ipcm"})
	if !strings.Contains(err.Error(), "23003-5") {
		t.Errorf("PCM is refused as %v, without saying which specification it is waiting on", err)
	}
}

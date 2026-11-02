// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The oracle for the audio-only writers is FFmpeg's own: the same source is
// stream-copied by `ffmpeg -c copy` into the same format, and the two outputs
// must agree on what an independent demuxer reads out of them -- every packet's
// size, timestamps and checksum (framemd5), the duration, rate and channel
// count ffprobe reports, and the PCM a decoder produces.
//
// The tools are the Homebrew FFmpeg 9.0.2 this was written against; without
// them the tests skip, which is what a CI runner does.
const (
	oracleFFmpeg  = "/opt/homebrew/bin/ffmpeg"
	oracleFFprobe = "/opt/homebrew/bin/ffprobe"
)

func oracleTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{oracleFFmpeg, oracleFFprobe} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("oracle %s not installed", tool)
		}
	}
}

func oracleRun(t *testing.T, tool string, args ...string) string {
	t.Helper()
	cmd := exec.Command(tool, args...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, errs.String())
	}
	return out.String()
}

// oracleFrames is the framemd5 listing of a file's first audio stream, without
// the line naming the software that wrote it.
func oracleFrames(t *testing.T, path string) string {
	t.Helper()
	out := oracleRun(t, oracleFFmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-c", "copy", "-f", "framemd5", "-")
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "#software") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

type oracleProbe struct {
	Streams []struct {
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
		CodecName  string `json:"codec_name"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func oracleProbeOf(t *testing.T, path string) oracleProbe {
	t.Helper()
	out := oracleRun(t, oracleFFprobe, "-v", "error", "-select_streams", "a:0", "-show_entries",
		"stream=sample_rate,channels,codec_name:format=duration", "-of", "json", path)
	var p oracleProbe
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func oraclePCM(t *testing.T, path string) string {
	t.Helper()
	return oracleRun(t, oracleFFmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-f", "md5", "-")
}

type oracleCase struct {
	name   string
	src    string
	format Format
	muxer  string   // ffmpeg's -f
	flags  []string // ffmpeg's muxer options
	// coarse says the source states the end of its last packet in milliseconds.
	coarse bool
}

func TestAudioWritersMatchFFmpegStreamCopy(t *testing.T) {
	oracleTools(t)
	cases := []oracleCase{
		{name: "aac-lc mp4", src: "ffmpeg-aaclc.mp4", format: FormatADTS, muxer: "adts"},
		{name: "aac-lc mkv", src: "ffmpeg-aaclc.mkv", format: FormatADTS, muxer: "adts"},
		{name: "aac adts", src: "sine.aac", format: FormatADTS, muxer: "adts"},
		{name: "he-aac mp4", src: "ffmpeg-heaac.mp4", format: FormatADTS, muxer: "adts"},
		{name: "he-aac mkv", src: "ffmpeg-heaac.mkv", format: FormatADTS, muxer: "adts"},
		{name: "he-aacv2 m4a", src: "heaacv2.m4a", format: FormatADTS, muxer: "adts"},
		{name: "mp3 mp4", src: "ffmpeg-mp3.mp4", format: FormatMPEGAudio, muxer: "mp3", flags: []string{"-write_xing", "0", "-id3v2_version", "0"}},
		{name: "mp3 mkv", src: "ffmpeg-mp3.mkv", format: FormatMPEGAudio, muxer: "mp3", flags: []string{"-write_xing", "0", "-id3v2_version", "0"}},
		{name: "mp3 lsf", src: "ffmpeg-mp3_lsf.mp4", format: FormatMPEGAudio, muxer: "mp3", flags: []string{"-write_xing", "0", "-id3v2_version", "0"}},
		{name: "mp2 mp4", src: "ffmpeg-mp2.mp4", format: FormatMPEGAudio, muxer: "data"},
		{name: "mp2 mkv", src: "ffmpeg-mp2.mkv", format: FormatMPEGAudio, muxer: "mp2"},
		{name: "flac mp4", src: "ffmpeg-flac.mp4", format: FormatFLAC, muxer: "flac", flags: []string{"-write_header", "1"}},
		{name: "flac mkv", src: "ffmpeg-flac.mkv", format: FormatFLAC, muxer: "flac"},
		{name: "flac native", src: "sine.flac", format: FormatFLAC, muxer: "flac"},
		{name: "opus mp4", src: "ffmpeg-opus.mp4", format: FormatOgg, muxer: "ogg"},
		{name: "opus mkv", src: "ffmpeg-opus.mkv", format: FormatOgg, muxer: "ogg", coarse: true},
		{name: "opus ogg", src: "sine.opus", format: FormatOgg, muxer: "ogg"},
		{name: "vorbis mkv", src: "ffmpeg-vorbis.mkv", format: FormatOgg, muxer: "ogg"},
		{name: "vorbis ogg", src: "sine.oga", format: FormatOgg, muxer: "ogg"},
		{name: "flac mp4 to ogg", src: "ffmpeg-flac.mp4", format: FormatOgg, muxer: "ogg"},
		{name: "flac mkv to ogg", src: "ffmpeg-flac.mkv", format: FormatOgg, muxer: "ogg"},
		{name: "wav s16", src: "sine.wav", format: FormatWAV, muxer: "wav", flags: []string{"-rf64", "auto", "-fflags", "+bitexact"}},
		{name: "wav 3ch", src: "sine_3ch.wav", format: FormatWAV, muxer: "wav", flags: []string{"-rf64", "auto", "-fflags", "+bitexact"}},
		{name: "wav f32", src: "sine_f32.wav", format: FormatWAV, muxer: "wav", flags: []string{"-rf64", "auto", "-fflags", "+bitexact"}},
		{name: "wav u8", src: "sine_u8.wav", format: FormatWAV, muxer: "wav", flags: []string{"-rf64", "auto", "-fflags", "+bitexact"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := filepath.Join("testdata", c.src)
			data, err := os.ReadFile(src)
			if err != nil {
				t.Skipf("no fixture: %v", err)
			}
			r, err := NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			var drop []uint32
			for _, id := range r.TrackIDs() {
				if cfg, _ := r.TrackConfig(id); cfg.Kind != Audio {
					drop = append(drop, id)
				}
			}
			opts := []RemuxOption{AudioOutput(c.format)}
			if len(drop) > 0 {
				opts = append(opts, DropTracks(drop...))
			}
			dir := t.TempDir()
			ours := filepath.Join(dir, "ours."+c.muxer)
			f, err := os.Create(ours) // a file, so the writers can seek back as they do in life
			if err != nil {
				t.Fatal(err)
			}
			if err := Remux(f, r, opts...); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			theirs := filepath.Join(dir, "theirs."+c.muxer)
			args := []string{"-v", "error", "-y", "-i", src, "-map", "0:a:0", "-c", "copy"}
			args = append(append(args, c.flags...), "-f", c.muxer, theirs)
			oracleRun(t, oracleFFmpeg, args...)
			mine, _ := os.ReadFile(ours)

			if c.muxer == "data" {
				// FFmpeg's mp2 muxer refuses MPEG audio its MP4 demuxer named mp3;
				// the raw frames are what both would write.
				if want, _ := os.ReadFile(theirs); !bytes.Equal(mine, want) {
					t.Errorf("frames differ: ours %d bytes, ffmpeg %d", len(mine), len(want))
				}
				return
			}
			a, b := oracleFrames(t, ours), oracleFrames(t, theirs)
			if c.coarse {
				a, b = dropLastPacket(a), dropLastPacket(b)
			}
			if a != b {
				t.Errorf("packets differ at %s", firstDifference(a, b))
			}
			pa, pb := oracleProbeOf(t, ours), oracleProbeOf(t, theirs)
			if c.coarse {
				// Matroska's Sample.Duration is in milliseconds, and the end of the
				// last Opus packet is stated to the millisecond: see oggSink.
				if x, y := parseSeconds(t, pa), parseSeconds(t, pb); x-y > 0.0006 || y-x > 0.0006 {
					t.Errorf("duration %v against ffmpeg's %v", x, y)
				}
				pa.Format.Duration, pb.Format.Duration = "", ""
			}
			if !sameProbe(pa, pb) {
				t.Errorf("ffprobe differs: ours %+v, ffmpeg %+v", pa, pb)
			}
			if x, y := oraclePCM(t, ours), oraclePCM(t, theirs); x != y && !c.coarse {
				t.Errorf("decoded PCM differs: ours %s ffmpeg %s", x, y)
			}
			if c.format == FormatWAV {
				want, _ := os.ReadFile(theirs)
				if !bytes.Equal(mine, want) {
					t.Errorf("WAVE bytes differ: ours %d bytes, ffmpeg %d", len(mine), len(want))
				}
			}
		})
	}
}

// firstDifference names the first line two listings disagree on.
func firstDifference(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range min(len(x), len(y)) {
		if x[i] != y[i] {
			return fmt.Sprintf("line %d\nours:   %s\nffmpeg: %s", i, x[i], y[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(x), len(y))
}

func dropLastPacket(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

func parseSeconds(t *testing.T, p oracleProbe) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sameProbe(a, b oracleProbe) bool {
	if len(a.Streams) != len(b.Streams) || len(a.Streams) == 0 {
		return false
	}
	return a.Streams[0] == b.Streams[0] && a.Format.Duration == b.Format.Duration
}

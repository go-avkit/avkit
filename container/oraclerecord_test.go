// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The oracle of the lossless-codec and chapter tests is FFmpeg: what ffprobe and
// ffmpeg read out of a file this package wrote must equal what they read out of
// the file FFmpeg wrote (or, for a cut, of what FFmpeg's own cut writes).
//
// A test that skips when FFmpeg is missing proves nothing on a machine without
// it, so what FFmpeg read is RECORDED, in testdata/oracle.json, next to a hash
// of the bytes this package wrote. With FFmpeg installed the test asks it again
// and compares both sides, live; without it the test checks that the bytes are
// still the ones FFmpeg was shown (the hash) and that what it recorded for the two
// sides is equal. `go test -run X -update-oracle` re-records.
var updateOracle = flag.Bool("update-oracle", false, "record what FFmpeg reads into testdata/oracle.json")

const oracleFile = "testdata/oracle.json"

// oracleView is what FFmpeg reads out of one file.
type oracleView struct {
	Streams  string   `json:"streams"`  // ffprobe, the audio stream's parameters and extradata
	Chapters string   `json:"chapters"` // ffprobe -show_chapters: start, end and title of each
	Sums     []string `json:"sums"`     // ffmpeg -c copy -f framemd5: size and checksum of each packet
	Duration float64  `json:"duration"` // seconds
}

// oracleRecord is the recording of one comparison.
type oracleRecord struct {
	OutputSHA256 string     `json:"output_sha256"`
	Ours         oracleView `json:"ours"`
	Reference    oracleView `json:"reference"`
}

// oracleViewOf asks FFmpeg what it reads in path. Only the fields a test is
// about are filled: wantSums reads every packet, wantChapters the chapters.
func oracleViewOf(t *testing.T, path string, wantSums, wantChapters bool) oracleView {
	t.Helper()
	var v oracleView
	v.Streams = oracleRun(t, oracleFFprobe, "-v", "error", "-select_streams", "a:0", "-show_data", "-show_entries",
		"stream=codec_name,sample_rate,channels,channel_layout,bits_per_raw_sample,extradata_size,extradata",
		"-of", "default=nw=1", path)
	d := strings.TrimSpace(oracleRun(t, oracleFFprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path))
	v.Duration, _ = strconv.ParseFloat(d, 64)
	if wantChapters {
		v.Chapters = oracleRun(t, oracleFFprobe, "-v", "error", "-show_chapters", "-show_entries",
			"chapter=id,start_time,end_time:chapter_tags=title", "-of", "default=nw=1", path)
	}
	if wantSums {
		out := oracleRun(t, oracleFFmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-c", "copy",
			"-f", "framemd5", "-")
		for _, line := range strings.Split(out, "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// stream, dts, pts, duration, size, md5: the times differ with the
			// container's clock, the size and the checksum are the packet.
			f := strings.Split(line, ",")
			v.Sums = append(v.Sums, strings.TrimSpace(f[4])+" "+strings.TrimSpace(f[5]))
		}
	}
	return v
}

// sameDuration says two durations agree to what the coarser container can state:
// a Matroska file counts in milliseconds.
func sameDuration(a, b float64) bool {
	d := a - b
	return d > -0.011 && d < 0.011
}

// checkOracle compares what FFmpeg reads out of ours with what it reads out of
// the reference, for the comparison named key. reference is called only when
// FFmpeg is installed, and returns the path of the file to compare with.
func checkOracle(t *testing.T, key, ext string, ours []byte, wantSums, wantChapters bool, reference func(dir string) string) {
	t.Helper()
	sum := sha256.Sum256(ours)
	rec := oracleRecord{OutputSHA256: hex.EncodeToString(sum[:])}
	recorded := map[string]oracleRecord{}
	if raw, err := os.ReadFile(oracleFile); err == nil {
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatalf("%s: %v", oracleFile, err)
		}
	}
	live := true
	for _, tool := range []string{oracleFFmpeg, oracleFFprobe} {
		if _, err := os.Stat(tool); err != nil {
			live = false
		}
	}
	if live {
		dir := t.TempDir()
		path := filepath.Join(dir, "ours"+ext)
		if err := os.WriteFile(path, ours, 0o600); err != nil {
			t.Fatal(err)
		}
		rec.Ours = oracleViewOf(t, path, wantSums, wantChapters)
		rec.Reference = oracleViewOf(t, reference(dir), wantSums, wantChapters)
		if *updateOracle {
			recorded[key] = rec
			raw, err := json.MarshalIndent(recorded, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(oracleFile, append(raw, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		var ok bool
		if rec, ok = recorded[key]; !ok {
			t.Fatalf("no recording of %q in %s and no FFmpeg to ask", key, oracleFile)
		}
		if rec.OutputSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s: the bytes written are not the ones FFmpeg read when this was recorded; "+
				"re-record with -update-oracle on a machine with FFmpeg", key)
		}
	}
	compareViews(t, key, rec.Ours, rec.Reference)
	if live && !*updateOracle {
		// And what was recorded must still be what FFmpeg says.
		want, ok := recorded[key]
		if !ok {
			t.Fatalf("%s is not recorded in %s: run with -update-oracle", key, oracleFile)
		}
		if want.OutputSHA256 != rec.OutputSHA256 {
			t.Fatalf("%s: the bytes written changed since they were recorded: run with -update-oracle", key)
		}
		compareViews(t, key+" (recorded)", want.Ours, rec.Ours)
	}
}

func compareViews(t *testing.T, key string, ours, ref oracleView) {
	t.Helper()
	if ours.Streams != ref.Streams {
		t.Errorf("%s: FFmpeg reads the stream as\n%s\nwhere the reference reads\n%s", key, ours.Streams, ref.Streams)
	}
	if ours.Chapters != ref.Chapters {
		t.Errorf("%s: FFmpeg reads the chapters as\n%s\nwhere the reference reads\n%s", key, ours.Chapters, ref.Chapters)
	}
	if strings.Join(ours.Sums, "\n") != strings.Join(ref.Sums, "\n") {
		t.Errorf("%s: FFmpeg reads %d packets that differ from the reference's %d", key, len(ours.Sums), len(ref.Sums))
	}
	if !sameDuration(ours.Duration, ref.Duration) {
		t.Errorf("%s: FFmpeg reads a duration of %v s, the reference %v s", key, ours.Duration, ref.Duration)
	}
}

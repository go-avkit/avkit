// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
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

// What an oracle comparison covers.
const (
	oracleStreams  = 1 << iota // the audio stream's parameters and extradata
	oracleSums                 // the size and checksum of every packet
	oracleChapters             // the chapters
	oracleDuration             // the duration of the file
	oracleEnds                 // the ends of the chapters, which a chpl cannot state
)

// oracleChapter is one chapter as ffprobe reads it, in seconds.
type oracleChapter struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

// oracleView is what FFmpeg reads out of one file.
type oracleView struct {
	Streams  string          `json:"streams,omitempty"`  // ffprobe, the audio stream's parameters and extradata
	Chapters []oracleChapter `json:"chapters,omitempty"` // ffprobe -show_chapters
	Sums     []string        `json:"sums,omitempty"`     // ffmpeg -c copy -f framemd5: size and checksum of each packet
	Duration float64         `json:"duration"`           // seconds
}

// oracleRecord is the recording of one comparison.
type oracleRecord struct {
	OutputSHA256 string     `json:"output_sha256"`
	Ours         oracleView `json:"ours"`
	Reference    oracleView `json:"reference"`
}

// oracleViewOf asks FFmpeg what it reads in path, for what asks for.
func oracleViewOf(t *testing.T, path string, what int) oracleView {
	t.Helper()
	var v oracleView
	if what&oracleStreams != 0 {
		v.Streams = oracleRun(t, oracleFFprobe, "-v", "error", "-select_streams", "a:0", "-show_data", "-show_entries",
			"stream=codec_name,sample_rate,channels,channel_layout,bits_per_raw_sample,extradata_size,extradata",
			"-of", "default=nw=1", path)
	}
	if what&oracleDuration != 0 {
		d := strings.TrimSpace(oracleRun(t, oracleFFprobe, "-v", "error", "-show_entries", "format=duration",
			"-of", "default=nw=1:nk=1", path))
		v.Duration, _ = strconv.ParseFloat(d, 64)
	}
	if what&oracleChapters != 0 {
		var probe struct {
			Chapters []struct {
				StartTime string            `json:"start_time"`
				EndTime   string            `json:"end_time"`
				Tags      map[string]string `json:"tags"`
			} `json:"chapters"`
		}
		out := oracleRun(t, oracleFFprobe, "-v", "error", "-show_chapters", "-of", "json", path)
		if err := json.Unmarshal([]byte(out), &probe); err != nil {
			t.Fatal(err)
		}
		for _, c := range probe.Chapters {
			start, _ := strconv.ParseFloat(c.StartTime, 64)
			end, _ := strconv.ParseFloat(c.EndTime, 64)
			v.Chapters = append(v.Chapters, oracleChapter{Start: start, End: end, Title: c.Tags["title"]})
		}
	}
	if what&oracleSums != 0 {
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
func sameDuration(a, b float64) bool { return math.Abs(a-b) < 0.011 }

// checkOracle compares what FFmpeg reads out of ours with what it reads out of
// the reference, for the comparison named key. reference is called only when
// FFmpeg is installed, and returns the path of the file to compare with.
func checkOracle(t *testing.T, key, ext string, ours []byte, what int, reference func(dir string) string) {
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
		rec.Ours = oracleViewOf(t, path, what)
		rec.Reference = oracleViewOf(t, reference(dir), what)
		if *updateOracle {
			recorded[key] = rec
			var raw strings.Builder
			enc := json.NewEncoder(&raw)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", " ")
			if err := enc.Encode(recorded); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(oracleFile, []byte(raw.String()), 0o644); err != nil {
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
	compareViews(t, key, rec.Ours, rec.Reference, what)
	if live && !*updateOracle {
		// And what was recorded must still be what FFmpeg says.
		want, ok := recorded[key]
		if !ok {
			t.Fatalf("%s is not recorded in %s: run with -update-oracle", key, oracleFile)
		}
		if want.OutputSHA256 != rec.OutputSHA256 {
			t.Fatalf("%s: the bytes written changed since they were recorded: run with -update-oracle", key)
		}
		compareViews(t, key+" (recorded)", want.Ours, rec.Ours, what)
	}
}

// chapterEndSlack is how far the end of the LAST chapter may differ. A chapter
// of an MP4's chpl states no end, so FFmpeg takes the last one from the file's
// duration, which is the media's and so follows an encoder's padding; every
// other end is the next chapter's start, or stated.
const chapterEndSlack = 0.025

func compareViews(t *testing.T, key string, ours, ref oracleView, what int) {
	t.Helper()
	if ours.Streams != ref.Streams {
		t.Errorf("%s: FFmpeg reads the stream as\n%s\nwhere the reference reads\n%s", key, ours.Streams, ref.Streams)
	}
	if len(ours.Chapters) != len(ref.Chapters) {
		t.Errorf("%s: FFmpeg reads chapters %+v where the reference reads %+v", key, ours.Chapters, ref.Chapters)
	} else {
		for i, c := range ours.Chapters {
			want := ref.Chapters[i]
			slack := 1e-6
			if i == len(ours.Chapters)-1 {
				slack = chapterEndSlack
			}
			if what&oracleEnds == 0 {
				slack = math.Inf(1)
			}
			if c.Title != want.Title || math.Abs(c.Start-want.Start) > 1e-6 || math.Abs(c.End-want.End) > slack {
				t.Errorf("%s: FFmpeg reads chapter %d as %+v where the reference reads %+v", key, i+1, c, want)
			}
		}
	}
	if strings.Join(ours.Sums, "\n") != strings.Join(ref.Sums, "\n") {
		t.Errorf("%s: FFmpeg reads %d packets that differ from the reference's %d", key, len(ours.Sums), len(ref.Sums))
	}
	if what&oracleDuration != 0 && !sameDuration(ours.Duration, ref.Duration) {
		t.Errorf("%s: FFmpeg reads a duration of %v s, the reference %v s", key, ours.Duration, ref.Duration)
	}
}

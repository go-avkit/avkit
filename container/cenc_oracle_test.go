// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The oracle is ffmpeg 9.0.2's mov demuxer, which decrypts cenc and cbcs with
// -decryption_key. The files under testdata/cenc were written by this package
// from testdata/segment/source.mp4 (H.264 and AAC) with the fixed vectors of
// cenc_test.go, and run through
//
//	ffmpeg -decryption_key <key> -i <file> -c copy -f framemd5 <name>.ffmpeg.framemd5
//
// whose hashes equal those of the clear source
// (ffmpeg -i source.mp4 -map 0:v -c copy -f framemd5 clear-video1.framemd5, and -map 0:a);
// the ffmpeg-cenc-* files were written by ffmpeg's own muxer
// (-encryption_scheme cenc-aes-ctr, the only scheme its muxer has) from the same
// source. CI needs no ffmpeg: it compares with what was recorded.

func sameSums(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(want) == 0 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: %d packet hashes, want %d, and they differ", what, len(got), len(want))
	}
}

func TestCENCOracleFFmpegDecryptsOurFiles(t *testing.T) {
	for _, scheme := range []string{"cenc", "cbcs"} {
		for _, kind := range []string{"video1", "audio1"} {
			clear := framemd5Sums(t, "clear-"+kind+".framemd5")
			// What ffmpeg decrypted was the clear source.
			sameSums(t, scheme+" "+kind+" ffmpeg", framemd5Sums(t, scheme+"-"+kind+".ffmpeg.framemd5"), clear)
			// Our decryption of the same file agrees.
			enc := cencData(t, scheme+"-"+kind+".mp4")
			dec, err := DecryptFile(enc, cencKey(t))
			if err != nil {
				t.Fatal(err)
			}
			sameSums(t, scheme+" "+kind+" ours", packetSums(t, dec), clear)
			// The file is encrypted: read as it is, its samples are not the clear ones.
			if strings.Join(packetSums(t, enc), ",") == strings.Join(clear, ",") {
				t.Errorf("%s %s: the committed file is not encrypted", scheme, kind)
			}
		}
	}
}

// TestCENCCommittedFilesAreWhatWeWrite pins the output: the same source, key, IV
// and segment length give the committed bytes, so the files ffmpeg was run on
// are the files this code writes.
func TestCENCCommittedFilesAreWhatWeWrite(t *testing.T) {
	for _, scheme := range []EncryptionScheme{SchemeCENC, SchemeCBCS} {
		seg, err := Segment(segmentSource(t, "source.mp4"), SegmentDuration(2*time.Second), EncryptSegments(cencConfig(t, scheme)))
		if err != nil {
			t.Fatal(err)
		}
		for i := range seg.Tracks {
			name := string(scheme) + "-" + seg.Tracks[i].Name + ".mp4"
			if !bytes.Equal(trackFile(&seg.Tracks[i]), cencData(t, name)) {
				t.Errorf("%s differs from what Segment writes now", name)
			}
		}
	}
}

func TestCENCOracleWeDecryptFFmpegsFiles(t *testing.T) {
	for _, kind := range []string{"video1", "audio1"} {
		dec, err := DecryptFile(cencData(t, "ffmpeg-cenc-"+kind+".mp4"), cencKey(t))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		sameSums(t, "ffmpeg-cenc "+kind, packetSums(t, dec), framemd5Sums(t, "clear-"+kind+".framemd5"))
	}
}

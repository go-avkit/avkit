// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"runtime"
	"testing"
)

// hevcRecordAnnouncing is an hvcC record whose one array announces count NAL
// units and holds none of them.
func hevcRecordAnnouncing(count uint16) []byte {
	rec := make([]byte, 23) // the 22 bytes of fields, then numOfArrays
	rec[0] = 1              // configurationVersion
	rec[21] = 0x0f          // lengthSizeMinusOne = 3
	rec[22] = 1             // numOfArrays
	return append(rec, 0x20, byte(count>>8), byte(count))
}

// TestAMatroskaHEVCRecordThatOutrunsItsBytesIsCheap is the CPU amplification
// L5's fuzzing ran into. mp4ff's DecodeHEVCDecConfRec used to loop over the NAL
// unit count it reads even after the record had run out, appending an empty
// slice per turn: 65535 turns, 8 MB of garbage and about 2.5 ms for a 26-byte
// record, once per Matroska track (6.8 s on a 323 kB file of 4000 such tracks).
// mp4ff v0.61.0 stops at the end of the record (Eyevinn/mp4ff#654); this test
// is what keeps that true of the version we build against.
func TestAMatroskaHEVCRecordThatOutrunsItsBytesIsCheap(t *testing.T) {
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEGH/ISO/HEVC",
		CodecPrivate: hevcRecordAnnouncing(0xffff), Video: &mkvVideo{PixelWidth: 16, PixelHeight: 16}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := mkvTrackConfig(te, 1000)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
	// Without the upstream fix the decoder allocates 8 MB for it (measured).
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<10 {
		t.Errorf("refusing a 26-byte record allocated %d bytes", got)
	}
}

// A record the decoder refuses -- a version it does not know -- is refused
// through the decoder's own error.
func TestMkvTrackConfigRefusesAnHEVCRecordOfAnotherVersion(t *testing.T) {
	rec := hvcCPayload(t)
	rec[0] = 0
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEGH/ISO/HEVC",
		CodecPrivate: rec, Video: &mkvVideo{PixelWidth: 16, PixelHeight: 16}}
	if _, err := mkvTrackConfig(te, 1000); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

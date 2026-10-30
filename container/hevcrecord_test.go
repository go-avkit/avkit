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
	rec := make([]byte, hevcRecordHeader)
	rec[0] = 1     // configurationVersion
	rec[21] = 0x0f // lengthSizeMinusOne = 3
	rec[22] = 1    // numOfArrays
	return append(rec, 0x20, byte(count>>8), byte(count))
}

func TestCheckHEVCRecord(t *testing.T) {
	if err := checkHEVCRecord(hvcCPayload(t)); err != nil {
		t.Fatalf("a whole record was refused: %v", err)
	}
	if err := checkHEVCRecord(hevcRecordAnnouncing(0)); err != nil {
		t.Fatalf("an array of no NAL unit was refused: %v", err)
	}
	whole := hvcCPayload(t)
	for name, rec := range map[string][]byte{
		"shorter than its header":        whole[:hevcRecordHeader-1],
		"an array cut before its count":  whole[:hevcRecordHeader+2],
		"65535 NAL units announced":      hevcRecordAnnouncing(0xffff),
		"a NAL unit cut before its size": append(hevcRecordAnnouncing(1), 0x00),
		"a NAL unit cut short":           whole[:len(whole)-1],
	} {
		if err := checkHEVCRecord(rec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestAMatroskaHEVCRecordThatOutrunsItsBytesIsCheap is the CPU amplification
// L5's fuzzing ran into. mp4ff's DecodeHEVCDecConfRec loops over the NAL unit
// count it reads even after the record has run out, appending an empty slice
// per turn: 65535 turns, megabytes of garbage and about 2.5 ms for a 26-byte
// record, once per Matroska track. Measured on a 323 kB file of 4000 such
// tracks, reading every track's configuration took 6.8 s; it takes 22 ms now.
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
	// Without the walk the decoder allocates 8 MB for it (measured); the
	// refusal itself is a few hundred bytes.
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<10 {
		t.Errorf("refusing a 26-byte record allocated %d bytes", got)
	}
}

// A record that walks cleanly but that the decoder refuses -- a version it
// does not know -- is still refused through the decoder's own error.
func TestMkvTrackConfigRefusesAnHEVCRecordOfAnotherVersion(t *testing.T) {
	rec := hvcCPayload(t)
	rec[0] = 0
	te := mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackVideo, CodecID: "V_MPEGH/ISO/HEVC",
		CodecPrivate: rec, Video: &mkvVideo{PixelWidth: 16, PixelHeight: 16}}
	if _, err := mkvTrackConfig(te, 1000); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("err = %v, want ErrTrackConfig", err)
	}
}

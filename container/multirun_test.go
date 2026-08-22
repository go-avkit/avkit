// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// multiRunFragment writes an initialisation segment and one fragment whose
// track fragment holds several sample runs. Real streams are built this way —
// a packager commonly emits one run per group of pictures — and a fragment
// written by this package's own muxer holds a single run, so the shape has to
// be built here for anything to be asserted about it.
func multiRunFragment(t *testing.T, runs, perRun int) (data []byte, want [][]byte) {
	t.Helper()
	sps, pps, _, _ := avcParameterSets(t)
	init := mp4.CreateEmptyInit()
	init.Moov.Mvhd.NextTrackID = 1
	init.AddEmptyTrack(90000, "video", "und")
	if err := init.Moov.Traks[0].SetAVCDescriptor("avc1", sps, pps, true); err != nil {
		t.Fatalf("describe the track: %v", err)
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		t.Fatalf("encode the init segment: %v", err)
	}

	frag, err := mp4.CreateFragment(1, 1)
	if err != nil {
		t.Fatalf("create a fragment: %v", err)
	}
	traf := frag.Moof.Traf
	// CreateFragment leaves one run in place; the others are added beside it,
	// which is what makes this fixture worth having.
	var payload []byte
	for run := 0; run < runs; run++ {
		trun := traf.Trun
		if run > 0 {
			trun = mp4.CreateTrun(uint32(run))
			if err := traf.AddChild(trun); err != nil {
				t.Fatalf("add run %d: %v", run, err)
			}
		}
		for i := 0; i < perRun; i++ {
			frame := []byte{0, 0, 0, 2, 0x65, byte(run), byte(i)}
			want = append(want, frame)
			payload = append(payload, frame...)
			trun.AddSample(mp4.Sample{
				Flags: mp4.SyncSampleFlags, Dur: 3000, Size: uint32(len(frame)),
			})
		}
	}
	if len(traf.Truns) != runs {
		t.Fatalf("the fixture holds %d runs, want %d", len(traf.Truns), runs)
	}
	frag.Mdat.Data = payload
	// Every run states where its own data begins, the way a packager writes it.
	offset := int64(frag.Moof.Size()) + 8
	for _, trun := range traf.Truns {
		trun.DataOffset = int32(offset)
		for _, s := range trun.Samples {
			offset += int64(s.Size)
		}
	}
	if err := frag.Encode(&buf); err != nil {
		t.Fatalf("encode the fragment: %v", err)
	}
	return buf.Bytes(), want
}

// TestReaderReadsEverySampleRunOfAFragment guards against the loss that reading
// only a track fragment's first run causes: a real stream puts a fragment's
// pictures in several runs, and taking the first alone drops most of the file
// without reporting anything.
func TestReaderReadsEverySampleRunOfAFragment(t *testing.T) {
	const runs, perRun = 5, 4
	data, want := multiRunFragment(t, runs, perRun)
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("track ids = %v", ids)
	}
	got, err := r.Samples(ids[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(got) != runs*perRun {
		t.Fatalf("read %d samples, want the %d written in %d runs", len(got), runs*perRun, runs)
	}
	for i := range got {
		if !bytes.Equal(got[i].Data, want[i]) {
			t.Fatalf("sample %d = % x, want % x", i, got[i].Data, want[i])
		}
		if got[i].Duration != 3000 || !got[i].Sync {
			t.Fatalf("sample %d = %+v", i, got[i])
		}
	}
}

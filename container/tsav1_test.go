// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/asticode/go-astits"
)

// av1Padding is a padding OBU (type 15) stating its size, whose payload holds
// every sequence the start-code form has to escape — the start code itself,
// 0x000000, 0x000002 and 0x000003 followed by more than 3 — and ends in zero
// bytes, which is where the escaping meets the next start code.
var av1Padding = []byte{0x7A, 15,
	0, 0, 1, 0, 0, 0, 0, 0, 2, 0, 0, 3, 7, 0, 0}

// TestTSMuxerWritesAV1AsTheAOMCarriageSays reads back with astits what the
// AOM specification asks of a stream (2.1, 2.2, 3.2, 3.4) and with this
// package's reader that the samples survive.
func TestTSMuxerWritesAV1AsTheAOMCarriageSays(t *testing.T) {
	key, inter := mustHex(t, av1KeyUnitHex), mustHex(t, av1InterUnitHex)
	padded := append(append([]byte(nil), inter...), av1Padding...)
	// The last OBU of an MP4 sample may leave its size out (AV1-ISOBMFF 2.4).
	unsized := append(append([]byte(nil), inter...), 0x78, 5, 0, 0)

	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	cfg := av1Track(TSTimescale, 352, 288)
	cfg.CodecConfig = mustHex(t, av1RecordHex)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for i, s := range []Sample{
		{Data: key, Duration: 3000, Sync: true},
		{Data: padded, Duration: 3000},
		{Data: unsized, Duration: 3000},
	} {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.Bytes()

	es := tsProgramMap(t, out).ElementaryStreams[0]
	if es.StreamType != astits.StreamTypePrivateData {
		t.Errorf("stream type = %#x, want 0x06 (3.1)", es.StreamType)
	}
	ds := es.ElementaryStreamDescriptors
	if len(ds) != 2 || ds[0].Registration == nil || ds[0].Registration.FormatIdentifier != av1RegistrationID {
		t.Fatalf("the registration descriptor 'AV01' is not first (2.1): %+v", ds)
	}
	// marker and version, then the av1C's own profile, level, tier and
	// colour bytes, then hdr_wcg_idc 3: no indication.
	if ds[1].Tag != 0x80 || !bytes.Equal(ds[1].UserDefined, []byte{0x81, 0x00, 0x0C, 0xC0}) {
		t.Errorf("AV1 video descriptor = tag %#x %x (2.2)", ds[1].Tag, ds[1].UserDefined)
	}
	units := tsUnitsOfStream(t, out, es.ElementaryPID)
	if len(units) != 3 {
		t.Fatalf("%d units, want one per temporal unit (3.4)", len(units))
	}
	for i, u := range units {
		h := u.PES.Header
		if h.StreamID != 0xBD || !h.OptionalHeader.DataAlignmentIndicator {
			t.Errorf("unit %d: stream_id %#x, aligned %v; want 0xBD, aligned (3.4)",
				i, h.StreamID, h.OptionalHeader.DataAlignmentIndicator)
		}
		if !bytes.HasPrefix(u.PES.Data, []byte{0, 0, 1}) {
			t.Errorf("unit %d does not begin with a start code (3.2): %x", i, u.PES.Data)
		}
	}
	if !units[0].FirstPacket.AdaptationField.RandomAccessIndicator ||
		units[1].FirstPacket.AdaptationField.RandomAccessIndicator {
		t.Error("random_access_indicator does not follow the key frames (3.4)")
	}
	// Inside each OBU, nothing that reads as a start code or as 0x000000.
	for _, unit := range splitStartCodes(units[1].PES.Data) {
		for i := 0; i+2 < len(unit); i++ {
			if unit[i] == 0 && unit[i+1] == 0 && unit[i+2] <= 2 {
				t.Errorf("an unescaped %x inside an OBU: %x", unit[i:i+3], unit)
			}
		}
	}

	r, err := NewReader(out)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	back, err := r.TrackConfig(id)
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	if got := hex.EncodeToString(back.CodecConfig); got != av1RecordHex || back.Width != 352 {
		t.Errorf("config read back = %+v (av1C %s)", back, got)
	}
	samples, err := r.Samples(id)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	// The temporal delimiters the samples began with are dropped: an MP4
	// sample SHOULD NOT hold one (AV1-ISOBMFF 2.4). The unsized OBU comes
	// back stating its size, which is the form an MP4 sample prefers.
	want := [][]byte{key[2:], padded[2:], append(append([]byte(nil), inter[2:]...), 0x7A, 3, 5, 0, 0)}
	if len(samples) != len(want) {
		t.Fatalf("%d samples, want %d", len(samples), len(want))
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, want[i]) {
			t.Errorf("sample %d = %x\n           want %x", i, s.Data, want[i])
		}
	}
	if !samples[0].Sync || samples[1].Sync {
		t.Errorf("sync flags = %v, %v", samples[0].Sync, samples[1].Sync)
	}
}

// TestAV1ThatDoesNotReadIsRefusedOrPassedOver: a sample whose OBUs do not
// parse is refused on the way in; a unit that does not read on the way out is
// passed over, as an AVC unit is.
func TestAV1ThatDoesNotReadIsRefusedOrPassedOver(t *testing.T) {
	for name, sample := range map[string][]byte{
		"a size that runs past the sample": {0x7A, 9, 0},
		"a size that never ends":           {0x7A, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"a size cut short":                 {0x7A, 0xFF},
		"an extension byte cut short":      {0x7E},
	} {
		if _, err := av1ToStartCodes(sample); !errors.Is(err, ErrSample) {
			t.Errorf("%s: %v, want ErrSample", name, err)
		}
		if _, err := av1FromStartCodes(append([]byte{0, 0, 1}, sample...)); !errors.Is(err, ErrSample) {
			t.Errorf("%s, read: %v, want ErrSample", name, err)
		}
	}
	key := mustHex(t, av1KeyUnitHex)
	data := av1TSFixture(t, []byte{0, 0, 1, 0x80}, key)
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	samples, err := r.Samples(r.TrackIDs()[0])
	if err != nil || len(samples) != 1 || !bytes.Equal(samples[0].Data, key) {
		t.Errorf("samples = %d (%v), want the one that reads", len(samples), err)
	}
}

// TestSplitStartCodesKeepsWhatLiesBetween: unlike splitAnnexB it keeps the
// zero bytes a unit ends in, which an AV1 payload may hold.
func TestSplitStartCodesKeepsWhatLiesBetween(t *testing.T) {
	got := splitStartCodes([]byte{9, 0, 0, 1, 5, 0, 0, 0, 1, 6, 0, 0, 1})
	if len(got) != 2 || !bytes.Equal(got[0], []byte{5, 0}) || !bytes.Equal(got[1], []byte{6}) {
		t.Errorf("units = %x", got)
	}
	if got := splitStartCodes([]byte{1, 2, 3}); got != nil {
		t.Errorf("no start code, units = %x", got)
	}
}

// FuzzAV1StartCodeForm: whatever sample the writer accepts, the reader gives
// back the same OBUs, each stating its size, temporal delimiters aside.
func FuzzAV1StartCodeForm(f *testing.F) {
	f.Add(av1Padding)
	f.Add([]byte{0x12, 0x00, 0x32, 0x01, 0x10})
	f.Add([]byte{0x78, 0, 0, 0})
	f.Fuzz(func(t *testing.T, sample []byte) {
		payload, err := av1ToStartCodes(sample)
		if err != nil {
			return
		}
		back, err := av1FromStartCodes(payload)
		if err != nil {
			t.Fatalf("the reader refuses what the writer wrote from %x: %v", sample, err)
		}
		// Rebuild the expected sample the way the reader states it.
		var want []byte
		for pos := 0; pos < len(sample); {
			obu, n, err := av1SizedOBU(sample[pos:])
			if err != nil {
				t.Fatalf("the writer accepted an OBU this does not: %v", err)
			}
			if obu[0]>>3&0x0F != av1TemporalDelimiter {
				want = append(want, obu...)
			}
			pos += n
		}
		if !bytes.Equal(back, want) {
			t.Fatalf("round trip of %x\n gave %x\n want %x", sample, back, want)
		}
	})
}

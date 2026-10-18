// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// TestTheHEVCConstraintFlagsAreReadTheSameOnEveryArchitecture.
//
// ⛔ mp4ff loses the top 16 of these 48 bits on a 32-bit target. The fixture's
// flags are B0 00 00 00 00 00, and they lie across two emulation prevention
// bytes, so reading them means unescaping first: a reader that skipped that
// would find 00 B0 00 00 03 00 instead.
func TestTheHEVCConstraintFlagsAreReadTheSameOnEveryArchitecture(t *testing.T) {
	flags, ok := hevcConstraintFlags(mustHex(t, hevcSPSHex))
	if !ok {
		t.Fatal("the fixture SPS has no constraint flags")
	}
	if want := uint64(0xB0) << 40; flags != want {
		t.Errorf("flags = %012x, want %012x", flags, want)
	}

	var ptl hevc.ProfileTierLevel
	fixHEVCConstraints(&ptl, mustHex(t, hevcSPSHex))
	if ptl.GeneralConstraintIndicatorFlags != flags || !ptl.GeneralProgressiveSourceFlag ||
		ptl.GeneralInterlacedSourceFlag || !ptl.GeneralNonPackedConstraintFlag ||
		!ptl.GeneralFrameOnlyConstraintFlag {
		t.Errorf("profile_tier_level = %+v, want the flags B0 states: progressive, non-packed, frame-only", ptl)
	}
}

// TestAnSPSWithoutConstraintFlagsChangesNothing: a unit too short to hold them,
// and the multilayer extension form, which signals no profile_tier_level, leave
// whatever mp4ff read in place.
func TestAnSPSWithoutConstraintFlagsChangesNothing(t *testing.T) {
	for name, sps := range map[string][]byte{
		"too short": {0x42, 0x01, 0x01},
		// nuh_layer_id 1, then sps_ext_or_max_sub_layers_minus1 = 7.
		"the multilayer extension form": {0x42, 0x09, 0x0e, 2, 0x20, 0, 0, 0, 0xb0, 0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := hevcConstraintFlags(sps); ok {
				t.Error("constraint flags were found where there are none")
			}
			ptl := hevc.ProfileTierLevel{GeneralConstraintIndicatorFlags: 42}
			fixHEVCConstraints(&ptl, sps)
			record := hevc.DecConfRec{GeneralConstraintIndicatorFlags: 42}
			fixRecordConstraints(&record, [][]byte{sps})
			trak := &mp4.TrakBox{Mdia: &mp4.MdiaBox{Minf: &mp4.MinfBox{Stbl: &mp4.StblBox{Stsd: &mp4.StsdBox{
				HvcX: &mp4.VisualSampleEntryBox{HvcC: &mp4.HvcCBox{DecConfRec: hevc.DecConfRec{GeneralConstraintIndicatorFlags: 42}}},
			}}}}}
			fixHvcCConstraints(trak, [][]byte{sps})
			if ptl.GeneralConstraintIndicatorFlags != 42 || record.GeneralConstraintIndicatorFlags != 42 ||
				trak.Mdia.Minf.Stbl.Stsd.HvcX.HvcC.GeneralConstraintIndicatorFlags != 42 {
				t.Error("flags were overwritten from an SPS that has none")
			}
		})
	}
}

// TestTheWrittenHvcCStatesTheConstraintFlags: both writers of an hvcC record,
// the MP4 sample entry and the Matroska private data, carry the six bytes the
// SPS states -- which on a 32-bit target is the fix, and on a 64-bit one
// agrees with mp4ff.
func TestTheWrittenHvcCStatesTheConstraintFlags(t *testing.T) {
	vps, sps, pps := mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex), mustHex(t, hevcPPSHex)
	want := uint64(0xB0) << 40

	trak := mp4.CreateEmptyTrak(1, 90000, "video", "und")
	if err := describe(trak, TrackConfig{Codec: "hvc1", VPS: [][]byte{vps}, SPS: [][]byte{sps}, PPS: [][]byte{pps}}); err != nil {
		t.Fatal(err)
	}
	if got := trak.Mdia.Minf.Stbl.Stsd.HvcX.HvcC.GeneralConstraintIndicatorFlags; got != want {
		t.Errorf("MP4 hvcC flags = %012x, want %012x", got, want)
	}

	private, err := webmHEVCPrivate(TrackConfig{VPS: [][]byte{vps}, SPS: [][]byte{sps}, PPS: [][]byte{pps}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := hevc.DecodeHEVCDecConfRec(private.private)
	if err != nil {
		t.Fatal(err)
	}
	if record.GeneralConstraintIndicatorFlags != want {
		t.Errorf("Matroska hvcC flags = %012x, want %012x", record.GeneralConstraintIndicatorFlags, want)
	}
	if !bytes.Equal(record.GetNalusForType(hevc.NALU_SPS)[0], sps) {
		t.Error("the SPS itself was changed")
	}
}

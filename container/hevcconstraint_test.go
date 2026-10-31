// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// TestTheWrittenHvcCStatesTheConstraintFlags: both writers of an hvcC record,
// the MP4 sample entry and the Matroska private data, carry the six bytes the
// SPS states (B0 00 00 00 00 00). mp4ff before v0.61.0 lost the top 16 of these
// 48 bits on a 32-bit target (Eyevinn/mp4ff#652); the linux/386 CI job runs this
// test, so a regression there shows up as a failure here.
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

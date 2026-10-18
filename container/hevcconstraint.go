// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// hevcConstraintFlags reads general_constraint_indicator_flags straight out of
// an SPS NAL unit: the 48 bits of profile_tier_level that follow the 32 profile
// compatibility flags (ITU-T H.265 §7.3.2.2.1 and §7.3.3).
//
// ⛔ mp4ff reads them with a 48-bit Read whose result is a uint, so on a 32-bit
// target the top 16 bits are lost: the codec string comes out as
// hvc1.2.4.L123.0 instead of hvc1.2.4.L123.B0, and the hvcC record states the
// wrong progressive, interlaced, non-packed and frame-only flags. The field
// sits at a fixed position, so it is read here instead, the same on every
// architecture.
//
// It answers false for what has no such field: a unit too short, and the
// multilayer extension form, which signals no profile_tier_level of its own.
func hevcConstraintFlags(nalu []byte) (uint64, bool) {
	// After the two-byte NAL unit header: one byte holding
	// sps_video_parameter_set_id, sps_max_sub_layers_minus1 and the temporal
	// nesting flag; one holding the profile space, tier and profile; four of
	// compatibility flags. Then the six this is after.
	const at, n = 2 + 1 + 1 + 4, 6
	rbsp := unescapeHead(nalu, at+n)
	if len(rbsp) < at+n {
		return 0, false
	}
	layer := (rbsp[0]&1)<<5 | rbsp[1]>>3
	if layer != 0 && (rbsp[2]>>1)&7 == 7 {
		return 0, false
	}
	var flags uint64
	for _, b := range rbsp[at : at+n] {
		flags = flags<<8 | uint64(b)
	}
	return flags, true
}

// unescapeHead returns the first n bytes of a NAL unit with its emulation
// prevention bytes removed -- an 0x03 after two zero bytes is not payload --
// or fewer when the unit holds fewer.
func unescapeHead(nalu []byte, n int) []byte {
	out := make([]byte, 0, n)
	zeros := 0
	for _, b := range nalu {
		if len(out) == n {
			break
		}
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// fixHEVCConstraints restates the constraint flags of a parsed profile_tier_level
// from the SPS it was parsed from, and the four flags mp4ff derives from them.
func fixHEVCConstraints(ptl *hevc.ProfileTierLevel, sps []byte) {
	flags, ok := hevcConstraintFlags(sps)
	if !ok {
		return
	}
	ptl.GeneralConstraintIndicatorFlags = flags
	ptl.GeneralProgressiveSourceFlag = flags>>47&1 == 1
	ptl.GeneralInterlacedSourceFlag = flags>>46&1 == 1
	ptl.GeneralNonPackedConstraintFlag = flags>>45&1 == 1
	ptl.GeneralFrameOnlyConstraintFlag = flags>>44&1 == 1
}

// fixHvcCConstraints restates the constraint flags of the hvcC record that
// SetHEVCDescriptor has just given the track, from the first SPS. describe has
// checked that there is one. Parameter sets mp4ff cannot read leave no entry at
// all, which describe's caller reports; there is nothing to restate then.
func fixHvcCConstraints(trak *mp4.TrakBox, sps [][]byte) {
	entry := trak.Mdia.Minf.Stbl.Stsd.HvcX
	if entry == nil || entry.HvcC == nil {
		return
	}
	if flags, ok := hevcConstraintFlags(sps[0]); ok {
		entry.HvcC.GeneralConstraintIndicatorFlags = flags
	}
}

// fixRecordConstraints does the same for a record built on its own.
func fixRecordConstraints(record *hevc.DecConfRec, sps [][]byte) {
	if flags, ok := hevcConstraintFlags(sps[0]); ok {
		record.GeneralConstraintIndicatorFlags = flags
	}
}

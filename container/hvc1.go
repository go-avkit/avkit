// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrNotHEVC means a track a caller asked about HEVC is not an HEVC track.
var ErrNotHEVC = errors.New("container: not an HEVC track")

// HVC1Conformance says what stands between an HEVC track and the hvc1 rule.
//
// ISO/IEC 14496-15 gives HEVC in an MP4 two sample entries that differ by one
// rule and nothing else: under "hvc1" the parameter sets are in the sample entry
// and shall NOT be in the samples, while under "hev1" the samples may carry
// their own. Players are entitled to act on that: macOS reads an hvc1 track and
// refuses a hev1 one outright, which is why the difference is worth a type.
type HVC1Conformance struct {
	// Codec is the sample entry as it stands: "hvc1" or "hev1".
	Codec string
	// InSamples is how many parameter set units the samples carry. Any at all
	// breaks the hvc1 rule, even when they agree with the entry.
	InSamples int
	// EntryDisagrees means the samples use a parameter set the entry does not
	// state. This is the case that matters: a player obeying the hvc1 rule
	// reads the entry and ignores the samples, so it decodes the stream with
	// parameters the stream does not use.
	EntryDisagrees bool
	// Unstated are the parameter sets the samples use that the entry lacks, as
	// raw NAL units. Empty when the entry states them all.
	Unstated [][]byte
}

// Conformant reports whether the track already meets the hvc1 rule.
func (c HVC1Conformance) Conformant() bool {
	return c.Codec == "hvc1" && c.InSamples == 0
}

// CheckHVC1 asks whether an HEVC track meets the hvc1 rule, without writing
// anything.
//
// ⛔ It exists because retagging hev1 as hvc1 -- which is all a tag change is,
// and all that the usual tools do -- is not always sound. Measured over 67 hev1
// files: 60 carried no parameter set in their samples, so for those a tag change
// is the whole repair. The other 7 repeated a full set at every random access
// point AND those sets disagreed with the sample entry, in one field:
// init_qp_minus26, 26 in the stream against 30 in the entry. Every slice codes
// its own QP as a delta from that value, so a player reading the entry
// dequantises the whole film four steps off. Decoded both ways, the pictures
// differ in 92 % of their bytes by a mean of 84 of 255, and the decoder reports
// the deltas as out of range. A tag change alone had made those files LOOK
// repaired, because the decoders to hand still read the samples.
func CheckHVC1(entry TrackConfig, samples []Sample) (HVC1Conformance, error) {
	name := entry.Codec
	if name != "hvc1" && name != "hev1" {
		return HVC1Conformance{}, fmt.Errorf("%w: sample entry %q", ErrNotHEVC, name)
	}
	c := HVC1Conformance{Codec: name}
	// ⛔ The sets are gathered by walking the samples rather than through
	// ConfigFromSamples, which needs all three to be there. A conformant track
	// has NONE of them in its samples, so asking that way makes the function
	// fail on exactly the state it exists to recognise -- which it did, on the
	// first file it was pointed at after the repair.
	form, err := naluForm(samples)
	if err != nil {
		return HVC1Conformance{}, err
	}
	held := map[string]bool{}
	for _, set := range [][][]byte{entry.VPS, entry.SPS, entry.PPS} {
		for _, p := range set {
			held[string(p)] = true
		}
	}
	seen := map[string]bool{}
	for _, s := range samples {
		units, err := naluUnits(s.Data, form)
		if err != nil {
			return HVC1Conformance{}, fmt.Errorf("%w: %v", ErrSample, err)
		}
		for _, u := range units {
			switch naluKind(u, true) {
			case naluVPS, naluSPS, naluPPS:
			default:
				continue
			}
			c.InSamples++
			if held[string(u)] || seen[string(u)] {
				continue
			}
			seen[string(u)] = true
			c.EntryDisagrees = true
			c.Unstated = append(c.Unstated, bytes.Clone(u))
		}
	}
	return c, nil
}

// ConformHVC1 makes every HEVC track of the output a conformant hvc1 track: the
// sample entry states the parameter sets the SAMPLES use, and the samples carry
// none.
//
// The stream is taken as the authority, always, and not only where the two
// disagree. The samples are what a decoder has to decode; an entry that
// contradicts them is wrong about the file whichever of the two was written
// first, and there is no third source to arbitrate. Taking the entry instead
// would keep a file that decodes into garbage -- CheckHVC1 records what that
// looks like -- and refusing the disagreement would leave the caller holding a
// file nothing can repair.
//
// It composes with the other options, so a cut or a concatenation can be made
// conformant in the same pass. A track that is not HEVC is left exactly as it
// is, which is what lets this be passed for a whole file without knowing what
// is in it.
//
// The sample bytes DO change here, unlike every other remux: a parameter set
// unit is removed from each sample that carries one. Nothing else in the sample
// is touched, and a sample left with no units at all is an error rather than an
// empty frame, because a frame is not a thing a track can be missing.
func ConformHVC1() RemuxOption {
	return func(s *remuxSettings) { s.conformHVC1 = true }
}

// conformTrack rewrites one track to the hvc1 rule, or returns it unchanged when
// it is not HEVC.
func conformTrack(t sourceTrack) (sourceTrack, error) {
	if t.cfg.Codec != "hvc1" && t.cfg.Codec != "hev1" {
		return t, nil
	}
	form, err := naluForm(t.samples)
	if err != nil {
		return t, err
	}
	// ⛔ The sets are collected while stripping them, not asked of
	// ConfigFromSamples, which needs all three to be in the samples. 60 of the
	// 67 hev1 files measured carry NONE -- their sets are only in the sample
	// entry, and for them the repair is the tag alone. Asking the samples first
	// failed on all 60, which is the larger half of the population.
	var vps, sps, pps [][]byte
	out := make([]Sample, len(t.samples))
	for i, s := range t.samples {
		units, err := naluUnits(s.Data, form)
		if err != nil {
			return t, fmt.Errorf("%w: sample %d: %v", ErrSample, i, err)
		}
		kept := make([][]byte, 0, len(units))
		for _, u := range units {
			switch naluKind(u, true) {
			case naluVPS:
				vps = appendUnique(vps, u)
			case naluSPS:
				sps = appendUnique(sps, u)
			case naluPPS:
				pps = appendUnique(pps, u)
			default:
				kept = append(kept, u)
			}
		}
		if len(kept) == 0 {
			return t, fmt.Errorf("%w: sample %d holds nothing but parameter sets", ErrSample, i)
		}
		out[i] = s
		out[i].Data = joinLengthPrefixed(kept)
	}
	// A stream that states a set more than once, differently, cannot be carried
	// by one sample entry: hvc1 puts the sets there and nowhere else, so a
	// change part way through has no place to be written and would be lost
	// silently. Refusing says so while the caller still holds the original.
	if len(vps) > 1 || len(sps) > 1 || len(pps) > 1 {
		return t, fmt.Errorf("%w: the samples state %d VPS, %d SPS and %d PPS, and hvc1 holds one of each",
			ErrTrackConfig, len(vps), len(sps), len(pps))
	}
	// Where the samples carried nothing, the entry is all there is, and it is
	// kept as it stands. Where they carried something, it wins: the samples are
	// what a decoder decodes, and an entry contradicting them is wrong about the
	// file whichever was written first.
	t.cfg.Codec = "hvc1"
	if len(vps) > 0 {
		t.cfg.VPS = vps
	}
	if len(sps) > 0 {
		t.cfg.SPS = sps
	}
	if len(pps) > 0 {
		t.cfg.PPS = pps
	}
	if len(t.cfg.SPS) == 0 || len(t.cfg.PPS) == 0 {
		return t, fmt.Errorf("%w: hvc1 needs an SPS and a PPS, and neither the entry nor the samples hold %s",
			ErrTrackConfig, missing(t.cfg))
	}
	t.samples = out
	return t, nil
}

// missing names which parameter sets a configuration lacks, so the refusal says
// what is absent rather than that something is.
func missing(cfg TrackConfig) string {
	switch {
	case len(cfg.SPS) == 0 && len(cfg.PPS) == 0:
		return "either"
	case len(cfg.SPS) == 0:
		return "an SPS"
	default:
		return "a PPS"
	}
}

// joinLengthPrefixed joins NAL units in the form an MP4 sample holds them.
//
// Four bytes, always, and never the shorter prefixes the format allows: the
// muxer writes a configuration record stating four, and a length written to a
// different width than the record announces is read as the wrong unit boundary
// for the rest of the sample.
func joinLengthPrefixed(units [][]byte) []byte {
	n := 0
	for _, u := range units {
		n += 4 + len(u)
	}
	out := make([]byte, 0, n)
	var prefix [4]byte
	for _, u := range units {
		binary.BigEndian.PutUint32(prefix[:], uint32(len(u)))
		out = append(out, prefix[:]...)
		out = append(out, u...)
	}
	return out
}

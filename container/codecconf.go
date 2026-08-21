// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/vp9"
	"github.com/asticode/go-astits"
)

// An MP4 states a track's configuration in its sample entry, so Reader can hand
// it over without looking at a single coded byte. An elementary stream states it
// nowhere: an MPEG-TS carries no avcC, no hvcC and no av1C, and its parameter
// sets travel in the samples themselves. A caller holding only samples — an HLS
// segment, an ADTS stream, a stream of OBUs — therefore has nothing to give
// Muxer.AddTrack, and a track written from a guess plays as garbage without
// saying so.
//
// ConfigFromSamples closes that gap: it reads the bitstream headers the samples
// carry and returns the same TrackConfig a container would have stated, plus the
// profile and level a manifest names. Bitstream parsing is delegated to the
// mp4ff codec packages (avc, hevc, av1, aac, vp9), which already read SPS, PPS,
// VPS, sequence header OBUs, ADTS headers and VP9 uncompressed headers; what
// this file adds is deciding which of them applies, refusing what cannot be read
// soundly, and projecting the result onto this package's own types.

// Errors reported when deriving a configuration from samples.
var (
	// ErrNoConfiguration means the samples carry nothing that describes the
	// track: no parameter set, no sequence header, no key frame.
	ErrNoConfiguration = errors.New("container: the samples describe no configuration")
	// ErrCodecMismatch means the samples do not hold the codec the caller
	// stated. The codec is never inferred from the bitstream — the two
	// NAL-based codecs spell a unit's type in different bits of its header,
	// and reading one as the other turns a picture into a parameter set — so
	// a contradiction is reported instead of resolved.
	ErrCodecMismatch = errors.New("container: the samples do not hold the stated codec")
)

// StreamConfig is what a track's own samples say about it.
//
// The embedded TrackConfig is what Muxer.AddTrack and TSMuxer.AddTrack take, so
// deriving a configuration and writing the track is two calls. The remaining
// fields are what a manifest states and a sample entry does not: the coded frame
// size before cropping, the pixel aspect ratio, and the profile, tier and level.
type StreamConfig struct {
	TrackConfig

	// CodedWidth and CodedHeight are the frame size the bitstream codes,
	// which is a whole number of macroblocks or coding units and so is
	// usually larger than what is shown: 1080 lines of video are coded as
	// 1088 and cropped. TrackConfig.Width and Height hold the visible size,
	// the one a player displays and a sample entry states.
	CodedWidth, CodedHeight int
	// SARWidth and SARHeight are the sample (pixel) aspect ratio, 1:1 unless
	// the bitstream states otherwise. Display size is the visible size scaled
	// by this ratio, which is how anamorphic video ends up wider than it is
	// coded.
	SARWidth, SARHeight int
	// Profile, Tier and Level are the bitstream's own, as its headers code
	// them: profile_idc and level_idc for AVC, general_profile_idc,
	// general_tier_flag and general_level_idc for HEVC, seq_profile,
	// seq_tier_0 and seq_level_idx_0 for AV1, and the VP9 profile with the
	// level of the vpcC table. Tier is 0 for the codecs that have none.
	Profile, Tier, Level byte
	// ProfileCompatibility is the AVC constraint_set flags byte, or the HEVC
	// general_profile_compatibility_flags; 0 for the other codecs.
	ProfileCompatibility uint32
	// CodecString is the RFC 6381 codecs parameter of this track, such as
	// avc1.64000A, which is what an HLS or DASH manifest names it by.
	CodecString string
}

// ConfigOption tunes how a configuration is derived.
type ConfigOption func(*configSettings)

type configSettings struct {
	timescale uint32
	language  string
}

// SampleTimescale states the unit the samples' durations are counted in, per
// second, which is carried straight into TrackConfig.Timescale. VP9 needs it for
// a second reason: a vpcC record has to state a level, and a level is the frame
// rate as much as the frame size, so a VP9 track derived without a timescale is
// refused rather than levelled by guess.
func SampleTimescale(timescale uint32) ConfigOption {
	return func(s *configSettings) { s.timescale = timescale }
}

// ConfigLanguage states the track's ISO-639-2 language code, which no bitstream
// carries.
func ConfigLanguage(language string) ConfigOption {
	return func(s *configSettings) { s.language = language }
}

// ConfigFromSamples derives a track's configuration from its coded samples.
//
// codec is the sample entry the caller means to write — "avc1", "avc3", "hvc1",
// "hev1", "av01", "vp09" or "mp4a" — and it is taken as stated: the codec is
// never inferred from the sample data, because guessing AVC against HEVC from a
// NAL header byte misreads every unit of the stream. Samples that contradict the
// stated codec are reported as ErrCodecMismatch.
//
// The samples are read in the form each codec's elementary stream has them: AVC
// and HEVC either start-code separated (Annex B, as MPEG-TS carries them) or
// four-byte length prefixed (as an MP4 sample holds them); AV1 as temporal units
// of OBUs; AAC as ADTS frames. Nothing is written back: the samples are read,
// never modified.
//
// A codec whose configuration is not in its samples, or whose bitstream this
// cannot read soundly, is refused with ErrUnsupportedCodec rather than
// described by guess, because a wrong configuration is silent — the file plays
// as garbage.
func ConfigFromSamples(codec string, samples []Sample, opts ...ConfigOption) (StreamConfig, error) {
	var settings configSettings
	for _, opt := range opts {
		opt(&settings)
	}
	name := strings.ToLower(strings.TrimSpace(codec))
	if len(samples) == 0 {
		return StreamConfig{}, fmt.Errorf("%w: %s has no sample to read", ErrNoConfiguration, name)
	}
	var (
		cfg StreamConfig
		err error
	)
	switch name {
	case "avc1", "avc3":
		var scan naluScan
		if scan, err = scanNalus(samples, false); err == nil {
			cfg, err = avcStreamConfig(name, scan)
		}
	case "hvc1", "hev1":
		var scan naluScan
		if scan, err = scanNalus(samples, true); err == nil {
			cfg, err = hevcStreamConfig(name, scan)
		}
	case "av01":
		cfg, err = av1StreamConfig(name, samples)
	case "vp09":
		cfg, err = vp9StreamConfig(name, samples, settings.timescale)
	case "mp4a":
		cfg, err = aacStreamConfig(name, samples)
	case "vp08":
		// A VP8 key frame states its size, but VP8 defines no level at all,
		// and a vpcC record whose level is zero states nothing a player can
		// use — which is why the muxer refuses one.
		return refuse(name, "VP8 states no level, and a vpcC record without one describes nothing")
	case "opus":
		return refuse(name, "an Opus stream carries no identification header; only its container states one")
	case "ac-3", "ec-3":
		return refuse(name, "the bit stream information of an AC-3 sync frame is not read by this package")
	default:
		return StreamConfig{}, fmt.Errorf("%w: %q", ErrUnsupportedCodec, codec)
	}
	if err != nil {
		return StreamConfig{}, err
	}
	cfg.Codec = name
	cfg.Timescale = settings.timescale
	cfg.Language = settings.language
	return cfg, nil
}

// refuse says why a codec's configuration cannot come from its samples.
func refuse(codec, why string) (StreamConfig, error) {
	return StreamConfig{}, fmt.Errorf("%w: %s: %s", ErrUnsupportedCodec, codec, why)
}

// naluScan is what the NAL units of a track's samples hold.
type naluScan struct {
	sets parameterSets
	// otherCodec records that a unit reads as a parameter set of the other
	// NAL-based codec. It is not used to decide what the stream is — that is
	// the caller's to state — only to say so when the stated codec finds
	// nothing, which is the mistake it always turns out to be.
	otherCodec bool
	// samples is how many samples were read, for the error messages.
	samples int
}

// The two forms a NAL-based sample can arrive in.
const (
	// formAnnexB separates units with start codes, as an elementary stream
	// and so an MPEG-TS carries them.
	formAnnexB = iota
	// formLengthPrefixed puts a four-byte length before each unit, as an MP4
	// sample holds them.
	formLengthPrefixed
)

// scanNalus reads every NAL unit of every sample and keeps the parameter sets,
// which is where an elementary stream states its configuration. Which codec's
// headers the units carry has to be stated, because the bytes do not say.
func scanNalus(samples []Sample, isHEVC bool) (naluScan, error) {
	form, err := naluForm(samples)
	if err != nil {
		return naluScan{}, err
	}
	header := 1
	if isHEVC {
		header = 2 // an HEVC unit's type sits in the second bit of two bytes
	}
	scan := naluScan{samples: len(samples)}
	for i, s := range samples {
		nalus, err := naluUnits(s.Data, form)
		if err != nil {
			return naluScan{}, fmt.Errorf("%w: sample %d: %v", ErrSample, i, err)
		}
		for j, nalu := range nalus {
			if len(nalu) < header {
				return naluScan{}, fmt.Errorf("%w: sample %d unit %d is %d bytes, too short for a NAL header",
					ErrSample, i, j, len(nalu))
			}
			// forbidden_zero_bit is zero in every NAL unit of both codecs,
			// so a unit that sets it is not one: reading on would parse
			// arbitrary bytes as a parameter set.
			if nalu[0]&0x80 != 0 {
				return naluScan{}, fmt.Errorf("%w: sample %d unit %d sets forbidden_zero_bit", ErrCodecMismatch, i, j)
			}
			switch naluKind(nalu, isHEVC) {
			case naluSPS:
				scan.sets.sps = appendUnique(scan.sets.sps, nalu)
			case naluPPS:
				scan.sets.pps = appendUnique(scan.sets.pps, nalu)
			case naluVPS:
				scan.sets.vps = appendUnique(scan.sets.vps, nalu)
			}
			if k := naluKind(nalu, !isHEVC); k == naluSPS || k == naluVPS {
				scan.otherCodec = true
			}
		}
	}
	return scan, nil
}

// naluForm decides which form the samples hold their units in, from the first
// one long enough to tell. A start code decides it: a length-prefixed sample
// could only begin 00 00 00 01 if its first unit were a single byte, which no
// picture and no parameter set is, so reading that as a start code is the safe
// way round.
func naluForm(samples []Sample) (int, error) {
	for _, s := range samples {
		if len(s.Data) < 4 {
			continue
		}
		if s.Data[0] == 0 && s.Data[1] == 0 && (s.Data[2] == 1 || (s.Data[2] == 0 && s.Data[3] == 1)) {
			return formAnnexB, nil
		}
		return formLengthPrefixed, nil
	}
	return 0, fmt.Errorf("%w: no sample is long enough to hold a NAL unit", ErrNoConfiguration)
}

// naluUnits cuts one sample into its NAL units.
func naluUnits(data []byte, form int) ([][]byte, error) {
	if form == formLengthPrefixed {
		// A prefix reaching past the end of the sample means the sample is
		// truncated, and the units read before it cannot be trusted to be
		// whole either.
		nalus, exact := splitLengthPrefixed(data)
		if !exact {
			return nil, fmt.Errorf("the four-byte lengths do not describe the sample's %d bytes exactly", len(data))
		}
		return nalus, nil
	}
	nalus := splitAnnexB(data)
	if len(nalus) == 0 {
		return nil, errors.New("no start code separates a NAL unit here")
	}
	return nalus, nil
}

// appendUnique adds a parameter set unless it is already held. An elementary
// stream repeats its parameter sets so a player can join at any point; a sample
// entry names each of them once.
func appendUnique(sets [][]byte, nalu []byte) [][]byte {
	for _, held := range sets {
		if bytes.Equal(held, nalu) {
			return sets
		}
	}
	return append(sets, nalu)
}

// avcStreamConfig reads an AVC track's configuration out of its parameter sets.
func avcStreamConfig(codec string, scan naluScan) (StreamConfig, error) {
	if len(scan.sets.sps) == 0 || len(scan.sets.pps) == 0 {
		return StreamConfig{}, missingSets(codec, scan,
			fmt.Sprintf("%d SPS and %d PPS, and both are needed", len(scan.sets.sps), len(scan.sets.pps)))
	}
	byID := make(map[uint32]*avc.SPS, len(scan.sets.sps))
	var first *avc.SPS
	for i, nalu := range scan.sets.sps {
		// The VUI is parsed past the aspect ratio, because the frame rate and
		// the colour description live behind it.
		sps, err := avc.ParseSPSNALUnit(nalu, true)
		if err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s SPS %d does not parse as an AVC one: %v",
				ErrCodecMismatch, codec, i, err)
		}
		byID[sps.ParameterID] = sps
		if first == nil {
			first = sps
		}
	}
	// A PPS is read against the SPS it names: one that parses against none of
	// them belongs to another stream, and writing it would describe a track
	// no decoder could set up.
	for i, nalu := range scan.sets.pps {
		if _, err := avc.ParsePPSNALUnit(nalu, byID); err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s PPS %d does not parse against the stream's SPS: %v",
				ErrTrackConfig, codec, i, err)
		}
	}
	cfg := StreamConfig{
		TrackConfig: TrackConfig{
			Kind: Video, Width: int(first.Width), Height: int(first.Height),
			SPS: scan.sets.sps, PPS: scan.sets.pps,
		},
		Profile:              byte(first.Profile),
		ProfileCompatibility: first.ProfileCompatibility,
		Level:                byte(first.Level),
		CodecString:          avc.CodecString(codec, first),
	}
	// The coded size is the cropping offsets put back: mp4ff subtracts them
	// from Width and Height, which is the visible size a player shows.
	cropX, cropY := avcCropUnits(first)
	cfg.CodedWidth = cfg.Width + int((first.FrameCropLeftOffset+first.FrameCropRightOffset)*cropX)
	cfg.CodedHeight = cfg.Height + int((first.FrameCropTopOffset+first.FrameCropBottomOffset)*cropY)
	// An SPS that states no aspect ratio, and one with no VUI at all, both
	// mean square pixels.
	var sarWidth, sarHeight uint
	if first.VUI != nil {
		sarWidth, sarHeight = first.VUI.SampleAspectRatioWidth, first.VUI.SampleAspectRatioHeight
	}
	cfg.SARWidth, cfg.SARHeight = aspectRatio(sarWidth, sarHeight)
	return cfg, nil
}

// avcSubsampling is SubWidthC and SubHeightC by chroma_format_idc: monochrome
// and 4:4:4 crop in luma samples, 4:2:0 in two-by-two chroma samples, 4:2:2 in
// two-by-one. A format outside the table is one no SPS with cropping parses
// with, so its offsets are zero and its units are never used.
var avcSubsampling = map[byte][2]uint{0: {1, 1}, 1: {2, 2}, 2: {2, 1}, 3: {1, 1}}

// avcCropUnits is how many luma samples one unit of an SPS's cropping offsets
// stands for, horizontally and vertically (ISO/IEC 14496-10, 7.4.2.1.1), which
// is what has to be added back to recover the coded size from the visible one.
func avcCropUnits(sps *avc.SPS) (uint, uint) {
	sub := avcSubsampling[sps.ChromaFormatIDC]
	// Interlaced coding crops in pairs of rows.
	frameUnits := uint(2)
	if sps.FrameMbsOnlyFlag {
		frameUnits = 1
	}
	return sub[0], sub[1] * frameUnits
}

// hevcStreamConfig reads an HEVC track's configuration out of its parameter
// sets.
func hevcStreamConfig(codec string, scan naluScan) (StreamConfig, error) {
	if len(scan.sets.vps) == 0 || len(scan.sets.sps) == 0 || len(scan.sets.pps) == 0 {
		return StreamConfig{}, missingSets(codec, scan,
			fmt.Sprintf("%d VPS, %d SPS and %d PPS, and all three are needed",
				len(scan.sets.vps), len(scan.sets.sps), len(scan.sets.pps)))
	}
	for i, nalu := range scan.sets.vps {
		if _, err := hevc.ParseVPSNALUnit(nalu); err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s VPS %d does not parse as an HEVC one: %v",
				ErrCodecMismatch, codec, i, err)
		}
	}
	byID := make(map[uint32]*hevc.SPS, len(scan.sets.sps))
	var first *hevc.SPS
	for i, nalu := range scan.sets.sps {
		sps, err := hevc.ParseSPSNALUnit(nalu)
		if err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s SPS %d does not parse as an HEVC one: %v",
				ErrCodecMismatch, codec, i, err)
		}
		byID[uint32(sps.SpsID)] = sps
		if first == nil {
			first = sps
		}
	}
	for i, nalu := range scan.sets.pps {
		if _, err := hevc.ParsePPSNALUnit(nalu, byID); err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s PPS %d does not parse against the stream's SPS: %v",
				ErrTrackConfig, codec, i, err)
		}
	}
	// ImageSize applies the conformance window, which is HEVC's cropping: the
	// coded size is a whole number of coding units, the window says what of it
	// is shown.
	width, height := first.ImageSize()
	ptl := first.ProfileTierLevel
	cfg := StreamConfig{
		TrackConfig: TrackConfig{
			Kind: Video, Width: int(width), Height: int(height),
			VPS: scan.sets.vps, SPS: scan.sets.sps, PPS: scan.sets.pps,
		},
		CodedWidth:           int(first.PicWidthInLumaSamples),
		CodedHeight:          int(first.PicHeightInLumaSamples),
		Profile:              ptl.GeneralProfileIDC,
		ProfileCompatibility: ptl.GeneralProfileCompatibilityFlags,
		Level:                ptl.GeneralLevelIDC,
		CodecString:          hevc.CodecString(codec, first),
	}
	if ptl.GeneralTierFlag {
		cfg.Tier = 1 // the high tier; the main tier is 0
	}
	var sarWidth, sarHeight uint
	if first.VUI != nil {
		sarWidth, sarHeight = first.VUI.SampleAspectRatioWidth, first.VUI.SampleAspectRatioHeight
	}
	cfg.SARWidth, cfg.SARHeight = aspectRatio(sarWidth, sarHeight)
	return cfg, nil
}

// missingSets says which parameter sets are missing, and names the other
// NAL-based codec when the units turn out to hold its parameter sets instead:
// that is what naming the wrong one of the two looks like from here, and it is
// worth saying so rather than reporting an absence.
func missingSets(codec string, scan naluScan, held string) error {
	if scan.otherCodec {
		return fmt.Errorf("%w: %d samples were read as %s and hold %s, "+
			"but their units are the parameter sets of the other NAL-based codec",
			ErrCodecMismatch, scan.samples, codec, held)
	}
	return fmt.Errorf("%w: %d %s samples hold %s", ErrNoConfiguration, scan.samples, codec, held)
}

// aspectRatio is the pixel aspect ratio a bitstream states, or the square pixels
// to assume when it states none.
func aspectRatio(width, height uint) (int, int) {
	if width == 0 || height == 0 {
		return 1, 1
	}
	return int(width), int(height)
}

// encodeAv1Record exists so the failure it guards can be tested: an av1C record
// is encoded into a buffer sized from the record itself, so nothing can go
// wrong, and code that cannot be exercised is code nobody knows the behaviour
// of.
var encodeAv1Record = func(rec *av1.CodecConfRec, w io.Writer) error { return rec.Encode(w) }

// av1StreamConfig builds an AV1 track's av1C record from the sequence header
// OBU its samples carry, which is the only place an elementary stream states it.
func av1StreamConfig(codec string, samples []Sample) (StreamConfig, error) {
	for i, s := range samples {
		obus, err := av1.SplitOBUs(s.Data)
		if err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s sample %d does not split into OBUs: %v",
				ErrCodecMismatch, codec, i, err)
		}
		for j, obu := range obus {
			if obu.Header.Type != av1.OBUSequenceHeader {
				continue
			}
			seq, err := av1.ParseSequenceHeader(obu.Payload)
			if err != nil {
				return StreamConfig{}, fmt.Errorf("%w: %s sample %d OBU %d does not parse as a sequence header: %v",
					ErrCodecMismatch, codec, i, j, err)
			}
			// The record carries the sequence header itself, re-encoded with
			// its size field, which is the form the configOBUs field takes.
			rec := av1.CodecConfRecFromSequenceHeader(seq, obu.Encode())
			var payload bytes.Buffer
			if err := encodeAv1Record(&rec, &payload); err != nil {
				return StreamConfig{}, fmt.Errorf("%w: %s av1C: %v", ErrTrackConfig, codec, err)
			}
			// The record is handed back through this package's own av1C
			// decoder, so what is returned is what the muxer will accept
			// rather than something it turns down a call later.
			if _, err := decodeAv1C(payload.Bytes()); err != nil {
				return StreamConfig{}, err
			}
			return StreamConfig{
				TrackConfig: TrackConfig{
					Kind: Video, Width: int(seq.Width()), Height: int(seq.Height()),
					CodecConfig: payload.Bytes(),
				},
				CodedWidth: int(seq.Width()), CodedHeight: int(seq.Height()),
				SARWidth: 1, SARHeight: 1,
				Profile: seq.SeqProfile, Tier: seq.SeqTier0, Level: seq.SeqLevelIdx0,
				CodecString: seq.CodecString(codec),
			}, nil
		}
	}
	return StreamConfig{}, fmt.Errorf("%w: none of the %d %s samples carries a sequence header OBU",
		ErrNoConfiguration, len(samples), codec)
}

// vp9StreamConfig builds a VP9 track's vpcC record from the uncompressed header
// of a key frame, the only frame that states the frame size and the colour
// configuration.
func vp9StreamConfig(codec string, samples []Sample, timescale uint32) (StreamConfig, error) {
	if timescale == 0 {
		return StreamConfig{}, fmt.Errorf("%w: %s needs the timescale its sample durations are counted in, "+
			"because a vpcC level is a frame rate as much as a frame size", ErrNoConfiguration, codec)
	}
	rate, err := frameRate(samples, timescale)
	if err != nil {
		return StreamConfig{}, err
	}
	for i, s := range samples {
		header, err := vp9.ParseFrameHeader(s.Data)
		if err != nil {
			return StreamConfig{}, fmt.Errorf("%w: %s sample %d does not begin with a VP9 uncompressed header: %v",
				ErrCodecMismatch, codec, i, err)
		}
		if !header.KeyFrame || header.ShowExistingFrame {
			continue
		}
		primaries, transfer, matrix := header.CICP()
		// The level is the smallest one that can carry this frame size at the
		// rate the samples run at; a caller who knows the level the encoder
		// stated can overwrite it.
		level := vp9.Level(header.Width, header.Height, rate)
		return StreamConfig{
			TrackConfig: TrackConfig{
				Kind: Video, Width: int(header.Width), Height: int(header.Height),
				VPx: &VPxConfig{
					Profile: header.Profile, Level: level, BitDepth: header.BitDepth,
					ChromaSubsampling: header.VpcCChromaSubsampling(), FullRange: header.ColorRange,
					ColourPrimaries: primaries, TransferCharacteristics: transfer,
					MatrixCoefficients: matrix,
				},
			},
			CodedWidth: int(header.Width), CodedHeight: int(header.Height),
			SARWidth: 1, SARHeight: 1,
			Profile: header.Profile, Level: level,
			CodecString: fmt.Sprintf("%s.%02d.%02d.%02d", codec, header.Profile, level, header.BitDepth),
		}, nil
	}
	return StreamConfig{}, fmt.Errorf("%w: none of the %d %s samples is a key frame, "+
		"and only a key frame states the frame size and the colour configuration",
		ErrNoConfiguration, len(samples), codec)
}

// frameRate is how many samples a second the track runs at, which their own
// durations state.
func frameRate(samples []Sample, timescale uint32) (float64, error) {
	var total uint64
	for i, s := range samples {
		if s.Duration == 0 {
			return 0, fmt.Errorf("%w: sample %d states no duration, so the track has no frame rate",
				ErrSample, i)
		}
		total += uint64(s.Duration)
	}
	return float64(timescale) * float64(len(samples)) / float64(total), nil
}

// aacStreamConfig turns the ADTS header of an AAC stream into the
// AudioSpecificConfig an MP4 states instead.
func aacStreamConfig(codec string, samples []Sample) (StreamConfig, error) {
	first := samples[0].Data
	// The offset a decode returns is what it skipped to find the sync word,
	// not the header's length. A sample that does not begin with one is not an
	// ADTS frame: raw AAC frames, which is what an MP4 holds, describe
	// themselves nowhere.
	header, skipped, err := aac.DecodeADTSHeader(bytes.NewReader(first))
	if err != nil {
		return StreamConfig{}, fmt.Errorf("%w: %s sample 0 holds no ADTS header: %v",
			ErrNoConfiguration, codec, err)
	}
	if skipped != 0 {
		return StreamConfig{}, fmt.Errorf("%w: %s sample 0 has %d bytes before its ADTS sync word, "+
			"so it is not an ADTS frame", ErrCodecMismatch, codec, skipped)
	}
	if frame := int(header.HeaderLength) + int(header.PayloadLength); frame > len(first) {
		return StreamConfig{}, fmt.Errorf("%w: %s sample 0 states an ADTS frame of %d bytes but holds %d",
			ErrSample, codec, frame, len(first))
	}
	rate := adtsFrequency(header.SamplingFrequencyIndex)
	if rate == 0 {
		return StreamConfig{}, fmt.Errorf("%w: %s sample 0 states sampling frequency index %d, "+
			"which names no rate", ErrCodecMismatch, codec, header.SamplingFrequencyIndex)
	}
	if header.ChannelConfig == 0 {
		return StreamConfig{}, fmt.Errorf("%w: %s sample 0 states channel configuration 0, "+
			"which means the mapping is inside the stream", ErrNoConfiguration, codec)
	}
	asc := aac.AudioSpecificConfig{
		ObjectType:           header.ObjectType,
		ChannelConfiguration: header.ChannelConfig,
		SamplingFrequency:    rate,
	}
	var payload bytes.Buffer
	if err := asc.Encode(&payload); err != nil {
		return StreamConfig{}, fmt.Errorf("%w: %s object type %d has no AudioSpecificConfig this can write: %v",
			ErrUnsupportedCodec, codec, header.ObjectType, err)
	}
	return StreamConfig{
		TrackConfig: TrackConfig{
			Kind: Audio, Channels: int(header.ChannelConfig), SampleRate: rate,
			AudioObjectType: header.ObjectType, CodecConfig: payload.Bytes(),
		},
		Profile: header.ObjectType,
		// The object type indication of MPEG-4 audio is 0x40, and the audio
		// object type follows it (RFC 6381).
		CodecString: fmt.Sprintf("%s.40.%d", codec, header.ObjectType),
	}, nil
}

// av1RegistrationID is the format identifier 'AV01' of the registration
// descriptor an MPEG-TS marks an AV1 stream with. AV1 has no stream type of its
// own: it travels as private data, and only that descriptor says what the
// private data is.
const av1RegistrationID = 0x41563031

// isAV1Stream reports whether an elementary stream of private data is an AV1
// one.
func isAV1Stream(es *astits.PMTElementaryStream) bool {
	for _, d := range es.ElementaryStreamDescriptors {
		if d != nil && d.Registration != nil && d.Registration.FormatIdentifier == av1RegistrationID {
			return true
		}
	}
	return false
}

// syncUnit reports whether an access unit of this track can be decoded on its
// own, which is what lets a player start there.
func (t *tsTrack) syncUnit(data []byte) bool {
	switch {
	case t.track.Kind == Audio:
		return true // every AAC frame decodes on its own
	case t.av1:
		return av1RandomAccess(data)
	default:
		return isSyncUnit(data, t.hevc)
	}
}

// av1RandomAccess reports whether an AV1 temporal unit is a random access
// point. A sequence header alone is not one — a stream may repeat it before
// every unit, and marking an inter frame as a starting point makes a player
// decode rubbish at every seek — so the frame header is read as well.
func av1RandomAccess(data []byte) bool {
	obus, err := av1.SplitOBUs(data)
	if err != nil {
		return false
	}
	var seq *av1.SequenceHeader
	for _, obu := range obus {
		if obu.Header.Type != av1.OBUSequenceHeader {
			continue
		}
		if seq, err = av1.ParseSequenceHeader(obu.Payload); err != nil {
			return false
		}
		break
	}
	if seq == nil {
		// Without a sequence header no decoder can set itself up here,
		// whatever the frame that follows is.
		return false
	}
	rap, err := av1.IsRAPSample(data, seq)
	return err == nil && rap
}

// deriveConfiguration fills in what a transport stream does not state. It
// carries no configuration record at all: a video track's frame size is in its
// parameter sets, and an AV1 track's whole av1C has to be built from the
// sequence header its samples carry.
//
// A stream this cannot read is left as it was: a track whose frame size is
// unknown is still worth handing over, and Muxer.AddTrack is the one that
// decides whether what it was given is enough to write.
func (t *tsTrack) deriveConfiguration() {
	if t.track.Kind != Video {
		return
	}
	var (
		cfg StreamConfig
		err error
	)
	switch {
	case t.av1:
		cfg, err = av1StreamConfig(t.config.Codec, t.samples)
	case t.hevc:
		// The parameter sets have already been lifted out of the samples,
		// which is where an MP4 wants them, so they are read from there.
		cfg, err = hevcStreamConfig(t.config.Codec, naluScan{sets: t.parameterSets()})
	default:
		cfg, err = avcStreamConfig(t.config.Codec, naluScan{sets: t.parameterSets()})
	}
	if err != nil {
		return
	}
	t.track.Width, t.track.Height = cfg.Width, cfg.Height
	t.config.Width, t.config.Height = cfg.Width, cfg.Height
	if len(t.config.CodecConfig) == 0 {
		t.config.CodecConfig = cfg.CodecConfig
	}
}

// parameterSets are the ones the track has already collected.
func (t *tsTrack) parameterSets() parameterSets {
	return parameterSets{sps: t.config.SPS, pps: t.config.PPS, vps: t.config.VPS}
}

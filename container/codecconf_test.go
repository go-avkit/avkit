// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/asticode/go-astits"
)

// The fixtures below are real parameter sets and real headers, so what is
// asserted about them is what a decoder would make of them. Each one says where
// it comes from and what it codes, because a configuration that is wrong by one
// field is not a test failure a player reports: it is a file that plays as
// garbage.
const (
	// An HEVC Main 10 SPS of 960x544 coded luma samples whose conformance
	// window drops two rows of chroma, so 540 lines are shown. From
	// Eyevinn/mp4ff hevc/sps_test.go.
	hevcSPSHex = "420101022000000300b0000003000003007ba0078200887db6718b92448053888892" +
		"cf24a69272c9124922dc91aa48fca223ff000100016a02020201"
	// A Main-profile level-4 VPS and the PPS of the same stream, from
	// Eyevinn/mp4ff hevc/vps_test.go and hevc/pps_test.go.
	hevcVPSHex = "40010c01ffff016000000300900000030000030078959809"
	hevcPPSHex = "4401c0f7c0cc90"
	// An AVC High-profile SPS coding 320x192 luma samples and cropping six
	// units off the bottom, so 320x180 is shown, and stating no aspect ratio
	// at all. From Eyevinn/mp4ff avc/sps_test.go.
	avcCroppedSPSHex = "6764000dacd941419f9e10000003001000000303c0f1429960"
	// An AV1 temporal unit: a temporal delimiter, the sequence header OBU of
	// AOM's av1-1-b8-23-film_grain-50.ivf (352x288, profile 0, 8-bit 4:2:0)
	// and a key frame. From Eyevinn/mp4ff av1/frame_test.go.
	av1KeyUnitHex = "1200" + "0a0b00000004457e3e7dfcc060" + "320110"
	// The same without a sequence header, carrying an inter frame.
	av1InterUnitHex = "1200" + "320130"
	// The av1C record the key unit's sequence header amounts to: marker and
	// version, profile 0 and level 0, then 4:2:0 8-bit colour, no initial
	// presentation delay, and the sequence header OBU itself.
	av1RecordHex = "81000c00" + "0a0b00000004457e3e7dfcc060"
)

// A VP9 profile-0 key frame coding 320x180, colour space unknown, studio range,
// and an inter frame that states nothing. From Eyevinn/mp4ff vp9/vp9_test.go.
var (
	vp9KeyFrame   = []byte{0x82, 0x49, 0x83, 0x42, 0x00, 0x13, 0xf0, 0x0b, 0x30}
	vp9InterFrame = []byte{0x86, 0x00}
	// The same header with frame_width_minus_1 and frame_height_minus_1
	// rewritten to 1919 and 1079: an uncompressed header states them as two
	// sixteen-bit fields straight after the four bits of colour configuration,
	// so 1920x1080 is 0x00 0x77 0xF0 0x43 0x70 where 320x180 was 0x00 0x13
	// 0xF0 0x0B 0x30.
	vp9HDKeyFrame = []byte{0x82, 0x49, 0x83, 0x42, 0x00, 0x77, 0xf0, 0x43, 0x70}
)

// mustHex decodes a fixture.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("fixture %q: %v", s, err)
	}
	return b
}

// avcIDR is an AVC picture coded without reference to another.
var avcIDR = []byte{0x65, 0x88, 0x84, 0x21}

// hevcIDR is the HEVC equivalent: NAL type 19, IDR_W_RADL.
var hevcIDR = []byte{0x26, 0x01, 0x88, 0x84}

// videoSamples turns coded units into samples a second long between them.
func videoSamples(datas ...[]byte) []Sample {
	out := make([]Sample, 0, len(datas))
	for i, data := range datas {
		out = append(out, Sample{Data: data, Duration: 3600, Sync: i == 0})
	}
	return out
}

func TestConfigFromAVCAnnexBSamples(t *testing.T) {
	sps, pps, width, height := avcParameterSets(t)
	// A transport stream repeats its parameter sets before every picture, so
	// the same sets arrive three times and must be named once.
	samples := videoSamples(
		annexB([]byte{0x09, 0x10}, sps[0], pps[0], avcIDR),
		annexB(sps[0], pps[0], avcIDR),
		annexB(sps[0], pps[0], avcIDR),
	)
	cfg, err := ConfigFromSamples("avc1", samples, SampleTimescale(TSTimescale), ConfigLanguage("fra"))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Kind != Video || cfg.Codec != "avc1" || cfg.Timescale != TSTimescale || cfg.Language != "fra" {
		t.Errorf("track = %+v", cfg.TrackConfig)
	}
	// The fixture's sample entry states 32x24, which is what its SPS codes
	// once the four cropping units are taken off the bottom of 32 rows.
	if cfg.Width != width || cfg.Height != height {
		t.Errorf("visible size = %dx%d, want %dx%d", cfg.Width, cfg.Height, width, height)
	}
	if cfg.CodedWidth != 32 || cfg.CodedHeight != 32 {
		t.Errorf("coded size = %dx%d, want 32x32", cfg.CodedWidth, cfg.CodedHeight)
	}
	if cfg.SARWidth != 1 || cfg.SARHeight != 1 {
		t.Errorf("aspect ratio = %d:%d, want 1:1", cfg.SARWidth, cfg.SARHeight)
	}
	if cfg.Profile != 100 || cfg.ProfileCompatibility != 0 || cfg.Level != 10 || cfg.Tier != 0 {
		t.Errorf("profile/level = %d/%d/%d tier %d, want 100/0/10 tier 0",
			cfg.Profile, cfg.ProfileCompatibility, cfg.Level, cfg.Tier)
	}
	if cfg.CodecString != "avc1.64000A" {
		t.Errorf("codec string = %q, want avc1.64000A", cfg.CodecString)
	}
	if len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("repeated parameter sets were kept: %d SPS, %d PPS", len(cfg.SPS), len(cfg.PPS))
	}
	if !bytes.Equal(cfg.SPS[0], sps[0]) || !bytes.Equal(cfg.PPS[0], pps[0]) {
		t.Errorf("the parameter sets came back changed")
	}
}

func TestConfigFromAVCLengthPrefixedSamples(t *testing.T) {
	sps, pps, width, height := avcParameterSets(t)
	samples := videoSamples(
		lengthPrefixed(sps[0], pps[0], avcIDR),
		lengthPrefixed(avcIDR),
	)
	cfg, err := ConfigFromSamples("avc3", samples, SampleTimescale(90000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Codec != "avc3" || cfg.Width != width || cfg.Height != height {
		t.Errorf("config = %+v", cfg.TrackConfig)
	}
	if cfg.Language != "" {
		t.Errorf("language = %q, want the empty string when none was stated", cfg.Language)
	}
}

func TestConfigFromAVCReportsTheVisibleSizeAndSquarePixels(t *testing.T) {
	_, pps, _, _ := avcParameterSets(t)
	sps := mustHex(t, avcCroppedSPSHex)
	cfg, err := ConfigFromSamples("avc1", videoSamples(annexB(sps, pps[0], avcIDR)),
		SampleTimescale(TSTimescale))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	// 320x192 is coded; six cropping units of two rows each are dropped, so
	// 320x180 is shown. Writing the coded size would letterbox the picture.
	if cfg.Width != 320 || cfg.Height != 180 {
		t.Errorf("visible size = %dx%d, want 320x180", cfg.Width, cfg.Height)
	}
	if cfg.CodedWidth != 320 || cfg.CodedHeight != 192 {
		t.Errorf("coded size = %dx%d, want 320x192", cfg.CodedWidth, cfg.CodedHeight)
	}
	// This SPS states no aspect ratio, which means square pixels, not zero.
	if cfg.SARWidth != 1 || cfg.SARHeight != 1 {
		t.Errorf("aspect ratio = %d:%d, want 1:1", cfg.SARWidth, cfg.SARHeight)
	}
	if cfg.Level != 13 || cfg.CodecString != "avc1.64000D" {
		t.Errorf("level = %d, codec string = %q", cfg.Level, cfg.CodecString)
	}
}

func TestConfigFromHEVCSamples(t *testing.T) {
	vps, sps, pps := mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex), mustHex(t, hevcPPSHex)
	samples := videoSamples(
		annexB([]byte{0x46, 0x01, 0x50}, vps, sps, pps, hevcIDR),
		annexB(hevcIDR),
	)
	cfg, err := ConfigFromSamples("hvc1", samples, SampleTimescale(90000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Kind != Video || cfg.Codec != "hvc1" {
		t.Errorf("track = %+v", cfg.TrackConfig)
	}
	// 544 rows are coded; the conformance window drops two chroma rows, which
	// is four luma rows, leaving the 540 a player shows.
	if cfg.Width != 960 || cfg.Height != 540 {
		t.Errorf("visible size = %dx%d, want 960x540", cfg.Width, cfg.Height)
	}
	if cfg.CodedWidth != 960 || cfg.CodedHeight != 544 {
		t.Errorf("coded size = %dx%d, want 960x544", cfg.CodedWidth, cfg.CodedHeight)
	}
	if cfg.Profile != 2 || cfg.Tier != 0 || cfg.Level != 123 {
		t.Errorf("profile/tier/level = %d/%d/%d, want 2/0/123 (Main 10, main tier, level 4.1)",
			cfg.Profile, cfg.Tier, cfg.Level)
	}
	if cfg.ProfileCompatibility != 0x20000000 {
		t.Errorf("compatibility flags = %#x, want 0x20000000", cfg.ProfileCompatibility)
	}
	if cfg.SARWidth != 1 || cfg.SARHeight != 1 {
		t.Errorf("aspect ratio = %d:%d, want 1:1", cfg.SARWidth, cfg.SARHeight)
	}
	if cfg.CodecString != "hvc1.2.4.L123.B0" {
		t.Errorf("codec string = %q, want hvc1.2.4.L123.B0", cfg.CodecString)
	}
	if len(cfg.VPS) != 1 || len(cfg.SPS) != 1 || len(cfg.PPS) != 1 {
		t.Fatalf("parameter sets = %d VPS, %d SPS, %d PPS", len(cfg.VPS), len(cfg.SPS), len(cfg.PPS))
	}
}

func TestConfigFromHEVCReadsTheHighTier(t *testing.T) {
	vps, pps := mustHex(t, hevcVPSHex), mustHex(t, hevcPPSHex)
	sps := mustHex(t, hevcSPSHex)
	// general_tier_flag is one bit of profile_tier_level, which starts at the
	// fourth byte of an SPS: two bits of general_profile_space, the tier, then
	// five bits of general_profile_idc. Setting it moves the same stream to
	// the high tier and shifts nothing else, so everything but the tier and
	// the codec string's H must read back unchanged.
	sps[3] |= 0x20
	cfg, err := ConfigFromSamples("hev1", videoSamples(annexB(vps, sps, pps, hevcIDR)),
		SampleTimescale(90000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Tier != 1 || cfg.Profile != 2 || cfg.Level != 123 {
		t.Errorf("profile/tier/level = %d/%d/%d, want 2/1/123", cfg.Profile, cfg.Tier, cfg.Level)
	}
	if cfg.CodecString != "hev1.2.4.H123.B0" {
		t.Errorf("codec string = %q, want hev1.2.4.H123.B0", cfg.CodecString)
	}
	if cfg.Width != 960 || cfg.Height != 540 {
		t.Errorf("size = %dx%d, want 960x540", cfg.Width, cfg.Height)
	}
}

func TestConfigFromAV1Samples(t *testing.T) {
	samples := videoSamples(mustHex(t, av1KeyUnitHex), mustHex(t, av1InterUnitHex))
	cfg, err := ConfigFromSamples("av01", samples, SampleTimescale(30000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Kind != Video || cfg.Codec != "av01" || cfg.Timescale != 30000 {
		t.Errorf("track = %+v", cfg.TrackConfig)
	}
	if cfg.Width != 352 || cfg.Height != 288 || cfg.CodedWidth != 352 || cfg.CodedHeight != 288 {
		t.Errorf("size = %dx%d (coded %dx%d), want 352x288", cfg.Width, cfg.Height, cfg.CodedWidth, cfg.CodedHeight)
	}
	if got, want := hex.EncodeToString(cfg.CodecConfig), av1RecordHex; got != want {
		t.Errorf("av1C = %s, want %s", got, want)
	}
	if cfg.Profile != 0 || cfg.Tier != 0 || cfg.Level != 0 {
		t.Errorf("profile/tier/level = %d/%d/%d, want 0/0/0", cfg.Profile, cfg.Tier, cfg.Level)
	}
	if cfg.CodecString != "av01.0.00M.08.0.110.02.02.02.0" {
		t.Errorf("codec string = %q", cfg.CodecString)
	}
	// The record has to be one the muxer takes, which is the whole point of
	// deriving it.
	if _, err := decodeAv1C(cfg.CodecConfig); err != nil {
		t.Errorf("the derived av1C is not one this package can read back: %v", err)
	}
}

func TestConfigFromVP9Samples(t *testing.T) {
	samples := videoSamples(vp9KeyFrame, vp9InterFrame, vp9InterFrame)
	cfg, err := ConfigFromSamples("vp09", samples, SampleTimescale(90000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Kind != Video || cfg.Codec != "vp09" {
		t.Errorf("track = %+v", cfg.TrackConfig)
	}
	if cfg.Width != 320 || cfg.Height != 180 {
		t.Errorf("size = %dx%d, want 320x180", cfg.Width, cfg.Height)
	}
	if cfg.VPx == nil {
		t.Fatal("no vpcC configuration was derived")
	}
	// Three samples of 3600 units at 90 kHz is 25 frames a second, and the
	// smallest VP9 level carrying 320x180 at 25 fps is 1.1, which the vpcC
	// table numbers 11.
	want := VPxConfig{Profile: 0, Level: 11, BitDepth: 8, ChromaSubsampling: 1,
		ColourPrimaries: 2, TransferCharacteristics: 2, MatrixCoefficients: 2}
	if *cfg.VPx != want {
		t.Errorf("vpcC = %+v, want %+v", *cfg.VPx, want)
	}
	if cfg.Profile != 0 || cfg.Level != 11 || cfg.CodecString != "vp09.00.11.08" {
		t.Errorf("profile %d level %d codec string %q", cfg.Profile, cfg.Level, cfg.CodecString)
	}
	// A vpcC this package cannot write would make the derivation pointless.
	if _, err := vpxConfig("vp09", cfg.TrackConfig); err != nil {
		t.Errorf("the derived vpcC is not one the muxer takes: %v", err)
	}
}

// TestConfigFromVP9LevelsForTheRateTheSamplesRunAt pins the one field of a vpcC
// record that is not in the bitstream at all: the level, which is the frame rate
// as much as the frame size. The same frame at two rates must level differently,
// or the rate is not being read.
func TestConfigFromVP9LevelsForTheRateTheSamplesRunAt(t *testing.T) {
	for name, tc := range map[string]struct {
		duration uint32
		want     byte
	}{
		"1920x1080 at 30 frames a second is level 4":   {3000, 40},
		"1920x1080 at 60 frames a second is level 4.1": {1500, 41},
	} {
		t.Run(name, func(t *testing.T) {
			samples := []Sample{
				{Data: vp9HDKeyFrame, Duration: tc.duration, Sync: true},
				{Data: vp9InterFrame, Duration: tc.duration},
			}
			cfg, err := ConfigFromSamples("vp09", samples, SampleTimescale(90000))
			if err != nil {
				t.Fatalf("ConfigFromSamples: %v", err)
			}
			if cfg.Width != 1920 || cfg.Height != 1080 {
				t.Fatalf("size = %dx%d, want 1920x1080", cfg.Width, cfg.Height)
			}
			if cfg.VPx.Level != tc.want || cfg.Level != tc.want {
				t.Errorf("level = %d, want %d", cfg.VPx.Level, tc.want)
			}
		})
	}
}

func TestConfigFromAACSamples(t *testing.T) {
	frame := adtsFrame(t, []byte{0x21, 0x22, 0x23, 0x24})
	samples := []Sample{
		{Data: frame, Duration: 1024, Sync: true},
		{Data: frame, Duration: 1024, Sync: true},
	}
	cfg, err := ConfigFromSamples("mp4a", samples, SampleTimescale(48000))
	if err != nil {
		t.Fatalf("ConfigFromSamples: %v", err)
	}
	if cfg.Kind != Audio || cfg.Codec != "mp4a" {
		t.Errorf("track = %+v", cfg.TrackConfig)
	}
	if cfg.SampleRate != 48000 || cfg.Channels != 2 || cfg.AudioObjectType != 2 {
		t.Errorf("audio = %d Hz, %d channels, object type %d; want 48000, 2, 2",
			cfg.SampleRate, cfg.Channels, cfg.AudioObjectType)
	}
	// The AudioSpecificConfig of AAC-LC at 48 kHz stereo: five bits of object
	// type 2, four of sampling frequency index 3, four of channel
	// configuration 2.
	if got := hex.EncodeToString(cfg.CodecConfig); got != "1190" {
		t.Errorf("AudioSpecificConfig = %s, want 1190", got)
	}
	if cfg.CodecString != "mp4a.40.2" {
		t.Errorf("codec string = %q, want mp4a.40.2", cfg.CodecString)
	}
	if cfg.Width != 0 || cfg.Height != 0 {
		t.Errorf("an audio track was given a frame size: %+v", cfg.TrackConfig)
	}
}

func TestConfigFromSamplesRefusesWhatItCannotDerive(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	avc := videoSamples(annexB(sps[0], pps[0], avcIDR))
	for name, tc := range map[string]struct {
		codec   string
		samples []Sample
		want    error
		says    string
	}{
		"no sample at all":     {"avc1", nil, ErrNoConfiguration, "no sample"},
		"a codec nobody named": {"mystery", avc, ErrUnsupportedCodec, "mystery"},
		"VP8, which has no level": {"vp08", videoSamples([]byte{0x9d, 0x01, 0x2a}),
			ErrUnsupportedCodec, "no level"},
		"Opus, whose header is in its container": {"opus", videoSamples([]byte{0x01}),
			ErrUnsupportedCodec, "identification header"},
		"AC-3":          {"ac-3", videoSamples([]byte{0x0b, 0x77}), ErrUnsupportedCodec, "bit stream information"},
		"Enhanced AC-3": {"ec-3", videoSamples([]byte{0x0b, 0x77}), ErrUnsupportedCodec, "bit stream information"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples(tc.codec, tc.samples)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

func TestConfigFromSamplesNamesTheCodecTheSamplesActuallyHold(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	avcSamples := videoSamples(annexB(sps[0], pps[0], avcIDR))
	hevcSamples := videoSamples(annexB(mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex),
		mustHex(t, hevcPPSHex), hevcIDR))

	// The two codecs spell a unit's type in different bits of its header, so
	// reading one as the other finds no parameter set at all. Saying which
	// mistake was made is worth more than reporting an absence.
	_, err := ConfigFromSamples("hvc1", avcSamples, SampleTimescale(90000))
	if !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("AVC read as HEVC: %v, want ErrCodecMismatch", err)
	}
	if !strings.Contains(err.Error(), "other NAL-based codec") {
		t.Errorf("error %q does not point at the other codec", err)
	}
	_, err = ConfigFromSamples("avc1", hevcSamples, SampleTimescale(90000))
	if !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("HEVC read as AVC: %v, want ErrCodecMismatch", err)
	}
	if !strings.Contains(err.Error(), "other NAL-based codec") {
		t.Errorf("error %q does not point at the other codec", err)
	}
}

func TestConfigFromSamplesRefusesMissingParameterSets(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	vps, hsps, hpps := mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex), mustHex(t, hevcPPSHex)
	for name, tc := range map[string]struct {
		codec   string
		samples []Sample
		says    string
	}{
		"AVC without a PPS":  {"avc1", videoSamples(annexB(sps[0], avcIDR)), "1 SPS and 0 PPS"},
		"AVC without an SPS": {"avc1", videoSamples(annexB(pps[0], avcIDR)), "0 SPS and 1 PPS"},
		"HEVC without a VPS": {"hvc1", videoSamples(annexB(hsps, hpps, hevcIDR)), "0 VPS"},
		"HEVC without a PPS": {"hvc1", videoSamples(annexB(vps, hsps, hevcIDR)), "0 PPS"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples(tc.codec, tc.samples, SampleTimescale(90000))
			if !errors.Is(err, ErrNoConfiguration) {
				t.Fatalf("error = %v, want ErrNoConfiguration", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

func TestConfigFromSamplesRefusesUnreadableNalus(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	sound := annexB(sps[0], pps[0], avcIDR)
	for name, tc := range map[string]struct {
		codec   string
		samples []Sample
		want    error
		says    string
	}{
		"no sample long enough to hold a unit": {
			"avc1", videoSamples([]byte{0x65}, []byte{0x41, 0x9a}), ErrNoConfiguration, "long enough",
		},
		"a length reaching past the sample": {
			"avc1", videoSamples([]byte{0x00, 0x00, 0x00, 0x10, 0x65, 0x88}), ErrSample, "do not describe",
		},
		"a sample with no start code": {
			"avc1", videoSamples(sound, []byte{0xde, 0xad, 0xbe, 0xef}), ErrSample, "no start code",
		},
		"a unit too short for an HEVC header": {
			"hvc1", videoSamples(annexB(mustHex(t, hevcVPSHex), []byte{0x26})), ErrSample, "too short",
		},
		"a unit that sets forbidden_zero_bit": {
			"avc1", videoSamples(annexB(sps[0], []byte{0x85, 0x01})), ErrCodecMismatch, "forbidden_zero_bit",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples(tc.codec, tc.samples, SampleTimescale(90000))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

func TestConfigFromSamplesRefusesUnparseableParameterSets(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	vps, hsps, hpps := mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex), mustHex(t, hevcPPSHex)
	// A unit of the right type whose payload stops immediately: the type says
	// what it claims to be, the bits say it cannot be read.
	for name, tc := range map[string]struct {
		codec   string
		samples []Sample
		want    error
		says    string
	}{
		"an AVC SPS that stops at its header": {
			"avc1", videoSamples(annexB([]byte{0x67}, pps[0], avcIDR)), ErrCodecMismatch, "SPS 0 does not parse",
		},
		"an AVC PPS that stops at its header": {
			"avc1", videoSamples(annexB(sps[0], []byte{0x68}, avcIDR)), ErrTrackConfig, "PPS 0 does not parse",
		},
		"an HEVC VPS that stops at its header": {
			"hvc1", videoSamples(annexB([]byte{0x40, 0x01}, hsps, hpps)), ErrCodecMismatch, "VPS 0 does not parse",
		},
		"an HEVC SPS that stops at its header": {
			"hvc1", videoSamples(annexB(vps, []byte{0x42, 0x01}, hpps)), ErrCodecMismatch, "SPS 0 does not parse",
		},
		"an HEVC PPS that stops at its header": {
			"hvc1", videoSamples(annexB(vps, hsps, []byte{0x44, 0x01})), ErrTrackConfig, "PPS 0 does not parse",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples(tc.codec, tc.samples, SampleTimescale(90000))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

func TestConfigFromAV1RefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct {
		samples []Sample
		want    error
		says    string
	}{
		"an OBU header that cannot be read": {
			videoSamples([]byte{0x80, 0x00}), ErrCodecMismatch, "does not split into OBUs",
		},
		"a sequence header with no payload": {
			videoSamples(mustHex(t, "0a00")), ErrCodecMismatch, "does not parse as a sequence header",
		},
		"no sequence header at all": {
			videoSamples(mustHex(t, av1InterUnitHex), mustHex(t, av1InterUnitHex)),
			ErrNoConfiguration, "sequence header OBU",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples("av01", tc.samples, SampleTimescale(30000))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

// TestConfigFromAV1GuardsItsRecord exercises the two failures that cannot be
// reached through the library as it stands: an av1C record is encoded into a
// buffer sized from the record itself, and decoded straight back by this
// package's own decoder.
func TestConfigFromAV1GuardsItsRecord(t *testing.T) {
	samples := videoSamples(mustHex(t, av1KeyUnitHex))

	original := encodeAv1Record
	defer func() { encodeAv1Record = original }()
	encodeAv1Record = func(*av1.CodecConfRec, io.Writer) error { return errors.New("no room") }
	_, err := ConfigFromSamples("av01", samples, SampleTimescale(30000))
	if !errors.Is(err, ErrTrackConfig) || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("a record that cannot be encoded: %v", err)
	}
	encodeAv1Record = original

	decoder := decodeAv1CBox
	defer func() { decodeAv1CBox = decoder }()
	decodeAv1CBox = func(mp4.BoxHeader, uint64, io.Reader) (mp4.Box, error) {
		return nil, errors.New("not a record")
	}
	if _, err := ConfigFromSamples("av01", samples, SampleTimescale(30000)); !errors.Is(err, ErrTrackConfig) {
		t.Fatalf("a record this package cannot read back: %v", err)
	}
}

func TestConfigFromVP9RefusesWhatItCannotLevel(t *testing.T) {
	for name, tc := range map[string]struct {
		samples   []Sample
		timescale uint32
		want      error
		says      string
	}{
		"without a timescale there is no frame rate": {
			videoSamples(vp9KeyFrame), 0, ErrNoConfiguration, "timescale",
		},
		"a sample without a duration has no rate either": {
			[]Sample{{Data: vp9KeyFrame}}, 90000, ErrSample, "no duration",
		},
		"data that is not a VP9 frame": {
			videoSamples([]byte{0x00, 0x00}), 90000, ErrCodecMismatch, "uncompressed header",
		},
		"nothing but inter frames": {
			videoSamples(vp9InterFrame, vp9InterFrame), 90000, ErrNoConfiguration, "key frame",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples("vp09", tc.samples, SampleTimescale(tc.timescale))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

func TestConfigFromAACRefusesWhatIsNotADTS(t *testing.T) {
	frame := adtsFrame(t, []byte{0x21, 0x22, 0x23, 0x24})
	// The header's own bits are patched so each refusal is reached with a
	// header that is otherwise sound.
	patch := func(f func(b []byte)) []byte {
		b := append([]byte(nil), frame...)
		f(b)
		return b
	}
	for name, tc := range map[string]struct {
		data []byte
		want error
		says string
	}{
		"a raw AAC frame, which describes itself nowhere": {
			[]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22}, ErrNoConfiguration, "no ADTS header",
		},
		"a sync word that is not at the start": {
			append([]byte{0x00}, frame...), ErrCodecMismatch, "before its ADTS sync word",
		},
		"a frame shorter than its header states": {
			frame[:len(frame)-2], ErrSample, "states an ADTS frame",
		},
		"a sampling frequency index that names no rate": {
			patch(func(b []byte) { b[2] = b[2]&0xc3 | 13<<2 }), ErrCodecMismatch, "index 13",
		},
		"a channel configuration of zero": {
			patch(func(b []byte) { b[2] &= 0xfe; b[3] &= 0x3f }), ErrNoConfiguration, "configuration 0",
		},
		"an object type with no AudioSpecificConfig": {
			patch(func(b []byte) { b[2] &= 0x3f }), ErrUnsupportedCodec, "object type 1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ConfigFromSamples("mp4a", []Sample{{Data: tc.data, Duration: 1024}},
				SampleTimescale(48000))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

// TestDerivedConfigWritesAReadableFile is the whole point of the exercise:
// samples with no container-level description of any kind become a file whose
// dimensions and sample count read back exactly.
func TestDerivedConfigWritesAReadableFile(t *testing.T) {
	sps, pps, _, _ := avcParameterSets(t)
	vps, hsps, hpps := mustHex(t, hevcVPSHex), mustHex(t, hevcSPSHex), mustHex(t, hevcPPSHex)
	for name, tc := range map[string]struct {
		codec         string
		samples       []Sample
		width, height int
	}{
		"AVC as a transport stream carries it": {
			"avc1", videoSamples(
				annexB(sps[0], pps[0], avcIDR),
				annexB(avcIDR),
				annexB(sps[0], pps[0], avcIDR),
			), 32, 24,
		},
		"HEVC as a transport stream carries it": {
			"hvc1", videoSamples(
				annexB(vps, hsps, hpps, hevcIDR),
				annexB(hevcIDR),
				annexB(hevcIDR),
			), 960, 540,
		},
		"AV1 as a stream of temporal units": {
			"av01", videoSamples(
				mustHex(t, av1KeyUnitHex),
				mustHex(t, av1InterUnitHex),
				mustHex(t, av1InterUnitHex),
			), 352, 288,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ConfigFromSamples(tc.codec, tc.samples, SampleTimescale(TSTimescale))
			if err != nil {
				t.Fatalf("ConfigFromSamples: %v", err)
			}
			var buf bytes.Buffer
			m := NewMuxer(&buf)
			id, err := m.AddTrack(cfg.TrackConfig)
			if err != nil {
				t.Fatalf("AddTrack: %v", err)
			}
			for i, s := range tc.samples {
				// The parameter sets belong in the sample entry, and an MP4
				// separates its units by length rather than start code.
				data := s.Data
				if len(cfg.SPS) > 0 {
					data, _ = convertAnnexB(data, tc.codec == "hvc1")
				}
				if err := m.WriteSample(id, Sample{Data: data, Duration: s.Duration, Sync: s.Sync}); err != nil {
					t.Fatalf("WriteSample %d: %v", i, err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			r, err := NewReader(buf.Bytes())
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			track := r.File().Tracks[0]
			if track.Codec != tc.codec || track.Width != tc.width || track.Height != tc.height {
				t.Errorf("read back %q %dx%d, want %q %dx%d",
					track.Codec, track.Width, track.Height, tc.codec, tc.width, tc.height)
			}
			samples, err := r.Samples(track.ID)
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(samples) != len(tc.samples) {
				t.Errorf("read back %d samples, wrote %d", len(samples), len(tc.samples))
			}
		})
	}
}

// av1TSFixture builds a transport stream carrying AV1, which has no stream type
// of its own: it travels as private data marked by a registration descriptor.
func av1TSFixture(t *testing.T, units ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := astits.NewMuxer(context.Background(), &buf)
	if err := m.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256, StreamType: astits.StreamTypePrivateData,
		ElementaryStreamDescriptors: []*astits.Descriptor{{
			Tag: astits.DescriptorTagRegistration, Length: 4,
			Registration: &astits.DescriptorRegistration{FormatIdentifier: av1RegistrationID},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	m.SetPCRPID(256)
	for i, unit := range units {
		if _, err := m.WriteData(&astits.MuxerData{PID: 256, PES: &astits.PESData{
			Header: &astits.PESHeader{StreamID: 0xBD, OptionalHeader: &astits.PESOptionalHeader{
				MarkerBits:      2,
				PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
				PTS:             &astits.ClockReference{Base: int64(i) * 3000},
			}},
			Data: unit,
		}}); err != nil {
			t.Fatalf("write unit %d: %v", i, err)
		}
	}
	return buf.Bytes()
}

func TestTransportStreamCarryingAV1IsRemuxable(t *testing.T) {
	key, inter := mustHex(t, av1KeyUnitHex), mustHex(t, av1InterUnitHex)
	data := av1TSFixture(t, key, inter, inter)
	r, err := NewReader(data)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	ids := r.TrackIDs()
	if len(ids) != 1 {
		t.Fatalf("tracks = %+v", r.File().Tracks)
	}
	cfg, err := r.TrackConfig(ids[0])
	if err != nil {
		t.Fatalf("TrackConfig: %v", err)
	}
	// Before this, a transport stream holding AV1 could not be remuxed at
	// all: there is no av1C anywhere in it to hand over.
	if cfg.Codec != "av01" || cfg.Width != 352 || cfg.Height != 288 {
		t.Errorf("config = %+v", cfg)
	}
	if got := hex.EncodeToString(cfg.CodecConfig); got != av1RecordHex {
		t.Errorf("av1C = %s, want %s", got, av1RecordHex)
	}
	samples, err := r.Samples(ids[0])
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	// Only the unit carrying a key frame can be started at. A sequence header
	// alone would not do: streams repeat it, and marking an inter frame as a
	// starting point makes a player decode rubbish at every seek.
	for i, want := range []bool{true, false, false} {
		if samples[i].Sync != want {
			t.Errorf("sample %d sync = %v, want %v", i, samples[i].Sync, want)
		}
	}
	if !bytes.Equal(samples[0].Data, key) {
		t.Errorf("the temporal unit came back changed")
	}

	var buf bytes.Buffer
	m := NewMuxer(&buf)
	id, err := m.AddTrack(cfg)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	for i, s := range samples {
		if err := m.WriteSample(id, s); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out, err := NewReader(buf.Bytes())
	if err != nil {
		t.Fatalf("NewReader(mp4): %v", err)
	}
	track := out.File().Tracks[0]
	if track.Codec != "av01" || track.Width != 352 || track.Height != 288 {
		t.Errorf("remuxed track = %+v", track)
	}
	written, err := out.Samples(track.ID)
	if err != nil {
		t.Fatalf("Samples(mp4): %v", err)
	}
	if len(written) != 3 {
		t.Errorf("remuxed %d samples, read 3", len(written))
	}
}

func TestTransportStreamStatesTheFrameSizeItsSPSCodes(t *testing.T) {
	// A transport stream carries no dimensions anywhere: they are in the SPS.
	file, err := Demux(tsFixture(t, 2))
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	video := file.VideoTracks()
	if len(video) != 1 {
		t.Fatalf("tracks = %+v", file.Tracks)
	}
	if video[0].Width != 32 || video[0].Height != 24 {
		t.Errorf("size = %dx%d, want 32x24", video[0].Width, video[0].Height)
	}
	// An audio track has no frame size to derive, and no derivation to do.
	audio := file.AudioTracks()
	if len(audio) != 1 || audio[0].Width != 0 {
		t.Errorf("audio = %+v", audio)
	}
}

func TestAV1RandomAccessReadsTheFrameNotJustTheHeader(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want bool
	}{
		"a sequence header and a key frame":    {mustHex(t, av1KeyUnitHex), true},
		"an inter frame with no header":        {mustHex(t, av1InterUnitHex), false},
		"OBUs that cannot be split":            {[]byte{0x80, 0x00}, false},
		"a sequence header with no payload":    {mustHex(t, "0a00"), false},
		"a sequence header and an inter frame": {mustHex(t, "0a0b00000004457e3e7dfcc060"+"320130"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := av1RandomAccess(tc.data); got != tc.want {
				t.Errorf("av1RandomAccess = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAV1RandomAccessNeedsAReadableFrameHeader covers the frame whose header
// cannot be read at all, which IsRAPSample reports as an error rather than as
// "not a random access point".
func TestAV1RandomAccessNeedsAReadableFrameHeader(t *testing.T) {
	// A sequence header, then a frame OBU with an empty payload.
	if av1RandomAccess(mustHex(t, "0a0b00000004457e3e7dfcc060"+"3200")) {
		t.Error("a frame with no header was taken for a random access point")
	}
}

func TestUnknownStreamTypesStayUnknown(t *testing.T) {
	// Private data is only AV1 when a registration descriptor says so.
	if isAV1Stream(&astits.PMTElementaryStream{ElementaryPID: 258,
		StreamType: astits.StreamTypePrivateData}) {
		t.Error("private data with no descriptor was taken for AV1")
	}
	if !isAV1Stream(&astits.PMTElementaryStream{ElementaryPID: 258,
		StreamType: astits.StreamTypePrivateData,
		ElementaryStreamDescriptors: []*astits.Descriptor{
			nil,
			{Tag: astits.DescriptorTagMaximumBitrate},
			{Tag: astits.DescriptorTagRegistration,
				Registration: &astits.DescriptorRegistration{FormatIdentifier: av1RegistrationID}},
		}}) {
		t.Error("a registration descriptor naming AV01 was not recognised")
	}
}

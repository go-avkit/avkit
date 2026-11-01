// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

// The two sampling frequencies of an AAC track, as Matroska states them.
//
// An HE-AAC stream is an AAC core coded at half the rate it is heard at, and
// spectral band replication (SBR) restores the upper band: 22.05 kHz of AAC
// decoding to 44.1 kHz of audio. Matroska has an element for each,
// SamplingFrequency and OutputSamplingFrequency (RFC 9559 §5.1.4.1.29.1 and
// .29.2, "Real output sampling frequency in Hz that is used for Spectral Band
// Replication (SBR) techniques"), and ffmpeg's matroskaenc writes the
// AudioSpecificConfig's own rates into them (get_aac_sample_rates): the core
// rate as SamplingFrequency, and the extension rate, when the record signals
// SBR, as OutputSamplingFrequency.
//
// The AudioSpecificConfig syntax (ISO/IEC 14496-3 Table 1.15) signals SBR in
// one of two ways: hierarchically, with audioObjectType 5
// (SBR) or 29 (PS) followed by the extension sampling frequency and the core's
// own object type; or backward compatibly, after an ordinary AAC-LC
// configuration, with a syncExtensionType of 0x2B7, extensionAudioObjectType 5
// and sbrPresentFlag set, then the extension sampling frequency. The second is
// what Apple's encoders write -- 13 90 56 e5 a0 is AAC-LC at 22050 Hz, then SBR
// at 44100 -- and the one mp4ff's decoder does not read.
//
// This reads both the way ffmpeg's ff_mpeg4audio_get_config_gb does, because
// ffmpeg is what the result is measured against: the header fields, then for a
// core of AAC Main, LC, SSR or LTP, or ER AAC LC, LD or ELD, the frameLengthFlag,
// then, if no hierarchical SBR was signalled, a bit-by-bit search for the sync
// extension while more than 15 bits remain. A sync extension stating an
// extension rate equal to the core's still states it. Bits read past the end of
// the record read as zero, as ffmpeg's bit reader reads its padding. ffmpeg's
// reading of ALS and USAC, which move the rate or read more of the record, is
// not repeated: aac.DecodeAudioSpecificConfig refuses both, so no record of
// either reaches the muxer.

// The audio object types and the sync extension this reads.
const (
	aacObjectMain        = 1
	aacObjectLC          = 2
	aacObjectSSR         = 3
	aacObjectLTP         = 4
	aacObjectERLC        = 17
	aacObjectERLD        = 23
	aacObjectERELD       = 39
	aacObjectSBR         = 5
	aacObjectPS          = 29
	aacObjectEscape      = 31
	aacSyncExtensionType = 0x2B7
)

// ascBits reads an AudioSpecificConfig most significant bit first.
type ascBits struct {
	data []byte
	pos  int
}

// left is how many bits remain.
func (b *ascBits) left() int { return len(b.data)*8 - b.pos }

// peek is the next n bits, without consuming them; a bit past the end is zero.
func (b *ascBits) peek(n int) uint32 {
	var v uint32
	for i := range n {
		v <<= 1
		if at := b.pos + i; at < len(b.data)*8 {
			v |= uint32(b.data[at/8]>>(7-at%8)) & 1
		}
	}
	return v
}

// read is the next n bits.
func (b *ascBits) read(n int) uint32 {
	v := b.peek(n)
	b.pos += n
	return v
}

// objectType is GetAudioObjectType(): five bits, and six more after the escape.
func (b *ascBits) objectType() uint32 {
	if t := b.read(5); t != aacObjectEscape {
		return t
	}
	return 32 + b.read(6)
}

// frequency is a sampling frequency index and, for index 15, the 24-bit
// frequency that follows it. A reserved index states no frequency.
func (b *ascBits) frequency() int {
	if index := b.read(4); index != 0xF {
		return adtsFrequency(byte(index))
	}
	return int(b.read(24))
}

// aacRates is the core sampling frequency an AudioSpecificConfig states and,
// when it signals SBR, the output one; zero output when it does not.
func aacRates(asc []byte) (core, output int) {
	s := readASC(asc)
	return s.core, s.output
}

// aacPSSyncExtensionType is the psSyncExtensionType of ISO/IEC 14496-3
// Table 1.15, which follows the SBR extension of a backward-compatible
// HE-AACv2 record and is followed by the one-bit psPresentFlag.
const aacPSSyncExtensionType = 0x548

// ascSignalling is what an AudioSpecificConfig says about its core and about
// the extensions that make the output larger than the core: spectral band
// replication, which doubles the rate, and parametric stereo, which doubles
// the channels of a mono core.
type ascSignalling struct {
	object    uint32 // the audioObjectType as stated first: 5 and 29 are hierarchical signalling
	inner     uint32 // the core's own audioObjectType, equal to object unless that is 5 or 29
	coreIndex uint32 // the core's samplingFrequencyIndex, 15 when the rate is stated in full
	core      int    // the core's sampling frequency
	output    int    // the extension sampling frequency, 0 when SBR is not signalled
	config    uint32 // channelConfiguration, of the core
	ps        bool   // parametric stereo is signalled, and the core is mono
}

// readASC reads the signalling of a record the way ffmpeg's
// ff_mpeg4audio_get_config_gb does: the header fields; then, for audio object
// type 5 or 29, the extension rate and the core's own object type; and
// otherwise, after the core's GASpecificConfig, a bit-by-bit search for the
// backward-compatible sync extension and the PS one behind it. ffmpeg only keeps
// PS for a mono core ("c->channels & ~0x01"), and so does this.
func readASC(asc []byte) ascSignalling {
	b := &ascBits{data: asc}
	var s ascSignalling
	s.object = b.objectType()
	s.coreIndex = b.peek(4)
	s.core = b.frequency()
	s.config = b.read(4)
	s.inner = s.object
	if s.object == aacObjectSBR || s.object == aacObjectPS {
		// ffmpeg's test for the "MP3onMP4" draft that reuses 29 is left out:
		// it needs the six bits after the extension index to be zero, and
		// every record reaching here has passed aac.DecodeAudioSpecificConfig,
		// which requires the core object type there to be AAC-LC, 00010.
		s.output = b.frequency()
		s.inner = b.objectType()
		s.ps = s.object == aacObjectPS && s.config == 1
		return s
	}
	switch s.object {
	case aacObjectMain, aacObjectLC, aacObjectSSR, aacObjectLTP, aacObjectERLC, aacObjectERLD, aacObjectERELD:
		b.read(1) // frameLengthFlag, the first bit of GASpecificConfig
	}
	for b.left() > 15 {
		if b.peek(11) != aacSyncExtensionType {
			b.read(1)
			continue
		}
		b.read(11)
		if b.objectType() == aacObjectSBR && b.read(1) == 1 {
			s.output = b.frequency()
			if b.left() > 11 && b.read(11) == aacPSSyncExtensionType {
				s.ps = b.read(1) == 1 && s.config == 1
			}
		}
		break
	}
	return s
}

// aacOutputChannels is how many channels an AudioSpecificConfig's stream
// plays as: the core's channelConfiguration (Table 1.19), except that a mono
// core with parametric stereo signalled is stereo. The two differ for every
// HE-AACv2 file, and the channelConfiguration is the one that is wrong for a
// listener: ffmpeg states 2 in the sample entry of an MP4 and in the Channels
// element of a Matroska file, and so does Apple's encoder. Zero means the
// bitstream states the layout.
func aacOutputChannels(asc []byte) int {
	s := readASC(asc)
	n := aacChannels(byte(s.config))
	if n == 1 && s.ps {
		return 2
	}
	return n
}

// adtsParameters is what an ADTS header states of a track whose record is asc,
// as ffmpeg's adtsenc.c reads them from the extradata (adts_decode_extradata):
// the object type of the CORE and its sampling frequency index, never the
// extension's, because an ADTS header has no place for SBR or PS and the
// decoder finds them in the frames. ok is false for a record no ADTS header
// can state: a core other than Main, LC, SSR or LTP, or a rate written in full.
func adtsParameters(asc []byte) (objectType byte, rate int, configuration byte, ok bool) {
	s := readASC(asc)
	if s.inner < aacObjectMain || s.inner > aacObjectLTP || s.coreIndex == 0xF || s.config > 7 || s.core == 0 {
		return 0, 0, 0, false
	}
	return byte(s.inner), s.core, byte(s.config), true
}

// webmAACRates states an AAC track's rates as ffmpeg's matroskaenc states them:
// the core rate as SamplingFrequency, and the output rate, when the record
// signals SBR, as OutputSamplingFrequency. The record has a core rate: it was
// either built from the track's own or decoded by aac.DecodeAudioSpecificConfig,
// which refuses a reserved frequency index. An extension index that is reserved
// states no output rate, and matroskaenc writes none for it either.
func webmAACRates(audio *webmAudio, asc []byte) {
	core, output := aacRates(asc)
	audio.SamplingFrequency = float64(core)
	if output > 0 {
		audio.OutputSamplingFrequency = float64(output)
	}
}

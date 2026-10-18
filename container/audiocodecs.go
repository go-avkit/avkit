// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"fmt"
	"math"

	"github.com/Eyevinn/mp4ff/mp4"
)

// FLAC and MPEG audio in an MP4, and FLAC, MPEG audio, AC-3 and Enhanced AC-3
// in Matroska. Each is written the way its own mapping says, and nothing here
// decodes a sample: what a container needs is the configuration, and both
// codecs state it in a header this file reads.
//
// FLAC (RFC 9639) states its configuration once, ahead of the first frame: the
// "fLaC" signature and the metadata blocks, of which STREAMINFO comes first.
// That is the form TrackConfig.CodecConfig holds it in, because it is what
// Matroska carries as CodecPrivate (RFC 9639 10.2) and what a native .flac file
// begins with; an MP4 carries the same blocks without the signature in a dfLa
// box ("Encapsulation of FLAC in ISO Base Media File Format", Xiph, 3.3.2), and
// is converted, never copied.
//
// MPEG audio (ISO/IEC 11172-3 and 13818-3) states its configuration in every
// frame header and nowhere else. An MP4 carries it in an mp4a sample entry
// whose esds names the standard by object type indication, 0x6B or 0x69 in the
// MP4RA registry, and holds no decoder specific info: ISO/IEC 14496-3 defines
// none for either, which FFmpeg's isom.c cites (9.D.2.2) when it reads one.
//
// ⛔ Vorbis is not here on purpose: MP4RA registers no sample entry for it, so
// there is no box a player would recognise, and Muxer refuses it by name.

// The object type indications of MPEG audio in the MP4RA registry ("oti"):
// 0x6B is ISO/IEC 11172-3, the 32, 44.1 and 48 kHz of MPEG-1, and 0x69 is
// ISO/IEC 13818-3, the lower rates MPEG-2 adds. FFmpeg's movenc.c chooses
// between them the same way, by sample rate.
const (
	mpaObjectTypeMPEG1 byte = 0x6B
	mpaObjectTypeMPEG2 byte = 0x69
)

// mpaObjectType is the object type indication an MP4 states MPEG audio at this
// sample rate with. The "MPEG 2.5" rates belong to neither standard, and so to
// no object type: they are refused, as FFmpeg's movenc.c refuses MP3 below
// 16 kHz in an MP4.
func mpaObjectType(rate int) (byte, bool) {
	for id, rates := range mpaRates {
		for _, r := range rates[:3] {
			if r == rate {
				if id == 1 {
					return mpaObjectTypeMPEG1, true
				}
				return mpaObjectTypeMPEG2, true
			}
		}
	}
	return 0, false
}

// isMPEGAudioEsds reports whether an esds names MPEG audio rather than AAC.
func isMPEGAudioEsds(esds *mp4.EsdsBox) bool {
	dec := esds.DecConfigDescriptor
	return dec != nil && (dec.ObjectType == mpaObjectTypeMPEG1 || dec.ObjectType == mpaObjectTypeMPEG2)
}

// describeMPEGAudio writes the mp4a sample entry of an MPEG audio track: an
// esds naming the standard, and no decoder specific info.
func describeMPEGAudio(trak *mp4.TrakBox, codec string, cfg TrackConfig) error {
	oti, ok := mpaObjectType(cfg.SampleRate)
	switch {
	case !ok:
		return fmt.Errorf("%w: %s at %d Hz: an MP4 states MPEG audio at the rates of ISO/IEC 11172-3 "+
			"(object type 0x6B) or ISO/IEC 13818-3 (0x69), and this is neither", ErrTrackConfig, codec, cfg.SampleRate)
	case cfg.Channels < 1 || cfg.Channels > 2:
		return fmt.Errorf("%w: %s needs a channel count of 1 or 2, not %d", ErrTrackConfig, codec, cfg.Channels)
	}
	esds := mp4.CreateEsdsBox(nil)
	esds.DecConfigDescriptor.ObjectType = oti
	esds.DecConfigDescriptor.DecSpecificInfo = nil
	entry := mp4.CreateAudioSampleEntryBox("mp4a", uint16(cfg.Channels), 16, uint16(cfg.SampleRate), esds)
	trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	return nil
}

// mpegAudioTrackConfig names an MP4's MPEG audio track after its frames. The
// esds says which standard and not which layer, and the layer is what decides
// whether Matroska calls the track A_MPEG/L2 or A_MPEG/L3, so it is read off
// the first frame, as FFmpeg's parser does. The frame is also what states the
// rate and channel count; the sample entry's are template fields.
func mpegAudioTrackConfig(cfg *TrackConfig, samples []Sample, err error) error {
	if err != nil {
		return fmt.Errorf("%w: an MPEG audio track is named by its first frame, which cannot be read: %v",
			ErrTrackConfig, err)
	}
	f, err := parseMPAFrame(samples[0].Data)
	if err != nil {
		return fmt.Errorf("%w: the first sample of an MPEG audio track is not a frame: %v", ErrTrackConfig, err)
	}
	cfg.Codec = f.Codec()
	cfg.SampleRate, cfg.Channels = f.SampleRate, f.Channels
	return nil
}

// mpaStreamConfig reads an MPEG audio track's configuration off its frame
// headers. Every sample has to be a frame of the stated layer at one sampling
// frequency: a stream that changes either is not one track.
func mpaStreamConfig(codec string, samples []Sample) (StreamConfig, error) {
	var first mpaFrame
	for i, s := range samples {
		f, err := parseMPAFrame(s.Data)
		if err != nil {
			return StreamConfig{}, fmt.Errorf("%s sample %d: %w", codec, i, err)
		}
		switch {
		case f.Codec() != codec:
			return StreamConfig{}, fmt.Errorf("%w: %s sample %d is a Layer %d frame, which is %s",
				ErrCodecMismatch, codec, i, f.Layer, f.Codec())
		case i == 0:
			first = f
		case f.SampleRate != first.SampleRate:
			return StreamConfig{}, fmt.Errorf("%w: %s sample %d is at %d Hz and the first at %d Hz",
				ErrCodecMismatch, codec, i, f.SampleRate, first.SampleRate)
		}
	}
	// Every rate parseMPAFrame accepts has an object type.
	oti, _ := mpaObjectType(first.SampleRate)
	return StreamConfig{
		TrackConfig: TrackConfig{Kind: Audio, SampleRate: first.SampleRate, Channels: first.Channels},
		// RFC 6381 3.3: an mp4a whose object type is not 0x40 is named by
		// the object type alone, in hexadecimal.
		CodecString: fmt.Sprintf("mp4a.%02X", oti),
	}, nil
}

// ac3StreamConfig reads an AC-3 or Enhanced AC-3 track's configuration off its
// first sync frame, which states everything the dac3 and dec3 records hold. A
// stated codec the frames contradict is reported rather than written: an ec-3
// sample entry around AC-3 frames is one no decoder sets up correctly.
func ac3StreamConfig(codec string, samples []Sample) (StreamConfig, error) {
	for i, s := range samples {
		f, err := parseAC3Frame(s.Data)
		if err != nil {
			continue
		}
		if f.EAC3 != (codec == "ec-3") {
			return StreamConfig{}, fmt.Errorf("%w: %s sample %d is a sync frame of bsid %d",
				ErrCodecMismatch, codec, i, f.BSID)
		}
		break
	}
	got, err := ac3TrackConfig(codec, samples)
	if err != nil {
		return StreamConfig{}, err
	}
	// ETSI TS 102 366 Annex F.4 names both by their sample entry.
	return StreamConfig{TrackConfig: got, CodecString: codec}, nil
}

// The FLAC metadata this package reads: RFC 9639 8.1 and 8.2.
const (
	flacSignature       = "fLaC"
	flacBlockHeaderSize = 4
	flacStreamInfo      = 0   // block type
	flacForbiddenBlock  = 127 // forbidden, to avoid confusion with a frame sync code
	flacStreamInfoSize  = 34
	flacMinBitDepth     = 4
)

// flacStreamFormat is what STREAMINFO states that a container repeats.
type flacStreamFormat struct {
	SampleRate, Channels, BitDepth int
}

// parseFLACConfig takes a FLAC configuration — the signature and every
// metadata block — apart, and reads the format out of its STREAMINFO.
//
// It is strict where a mistake would be silent: a configuration that stops
// before the block marked last is a truncated one, and bytes after that block
// are audio frames, neither of which belongs in a sample entry.
func parseFLACConfig(config []byte) ([]mp4.FLACMetadataBlock, flacStreamFormat, error) {
	var format flacStreamFormat
	if len(config) < len(flacSignature) || string(config[:len(flacSignature)]) != flacSignature {
		return nil, format, fmt.Errorf("%w: a FLAC configuration is %q followed by the metadata blocks (RFC 9639 10.2)",
			ErrTrackConfig, flacSignature)
	}
	rest := config[len(flacSignature):]
	var blocks []mp4.FLACMetadataBlock
	last := false
	for len(rest) > 0 && !last {
		if len(rest) < flacBlockHeaderSize {
			return nil, format, fmt.Errorf("%w: FLAC metadata block %d stops inside its header",
				ErrTrackConfig, len(blocks))
		}
		last = rest[0]&0x80 != 0
		kind := rest[0] & 0x7F
		length := uint32(rest[1])<<16 | uint32(rest[2])<<8 | uint32(rest[3])
		switch {
		case kind == flacForbiddenBlock:
			return nil, format, fmt.Errorf("%w: FLAC metadata block %d has the forbidden type %d",
				ErrTrackConfig, len(blocks), kind)
		case uint64(length) > uint64(len(rest)-flacBlockHeaderSize):
			return nil, format, fmt.Errorf("%w: FLAC metadata block %d states %d bytes and %d follow",
				ErrTrackConfig, len(blocks), length, len(rest)-flacBlockHeaderSize)
		case kind == flacStreamInfo && len(blocks) > 0:
			return nil, format, fmt.Errorf("%w: FLAC metadata block %d is a second STREAMINFO",
				ErrTrackConfig, len(blocks))
		case kind != flacStreamInfo && len(blocks) == 0:
			return nil, format, fmt.Errorf("%w: the first FLAC metadata block is of type %d, not STREAMINFO (RFC 9639 8.2)",
				ErrTrackConfig, kind)
		}
		end := flacBlockHeaderSize + int(length)
		blocks = append(blocks, mp4.FLACMetadataBlock{
			LastMetadataBlockFlag: last,
			BlockType:             kind,
			Length:                length,
			BlockData:             rest[flacBlockHeaderSize:end],
		})
		rest = rest[end:]
	}
	switch {
	case !last:
		return nil, format, fmt.Errorf("%w: the FLAC configuration stops before the metadata block marked last",
			ErrTrackConfig)
	case len(rest) > 0:
		return nil, format, fmt.Errorf("%w: %d bytes follow the last FLAC metadata block", ErrTrackConfig, len(rest))
	case blocks[0].Length != flacStreamInfoSize:
		return nil, format, fmt.Errorf("%w: STREAMINFO is %d bytes, not %d", ErrTrackConfig,
			blocks[0].Length, flacStreamInfoSize)
	}
	// u(16) and u(16) block sizes, u(24) and u(24) frame sizes, then u(20)
	// sample rate, u(3) channels less one and u(5) bits per sample less one.
	info := blocks[0].BlockData
	format.SampleRate = int(info[10])<<12 | int(info[11])<<4 | int(info[12])>>4
	format.Channels = int(info[12]>>1&7) + 1
	format.BitDepth = (int(info[12]&1)<<4 | int(info[13])>>4) + 1
	switch {
	case format.SampleRate == 0:
		// RFC 9639 8.2: zero is for a stream that is not audio.
		return nil, format, fmt.Errorf("%w: STREAMINFO states a sample rate of 0, which is not audio", ErrTrackConfig)
	case format.BitDepth < flacMinBitDepth:
		return nil, format, fmt.Errorf("%w: STREAMINFO states %d bits per sample; FLAC starts at %d",
			ErrTrackConfig, format.BitDepth, flacMinBitDepth)
	}
	return blocks, format, nil
}

// flacEntryRate is the sample rate a FLACSampleEntry states. Its field holds
// sixteen bits of integer, so the Xiph mapping (3.3.1) has a higher rate stated
// as its greatest regular division that fits — 48000 for 96 and 192 kHz — and
// 65535 when halving leaves a fraction. The rate itself is in STREAMINFO.
func flacEntryRate(rate int) int {
	for rate > math.MaxUint16 {
		if rate%2 != 0 {
			return math.MaxUint16
		}
		rate /= 2
	}
	return rate
}

// describeFLAC writes the fLaC sample entry and its dfLa box, every metadata
// block the configuration holds preserved as the mapping asks (3.3.2).
func describeFLAC(trak *mp4.TrakBox, cfg TrackConfig) error {
	if len(cfg.CodecConfig) == 0 {
		return fmt.Errorf("%w: fLaC needs its metadata blocks as CodecConfig, %q first", ErrTrackConfig, flacSignature)
	}
	blocks, format, err := parseFLACConfig(cfg.CodecConfig)
	if err != nil {
		return err
	}
	entry := mp4.CreateAudioSampleEntryBox("fLaC", uint16(format.Channels), uint16(format.BitDepth),
		uint16(flacEntryRate(format.SampleRate)), &mp4.DfLaBox{MetadataBlocks: blocks})
	trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	return nil
}

// flacSampleEntry is a track's FLAC sample entry, or nil. mp4ff keeps no field
// for one, so it is found among the sample descriptions.
func flacSampleEntry(stsd *mp4.StsdBox) *mp4.AudioSampleEntryBox {
	sd, err := stsd.GetSampleDescription(0)
	if err != nil {
		return nil
	}
	entry, ok := sd.(*mp4.AudioSampleEntryBox)
	if !ok || entry.Type() != "fLaC" || entry.DfLa == nil {
		return nil
	}
	return entry
}

// flacTrackConfig turns a dfLa back into the configuration Matroska carries,
// and takes the format from STREAMINFO, which the mapping says is the one to
// read: the sample entry's rate is a division of it above 65535 Hz.
func flacTrackConfig(cfg *TrackConfig, dfla *mp4.DfLaBox) {
	config := []byte(flacSignature)
	for i, block := range dfla.MetadataBlocks {
		head := block.BlockType & 0x7F
		if i == len(dfla.MetadataBlocks)-1 {
			head |= 0x80
		}
		config = append(config, head, byte(len(block.BlockData)>>16), byte(len(block.BlockData)>>8),
			byte(len(block.BlockData)))
		config = append(config, block.BlockData...)
	}
	cfg.CodecConfig = config
	if _, format, err := parseFLACConfig(config); err == nil {
		cfg.SampleRate, cfg.Channels = format.SampleRate, format.Channels
	}
}

// webmFLACPrivate states the FLAC configuration as Matroska's CodecPrivate,
// which RFC 9639 10.2 defines as everything before the first frame: the form
// TrackConfig already holds it in. The Audio element repeats STREAMINFO.
func webmFLACPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	_, format, err := parseFLACConfig(cfg.CodecConfig)
	if err != nil {
		return webmCodecPrivate{}, err
	}
	return webmCodecPrivate{
		private: cfg.CodecConfig,
		audio:   &webmAudio{SamplingFrequency: float64(format.SampleRate), Channels: uint64(format.Channels)},
	}, nil
}

// webmFramesOnly describes a codec whose Matroska mapping has no
// initialisation at all: MPEG audio and AC-3 state their format in every frame,
// and the Matroska codec specifications say "Initialization: none" for
// A_MPEG/L1, /L2, /L3, A_AC3 and A_EAC3. The Audio element is all there is, and
// for AC-3 the specification says the channel count is read from it.
func webmFramesOnly(id string) func(TrackConfig) (webmCodecPrivate, error) {
	return func(cfg TrackConfig) (webmCodecPrivate, error) {
		audio, err := webmAudioFields(cfg, id)
		if err != nil {
			return webmCodecPrivate{}, err
		}
		return webmCodecPrivate{audio: audio}, nil
	}
}

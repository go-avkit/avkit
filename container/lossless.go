// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Apple Lossless (ALAC) and Dolby TrueHD (MLP) in an MP4 and in Matroska. Each
// is written the way its own mapping says, nothing here decodes a sample, and
// what a container needs is the configuration, which both codecs state in a
// record this file reads.
//
// ALAC. Apple's ALACMagicCookieDescription.txt (macosforge/alac) defines the
// "magic cookie": a 24-byte ALACSpecificConfig, optionally followed by a 24-byte
// ALACChannelLayoutInfo. An MP4 carries it in an "alac" box (a full box, so
// preceded by version and flags) inside an "alac" audio sample entry; the MP4RA
// registry lists the entry. TrackConfig.CodecConfig holds the cookie alone, the
// form both containers reduce to. Matroska's A_ALAC keeps it in CodecPrivate,
// which FFmpeg's matroskaenc writes as the bare cookie (measured: 24 bytes for a
// stereo track, where FFmpeg's decoder holds the 36-byte atom of the cookie
// description in its extradata); the reader takes the bare cookie or that atom.
//
// TrueHD. Dolby's "Dolby TrueHD (MLP) bitstreams within the ISO base media file
// format" gives the track an "mlpa" audio sample entry holding one "dmlp" box:
// the 32-bit format_info of the stream's major sync, the 15-bit peak_data_rate
// and a reserved bit, and 32 reserved bits. TrackConfig.CodecConfig holds that
// ten-byte payload. The entry states the sample rate as a plain 32-bit integer
// where every other audio entry has 16.16 fixed point, as FFmpeg's movenc writes
// it. Matroska's A_TRUEHD has no CodecPrivate; the format is in the major sync
// of the access units, which is where a track read from Matroska takes its dmlp
// from. A sample is one access unit, 40 << (ratebits & 7) samples long.
//
// ⛔ The text of the Dolby specification could not be opened while this was
// written (developer.dolby.com did not resolve), so the layout above is the one
// FFmpeg's movenc.c (mov_write_dmlp_tag) writes and mov.c reads, checked against
// a file it wrote.
//
// ⛔ Neither codec is written into an MPEG-TS. ATSC A/52 defines AC-3 and
// Enhanced AC-3 and no TrueHD; the stream type 0x83 of a Blu-ray TrueHD stream
// belongs to a specification that is not published; and no text maps ALAC onto a
// transport stream at all.

const (
	alacCookieSize = 24 // ALACSpecificConfig
	alacLayoutSize = 24 // ALACChannelLayoutInfo
	alacMaxEntry   = 65535
	alacMaxChannel = 8
	// alacAtomHeader is the size, "alac" and version/flags a Matroska CodecPrivate
	// puts before the cookie.
	alacAtomHeader = 12
)

// alacFormat is what the ALACSpecificConfig states that a container repeats.
type alacFormat struct {
	BitDepth, Channels int
	SampleRate         int
}

// parseALACCookie checks a magic cookie and reads its format.
func parseALACCookie(cookie []byte) (alacFormat, error) {
	var f alacFormat
	if n := len(cookie); n != alacCookieSize && n != alacCookieSize+alacLayoutSize {
		return f, fmt.Errorf("%w: an ALAC cookie is %d bytes, or %d with its channel layout; this one is %d",
			ErrTrackConfig, alacCookieSize, alacCookieSize+alacLayoutSize, n)
	}
	f.BitDepth = int(cookie[5])
	f.Channels = int(cookie[9])
	rate := binary.BigEndian.Uint32(cookie[20:24])
	switch {
	case binary.BigEndian.Uint32(cookie[0:4]) == 0:
		return f, fmt.Errorf("%w: the ALAC frame length is 0", ErrTrackConfig)
	case cookie[4] != 0:
		// ALACMagicCookieDescription: the compatible version "must be 0".
		return f, fmt.Errorf("%w: the ALAC compatible version is %d, and the description says it must be 0",
			ErrTrackConfig, cookie[4])
	case f.BitDepth != 16 && f.BitDepth != 20 && f.BitDepth != 24 && f.BitDepth != 32:
		return f, fmt.Errorf("%w: ALAC codes 16, 20, 24 or 32 bits, not %d", ErrTrackConfig, f.BitDepth)
	case f.Channels < 1 || f.Channels > alacMaxChannel:
		return f, fmt.Errorf("%w: ALAC codes 1 to %d channels, not %d", ErrTrackConfig, alacMaxChannel, f.Channels)
	case rate == 0 || rate > 1<<31-1:
		return f, fmt.Errorf("%w: the ALAC sample rate is %d", ErrTrackConfig, rate)
	}
	f.SampleRate = int(rate)
	return f, nil
}

// alacBox builds the alac box that holds a cookie.
func alacBox(cookie []byte) *mp4.AlacBox {
	b := &mp4.AlacBox{
		FrameLength:       binary.BigEndian.Uint32(cookie[0:4]),
		CompatibleVersion: cookie[4],
		BitDepth:          cookie[5],
		Pb:                cookie[6],
		Mb:                cookie[7],
		Kb:                cookie[8],
		NumChannels:       cookie[9],
		MaxRun:            binary.BigEndian.Uint16(cookie[10:12]),
		MaxFrameBytes:     binary.BigEndian.Uint32(cookie[12:16]),
		AvgBitRate:        binary.BigEndian.Uint32(cookie[16:20]),
		SampleRate:        binary.BigEndian.Uint32(cookie[20:24]),
	}
	if len(cookie) > alacCookieSize {
		b.ChannelLayoutInfo = cookie[alacCookieSize:]
	}
	return b
}

// alacCookie is the magic cookie an alac box holds.
func alacCookie(b *mp4.AlacBox) []byte {
	c := make([]byte, alacCookieSize, alacCookieSize+len(b.ChannelLayoutInfo))
	binary.BigEndian.PutUint32(c[0:4], b.FrameLength)
	c[4], c[5], c[6], c[7], c[8], c[9] = b.CompatibleVersion, b.BitDepth, b.Pb, b.Mb, b.Kb, b.NumChannels
	binary.BigEndian.PutUint16(c[10:12], b.MaxRun)
	binary.BigEndian.PutUint32(c[12:16], b.MaxFrameBytes)
	binary.BigEndian.PutUint32(c[16:20], b.AvgBitRate)
	binary.BigEndian.PutUint32(c[20:24], b.SampleRate)
	return append(c, b.ChannelLayoutInfo...)
}

// describeALAC writes the alac sample entry and the alac box inside it.
//
// The entry's rate field holds sixteen bits of integer. A higher rate is stated
// as 0, which is what FFmpeg's movenc writes for a track outside QuickTime: the
// cookie states the rate, and is the one a reader takes.
func describeALAC(trak *mp4.TrakBox, cfg TrackConfig) error {
	if len(cfg.CodecConfig) == 0 {
		return fmt.Errorf("%w: alac needs its magic cookie as CodecConfig", ErrTrackConfig)
	}
	format, err := parseALACCookie(cfg.CodecConfig)
	if err != nil {
		return err
	}
	rate := format.SampleRate
	if rate > alacMaxEntry {
		rate = 0
	}
	entry := mp4.CreateAudioSampleEntryBox("alac", uint16(format.Channels), uint16(format.BitDepth),
		uint16(rate), alacBox(cfg.CodecConfig))
	trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	return nil
}

// alacSampleEntry is the cookie of a track's ALAC sample entry, or nil. The box
// is a direct child of the entry in an MP4 and sits in a wave box in QuickTime.
func alacSampleEntry(stsd *mp4.StsdBox) []byte {
	sd, err := stsd.GetSampleDescription(0)
	if err != nil {
		return nil
	}
	entry, ok := sd.(*mp4.AudioSampleEntryBox)
	if !ok || entry.Type() != "alac" {
		return nil
	}
	box := entry.Alac
	if box == nil && entry.Wave != nil {
		box = entry.Wave.Alac
	}
	if box == nil {
		return nil
	}
	return alacCookie(box)
}

// alacTrackConfig takes a cookie into the configuration: the cookie is
// authoritative, the sample entry's rate field being sixteen bits wide.
func alacTrackConfig(cfg *TrackConfig, cookie []byte) {
	cfg.CodecConfig = cookie
	if f, err := parseALACCookie(cookie); err == nil {
		cfg.Channels, cfg.SampleRate = f.Channels, f.SampleRate
	}
}

// alacMatroskaCookie reads the cookie out of an A_ALAC CodecPrivate, in the atom
// form or bare.
func alacMatroskaCookie(private []byte) []byte {
	if len(private) > alacAtomHeader && string(private[4:8]) == "alac" &&
		uint64(binary.BigEndian.Uint32(private[0:4])) == uint64(len(private)) {
		return private[alacAtomHeader:]
	}
	return private
}

// webmALACPrivate states an ALAC track in Matroska.
func webmALACPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	format, err := parseALACCookie(cfg.CodecConfig)
	if err != nil {
		return webmCodecPrivate{}, err
	}
	return webmCodecPrivate{
		private: cfg.CodecConfig,
		audio:   &webmAudio{SamplingFrequency: float64(format.SampleRate), Channels: uint64(format.Channels)},
	}, nil
}

// Dolby TrueHD.
const (
	mlpaEntry       = "mlpa"
	dmlpPayloadSize = 10 // format_info, peak_data_rate and a reserved bit, 32 reserved bits
	dmlpBoxSize     = 8 + dmlpPayloadSize
	mlpaFixedSize   = 28 // the audio sample entry fields, ahead of its boxes
	mlpMajorSync    = 0xF8726FBA
	// The major sync of an access unit starts after the two bytes of length and
	// parity and the two of the first substream's header, and is followed by
	// format_info, the signature and flags, two reserved bytes, and the 16 bits
	// that hold the variable rate bit and the peak data rate.
	mlpSyncOffset    = 4
	mlpFormatOffset  = mlpSyncOffset + 4
	mlpPeakOffset    = mlpFormatOffset + 10
	mlpMajorSyncSize = mlpPeakOffset + 2
	mlpMaxChannels   = 16
)

// mlpSampleRate is the rate a format_info states: ratebits of 0 to 2 are 48, 96
// and 192 kHz, 8 to 10 are 44.1, 88.2 and 176.4 kHz, and the rest are reserved.
func mlpSampleRate(formatInfo uint32) (int, bool) {
	bits := formatInfo >> 28
	switch {
	case bits <= 2:
		return 48000 << bits, true
	case bits >= 8 && bits <= 10:
		return 44100 << (bits & 7), true
	}
	return 0, false
}

// mlpAccessUnitSamples is how many samples one TrueHD access unit lasts.
func mlpAccessUnitSamples(formatInfo uint32) int { return 40 << (formatInfo >> 28 & 7) }

// parseDMLP checks a dmlp payload and returns its format_info.
func parseDMLP(payload []byte) (uint32, error) {
	if len(payload) != dmlpPayloadSize {
		return 0, fmt.Errorf("%w: a dmlp payload is %d bytes, not %d", ErrTrackConfig, dmlpPayloadSize, len(payload))
	}
	info := binary.BigEndian.Uint32(payload)
	if _, ok := mlpSampleRate(info); !ok {
		return 0, fmt.Errorf("%w: the TrueHD format_info %#08x states a reserved sampling frequency",
			ErrTrackConfig, info)
	}
	return info, nil
}

// describeTrueHD writes the mlpa sample entry and its dmlp box.
func describeTrueHD(trak *mp4.TrakBox, cfg TrackConfig) error {
	if len(cfg.CodecConfig) == 0 {
		return fmt.Errorf("%w: mlpa needs its dmlp payload as CodecConfig", ErrTrackConfig)
	}
	info, err := parseDMLP(cfg.CodecConfig)
	if err != nil {
		return err
	}
	rate, _ := mlpSampleRate(info)
	switch {
	case cfg.Channels < 1 || cfg.Channels > mlpMaxChannels:
		return fmt.Errorf("%w: mlpa needs a channel count of 1 to %d, not %d", ErrTrackConfig, mlpMaxChannels, cfg.Channels)
	case cfg.SampleRate != 0 && cfg.SampleRate != rate:
		return fmt.Errorf("%w: mlpa states %d Hz and its format_info %d Hz", ErrTrackConfig, cfg.SampleRate, rate)
	}
	payload := make([]byte, mlpaFixedSize, mlpaFixedSize+dmlpBoxSize)
	payload[7] = 1                                                 // data_reference_index
	binary.BigEndian.PutUint16(payload[16:], uint16(cfg.Channels)) // channelcount
	binary.BigEndian.PutUint16(payload[18:], 16)                   // samplesize
	binary.BigEndian.PutUint32(payload[24:], uint32(rate))         // samplerate, an integer
	payload = binary.BigEndian.AppendUint32(payload, dmlpBoxSize)
	payload = append(payload, "dmlp"...)
	payload = append(payload, cfg.CodecConfig...)
	trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateUnknownBox(mlpaEntry, uint64(8+len(payload)), payload))
	return nil
}

// mlpaFormat is what a track's mlpa sample entry states.
type mlpaFormat struct {
	dmlp           []byte
	channels, rate int
	formatInfo     uint32
}

// mlpaSampleEntry reads a track's mlpa entry, or returns nil when it has none or
// states no usable dmlp. mp4ff keeps no type for the entry, so it arrives as an
// unknown box and the fixed fields and the boxes after them are read here, each
// size checked against what is left.
func mlpaSampleEntry(stsd *mp4.StsdBox) *mlpaFormat {
	sd, err := stsd.GetSampleDescription(0)
	if err != nil {
		return nil
	}
	unknown, ok := sd.(*mp4.UnknownBox)
	if !ok || unknown.Type() != mlpaEntry {
		return nil
	}
	p := unknown.Payload()
	if len(p) < mlpaFixedSize {
		return nil
	}
	f := &mlpaFormat{channels: int(binary.BigEndian.Uint16(p[16:]))}
	for rest := p[mlpaFixedSize:]; len(rest) >= 8; {
		size := uint64(binary.BigEndian.Uint32(rest))
		if size < 8 || size > uint64(len(rest)) {
			return nil
		}
		if string(rest[4:8]) == "dmlp" {
			info, err := parseDMLP(rest[8:size])
			if err != nil {
				return nil
			}
			f.dmlp, f.formatInfo = rest[8:size], info
			f.rate, _ = mlpSampleRate(info)
			return f
		}
		rest = rest[size:]
	}
	return nil
}

// webmTrueHDPrivate states a TrueHD track in Matroska, which gives A_TRUEHD no
// CodecPrivate: the Audio element is all there is. A dmlp the caller holds is
// still checked, since a track whose format_info is not TrueHD's is not one.
func webmTrueHDPrivate(cfg TrackConfig) (webmCodecPrivate, error) {
	if len(cfg.CodecConfig) > 0 {
		info, err := parseDMLP(cfg.CodecConfig)
		if err != nil {
			return webmCodecPrivate{}, err
		}
		if rate, _ := mlpSampleRate(info); cfg.SampleRate != 0 && cfg.SampleRate != rate {
			return webmCodecPrivate{}, fmt.Errorf("%w: A_TRUEHD states %d Hz and its format_info %d Hz",
				ErrTrackConfig, cfg.SampleRate, rate)
		}
	}
	audio, err := webmAudioFields(cfg, "A_TRUEHD")
	if err != nil {
		return webmCodecPrivate{}, err
	}
	return webmCodecPrivate{audio: audio}, nil
}

// trueHDTrackConfig takes an entry's dmlp into the configuration.
func trueHDTrackConfig(cfg *TrackConfig, f *mlpaFormat) {
	cfg.CodecConfig = f.dmlp
	cfg.SampleRate = f.rate
	cfg.Channels = f.channels
}

// mlpMajorSyncOf reads the dmlp payload a TrueHD access unit's major sync
// states, or false when the access unit has none: only some carry one.
func mlpMajorSyncOf(au []byte) ([]byte, bool) {
	if len(au) < mlpMajorSyncSize || binary.BigEndian.Uint32(au[mlpSyncOffset:]) != mlpMajorSync {
		return nil, false
	}
	dmlp := make([]byte, dmlpPayloadSize)
	copy(dmlp, au[mlpFormatOffset:mlpFormatOffset+4])
	// The peak data rate is the low 15 bits of the last two bytes, and the dmlp
	// states it in the high 15 bits of its own two, a reserved bit below.
	binary.BigEndian.PutUint16(dmlp[4:], binary.BigEndian.Uint16(au[mlpPeakOffset:])<<1)
	return dmlp, true
}

// trueHDTiming counts a TrueHD track read from Matroska in samples, each access
// unit lasting what its format says. Matroska counts in milliseconds, an access
// unit lasts 40 samples at 48 kHz -- 0.83 ms -- and the block timestamps round
// to the tick, so the durations the file's times give are 1 and 0 alternately;
// written into an MP4, a sample of duration 0 is a sample with no time of its own.
// The format states the true one, and the track keeps its length by it.
func trueHDTiming(cfg *TrackConfig, samples []Sample) {
	info, err := parseDMLP(cfg.CodecConfig)
	if err != nil {
		return
	}
	rate, _ := mlpSampleRate(info)
	cfg.Timescale = uint32(rate)
	for i := range samples {
		samples[i].Duration = uint32(mlpAccessUnitSamples(info))
	}
}

// trueHDFromSamples fills in what Matroska leaves out: the dmlp, taken from the
// first access unit that carries a major sync.
func trueHDFromSamples(cfg TrackConfig, samples []Sample) (TrackConfig, error) {
	for _, s := range samples {
		dmlp, ok := mlpMajorSyncOf(s.Data)
		if !ok {
			continue
		}
		info, err := parseDMLP(dmlp)
		if err != nil {
			return cfg, err
		}
		cfg.CodecConfig = dmlp
		cfg.SampleRate, _ = mlpSampleRate(info)
		return cfg, nil
	}
	return cfg, fmt.Errorf("%w: mlpa track states no configuration and no access unit holds a major sync", ErrTrackConfig)
}

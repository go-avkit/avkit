// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Eyevinn/mp4ff/aac"
)

// Audio-only output: the writers that take a single audio track out of any
// container without re-encoding it, the counterpart of the elementary-stream
// readers (adts.go, mpegaudio.go, flac.go, ogg.go, wav.go).
//
// The formats are named by the Format value their reader already has --
// FormatADTS, FormatMPEGAudio, FormatFLAC, FormatOgg, FormatWAV -- and each
// takes the codecs its specification defines and refuses the rest with
// ErrUnsupportedCodec, naming what it does carry: an Opus track is not an ADTS
// stream, and a header that said it was would be a file no player opens.
//
//   - ADTS (ISO/IEC 13818-7 §6.2, ISO/IEC 14496-3 §1.A.2): AAC from "mp4a".
//   - Raw MPEG audio: Layers I to III ("mp1", "mp2", "mp3") as their frames are.
//   - Native FLAC (RFC 9639 §6): "fLaC".
//   - Ogg (RFC 3533): Opus (RFC 7845), Vorbis (Vorbis I §A) and FLAC (RFC 9639
//     §10.1).
//   - WAVE (RIFF; RF64, EBU Tech 3306): "ipcm", "fpcm" and "raw ".
//
// What is NOT written, in every format: the tags a container carries beside the
// audio. The writers have the track and nothing else -- no ID3v2 or Xing/Info
// header for MPEG audio, no VORBIS_COMMENT of the source for FLAC and Ogg (an
// empty one is written where the format requires one), no LIST INFO for WAVE --
// and so the encoder priming and padding that a container states outside the
// frames (TrackConfig.StartDelay and EditList) is not stated either. Opus is the
// exception: its pre-skip is part of the codec configuration and is written.

// ErrAudioFormat means the format asked for is not one of the audio-only output
// formats.
var ErrAudioFormat = errors.New("container: not an audio-only output format")

// AudioOption configures an AudioWriter.
type AudioOption func(*audioSettings)

type audioSettings struct {
	serial    uint32
	serialSet bool
	rf64      bool
}

// OggSerial fixes the serial number of the logical stream an Ogg output holds.
// Without it the number is derived from the stream's identification header, so
// the same track always produces the same file.
func OggSerial(serial uint32) AudioOption {
	return func(s *audioSettings) { s.serial, s.serialSet = serial, true }
}

// WAVRF64 makes a WAVE output an RF64 file (EBU Tech 3306) whatever its size.
// Without it the output is RIFF, and becomes RF64 by itself when the writer can
// seek back and the data turns out not to fit RIFF's 32-bit sizes; an output
// that cannot seek can only state its size by being RF64 from the start, and
// leaves it unstated.
func WAVRF64() AudioOption {
	return func(s *audioSettings) { s.rf64 = true }
}

// audioSink is one format's writing state, once its track is declared.
type audioSink interface {
	sample(s Sample) error
	// finish writes what follows the audio. A failure of the output is the
	// stickyWriter's to report, so there is no error to return.
	finish()
}

// audioSinks maps each output format to the function that checks a track and
// writes the start of the file.
var audioSinks = map[Format]func(*AudioWriter, TrackConfig) (audioSink, error){
	FormatADTS:      newADTSSink,
	FormatMPEGAudio: newMPEGSink,
	FormatFLAC:      newFLACSink,
	FormatOgg:       newOggSink,
	FormatWAV:       newWAVSink,
}

// audioTrackID is the identifier AddTrack gives the one track.
const audioTrackID = 1

// AudioWriter writes one audio track as an audio-only file. It has the shape of
// the muxers: AddTrack, then WriteSample for every sample, then Close.
type AudioWriter struct {
	format Format
	w      io.Writer
	out    *stickyWriter
	set    audioSettings
	sink   audioSink
	closed bool
}

// NewAudioWriter returns an AudioWriter producing format on w. The format is
// checked by AddTrack, which is where the track it must carry is known too.
func NewAudioWriter(format Format, w io.Writer, opts ...AudioOption) *AudioWriter {
	a := &AudioWriter{format: format, w: w, out: &stickyWriter{w: w}}
	for _, opt := range opts {
		opt(&a.set)
	}
	return a
}

// AddTrack declares the track and writes whatever the format puts in front of
// the audio. An audio-only file holds one track: a second is refused, as is any
// track whose codec the format does not carry.
func (a *AudioWriter) AddTrack(cfg TrackConfig) (uint32, error) {
	build, ok := audioSinks[a.format]
	name := describeFormat(a.format)
	switch {
	case a.closed:
		return 0, ErrClosed
	case !ok:
		return 0, fmt.Errorf("%w: %s", ErrAudioFormat, name)
	case a.sink != nil:
		return 0, fmt.Errorf("%w: %s output holds a single track", ErrTrackConfig, name)
	case cfg.Kind != Audio:
		return 0, fmt.Errorf("%w: %s output holds audio, not a %s track", ErrTrackConfig, name, cfg.Kind)
	}
	sink, err := build(a, cfg)
	if err == nil {
		err = a.writeError()
	}
	if err != nil {
		return 0, err
	}
	a.sink = sink
	return audioTrackID, nil
}

// WriteSample writes one sample of the track. Its Duration is only read where a
// format states an end that the frames alone do not (the last page of an Ogg
// stream, see oggSink); its other fields mean nothing to an audio-only file.
func (a *AudioWriter) WriteSample(trackID uint32, s Sample) error {
	switch {
	case a.closed:
		return ErrClosed
	case a.sink == nil:
		return ErrNoTracks
	case trackID != audioTrackID:
		return fmt.Errorf("%w: %d", ErrUnknownTrack, trackID)
	}
	if err := a.sink.sample(s); err != nil {
		return err
	}
	return a.writeError()
}

// Close writes what the format puts after the audio and refuses any further
// use. It reports ErrNoTracks when no track was ever declared.
func (a *AudioWriter) Close() error {
	if a.closed {
		return ErrClosed
	}
	a.closed = true
	if a.sink == nil {
		return ErrNoTracks
	}
	a.sink.finish()
	return a.writeError()
}

// writeError is the first error w returned, if any.
func (a *AudioWriter) writeError() error {
	if a.out.err != nil {
		return fmt.Errorf("container: write %s: %w", describeFormat(a.format), a.out.err)
	}
	return nil
}

// stickyWriter remembers the first error its writer returns and writes nothing
// after it, so a sink can write a header in pieces and look once.
type stickyWriter struct {
	w   io.Writer
	err error
	n   int64 // bytes written
}

// end is what a format with nothing after its audio writes: nothing.
func (s *stickyWriter) end() { s.n += 0 }

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	s.n += int64(n)
	s.err = err
	return n, err
}

// refuseAudioCodec is the refusal of a track whose codec the format does not
// carry, naming what it does.
func refuseAudioCodec(format Format, cfg TrackConfig, carries string) error {
	return fmt.Errorf("%w: %q cannot be written as %s, which carries %s",
		ErrUnsupportedCodec, cfg.Codec, describeFormat(format), carries)
}

// audioCodec is a track's codec name as the writers compare it.
func audioCodec(cfg TrackConfig) string {
	return strings.ToLower(strings.TrimSpace(cfg.Codec))
}

// ---- ADTS ----------------------------------------------------------------

type adtsSink struct {
	out    *stickyWriter
	header aac.ADTSHeader
}

// newADTSSink checks an AAC track and states its configuration the way
// FFmpeg's adtsenc.c does (adts_decode_extradata): from the
// AudioSpecificConfig, as the CORE's object type, sampling frequency index and
// channel configuration. An ADTS header has no place for SBR or PS, so an
// HE-AAC stream is announced at its core rate, half of what is heard, and the
// decoder finds the extension in the frames; adtsParameters is the rule the
// transport stream muxer applies, and the one used here.
//
// Everything ADTS cannot state is refused rather than written wrong: a core
// that is not Main, LC, SSR or LTP, a sampling frequency written in full, a
// channel configuration of 0 (the layout is in a program config element that
// ADTS would have to carry in the first frame), and a 960/120 window, a
// scalable or an extension flag in the GASpecificConfig (adtsenc.c refuses the
// same three). The header is the one adtsenc.c writes: MPEG-4 identifier,
// protection absent, a buffer fullness of 0x7FF and one raw data block.
func newADTSSink(a *AudioWriter, cfg TrackConfig) (audioSink, error) {
	if audioCodec(cfg) != "mp4a" {
		return nil, refuseAudioCodec(FormatADTS, cfg, "AAC (mp4a)")
	}
	objectType, rate, configuration := cfg.AudioObjectType, cfg.SampleRate, byte(0)
	if objectType == 0 {
		objectType = aac.AAClc
	}
	if len(cfg.CodecConfig) > 0 {
		var ok bool
		objectType, rate, configuration, ok = adtsParameters(cfg.CodecConfig)
		if !ok {
			return nil, fmt.Errorf("%w: mp4a: the AudioSpecificConfig % x states a core no ADTS header can state",
				ErrTrackConfig, cfg.CodecConfig)
		}
		if flags := ascGAFlags(cfg.CodecConfig); flags != 0 {
			return nil, fmt.Errorf("%w: mp4a: the AudioSpecificConfig % x sets the GASpecificConfig flags %03b "+
				"(960/120 window, scalable, extension), which ADTS does not allow", ErrTrackConfig, cfg.CodecConfig, flags)
		}
	} else if c, ok := aacConfiguration(cfg.Channels); ok {
		configuration = c
	}
	if configuration == 0 {
		return nil, fmt.Errorf("%w: mp4a: ADTS needs a channel configuration of 1 to 7 (%d channels is not one, "+
			"and a program config element is not carried)", ErrTrackConfig, cfg.Channels)
	}
	header, err := aac.NewADTSHeader(rate, configuration, objectType, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: mp4a: %v", ErrTrackConfig, err)
	}
	return &adtsSink{out: a.out, header: *header}, nil
}

// ascGAFlags are the three flags that open the GASpecificConfig of an
// AudioSpecificConfig whose core is a plain AAC object: the 960/120 frame
// length, depends-on-core-coder and the extension flag.
func ascGAFlags(asc []byte) uint32 {
	b := &ascBits{data: asc}
	object := b.objectType()
	b.frequency()
	b.read(4) // channelConfiguration
	if object == aacObjectSBR || object == aacObjectPS {
		b.frequency()
		b.objectType()
	}
	return b.read(3)
}

func (s *adtsSink) sample(smp Sample) error {
	if len(smp.Data) == 0 {
		return nil // adtsenc.c writes nothing for an empty packet
	}
	frame, err := toADTS(smp.Data, s.header)
	if err != nil {
		return err
	}
	s.out.Write(frame)
	return nil
}

func (s *adtsSink) finish() { s.out.end() }

// ---- MPEG audio ----------------------------------------------------------

type mpegSink struct {
	out   *stickyWriter
	codec string
}

// newMPEGSink takes Layers I to III. FFmpeg's mp3enc.c writes, by default, an
// ID3v2.4 tag in front and a Xing/Info frame (with the LAME fields) behind it;
// neither is written here. The tag holds metadata this writer does not have, and
// the Xing frame holds a frame count and a byte count that are known only at the
// end, written by seeking back -- which an io.Writer cannot do, and a stream
// without one plays identically (a decoder reads the frames; only a player's
// estimate of the duration of a variable-rate file improves with it). FFmpeg's
// own demuxer drops that frame, so the packets it reads back are the same.
// mp3enc.c accepts Layer III only (mp2 has its own raw muxer, mp1 none); the
// frames of all three are the same bytes in a file, so all three are written.
func newMPEGSink(a *AudioWriter, cfg TrackConfig) (audioSink, error) {
	switch codec := audioCodec(cfg); codec {
	case "mp1", "mp2", "mp3":
		return &mpegSink{out: a.out, codec: codec}, nil
	}
	return nil, refuseAudioCodec(FormatMPEGAudio, cfg, "MPEG audio Layers I to III (mp1, mp2, mp3)")
}

func (s *mpegSink) sample(smp Sample) error {
	if err := checkMPASample(smp.Data, s.codec); err != nil {
		return err
	}
	s.out.Write(smp.Data)
	return nil
}

func (s *mpegSink) finish() { s.out.end() }

// ---- native FLAC ---------------------------------------------------------

type flacSink struct{ out *stickyWriter }

// newFLACSink writes "fLaC" and the metadata blocks of the track's
// configuration, STREAMINFO first, exactly as the source states them: the total
// number of samples and the MD5 signature in STREAMINFO are the source's, a zero
// meaning "unknown" (RFC 9639 §8.2) stays zero, and nothing is computed or
// invented. A SEEKTABLE is left out because it states byte offsets into a file
// that is not this one (RFC 9639 §8.5); the last-block flag is set on whichever
// block ends up last. FFmpeg's flacenc.c writes STREAMINFO, a VORBIS_COMMENT and
// 8192 bytes of padding, and patches STREAMINFO at the end when its encoder
// sent an update; none of that is the source's.
func newFLACSink(a *AudioWriter, cfg TrackConfig) (audioSink, error) {
	if audioCodec(cfg) != "flac" {
		return nil, refuseAudioCodec(FormatFLAC, cfg, "FLAC (fLaC)")
	}
	blocks, _, err := parseFLACConfig(cfg.CodecConfig)
	if err != nil {
		return nil, err
	}
	kept := blocks[:0]
	for _, b := range blocks {
		if b.BlockType != flacSeekTable {
			kept = append(kept, b)
		}
	}
	a.out.Write([]byte(flacSignature))
	for i, b := range kept {
		head := b.BlockType
		if i == len(kept)-1 {
			head |= 0x80
		}
		n := len(b.BlockData)
		a.out.Write([]byte{head, byte(n >> 16), byte(n >> 8), byte(n)})
		a.out.Write(b.BlockData)
	}
	return &flacSink{out: a.out}, nil
}

// flacSeekTable is the SEEKTABLE metadata block type (RFC 9639 §8.1).
const flacSeekTable = 3

func (s *flacSink) sample(smp Sample) error {
	if _, ok := parseFLACFrameHeader(smp.Data); !ok {
		return fmt.Errorf("%w: %d bytes that do not open with a FLAC frame header (RFC 9639 §9.1)",
			ErrSample, len(smp.Data))
	}
	s.out.Write(smp.Data)
	return nil
}

func (s *flacSink) finish() { s.out.end() }

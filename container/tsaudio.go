// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"fmt"

	"github.com/asticode/go-astits"
)

// AC-3, Enhanced AC-3 and MPEG audio travel in a transport stream as the sync
// frames their encoders produce, with no header of the container's own in
// front: unlike AAC, whose ADTS header ts.go strips, the frames are what an MP4
// sample holds too. Reading one is a matter of finding where each frame ends,
// which only the frame's own header says.
//
// ⛔ A packetised unit need not hold whole frames. ISO/IEC 13818-1 2.4.3.7 has
// a unit's PTS refer to the first access unit that begins in it, which only
// means something because a unit may also hold the end of the one before.
// FFmpeg aligns them, which is why a reader that assumed it would pass every
// test made from FFmpeg's output; a frame that straddles two units is carried
// over here instead, and timed from the unit it began in.

// The registration descriptor's format identifiers (ATSC A/52:2012 Annex A
// 3, Annex G 2).
const (
	ac3RegistrationID  = 0x41432D33 // "AC-3"
	eac3RegistrationID = 0x45414333 // "EAC3", for systems other than ATSC
)

// The stream types ISO/IEC 13818-1 Table 2-34 assigns MPEG audio: the first
// for ISO/IEC 11172-3, the second for ISO/IEC 13818-3, whose lower sampling
// frequencies the first cannot state.
const (
	tsMPEG1Audio = astits.StreamTypeMPEG1Audio // 0x03
	tsMPEG2Audio = astits.StreamTypeMPEG2Audio // 0x04
)

// PES stream identifiers, from ISO/IEC 13818-1 Table 2-22.
const (
	tsPrivateStream1 = 0xBD
	tsAudioStream0   = 0xC0
)

// ⛔ astits picks a stream identifier from the stream type when it is left at
// zero, and two of its picks are wrong for what this package writes: MPEG-1
// audio (0x03) falls through to private_stream_1, and AC-3 (0x81) is given
// 0xFD, the extended stream identifier, which needs a PES extension astits
// does not write. So the identifier is always stated.

// tsAudioForm says how the payload of an audio stream divides into frames.
type tsAudioForm uint8

const (
	tsADTS tsAudioForm = iota // AAC, each frame behind an ADTS header
	tsAC3                     // AC-3 or Enhanced AC-3 sync frames
	tsMPA                     // MPEG audio frames, Layers I to III
)

// tsFrameHeaderMax is more than either header needs to be read whole: fewer
// bytes than this that do not parse may be the start of a frame the next unit
// completes, and are carried over rather than skipped.
const tsFrameHeaderMax = 16

// tsFrame is what splitting needs to know about one frame.
type tsFrame struct {
	size     int
	duration int64 // in TSTimescale units
	// continues marks an Enhanced AC-3 frame that belongs to the access
	// unit before it: a dependent substream, or an independent one other
	// than the first. ATSC A/52 Annex G 3.3 puts every substream of one
	// period in one access unit, and an MP4 sample is that access unit.
	continues bool
}

// privateStreamCodec names what an elementary stream of private data carries,
// or nothing when no descriptor says. Private data is the stream type that
// says least: the descriptors are all there is.
func privateStreamCodec(es *astits.PMTElementaryStream) string {
	if isAV1Stream(es) {
		return "av01"
	}
	for _, d := range es.ElementaryStreamDescriptors {
		if d == nil {
			continue
		}
		switch {
		// DVB, ETSI EN 300 468 Annex D.2 and D.4: the descriptor's tag alone
		// identifies the stream.
		case d.Tag == astits.DescriptorTagAC3:
			return "ac-3"
		case d.Tag == astits.DescriptorTagEnhancedAC3:
			return "ec-3"
		// Any other system, ATSC A/52 Annex A 3 and Annex G 2.
		case d.Registration != nil && d.Registration.FormatIdentifier == ac3RegistrationID:
			return "ac-3"
		case d.Registration != nil && d.Registration.FormatIdentifier == eac3RegistrationID:
			return "ec-3"
		}
	}
	return ""
}

// appendFrames splits one packetised unit into the frames it holds, carrying
// a frame the unit does not complete over to the next.
func (t *tsTrack) appendFrames(pes *astits.PESData) {
	data := pes.Data
	carried := len(t.carry)
	if carried > 0 {
		data = append(t.carry, data...)
		t.carry = nil
	}
	timed := pesTimed(pes)
	for offset := 0; offset < len(data); {
		// The unit's timestamp belongs to the first frame that begins in it,
		// not to one carried over from the unit before.
		if timed && offset >= carried {
			t.nextAt, timed = pesTime(pes), false
		}
		f, err := t.frameAt(data[offset:])
		switch {
		case err != nil && len(data)-offset < tsFrameHeaderMax,
			err == nil && offset+f.size > len(data):
			t.carry = append([]byte(nil), data[offset:]...)
			return
		case err != nil:
			// Not a frame: look for the next sync word one byte on.
			offset++
			continue
		}
		frame := data[offset : offset+f.size]
		offset += f.size
		if last := len(t.pending) - 1; f.continues && last >= 0 {
			// Copied rather than appended in place: the unit before is a
			// slice of a buffer that holds more than it.
			unit := &t.pending[last]
			unit.data = append(append([]byte(nil), unit.data...), frame...)
			continue
		}
		t.pending = append(t.pending, tsUnit{data: frame, time: t.nextAt})
		t.nextAt += f.duration
		t.unitDuration = f.duration
	}
}

// pesTimed reports whether a unit states when it is presented.
func pesTimed(pes *astits.PESData) bool {
	return pes.Header != nil && pes.Header.OptionalHeader != nil &&
		pes.Header.OptionalHeader.PTS != nil
}

// frameAt reads the header of the frame data begins with. The first frame
// that reads is also what describes the track: the stream type says which
// family the frames belong to, and only the frames say which member.
func (t *tsTrack) frameAt(data []byte) (tsFrame, error) {
	if t.audio == tsMPA {
		f, err := parseMPAFrame(data)
		if err != nil {
			return tsFrame{}, err
		}
		if t.config.SampleRate == 0 {
			t.describeAudio(f.Codec(), f.SampleRate, f.Channels)
		}
		return tsFrame{size: f.FrameSize, duration: tsSamples(f.Samples, f.SampleRate)}, nil
	}
	f, err := parseAC3Frame(data)
	if err != nil {
		return tsFrame{}, err
	}
	continues := f.EAC3 && (f.StreamType == 1 || f.SubstreamID != 0)
	if t.config.SampleRate == 0 && !continues {
		codec := "ac-3"
		if f.EAC3 {
			codec = "ec-3"
		}
		t.describeAudio(codec, f.SampleRate, f.Channels)
	}
	return tsFrame{
		size:      f.FrameSize,
		duration:  tsSamples(f.NumBlocks*256, f.SampleRate),
		continues: continues,
	}, nil
}

// describeAudio names the track after what its frames say it is.
func (t *tsTrack) describeAudio(codec string, rate, channels int) {
	t.track.Codec, t.config.Codec = codec, codec
	t.track.SampleRate, t.config.SampleRate = rate, rate
	t.track.Channels, t.config.Channels = channels, channels
}

// tsSamples is how long a number of samples lasts in the transport clock.
func tsSamples(samples, rate int) int64 {
	return int64(samples) * TSTimescale / int64(rate)
}

// checkAC3Sample refuses a sample that does not begin with a sync frame of
// the codec the track was declared with: written anyway, it would be a stream
// whose program map says one thing and whose frames another.
func checkAC3Sample(data []byte, eac3 bool) error {
	f, err := parseAC3Frame(data)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSample, err)
	}
	if f.EAC3 != eac3 {
		return fmt.Errorf("%w: an %s sync frame in a track declared as %s",
			ErrSample, ac3Name(f.EAC3), ac3Name(eac3))
	}
	return nil
}

// ac3Name is how an error names one of the two codecs.
func ac3Name(eac3 bool) string {
	if eac3 {
		return "Enhanced AC-3"
	}
	return "AC-3"
}

// checkMPASample refuses a sample that does not begin with a frame of the
// layer the track was declared with, for the same reason.
func checkMPASample(data []byte, codec string) error {
	f, err := parseMPAFrame(data)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSample, err)
	}
	if f.Codec() != codec {
		return fmt.Errorf("%w: a Layer %d frame in a track declared as %s", ErrSample, f.Layer, codec)
	}
	return nil
}

// mpaStreamType chooses between the two stream types of MPEG audio by the
// sample rate, which is what tells the two standards apart: FFmpeg's
// mpegtsenc.c makes the same choice (get_dvb_stream_type).
func mpaStreamType(rate int) (astits.StreamType, bool) {
	switch rate {
	case 32000, 44100, 48000:
		return tsMPEG1Audio, true
	case 16000, 22050, 24000:
		return tsMPEG2Audio, true
	}
	return 0, false
}

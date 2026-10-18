// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"

	"github.com/at-wat/ebml-go"
)

// Matroska TrackType codes (Matroska spec §track-type).
const (
	mkvTrackVideo    = 1
	mkvTrackAudio    = 2
	mkvTrackSubtitle = 17
)

// defaultTimecodeScale is Matroska's default segment TimecodeScale: nanoseconds
// per timecode tick (1 ms). Used when a Segment omits the element.
const defaultTimecodeScale = 1_000_000

// The subset of the EBML/Matroska tree this demuxer reads: the header's DocType
// (to tell WebM from MKV) and the segment's Info + Tracks. Clusters and every
// other element are skipped by ebml-go.
type mkvDoc struct {
	Header  mkvHeader  `ebml:"EBML"`
	Segment mkvSegment `ebml:"Segment"`
}

type mkvHeader struct {
	DocType string `ebml:"EBMLDocType"`
}

type mkvSegment struct {
	Info        mkvInfo          `ebml:"Info"`
	Tracks      mkvTracks        `ebml:"Tracks"`
	Attachments []mkvAttachments `ebml:"Attachments,omitempty"`
	Tags        []mkvTagsElement `ebml:"Tags,omitempty"`
}

type mkvInfo struct {
	TimecodeScale uint64  `ebml:"TimecodeScale,omitempty"`
	Duration      float64 `ebml:"Duration,omitempty"`
	Title         string  `ebml:"Title,omitempty"`
}

type mkvTracks struct {
	TrackEntry []mkvTrackEntry `ebml:"TrackEntry"`
}

type mkvTrackEntry struct {
	TrackNumber uint64    `ebml:"TrackNumber"`
	TrackType   uint64    `ebml:"TrackType"`
	CodecID     string    `ebml:"CodecID"`
	Language    string    `ebml:"Language,omitempty"`
	Video       *mkvVideo `ebml:"Video"`
	Audio       *mkvAudio `ebml:"Audio"`
	// CodecPrivate is the codec's own configuration, in whatever form the
	// codec's Matroska mapping states: an avcC record for AVC, an hvcC one
	// for HEVC, an av1C one for AV1, an identification header for Opus, an
	// audio specific config for AAC.
	CodecPrivate []byte `ebml:"CodecPrivate,omitempty"`
	// DefaultDuration is how long a frame of this track lasts, in
	// nanoseconds — not in timestamp ticks, which is what every other
	// duration of a Matroska file is counted in.
	DefaultDuration uint64 `ebml:"DefaultDuration,omitempty"`
	// CodecDelay is the encoder priming at the head of the track, in
	// nanoseconds: media the decoder consumes and does not present. An MP4
	// says the same thing with an edit list.
	CodecDelay uint64 `ebml:"CodecDelay,omitempty"`
}

type mkvVideo struct {
	PixelWidth  uint64 `ebml:"PixelWidth"`
	PixelHeight uint64 `ebml:"PixelHeight"`
	// The crop and display elements state the shape of a pixel (RFC 9559
	// §5.1.4.1.28): the frame shown is PixelWidth and PixelHeight less the
	// crops, and it is displayed at DisplayWidth by DisplayHeight, in the unit
	// DisplayUnit names. A crop defaults to zero, DisplayUnit to 0 (pixels),
	// and a display size cannot be zero, so a zero there is its absence.
	PixelCropBottom uint64     `ebml:"PixelCropBottom,omitempty"`
	PixelCropTop    uint64     `ebml:"PixelCropTop,omitempty"`
	PixelCropLeft   uint64     `ebml:"PixelCropLeft,omitempty"`
	PixelCropRight  uint64     `ebml:"PixelCropRight,omitempty"`
	DisplayWidth    uint64     `ebml:"DisplayWidth,omitempty"`
	DisplayHeight   uint64     `ebml:"DisplayHeight,omitempty"`
	DisplayUnit     uint64     `ebml:"DisplayUnit,omitempty"`
	Colour          *mkvColour `ebml:"Colour"`
}

// mkvColour is what a video track states about its colour, which for VP8 and
// VP9 is the only place a vpcC record can be filled from.
//
// The three fields that are slices are the ones whose zero is a value in its
// own right — an identity matrix, and no subsampling at all — so their absence
// has to be told apart from a stated zero, and a slice is how ebml-go reports
// an element that may or may not be there.
type mkvColour struct {
	MatrixCoefficients      []uint64 `ebml:"MatrixCoefficients,omitempty"`
	ChromaSubsamplingHorz   []uint64 `ebml:"ChromaSubsamplingHorz,omitempty"`
	ChromaSubsamplingVert   []uint64 `ebml:"ChromaSubsamplingVert,omitempty"`
	BitsPerChannel          uint64   `ebml:"BitsPerChannel,omitempty"`
	ChromaSitingVert        uint64   `ebml:"ChromaSitingVert,omitempty"`
	Range                   uint64   `ebml:"Range,omitempty"`
	TransferCharacteristics uint64   `ebml:"TransferCharacteristics,omitempty"`
	Primaries               uint64   `ebml:"Primaries,omitempty"`
}

type mkvAudio struct {
	SamplingFrequency float64 `ebml:"SamplingFrequency"`
	Channels          uint64  `ebml:"Channels"`
}

// mkvLeafLimit caps the leaf elements ebml-go will read from a document of n
// bytes at n.
//
// ⛔ ebml-go sizes a leaf's buffer from the size the element STATES and only
// then finds out whether the bytes are there; its own cap is 128 MiB. A
// 26-byte file stating a DocType of 128 MiB - 1 made each parse allocate 128
// MiB. Every document parsed here is whole in memory, so no leaf in it can be
// longer than it, and a stated size past that is refused before any buffer is
// made. (Zero would turn the limit off, hence the floor of one byte.)
func mkvLeafLimit(n int) ebml.UnmarshalOption {
	limit := uint64(ebml.DefaultMaxLeafElementSize)
	if uint64(n) < limit {
		limit = uint64(n)
	}
	return ebml.WithMaxLeafElementSize(max(limit, 1))
}

// demuxMatroska reads an EBML/Matroska (MKV or WebM) file with the reference
// ebml-go library and projects its Info + Tracks onto the unified metadata.
// Unknown-element tolerance is deliberately off: ebml-go's ignore-unknown mode
// swallows every read/size error and returns an empty document, so a corrupt file
// would look like an empty one — surfacing the error is the safer contract.
func demuxMatroska(data []byte) (*File, error) {
	var doc mkvDoc
	if err := ebml.Unmarshal(bytes.NewReader(data), &doc, mkvLeafLimit(len(data))); err != nil {
		return nil, err
	}
	scale := doc.Segment.Info.TimecodeScale
	if scale == 0 {
		scale = defaultTimecodeScale
	}
	f := &File{
		Format: matroskaFormat(doc.Header.DocType),
		// Ticks per second: 1e9 ns/s ÷ ns/tick. TimecodeScale is a power-of-ten
		// divisor of a second, so this division is exact.
		Timescale: uint32(1_000_000_000 / scale),
		Duration:  uint64(doc.Segment.Info.Duration),
		Metadata:  mkvMetadata(doc.Segment.Info.Title, doc.Segment.Tags, doc.Segment.Attachments),
	}
	for _, te := range doc.Segment.Tracks.TrackEntry {
		f.Tracks = append(f.Tracks, mkvTrack(te, f.Timescale, f.Duration))
	}
	return f, nil
}

// matroskaFormat maps the EBML DocType to a File.Format.
func matroskaFormat(docType string) string {
	if docType == "webm" {
		return "webm"
	}
	return "matroska"
}

// mkvTrack projects one Matroska TrackEntry onto a Track. Matroska carries no
// per-track timescale or duration, so both are inherited from the segment (the
// same fallback VLC's demuxer uses).
func mkvTrack(te mkvTrackEntry, timescale uint32, duration uint64) Track {
	tr := Track{
		ID:        uint32(te.TrackNumber),
		Kind:      kindFromTrackType(te.TrackType),
		Codec:     te.CodecID,
		Language:  te.Language,
		Timescale: timescale,
		Duration:  duration,
	}
	if te.Video != nil {
		tr.Width = int(te.Video.PixelWidth)
		tr.Height = int(te.Video.PixelHeight)
	}
	if te.Audio != nil {
		tr.Channels = int(te.Audio.Channels)
		tr.SampleRate = int(te.Audio.SamplingFrequency)
	}
	return tr
}

// kindFromTrackType maps a Matroska TrackType code to a Kind.
func kindFromTrackType(t uint64) Kind {
	switch t {
	case mkvTrackVideo:
		return Video
	case mkvTrackAudio:
		return Audio
	case mkvTrackSubtitle:
		return Subtitle
	default:
		return Other
	}
}

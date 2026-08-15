// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package container demuxes time-based media containers — MP4/ISO-BMFF and
// Matroska/WebM — in pure Go (CGO=0). It reads the container structure and
// exposes each elementary stream's metadata (kind, codec, dimensions, timing);
// codec bitstream decoding lives in sibling packages.
package container

import "fmt"

// Kind classifies a media track.
type Kind uint8

const (
	Other Kind = iota
	Video
	Audio
	Subtitle
)

// String returns the track kind's lowercase name.
func (k Kind) String() string {
	switch k {
	case Video:
		return "video"
	case Audio:
		return "audio"
	case Subtitle:
		return "subtitle"
	default:
		return "other"
	}
}

// Track is one demuxed elementary stream's metadata.
type Track struct {
	ID            uint32
	Kind          Kind
	Codec         string // fourcc / codec id, e.g. "avc1", "vp09", "mp4a", "V_VP9"
	Width, Height int    // video frame size in pixels (0 for non-video)
	Channels      int    // audio channel count (0 for non-audio)
	SampleRate    int    // audio sample rate in Hz (0 for non-audio)
	Timescale     uint32 // media timescale (units per second)
	Duration      uint64 // track duration in Timescale units
	Language      string // ISO-639-2 code, e.g. "und"
}

// DurationSeconds returns the track's duration in seconds (0 if unknown).
func (t Track) DurationSeconds() float64 {
	if t.Timescale == 0 {
		return 0
	}
	return float64(t.Duration) / float64(t.Timescale)
}

// File is a demuxed container's structure.
type File struct {
	Format    string // "mp4" or "matroska" or "webm"
	Brand     string // MP4 major brand (e.g. "isom"); "" for Matroska
	Timescale uint32 // movie/segment timescale (units per second)
	Duration  uint64 // overall duration in Timescale units
	Tracks    []Track
}

// DurationSeconds returns the overall duration in seconds (0 if unknown).
func (f *File) DurationSeconds() float64 {
	if f.Timescale == 0 {
		return 0
	}
	return float64(f.Duration) / float64(f.Timescale)
}

// VideoTracks returns the file's video tracks, in file order.
func (f *File) VideoTracks() []Track { return f.tracksOfKind(Video) }

// AudioTracks returns the file's audio tracks, in file order.
func (f *File) AudioTracks() []Track { return f.tracksOfKind(Audio) }

func (f *File) tracksOfKind(k Kind) []Track {
	var out []Track
	for _, t := range f.Tracks {
		if t.Kind == k {
			out = append(out, t)
		}
	}
	return out
}

// Demux sniffs data's container format and returns its demuxed structure. It
// returns an error for an unrecognised or malformed container.
func Demux(data []byte) (*File, error) {
	switch Sniff(data) {
	case FormatMP4:
		return demuxMP4(data)
	case FormatMatroska:
		return demuxMatroska(data)
	default:
		return nil, fmt.Errorf("container: unrecognised format")
	}
}

// Format identifies a container format.
type Format uint8

const (
	FormatUnknown Format = iota
	FormatMP4
	FormatMatroska // MKV and WebM (both EBML/Matroska)
)

// Sniff identifies the container format from data's leading bytes.
func Sniff(data []byte) Format {
	// ISO-BMFF: an 'ftyp' box near the start (its type field at offset 4).
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		return FormatMP4
	}
	// Matroska/WebM: the EBML header magic 0x1A45DFA3.
	if len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return FormatMatroska
	}
	return FormatUnknown
}

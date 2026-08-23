// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Join writes the tracks of every input side by side into one file: the picture
// of one and the sound of another become one film. Where Concat puts its inputs
// one after another in time, Join puts them together in it — which is what a
// stream delivered as separate representations needs, since a packager keeps
// them apart and expects a player to pair them.
//
// Nothing is re-encoded. Samples are written in decoding order across the
// inputs, so the file is interleaved the way a player reads it rather than one
// whole track followed by another, which would make a player seek the length of
// the film to hear it.
//
// An input carrying a track this package cannot describe is not fatal: that
// track is left out and named in the error only if nothing else remains, since
// losing a stray text track is better than losing the film with it.
func Join(w io.Writer, srcs []*Reader, opts ...RemuxOption) error {
	if len(srcs) == 0 {
		return fmt.Errorf("%w: no input", ErrNoTracks)
	}
	m := NewMuxer(w, settingsFor(opts).mux...)
	if err := joinInto(m, srcs, opts...); err != nil {
		return err
	}
	return m.Close()
}

// JoinProgressive is Join, writing the ordinary MP4 a file on disk should be
// rather than the fragmented one a player streams.
func JoinProgressive(w io.Writer, srcs []*Reader, opts ...RemuxOption) error {
	if len(srcs) == 0 {
		return fmt.Errorf("%w: no input", ErrNoTracks)
	}
	m := NewProgressiveMuxer(w)
	if err := joinInto(m, srcs, opts...); err != nil {
		return err
	}
	return m.Close()
}

// sampleWriter is what both muxers offer, and all a join needs of them.
type sampleWriter interface {
	AddTrack(TrackConfig) (uint32, error)
	WriteSample(uint32, Sample) error
}

// joinInto declares every track, then copies their samples in decoding order.
func joinInto(m sampleWriter, srcs []*Reader, opts ...RemuxOption) error {
	set := settingsFor(opts)
	var tracks []*joinTrack
	for _, src := range srcs {
		for _, id := range src.TrackIDs() {
			if set.drop[id] {
				continue
			}
			cfg, err := src.TrackConfig(id)
			if err != nil {
				continue // a track this package cannot describe
			}
			samples, err := src.Samples(id)
			if err != nil {
				continue // a track holding nothing
			}
			outID, err := m.AddTrack(cfg)
			if err != nil {
				return fmt.Errorf("container: declare track %d: %w", id, err)
			}
			tracks = append(tracks, &joinTrack{
				id: outID, timescale: cfg.Timescale, samples: samples,
			})
		}
	}
	if len(tracks) == 0 {
		return fmt.Errorf("%w: nothing to join", ErrNoTracks)
	}
	for {
		pick := furthestBehind(tracks)
		if pick == nil {
			return nil
		}
		s := pick.samples[pick.next]
		if err := m.WriteSample(pick.id, s); err != nil {
			return fmt.Errorf("container: write sample: %w", err)
		}
		pick.clock += uint64(s.Duration)
		pick.next++
	}
}

// joinTrack is one track being copied, with what is left of it.
type joinTrack struct {
	id        uint32
	timescale uint32
	samples   []Sample
	next      int
	clock     uint64 // decode time of samples[next], in timescale units
}

// furthestBehind is the track whose next sample is earliest, so that what is
// written is always the sample a player would want next.
func furthestBehind(tracks []*joinTrack) *joinTrack {
	var pick *joinTrack
	var earliest float64
	for _, t := range tracks {
		if t.next >= len(t.samples) {
			continue
		}
		at := float64(t.clock) / float64(t.timescale)
		if pick == nil || at < earliest {
			pick, earliest = t, at
		}
	}
	return pick
}

// HoldsFragments reports whether an MP4 states its samples in fragments rather
// than in the sample tables of its moov.
//
// Only the box headers are read: walking them by the size each states costs one
// read apiece, where decoding the file to ask would cost the film. A file that
// is not an MP4 at all simply holds no fragments.
func HoldsFragments(src io.ReaderAt) (bool, error) {
	if src == nil {
		return false, fmt.Errorf("%w: nothing to read", ErrUnsupportedFormat)
	}
	var at int64
	header := make([]byte, 16)
	for {
		n, err := src.ReadAt(header, at)
		if n < 8 {
			if err == nil || errors.Is(err, io.EOF) {
				return false, nil // the last box ended the file
			}
			return false, err
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		switch string(header[4:8]) {
		case "moof":
			return true, nil
		case "mdat":
			// Media at the top level, with a moov indexing it: that is what
			// an ordinary file looks like.
			return false, nil
		}
		switch {
		case size == 1:
			if n < 16 {
				return false, fmt.Errorf("%w: a box announces a 64-bit size it does not carry",
					ErrUnsupportedFormat)
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
		case size == 0:
			return false, nil // the box runs to the end of the file
		}
		if size < 8 {
			return false, fmt.Errorf("%w: a box announces %d bytes", ErrUnsupportedFormat, size)
		}
		at += size
	}
}

// OpenFile reads a container from disk, choosing how by what it turns out to
// be: an MP4 stays on disk and its samples are fetched as they are asked for,
// while a transport stream or a Matroska file is read whole, because a demuxer
// walks those from end to end and there is nothing to address into.
//
// The returned function closes what was opened. It is not nil.
func OpenFile(path string) (*Reader, func() error, error) {
	noClose := func() error { return nil }
	fh, err := openFile(path)
	if err != nil {
		return nil, noClose, err
	}
	info, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, noClose, err
	}
	if r, err := NewFileReader(fh, info.Size()); err == nil {
		return r, fh.Close, nil
	}
	// Read whole: the format cannot be addressed into.
	fh.Close()
	data, err := readFile(path)
	if err != nil {
		return nil, noClose, err
	}
	r, err := NewReader(data)
	if err != nil {
		return nil, noClose, err
	}
	return r, noClose, nil
}

// openedFile is what OpenFile needs of a file: to read into it, seek in it, know
// how big it is and let it go.
type openedFile interface {
	FileSource
	Stat() (os.FileInfo, error)
	Close() error
}

// The filesystem OpenFile reads through, as variables so the ways a file gives
// way — vanishing between the open and the read, refusing to be read at all —
// can be staged: they cannot be reached on a file that behaves.
var (
	openFile = func(path string) (openedFile, error) { return os.Open(path) }
	readFile = os.ReadFile
)

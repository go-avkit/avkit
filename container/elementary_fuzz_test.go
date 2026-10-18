// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

// Each reader here walks bytes that came off a disk or a network, so each is
// fuzzed on its own, from the fixtures and from what the tests build. What is
// held is what every caller relies on: nothing panics, nothing reads outside
// the input, and every sample handed back has bytes and a duration, which is
// what the muxers refuse to write without.

func fuzzElementary(f *testing.F, read func([]byte) ([]*mkvReadTrack, error), seeds ...[]byte) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tracks, err := read(data)
		if err != nil && !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrMalformed) {
			t.Fatalf("an error that is neither a cut nor a malformation: %v", err)
		}
		for _, track := range tracks {
			for i, s := range track.samples {
				if len(s.Data) == 0 || s.Duration == 0 {
					t.Fatalf("track %d sample %d: %d bytes lasting %d", track.track.ID, i, len(s.Data), s.Duration)
				}
			}
		}
		// The public entry points agree with the reader they dispatch to.
		if f, ok := elementaryFor(Sniff(data)); ok {
			if _, err := newElementaryReader(f, data); err != nil && !errors.Is(err, ErrTruncated) &&
				!errors.Is(err, ErrMalformed) {
				t.Fatalf("NewReader: %v", err)
			}
		}
	})
}

func seedFile(f *testing.F, name string) []byte {
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		f.Fatal(err)
	}
	return data
}

func FuzzADTS(f *testing.F) {
	frame := adtsFrameOf(1, 4, 2, make([]byte, 20), true)
	fuzzElementary(f, readADTS, seedFile(f, "sine.aac"), concat(id3v2Tag(4, false), frame, frame))
}

func FuzzFLAC(f *testing.F) {
	frame := flacFrameBytes(flacHeaderBytes(true, 7, 0, 0, 0x01, 0x00), []byte{1})
	fuzzElementary(f, func(data []byte) ([]*mkvReadTrack, error) {
		if !sniffFLAC(data) {
			return nil, nil // readFLAC is only ever handed what sniffs as FLAC
		}
		return readFLAC(data)
	}, seedFile(f, "sine.flac"),
		concat([]byte(flacMarker), flacBlock(true, flacStreamInfo, streamInfo(8000, 1, 16, 0)), frame))
}

// FuzzOgg mends the CRC of every page it can find before reading: without it
// almost every mutation is refused at the checksum, and the mappings behind it
// are never reached.
func FuzzOgg(f *testing.F) {
	fuzzElementary(f, func(data []byte) ([]*mkvReadTrack, error) {
		return readOgg(mendOggCRCs(data))
	}, seedFile(f, "sine.opus"), seedFile(f, "sine.ogg"), seedFile(f, "sine.oga"))
}

// mendOggCRCs rewrites the CRC of each whole page at the start of data.
func mendOggCRCs(data []byte) []byte {
	out := append([]byte(nil), data...)
	for at := 0; at+oggHeaderSize <= len(out) && string(out[at:at+4]) == "OggS"; {
		header := oggHeaderSize + int(out[at+26])
		if at+header > len(out) {
			break
		}
		end := at + header
		for _, l := range out[at+oggHeaderSize : at+header] {
			end += int(l)
		}
		if end > len(out) {
			break
		}
		binary.LittleEndian.PutUint32(out[at+22:], oggCRC(out[at:end]))
		at = end
	}
	return out
}

func FuzzWAV(f *testing.F) {
	fuzzElementary(f, func(data []byte) ([]*mkvReadTrack, error) {
		if !sniffWAV(data) {
			return nil, nil // readWAV is only ever handed what sniffs as WAVE
		}
		tracks, err := readWAV(data)
		if errors.Is(err, ErrUnsupportedCodec) {
			err = nil
		}
		return tracks, err
	}, seedFile(f, "sine.wav"), seedFile(f, "sine_3ch.wav"), seedFile(f, "sine_rf64.wav"))
}

// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzIlstItem hands arbitrary bytes to the ilst item reader, which reads
// sizes out of them. It must neither panic nor return a value that is not a
// piece of what it was given: nothing is allocated from a size it reads.
func FuzzIlstItem(f *testing.F) {
	f.Add(box("\xa9nam", dataBox(ilstUTF8, []byte("title"))))
	f.Add(box("covr", dataBox(ilstJPEG, []byte{0xff, 0xd8, 0xff})))
	f.Add(box("\xa9ART", dataBox(ilstUTF16, []byte{0, 'a'})))
	f.Add([]byte{0, 0, 0, 1, 'c', 'o', 'v', 'r', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 0, 'c', 'o', 'v', 'r', 0, 0, 0, 0, 'd', 'a', 't', 'a'})
	f.Fuzz(func(t *testing.T, raw []byte) {
		var md Metadata
		md.addIlstItem(raw)
		if c := md.Cover; c != nil && !bytes.Contains(raw, c.Data) {
			t.Fatalf("a cover of %d bytes out of %d that are not its own", len(c.Data), len(raw))
		}
		for _, f := range md.metadataText() {
			if len(f.value) > 2*len(raw) {
				t.Fatalf("%s of %d bytes out of %d", f.name, len(f.value), len(raw))
			}
		}
		if md.IsZero() {
			return
		}
		// Whatever was read can be written back.
		if err := md.validate(); err != nil {
			t.Fatalf("read metadata the muxers refuse: %v", err)
		}
		_ = mp4Udta(&md)
	})
}

// FuzzMatroskaMetadata reads arbitrary Matroska documents, seeded with the
// ffmpeg fixture, through both entry points. Tags and attachments are read
// from them, and the cover's size comes from the document.
func FuzzMatroskaMetadata(f *testing.F) {
	f.Add(readFixtureF(f, "metadata-ffmpeg.mkv"))
	f.Add(readFixtureF(f, "tiny.mkv"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, md := range metadataOf(data) {
			if err := md.validate(); err != nil {
				t.Fatalf("read metadata the muxers refuse: %v", err)
			}
			if c := md.Cover; c != nil && len(c.Data) > len(data) {
				t.Fatalf("a cover of %d bytes out of a file of %d", len(c.Data), len(data))
			}
		}
	})
}

// FuzzMP4Metadata does the same for MP4, seeded with the ffmpeg fixture.
func FuzzMP4Metadata(f *testing.F) {
	f.Add(readFixtureF(f, "metadata-ffmpeg.mp4"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, md := range metadataOf(data) {
			if err := md.validate(); err != nil {
				t.Fatalf("read metadata the muxers refuse: %v", err)
			}
		}
	})
}

// metadataOf is the metadata Demux and NewReader read out of data, where they
// read any.
func metadataOf(data []byte) []Metadata {
	var out []Metadata
	if f, err := Demux(data); err == nil {
		out = append(out, f.Metadata)
	}
	if r, err := NewReader(data); r != nil && (err == nil || r.File() != nil) {
		out = append(out, r.File().Metadata)
	}
	return out
}

func readFixtureF(f *testing.F, name string) []byte {
	f.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

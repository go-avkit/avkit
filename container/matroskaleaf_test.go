// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"runtime"
	"testing"
)

// ⛔ ebml-go sizes a leaf element's buffer from the size the element states,
// capped only at its default of 128 MiB, and then finds out the bytes are not
// there. A 26-byte Matroska file stating a DocType of 128 MiB - 1 made NewReader
// allocate 128 MiB, and Demux the same again. Every Matroska file this package
// parses is already whole in memory, so no leaf in it can be longer than the
// file: that is now the cap.
func TestMatroskaLeafCannotOutsizeTheFile(t *testing.T) {
	size := uint32(1<<27 - 1)
	data := []byte{
		0x1A, 0x45, 0xDF, 0xA3, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // EBML header, unknown size
		0x42, 0x82, // DocType, a string
		0x01, 0, 0, 0, byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size),
		'w', 'e', 'b', 'm',
	}
	for name, parse := range map[string]func() error{
		"NewReader": func() error { _, err := NewReader(data); return err },
		"Demux":     func() error { _, err := Demux(data); return err },
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := parse()
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: a %d-byte file stating a %d-byte DocType was accepted", name, len(data), size)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
			t.Errorf("%s: %d bytes allocated for a %d-byte file", name, got, len(data))
		}
	}
}

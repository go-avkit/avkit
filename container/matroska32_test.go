// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"testing"
	"time"
)

// within fails the test if fn has not returned after d: a parser that stops
// making progress never returns, and a test that waited for it would hang the
// suite instead of naming the input.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		// Errorf, not Fatalf: the other inputs still get their answer, and
		// the goroutine left behind belongs to a test that has failed.
		t.Errorf("%s: no answer after %v", what, d)
	}
}

// ⛔ EBML sizes are up to 56 bits, and the truncation rescue added them to
// offsets as int. Where int is 32 bits wide a size of 2^32 - n converts to -n,
// so a size equal to minus the header's own width moved the walk by exactly
// nothing -- an infinite loop -- and a Segment size of that kind made the walk
// limit negative, which sliced out of range. Each input below is a few dozen
// bytes. On a 64-bit target the conversion is exact and these pass either way;
// the failures were reproduced under GOARCH=arm (linux/arm/v7).
func TestMatroskaSizesBeyondIntDoNotStallOrPanic(t *testing.T) {
	ebmlHeader := []byte{0x1A, 0x45, 0xDF, 0xA3}
	segment := []byte{0x18, 0x53, 0x80, 0x67}
	// eightByteSize is an EBML size of 2^32 - minus in eight bytes.
	eightByteSize := func(minus uint32) []byte {
		v := uint32(0) - minus
		return []byte{0x01, 0, 0, 0, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
	pad := make([]byte, 32)
	cases := map[string][]byte{
		// The EBML header states a size of minus its own header: the walk to
		// the Segment stood still.
		"ebml header": concat(ebmlHeader, eightByteSize(12), pad),
		// The Segment states a size that puts the end of its children before
		// its start.
		"segment": concat(ebmlHeader, []byte{0x80}, segment, eightByteSize(20), pad),
		// A child of an unknown-size Segment states minus its own header.
		"segment child": concat(ebmlHeader, []byte{0x80}, segment,
			[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, []byte{0xEC}, eightByteSize(9), pad),
	}
	for name, data := range cases {
		within(t, 10*time.Second, name, func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s: panic: %v", name, p)
				}
			}()
			_, _ = completeMatroskaPrefix(data)
			_, _ = NewReader(data)
		})
	}
}

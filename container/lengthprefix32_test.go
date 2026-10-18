// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import "testing"

// ⛔ A four-byte NAL unit length was converted to int and then added to the
// offset. Where int is 32 bits wide, a length near 2^31 made that sum wrap
// negative, passed the bounds check and sliced out of range -- a panic in
// ConfigFromSamples, CheckHVC1 and the MPEG-TS muxer for any sample carrying
// such four bytes. On a 64-bit target the arithmetic is exact and this passes
// either way; the failure ("slice bounds out of range [:-2147483645]") was
// reproduced under GOARCH=arm (linux/arm/v7).
func TestLengthPrefixNearTwoToTheThirtyOneIsRefused(t *testing.T) {
	for _, length := range [][]byte{
		{0x7F, 0xFF, 0xFF, 0xFF},
		{0x7F, 0xFF, 0xFF, 0xFC},
		{0x80, 0x00, 0x00, 0x00},
		{0xFF, 0xFF, 0xFF, 0xFF},
	} {
		data := append(append([]byte(nil), length...), 1, 2, 3, 4, 5, 6, 7, 8)
		units, exact := splitLengthPrefixed(data)
		if exact || len(units) != 0 {
			t.Errorf("length % x over 8 bytes: %d units, exact=%v", length, len(units), exact)
		}
	}
}

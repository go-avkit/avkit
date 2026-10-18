// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"encoding/binary"
	"fmt"
)

// hevcRecordHeader is the length of an HEVCDecoderConfigurationRecord up to
// and including numOfArrays (ISO/IEC 14496-15 §8.3.3.1.2): the 22 bytes of
// profile, level and format fields, then the array count.
const hevcRecordHeader = 23

// checkHEVCRecord walks the NAL unit arrays of an HEVCDecoderConfigurationRecord
// and refuses one whose counts promise more than its bytes hold, before
// mp4ff's hevc.DecodeHEVCDecConfRec is handed it.
//
// That decoder reads numNalus (16 bits) and then loops that many times, reading
// a length and the bytes it announces. Once the record runs out, every read
// returns zero, the loop carries on to the end of the count, and each turn
// appends an empty slice: a record that announces 65535 NAL units and stops
// costs 65535 appends and 8 MB of allocation, a few milliseconds, whatever its
// size. A
// Matroska file states one such record per track and refuses each track on its
// own, so a 323 kB file of 4000 HEVC tracks took 10 s to read: some 30 µs per
// input byte. Every NAL unit takes at least its two length bytes, so the walk
// here is bounded by the record's own length.
func checkHEVCRecord(record []byte) error {
	if len(record) < hevcRecordHeader {
		return fmt.Errorf("an hvcC record is at least %d bytes, not %d", hevcRecordHeader, len(record))
	}
	at := hevcRecordHeader
	for i := range int(record[hevcRecordHeader-1]) {
		if len(record)-at < 3 {
			return fmt.Errorf("hvcC array %d stops before its NAL unit count", i)
		}
		count := int(binary.BigEndian.Uint16(record[at+1:]))
		at += 3
		for j := range count {
			if len(record)-at < 2 {
				return fmt.Errorf("hvcC array %d announces %d NAL units and holds %d", i, count, j)
			}
			n := int(binary.BigEndian.Uint16(record[at:]))
			at += 2
			if len(record)-at < n {
				return fmt.Errorf("hvcC array %d NAL unit %d announces %d bytes and holds %d",
					i, j, n, len(record)-at)
			}
			at += n
		}
	}
	return nil
}

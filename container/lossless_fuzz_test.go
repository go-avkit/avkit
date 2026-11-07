// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// FuzzLosslessRecords feeds arbitrary bytes to every record this package reads
// for ALAC and TrueHD: the magic cookie, the dmlp payload, a TrueHD access unit's
// major sync, the mlpa sample entry, and the A_ALAC CodecPrivate. Nothing may
// panic, and what is accepted must come back out unchanged.
func FuzzLosslessRecords(f *testing.F) {
	f.Add(goodCookie())
	f.Add(append(goodCookie(), make([]byte, 24)...))
	f.Add(goodDMLP())
	f.Add(aMajorSync())
	f.Add(mlpaEntryBytes(2, isoBox("dmlp", goodDMLP())))
	f.Add(append([]byte{0, 0, 0, 36, 'a', 'l', 'a', 'c', 0, 0, 0, 0}, goodCookie()...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := parseALACCookie(data); err == nil {
			if got := alacCookie(alacBox(data)); !bytes.Equal(got, data) {
				t.Fatalf("cookie % x came back as % x", data, got)
			}
		}
		_, _ = parseDMLP(data)
		if dmlp, ok := mlpMajorSyncOf(data); ok && len(dmlp) != dmlpPayloadSize {
			t.Fatalf("a major sync gave a dmlp of %d bytes", len(dmlp))
		}
		_ = alacMatroskaCookie(data)
		_ = mlpaSampleEntry(stsdWith(mp4.CreateUnknownBox("mlpa", uint64(8+len(data)), data)))
		_, _ = mkvTrackConfig(mkvTrackEntry{TrackNumber: 1, TrackType: mkvTrackAudio, CodecID: "A_ALAC",
			CodecPrivate: data, Audio: &mkvAudio{SamplingFrequency: 48000, Channels: 2}}, 1000)
	})
}

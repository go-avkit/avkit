// Copyright (c) the go-avkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package container

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
)

// demuxMP4 reads an ISO-BMFF (MP4/MOV) file with the reference mp4ff library and
// projects its box tree onto the unified File/Track metadata. Container parsing
// is delegated to mp4ff; this file only maps its structures.
func demuxMP4(data []byte) (*File, error) {
	mf, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return mp4File(mf)
}

// mp4File projects an already decoded box tree onto the unified metadata, so
// the reader can share one parse with the demuxer.
func mp4File(mf *mp4.File) (*File, error) {
	if mf.Moov == nil {
		return nil, fmt.Errorf("container: MP4 has no moov box")
	}
	f := &File{Format: "mp4"}
	if mf.Ftyp != nil {
		f.Brand = strings.TrimRight(mf.Ftyp.MajorBrand(), " \x00")
	}
	if mf.Moov.Mvhd != nil {
		f.Timescale = mf.Moov.Mvhd.Timescale
		f.Duration = mf.Moov.Mvhd.Duration
	}
	for _, trak := range mf.Moov.Traks {
		f.Tracks = append(f.Tracks, mp4Track(trak))
	}
	return f, nil
}

// mp4Track projects one mp4ff TrakBox onto a Track.
func mp4Track(trak *mp4.TrakBox) Track {
	var tr Track
	if trak.Tkhd != nil {
		tr.ID = trak.Tkhd.TrackID
		tr.Width = int(trak.Tkhd.Width >> 16) // 16.16 fixed-point display size
		tr.Height = int(trak.Tkhd.Height >> 16)
	}
	if trak.Mdia == nil {
		return tr
	}
	if m := trak.Mdia.Mdhd; m != nil {
		tr.Timescale = m.Timescale
		tr.Duration = m.Duration
		tr.Language = strings.TrimRight(m.GetLanguage(), " \x00")
	}
	if h := trak.Mdia.Hdlr; h != nil {
		tr.Kind = kindFromHandler(h.HandlerType)
	}
	if trak.Mdia.Minf != nil && trak.Mdia.Minf.Stbl != nil && trak.Mdia.Minf.Stbl.Stsd != nil {
		applySampleEntry(&tr, trak.Mdia.Minf.Stbl.Stsd)
	}
	return tr
}

// kindFromHandler maps an ISO-BMFF handler type to a Kind.
func kindFromHandler(handler string) Kind {
	switch handler {
	case "vide":
		return Video
	case "soun":
		return Audio
	case "subt", "sbtl", "text":
		return Subtitle
	default:
		return Other
	}
}

// applySampleEntry reads the first sample description for the codec four-CC and,
// where mp4ff exposes them, the video frame size or audio channel/rate.
func applySampleEntry(tr *Track, stsd *mp4.StsdBox) {
	sd, err := stsd.GetSampleDescription(0)
	if err != nil {
		return
	}
	tr.Codec = strings.TrimRight(sd.Type(), " \x00")
	switch e := sd.(type) {
	case *mp4.VisualSampleEntryBox:
		if e.Width != 0 {
			tr.Width, tr.Height = int(e.Width), int(e.Height)
		}
	case *mp4.AudioSampleEntryBox:
		tr.Channels = int(e.ChannelCount)
		tr.SampleRate = int(e.SampleRate)
	}
}
